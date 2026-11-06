#!/usr/bin/env python3
"""Sub2 Argon502 switch; frozen f183 lifecycle, no pull or provider request."""
import argparse
import copy
import datetime as dt
import http.client
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import time
import urllib.parse
import urllib.request
from ingress_gate import Gate, GateError

UNBOUND_SOURCE = '<UNBOUND_SOURCE_REVISION>'
UNBOUND_IMAGE = '<UNBOUND_IMAGE_RUNTIME_ID>'
SOURCE = '7c12de7380be0142e18bcdc6397b6537f2af9968'
BASE = 'f18388a440ddce04c5da0d0917ba2e4db04a5b0e'
OLD_IMAGE = 'sha256:06ee81a40b6a67759d9d286a302b1ac25adf236f8bcbe623b2e33d34c20384f4'
OLD_CONTAINER = 'b7df1c4ed5b02e8b6a8d3f1999d7d2eb368f76b3bb965f14aa6eb89d8d2de7f7'
OLD_STARTED_AT = '2026-11-06T17:43:56.662935341Z'
IMAGE = '<UNBOUND_IMAGE_RUNTIME_ID>'
PREHEADER_ENV = 'GATEWAY_STREAM_PREHEADER_KEEPALIVE_ENABLED=true'
RUN_LABEL = 'sub2.argon502.run'
APP = 'sub2api-gcp'
APP_NETWORK = 'sub2api-migration-app'
DEPENDENCY_NETWORK = 'sub2api-migration'
NETWORKS = {APP_NETWORK, DEPENDENCY_NETWORK}
APP_NETWORK_ID = '522fc2807c925d62b0e18f8171fc94548a7b531a27b8aa507e3d523570cdcde1'
AXON = 'axonhub-gcp'
HOST_PORT = '17861'
DRAIN_SECONDS = 135
BINDING = Path('/etc/sub2-core-backup/source.json')
TIMER = 'sub2-core-backup.timer'
BACKUP = 'sub2-core-backup.service'
ROOT = Path('/opt/sub2api-argon502-20261107')


class Failure(Exception):
    pass


def require(value, reason):
    if not value:
        raise Failure(reason)


def is_unbound(value):
    return not value or value in (UNBOUND_SOURCE, UNBOUND_IMAGE) or (isinstance(value, str) and value.startswith('<UNBOUND_'))


def require_bound_release(image):
    require(not is_unbound(SOURCE) and not is_unbound(IMAGE) and not is_unbound(image), 'release_identity_unbound')
    require(image == IMAGE, 'staged_image_identity_changed')


def env_name(entry):
    return entry.split('=', 1)[0] if isinstance(entry, str) else ''


def original_env(config):
    env = config.get('Env')
    require(isinstance(env, list) and all(isinstance(entry, str) for entry in env), 'app_env_invalid')
    require([entry for entry in env if env_name(entry) == PREHEADER_ENV.split('=', 1)[0]] == [PREHEADER_ENV],
            'old_env_requires_unique_enabled_preheader_setting')
    return env


def candidate_env(config):
    return list(original_env(config))


def stamp():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def read(path):
    return json.loads(Path(path).read_text(encoding='utf-8'))


