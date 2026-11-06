#!/usr/bin/env python3
"""Isolated lifecycle faults: no Docker, systemd, network or real data required."""
import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import release as r

OLD = '1'*64
NEW = '2'*64
SOURCE = 'fixture-source-revision'
IMAGE = NEW


class LostProcess(BaseException):
    pass


class FakeGate:
    def __init__(self, host):
        self.host = host

    def present(self):
        return self.host.gated

    def verify(self):
        r.require(self.host.gated, 'fixture_gate_absent')
        return {'nginx_out': 1, 'internal_forward': 1}

    def install(self):
        r.require(not self.host.gated, 'fixture_gate_already_present')
        self.host.event('gate_install', lambda: setattr(self.host, 'gated', True))
        return self.verify()

    def remove(self):
        if self.host.gated:
            self.host.event('gate_remove', lambda: setattr(self.host, 'gated', False))


class Fake(r.Host):
    def __init__(self):
        config = {'Env': ['EXAMPLE=fixture-only'], 'Image': r.OLD_IMAGE, 'Hostname': OLD[:12],
                  'Entrypoint': ['/app/docker-entrypoint.sh'], 'Cmd': ['/app/sub2api'],
                  'WorkingDir': '/app', 'User': '1000', 'Healthcheck': {'Test': ['CMD', 'true']},
                  'Labels': {'task46.run': 'final-20261105-window1', 'com.docker.compose.image': r.OLD_IMAGE}}
        runtime = {'Binds': ['/fixture/data:/app/data:rw'], 'NetworkMode': r.APP_NETWORK,
                   'PortBindings': {'8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': r.HOST_PORT}]},
                   'RestartPolicy': {'Name': 'unless-stopped', 'MaximumRetryCount': 0},
                   'LogConfig': {'Type': 'json-file', 'Config': {'max-size': '20m', 'max-file': '3'}}}
        old = {'Id': OLD, 'Image': r.OLD_IMAGE, 'Name': '/' + r.APP, 'Config': config, 'HostConfig': runtime,
               'RestartCount': 0, 'State': {'Running': True, 'StartedAt': '2026-11-05T11:59:27Z', 'Health': {'Status': 'healthy'}},
               'NetworkSettings': {'Networks': {name: {'NetworkID': name + '-id', 'Aliases': [r.APP], 'IPAMConfig': None, 'DriverOpts': None} for name in r.NETWORKS}}}
        self.apps = {OLD: old}
        for name in ('pg', 'redis'):
            dep = copy.deepcopy(old)
            dep.update(Id=name, Name='/' + name, Image='fixture-' + name)
            dep['NetworkSettings']['Networks'] = {r.DEPENDENCY_NETWORK: {'NetworkID': r.DEPENDENCY_NETWORK + '-id'}}
            self.apps[name] = dep
        self.source = {'app': {'name': r.APP, 'id': OLD, 'started_at': old['State']['StartedAt']},
                       'pg': {'name': 'pg', 'id': 'pg', 'started_at': old['State']['StartedAt']},
                       'redis': {'name': 'redis', 'id': 'redis', 'started_at': old['State']['StartedAt']},
                       'dependency_network': r.DEPENDENCY_NETWORK, 'appdata': '/fixture/data'}
        self.timer_active = True
        self.backup_active = False
        self.connections = 0
        self.gated = False
        self.fail_candidate_health = False
        self.candidate_wont_stop = False
        self.events = []
        self.fault = None
        self.max_active = 1
        self.start_count = 0
        self.tick = 0

    def event(self, label, action):
        if label.startswith(('term_', 'start_', 'binding_cas')):
            r.require(self.gated, 'fixture_app_mutation_without_both_gates')
        self.events.append(label)
        if self.fault == (len(self.events), 'before'):
            self.fault = None
            raise LostProcess(label)
        result = action()
        active = sum(bool(v['State']['Running']) for k, v in self.apps.items() if k in (OLD, NEW))
        self.max_active = max(self.max_active, active)
        if self.fault == (len(self.events), 'after'):
            self.fault = None
            raise LostProcess(label)
        return result

    def inspect(self, value):
        if value in self.apps:
            return copy.deepcopy(self.apps[value])
        return copy.deepcopy(next(v for v in self.apps.values() if v['Name'] == '/' + value))

    def image(self, value):
        return {'Id': value, 'Os': 'linux', 'Architecture': 'amd64', 'Config': {'Labels': {'org.opencontainers.image.revision': r.SOURCE}}}

    def network(self, name):
        return {'Internal': name == r.DEPENDENCY_NETWORK,
                'Containers': {key: {} for key, value in self.apps.items() if name in value['NetworkSettings']['Networks']}}

    def healthy(self, container):
        if self.fail_candidate_health and container == NEW:
            raise r.Failure('fixture_health_failure')
        value = self.inspect(container)
        r.require(value['State']['Running'], 'fixture_not_running')
        return value

    def health_port(self):
        r.require(any(app['Name'] == '/' + r.APP and app['State']['Running'] for app in self.apps.values()), 'fixture_port_unhealthy')

    def unit(self, name):
        if name == r.TIMER:
            return {'ActiveState': 'active' if self.timer_active else 'inactive', 'SubState': 'waiting' if self.timer_active else 'dead', 'UnitFileState': 'enabled'}
        return {'ActiveState': 'active' if self.backup_active else 'inactive'}

    def timer(self, active):
        self.event('timer_' + str(active), lambda: setattr(self, 'timer_active', active))

    def binding(self):
        return copy.deepcopy(self.source)

    def binding_cas(self, allowed, replacement):
        r.require(self.source in allowed, 'backup_source_binding_changed')
        self.event('binding_cas', lambda: setattr(self, 'source', copy.deepcopy(replacement)))

    def backup_source_check(self):
        app = self.inspect(r.APP)
        r.require(self.source['app']['id'] == app['Id'] and app['State']['Running'], 'fixture_backup_bad_binding')
        return {'stage': 'core_backup_source_bound', 'source': self.binding(), 'image': app['Image'],
                'revision': r.BASE if app['Id'] == OLD else r.SOURCE}

    def ingress_facts(self):
        return {'bridge': 'br-522fc2807c92', 'nginx_uid': 33}

    def gate(self, directory, plan):
        return FakeGate(self)

    def prove_gate(self, directory, plan):
        return self.gate(directory, plan).verify()

    def quiet(self, container):
        r.require(self.gated, 'fixture_drain_without_gate')
        r.require(self.connections == 0, 'fixture_sub2_inbound_busy')

    def restart_policy(self, container, policy):
        self.event('restart_policy_' + container[:4], lambda: self.apps[container]['HostConfig'].update(RestartPolicy=copy.deepcopy(policy)))

    def terminate(self, container):
        def action():
            if not (container == NEW and self.candidate_wont_stop):
                self.apps[container]['State']['Running'] = False
        self.event('term_' + container[:4], action)

    def wait_stopped(self, container):
        r.require(not self.apps[container]['State']['Running'], 'graceful_stop_timeout_no_sigkill_sent')

    def rename(self, container, name):
        def action():
            r.require(not any(value['Name'] == '/' + name and key != container for key, value in self.apps.items()), 'fixture_name_collision')
            self.apps[container]['Name'] = '/' + name
        self.event('rename_' + container[:4], action)

    def disconnect(self, container, name):
        self.event('disconnect_' + container[:4] + '_' + name, lambda: self.apps[container]['NetworkSettings']['Networks'].pop(name, None))

    def connect(self, container, name, aliases):
        def action():
            self.apps[container]['NetworkSettings']['Networks'][name] = {'NetworkID': name + '-id', 'Aliases': aliases, 'IPAMConfig': None, 'DriverOpts': None}
        self.event('connect_' + container[:4] + '_' + name, action)

    def create(self, spec):
        def action():
            app = copy.deepcopy(self.apps[OLD])
            app.update(Id=NEW, Name='/' + r.APP, Image=spec['Image'], Config={key: value for key, value in spec.items() if key not in ('HostConfig', 'NetworkingConfig')}, HostConfig=copy.deepcopy(spec['HostConfig']))
            app['State']['Running'] = False
            app['NetworkSettings']['Networks'] = {name: {'NetworkID': name + '-id', 'Aliases': endpoint['Aliases'], 'IPAMConfig': None, 'DriverOpts': None} for name, endpoint in spec['NetworkingConfig']['EndpointsConfig'].items()}
            self.apps[NEW] = app
            return NEW
        return self.event('create_candidate', action)

    def candidates(self, run):
        return [{'Id': key} for key, app in self.apps.items() if app['Config']['Labels'].get(r.RUN_LABEL) == run]

    def start(self, container):
        def action():
            r.require(not any(value['State']['Running'] for key, value in self.apps.items() if key in (OLD, NEW) and key != container), 'fixture_two_active_owners')
            self.tick += 1
            self.apps[container]['State'].update(Running=True, StartedAt='restart-' + str(self.tick))
            self.start_count += container == NEW
        self.event('start_' + container[:4], action)


