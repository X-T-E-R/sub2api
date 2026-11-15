"""Dual-slot ordering, failure recovery, backup binding and Docker API contracts."""
import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock
import inplace_window as iw


def make_valid_go(action='forward', **overrides):
    go = json.loads((iw.HERE / 'go.template.json').read_text())
    go.update(explicit_parent_go=True, action=action, window='bluegreen-fixture-20261115')
    go.update(overrides)
    return go


class FakeNailongOps:
    def __init__(self, go):
        self.go = go
        self.old = dict(id=go['current_container_id'], name=iw.APP_NAME, image=go['current_image'],
                        revision=go['current_revision'], started_at=go['current_started_at'],
                        running=True, health='healthy', restart_count=0, appdata=go['appdata'], port=go['current_port'])
        self.active = self.old
        self.new = None
        self.calls = []
        self.fail = None
        self.binding = {'dependency_network': iw.DEPENDENCY_NETWORK, 'appdata': go['appdata']}
        for role, info in self.roles().items():
            self.binding[role] = {k: info[k] for k in ('name', 'id', 'started_at')}

    def roles(self):
        return {'app': self.active, **{k: dict(self.go[k], running=True, health='healthy') for k in ('pg', 'redis')}}

    def observe_live(self, rev):
        return dict(app=self.active, loaded_target={'id': self.go['target_image'], 'revision': self.go['target_revision']},
                    thought_policy=iw.EXPECTED_THOUGHT_POLICY,
                    **{k: dict(self.go[k], running=True, health='healthy') for k in ('pg', 'redis', 'axon_nailong')})

    def routed_container(self):
        return self.active

    def admit_core_identity_pair(self, *args):
        return {'admitted': True}

    def verify_peer_safe_image(self, *args):
        pass

    def capture_current_container_spec(self, cid, appdata):
        self.calls.append('capture')
        return {'env': ['JWT_SECRET=fixture', 'TOTP_ENCRYPTION_KEY=fixture'], 'port': self.active['port']}

    def create_and_start_target(self, window, image, rev, env, appdata, port, directory):
        self.calls.append('warm')
        assert self.old['running'] and self.active is self.old
        self.new = dict(self.old, id='99'*32, name='standby', image=image, revision=rev, port=port,
                        started_at='2026-11-15T09:00:00Z', health='healthy')
        iw.write_atomic(directory, 'target-created.json', {'id': self.new['id']})
        if self.fail == 'warm':
            raise iw.InplaceError('target_readiness_timeout')
        return self.new

    def make_canonical(self, cid, window):
        pass

    def activate_slot(self, expected_id, target_id, window):
        self.calls.append('switch')
        assert self.active['id'] == expected_id
        old_port = self.active['port']
        self.active = self.old if target_id == self.old['id'] else self.new
        self.active['name'] = iw.APP_NAME
        if self.fail == 'lost-switch-ack':
            self.fail = None
            raise iw.InplaceError('lost_switch_ack')
        return {'from_port': old_port, 'to_port': self.active['port'], 'retiring_workers': [12]}

    def prepare_original(self, go, directory):
        self.calls.append('prepare-original')
        self.old['running'] = True
        return self.old

    def schedule_retirement(self, cid, workers, directory):
        self.calls.append('drain')
        assert self.active['id'] != cid
        (self.old if cid == self.old['id'] else self.new)['running'] = False

    def discard_unrouted_candidate(self, cid, directory):
        self.calls.append('discard')
        assert self.active['id'] != cid
        self.new['running'] = False

    def inspect_backup_roles(self):
        return {'binding': self.binding, 'roles': self.roles(), 'appdata': self.go['appdata']}

    def write_source_binding(self, binding):
        self.calls.append('bind')
        self.binding = binding

    def run_source_check(self):
        if self.fail == 'backup' and self.active is self.new:
            raise iw.InplaceError('backup_fixture_failure')
        return {'stage': 'core_backup_source_bound', 'image': self.active['image'],
                'revision': self.active['revision'], 'source': self.binding}

    def ensure_backup_timer_active(self):
        pass

    def backup_timer_state(self):
        return 'active'


