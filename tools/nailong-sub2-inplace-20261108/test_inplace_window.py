#!/usr/bin/env python3
"""Unit & failure-fixture tests for Nailong same-host in-place Sub2 release & recovery carrier."""
import io
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest import mock

import inplace_window as iw


def make_valid_go(action='forward', **overrides):
    base = json.loads((iw.HERE / 'go.template.json').read_text(encoding='utf8'))
    base['explicit_parent_go'] = True
    base['action'] = action
    base['window'] = 'nailong-1d14bbdc3-20261108t1300z'
    base.update(overrides)
    return base


class FakeNailongOps:
    def __init__(self, go):
        self.go = go
        self.current_id = go['current_container_id']
        self.new_id = '99' * 32
        self.current_started_at = go['current_started_at']
        self.new_started_at = '2026-11-08T13:05:10.000000000Z'
        self.recovered_started_at = '2026-11-08T13:06:22.000000000Z'

        self.active_sub2 = {
            'id': self.current_id,
            'name': 'sub2api-gcp',
            'image': go['current_image'],
            'revision': go['current_revision'],
            'running': True,
            'health': 'healthy',
            'started_at': self.current_started_at,
            'restart_count': 0,
            'appdata': go['appdata'],
        }
        self.retained = None
        self.pg = {
            'id': go['pg']['id'],
            'name': go['pg']['name'],
            'running': True,
            'health': 'healthy',
            'started_at': go['pg']['started_at'],
        }
        self.redis = {
            'id': go['redis']['id'],
            'name': go['redis']['name'],
            'running': True,
            'health': 'healthy',
            'started_at': go['redis']['started_at'],
        }
        self.axon = {
            'id': go['axon_nailong']['id'],
            'name': go['axon_nailong']['name'],
            'running': True,
            'health': None,
            'started_at': go['axon_nailong']['started_at'],
        }
        self.thought_policy = dict(iw.EXPECTED_THOUGHT_POLICY)
        self.source_binding = {
            'app': {'name': 'sub2api-gcp', 'id': self.current_id, 'started_at': self.current_started_at},
            'pg': {'name': go['pg']['name'], 'id': go['pg']['id'], 'started_at': go['pg']['started_at']},
            'redis': {'name': go['redis']['name'], 'id': go['redis']['id'], 'started_at': go['redis']['started_at']},
            'dependency_network': 'sub2api-nailong',
            'appdata': go['appdata'],
        }
        self.timer_state = 'active'
        self.fail_on_stop_ack = False
        self.fail_on_create = False
        self.fail_new_container_stop_timeout = False
        self.fail_on_source_check = False
        self.fail_on_recover_source_check = False
        self.original_start_calls = 0
        self.admitted_pairs = {
            (go['current_image'], go['current_revision']),
            (go['target_image'], go['target_revision']),
        }

    def observe_live(self, target_revision):
        return {
            'app': dict(self.active_sub2) if self.active_sub2 else None,
            'pg': dict(self.pg),
            'redis': dict(self.redis),
            'axon_nailong': dict(self.axon),
            'loaded_target': {
                'id': self.go['target_image'],
                'revision': self.go['target_revision'],
                'repo_tags': ['sub2api:1d14bbdc3'],
            },
            'thought_policy': dict(self.thought_policy),
            'backup_timer': self.timer_state,
            'source_check': {
                'stage': 'core_backup_source_bound',
                'image': self.active_sub2['image'] if self.active_sub2 else None,
                'revision': self.active_sub2['revision'] if self.active_sub2 else None,
                'source': json.loads(json.dumps(self.source_binding)),
            },
            'nailong_identity_sha': iw.PARENT_CORE_IDENTITY_SHA,
            'lag_identity_sha': iw.PARENT_CORE_IDENTITY_SHA,
            'docker_api141_probe': {
                'ok': True,
                'api_version': '1.41',
                'app_id': self.current_id,
                'target_image_id': self.go['target_image'],
            },
        }

    def admit_core_identity_pair(self, candidate_bytes, target_image, target_revision):
        self.admitted_pairs.add((target_image, target_revision))
        return {'nailong_sha256': 'admitted_sha', 'lag_sha256': 'admitted_sha', 'pair': [target_image, target_revision]}

    def capture_current_container_spec(self, expected_id, expected_appdata):
        assert self.active_sub2['id'] == expected_id
        assert self.active_sub2['appdata'] == expected_appdata
        return {
            'id': expected_id,
            'env': ['JWT_SECRET=jwt-fixed', 'TOTP_ENCRYPTION_KEY=totp-fixed', 'DATABASE_HOST=postgres', 'REDIS_HOST=redis'],
            'appdata': expected_appdata,
        }

    def stop_and_rename_current(self, expected_id, retained_name):
        assert self.active_sub2 and self.active_sub2['id'] == expected_id
        stopped = dict(self.active_sub2, name=retained_name, running=False)
        self.retained = stopped
        self.active_sub2 = None
        if self.fail_on_stop_ack:
            raise iw.InplaceError('ssh_connection_reset_after_remote_stop')
        return {'id': expected_id, 'retained_name': retained_name, 'exit_code': 0}

    def create_and_start_target(self, window, target_image, target_revision, env_list, appdata):
        if self.fail_on_create:
            still_running = bool(self.fail_new_container_stop_timeout)
            self.active_sub2 = {
                'id': self.new_id,
                'name': 'sub2api-gcp',
                'image': target_image,
                'revision': target_revision,
                'running': still_running,
                'health': 'unhealthy',
                'started_at': self.new_started_at,
                'restart_count': 1,
                'appdata': appdata,
            }
            raise iw.InplaceError('target_exited_or_restarted')
        self.active_sub2 = {
            'id': self.new_id,
            'name': 'sub2api-gcp',
            'image': target_image,
            'revision': target_revision,
            'running': True,
            'health': 'healthy',
            'started_at': self.new_started_at,
            'restart_count': 0,
            'appdata': appdata,
            'loopback_http': 200,
        }
        return dict(self.active_sub2)

    def observe_containers(self, original_id, retained_name):
        return {
            'active_sub2': dict(self.active_sub2) if self.active_sub2 else None,
            'original_by_id': dict(self.retained) if self.retained and self.retained['id'] == original_id else (
                dict(self.active_sub2) if self.active_sub2 and self.active_sub2['id'] == original_id else None
            ),
            'retained': dict(self.retained) if self.retained and self.retained['name'] == retained_name else None,
        }

    def stop_and_remove_new_container(self, new_container_id, failed_archive_name):
        assert self.active_sub2 and self.active_sub2['id'] == new_container_id
        if self.fail_new_container_stop_timeout:
            raise iw.InplaceError('new_container_stop_timeout_still_running')
        self.active_sub2 = None
        return {
            'stopped_id': new_container_id,
            'running': False,
            'exit_code': 0,
            'archived_as': failed_archive_name,
        }

    def restore_and_start_original_container(self, original_id, retained_name):
        self.original_start_calls += 1
        assert self.retained and self.retained['id'] == original_id
        self.active_sub2 = {
            'id': original_id,
            'name': 'sub2api-gcp',
            'image': self.go['current_image'],
            'revision': self.go['current_revision'],
            'running': True,
            'health': 'healthy',
            'started_at': self.recovered_started_at,
            'restart_count': 0,
            'appdata': self.go['appdata'],
            'loopback_http': 200,
        }
        self.retained = None
        return dict(self.active_sub2)

    def inspect_backup_roles(self):
        return {
            'binding': json.loads(json.dumps(self.source_binding)),
            'roles': {
                'app': dict(self.active_sub2),
                'pg': dict(self.pg),
                'redis': dict(self.redis),
            },
            'appdata': self.active_sub2['appdata'],
        }

    def write_source_binding(self, binding):
        self.source_binding = json.loads(json.dumps(binding))

    def run_source_check(self):
        if self.fail_on_source_check and self.active_sub2 and self.active_sub2['id'] == self.new_id:
            raise iw.InplaceError('source_app_identity_or_health_changed')
        if self.fail_on_recover_source_check and self.active_sub2 and self.active_sub2['id'] == self.current_id:
            raise iw.InplaceError('source_check_failed_on_recover')
        app_id = self.source_binding['app']['id']
        app_started = self.source_binding['app']['started_at']
        if not self.active_sub2 or self.active_sub2['id'] != app_id or self.active_sub2['started_at'] != app_started:
            raise iw.InplaceError('source_app_identity_or_health_changed')
        return {
            'stage': 'core_backup_source_bound',
            'image': self.active_sub2['image'],
            'revision': self.active_sub2['revision'],
            'source': json.loads(json.dumps(self.source_binding)),
        }

    def ensure_backup_timer_active(self):
        self.timer_state = 'active'

    def backup_timer_state(self):
        return self.timer_state