def save(path, value):
    path = Path(path)
    temp = path.with_suffix(path.suffix + '.new')
    with temp.open('w', encoding='utf-8') as file:
        os.chmod(temp, 0o600)
        json.dump(value, file, sort_keys=True, indent=2)
        file.write('\n')
        file.flush()
        os.fsync(file.fileno())
    os.replace(temp, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def phase(directory, state, name):
    state.update(phase=name, at=stamp())
    save(directory/'state.json', state)


class Connection(http.client.HTTPConnection):
    def __init__(self):
        super().__init__('localhost', timeout=30)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(30)
        self.sock.connect('/var/run/docker.sock')


class Host:
    def api(self, method, path, body=None):
        connection = Connection()
        try:
            data = json.dumps(body).encode() if body is not None else None
            connection.request(method, '/v1.45' + path, data, {'Content-Type': 'application/json'})
            response = connection.getresponse()
            raw = response.read()
            require(response.status in (200, 201, 204, 304), 'docker_api_failed')
            return json.loads(raw) if raw else None
        finally:
            connection.close()

    def inspect(self, container):
        return self.api('GET', '/containers/' + container + '/json')

    def image(self, image):
        return self.api('GET', '/images/' + image + '/json')

    def network(self, name):
        return self.api('GET', '/networks/' + name)

    def candidates(self, run):
        query = urllib.parse.urlencode({'all': '1', 'filters': json.dumps({'label': [RUN_LABEL + '=' + run]})})
        return self.api('GET', '/containers/json?' + query)

    def restart_policy(self, container, policy):
        self.api('POST', '/containers/' + container + '/update', {'RestartPolicy': policy})

    def terminate(self, container):
        if self.inspect(container)['State']['Running']:
            self.api('POST', '/containers/' + container + '/kill?signal=SIGTERM')

    def wait_stopped(self, container):
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            if not self.inspect(container)['State']['Running']:
                return
            time.sleep(1)
        raise Failure('graceful_stop_timeout_no_sigkill_sent')

    def rename(self, container, name):
        self.api('POST', '/containers/' + container + '/rename?' + urllib.parse.urlencode({'name': name}))

    def disconnect(self, container, network):
        if network in self.inspect(container)['NetworkSettings']['Networks']:
            self.api('POST', '/networks/' + network + '/disconnect', {'Container': container, 'Force': False})

    def connect(self, container, network, aliases):
        if network not in self.inspect(container)['NetworkSettings']['Networks']:
            self.api('POST', '/networks/' + network + '/connect', {'Container': container, 'EndpointConfig': {'Aliases': aliases}})

    def create(self, payload):
        return self.api('POST', '/containers/create?name=' + APP, payload)['Id']

    def start(self, container):
        self.api('POST', '/containers/' + container + '/start')

    def healthy(self, container):
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            app = self.inspect(container)
            require(app['State']['Running'] and app['RestartCount'] == 0, 'app_exited_or_restarted')
            if app['State'].get('Health', {}).get('Status') == 'healthy':
                return app
            time.sleep(2)
        raise Failure('app_health_timeout')

    def health_port(self):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open('http://127.0.0.1:' + HOST_PORT + '/health', timeout=5) as response:
            require(response.status == 200 and json.loads(response.read(4096)).get('status') == 'ok', 'host_health_failed')

    def unit(self, name):
        result = subprocess.run(['systemctl', 'show', '-p', 'ActiveState,SubState,UnitFileState', name], capture_output=True, text=True, timeout=10)
        require(result.returncode == 0, 'unit_read_failed')
        return dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)

    def timer(self, active):
        result = subprocess.run(['systemctl', 'start' if active else 'stop', TIMER], capture_output=True, timeout=15)
        require(result.returncode == 0, 'backup_timer_change_failed')

    def quiet(self, container):
        # Measure all inbound HTTP connections in Sub2 itself, not in Axon or
        # the public proxy. Count accepted, pending and half-closed connections.
        command = ['docker', 'exec', container, 'sh', '-c',
                   "awk '$2 ~ /:1F90$/ {if ($4==\"0A\") l++; if ($4 ~ /^(01|03|08|0C)$/) n++} END {print l+0,n+0}' /proc/net/tcp /proc/net/tcp6"]
        deadline = time.monotonic() + DRAIN_SECONDS
        quiet = 0
        while time.monotonic() < deadline:
            result = subprocess.run(command, capture_output=True, text=True, timeout=5)
            counts = result.stdout.split()
            require(result.returncode == 0 and len(counts) == 2 and all(value.isdecimal() for value in counts), 'unknown_sub2_http_sockets')
            listeners, active = map(int, counts)
            require(listeners > 0, 'sub2_http_listener_absent')
            quiet = quiet + 1 if active == 0 else 0
            if quiet == 3:
                return
            time.sleep(2)
        raise Failure('sub2_http_drain_timeout')

    def binding(self):
        require(BINDING.is_file() and not BINDING.is_symlink() and BINDING.stat().st_mode & 0o077 == 0, 'binding_not_private_regular_file')
        return read(BINDING)

    def binding_cas(self, allowed, replacement):
        require(self.binding() in allowed, 'backup_source_binding_changed')
        save(BINDING, replacement)
        require(self.binding() == replacement, 'backup_binding_readback_failed')

    def backup_source_check(self):
        result = subprocess.run(['/usr/bin/python3', '-B', '/opt/sub2-core-backup/core-backup/core_backup.py',
                                 'source-check', '--binding', str(BINDING)], capture_output=True, text=True, timeout=20)
        require(result.returncode == 0, 'core_backup_source_check_failed')
        return json.loads(result.stdout)

    def ingress_facts(self):
        network = self.network(APP_NETWORK)
        require(network['Id'] == APP_NETWORK_ID and network['Driver'] == 'bridge' and not network['Internal'], 'application_bridge_changed')
        require({row['Name'] for row in network['Containers'].values()} == {APP, AXON}, 'application_bridge_consumers_changed')
        bridge = network.get('Options', {}).get('com.docker.network.bridge.name') or 'br-' + network['Id'][:12]
        link = subprocess.run(['ip', '-j', 'link', 'show', 'dev', bridge], capture_output=True, text=True, timeout=5)
        require(link.returncode == 0 and 'UP' in json.loads(link.stdout)[0]['flags'] and Path('/sys/class/net', bridge, 'bridge').is_dir(), 'application_bridge_not_up')
        processes = subprocess.run(['ps', '-eo', 'pid=,ppid=,euid=,args='], capture_output=True, text=True, timeout=5)
        require(processes.returncode == 0, 'nginx_process_read_failed')
        host_namespace = os.readlink('/proc/self/ns/net')
        rows = []
        for line in processes.stdout.splitlines():
            row = line.split(None, 3)
            if len(row) == 4 and row[3].startswith(('nginx: master process ', 'nginx: worker process')):
                if os.readlink('/proc/' + row[0] + '/ns/net') == host_namespace:
                    rows.append(row)
        masters = [row for row in rows if row[3].startswith('nginx: master process ')]
        workers = [row for row in rows if row[3].startswith('nginx: worker process')]
        require(len(masters) == 1 and masters[0][2] == '0' and workers, 'nginx_master_or_workers_missing')
        for pid, ppid, uid, _ in workers:
            fields = next(line.split()[1:] for line in Path('/proc', pid, 'status').read_text().splitlines() if line.startswith('Uid:'))
            require(uid == fields[1] == fields[3] == '33' and ppid == masters[0][0], 'nginx_worker_identity_changed')
        return {'bridge': bridge, 'network_id': network['Id'], 'nginx_uid': 33, 'nginx_master': int(masters[0][0]), 'axon': identity(self.inspect(AXON))}

    def gate(self, directory, plan):
        return Gate(directory.name, plan['ingress'])

    def prove_gate(self, directory, plan):
        require(self.ingress_facts() == plan['ingress'], 'ingress_identity_changed')
        gate = self.gate(directory, plan)
        before = gate.verify()
        self.health_port()  # root bypass, never a provider request
        public = subprocess.run(['/usr/bin/python3', '-c',
            'import errno,os,socket;os.setgroups([]);os.setgid(33);os.setuid(33);s=socket.socket();s.settimeout(2);raise SystemExit(0 if s.connect_ex(("127.0.0.1",17861))==errno.ECONNREFUSED else 1)'],
            capture_output=True, timeout=4)
        require(public.returncode == 0, 'public_ingress_gate_not_proven')
        private = subprocess.run(['docker', 'exec', AXON, 'wget', '-q', '-O', '-', '-T', '2', 'http://sub2api-gcp:8080/health'], capture_output=True, timeout=4)
        after = gate.verify()
        require(private.returncode != 0 and after['nginx_out'] > before['nginx_out'] and after['internal_forward'] > before['internal_forward'], 'private_ingress_gate_not_proven')
        return after


