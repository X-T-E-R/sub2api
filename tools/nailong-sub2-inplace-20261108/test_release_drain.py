"""Silent retirement preserves old ingress/SSE, and never monopolizes rollback."""
from contextlib import nullcontext
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import inplace_window as iw


class ReleaseDrainTests(unittest.TestCase):
    def setUp(self):
        self.ops = object.__new__(iw.LocalNailongOps)
        self.ops.api141 = mock.Mock()
        self.ops.active_request_count = mock.Mock(return_value=0)
        self.ops.routed_container = mock.Mock(return_value={'id': 'new'})
        self.ops._inspect_container = mock.Mock(side_effect=[{'running': True, 'started_at': 'epoch', 'port': 17861}, {'running': False, 'started_at': 'epoch', 'port': 17861}])
        self.patches = [mock.patch.object(iw, 'release_lock', side_effect=nullcontext),
                        mock.patch.object(iw.slot_routing, 'retiring_worker_pids', return_value=set()),
                        mock.patch.object(iw.time, 'sleep')]
        for patch in self.patches:
            patch.start()
            self.addCleanup(patch.stop)

    def test_waits_for_retiring_workers_before_sigterm_no_hold_or_disconnect(self):
        order = []
        self.ops.api141.side_effect = lambda *a: order.append(a[1])
        def workers():
            order.append('workers')
            return {42} if order.count('workers') == 1 else set()
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.slot_routing, 'worker_pids', side_effect=workers):
            self.ops.drain_retired_slot('old', [42], tmp, 'epoch')
        self.assertEqual(order[:2], ['workers', 'workers'])
        self.assertEqual(order[-1], '/containers/old/kill?signal=SIGTERM')
        self.assertFalse(any('SIGUSR' in item or 'disconnect' in item or 'SIGKILL' in item for item in order))

    def test_newer_compensating_generation_is_also_waited(self):
        self.ops._inspect_container.side_effect = [{'running': True, 'started_at': 'epoch', 'port': 17861}]*2 + [{'running': False, 'started_at': 'epoch', 'port': 17861}]
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.slot_routing, 'worker_pids', side_effect=[{202}, set()]) as workers, mock.patch.object(iw.slot_routing, 'retiring_worker_pids', side_effect=[{202}, set()]):
            self.ops.drain_retired_slot('old', [], tmp, 'epoch')
        self.assertEqual(workers.call_count, 2)
        self.assertTrue(self.ops.api141.call_args.args[1].endswith('SIGTERM'))

    def test_direct_pre_migration_request_finishes_before_term(self):
        self.ops.active_request_count.side_effect = [1, 0]
        self.ops._inspect_container.side_effect = [{'running': True, 'started_at': 'epoch', 'port': 17861}]*2 + [{'running': False, 'started_at': 'epoch', 'port': 17861}]
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.slot_routing, 'worker_pids', return_value=set()):
            self.ops.drain_retired_slot('old', [], tmp, 'epoch')
        self.assertEqual(self.ops.active_request_count.call_count, 2)
        self.assertEqual(self.ops.api141.call_count, 2)

    def test_reactivated_or_restarted_container_cancels_retirement(self):
        for routed_id, epoch in [('old','epoch'), ('new','new-epoch')]:
            self.ops.routed_container.return_value = {'id': routed_id}
            self.ops._inspect_container.side_effect = None
            self.ops._inspect_container.return_value = {'running': True, 'started_at': epoch}
            with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.slot_routing, 'worker_pids', return_value=set()):
                self.ops.drain_retired_slot('old', [], tmp, 'epoch')
                receipt = json.loads((Path(tmp) / 'retirement-status-old.json').read_text())
            self.assertEqual(receipt['stage'], 'retirement_cancelled_reactivated')
        self.ops.api141.assert_not_called()

    def test_retirement_timeout_keeps_no_sigkill_contract(self):
        self.ops._inspect_container.return_value = {'running': True, 'started_at': 'epoch', 'port': 17861}
        self.ops._inspect_container.side_effect = None
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.slot_routing, 'worker_pids', return_value=set()), mock.patch.object(iw.time, 'monotonic', side_effect=[0, 361]):
            with self.assertRaisesRegex(iw.InplaceError, 'retired_slot_stop_timeout_no_kill'):
                self.ops.drain_retired_slot('old', [], tmp, 'epoch')
        self.assertFalse(any('SIGKILL' in call.args[1] for call in self.ops.api141.call_args_list))

    def test_schedule_captures_unrecorded_workers_and_runs_independent_unit(self):
        self.ops._inspect_container.side_effect = None
        self.ops._inspect_container.return_value = {'started_at': 'epoch', 'port': 17861}
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(iw.slot_routing, 'retiring_worker_pids', return_value={202}), mock.patch.object(iw.subprocess, 'run') as run:
            result = self.ops.schedule_retirement('a'*64, [101], tmp)
        self.assertEqual(result['retiring_workers'], [101,202])
        self.assertIn('systemd-run', run.call_args.args[0])
        self.assertIn('retire', run.call_args.args[0])


if __name__ == '__main__':
    unittest.main()