class BlueGreenTests(unittest.TestCase):
    def test_go_rejects_stop_first_schema_and_template(self):
        iw.validate_go(make_valid_go(), 'forward')
        for overrides in ({'schema': 'nailong-sub2-inplace-go-1'}, {'dual_slot': False}, {'current_port': 8080}, {'worker_apply': True}):
            with self.assertRaises(iw.InplaceError):
                iw.validate_go(make_valid_go(**overrides), 'forward')
        with self.assertRaisesRegex(iw.InplaceError, 'template_is_not_a_go'):
            iw.validate_go(make_valid_go(), 'forward', 'go.template.json')
        for name in iw.PROTECTED_UNITS:
            with self.assertRaises(iw.InplaceError):
                iw.assert_stoppable(name)

    def test_both_slot_directions_warm_switch_bind_then_drain(self):
        for port in iw.SLOT_PORTS:
            with self.subTest(port=port), tempfile.TemporaryDirectory() as tmp:
                go = make_valid_go(current_port=port)
                ops = FakeNailongOps(go)
                result = iw.forward(go, ops, directory=tmp)
                self.assertEqual(result['stage'], 'forward_terminal')
                self.assertEqual(ops.calls, ['capture', 'warm', 'switch', 'bind', 'drain'])
                self.assertNotEqual(result['route']['to_port'], port)
                self.assertEqual(ops.binding['app']['id'], ops.new['id'])
                self.assertEqual(ops.binding['app']['started_at'], ops.new['started_at'])
                self.assertTrue(ops.new['running'])
                self.assertFalse(ops.old['running'])

    def test_warm_failure_never_switches_or_stops_original(self):
        ops = FakeNailongOps(make_valid_go())
        ops.fail = 'warm'
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(iw.InplaceError, 'target_readiness_timeout'):
                iw.forward(ops.go, ops, directory=tmp)
        self.assertNotIn('switch', ops.calls)
        self.assertNotIn('drain', ops.calls)
        self.assertIs(ops.active, ops.old)
        self.assertTrue(ops.old['running'])
        self.assertFalse(ops.new['running'])

    def test_lost_reload_ack_reconciles_actual_routing_and_swaps_back(self):
        ops = FakeNailongOps(make_valid_go())
        ops.fail = 'lost-switch-ack'
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(iw.InplaceError, 'lost_switch_ack'):
                iw.forward(ops.go, ops, directory=tmp)
            self.assertTrue((Path(tmp) / 'recover-terminal.json').exists())
        self.assertIs(ops.active, ops.old)
        self.assertEqual(ops.calls.count('switch'), 2)
        self.assertTrue(ops.old['running'])

    def test_backup_failure_swaps_back_before_draining_candidate(self):
        ops = FakeNailongOps(make_valid_go())
        ops.fail = 'backup'
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(iw.InplaceError, 'backup_fixture_failure'):
                iw.forward(ops.go, ops, directory=tmp)
        self.assertIs(ops.active, ops.old)
        self.assertEqual(ops.binding['app']['id'], ops.old['id'])
        self.assertEqual(ops.calls[:7], ['capture', 'warm', 'switch', 'bind', 'prepare-original', 'switch', 'bind'])

    def test_explicit_rollback_warms_old_before_switch_and_rebind(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        with tempfile.TemporaryDirectory() as tmp:
            iw.forward(go, ops, directory=tmp)
            ops.calls.clear()
            result = iw.recover(dict(go, action='recover'), ops, directory=tmp)
        self.assertEqual(result['stage'], 'recovered_to_previous_image')
        self.assertEqual(ops.calls, ['prepare-original', 'switch', 'bind', 'drain'])
        self.assertTrue(ops.old['running'])
        self.assertFalse(ops.new['running'])
        self.assertEqual(ops.binding['app']['id'], go['current_container_id'])

    def test_commissioning_rollback_roundtrip_finishes_on_new_slot(self):
        go = make_valid_go(verify_rollback=True)
        ops = FakeNailongOps(go)
        with tempfile.TemporaryDirectory() as tmp:
            result = iw.forward(go, ops, directory=tmp)
            self.assertTrue((Path(tmp) / 'rollback-verified.json').exists())
        self.assertEqual(ops.calls, ['capture', 'warm', 'switch', 'bind', 'switch', 'bind', 'switch', 'bind', 'drain'])
        self.assertIs(ops.active, ops.new)
        self.assertEqual(result['app']['id'], ops.new['id'])

    def test_sanitized_error_labels(self):
        self.assertEqual(iw.error_label(iw.InplaceError('REDIS_PASSWORD=private')), 'inplace_error')
        self.assertEqual(iw.error_label(iw.InplaceError('target_readiness_timeout')), 'target_readiness_timeout')


class LocalDockerTests(unittest.TestCase):
    def setUp(self):
        self.ops = object.__new__(iw.LocalNailongOps)
        self.ops.api141 = mock.Mock()

    def test_readiness_checks_health_and_readyz_on_standby_port(self):
        info = dict(image='image', revision='revision', running=True, restart_count=0, health='healthy', port=17862)
        self.ops._inspect_container = mock.Mock(return_value=info)
        response = mock.MagicMock()
        response.__enter__.return_value.status = 200
        with mock.patch.object(iw.urllib.request, 'urlopen', return_value=response) as urlopen:
            self.assertEqual(self.ops.wait_ready('cid', 'image', 'revision'), info)
        self.assertEqual([call.args[0] for call in urlopen.call_args_list],
                         ['http://127.0.0.1:17862/health', 'http://127.0.0.1:17862/readyz'])

    def test_create_uses_distinct_port_alias_and_preserves_both_networks(self):
        def body(*args):
            return {'HostConfig': {'PortBindings': {'8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '17861'}]}},
                    'NetworkingConfig': {'EndpointsConfig': {iw.APP_NETWORK: {'Aliases': [iw.ALIAS]}}}}
        self.ops._contract = types.SimpleNamespace(api141_create_body=body)
        self.ops._inspect_container = mock.Mock(side_effect=[None, {'id': 'cid', 'running': False, 'networks': [iw.APP_NETWORK]}])
        self.ops.api141.side_effect = [json.dumps({'Id': 'image', 'Config': {'Labels': {'org.opencontainers.image.revision': 'revision', 'io.sub2api.peer-safe-startup': 'true'}}}),
                                       json.dumps({'Id': 'cid'}), b'', b'']
        self.ops.wait_ready = mock.Mock(return_value={'id': 'cid'})
        with tempfile.TemporaryDirectory() as tmp:
            self.ops.create_and_start_target('window', 'image', 'revision', ['JWT_SECRET=fixed'], '/appdata', 17862, tmp)
        calls = self.ops.api141.call_args_list
        create = calls[1].args[2]
        self.assertEqual(create['HostConfig']['PortBindings']['8080/tcp'][0]['HostPort'], '17862')
        self.assertEqual(create['StopTimeout'], 360)
        self.assertIn('SERVER_SHUTDOWN_TIMEOUT_SECONDS=300', create['Env'])
        self.assertEqual(list(create['NetworkingConfig']['EndpointsConfig']), [iw.APP_NETWORK])
        self.assertNotIn(iw.ALIAS, create['NetworkingConfig']['EndpointsConfig'][iw.APP_NETWORK]['Aliases'])
        self.assertEqual(calls[2].args[1], '/networks/' + iw.DEPENDENCY_NETWORK + '/connect')
        self.assertFalse(any('disconnect' in call.args[1] or 'SIGUSR' in call.args[1] for call in calls))

    def test_cold_legacy_rollback_uses_pinned_peer_safe_rebuild_not_old_boot(self):
        go = make_valid_go(rollback_compatible={'image': 'sha256:' + 'a'*64, 'revision': 'b'*40})
        self.ops._inspect_container = mock.Mock(return_value={'id': go['current_container_id'], 'image': go['current_image'],
                                            'revision': go['current_revision'], 'running': False, 'port': 17861})
        self.ops.api141.return_value = json.dumps({'Config': {'Labels': {}}})
        self.ops.routed_container = mock.Mock(return_value={'id': 'serving', 'image': go['target_image'], 'revision': go['target_revision']})
        self.ops.admit_core_identity_pair = mock.Mock()
        self.ops.capture_current_container_spec = mock.Mock(return_value={'env': ['fixture']})
        self.ops.create_and_start_target = mock.Mock(return_value={'id': 'rollback-safe'})
        with tempfile.TemporaryDirectory() as tmp:
            result = self.ops.prepare_original(go, tmp)
        self.assertEqual(result['id'], 'rollback-safe')
        self.assertEqual(self.ops.create_and_start_target.call_args.args[1:3], ('sha256:' + 'a'*64, 'b'*40))
        self.assertFalse(any('/start' in call.args[1] for call in self.ops.api141.call_args_list))

    def test_lost_start_ack_resumes_existing_pinned_standby(self):
        self.ops.verify_peer_safe_image = mock.Mock()
        self.ops._contract = types.SimpleNamespace(api141_create_body=lambda *a: {
            'HostConfig': {'PortBindings': {'8080/tcp': [{'HostPort': '17861'}]}},
            'NetworkingConfig': {'EndpointsConfig': {iw.APP_NETWORK: {'Aliases': []}}}})
        self.ops._inspect_container = mock.Mock(return_value={'id': 'resumed', 'image': 'image', 'revision': 'revision',
                         'port': 17861, 'appdata': '/appdata', 'running': True, 'networks': [iw.APP_NETWORK, iw.DEPENDENCY_NETWORK]})
        self.ops.wait_ready = mock.Mock(return_value={'id': 'resumed'})
        with tempfile.TemporaryDirectory() as tmp:
            result = self.ops.create_and_start_target('rollback', 'image', 'revision', [], '/appdata', 17861, tmp)
        self.assertEqual(result['id'], 'resumed')
        self.ops.api141.assert_not_called()

    def test_old_restart_policy_is_disabled_before_route_switch(self):
        self.ops.routed_container = mock.Mock(return_value={'id': 'old', 'port': 17861})
        self.ops._inspect_container = mock.Mock(return_value={'id': 'new', 'port': 17862, 'image': 'image', 'revision': 'revision'})
        self.ops.api141.side_effect = [b'', json.dumps({'Config': {'Labels': {'io.sub2api.peer-safe-startup': 'true'}}}), b'']
        self.ops.wait_ready = mock.Mock()
        self.ops.make_canonical = mock.Mock()
        with mock.patch.object(iw.slot_routing, 'switch', return_value={}) as switch:
            self.ops.activate_slot('old', 'new', 'window')
            self.assertEqual(self.ops.api141.call_args_list[0].args, ('POST', '/containers/old/update', {'RestartPolicy': {'Name': 'no'}}))
            self.assertEqual(self.ops.api141.call_args_list[-1].args, ('POST', '/containers/new/update', {'RestartPolicy': {'Name': 'unless-stopped'}}))
            switch.assert_called_once_with(17861, 17862)

    def test_launcher_does_not_accept_forward_terminal_for_recover(self):
        ops = iw.RemoteNailongOps()
        ops._json_py = mock.Mock(side_effect=[{'execution_id': '123'},
            {'status': {'stage': 'forward_terminal', 'execution_id': '123'}, 'unit_state': 'inactive'}])
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.time, 'time_ns', return_value=123):
            with self.assertRaisesRegex(iw.InplaceError, 'forward_terminal'):
                ops.launch_via_systemd(make_valid_go('recover'), 'recover', tmp)
        launch_script = ops._json_py.call_args_list[0].args[1]
        self.assertIn("state['ActiveState'] not in ('inactive','failed')", launch_script)
        self.assertIn("state.get('Job')", launch_script)
        self.assertIn('fcntl.flock(lock, fcntl.LOCK_EX)', launch_script)

    def test_docker_141_binding(self):
        connection = mock.Mock()
        connection.getresponse.return_value = types.SimpleNamespace(status=200, read=lambda: b'{}')
        migration = types.SimpleNamespace(DockerConnection=lambda: connection)
        iw.bind_nailong_docker_api141(migration, types.SimpleNamespace())
        migration.api('GET', '/containers/sub2api-gcp/json')
        self.assertEqual(connection.request.call_args.args[1], '/v1.41/containers/sub2api-gcp/json')


if __name__ == '__main__':
    unittest.main()