def verify_backup_source(host, binding, image, revision):
    receipt = host.backup_source_check()
    app = receipt.get('source', {}).get('app', {})
    require(receipt.get('stage') == 'core_backup_source_bound' and receipt.get('image') == image
            and receipt.get('revision') == revision and all(app.get(key) == value for key, value in binding['app'].items()),
            'core_backup_source_receipt_mismatch')


def identity(app):
    return {key: app[key] for key in ('Id', 'Image', 'RestartCount')} | {'StartedAt': app['State']['StartedAt']}


def endpoints(app):
    return {name: {'id': row['NetworkID'], 'aliases': row['Aliases']} for name, row in app['NetworkSettings']['Networks'].items()}


def require_contract(app, binding):
    config, runtime = app['Config'], app['HostConfig']
    require(app['Name'] == '/' + APP and set(app['NetworkSettings']['Networks']) == NETWORKS, 'app_name_or_networks_changed')
    require(runtime['NetworkMode'] == APP_NETWORK, 'primary_network_changed')
    require(runtime['PortBindings'] == {'8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': HOST_PORT}]}, 'host_port_changed')
    require(runtime['Binds'] == [binding['appdata'] + ':/app/data:rw'], 'appdata_changed')
    require(runtime['RestartPolicy'] == {'Name': 'unless-stopped', 'MaximumRetryCount': 0}, 'restart_policy_changed')
    require(binding['dependency_network'] == DEPENDENCY_NETWORK, 'dependency_network_changed')
    require(binding['app'] == {'id': app['Id'], 'name': APP, 'started_at': app['State']['StartedAt']}, 'backup_app_binding_changed')
    for row in app['NetworkSettings']['Networks'].values():
        require(row['Aliases'] == [APP] and not row.get('IPAMConfig') and not row.get('DriverOpts') and not row.get('Links'), 'network_endpoint_requires_mapping')
    require(config['Entrypoint'] == ['/app/docker-entrypoint.sh'] and config['Cmd'] == ['/app/sub2api'], 'app_command_changed')
    require(config.get('Labels', {}).get('task46.run') == 'final-20261105-window1', 'migration_owner_changed')