class FakeDocker20Daemon:
    """Simulates Nailong's Docker 20.10.24 Unix socket daemon: rejects `/v1.45` with HTTP 400 and serves `/v1.41`."""

    def __init__(self, go):
        self.go = go
        self.requests = []
        self.containers = {
            go['current_container_id']: {
                'Id': go['current_container_id'],
                'Name': '/sub2api-gcp',
                'Image': go['current_image'],
                'Config': {
                    'Labels': {'org.opencontainers.image.revision': go['current_revision']},
                    'Env': ['JWT_SECRET=s1', 'TOTP_ENCRYPTION_KEY=t1'],
                },
                'State': {
                    'Running': True,
                    'Status': 'running',
                    'ExitCode': 0,
                    'StartedAt': go['current_started_at'],
                    'Health': {'Status': 'healthy'},
                },
                'RestartCount': 0,
                'HostConfig': {'RestartPolicy': {'Name': 'unless-stopped'}},
                'Mounts': [{'Destination': '/app/data', 'Source': go['appdata']}],
            },
        }
        self.images = {
            go['target_image']: {
                'Id': go['target_image'],
                'RepoTags': ['sub2api:1d14bbdc3'],
                'Config': {'Labels': {'org.opencontainers.image.revision': go['target_revision']}},
            },
        }
        self.new_id = 'ab' * 32

    def resolve_container(self, ref):
        if ref in self.containers:
            return self.containers[ref]
        for c in self.containers.values():
            if c['Name'].lstrip('/') == ref:
                return c
        return None

    def handle(self, method, path, payload):
        self.requests.append((method, path, payload))
        if not path.startswith('/v1.41/'):
            version = path.split('/', 2)[1] if path.startswith('/v') else 'unknown'
            msg = f'client version {version.lstrip("v")} is too new. Maximum supported API version is 1.41'
            return 400, json.dumps({'message': msg}).encode('utf8')

        sub = path[len('/v1.41'):]
        if method == 'GET' and sub.startswith('/containers/') and sub.endswith('/json'):
            ref = sub[len('/containers/'):-len('/json')]
            c = self.resolve_container(ref)
            if not c:
                return 404, json.dumps({'message': f'No such container: {ref}'}).encode('utf8')
            return 200, json.dumps(c).encode('utf8')

        if method == 'GET' and sub.startswith('/images/') and sub.endswith('/json'):
            ref = sub[len('/images/'):-len('/json')]
            img = self.images.get(ref)
            if not img:
                return 404, json.dumps({'message': f'No such image: {ref}'}).encode('utf8')
            return 200, json.dumps(img).encode('utf8')

        if method == 'POST' and sub.startswith('/containers/') and sub.endswith('/update'):
            ref = sub[len('/containers/'):-len('/update')]
            c = self.resolve_container(ref)
            body = json.loads(payload.decode('utf8'))
            c['HostConfig']['RestartPolicy'] = body['RestartPolicy']
            return 200, b'{"Warnings":null}'

        if method == 'POST' and sub.startswith('/containers/') and '/kill?' in sub:
            ref = sub[len('/containers/'):sub.index('/kill?')]
            c = self.resolve_container(ref)
            c['State']['Running'] = False
            c['State']['Status'] = 'exited'
            c['State']['ExitCode'] = 0
            return 204, b''

        if method == 'POST' and sub.startswith('/containers/') and '/rename?name=' in sub:
            ref, new_name = sub[len('/containers/'):].split('/rename?name=', 1)
            c = self.resolve_container(ref)
            c['Name'] = '/' + new_name
            return 204, b''

        if method == 'POST' and sub == '/containers/create?name=sub2api-gcp':
            body = json.loads(payload.decode('utf8'))
            # Docker 20 API 1.41 rejects >1 EndpointsConfig entry at create time
            if len(body.get('NetworkingConfig', {}).get('EndpointsConfig', {})) != 1:
                return 400, json.dumps({'message': 'Container cannot be connected to network endpoints'}).encode('utf8')
            self.containers[self.new_id] = {
                'Id': self.new_id,
                'Name': '/sub2api-gcp',
                'Image': body['Image'],
                'Config': {
                    'Labels': {'org.opencontainers.image.revision': self.go['target_revision']},
                    'Env': body.get('Env', []),
                },
                'State': {
                    'Running': False,
                    'Status': 'created',
                    'ExitCode': 0,
                    'StartedAt': '2026-11-08T13:20:00.000000000Z',
                    'Health': {'Status': 'healthy'},
                },
                'RestartCount': 0,
                'HostConfig': body['HostConfig'],
                'Mounts': [{'Destination': '/app/data', 'Source': self.go['appdata']}],
            }
            return 201, json.dumps({'Id': self.new_id, 'Warnings': []}).encode('utf8')

        if method == 'POST' and sub == '/networks/sub2api-nailong/connect':
            return 200, b''

        if method == 'POST' and sub.startswith('/containers/') and sub.endswith('/start'):
            ref = sub[len('/containers/'):-len('/start')]
            c = self.resolve_container(ref)
            c['State']['Running'] = True
            c['State']['Status'] = 'running'
            if c['Id'] == self.go['current_container_id']:
                c['State']['StartedAt'] = '2026-11-08T13:21:00.000000000Z'
            return 204, b''

        return 500, json.dumps({'message': f'Unhandled {method} {path}'}).encode('utf8')


