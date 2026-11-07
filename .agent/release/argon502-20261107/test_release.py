#!/usr/bin/env python3
"""Only the f183 -> 7c12 carrier deltas; no Docker, remote or provider calls."""
import copy
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch
import release as r

PRIOR = Path(__file__).resolve().parent.parent / 'stream-preheader-20261106'
spec = importlib.util.spec_from_file_location('prior_carrier_tests', PRIOR / 'test_release.py')
prior = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prior)
prior.OLD = r.OLD_CONTAINER
OLD, NEW = prior.OLD, prior.NEW


class Fake(prior.Fake):
    def __init__(self):
        super().__init__()
        app = self.apps[OLD]
        app['Config']['Env'] = ['EXAMPLE=fixture-only', r.PREHEADER_ENV, 'TAIL=preserved', 'EMPTY=']
        app['State']['StartedAt'] = r.OLD_STARTED_AT
        self.source['app']['started_at'] = r.OLD_STARTED_AT


class ReleaseDifference(unittest.TestCase):
    def setUp(self):
        self.bound = patch.object(r, 'IMAGE', NEW)
        self.bound.start()
        self.addCleanup(self.bound.stop)

    def test_exact_source_and_old_identity(self):
        self.assertEqual(r.SOURCE, '7c12de7380be0142e18bcdc6397b6537f2af9968')
        self.assertEqual(r.BASE, 'f18388a440ddce04c5da0d0917ba2e4db04a5b0e')
        host = Fake()
        plan = r.prepare(host, NEW)
        self.assertEqual(plan['old']['Id'], r.OLD_CONTAINER)
        self.assertEqual(plan['old']['StartedAt'], r.OLD_STARTED_AT)
        self.assertEqual(plan['old']['Image'], r.OLD_IMAGE)
        self.assertEqual(plan['base'], r.BASE)
        self.assertEqual(host.events, [])

    def test_unbound_image_cannot_prepare(self):
        host = Fake()
        with patch.object(r, 'IMAGE', r.UNBOUND_IMAGE):
            with self.assertRaisesRegex(r.Failure, 'release_identity_unbound'):
                r.prepare(host, r.UNBOUND_IMAGE)
        self.assertEqual(host.events, [])

    def test_env_unique_true_required_without_append_or_reorder(self):
        host = Fake()
        original = copy.deepcopy(host.apps[OLD]['Config'])
        selected = r.candidate_env(original)
        self.assertEqual(selected, original['Env'])
        self.assertIsNot(selected, original['Env'])
        for env in (['EXAMPLE=fixture-only'], [r.PREHEADER_ENV.replace('true', 'false')],
                    [r.PREHEADER_ENV, r.PREHEADER_ENV], [r.PREHEADER_ENV, r.PREHEADER_ENV.replace('true', 'false')]):
            with self.subTest(env=env), self.assertRaisesRegex(r.Failure, 'unique_enabled_preheader'):
                r.candidate_env({'Env': env})
        self.assertEqual(host.apps[OLD]['Config'], original)

    def test_payload_preserves_env_runtime_without_claiming_previous_runs(self):
        host = Fake()
        old = host.inspect(OLD)
        previous_runs = {'sub2.stream_preheader.run': 'run-20261106T174243Z',
                         'argon.keepalive.run': 'run-20261105T211000Z'}
        old['Config']['Labels'].update(previous_runs | {'fixture.unrelated': 'preserved'})
        before = copy.deepcopy(old)
        spec = r.payload(old, NEW, {'Labels': {'org.opencontainers.image.revision': r.SOURCE}}, 'run-20261107T010203Z')
        self.assertEqual(spec['Env'], before['Config']['Env'])
        self.assertEqual(spec['Env'].count(r.PREHEADER_ENV), 1)
        self.assertEqual(spec['HostConfig'], before['HostConfig'])
        self.assertEqual(spec['Labels'], {
            'task46.run': 'final-20261105-window1', 'fixture.unrelated': 'preserved',
            'com.docker.compose.image': NEW, 'org.opencontainers.image.revision': r.SOURCE,
            r.RUN_LABEL: 'run-20261107T010203Z',
        })
        self.assertEqual(spec['Image'], NEW)
        self.assertEqual(old, before)

    def test_replaced_baseline_rejects_before_mutation(self):
        for field in ('id', 'started_at', 'image'):
            host = Fake()
            app = host.apps[OLD]
            if field == 'id':
                app['Id'] = 'different-container'
            elif field == 'started_at':
                app['State']['StartedAt'] = 'another-epoch'
            else:
                app['Image'] = 'sha256:different-image'
            with self.subTest(field=field), self.assertRaisesRegex(r.Failure, 'old_(runtime_baseline|image)_changed'):
                r.prepare(host, NEW)
            self.assertEqual(host.events, [])

    def test_backup_receipt_requires_actual_image_revision_and_three_app_fields(self):
        host = Fake()
        binding = host.binding()
        good = host.backup_source_check()
        r.verify_backup_source(host, binding, r.OLD_IMAGE, r.BASE)
        for field in ('image', 'revision', 'id', 'name', 'started_at'):
            bad = copy.deepcopy(good)
            if field in ('image', 'revision'):
                bad[field] = 'wrong'
            else:
                bad['source']['app'][field] = 'wrong'
            with self.subTest(field=field), patch.object(host, 'backup_source_check', return_value=bad):
                with self.assertRaisesRegex(r.Failure, 'core_backup_source_receipt_mismatch'):
                    r.verify_backup_source(host, binding, r.OLD_IMAGE, r.BASE)
        self.assertEqual(host.events, [])

    def test_verify_candidate_and_rollback_env_both_exact(self):
        host = Fake()
        old = host.inspect(OLD)
        plan = r.prepare(host, NEW)
        candidate = copy.deepcopy(old)
        candidate['Image'] = NEW
        policy = old['HostConfig']['RestartPolicy']
        r.verify_app(host, old, candidate, plan, NEW, policy, candidate=True)
        r.verify_app(host, old, old, plan, r.OLD_IMAGE, policy)
        for env in (list(reversed(old['Config']['Env'])), old['Config']['Env'] + [r.PREHEADER_ENV],
                    [value for value in old['Config']['Env'] if value != r.PREHEADER_ENV]):
            for mode in (False, True):
                changed = copy.deepcopy(candidate if mode else old)
                changed['Config']['Env'] = env
                with self.subTest(mode=mode, env=env), self.assertRaisesRegex(r.Failure, 'app_config_changed_Env'):
                    r.verify_app(host, old, changed, plan, NEW if mode else r.OLD_IMAGE, policy, candidate=mode)

    def test_binding_changes_only_three_app_fields(self):
        host = Fake()
        before = host.binding()
        app = host.inspect(OLD)
        app['Id'] = NEW
        app['State']['StartedAt'] = 'new-started-at'
        changed = r.changed_binding(before, app)
        self.assertEqual(changed['app'], {'name': r.APP, 'id': NEW, 'started_at': 'new-started-at'})
        self.assertEqual({key: value for key, value in changed.items() if key != 'app'},
                         {key: value for key, value in before.items() if key != 'app'})
        self.assertEqual(host.binding(), before)

    def test_new_root_label_units_and_recovery_use_same_script(self):
        self.assertEqual(r.ROOT.as_posix(), '/opt/sub2api-argon502-20261107')
        self.assertEqual(r.RUN_LABEL, 'sub2.argon502.run')
        directory = r.ROOT / 'run-20261107T010203Z'
        script = str(Path(r.__file__).resolve())
        command = r.service_command(directory)
        self.assertEqual(command[1], '--unit=sub2-argon502-run-20261107T010203Z')
        self.assertIn('--property=ExecStopPost=/usr/bin/python3 -B ' + script + ' recover --run-dir ' + str(directory), command)
        self.assertEqual(command[-4:], [script, 'apply', '--run-dir', str(directory)])
        rollback = r.service_command(directory, 'rollback', 2)
        self.assertEqual(rollback[1], '--unit=sub2-argon502-run-20261107T010203Z-rollback-2')
        recovery = r.service_command(directory, 'recover')
        self.assertFalse(any('ExecStopPost=' in value for value in recovery))
        self.assertEqual(Path(script).parent, Path(__file__).resolve().parent)
        self.assertNotEqual(r.ROOT.as_posix(), '/opt/sub2api-stream-preheader-20261106')

    def test_gate_code_is_identical_to_linux_proven_carrier(self):
        here = Path(__file__).resolve().parent
        self.assertEqual((here / 'ingress_gate.py').read_bytes(), (PRIOR / 'ingress_gate.py').read_bytes())

    def test_lifecycle_and_recovery_code_reuse_linux_proven_functions(self):
        import ast
        def functions(path):
            tree = ast.parse(path.read_text(encoding='utf-8'))
            return {node.name: ast.dump(node) for node in tree.body
                    if isinstance(node, (ast.FunctionDef, ast.ClassDef))}
        previous = functions(PRIOR / 'release.py')
        current = functions(Path(r.__file__))
        self.assertEqual(set(previous), set(current))
        for name in previous.keys() - {'original_env', 'candidate_env', 'prepare', 'service_command', 'payload'}:
            with self.subTest(function=name):
                self.assertEqual(current[name], previous[name])


if __name__ == '__main__':
    unittest.main(verbosity=2)