def dependencies_of(host, binding, app_id):
    result = {}
    for role in ('pg', 'redis'):
        expected = binding[role]
        dep = host.inspect(expected['name'])
        require(dep['Id'] == expected['id'] and dep['State']['Running'] and dep['State'].get('Health', {}).get('Status') == 'healthy', 'dependency_not_healthy_or_changed_' + role)
        result[role] = identity(dep)
    network = host.network(DEPENDENCY_NETWORK)
    require(network['Internal'] and set(network['Containers']) == {app_id, *(v['Id'] for v in result.values())}, 'backup_dependency_network_membership_changed')
    return result


def prepare(host, image):
    require_bound_release(image)
    candidate = host.image(image)
    require(candidate['Id'] == image and candidate['Os'] == 'linux' and candidate['Architecture'] == 'amd64', 'image_platform_changed')
    require(candidate['Config'].get('Labels', {}).get('org.opencontainers.image.revision') == SOURCE, 'image_source_changed')
    app, binding = host.inspect(APP), host.binding()
    original_env(app['Config'])
    require(app['Image'] == OLD_IMAGE, 'old_image_changed')
    require(app['Id'] == OLD_CONTAINER and app['State']['StartedAt'] == OLD_STARTED_AT, 'old_runtime_baseline_changed')
    require_contract(app, binding)
    require(identity(host.healthy(app['Id'])) == identity(app), 'old_app_identity_changed')
    host.health_port()
    dependencies = dependencies_of(host, binding, app['Id'])
    timer = host.unit(TIMER)
    require(timer.get('ActiveState') == 'active' and timer.get('SubState') == 'waiting' and timer.get('UnitFileState') == 'enabled', 'backup_timer_not_enabled_waiting')
    require(host.unit(BACKUP).get('ActiveState') == 'inactive', 'backup_is_running')
    verify_backup_source(host, binding, OLD_IMAGE, BASE)
    return {'format': 1, 'source': SOURCE, 'base': BASE, 'image': image, 'old': identity(app),
            'binding': binding, 'networks': endpoints(app), 'dependencies': dependencies, 'timer': timer,
            'ingress': host.ingress_facts(), 'prepared_at': stamp()}