class Lifecycle(unittest.TestCase):
    def setUp(self):
        self.source_patch = patch.object(r, 'SOURCE', SOURCE)
        self.image_patch = patch.object(r, 'IMAGE', IMAGE)
        self.source_patch.start()
        self.image_patch.start()
        self.addCleanup(self.source_patch.stop)
        self.addCleanup(self.image_patch.stop)

    def stage(self, directory, host):
        r.save(directory/'plan.json', r.prepare(host, IMAGE))
        return (copy.deepcopy(host.source), copy.deepcopy(host.apps['pg']),
                copy.deepcopy(host.apps['redis']), copy.deepcopy(host.apps[OLD]['Config']['Env']))

    def invariant(self, host, original):
        before, pg, redis, old_env = original
        self.assertEqual(host.apps['pg'], pg)
        self.assertEqual(host.apps['redis'], redis)
        self.assertEqual(host.max_active, 1)
        self.assertLessEqual(host.start_count, 1)
        self.assertTrue(host.timer_active)
        self.assertFalse(host.gated)
        active = [value for key, value in host.apps.items() if key in (OLD, NEW) and value['State']['Running']]
        self.assertEqual(len(active), 1)
        self.assertEqual(host.source['app'], {'name': r.APP, 'id': active[0]['Id'], 'started_at': active[0]['State']['StartedAt']})
        self.assertEqual(active[0]['Name'], '/' + r.APP)
        self.assertEqual(set(active[0]['NetworkSettings']['Networks']), r.NETWORKS)
        self.assertEqual(active[0]['HostConfig']['RestartPolicy'], {'Name': 'unless-stopped', 'MaximumRetryCount': 0})
        self.assertEqual({key: value for key, value in host.source.items() if key != 'app'}, {key: value for key, value in before.items() if key != 'app'})
        expected_env = old_env + ([r.PREHEADER_ENV] if active[0]['Id'] == NEW else [])
        self.assertEqual(active[0]['Config']['Env'], expected_env)

    def test_prepare_refuses_unbound_release_identity(self):
        host = Fake()
        with patch.object(r, 'SOURCE', r.UNBOUND_SOURCE), patch.object(r, 'IMAGE', r.UNBOUND_IMAGE):
            with self.assertRaisesRegex(r.Failure, 'release_identity_unbound'):
                r.prepare(host, r.UNBOUND_IMAGE)
        self.assertEqual(host.events, [])

    def test_prepare_refuses_existing_preheader_setting(self):
        host = Fake()
        host.apps[OLD]['Config']['Env'].append(r.PREHEADER_ENV)
        with tempfile.TemporaryDirectory() as temp:
            with self.assertRaisesRegex(r.Failure, 'old_env_already_contains_preheader_setting'):
                r.prepare(host, IMAGE)
        self.assertEqual(host.events, [])

    def test_success_and_manual_rollback(self):
        with tempfile.TemporaryDirectory() as temp:
            directory, host = Path(temp), Fake()
            original = self.stage(directory, host)
            result = r.apply(host, directory)
            self.assertEqual(result['phase'], 'verified')
            self.assertEqual(host.apps[NEW]['Config']['Env'], original[3] + [r.PREHEADER_ENV])
            self.invariant(host, original)
            self.assertFalse(host.apps[OLD]['State']['Running'])
            with self.assertRaises(r.Failure):
                r.apply(host, directory)
            self.assertEqual(r.rollback(host, directory)['phase'], 'rolled_back')
            self.assertEqual(host.apps[OLD]['Config']['Env'], original[3])
            self.invariant(host, original)
            self.assertEqual(host.source['app']['id'], OLD)
            self.assertNotEqual(host.source['app']['started_at'], original[0]['app']['started_at'])
            r.recover(host, directory)
            self.invariant(host, original)

    def test_process_loss_before_and_after_every_mutation(self):
        with tempfile.TemporaryDirectory() as temp:
            host, directory = Fake(), Path(temp)
            self.stage(directory, host)
            r.apply(host, directory)
            labels = host.events[:]
        for index, label in enumerate(labels, 1):
            for position in ('before', 'after'):
                with self.subTest(mutation=label, position=position), tempfile.TemporaryDirectory() as temp:
                    host, directory = Fake(), Path(temp)
                    original = self.stage(directory, host)
                    host.fault = (index, position)
                    with self.assertRaises(LostProcess):
                        r.apply(host, directory)
                    r.recover(host, directory)  # ExecStopPost in a new process.
                    self.invariant(host, original)
        print('faulted_mutations=' + str(len(labels)*2))

    def test_process_loss_at_each_durable_phase(self):
        names = ['gate_install_intent', 'timer_stop_intent', 'old_stop_intent', 'old_detach_intent', 'candidate_create_intent', 'candidate_start_intent', 'candidate_binding_intent', 'candidate_committed', 'open_gate_intent', 'verified']
        original_phase = r.phase
        for name in names:
            for position in ('before', 'after'):
                with self.subTest(phase=name, position=position), tempfile.TemporaryDirectory() as temp:
                    host, directory = Fake(), Path(temp)
                    original = self.stage(directory, host)
                    def fault(path, state, value):
                        if value == name and position == 'before':
                            raise LostProcess(name)
                        original_phase(path, state, value)
                        if value == name and position == 'after':
                            raise LostProcess(name)
                    with patch.object(r, 'phase', fault), self.assertRaises(LostProcess):
                        r.apply(host, directory)
                    r.recover(host, directory)
                    self.invariant(host, original)

    def test_busy_direct_sub2_and_backup_race_leave_app_running(self):
        for reason in ('sub2', 'backup'):
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as temp:
                directory, host = Path(temp), Fake()
                original = self.stage(directory, host)
                if reason == 'sub2':
                    host.connections = 2
                else:
                    original_timer = host.timer
                    def race(active):
                        original_timer(active)
                        host.backup_active = True
                    host.timer = race
                with self.assertRaises(r.Failure):
                    r.apply(host, directory)
                self.assertEqual(r.recover(host, directory)['phase'], 'aborted')
                self.invariant(host, original)
                self.assertEqual(host.start_count, 0)

    def test_health_failure_and_candidate_refusal_to_stop(self):
        with tempfile.TemporaryDirectory() as temp:
            directory, host = Path(temp), Fake()
            original = self.stage(directory, host)
            host.fail_candidate_health = True
            with self.assertRaises(r.Failure):
                r.apply(host, directory)
            host.candidate_wont_stop = True
            with self.assertRaisesRegex(r.Failure, 'no_sigkill'):
                r.recover(host, directory)
            self.assertFalse(host.apps[OLD]['State']['Running'])
            self.assertTrue(host.apps[NEW]['State']['Running'])
            self.assertFalse(host.timer_active)
            host.candidate_wont_stop = False
            r.recover(host, directory)
            self.invariant(host, original)

    def test_changed_identity_or_source_does_not_stop(self):
        for condition in ('identity', 'source'):
            with self.subTest(condition=condition), tempfile.TemporaryDirectory() as temp:
                directory, host = Path(temp), Fake()
                original = self.stage(directory, host)
                if condition == 'identity':
                    host.apps[OLD]['State']['StartedAt'] = 'another-start'
                if condition == 'source':
                    host.source['app']['id'] = 'another-owner'
                with self.assertRaises(r.Failure):
                    r.apply(host, directory)
                self.assertEqual(host.events, [])
                self.assertEqual(r.recover(host, directory), {'phase': 'not_started'})
                self.assertTrue(host.apps[OLD]['State']['Running'])
                self.assertEqual(host.apps['pg'], original[1])

    def test_source_check_refusal_before_stop_and_candidate_mismatch_rolls_back(self):
        with tempfile.TemporaryDirectory() as temp:
            directory, host = Path(temp), Fake()
            original = self.stage(directory, host)
            receipt = host.backup_source_check
            with patch.object(host, 'backup_source_check', side_effect=r.Failure('core_backup_source_check_failed')):
                with self.assertRaisesRegex(r.Failure, 'source_check_failed'):
                    r.apply(host, directory)
                self.assertFalse((directory/'state.json').exists())
                self.assertEqual(host.events, [])
            expected = original[0] | {'app': {'name': r.APP, 'id': NEW, 'started_at': 'fixture'}}
            matching = {'stage': 'core_backup_source_bound', 'source': expected, 'image': IMAGE, 'revision': r.SOURCE}
            with patch.object(host, 'backup_source_check', return_value=matching):
                r.verify_backup_source(host, expected, IMAGE, r.SOURCE)
            for key, value in (('image', r.OLD_IMAGE), ('revision', r.BASE), ('stage', 'wrong_stage'), ('source', original[0])):
                with self.subTest(receipt_key=key), patch.object(host, 'backup_source_check', return_value=matching | {key: value}):
                    with self.assertRaisesRegex(r.Failure, 'core_backup_source_receipt_mismatch'):
                        r.verify_backup_source(host, expected, IMAGE, r.SOURCE)
            def stale_image():
                value = receipt()
                if host.source['app']['id'] == NEW:
                    value['image'] = r.OLD_IMAGE
                return value
            with patch.object(host, 'backup_source_check', side_effect=stale_image):
                with self.assertRaisesRegex(r.Failure, 'core_backup_source_receipt_mismatch'):
                    r.apply(host, directory)
                self.assertEqual(r.recover(host, directory)['phase'], 'rolled_back')
            self.invariant(host, original)
            self.assertEqual(host.source['app']['id'], OLD)

    def test_rollback_process_loss_before_and_after_every_mutation(self):
        with tempfile.TemporaryDirectory() as temp:
            host, directory = Fake(), Path(temp)
            self.stage(directory, host)
            r.apply(host, directory)
            host.events.clear()
            r.rollback(host, directory)
            labels = host.events[:]
        for index, label in enumerate(labels, 1):
            for position in ('before', 'after'):
                with self.subTest(mutation=label, position=position), tempfile.TemporaryDirectory() as temp:
                    host, directory = Fake(), Path(temp)
                    original = self.stage(directory, host)
                    r.apply(host, directory)
                    host.events.clear()
                    host.fault = (index, position)
                    with self.assertRaises(LostProcess):
                        r.rollback(host, directory)
                    r.recover(host, directory)
                    self.invariant(host, original)
                    events = host.events[:]
                    r.recover(host, directory)
                    self.assertEqual(events, host.events)
        print('rollback_faulted_mutations=' + str(len(labels)*2))

    def test_rollback_journal_interruptions(self):
        names = ['rollback_gate_install_intent', 'rollback_timer_stop_intent', 'manual_rollback_intent',
                 'candidate_stop_intent', 'old_restore_intent', 'rollback_binding_intent', 'old_committed', 'open_gate_intent', 'rolled_back']
        original_phase = r.phase
        for name in names:
            for position in ('before', 'after'):
                with self.subTest(phase=name, position=position), tempfile.TemporaryDirectory() as temp:
                    host, directory = Fake(), Path(temp)
                    original = self.stage(directory, host)
                    r.apply(host, directory)
                    def fault(path, state, value):
                        if value == name and position == 'before':
                            raise LostProcess(name)
                        original_phase(path, state, value)
                        if value == name and position == 'after':
                            raise LostProcess(name)
                    with patch.object(r, 'phase', fault), self.assertRaises(LostProcess):
                        r.rollback(host, directory)
                    r.recover(host, directory)
                    self.invariant(host, original)
                    self.assertLessEqual(host.events.count('start_1111'), 1)

    def test_actual_drain_waits_for_inflight_and_times_out_boundedly(self):
        from types import SimpleNamespace
        for busy_forever in (False, True):
            with self.subTest(busy_forever=busy_forever):
                clock = [0]
                samples = iter(['1 1', '1 1', '1 0', '1 0', '1 0'])
                def execute(*_args, **_kwargs):
                    return SimpleNamespace(returncode=0, stdout='1 1' if busy_forever else next(samples))
                with patch.object(r.time, 'monotonic', side_effect=lambda: clock[0]), \
                     patch.object(r.time, 'sleep', side_effect=lambda delay: clock.__setitem__(0, clock[0] + delay)), \
                     patch.object(r.subprocess, 'run', side_effect=execute), patch.object(r, 'DRAIN_SECONDS', 5 if busy_forever else 135):
                    if busy_forever:
                        with self.assertRaisesRegex(r.Failure, 'drain_timeout'):
                            r.Host().quiet(OLD)
                    else:
                        r.Host().quiet(OLD)
                        self.assertEqual(clock[0], 8)

    def test_gate_probe_failure_and_busy_rollback_always_release(self):
        with tempfile.TemporaryDirectory() as temp:
            host, directory = Fake(), Path(temp)
            original = self.stage(directory, host)
            with patch.object(host, 'prove_gate', side_effect=r.Failure('probe_failed')):
                with self.assertRaisesRegex(r.Failure, 'probe_failed'):
                    r.apply(host, directory)
            self.assertTrue(host.gated)
            self.assertEqual(r.recover(host, directory)['phase'], 'aborted')
            self.invariant(host, original)
            self.assertEqual(host.start_count, 0)
        with tempfile.TemporaryDirectory() as temp:
            host, directory = Fake(), Path(temp)
            original = self.stage(directory, host)
            r.apply(host, directory)
            host.connections = 2
            with self.assertRaises(r.Failure):
                r.rollback(host, directory)
            self.assertEqual(r.recover(host, directory)['phase'], 'rollback_aborted')
            self.invariant(host, original)
            self.assertEqual(host.source['app']['id'], NEW)
            host.connections = 0
            with self.assertRaisesRegex(r.Failure, 'attempt_already_used'):
                r.rollback(host, directory)
            self.assertEqual(r.rollback(host, directory, attempt=2)['phase'], 'rolled_back')
            self.invariant(host, original)

    def test_open_gate_failure_and_recovery_never_switches_again(self):
        with tempfile.TemporaryDirectory() as temp:
            host, directory = Fake(), Path(temp)
            original = self.stage(directory, host)
            with patch.object(FakeGate, 'remove', side_effect=r.Failure('delete_ack_unknown')):
                with self.assertRaisesRegex(r.Failure, 'delete_ack_unknown'):
                    r.apply(host, directory)
            self.assertTrue(host.gated)
            self.assertEqual(r.read(directory/'state.json')['phase'], 'open_gate_intent')
            prior_starts = host.events.count('start_2222')
            prior_terms = host.events.count('term_1111')
            self.assertEqual(r.recover(host, directory)['phase'], 'verified')
            self.invariant(host, original)
            self.assertEqual(host.events.count('start_2222'), prior_starts)
            self.assertEqual(host.events.count('term_1111'), prior_terms)
            self.assertEqual(host.source['app']['id'], NEW)

    def test_failed_restore_keeps_gate_and_same_run_recovery(self):
        with tempfile.TemporaryDirectory() as temp:
            host, directory = Fake(), Path(temp)
            original = self.stage(directory, host)
            r.apply(host, directory)
            source_check = host.backup_source_check
            def fail_old():
                if host.source['app']['id'] == OLD:
                    raise r.Failure('old_source_check_unavailable')
                return source_check()
            with patch.object(host, 'backup_source_check', side_effect=fail_old):
                with self.assertRaisesRegex(r.Failure, 'old_source_check_unavailable'):
                    r.rollback(host, directory)
            self.assertTrue(host.gated)
            self.assertFalse(host.timer_active)
            self.assertFalse(host.apps[NEW]['State']['Running'])
            self.assertTrue(host.apps[OLD]['State']['Running'])
            starts = host.events.count('start_1111')
            self.assertEqual(r.recover(host, directory)['phase'], 'rolled_back')
            self.invariant(host, original)
            self.assertEqual(host.events.count('start_1111'), starts)

    def test_detached_system_service_contract(self):
        directory = r.ROOT/'run-20261106T010203Z'
        command = r.service_command(directory)
        self.assertTrue(command[1].startswith('--unit=sub2-stream-preheader-'))
        self.assertIn('--no-block', command)
        self.assertIn('--property=Type=oneshot', command)
        self.assertIn('--property=TimeoutStopSec=600', command)
        self.assertIn('--property=KillMode=control-group', command)
        self.assertTrue(any(value.startswith('--property=ExecStopPost=/usr/bin/python3 ') and ' recover --run-dir ' in value for value in command))
        self.assertNotIn('--scope', command)
        self.assertNotIn('--pty', command)
        self.assertNotIn('--quiet-window-confirmed', command)
        retry = r.service_command(directory, 'rollback', 2)
        self.assertTrue(retry[1].endswith('-rollback-2'))
        self.assertEqual(retry[-2:], ['--attempt', '2'])
        recover = r.service_command(directory, 'recover')
        self.assertFalse(any('ExecStopPost=' in value for value in recover))


if __name__ == '__main__':
    unittest.main(verbosity=2)
