#!/usr/bin/env python3
"""Unit & failure-fixture tests for Nailong same-host in-place Sub2 release & recovery carrier."""
import json
from pathlib import Path
import tempfile
import unittest

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
        self.fail_on_create = False
        self.fail_on_source_check = False
        self.fail_on_recover_source_check = False
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
            'nailong_identity_sha': iw.CURRENT_CORE_IDENTITY_SHA,
            'lag_identity_sha': iw.CURRENT_CORE_IDENTITY_SHA,
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
        return {'id': expected_id, 'retained_name': retained_name, 'exit_code': 0}

    def create_and_start_target(self, window, target_image, target_revision, env_list, appdata):
        if self.fail_on_create:
            # Simulate a failed target container that was created under `sub2api-gcp` but exited unhealthy.
            self.active_sub2 = {
                'id': self.new_id,
                'name': 'sub2api-gcp',
                'image': target_image,
                'revision': target_revision,
                'running': False,
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
        self.active_sub2 = None
        return {'stopped_id': new_container_id, 'archived_as': failed_archive_name}

    def restore_and_start_original_container(self, original_id, retained_name):
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

    def test_forward_happy_path_rebinds_daily_backup_to_new_container_epoch(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        with tempfile.TemporaryDirectory() as tmp:
            result = iw.forward(go, ops, directory=Path(tmp))
            self.assertEqual(result['stage'], 'forward_terminal')
            self.assertEqual(result['app']['id'], ops.new_id)
            self.assertEqual(result['app']['image'], go['target_image'])
            self.assertEqual(result['app']['revision'], go['target_revision'])
            # Daily backup source.json must be rebound to the new container ID and StartedAt (not stale ca2f68cf!)
            self.assertEqual(ops.source_binding['app']['id'], ops.new_id)
            self.assertEqual(ops.source_binding['app']['started_at'], ops.new_started_at)
            self.assertEqual(result['backup']['source']['app']['id'], ops.new_id)
            self.assertEqual(result['backup']['source']['app']['started_at'], ops.new_started_at)
            self.assertEqual(result['pg_preserved'], go['pg']['id'])
            self.assertEqual(result['redis_preserved'], go['redis']['id'])
            self.assertEqual(result['appdata_preserved'], go['appdata'])
            self.assertFalse(result['old_gcp_touched'])

    def test_failure_fixture_target_start_failure_auto_recovers_original_container_and_rebinds_new_started_at(self):
        go = make_valid_go()
        ops = FakeNailongOps(go)
        ops.fail_on_create = True
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            with self.assertRaisesRegex(iw.InplaceError, 'target_exited_or_restarted'):
                iw.forward(go, ops, directory=tmp_path)

            # Verify automatic same-host recovery restored the original container on the same PG/Redis/appdata
            self.assertIsNotNone(ops.active_sub2)
            self.assertEqual(ops.active_sub2['id'], go['current_container_id'])
            self.assertEqual(ops.active_sub2['image'], go['current_image'])
            self.assertTrue(ops.active_sub2['running'])
            # Crucial: restarting the original container changes its Docker StartedAt;
            # source.json must be rebound to recovered_started_at so daily backup does not fail!
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

            # Because daily backup source-check failed on the new container (e.g. if core_identity pair was not admitted),
            # forward() automatically recovers back to the previous container and rebinds source.json to it.
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