def check(host, plan):
    require(plan['source'] == SOURCE and plan['base'] == BASE, 'plan_source_changed')
    observed = prepare(host, plan['image'])
    for key in ('old', 'binding', 'networks', 'dependencies', 'timer', 'ingress'):
        require(observed[key] == plan[key], 'preflight_drift_' + key)


def payload(old, image, image_config, run):
    result = copy.deepcopy(old['Config'])
    result['Image'] = image
    result['Env'] = candidate_env(old['Config'])
    if result['Hostname'] == old['Id'][:12]:
        result['Hostname'] = ''
    labels = result.setdefault('Labels', {})
    labels.update({key: value for key, value in image_config.get('Labels', {}).items() if key.startswith('org.opencontainers.image.')})
    labels.update({'org.opencontainers.image.revision': SOURCE, RUN_LABEL: run})
    if 'com.docker.compose.image' in labels:
        labels['com.docker.compose.image'] = image
    result['HostConfig'] = copy.deepcopy(old['HostConfig'])
    result['NetworkingConfig'] = {'EndpointsConfig': {name: {'Aliases': row['Aliases']} for name, row in old['NetworkSettings']['Networks'].items()}}
    return result


def changed_binding(original, app):
    result = copy.deepcopy(original)
    result['app'].update(name=APP, id=app['Id'], started_at=app['State']['StartedAt'])
    return result


def timer_restore(host, plan):
    host.timer(True)
    current = host.unit(TIMER)
    require(current.get('ActiveState') == 'active' and current.get('UnitFileState') == plan['timer']['UnitFileState'], 'backup_timer_not_restored')


NO_RESTART = {'Name': 'no', 'MaximumRetryCount': 0}


def verify_app(host, old, app, plan, image, policy, candidate=False):
    require(app['Image'] == image and app['Name'] == '/' + APP and endpoints(app) == plan['networks'], 'app_identity_or_networks_changed')
    expected_env = candidate_env(old['Config']) if candidate else old['Config'].get('Env')
    for key in ('Env', 'Entrypoint', 'Cmd', 'WorkingDir', 'User', 'Healthcheck'):
        expected = expected_env if key == 'Env' else old['Config'].get(key)
        require(app['Config'].get(key) == expected, 'app_config_changed_' + key)
    runtime = old['HostConfig'] | {'RestartPolicy': policy}
    require(app['HostConfig'] == runtime, 'app_host_config_changed')
    require(dependencies_of(host, plan['binding'], app['Id']) == plan['dependencies'], 'dependencies_changed')
    host.health_port()


def pause_backup(host):
    host.timer(False)
    require(host.unit(TIMER).get('ActiveState') == 'inactive', 'backup_timer_not_stopped')
    require(host.unit(BACKUP).get('ActiveState') == 'inactive', 'backup_running_do_not_stop_it')


FINAL = {'aborted', 'rolled_back', 'verified', 'rollback_aborted'}


def finish(host, directory, plan, state, final):
    # A durable opening intent prevents a lost DELETE/timer ACK from causing
    # another app switch. Recovery checks the settled owner, then only opens.
    if final in ('verified', 'rolled_back'):
        old = read(directory/'private-old.json')
        if final == 'verified':
            binding, image, revision = state['candidate_binding'], plan['image'], SOURCE
        else:
            binding, image, revision = state['rollback_binding'], OLD_IMAGE, BASE
        require(host.binding() == binding, 'settled_binding_changed')
        active = host.healthy(binding['app']['id'])
        require(changed_binding(binding, active) == binding, 'settled_app_start_changed')
        verify_app(host, old, active, plan, image, old['HostConfig']['RestartPolicy'], candidate=final == 'verified')
        verify_backup_source(host, binding, image, revision)
    state['completion'] = final
    phase(directory, state, 'open_gate_intent')
    if state.get('timer_owned'):
        timer_restore(host, plan)
    host.gate(directory, plan).remove()
    phase(directory, state, final)
    return state


