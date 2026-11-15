import unittest
from unittest import mock
import inplace_window as iw


class ReleaseDrainTests(unittest.TestCase):
    def setUp(self):
        self.ops = object.__new__(iw.LocalNailongOps)
        self.ops.api141 = mock.Mock()
        self.app = {'Id': 'a' * 64, 'State': {'Pid': 123, 'Running': True}}

    @mock.patch.object(iw.time, 'sleep')
    def test_application_hold_waits_for_actual_zero(self, sleep):
        self.ops.health_telemetry = mock.Mock(side_effect=[
            {'status': 'ok', 'draining': False, 'in_flight': 5},
            {'draining': True, 'in_flight': 3},
            {'draining': True, 'in_flight': 0},
        ])
        self.ops.concurrency_snapshot = mock.Mock()
        result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'application_admission_hold')
        self.assertEqual(result['in_flight'], 0)
        self.assertEqual(self.ops.api141.call_args.args[1], '/containers/' + self.app['Id'] + '/kill?signal=SIGUSR1')
        self.assertEqual(self.ops.api141.call_count, 1)
        self.ops.concurrency_snapshot.assert_not_called()

    @mock.patch.object(iw.time, 'sleep')
    def test_legacy_uses_active_slots_not_keepalive_connections(self, sleep):
        self.ops.health_telemetry = mock.Mock(return_value={'status': 'ok'})
        self.ops.concurrency_snapshot = mock.Mock(side_effect=[
            {'account_slots': 2, 'user_slots': 2, 'user_waiting': 1},
            {'account_slots': 0, 'user_slots': 0, 'user_waiting': 0},
        ])
        result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'legacy_zero_concurrency_slots')
        self.assertEqual(len(result['samples']), 2)
        self.ops.api141.assert_not_called()
        self.assertFalse(hasattr(iw.LocalNailongOps, 'connection_counts'))

    def test_legacy_busy_timeout_requires_explicit_override(self):
        self.ops.health_telemetry = mock.Mock(return_value={'status': 'ok'})
        self.ops.concurrency_snapshot = mock.Mock(return_value={'account_slots': 2})
        with mock.patch.object(iw.time, 'monotonic', side_effect=[0, 61]):
            with self.assertRaisesRegex(iw.InplaceError, 'legacy_active_slots_timeout_no_stop'):
                self.ops.acquire_release_idle(self.app)
        self.ops.api141.assert_not_called()

    def test_parent_authorized_bounded_legacy_forward_records_busy_state(self):
        self.ops.allow_legacy_busy_after_wait = True
        self.ops.health_telemetry = mock.Mock(return_value={'status': 'ok'})
        self.ops.concurrency_snapshot = mock.Mock(return_value={'account_slots': 2})
        with mock.patch.object(iw.time, 'monotonic', side_effect=[0, 61]):
            result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'legacy_explicit_bounded_forward')
        self.assertEqual(result['active_slots_at_stop']['account_slots'], 2)
        self.ops.api141.assert_not_called()

    def test_drain_probe_failure_resumes_admission(self):
        self.ops.health_telemetry = mock.Mock(side_effect=[{'draining': False, 'in_flight': 1}, OSError('health unavailable')])
        with self.assertRaises(OSError):
            self.ops.acquire_release_idle(self.app)
        self.assertTrue(self.ops.api141.call_args.args[1].endswith('signal=SIGUSR2'))
        self.assertFalse(any('SIGTERM' in call.args[1] for call in self.ops.api141.call_args_list))

    def test_slot_projection_keeps_auth_in_child_environment(self):
        self.ops.inspect_via_api141 = mock.Mock(return_value={'Config': {'Env': ['REDIS_PASSWORD=fixture-only']}})
        with mock.patch.object(iw.subprocess, 'run', return_value=mock.Mock(returncode=0, stdout='[0,0,0,0,0,0]')) as run:
            sample = self.ops.concurrency_snapshot()
        self.assertEqual(sample['account_slots'], 0)
        self.assertEqual(run.call_args.kwargs['env']['REDISCLI_AUTH'], 'fixture-only')
        self.assertNotIn('fixture-only', run.call_args.args[0])

    def test_unhealthy_candidate_uses_slot_projection(self):
        self.ops.health_telemetry = mock.Mock(side_effect=OSError('health unavailable'))
        self.ops.concurrency_snapshot = mock.Mock(return_value={'account_slots': 0})
        result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'legacy_zero_concurrency_slots')
        self.ops.api141.assert_not_called()


if __name__ == '__main__':
    unittest.main()
