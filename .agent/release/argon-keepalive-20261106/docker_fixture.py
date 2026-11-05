#!/usr/bin/env python3
"""Local Docker Desktop fixture only; needs two health-only images and its own scratch volume."""
import copy
import json
import os
from pathlib import Path
import socket
import subprocess
import urllib.request
import uuid
from unittest.mock import patch
import release as r
from test_release import FakeGate


class DockerFixture(r.Host):
    def __init__(self):
        self.active = True
        self.lose_create_ack = False
        self.created = []
        self.starts = []
        self.exec_checks = 0
        self.gated = False

    def event(self, label, action):
        return action()

    def ingress_facts(self):
        return {'bridge': 'br-522fc2807c92', 'nginx_uid': 33}

    def gate(self, directory, plan):
        return FakeGate(self)

    def prove_gate(self, directory, plan):
        return self.gate(directory, plan).verify()

    def unit(self, name):
        return {'ActiveState': ('active' if self.active else 'inactive') if name == r.TIMER else 'inactive',
                'UnitFileState': 'enabled' if name == r.TIMER else 'static', 'SubState': 'waiting' if self.active else 'dead'}

    def timer(self, active):
        self.active = active

    def backup_source_check(self):
        app = self.inspect(r.APP)
        source = self.binding()
        assert app['State']['Running'] and source['app']['id'] == app['Id']
        return {'stage': 'core_backup_source_bound', 'source': source, 'image': app['Image'],
                'revision': app['Config']['Labels']['org.opencontainers.image.revision']}

    def health_port(self):
        # Controller is on the isolated public-side fixture bridge; the real
        # host 127.0.0.1 bind is asserted by verify_app's full HostConfig match.
        app = self.inspect(r.APP)
        address = app['NetworkSettings']['Networks'][r.APP_NETWORK]['IPAddress']
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open('http://' + address + ':8080/health', timeout=3) as response:
            assert response.status == 200 and json.loads(response.read()) == {'status': 'ok'}

    def exec_adapter(self, command, **_kwargs):
        assert command[:2] == ['docker', 'exec']
        created = self.api('POST', '/containers/' + command[2] + '/exec', {'AttachStdout': True, 'AttachStderr': True, 'Cmd': command[3:]})
        connection = r.Connection()
        try:
            connection.request('POST', '/v1.45/exec/' + created['Id'] + '/start', json.dumps({'Detach': False, 'Tty': False}), {'Content-Type': 'application/json'})
            response = connection.getresponse()
            assert response.status == 200
            body = response.read()
        finally:
            connection.close()
        output = b''
        while body:
            size = int.from_bytes(body[4:8], 'big')
            output += body[8:8+size]
            body = body[8+size:]
        result = self.api('GET', '/exec/' + created['Id'] + '/json')
        self.exec_checks += 1
        return subprocess.CompletedProcess(command, result['ExitCode'], output.decode(), '')

    def quiet(self, container):
        with patch.object(r.subprocess, 'run', self.exec_adapter):
            super().quiet(container)

    def create(self, spec):
        candidate = super().create(spec)
        self.created.append(candidate)
        if self.lose_create_ack:
            self.lose_create_ack = False
            raise KeyboardInterrupt('fixture_create_accepted_ack_lost')
        return candidate

    def start(self, container):
        # Inspect the other retained app before every actual Docker START.
        for other in self.created:
            if other != container:
                assert not self.inspect(other)['State']['Running']
        if container in self.created:
            assert not self.inspect(self.old_id)['State']['Running']
        else:
            for candidate in self.created:
                assert not self.inspect(candidate)['State']['Running']
        self.starts.append(container)
        super().start(container)