def close_gate(host, directory, plan, state, intent):
    gate = host.gate(directory, plan)
    require(not gate.present(), 'gate_already_exists_before_attempt')
    phase(directory, state, intent)
    require(host.ingress_facts() == plan['ingress'], 'ingress_identity_changed')
    gate.install()
    state['gate_counters'] = host.prove_gate(directory, plan)


def drain(host, directory, plan, container):
    host.quiet(container)
    require(host.ingress_facts() == plan['ingress'], 'ingress_identity_changed_during_drain')
    host.gate(directory, plan).verify()


def recover(host, directory):
    if not (directory/'state.json').exists():
        return {'phase': 'not_started'}
    plan, state = read(directory/'plan.json'), read(directory/'state.json')
    current_phase = state['phase']
    gate = host.gate(directory, plan)
    if current_phase in FINAL:
        require(not gate.present(), 'terminal_run_has_gate')
        return state
    if current_phase == 'open_gate_intent':
        return finish(host, directory, plan, state, state['completion'])
    if current_phase in ('candidate_committed', 'old_committed'):
        return finish(host, directory, plan, state, 'verified' if current_phase == 'candidate_committed' else 'rolled_back')
    if current_phase in ('gate_install_intent', 'timer_stop_intent', 'rollback_gate_install_intent', 'rollback_timer_stop_intent'):
        # Busy/failed admission never stops either app; release both gates.
        final = 'rollback_aborted' if current_phase.startswith('rollback_') else 'aborted'
        return finish(host, directory, plan, state, final)
    if not gate.present():
        gate.install()
    gate.verify()  # Failed recovery leaves both gates in place.
    old = read(directory/'private-old.json')
    allowed = [plan['binding']] + [state[key] for key in ('candidate_binding', 'rollback_binding') if key in state]
    require(host.binding() in allowed, 'backup_source_binding_changed')
    pause_backup(host)
    candidates = host.candidates(directory.name)
    require(len(candidates) <= 1, 'multiple_owned_candidates')
    candidate_id = candidates[0]['Id'] if candidates else None
    require(not state.get('candidate_id') or state['candidate_id'] == candidate_id, 'candidate_missing_or_replaced')
    if candidate_id:
        candidate = host.inspect(candidate_id)
        require(candidate['Image'] == plan['image'] and candidate['Config']['Labels'].get(RUN_LABEL) == directory.name, 'candidate_owner_changed')
        state['candidate_id'] = candidate_id
        phase(directory, state, 'candidate_stop_intent')
        host.restart_policy(candidate_id, NO_RESTART)
        host.terminate(candidate_id)
        host.wait_stopped(candidate_id)
        for network in NETWORKS:
            host.disconnect(candidate_id, network)
        if host.inspect(candidate_id)['Name'] == '/' + APP:
            host.rename(candidate_id, APP + '-failed-' + directory.name)
    current = host.inspect(old['Id'])
    require(current['Image'] == plan['old']['Image'], 'retained_old_image_changed')
    if current_phase == 'old_stop_intent':
        # SIGTERM may have been accepted even though its caller got no ACK.
        host.restart_policy(old['Id'], NO_RESTART)
        host.terminate(old['Id'])
        host.wait_stopped(old['Id'])
    phase(directory, state, 'old_restore_intent')
    if current['Name'] != '/' + APP:
        require(current['Name'] == '/' + APP + '-before-' + directory.name, 'old_container_renamed_by_other_owner')
        host.rename(old['Id'], APP)
    for name, endpoint in old['NetworkSettings']['Networks'].items():
        host.connect(old['Id'], name, endpoint['Aliases'])
    policy = old['HostConfig']['RestartPolicy']
    host.restart_policy(old['Id'], policy)
    if not host.inspect(old['Id'])['State']['Running']:
        host.start(old['Id'])
    restored = host.healthy(old['Id'])
    verify_app(host, old, restored, plan, plan['old']['Image'], policy)
    state['rollback_binding'] = changed_binding(plan['binding'], restored)
    phase(directory, state, 'rollback_binding_intent')
    host.binding_cas(allowed, state['rollback_binding'])
    verify_backup_source(host, state['rollback_binding'], OLD_IMAGE, BASE)
    phase(directory, state, 'old_committed')
    return finish(host, directory, plan, state, 'rolled_back')


