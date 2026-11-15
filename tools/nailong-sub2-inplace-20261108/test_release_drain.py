import unittest
from unittest import mock
import inplace_window as iw


class ReleaseDrainTests(unittest.TestCase):
    def setUp(self):
        self.ops = object.__new__(iw.LocalNailongOps)
        self.ops.api141 = mock.Mock()
        self.ops.syn_hold = mock.Mock()
        self.app = {'Id': 'a' * 64, 'State': {'Pid': 123, 'Running': True}}

    @mock.patch.object(iw.time, 'sleep')
    def test_application_hold_waits_for_actual_zero(self, sleep):
        self.ops.health_telemetry = mock.Mock(side_effect=[
            {'status': 'ok', 'draining': False, 'in_flight': 5},
            {'draining': True, 'in_flight': 3},
            {'draining': True, 'in_flight': 0},
        ])
        result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'application_admission_hold')
        self.assertEqual(result['in_flight'], 0)
        self.assertEqual(self.ops.api141.call_args.args[1], '/containers/' + self.app['Id'] + '/kill?signal=SIGUSR1')
        self.assertEqual(self.ops.api141.call_count, 1)
        self.ops.syn_hold.assert_not_called()

    @mock.patch.object(iw.time, 'sleep')
    def test_legacy_checks_both_ingress_paths_and_rechecks_under_hold(self, sleep):
        self.ops.health_telemetry = mock.Mock(return_value={'status': 'ok'})
        self.ops.connection_counts = mock.Mock(side_effect=[
            {'host_17861': 0, 'container_8080': 2},
            {'host_17861': 0, 'container_8080': 0},
            {'host_17861': 1, 'container_8080': 1},
            {'host_17861': 0, 'container_8080': 0},
            {'host_17861': 0, 'container_8080': 0},
        ])
        result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'legacy_verified_idle_syn_hold')
        self.assertEqual(self.ops.syn_hold.call_args_list, [mock.call(self.app, True), mock.call(self.app, False), mock.call(self.app, True)])
        self.ops.api141.assert_not_called()

    def test_legacy_busy_timeout_never_signals(self):
        self.ops.health_telemetry = mock.Mock(return_value={'status': 'ok'})
        self.ops.connection_counts = mock.Mock()
        with mock.patch.object(iw.time, 'monotonic', side_effect=[0, 181]):
            with self.assertRaisesRegex(iw.InplaceError, 'legacy_idle_timeout_no_stop'):
                self.ops.acquire_release_idle(self.app)
        self.ops.syn_hold.assert_not_called()
        self.ops.api141.assert_not_called()

    def test_drain_probe_failure_resumes_admission(self):
        self.ops.health_telemetry = mock.Mock(side_effect=[{'draining': False, 'in_flight': 1}, OSError('health unavailable')])
        with self.assertRaises(OSError):
            self.ops.acquire_release_idle(self.app)
        self.assertTrue(self.ops.api141.call_args.args[1].endswith('signal=SIGUSR2'))
        self.assertFalse(any('SIGTERM' in call.args[1] for call in self.ops.api141.call_args_list))

    @mock.patch.object(iw.time, 'sleep')
    def test_unhealthy_candidate_still_requires_socket_quiet_before_recovery(self, sleep):
        self.ops.health_telemetry = mock.Mock(side_effect=OSError('health unavailable'))
        self.ops.connection_counts = mock.Mock(return_value={'host_17861': 0, 'container_8080': 0})
        result = self.ops.acquire_release_idle(self.app)
        self.assertEqual(result['mode'], 'legacy_verified_idle_syn_hold')
        self.assertEqual(self.ops.connection_counts.call_count, 2)
        self.ops.syn_hold.assert_called_once_with(self.app, True)
        self.ops.api141.assert_not_called()

    def test_socket_count_includes_docker_direct_and_half_closed_streams(self):
        table = 'header\n0: 00000000:1F90 00000000:0001 01\n1: 00000000:1F90 00000000:0002 08\n2: 00000000:1F90 00000000:0000 0A\n3: 00000000:1F90 00000000:0002 06\n'
        with mock.patch.object(iw.Path, 'read_text', return_value=table):
            self.assertEqual(self.ops.socket_count('/proc/123/net', 8080), 4)


if __name__ == '__main__':
    unittest.main()
