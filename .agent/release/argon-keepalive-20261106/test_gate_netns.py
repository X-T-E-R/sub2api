#!/usr/bin/env python3
"""Real, disposable Linux network namespaces; never run on the production namespace."""
import json
import os
import socket
import subprocess
import sys
import time
import ingress_gate as g

SOCKET_TOOL = r'''
import socket
import socketserver
import sys
import threading

if sys.argv[1] == 'server':
    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            while self.rfile.readline():
                self.wfile.write(b'ok\n')
                self.wfile.flush()
    class Server(socketserver.ThreadingTCPServer):
        allow_reuse_address = True
        daemon_threads = True
    pg = Server(('0.0.0.0', 5432), Handler)
    threading.Thread(target=pg.serve_forever, daemon=True).start()
    Server(('0.0.0.0', 8080), Handler).serve_forever()
else:
    try:
        with socket.create_connection((sys.argv[2], int(sys.argv[3])), timeout=1) as sock:
            if sys.argv[1] == 'client':
                print('connected', flush=True)
                for line in sys.stdin:
                    sock.sendall(line.encode())
                    assert sock.recv(32) == b'ok\n'
                    print('ok', flush=True)
            else:
                sock.sendall(b'health\n')
                assert sock.recv(32) == b'ok\n'
    except ConnectionRefusedError:
        sys.exit(3)
    except TimeoutError:
        sys.exit(4)
'''

BRIDGE = 'br-0123456789ab'
RUN = 'run-20261106T010203Z'
FACTS = {'bridge': BRIDGE, 'nginx_uid': 33}


def command(*args):
    result = subprocess.run(args, text=True, capture_output=True, timeout=10)
    assert result.returncode == 0, (args, result.stderr)
    return result.stdout


def request(host, port, namespace=None, uid=None):
    args = [sys.executable, '-c', SOCKET_TOOL, 'request', host, str(port)]
    if namespace:
        args = ['nsenter', '-t', str(namespace), '-n', *args]
    return subprocess.run(args, capture_output=True, text=True, timeout=4,
                          preexec_fn=(lambda: os.setuid(uid)) if uid is not None else None)


def client(host, port, namespace=None, uid=None):
    args = [sys.executable, '-c', SOCKET_TOOL, 'client', host, str(port)]
    if namespace:
        args = ['nsenter', '-t', str(namespace), '-n', *args]
    process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                               preexec_fn=(lambda: os.setuid(uid)) if uid is not None else None)
    assert process.stdout.readline().strip() == 'connected'
    return process


def send(process):
    process.stdin.write('echo\n')
    process.stdin.flush()
    assert process.stdout.readline().strip() == 'ok'