def apply(host, directory):
    require(not (directory/'state.json').exists(), 'already_attempted_use_status_or_recover')
    plan = read(directory/'plan.json')
    check(host, plan)
    old = host.inspect(plan['old']['Id'])
    save(directory/'private-old.json', old)  # Secret-bearing inspect remains root-only on GCP.
    state = {'old_id': old['Id'], 'candidate_id': None, 'source': SOURCE}
    close_gate(host, directory, plan, state, 'gate_install_intent')
    state['timer_owned'] = True
    phase(directory, state, 'timer_stop_intent')
    pause_backup(host)
    drain(host, directory, plan, old['Id'])
    require(identity(host.inspect(old['Id'])) == plan['old'], 'old_identity_changed_before_stop')
    require(host.binding() == plan['binding'], 'backup_source_binding_changed')
    phase(directory, state, 'old_stop_intent')
    host.restart_policy(old['Id'], NO_RESTART)
    host.terminate(old['Id'])
    host.wait_stopped(old['Id'])
    phase(directory, state, 'old_detach_intent')
    host.rename(old['Id'], APP + '-before-' + directory.name)
    for name in NETWORKS:
        host.disconnect(old['Id'], name)
    phase(directory, state, 'candidate_create_intent')
    spec = payload(old, plan['image'], host.image(plan['image'])['Config'], directory.name)
    spec['HostConfig']['RestartPolicy'] = NO_RESTART
    state['candidate_id'] = host.create(spec)
    phase(directory, state, 'candidate_start_intent')
    require(not host.inspect(old['Id'])['State']['Running'], 'old_app_still_running')
    host.start(state['candidate_id'])
    app = host.healthy(state['candidate_id'])
    verify_app(host, old, app, plan, plan['image'], NO_RESTART, candidate=True)
    state['candidate_binding'] = changed_binding(plan['binding'], app)
    phase(directory, state, 'candidate_binding_intent')
    host.binding_cas([plan['binding']], state['candidate_binding'])
    verify_backup_source(host, state['candidate_binding'], plan['image'], SOURCE)
    policy = old['HostConfig']['RestartPolicy']
    host.restart_policy(state['candidate_id'], policy)
    require(host.inspect(state['candidate_id'])['HostConfig']['RestartPolicy'] == policy, 'candidate_restart_policy_not_restored')
    phase(directory, state, 'candidate_committed')
    return finish(host, directory, plan, state, 'verified')


def rollback(host, directory, attempt=1):
    plan, state = read(directory/'plan.json'), read(directory/'state.json')
    require(state['phase'] in ('verified', 'rollback_aborted'), 'rollback_not_verified_candidate')
    require(attempt > state.get('rollback_attempt', 0), 'rollback_attempt_already_used')
    require(host.binding() == state['candidate_binding'], 'rollback_source_binding_changed')
    require(host.unit(TIMER) == plan['timer'] and host.unit(BACKUP).get('ActiveState') == 'inactive', 'backup_not_waiting_for_rollback')
    state.update(timer_owned=False, rollback_attempt=attempt)
    close_gate(host, directory, plan, state, 'rollback_gate_install_intent')
    state['timer_owned'] = True
    phase(directory, state, 'rollback_timer_stop_intent')
    pause_backup(host)
    drain(host, directory, plan, state['candidate_id'])
    phase(directory, state, 'manual_rollback_intent')
    return recover(host, directory)


def service_command(directory, action='apply', attempt=1):
    script = str(Path(__file__).resolve())
    suffix = '' if action == 'apply' else '-' + action + (('-' + str(attempt)) if attempt > 1 else '')
    unit = 'sub2-argon502-' + directory.name + suffix
    command = ['systemd-run', '--unit=' + unit, '--no-block', '--collect', '--property=Type=oneshot',
               '--property=TimeoutStartSec=600', '--property=TimeoutStopSec=600', '--property=UMask=0077', '--property=KillMode=control-group']
    if action != 'recover':
        command += ['--property=ExecStopPost=/usr/bin/python3 -B ' + script + ' recover --run-dir ' + str(directory)]
    command += ['/usr/bin/python3', '-B', script, action, '--run-dir', str(directory)]
    if action == 'rollback':
        command += ['--attempt', str(attempt)]
    return command