def main():
    assert os.environ.get('ARGON_LOCAL_FIXTURE') == '1', 'local fixture invocation required'
    host = DockerFixture()
    unique = 'argon-fixture-' + uuid.uuid4().hex[:10]
    controller = os.environ['HOSTNAME']
    app_network, dependency_network = unique + '-app', unique + '-dep'
    r.APP, r.APP_NETWORK, r.DEPENDENCY_NETWORK = unique, app_network, dependency_network
    r.NETWORKS = {app_network, dependency_network}
    r.HOST_PORT = '19861'
    r.OLD_IMAGE = host.image('argon-keepalive-fixture:old')['Id']
    image = r.IMAGE = host.image('argon-keepalive-fixture:new')['Id']
    volume = host.api('GET', '/volumes/' + os.environ['FIXTURE_VOLUME'])
    directory = Path('/scratch')/unique
    directory.mkdir(mode=0o700)
    (directory/'appdata').mkdir()
    r.BINDING = directory/'source.json'
    daemon_data = volume['Mountpoint'] + '/' + unique + '/appdata'
    created, networks = [], []
    connected = False
    try:
        for name, internal in ((app_network, False), (dependency_network, True)):
            host.api('POST', '/networks/create', {'Name': name, 'Driver': 'bridge', 'Internal': internal})
            networks.append(name)
        host.connect(controller, app_network, [controller])
        connected = True
        deps = {}
        for role in ('pg', 'redis'):
            name = unique + '-' + role
            container = host.api('POST', '/containers/create?name=' + name, {
                'Image': r.OLD_IMAGE, 'HostConfig': {'NetworkMode': dependency_network},
                'NetworkingConfig': {'EndpointsConfig': {dependency_network: {}}}})['Id']
            created.append(container)
            host.api('POST', '/containers/' + container + '/start')
            dep = host.healthy(container)
            deps[role] = {'name': name, 'id': container, 'started_at': dep['State']['StartedAt']}
        spec = {'Image': r.OLD_IMAGE, 'Env': ['FIXTURE_ONLY=preserved'],
                'Labels': {'task46.run': 'final-20261105-window1'},
                'HostConfig': {'Binds': [daemon_data + ':/app/data:rw'], 'NetworkMode': app_network,
                               'RestartPolicy': {'Name': 'unless-stopped'},
                               'PortBindings': {'8080/tcp': [{'HostIp': '127.0.0.1', 'HostPort': r.HOST_PORT}]},
                               'LogConfig': {'Type': 'json-file', 'Config': {'max-size': '20m', 'max-file': '3'}}},
                'NetworkingConfig': {'EndpointsConfig': {name: {'Aliases': [r.APP]} for name in r.NETWORKS}}}
        old_id = host.api('POST', '/containers/create?name=' + r.APP, spec)['Id']
        host.old_id = old_id
        created.append(old_id)
        host.api('POST', '/containers/' + old_id + '/start')
        old = host.healthy(old_id)
        source = {'app': {'name': r.APP, 'id': old_id, 'started_at': old['State']['StartedAt']},
                  **deps, 'dependency_network': dependency_network, 'appdata': daemon_data}
        r.save(r.BINDING, source)
        plan = r.prepare(host, image)
        r.save(directory/'plan.json', plan)
        # Gate semantics run on the target kernel; this fixture owns Docker's
        # real graceful-stop, clone, detach/attach and latest-state rollback.
        result = r.apply(host, directory)
        candidate_id = result['candidate_id']
        assert result['phase'] == 'verified' and host.binding()['app']['id'] == candidate_id and host.active
        assert not host.inspect(old_id)['State']['Running']
        assert not host.gated
        assert r.rollback(host, directory)['phase'] == 'rolled_back'
        assert not host.gated
        assert host.binding()['app']['id'] == old_id and host.active
        assert host.binding()['app']['started_at'] == host.inspect(old_id)['State']['StartedAt']
        assert host.binding()['app']['started_at'] != source['app']['started_at']
        assert not host.inspect(candidate_id)['State']['Running']
        # A second independent fixture run drops CREATE's acknowledgement.
        directory2 = Path('/scratch')/(unique + '-ack-loss')
        directory2.mkdir(mode=0o700)
        r.save(directory2/'plan.json', r.prepare(host, image))
        host.lose_create_ack = True
        try:
            r.apply(host, directory2)
            raise AssertionError('ack_loss_not_injected')
        except KeyboardInterrupt:
            pass
        assert r.recover(host, directory2)['phase'] == 'rolled_back'
        assert host.binding()['app']['id'] == old_id and host.active
        assert host.starts.count(candidate_id) == 1
        assert all(not host.inspect(value)['State']['Running'] for value in host.created)
        assert r.dependencies_of(host, host.binding(), old_id) == plan['dependencies']
        assert {key: value for key, value in host.binding().items() if key != 'app'} == {key: value for key, value in source.items() if key != 'app'}
        assert not host.gated
        print(json.dumps({'docker': 'actual local daemon', 'result': 'passed', 'cases': ['actual_clone_double_networks_and_preserved_runtime', 'switch_and_latest_binding_rollback', 'create_accepted_ack_lost_discovery_and_rollback'], 'tcp_exec_samples': host.exec_checks, 'provider_requests': 0, 'systemd_and_gate': 'stubbed here; packet rules tested in target netns', 'health_transport': 'fixture bridge, not GCP loopback'}))
    finally:
        # Only this fixture's UUID-owned containers/networks are removed.
        for container in list(dict.fromkeys(created + host.created)):
            host.api('DELETE', '/containers/' + container + '?force=1&v=1')
        if connected:
            host.disconnect(controller, app_network)
        for name in reversed(networks):
            host.api('DELETE', '/networks/' + name)


if __name__ == '__main__':
    main()