def main():
    parent_netns = os.environ['ARGON_NETNS_PARENT']
    assert parent_netns.startswith('net:[') and os.readlink('/proc/self/ns/net') != parent_netns
    assert {row['ifname'] for row in json.loads(command('ip', '-j', 'link', 'show'))} == {'lo'}, 'isolated fixture must start with no host NICs'
    processes = []
    gate = g.Gate(RUN, FACTS)
    try:
        command('ip', 'link', 'set', 'lo', 'up')
        command('ip', 'link', 'add', BRIDGE, 'type', 'bridge')
        command('ip', 'addr', 'add', '10.203.0.1/24', 'dev', BRIDGE)
        command('ip', 'link', 'set', BRIDGE, 'up')
        pids = []
        for role, address in (('client', '10.203.0.2/24'), ('server', '10.203.0.3/24')):
            process = subprocess.Popen(['unshare', '-n', 'sleep', '180'])
            processes.append(process)
            pids.append(process.pid)
            # Wait for unshare to finish before moving the veth into its namespace.
            parent_ns = os.readlink('/proc/self/ns/net')
            for _ in range(100):
                if os.readlink('/proc/' + str(process.pid) + '/ns/net') != parent_ns:
                    break
                time.sleep(.01)
            command('ip', 'link', 'add', role + '-br', 'type', 'veth', 'peer', 'name', role + '-ns')
            command('ip', 'link', 'set', role + '-ns', 'netns', str(process.pid))
            command('ip', 'link', 'set', role + '-br', 'master', BRIDGE)
            command('ip', 'link', 'set', role + '-br', 'up')
            command('nsenter', '-t', str(process.pid), '-n', 'ip', 'link', 'set', 'lo', 'up')
            command('nsenter', '-t', str(process.pid), '-n', 'ip', 'link', 'set', role + '-ns', 'up')
            command('nsenter', '-t', str(process.pid), '-n', 'ip', 'addr', 'add', address, 'dev', role + '-ns')
            command('nsenter', '-t', str(process.pid), '-n', 'ip', 'route', 'add', 'default', 'via', '10.203.0.1')
        service = subprocess.Popen(['nsenter', '-t', str(pids[1]), '-n', sys.executable, '-c', SOCKET_TOOL, 'server'])
        processes.append(service)
        processes.append(subprocess.Popen([sys.executable, '-c', SOCKET_TOOL, 'server']))
        # Exercise the real OUTPUT priority against DNAT, without sysctl changes.
        nat = '''create table ip fixture_nat
add chain ip fixture_nat output { type nat hook output priority -100; policy accept; }
add rule ip fixture_nat output ip daddr 127.0.0.1 tcp dport 17861 counter dnat to 127.0.0.2:8080
create table inet unrelated_baseline
add chain inet unrelated_baseline preserved { type filter hook output priority 10; policy accept; }
'''
        result = subprocess.run(['nft', '-f', '-'], input=nat, text=True, capture_output=True)
        assert result.returncode == 0, result.stderr
        baseline = command('nft', '-j', 'list', 'table', 'inet', 'unrelated_baseline')
        for _ in range(30):
            result = request('127.0.0.1', 17861)
            if result.returncode == 0:
                break
            time.sleep(.1)
        assert result.returncode == 0, result.stderr
        assert request('127.0.0.1', 17861, uid=33).returncode == 0
        assert request('10.203.0.3', 8080, namespace=pids[0]).returncode == 0
        public = client('127.0.0.1', 17861, uid=33)
        private = client('10.203.0.3', 8080, namespace=pids[0])
        processes.extend((public, private))
        try:
            gate.install()
        except g.GateError:
            print(json.dumps({'expected': gate.expected, 'actual': [gate.nft(['list', 'table', family, gate.name]) for family in sorted(gate.present())]}), flush=True)
            raise
        assert request('127.0.0.1', 17861, uid=33).returncode == 3
        assert request('10.203.0.3', 8080, namespace=pids[0]).returncode == 4
        assert request('127.0.0.1', 17861).returncode == 0
        assert request('10.203.0.3', 5432, namespace=pids[0]).returncode == 0
        send(public)
        send(private)
        public.stdin.close()
        private.stdin.close()
        assert public.wait(timeout=3) == private.wait(timeout=3) == 0
        counters = gate.verify()
        assert counters['nginx_out'] > 0 and counters['internal_forward'] > 0, counters
        # No address matching: a replacement IP is gated without changing rules.
        command('nsenter', '-t', str(pids[1]), '-n', 'ip', 'addr', 'add', '10.203.0.4/24', 'dev', 'server-ns')
        assert request('10.203.0.4', 8080, namespace=pids[0]).returncode == 4
        assert request('10.203.0.4', 8080).returncode == 0
        replacement = '''flush chain ip fixture_nat output
add rule ip fixture_nat output ip daddr 127.0.0.1 tcp dport 17861 counter dnat to 127.0.0.3:8080
'''
        subprocess.run(['nft', '-f', '-'], input=replacement, text=True, check=True, timeout=5)
        assert request('127.0.0.1', 17861).returncode == 0
        assert request('127.0.0.1', 17861, uid=33).returncode == 3
        gate.remove()
        gate.remove()
        assert request('127.0.0.1', 17861, uid=33).returncode == 0
        assert request('10.203.0.4', 8080, namespace=pids[0]).returncode == 0
        assert command('nft', '-j', 'list', 'table', 'inet', 'unrelated_baseline') == baseline
        assert not gate.present()
        print(json.dumps({'result': 'passed', 'nft': command('nft', '--version').strip(),
                          'blocked_new': ['uid33_loopback_before_dnat', 'bridged_internal_8080', 'replacement_ip_8080'],
                          'preserved': ['root_loopback_health', 'root_new_ip_health', 'established_public', 'established_private', 'unrelated_table'],
                          'counters': counters, 'removed': True}))
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
            process.wait(timeout=5)


if __name__ == '__main__':
    main()