def main():
    parser = argparse.ArgumentParser(__doc__)
    parser.add_argument('action', choices=('prepare', 'launch', 'apply', 'recover', 'rollback', 'launch-rollback', 'launch-recover', 'status'))
    parser.add_argument('--run-dir', required=True, type=Path)
    parser.add_argument('--attempt', type=int, default=1, help=argparse.SUPPRESS)
    args = parser.parse_args()
    require(os.name == 'posix' and os.geteuid() == 0, 'gcp_root_required')
    os.umask(0o077)
    directory = args.run_dir
    require(directory.parent == ROOT and re.fullmatch(r'run-[0-9]{8}T[0-9]{6}Z', directory.name), 'run_directory_invalid')
    require(not directory.is_symlink(), 'run_directory_symlink')
    if args.action == 'status':
        state = read(directory/'state.json') if (directory/'state.json').exists() else {'phase': 'not_started'}
        gate = Host().gate(directory, read(directory/'plan.json'))
        state['gate_families'] = sorted(gate.present())
        if state['gate_families']:
            try:
                state['gate_counters_live'] = gate.verify()
            except GateError as error:
                state['gate_error'] = str(error)
        print(json.dumps(state))
        return
    if args.action.startswith('launch'):
        require(directory.is_dir() and directory.stat().st_mode & 0o077 == 0, 'run_not_private')
        action, attempt = 'apply', 1
        if args.action == 'launch':
            require(not (directory/'state.json').exists(), 'already_attempted')
            marker = directory/'launch.json'
        else:
            action = 'rollback' if args.action == 'launch-rollback' else 'recover'
            state = read(directory/'state.json')
            previous = list(directory.glob('launch-' + action + '-*.json'))
            attempt = len(previous) + 1
            if action == 'rollback':
                require(state['phase'] in ('verified', 'rollback_aborted'), 'rollback_not_verified_candidate')
                require(not previous or state['phase'] == 'rollback_aborted' and state['rollback_attempt'] == attempt - 1,
                        'rollback_launch_unknown_use_status')
            else:
                require(state['phase'] not in FINAL, 'run_already_settled')
            marker = directory/('launch-' + action + '-' + str(attempt) + '.json')
        require(not marker.exists(), 'launch_already_attempted')
        command = service_command(directory, action, attempt)
        with marker.open('x', encoding='utf-8') as file:
            json.dump({'at': stamp(), 'unit': command[1].split('=', 1)[1]}, file)
            file.flush()
            os.fsync(file.fileno())
        result = subprocess.run(command, capture_output=True, text=True, timeout=15)
        require(result.returncode == 0, 'systemd_launch_unknown_inspect_unit_do_not_relaunch')
        print(json.dumps({'unit': command[1].split('=', 1)[1], 'accepted': True, 'state': str(directory/'state.json')}))
        return
    if args.action in ('apply', 'rollback'):
        require(bool(os.environ.get('INVOCATION_ID')), 'use_launch_system_service')
    import fcntl
    with Path('/run/lock/sub2-gcp-app-switch.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        host = Host()
        if args.action == 'prepare':
            require(not directory.exists(), 'run_already_exists')
            plan = prepare(host, IMAGE)
            host.gate(directory, plan).preflight()
            directory.mkdir(mode=0o700, parents=False)
            save(directory/'plan.json', plan)
            print(json.dumps({'prepared': True, 'plan': str(directory/'plan.json'), 'source': SOURCE}))
        else:
            require(directory.is_dir() and directory.stat().st_mode & 0o077 == 0, 'run_not_private')
            if args.action == 'apply':
                result = apply(host, directory)
            elif args.action == 'rollback':
                result = rollback(host, directory, args.attempt)
            else:
                result = recover(host, directory)
            print(json.dumps(result))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'failed': str(error) if isinstance(error, (Failure, GateError)) else type(error).__name__}))
        raise SystemExit(2)
