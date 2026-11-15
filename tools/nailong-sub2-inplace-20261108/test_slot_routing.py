from contextlib import nullcontext
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import slot_routing as routing


class RoutingTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        for key in ('UPSTREAM', 'PUBLIC', 'PRIVATE', 'BRIDGE'):
            patch = mock.patch.object(routing, key, root / (key + '.conf'))
            patch.start()
            self.addCleanup(patch.stop)
        self.lock = mock.patch.object(routing, 'nginx_lock', side_effect=nullcontext).start()
        self.addCleanup(mock.patch.stopall)

    def test_bootstrap_changes_only_sub2_proxy_pass_and_access_logs(self):
        public = 'server {\n client_max_body_size 100m;\n location / {\n    proxy_pass http://127.0.0.1:17861;\n}\n}\n'
        axon = 'location / {\n    proxy_pass http://axon_active_slot; # sibling owns this\n}\n'
        private = 'server {\n client_max_body_size 100m;\n location = /__task49_personal_sub2/v1/messages {\n    proxy_pass http://127.0.0.1:17861/v1/messages;\n}\n' + axon + '}\n'
        routing.PUBLIC.write_text(public)
        routing.PRIVATE.write_text(private)
        with mock.patch.object(routing, 'worker_pids', return_value={1}), mock.patch.object(routing, 'reload_nginx'):
            result = routing.install(17861, '172.20.0.1')
        self.assertIn(axon, routing.PRIVATE.read_text())
        self.assertIn('proxy_pass http://sub2api_active_slot/v1/messages;', routing.PRIVATE.read_text())
        self.assertIn('listen 172.20.0.1:17860;', routing.BRIDGE.read_text())
        self.assertEqual(result['bridge_base_url'], 'http://172.20.0.1:17860/v1')
        self.assertEqual(routing.active_port(), 17861)
        for path in (routing.PUBLIC, routing.PRIVATE, routing.BRIDGE):
            self.assertEqual(path.read_text().count('server {'), 1)
            self.assertIn('client_max_body_size 100m;', path.read_text(), str(path))
        self.assertNotIn('$request', routing.LOG_FORMAT.split('$request_time')[0])

    def test_install_refuses_any_sub2_server_missing_100m_body_limit(self):
        routing.PUBLIC.write_text('server { location / { proxy_pass http://127.0.0.1:17861; } }')
        routing.PRIVATE.write_text('server { client_max_body_size 100m; location = /__task49_personal_sub2/v1/messages { proxy_pass http://127.0.0.1:17861/v1/messages; } }')
        with self.assertRaisesRegex(RuntimeError, 'sub2_body_limit_missing'), mock.patch.object(routing, 'reload_nginx') as reload:
            routing.install(17861, '172.20.0.1')
        reload.assert_not_called()
        self.assertFalse(routing.UPSTREAM.exists())

    def test_switch_under_lock_rejects_stale_slot(self):
        routing.UPSTREAM.write_text(routing.upstream_text(17861))
        with self.assertRaisesRegex(RuntimeError, 'active_slot_changed'):
            routing.switch(17862, 17861)
        self.assertEqual(routing.active_port(), 17861)
        self.assertTrue(self.lock.called)

    def test_reload_failure_restores_previous_upstream(self):
        routing.UPSTREAM.write_text(routing.upstream_text(17861))
        with mock.patch.object(routing, 'worker_pids', return_value={1}), mock.patch.object(routing, 'reload_nginx', side_effect=[RuntimeError('test_failed'), None]) as reload:
            with self.assertRaisesRegex(RuntimeError, 'test_failed'):
                routing.switch(17861, 17862)
        self.assertEqual(routing.active_port(), 17861)
        self.assertEqual(reload.call_count, 2)

    def test_success_returns_retiring_generation_and_new_slot(self):
        routing.UPSTREAM.write_text(routing.upstream_text(17861))
        with mock.patch.object(routing, 'worker_pids', return_value={17, 18}), mock.patch.object(routing, 'reload_nginx'):
            result = routing.switch(17861, 17862)
        self.assertEqual(routing.active_port(), 17862)
        self.assertEqual(result['retiring_workers'], [17, 18])

    def test_reload_waits_for_new_workers_after_systemctl_ack(self):
        with mock.patch.object(routing.subprocess, 'run') as run, mock.patch.object(routing, 'worker_pids', side_effect=[{1}, {1, 2}]), mock.patch.object(routing.time, 'sleep') as sleep:
            routing.reload_nginx({1})
        self.assertEqual([call.args[0] for call in run.call_args_list], [['nginx', '-t'], ['systemctl', 'reload', 'nginx']])
        sleep.assert_called_once()


if __name__ == '__main__':
    unittest.main()