class InplaceWindowTests(unittest.TestCase):
    def test_validate_go_refuses_template_and_scope_violations(self):
        go = make_valid_go()
        self.assertEqual(iw.validate_go(go, 'forward')['window'], 'nailong-1d14bbdc3-20261108t1300z')

        with self.assertRaisesRegex(iw.InplaceError, 'template_is_not_a_go'):
            iw.validate_go(go, 'forward', filename='go.template.json')

        with self.assertRaisesRegex(iw.InplaceError, 'explicit_parent_go_required'):
            iw.validate_go(make_valid_go(explicit_parent_go=False), 'forward')

        with self.assertRaisesRegex(iw.InplaceError, 'scope_guard_violation'):
            iw.validate_go(make_valid_go(dns_apply=True), 'forward')

        with self.assertRaisesRegex(iw.InplaceError, 'scope_guard_violation'):
            iw.validate_go(make_valid_go(forbid_old_gcp_fallback=False), 'forward')

        with self.assertRaisesRegex(iw.InplaceError, 'thought_policy_contract_mismatch'):
            iw.validate_go(make_valid_go(thought_policy={
                'suppress_thought_only_stop': True,
                'continue_thought_only': True,
                'continue_zero_output': False,
            }), 'forward')

        for protected in iw.PROTECTED_UNITS:
            with self.assertRaisesRegex(iw.InplaceError, 'protected_unit_stop_forbidden'):
                iw.assert_stoppable(protected)

    def test_error_label_preserves_specific_sanitized_codes_and_never_generic_failure(self):
        class Failure(Exception):
            pass

        class ContractError(Exception):
            pass

        self.assertEqual(iw.error_label(Failure('docker_api_failed')), 'docker_api_failed')
        self.assertEqual(
            iw.error_label(Failure('docker_api_400:client version 1.45 is too new. Maximum supported API version is 1.41')),
            'docker_api_400:client version 1.45 is too new. Maximum supported API version is 1.41',
        )
        self.assertEqual(iw.error_label(ContractError('create_seam_mismatch')), 'create_seam_mismatch')
        self.assertEqual(iw.error_label(iw.InplaceError('target_exited_or_restarted')), 'target_exited_or_restarted')
        # Secret-bearing text must never be echoed
        self.assertEqual(iw.error_label(iw.InplaceError('REDIS_PASSWORD=secret_leaked')), 'inplace_error')

    def test_real_local_nailong_ops_binds_api141_and_executes_stop_create_and_recover(self):
        """Exercises the real LocalNailongOps methods against a simulated Docker 20.10.24 daemon that rejects /v1.45."""
        go = make_valid_go()
        daemon = FakeDocker20Daemon(go)

        tools_path = str(Path('C:/Programs/AI/MLAIGateWay/outputs/task53-sub2-nailong-20261107').resolve())
        if tools_path not in sys.path:
            sys.path.insert(0, tools_path)
        import contract as real_contract

        fake_migration = types.ModuleType('migration')

        class Failure(Exception):
            pass

        class FakeConn:
            def __init__(self):
                self._res = None

            def request(self, method, path, payload=None, headers=None):
                status, raw = daemon.handle(method, path, payload)
                self._res = (status, raw)

            def getresponse(self):
                status, raw = self._res
                res = types.SimpleNamespace(status=status, read=lambda: raw)
                return res

            def close(self):
                pass

        # Simulate Nailong's unpatched `/opt/sub2api-nailong/tools/migration.py` which calls `/v1.45`!
        def unpatched_api(method, path, body=None):
            conn = FakeConn()
            conn.request(method, '/v1.45' + path)
            res = conn.getresponse()
            if res.status not in (200, 201, 204):
                raise Failure('docker_api_failed')
            return res.read()

        fake_migration.Failure = Failure
        fake_migration.DockerConnection = FakeConn
        fake_migration.api = unpatched_api
        fake_migration.inspect = lambda name: json.loads(fake_migration.api('GET', '/containers/' + name + '/json'))

        # Verify unpatched migration.inspect fails with Failure('docker_api_failed') on Docker 20.10
        with self.assertRaises(Failure):
            fake_migration.inspect('sub2api-gcp')

        with mock.patch.dict(sys.modules, {'migration': fake_migration, 'contract': real_contract}):
            local_ops = iw.LocalNailongOps(tools_dir=None)
            # The separate ReleaseDrainTests exercise admission/connection safety;
            # this fixture owns Docker 1.41 create/stop/recovery compatibility.
            local_ops.acquire_release_idle = mock.Mock(return_value={'mode': 'application_admission_hold', 'in_flight': 0})
            probe = local_ops.probe_docker_api141(go['current_container_id'], go['target_image'])
            self.assertTrue(probe['ok'])
            self.assertEqual(probe['api_version'], '1.41')

            spec = local_ops.capture_current_container_spec(go['current_container_id'], go['appdata'])
            self.assertEqual(spec['id'], go['current_container_id'])

            retained = iw.archive_container_name(go['window'], go['current_container_id'])
            stopped = local_ops.stop_and_rename_current(go['current_container_id'], retained)
            self.assertEqual(stopped['retained_name'], retained)
            self.assertFalse(daemon.containers[go['current_container_id']]['State']['Running'])

            fake_http_resp = mock.MagicMock()
            fake_http_resp.status = 200
            with mock.patch('urllib.request.urlopen', return_value=fake_http_resp):
                started = local_ops.create_and_start_target(
                    window=go['window'],
                    target_image=go['target_image'],
                    target_revision=go['target_revision'],
                    env_list=spec['env'],
                    appdata=go['appdata'],
                )
                self.assertEqual(started['id'], daemon.new_id)
                self.assertEqual(started['health'], 'healthy')
                create_body = next(json.loads(payload) for method, path, payload in daemon.requests if path.startswith('/v1.41/containers/create'))
                self.assertEqual(create_body['StopTimeout'], 360)
                self.assertIn('SERVER_SHUTDOWN_TIMEOUT_SECONDS=300', create_body['Env'])

                # Now exercise LocalNailongOps stop_and_remove_new_container + restore_and_start_original_container
                removed = local_ops.stop_and_remove_new_container(daemon.new_id, 'sub2api-gcp-failed-test')
                self.assertFalse(removed['running'])
                restored = local_ops.restore_and_start_original_container(go['current_container_id'], retained)
                self.assertEqual(restored['id'], go['current_container_id'])
                self.assertEqual(restored['name'], 'sub2api-gcp')
                self.assertEqual(restored['started_at'], '2026-11-08T13:21:00.000000000Z')

        # Every request issued by LocalNailongOps must have used `/v1.41/`
        local_ops_paths = [p for _, p, _ in daemon.requests[1:]]
        self.assertTrue(all(p.startswith('/v1.41/') for p in local_ops_paths), local_ops_paths)

    def test_forward_happy_path_rebinds_daily_backup_to_new_container_epoch(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        with tempfile.TemporaryDirectory() as tmp:
            result = iw.forward(go, ops, directory=Path(tmp))
            self.assertEqual(result['stage'], 'forward_terminal')
            self.assertEqual(result['app']['id'], ops.new_id)
            self.assertEqual(result['app']['image'], go['target_image'])
            self.assertEqual(result['app']['revision'], go['target_revision'])
            self.assertEqual(ops.source_binding['app']['id'], ops.new_id)
            self.assertEqual(ops.source_binding['app']['started_at'], ops.new_started_at)
            self.assertEqual(result['backup']['source']['app']['id'], ops.new_id)
            self.assertEqual(result['backup']['source']['app']['started_at'], ops.new_started_at)
            self.assertEqual(result['pg_preserved'], go['pg']['id'])
            self.assertEqual(result['redis_preserved'], go['redis']['id'])
            self.assertEqual(result['appdata_preserved'], go['appdata'])
            self.assertFalse(result['old_gcp_touched'])

    def test_failure_fixture_lost_ack_on_stop_current_still_converges_via_recover(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        ops.fail_on_stop_ack = True
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            with self.assertRaisesRegex(iw.InplaceError, 'ssh_connection_reset_after_remote_stop'):
                iw.forward(go, ops, directory=tmp_path)

            self.assertTrue((tmp_path / 'stop-intent.json').is_file())
            self.assertTrue((tmp_path / 'stop-observed-after-error.json').is_file())
            self.assertTrue((tmp_path / 'current-stopped.json').is_file())
            self.assertTrue((tmp_path / 'recover-terminal.json').is_file())
            self.assertIsNotNone(ops.active_sub2)
            self.assertEqual(ops.active_sub2['id'], go['current_container_id'])
            self.assertTrue(ops.active_sub2['running'])
            self.assertEqual(ops.source_binding['app']['id'], go['current_container_id'])
            self.assertEqual(ops.source_binding['app']['started_at'], ops.recovered_started_at)

    def test_failure_fixture_new_container_stop_timeout_refuses_dual_active_start_of_old(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        ops.fail_on_create = True
        ops.fail_new_container_stop_timeout = True
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            with self.assertRaisesRegex(iw.InplaceError, 'new_container_still_running_or_stop_unknown'):
                iw.forward(go, ops, directory=tmp_path)

            self.assertEqual(ops.original_start_calls, 0)
            self.assertFalse((tmp_path / 'recover-terminal.json').exists())
            status = json.loads((tmp_path / 'status.json').read_text(encoding='utf8'))
            self.assertEqual(status['stage'], 'recovery_requires_operator')
            self.assertEqual(status['reason'], 'new_container_still_running_or_stop_unknown')
            self.assertEqual(status['error'], 'new_container_stop_timeout_still_running')
            self.assertFalse(status['original_started'])

    def test_failure_fixture_target_start_failure_auto_recovers_original_container_and_rebinds_new_started_at(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        ops.fail_on_create = True
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            with self.assertRaisesRegex(iw.InplaceError, 'target_exited_or_restarted'):
                iw.forward(go, ops, directory=tmp_path)

            self.assertIsNotNone(ops.active_sub2)
            self.assertEqual(ops.active_sub2['id'], go['current_container_id'])
            self.assertEqual(ops.active_sub2['image'], go['current_image'])
            self.assertTrue(ops.active_sub2['running'])
            self.assertEqual(ops.source_binding['app']['id'], go['current_container_id'])
            self.assertEqual(ops.source_binding['app']['started_at'], ops.recovered_started_at)
            self.assertNotEqual(ops.source_binding['app']['started_at'], go['current_started_at'])
            self.assertTrue((tmp_path / 'recover-terminal.json').is_file())
            self.assertTrue((tmp_path / 'new-container-removed.json').is_file())

    def test_failure_fixture_backup_rebind_mismatch_triggers_same_host_recovery_so_no_unbound_new_app_remains(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        ops.fail_on_source_check = True
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            with self.assertRaisesRegex(iw.InplaceError, 'source_app_identity_or_health_changed'):
                iw.forward(go, ops, directory=tmp_path)

            self.assertEqual(ops.active_sub2['id'], go['current_container_id'])
            self.assertEqual(ops.active_sub2['image'], go['current_image'])
            self.assertEqual(ops.source_binding['app']['id'], go['current_container_id'])
            self.assertEqual(ops.source_binding['app']['started_at'], ops.recovered_started_at)
            recover_doc = json.loads((tmp_path / 'recover-terminal.json').read_text(encoding='utf8'))
            self.assertEqual(recover_doc['stage'], 'recovered_to_previous_image')

    def test_failure_fixture_pg_or_thought_policy_drift_aborts_before_stopping_sub2(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        ops.thought_policy['continue_thought_only'] = True
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            with self.assertRaisesRegex(iw.InplaceError, 'thought_policy_drifted'):
                iw.forward(go, ops, directory=tmp_path)
            self.assertFalse((tmp_path / 'stop-intent.json').exists())
            self.assertFalse((tmp_path / 'current-stopped.json').exists())
            self.assertTrue(ops.active_sub2['running'])
            self.assertEqual(ops.active_sub2['id'], go['current_container_id'])

    def test_candidate_core_identity_admits_1d14bbdc3_and_preserves_existing_pairs(self):
        ns = {}
        code = iw.CANDIDATE_CORE_IDENTITY.read_text(encoding='utf8')
        exec(compile(code, str(iw.CANDIDATE_CORE_IDENTITY), 'exec'), ns)
        self.assertTrue(ns['admits'](
            'sha256:ad403de503301cc681c18b961a3532a8c739b4bed1b1b7de3043a9073b3c5e61',
            '1d14bbdc38c25e4279f6948c7128c4a43e6ef985',
        ))
        self.assertTrue(ns['admits'](
            'sha256:8a087d23f70de512fcb9b064b9b088fa453650dbf2e8d4b05929f9130c0937a7',
            'fc899738487a38a7d40664fdf92bc90375fbaf7b',
        ))
        self.assertFalse(ns['admits'](
            'sha256:ad403de503301cc681c18b961a3532a8c739b4bed1b1b7de3043a9073b3c5e61',
            'fc899738487a38a7d40664fdf92bc90375fbaf7b',
        ))


if __name__ == '__main__':
    unittest.main()
