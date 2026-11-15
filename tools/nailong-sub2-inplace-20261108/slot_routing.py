"""Nailong's Sub2-only Nginx routing; all config access shares the Axon edit lock."""
from contextlib import contextmanager
from pathlib import Path
import re
import subprocess
import time

PORTS = (17861, 17862)
UPSTREAM = Path('/etc/nginx/conf.d/05-sub2api-slot-upstream.conf')
LOCK = '/run/nailong-nginx-edit.lock'
PUBLIC = Path('/etc/nginx/conf.d/task53-sukaka-nailong.conf')
PRIVATE = Path('/etc/nginx/conf.d/task51-gcp-nailong-usw1.conf')
BRIDGE = Path('/etc/nginx/conf.d/06-sub2api-bridge.conf')
ACCESS_LOG = '/opt/sub2api-nailong/inplace/sub2-slot-access.log'
LOG_FORMAT = 'log_format sub2_slot_safe \'$msec $status $upstream_status $upstream_addr $request_time\';\n'


@contextmanager
def nginx_lock():
    import fcntl  # Host-only; the controller also imports this module on Windows.
    with open(LOCK, 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def upstream_text(port):
    if port not in PORTS:
        raise ValueError('invalid_slot_port')
    return (LOG_FORMAT + 'upstream sub2api_active_slot {\n'
            f'    server 127.0.0.1:{port} max_fails=0;\n'
            '    keepalive 32;\n}\n')


def active_port_unlocked():
    text = UPSTREAM.read_text()
    match = re.search(r'server 127\.0\.0\.1:(17861|17862) max_fails=0;', text)
    if not match:
        raise RuntimeError('sub2_upstream_contract_mismatch')
    return int(match[1])


def active_port():
    with nginx_lock():
        return active_port_unlocked()


def worker_pids():
    master = Path('/run/nginx.pid').read_text().strip()
    result = subprocess.run(['ps', '--ppid', master, '-o', 'pid=,args='], capture_output=True, text=True)
    return {int(line.split()[0]) for line in result.stdout.splitlines() if 'nginx: worker process' in line}


def retiring_worker_pids():
    master = Path('/run/nginx.pid').read_text().strip()
    result = subprocess.run(['ps', '--ppid', master, '-o', 'pid=,args='], capture_output=True, text=True)
    return {int(line.split()[0]) for line in result.stdout.splitlines() if 'nginx: worker process is shutting down' in line}


def atomic_text(path, text):
    tmp = path.with_suffix(path.suffix + '.new')
    tmp.write_text(text, encoding='utf8')
    tmp.replace(path)


def reload_nginx(previous_workers):
    subprocess.run(['nginx', '-t'], check=True, capture_output=True)
    subprocess.run(['systemctl', 'reload', 'nginx'], check=True, capture_output=True)
    # systemctl reload only acknowledges SIGHUP. Wait for a new worker generation
    # before allowing the old backend to retire; existing workers keep their SSEs.
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        current = worker_pids()
        if current - previous_workers:
            return
        time.sleep(0.05)
    raise RuntimeError('nginx_new_workers_not_observed')


def switch(expected_port, target_port):
    with nginx_lock():
        if active_port_unlocked() != expected_port:
            raise RuntimeError('active_slot_changed')
        previous = UPSTREAM.read_text()
        workers = worker_pids()
        atomic_text(UPSTREAM, upstream_text(target_port))
        try:
            reload_nginx(workers)
        except Exception:
            atomic_text(UPSTREAM, previous)
            reload_nginx(worker_pids())
            raise
        return {'from_port': expected_port, 'to_port': target_port, 'retiring_workers': sorted(workers)}


def install(port, bridge_gateway):
    """One-time bootstrap. Only Sub2 proxy_pass/access_log lines are changed."""
    public, private, bridge = PUBLIC, PRIVATE, BRIDGE
    with nginx_lock():
        if UPSTREAM.exists() and active_port_unlocked() != port:
            raise RuntimeError('bootstrap_active_slot_mismatch')
        before = {p: p.read_text() if p.exists() else None for p in (UPSTREAM, public, private, bridge)}
        updates = {UPSTREAM: upstream_text(port)}
        for path, old, new in (
            (public, 'proxy_pass http://127.0.0.1:17861;', 'proxy_pass http://sub2api_active_slot;'),
            (private, 'proxy_pass http://127.0.0.1:17861/v1/messages;', 'proxy_pass http://sub2api_active_slot/v1/messages;'),
        ):
            text = before[path]
            if old in text:
                if text.count(old) != 1:
                    raise RuntimeError('sub2_proxy_pass_ambiguous')
                text = text.replace(old, new)
            elif text.count(new) != 1:
                raise RuntimeError('sub2_proxy_pass_missing')
            log = f'access_log {ACCESS_LOG} sub2_slot_safe;'
            if log not in text:
                text = text.replace(new, new + '\n        ' + log)
            updates[path] = text
        updates[bridge] = (
            'server {\n'
            f'    listen {bridge_gateway}:17860;\n'
            '    server_name _;\n'
            '    client_max_body_size 100m;\n'
            f'    access_log {ACCESS_LOG} sub2_slot_safe;\n'
            '    location / {\n'
            '        proxy_pass http://sub2api_active_slot;\n'
            '        proxy_http_version 1.1;\n'
            '        proxy_set_header Connection "";\n'
            '        proxy_set_header Host $host;\n'
            '        proxy_buffering off;\n'
            '        proxy_request_buffering off;\n'
            '        proxy_read_timeout 3600s;\n'
            '        proxy_send_timeout 3600s;\n'
            '        proxy_next_upstream off;\n'
            '    }\n}\n')
        for path in (public, private, bridge):
            # These Sub2 vhosts each contain one server block. Preserve the
            # existing 100 MiB request contract, including the bridge hop.
            if not re.search(r'^\s*client_max_body_size\s+100m;', updates[path], re.MULTILINE):
                raise RuntimeError('sub2_body_limit_missing')
        workers = worker_pids()
        try:
            for path, text in updates.items():
                atomic_text(path, text)
            reload_nginx(workers)
        except Exception:
            for path, text in before.items():
                if text is None:
                    path.unlink(missing_ok=True)
                else:
                    atomic_text(path, text)
            reload_nginx(worker_pids())
            raise
        return {'port': port, 'bridge_base_url': f'http://{bridge_gateway}:17860/v1', 'retiring_workers': sorted(workers)}
