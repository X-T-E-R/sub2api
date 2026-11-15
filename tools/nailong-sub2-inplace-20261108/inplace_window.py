#!/usr/bin/env python3
"""Nailong dual-slot Sub2API release and recovery.

Start and warm the inactive loopback slot while production remains fully open.
Reload the shared Nginx upstream, rebind the canonical container and backup, then
silently retire the old slot in the host-managed release service. No admission
hold, network disconnection, dependency restart, or synthetic inference probe.
"""
import argparse
from contextlib import contextmanager
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.request

HERE = Path(__file__).resolve().parent
SCHEMA = 'nailong-sub2-bluegreen-go-2'
TARGET_HOST = '136.66.61.218'
APP_NAME = 'sub2api-gcp'
ALIAS = 'sub2api-gcp'
SLOT_PORTS = (17861, 17862)
import slot_routing
APP_NETWORK = 'sub2api-nailong-app'
DEPENDENCY_NETWORK = 'sub2api-nailong'
PG_NAME = 'sub2api-nailong-postgres'
REDIS_NAME = 'sub2api-nailong-redis'
AXON_NAME = 'task51-gcp-nailong-usw1'
PROTECTED_UNITS = (PG_NAME, REDIS_NAME, AXON_NAME, 'axonhub-gcp', 'task48-xray.service', 'docker.service')
BACKUP_TIMER = 'sub2-core-backup.timer'
BACKUP_SOURCE_JSON = '/etc/sub2-core-backup/source.json'
CORE_BACKUP_PY = '/opt/sub2-core-backup/core-backup/core_backup.py'
CORE_IDENTITY_PY = '/opt/sub2-core-backup/core-backup/core_identity.py'
PARENT_CORE_IDENTITY_SHA = '94d1b33189f1767c7a0bff4191e62476b1d93c2ef2419a7a2b4ba2b1afac55f5'
CANDIDATE_CORE_IDENTITY = HERE / 'core_identity_candidate.py'
NAILONG_INPLACE_ROOT = '/opt/sub2api-nailong/inplace'
NAILONG_SYSTEMD_UNIT_PATH = '/etc/systemd/system/sub2-nailong-inplace@.service'
EXPECTED_THOUGHT_POLICY = {
    'suppress_thought_only_stop': True,
    'continue_thought_only': False,
    'continue_zero_output': False,
}
TERMINAL_STAGES = {'forward_terminal', 'recovered_to_previous_image'}
WINDOW_NAME_RE = re.compile(r'^[a-z0-9][a-z0-9._-]{5,63}$')
HEX64_RE = re.compile(r'^[0-9a-f]{64}$')
HEX40_RE = re.compile(r'^[0-9a-f]{40}$')
SAFE_LABEL_RE = re.compile(r'^(?:[A-Za-z_][A-Za-z0-9_]{0,80}|docker_api_\d{3}(?::[A-Za-z0-9_./: -]{1,200})?)$')

SYSTEMD_UNIT_CONTENT = """[Unit]
Description=Sub2API Nailong Same-Host In-Place Release/Recover (%i)
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
User=root
UMask=0077
WorkingDirectory=/opt/sub2api-nailong/inplace
ExecStart=/usr/bin/python3 -B /opt/sub2api-nailong/inplace/inplace_window.py host-run --go /opt/sub2api-nailong/inplace/windows/%i/go.json
TimeoutStartSec=0
StandardOutput=journal
StandardError=journal
"""


class InplaceError(Exception):
    pass


def error_label(exc):
    """Preserve specific sanitized error codes from InplaceError, migration.Failure, and contract.ContractError.

    Never collapses a bounded label into generic `'Failure'` or leaks raw credentials/SQL/env text.
    """
    if isinstance(exc, InplaceError):
        text = ' '.join(str(exc).split())
        if text and len(text) <= 220 and not any(tok in text for tok in ('PASSWORD', 'SECRET', 'TOKEN', 'Bearer', 'BEGIN ')):
            return text
        return 'inplace_error'
    cls_name = type(exc).__name__
    text = ' '.join(str(exc).split())
    if text.startswith('inplace_failed='):
        text = text.split('inplace_failed=', 1)[1].strip()
    elif text.startswith('migration_failed='):
        text = text.split('migration_failed=', 1)[1].strip()
    if cls_name in ('Failure', 'ContractError') and SAFE_LABEL_RE.fullmatch(text):
        return text
    if SAFE_LABEL_RE.fullmatch(text):
        return text
    return cls_name


def claim(directory, name, payload):
    path = Path(directory) / name
    with path.open('x', encoding='utf8', newline='\n') as handle:
        json.dump(payload, handle, indent=2)
        handle.write('\n')
    return payload


def write_atomic(directory, name, payload):
    directory = Path(directory)
    target = directory / name
    tmp = directory / (name + '.tmp')
    tmp.write_text(json.dumps(payload, indent=2) + '\n', encoding='utf8', newline='\n')
    tmp.replace(target)
    return payload


def record_event(directory, stage, extra=None):
    body = {'stage': stage, 'at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()), **(extra or {})}
    write_atomic(directory, 'status.json', body)
    with (Path(directory) / 'events.jsonl').open('a', encoding='utf8', newline='\n') as handle:
        handle.write(json.dumps(body) + '\n')
    return body


def archive_container_name(window, container_id):
    return f'sub2api-gcp-before-{window}-{container_id[:12]}'


def validate_go(go, action, filename=None):
    if filename == 'go.template.json' or str(go.get('window', '')).endswith('TEMPLATE'):
        raise InplaceError('template_is_not_a_go')
    if not isinstance(go, dict) or go.get('schema') != SCHEMA or go.get('explicit_parent_go') is not True:
        raise InplaceError('explicit_parent_go_required')
    if action not in ('forward', 'recover') or go.get('action') != action:
        raise InplaceError('go_action_mismatch')
    window = go.get('window')
    if not isinstance(window, str) or not WINDOW_NAME_RE.fullmatch(window):
        raise InplaceError('invalid_window_name')
    if go.get('host') != TARGET_HOST or go.get('app_name') != APP_NAME:
        raise InplaceError('host_or_app_mismatch')
    if go.get('alias') != ALIAS or go.get('current_port') not in SLOT_PORTS:
        raise InplaceError('endpoint_contract_mismatch')
    if go.get('app_network') != APP_NETWORK or go.get('dependency_network') != DEPENDENCY_NETWORK:
        raise InplaceError('network_contract_mismatch')
    if (go.get('dual_slot') is not True or go.get('stop_only_sub2') is not True
            or go.get('preserve_pg_redis_appdata') is not True
            or go.get('forbid_old_gcp_fallback') is not True
            or go.get('dns_apply') is not False
            or go.get('worker_apply') is not False
            or go.get('rebind_daily_backup') is not True):
        raise InplaceError('scope_guard_violation')
    appdata = go.get('appdata')
    if not isinstance(appdata, str) or not appdata.startswith('/opt/sub2api-nailong/runs/') or not appdata.endswith('/appdata'):
        raise InplaceError('appdata_path_invalid')
    if not HEX64_RE.fullmatch(str(go.get('current_container_id') or '')):
        raise InplaceError('current_container_id_invalid')
    for img_key in ('current_image', 'target_image'):
        img = str(go.get(img_key) or '')
        if not img.startswith('sha256:') or not HEX64_RE.fullmatch(img.removeprefix('sha256:')):
            raise InplaceError('image_id_invalid')
    for rev_key in ('current_revision', 'target_revision'):
        if not HEX40_RE.fullmatch(str(go.get(rev_key) or '')):
            raise InplaceError('revision_invalid')
    if go['current_image'] == go['target_image'] or go['current_revision'] == go['target_revision']:
        raise InplaceError('target_must_differ_from_current')
    compatible = go.get('rollback_compatible')
    if compatible is not None and (not isinstance(compatible, dict)
            or not re.fullmatch(r'sha256:[0-9a-f]{64}', str(compatible.get('image')))
            or not HEX40_RE.fullmatch(str(compatible.get('revision')))):
        raise InplaceError('rollback_compatible_identity_invalid')
    if not isinstance(go.get('current_started_at'), str) or not go['current_started_at']:
        raise InplaceError('current_started_at_required')
    for dep_key, dep_name in (('pg', PG_NAME), ('redis', REDIS_NAME), ('axon_nailong', AXON_NAME)):
        dep = go.get(dep_key) or {}
        if (dep.get('name') != dep_name or not HEX64_RE.fullmatch(str(dep.get('id') or ''))
                or not isinstance(dep.get('started_at'), str) or not dep['started_at']):
            raise InplaceError(f'{dep_key}_pin_invalid')
    if go.get('thought_policy') != EXPECTED_THOUGHT_POLICY:
        raise InplaceError('thought_policy_contract_mismatch')
    return go


def assert_stoppable(name):
    if name != APP_NAME or name in PROTECTED_UNITS:
        raise InplaceError('protected_unit_stop_forbidden')


def verify_dependencies_and_policy(live, go):
    for key, need_health in (('pg', True), ('redis', True), ('axon_nailong', False)):
        observed = (live or {}).get(key) or {}
        expected = go[key]
        if (observed.get('id') != expected['id']
                or observed.get('started_at') != expected['started_at']
                or observed.get('running') is not True):
            raise InplaceError(f'{key}_epoch_or_state_changed')
        if need_health and observed.get('health') != 'healthy':
            raise InplaceError(f'{key}_unhealthy')
    if (live or {}).get('thought_policy') != EXPECTED_THOUGHT_POLICY:
        raise InplaceError('thought_policy_drifted')


def verify_live_current_app(live, go):
    app = (live or {}).get('app') or {}
    if (app.get('id') != go['current_container_id']
            or app.get('image') != go['current_image']
            or app.get('revision') != go['current_revision']
            or app.get('started_at') != go['current_started_at']
            or app.get('running') is not True
            or app.get('health') != 'healthy'
            or app.get('restart_count') != 0
            or app.get('appdata') != go['appdata']):
        raise InplaceError('current_app_identity_mismatch')
    loaded = (live or {}).get('loaded_target') or {}
    if loaded.get('id') != go['target_image'] or loaded.get('revision') != go['target_revision']:
        raise InplaceError('target_image_not_staged')


def build_updated_source_binding(existing_binding, live_roles, expected_appdata):
    if not isinstance(existing_binding, dict) or not isinstance(live_roles, dict):
        raise InplaceError('backup_binding_invalid')
    if existing_binding.get('dependency_network') != DEPENDENCY_NETWORK:
        raise InplaceError('backup_network_mismatch')
    updated = json.loads(json.dumps(existing_binding))
    updated['appdata'] = expected_appdata
    for role, expected_name in (('app', APP_NAME), ('pg', PG_NAME), ('redis', REDIS_NAME)):
        info = live_roles.get(role) or {}
        if (info.get('name') != expected_name
                or not HEX64_RE.fullmatch(str(info.get('id') or ''))
                or not isinstance(info.get('started_at'), str) or not info['started_at']
                or info.get('running') is not True
                or info.get('health') != 'healthy'):
            raise InplaceError(f'backup_role_unhealthy_{role}')
        updated[role] = {
            'name': expected_name,
            'id': info['id'],
            'started_at': info['started_at'],
        }
    return updated


def rebind_daily_backup(ops, directory, expected_app_id, expected_image, expected_revision, expected_appdata, pg_pin, redis_pin):
    inspected = ops.inspect_backup_roles()
    if inspected.get('appdata') != expected_appdata:
        raise InplaceError('backup_appdata_mismatch')
    roles = inspected.get('roles') or {}
    if (roles.get('app') or {}).get('id') != expected_app_id:
        raise InplaceError('backup_app_id_mismatch')
    if (roles.get('pg') or {}).get('id') != pg_pin['id'] or (roles.get('pg') or {}).get('started_at') != pg_pin['started_at']:
        raise InplaceError('pg_epoch_or_state_changed')
    if (roles.get('redis') or {}).get('id') != redis_pin['id'] or (roles.get('redis') or {}).get('started_at') != redis_pin['started_at']:
        raise InplaceError('redis_epoch_or_state_changed')
    updated_binding = build_updated_source_binding(inspected.get('binding'), roles, expected_appdata)
    write_atomic(directory, 'backup-rebind-intent.json', {'binding': updated_binding})
    ops.write_source_binding(updated_binding)
    readback = ops.run_source_check()
    if (readback.get('stage') != 'core_backup_source_bound'
            or readback.get('image') != expected_image
            or readback.get('revision') != expected_revision):
        raise InplaceError('backup_source_check_mismatch')
    bound_source = readback.get('source') or {}
    for role in ('app', 'pg', 'redis'):
        if (bound_source.get(role) or {}).get('id') != updated_binding[role]['id']:
            raise InplaceError('backup_source_check_mismatch')
        if (bound_source.get(role) or {}).get('started_at') != updated_binding[role]['started_at']:
            raise InplaceError('backup_source_check_mismatch')
    if bound_source.get('appdata') != expected_appdata:
        raise InplaceError('backup_source_check_mismatch')
    ops.ensure_backup_timer_active()
    timer_state = ops.backup_timer_state()
    if timer_state != 'active':
        raise InplaceError('backup_timer_not_active')
    result = {
        'stage': 'core_backup_source_bound',
        'timer': timer_state,
        'image': readback['image'],
        'revision': readback['revision'],
        'source': bound_source,
    }
    write_atomic(directory, 'backup-rebound.json', result)
    return result


@contextmanager
def release_lock():
    import fcntl
    with open(Path(NAILONG_INPLACE_ROOT) / 'release.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def recover(go, ops, directory=None):
    """Warm the retained original, swap the same upstream back, then drain the candidate."""
    validate_go(go, 'recover')
    directory = Path(directory) if directory is not None else HERE / 'windows' / go['window']
    directory.mkdir(parents=True, exist_ok=True)
    live = ops.observe_live(go['target_revision'])
    verify_dependencies_and_policy(live, go)
    active = ops.routed_container()
    original = ops.prepare_original(go, directory)
    route = None
    if active['id'] != original['id']:
        route = ops.activate_slot(active['id'], original['id'], go['window'] + '-recover')
        write_atomic(directory, 'recovery-traffic-switched.json', route)
    else:
        ops.make_canonical(original['id'], go['window'] + '-recover')
    backup = rebind_daily_backup(
        ops, directory, original['id'], original['image'], original['revision'],
        go['appdata'], go['pg'], go['redis'])
    if route:
        ops.schedule_retirement(active['id'], route['retiring_workers'], directory)
    body = record_event(directory, 'recovered_to_previous_image', {
        'app': original, 'backup': backup, 'route': route, 'execution_id': go.get('execution_id'),
        'pg_preserved': go['pg']['id'], 'redis_preserved': go['redis']['id']})
    write_atomic(directory, 'recover-terminal.json', body)
    return body


def forward(go, ops, template_name=None, directory=None):
    validate_go(go, 'forward', template_name)
    directory = Path(directory) if directory is not None else HERE / 'windows' / go['window']
    directory.mkdir(parents=True, exist_ok=True)
    if (directory / 'forward-started.json').exists():
        raise InplaceError('window_already_claimed')
    live = ops.observe_live(go['target_revision'])
    verify_dependencies_and_policy(live, go)
    verify_live_current_app(live, go)
    ops.verify_peer_safe_image(go['target_image'], go['target_revision'])
    if go.get('rollback_compatible'):
        ops.verify_peer_safe_image(go['rollback_compatible']['image'], go['rollback_compatible']['revision'])
        ops.admit_core_identity_pair(b'', go['rollback_compatible']['image'], go['rollback_compatible']['revision'])
    if ops.routed_container()['id'] != go['current_container_id']:
        raise InplaceError('routed_container_mismatch')
    if go.get('admit_core_identity_pair') is True:
        receipt = ops.admit_core_identity_pair(CANDIDATE_CORE_IDENTITY.read_bytes(), go['target_image'], go['target_revision'])
        claim(directory, 'core-identity-admitted.json', receipt)
    spec = ops.capture_current_container_spec(go['current_container_id'], go['appdata'])
    if spec['port'] != go['current_port']:
        raise InplaceError('current_port_mismatch')
    target_port = next(port for port in SLOT_PORTS if port != spec['port'])
    claim(directory, 'forward-started.json', {'window': go['window'], 'old_id': go['current_container_id'],
          'target_port': target_port, 'target_image': go['target_image']})
    started = None
    try:
        started = ops.create_and_start_target(go['window'], go['target_image'], go['target_revision'],
                                               spec['env'], go['appdata'], target_port, directory)
        claim(directory, 'target-healthy.json', started)
        record_event(directory, 'target_healthy_old_still_serving', started)
        verify_dependencies_and_policy(ops.observe_live(go['target_revision']), go)
        claim(directory, 'switch-intent.json', {'old_id': go['current_container_id'], 'new_id': started['id']})
        route = ops.activate_slot(go['current_container_id'], started['id'], go['window'])
        started['name'] = APP_NAME
        claim(directory, 'traffic-switched.json', route)
        record_event(directory, 'traffic_switched', route)
        backup = rebind_daily_backup(ops, directory, started['id'], go['target_image'], go['target_revision'],
                                     go['appdata'], go['pg'], go['redis'])
        if go.get('verify_rollback') is True:
            # Commissioning round-trip while BOTH slots remain warm. No old
            # image restart is needed, and the final active slot is the target.
            back = ops.activate_slot(started['id'], go['current_container_id'], go['window'] + '-rollback-check')
            rebind_daily_backup(ops, directory, go['current_container_id'], go['current_image'], go['current_revision'],
                                go['appdata'], go['pg'], go['redis'])
            again = ops.activate_slot(go['current_container_id'], started['id'], go['window'] + '-resume')
            backup = rebind_daily_backup(ops, directory, started['id'], go['target_image'], go['target_revision'],
                                         go['appdata'], go['pg'], go['redis'])
            claim(directory, 'rollback-verified.json', {'back': back, 'forward': again, 'backup': backup})
            route['retiring_workers'] = sorted(set(route['retiring_workers'] + back['retiring_workers'] + again['retiring_workers']))
    except Exception as error:
        write_atomic(directory, 'forward-error.json', {'error': error_label(error)})
        # Reconcile actual routing, not an ACK marker: reload/rename may have
        # succeeded even if its acknowledgement or the receipt write was lost.
        recovered = recover(dict(go, action='recover'), ops, directory)
        intent_path = directory / 'target-created.json'
        if intent_path.exists() and not recovered.get('route'):
            candidate = json.loads(intent_path.read_text())
            ops.discard_unrouted_candidate(candidate['id'], directory)
        raise
    # A retirement failure must never roll back a healthy new release. It leaves
    # the old container available, on both networks, for explicit recovery.
    retirement = ops.schedule_retirement(go['current_container_id'], route['retiring_workers'], directory)
    body = record_event(directory, 'forward_terminal', {
        'window': go['window'], 'app': started, 'route': route, 'retirement': retirement, 'execution_id': go.get('execution_id'),
        'retained_previous_container': {'id': go['current_container_id']},
        'pg_preserved': go['pg']['id'], 'redis_preserved': go['redis']['id'],
        'axon_preserved': go['axon_nailong']['id'], 'appdata_preserved': go['appdata'],
        'backup': backup, 'old_gcp_touched': False, 'dns_applied_by_runner': False,
        'worker_applied_by_runner': False})
    claim(directory, 'forward-terminal.json', body)
    return body


def ready(ops, template):
    live = ops.observe_live(template['target_revision'])
    verify_dependencies_and_policy(live, template)
    verify_live_current_app(live, template)
    if live['app']['port'] != template['current_port']:
        raise InplaceError('current_port_mismatch')
    if live['backup_timer'] != 'active' or live['source_check'].get('stage') != 'core_backup_source_bound':
        raise InplaceError('backup_not_ready')
    return {'stage': 'bluegreen_ready', 'live': live}


def bind_nailong_docker_api141(migration_mod, contract_mod):
    """Bind `migration.api` to Docker Engine API `/v1.41` (matching `remote_entry.py`).

    Nailong's `tools/migration.py` defaults to `/v1.45`, which Nailong's Docker 20.10.24
    daemon rejects with HTTP 400 (`Maximum supported API version is 1.41`).
    Importing `migration` without binding `/v1.41` caused `migration.inspect` to fail
    with `Failure('docker_api_failed')`.
    """
    def _docker_api(method, path, body=None):
        conn = migration_mod.DockerConnection()
        try:
            payload = json.dumps(body).encode('utf8') if body is not None else None
            conn.request(method, '/v1.41' + path, payload, {'Content-Type': 'application/json'})
            res = conn.getresponse()
            raw = res.read()
            if res.status not in (200, 201, 204):
                raise InplaceError(contract_mod.docker_failure_label(res.status, raw))
            return raw
        finally:
            conn.close()

    migration_mod.api = _docker_api
    return _docker_api


class LocalNailongOps:
    """Runs directly on Nailong (inside `sub2-nailong-inplace@<window>.service`) with zero SSH dependencies."""

    def __init__(self, tools_dir='/opt/sub2api-nailong/tools'):
        if tools_dir and tools_dir not in sys.path:
            sys.path.insert(0, tools_dir)
        import contract
        import migration
        self._contract = contract
        self._migration = migration
        bind_nailong_docker_api141(self._migration, self._contract)

    def api141(self, method, path, body=None):
        return self._migration.api(method, path, body)

    def inspect_via_api141(self, ref):
        return json.loads(self.api141('GET', '/containers/' + ref + '/json'))

    def probe_docker_api141(self, expected_app_id, expected_target_image):
        """Read-only probe of `/v1.41/containers/.../json` and `/v1.41/images/.../json`."""
        app = self.inspect_via_api141(APP_NAME)
        img = json.loads(self.api141('GET', '/images/' + expected_target_image + '/json'))
        rev = ((img.get('Config') or {}).get('Labels') or {}).get('org.opencontainers.image.revision')
        return {
            'ok': app.get('Id') == expected_app_id and img.get('Id') == expected_target_image,
            'api_version': '1.41',
            'app_id': app.get('Id'),
            'app_restart_policy': ((app.get('HostConfig') or {}).get('RestartPolicy') or {}).get('Name'),
            'target_image_id': img.get('Id'),
            'target_image_revision': rev,
        }

    def _inspect_container(self, ref):
        try:
            d = self.inspect_via_api141(ref)
        except InplaceError as exc:
            if str(exc).startswith('docker_api_404'):
                return None
            raise
        mounts = [m['Source'] for m in (d.get('Mounts') or []) if m.get('Destination') == '/app/data']
        return {
            'id': d['Id'],
            'name': d['Name'].lstrip('/'),
            'image': d['Image'],
            'revision': ((d['Config'].get('Labels') or {}).get('org.opencontainers.image.revision') or ''),
            'running': bool(d['State'].get('Running')),
            'status': d['State'].get('Status'),
            'health': (d['State'].get('Health') or {}).get('Status'),
            'started_at': d['State'].get('StartedAt'),
            'restart_count': d.get('RestartCount', 0),
            'port': self.container_port(d),
            'networks': sorted((d.get('NetworkSettings') or {}).get('Networks') or {}),
            'appdata': mounts[0] if len(mounts) == 1 else None,
        }

    def observe_live(self, target_revision):
        ids = subprocess.check_output(['docker', 'image', 'ls', '-q', '--no-trunc'], text=True).split()
        loaded = None
        seen = set()
        for img_id in ids:
            if img_id in seen:
                continue
            seen.add(img_id)
            try:
                info = json.loads(self.api141('GET', '/images/' + img_id + '/json'))
            except InplaceError:
                continue
            rev = ((info['Config'].get('Labels') or {}).get('org.opencontainers.image.revision') or '')
            if rev == target_revision:
                loaded = {'id': info['Id'], 'revision': rev, 'repo_tags': info.get('RepoTags') or []}
                break

        sql = "SELECT value FROM settings WHERE key = 'antigravity_thought_policy';"
        p_sql = subprocess.run(
            ['docker', 'exec', '-i', PG_NAME, 'psql', '-X', '-qAt', '-U', 'sub2api', '-d', 'sub2api'],
            input=sql.encode('utf8'), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        )
        thought_policy = json.loads(p_sql.stdout.decode('utf8').strip()) if p_sql.returncode == 0 and p_sql.stdout.strip() else None

        timer = subprocess.run(['systemctl', 'is-active', BACKUP_TIMER], stdout=subprocess.PIPE, text=True).stdout.strip()
        sc = subprocess.run(
            ['python3', '-B', CORE_BACKUP_PY, 'source-check', '--binding', BACKUP_SOURCE_JSON],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        sc_out = json.loads(sc.stdout) if sc.returncode == 0 and sc.stdout.strip() else {'error': sc.stderr.strip()}
        id_path = Path(CORE_IDENTITY_PY)
        id_sha = hashlib.sha256(id_path.read_bytes()).hexdigest() if id_path.is_file() else None
        runner_path = Path(NAILONG_INPLACE_ROOT) / 'inplace_window.py'
        unit_path = Path(NAILONG_SYSTEMD_UNIT_PATH)

        app_info = self._inspect_container(APP_NAME)
        api141_probe = None
        if app_info and loaded:
            try:
                api141_probe = self.probe_docker_api141(app_info['id'], loaded['id'])
            except Exception as exc:
                api141_probe = {'ok': False, 'error': error_label(exc)}

        return {
            'app': app_info,
            'pg': self._inspect_container(PG_NAME),
            'redis': self._inspect_container(REDIS_NAME),
            'axon_nailong': self._inspect_container(AXON_NAME),
            'loaded_target': loaded,
            'thought_policy': thought_policy,
            'backup_timer': timer,
            'source_check': sc_out,
            'nailong_identity_sha': id_sha,
            'lag_identity_sha': id_sha,
            'docker_api141_probe': api141_probe,
            'host_carrier': {
                'runner_sha256': hashlib.sha256(runner_path.read_bytes()).hexdigest() if runner_path.is_file() else None,
                'unit_present': unit_path.is_file(),
            },
        }

    def admit_core_identity_pair(self, candidate_bytes, target_image, target_revision):
        path = Path(CORE_IDENTITY_PY)
        ns = {}
        exec(compile(path.read_text(encoding='utf8'), str(path), 'exec'), ns)
        if not ns.get('admits') or not ns['admits'](target_image, target_revision):
            raise InplaceError('core_identity_pair_not_pre_staged')
        sha = hashlib.sha256(path.read_bytes()).hexdigest()
        return {'nailong_sha256': sha, 'pair': [target_image, target_revision]}

    def capture_current_container_spec(self, expected_id, expected_appdata):
        d = self.inspect_via_api141(APP_NAME)
        if d['Id'] != expected_id:
            raise InplaceError('current_container_id_mismatch')
        mounts = [m['Source'] for m in (d.get('Mounts') or []) if m.get('Destination') == '/app/data']
        if len(mounts) != 1 or mounts[0] != expected_appdata:
            raise InplaceError('current_appdata_mismatch')
        env = list(d['Config'].get('Env') or [])
        env_map = dict(item.split('=', 1) for item in env if '=' in item)
        if not env_map.get('JWT_SECRET') or not env_map.get('TOTP_ENCRYPTION_KEY'):
            raise InplaceError('jwt_or_totp_missing')
        return {'id': d['Id'], 'env': env, 'appdata': mounts[0], 'port': self.container_port(d)}

    @staticmethod
    def container_port(d):
        bindings = (d.get('NetworkSettings', {}).get('Ports', {}).get('8080/tcp') or
                    d.get('HostConfig', {}).get('PortBindings', {}).get('8080/tcp') or [])
        ports = [int(p['HostPort']) for p in bindings if p['HostIp'] == '127.0.0.1']
        return ports[0] if len(ports) == 1 else None

    def routed_container(self):
        port = slot_routing.active_port()
        rows = json.loads(self.api141('GET', '/containers/json'))
        for row in rows:
            if any(p.get('IP') == '127.0.0.1' and p.get('PublicPort') == port for p in row.get('Ports', [])):
                return self._inspect_container(row['Id'])
        raise InplaceError('active_slot_container_missing')

    def wait_ready(self, cid, image, revision):
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            info = self._inspect_container(cid)
            if info['image'] != image or info['revision'] != revision:
                raise InplaceError('target_identity_changed')
            if not info['running'] or info['restart_count']:
                raise InplaceError('target_exited_or_restarted')
            if info['health'] == 'healthy':
                try:
                    for path in ('/health', '/readyz'):
                        with urllib.request.urlopen(f"http://127.0.0.1:{info['port']}{path}", timeout=5) as response:
                            if response.status != 200:
                                raise InplaceError('target_not_ready')
                    return info
                except OSError:
                    pass
            time.sleep(1)
        raise InplaceError('target_readiness_timeout')

    def verify_peer_safe_image(self, image, revision):
        img = json.loads(self.api141('GET', '/images/' + image + '/json'))
        labels = img['Config'].get('Labels') or {}
        if (img['Id'] != image or labels.get('org.opencontainers.image.revision') != revision
                or labels.get('io.sub2api.peer-safe-startup') != 'true'):
            raise InplaceError('peer_safe_image_required')

    def create_and_start_target(self, window, target_image, target_revision, env_list, appdata, port, directory, receipt_name='target-created.json'):
        self.verify_peer_safe_image(target_image, target_revision)
        name = f'sub2api-gcp-slot-{port}-{window}'
        body = self._contract.api141_create_body(target_image, env_list,
                  {'task53.inplace_window': window}, [appdata + ':/app/data:rw'], APP_NETWORK, 'unless-stopped')
        body['StopTimeout'] = 360
        body['Env'] = [v for v in env_list if not v.startswith('SERVER_SHUTDOWN_TIMEOUT_SECONDS=')] + ['SERVER_SHUTDOWN_TIMEOUT_SECONDS=300']
        body['HostConfig']['PortBindings']['8080/tcp'][0]['HostPort'] = str(port)
        # Standby must never resolve under the active service alias. Network
        # membership stays intact through retirement to preserve outbound SSE.
        body['NetworkingConfig']['EndpointsConfig'][APP_NETWORK]['Aliases'] = [name]
        existing = self._inspect_container(name)
        if existing is None:
            created = json.loads(self.api141('POST', '/containers/create?name=' + name, body))
            cid = created['Id']
            write_atomic(directory, receipt_name, {'id': cid, 'port': port, 'name': name})
            existing = self._inspect_container(cid)
        else:
            cid = existing['id']
            if (existing['image'], existing['revision'], existing['port'], existing['appdata']) != (target_image, target_revision, port, appdata):
                raise InplaceError('standby_container_identity_mismatch')
            write_atomic(directory, receipt_name, {'id': cid, 'port': port, 'name': name, 'resumed': True})
        if DEPENDENCY_NETWORK not in existing['networks']:
            self.api141('POST', '/networks/' + DEPENDENCY_NETWORK + '/connect', {'Container': cid, 'EndpointConfig': {'Aliases': [name]}})
        if not existing['running']:
            self.api141('POST', '/containers/' + cid + '/start')
        return self.wait_ready(cid, target_image, target_revision)

    def make_canonical(self, target_id, window):
        current = self._inspect_container(APP_NAME)
        if current and current['id'] != target_id:
            self.api141('POST', '/containers/' + current['id'] + '/rename?name=' + archive_container_name(window, current['id']))
        target = self._inspect_container(target_id)
        if target['name'] != APP_NAME:
            self.api141('POST', '/containers/' + target_id + '/rename?name=' + APP_NAME)

    def activate_slot(self, expected_id, target_id, window):
        active = self.routed_container()
        if active['id'] != expected_id:
            raise InplaceError('active_slot_changed')
        target = self._inspect_container(target_id)
        self.wait_ready(target_id, target['image'], target['revision'])
        # A retained old slot must not reboot itself beside the new active slot.
        # In particular the bootstrap legacy image has a destructive startup sweep.
        self.api141('POST', '/containers/' + expected_id + '/update', {'RestartPolicy': {'Name': 'no'}})
        image = json.loads(self.api141('GET', '/images/' + target['image'] + '/json'))
        peer_safe = (image['Config'].get('Labels') or {}).get('io.sub2api.peer-safe-startup') == 'true'
        self.api141('POST', '/containers/' + target_id + '/update',
                    {'RestartPolicy': {'Name': 'unless-stopped' if peer_safe else 'no'}})
        self.make_canonical(target_id, window)
        try:
            route = slot_routing.switch(active['port'], target['port'])
        except Exception:
            self.make_canonical(expected_id, window + '-aborted')
            raise
        return dict(route, old_id=expected_id, new_id=target_id)

    def prepare_original(self, go, directory):
        cid, image, revision = go['current_container_id'], go['current_image'], go['current_revision']
        original = self._inspect_container(cid)
        if not original or original['image'] != image or original['revision'] != revision:
            raise InplaceError('original_container_identity_mismatch')
        if original['running']:
            return self.wait_ready(cid, image, revision)
        img = json.loads(self.api141('GET', '/images/' + image + '/json'))
        if (img['Config'].get('Labels') or {}).get('io.sub2api.peer-safe-startup') == 'true':
            self.api141('POST', '/containers/' + cid + '/update', {'RestartPolicy': {'Name': 'unless-stopped'}})
            self.api141('POST', '/containers/' + cid + '/start')
            return self.wait_ready(cid, image, revision)
        # A cold legacy boot erases the serving peer's live Redis slots.
        compatible = go.get('rollback_compatible')
        if not compatible:
            raise InplaceError('cold_rollback_requires_peer_safe_image')
        active = self.routed_container()
        if (active['image'], active['revision']) == (compatible['image'], compatible['revision']):
            return active
        self.admit_core_identity_pair(b'', compatible['image'], compatible['revision'])
        spec = self.capture_current_container_spec(active['id'], go['appdata'])
        return self.create_and_start_target(go['window'] + '-rollback', compatible['image'], compatible['revision'],
                                            spec['env'], go['appdata'], original['port'], directory,
                                            receipt_name='rollback-target-created.json')

    def active_request_count(self, port):
        with urllib.request.urlopen(f'http://127.0.0.1:{port}/health', timeout=5) as response:
            return json.load(response)['in_flight']

    def schedule_retirement(self, cid, workers, directory):
        info = self._inspect_container(cid)
        unit = f'sub2-slot-retire-{cid[:12]}-{time.time_ns()}'
        workers = sorted(set(workers) | slot_routing.retiring_worker_pids())
        subprocess.run(['systemd-run', '--quiet', '--collect', '--unit=' + unit,
                        '--property=TimeoutStartSec=0', '--property=UMask=0077',
                        '/usr/bin/python3', '-B', str(Path(NAILONG_INPLACE_ROOT) / 'inplace_window.py'),
                        'retire', '--container-id', cid, '--started-at', info['started_at'],
                        '--workers', ','.join(map(str, workers)), '--directory', str(directory)],
                       check=True, capture_output=True)
        receipt = {'unit': unit, 'id': cid, 'started_at': info['started_at'], 'retiring_workers': workers}
        write_atomic(directory, 'retirement-scheduled-' + cid[:12] + '.json', receipt)
        return receipt

    def drain_retired_slot(self, cid, retiring_workers, directory, expected_started_at):
        workers = set(retiring_workers)
        status_file = 'retirement-status-' + cid[:12] + '.json'
        write_atomic(directory, status_file, {'stage': 'waiting_for_retiring_workers', 'id': cid})
        while True:
            # Wait OUTSIDE the transition lock so a rollback can reactivate a warm
            # old slot immediately. A recovered/restarted epoch cancels retirement.
            while workers & slot_routing.worker_pids():
                time.sleep(1)
            with release_lock():
                info = self._inspect_container(cid)
                if self.routed_container()['id'] == cid or info['started_at'] != expected_started_at:
                    write_atomic(directory, status_file, {'stage': 'retirement_cancelled_reactivated', 'id': cid})
                    return
                if not info['running']:
                    write_atomic(directory, status_file, {'stage': 'retired', 'id': cid, 'running': False})
                    return
                # Include generations retired by compensating reloads / rollback
                # checks. Being unrouted alone never proves a slot has no SSEs.
                workers = slot_routing.retiring_worker_pids()
                # The commissioning release can have pre-migration Channel 50
                # requests that bypassed Nginx; retain them until completion too.
                if not workers and self.active_request_count(info['port']) == 0:
                    self.api141('POST', '/containers/' + cid + '/update', {'RestartPolicy': {'Name': 'no'}})
                    self.api141('POST', '/containers/' + cid + '/kill?signal=SIGTERM')
                    break
            time.sleep(1)
        deadline = time.monotonic() + 360
        while time.monotonic() < deadline:
            info = self._inspect_container(cid)
            if not info['running']:
                write_atomic(directory, status_file, {'stage': 'retired', 'id': cid, 'running': False})
                return
            time.sleep(1)
        raise InplaceError('retired_slot_stop_timeout_no_kill')

    def discard_unrouted_candidate(self, cid, directory):
        if self.routed_container()['id'] == cid:
            raise InplaceError('refusing_to_discard_active_slot')
        return self.schedule_retirement(cid, [], directory)

    def inspect_backup_roles(self):
        path = Path(BACKUP_SOURCE_JSON)
        if not path.is_file():
            raise InplaceError('source_binding_missing')
        binding = json.loads(path.read_text(encoding='utf8'))
        roles = {r: self._inspect_container(binding[r]['name']) for r in ('app', 'pg', 'redis')}
        if not roles['app'] or not roles['app'].get('appdata'):
            raise InplaceError('appdata_mount_invalid')
        return {'binding': binding, 'roles': roles, 'appdata': roles['app']['appdata']}

    def write_source_binding(self, binding):
        path = Path(BACKUP_SOURCE_JSON)
        tmp = path.with_suffix('.json.new')
        tmp.write_text(json.dumps(binding, indent=2) + '\n', encoding='utf8', newline='\n')
        tmp.chmod(0o600)
        tmp.replace(path)

    def run_source_check(self):
        proc = subprocess.run(
            ['python3', '-B', CORE_BACKUP_PY, 'source-check', '--binding', BACKUP_SOURCE_JSON],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=60,
        )
        if proc.returncode != 0:
            tail = proc.stderr.strip().splitlines()[-1][:180] if proc.stderr.strip() else 'source_check_failed'
            raise InplaceError(tail)
        return json.loads(proc.stdout)

    def ensure_backup_timer_active(self):
        if self.backup_timer_state() != 'active':
            raise InplaceError('backup_timer_inactive')

    def backup_timer_state(self):
        proc = subprocess.run(['systemctl', 'is-active', BACKUP_TIMER], stdout=subprocess.PIPE, text=True, timeout=20)
        text = proc.stdout.strip()
        return text.splitlines()[-1] if text else 'inactive'


class RemoteNailongOps:
    """Controller-side SSH operations: pre-stages host systemd carrier & dual-end core_identity, or delegates to systemd."""

    TARGET_CONFIG = 'C:/Users/xxoy1/.ssh/gcp-nailong-20261107/config-root'
    TARGET_SSH = [
        'ssh', '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes',
        '-F', TARGET_CONFIG, '-o', 'HostName=10.77.48.3', '-o', 'HostKeyAlias=136.66.61.218',
        '-o', 'ProxyCommand=C:/Windows/System32/OpenSSH/ssh.exe -T -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=12 -W %h:%p lag',
        'gcp-nailong-root',
    ]
    LAG_SSH = ['ssh', '-T', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', 'lag']

    def _call(self, host_cmd, remote_cmd, data=None, timeout=120):
        proc = subprocess.run([*host_cmd, remote_cmd], input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        if proc.returncode != 0:
            err = proc.stderr.decode(errors='replace').strip()
            tail = err.splitlines()[-1][:180] if err else 'ssh_command_failed'
            raise InplaceError(tail)
        return proc.stdout

    def _json_py(self, host_cmd, script, timeout=120):
        out = self._call(host_cmd, 'python3 -B -', data=script.encode('utf8'), timeout=timeout)
        return json.loads(out.decode('utf8'))

    def observe_live(self, target_revision):
        script = r'''
import json, sys
sys.path.insert(0, "/opt/sub2api-nailong/inplace")
import inplace_window as iw
ops = iw.LocalNailongOps()
print(json.dumps(ops.observe_live(%r)))
''' % (target_revision,)
        data = self._json_py(self.TARGET_SSH, script, timeout=60)
        lag_sha_script = (
            'import hashlib, json; from pathlib import Path; p=Path("/opt/sub2-core-backup/core-backup/core_identity.py"); '
            'print(json.dumps(hashlib.sha256(p.read_bytes()).hexdigest() if p.is_file() else None))'
        )
        data['lag_identity_sha'] = self._json_py(self.LAG_SSH, lag_sha_script, timeout=30)
        return data

    def admit_core_identity_pair(self, candidate_bytes, target_image, target_revision):
        # Preserve the current policy on each endpoint, including releases newer
        # than the historical candidate file bundled with this carrier.
        script = r'''
import json
from pathlib import Path
path = Path('/opt/sub2-core-backup/core-backup/core_identity.py')
source = path.read_text()
ns = {}
exec(compile(source, str(path), 'exec'), ns)
before = ns['ADMITTED_PAIRS']
pair = (%r, %r)
if pair not in before:
    source += '\n# Blue-green release; preserve every prior admitted image/revision pair.\nADMITTED_PAIRS = ADMITTED_PAIRS | frozenset((' + repr(pair) + ',))\n'
    check = {}
    exec(compile(source, str(path), 'exec'), check)
    assert check['ADMITTED_PAIRS'] == before | {pair}
    tmp = path.with_suffix('.py.new')
    tmp.write_text(source, encoding='utf8')
    tmp.chmod(0o644)
    tmp.replace(path)
print(json.dumps({'admitted': True, 'pair': pair, 'previous_pairs_preserved': len(before)}))
''' % (target_image, target_revision)
        return {'nailong': self._json_py(self.TARGET_SSH, script, timeout=30),
                'lag': self._json_py(self.LAG_SSH, script, timeout=30)}

    def stage_host_carrier(self, template):
        admitted = self.admit_core_identity_pair(b'', template['target_image'], template['target_revision'])
        compatible = template.get('rollback_compatible')
        if compatible:
            admitted['rollback_compatible'] = self.admit_core_identity_pair(b'', compatible['image'], compatible['revision'])
        files = {name: base64.b64encode((HERE / name).read_bytes()).decode('ascii')
                 for name in ('inplace_window.py', 'slot_routing.py', 'core_identity_candidate.py', 'go.template.json')}
        script = r'''
import base64, json
from pathlib import Path
root = Path('/opt/sub2api-nailong/inplace')
files = %r
for name, payload in files.items():
    target = root / name
    if target.exists():
        backup = root / (name + '.before-bluegreen-20261115')
        if not backup.exists():
            backup.write_bytes(target.read_bytes())
            backup.chmod(0o600)
    tmp = root / (name + '.new')
    tmp.write_bytes(base64.b64decode(payload))
    tmp.chmod(0o700 if name.endswith('.py') else 0o600)
    tmp.replace(target)
unit = Path('/etc/systemd/system/sub2-nailong-inplace@.service').read_text()
assert 'TimeoutStartSec=0' in unit
print(json.dumps({'stage': 'bluegreen_carrier_staged', 'files': sorted(files)}))
''' % files
        staged = self._json_py(self.TARGET_SSH, script, timeout=60)
        return dict(staged, admitted=admitted)

    def launch_via_systemd(self, go, action, local_window_dir):
        """Upload signed GO to Nailong and start `sub2-nailong-inplace@<window>.service`."""
        validate_go(go, action)
        window = go['window']
        local_window_dir = Path(local_window_dir)
        local_window_dir.mkdir(parents=True, exist_ok=True)

        execution_id = str(time.time_ns())
        go = dict(go, execution_id=execution_id)
        remote_dir = f'{NAILONG_INPLACE_ROOT}/windows/{window}'
        script = r'''
import fcntl, json, subprocess
from pathlib import Path
go = %r
unit = 'sub2-nailong-inplace@' + go['window'] + '.service'
with open('/opt/sub2api-nailong/inplace/launch.lock', 'a') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    state = dict(line.split('=', 1) for line in subprocess.check_output(['systemctl','show',unit,'-p','ActiveState','-p','Job'], text=True).splitlines())
    if state['ActiveState'] not in ('inactive','failed') or state.get('Job') not in ('','0',None):
        raise SystemExit('window_already_running')
    root = Path(%r)
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if (root / 'status.json').exists():
        (root / 'status.json').replace(root / ('status-before-' + go['execution_id'] + '.json'))
    path = root / 'go.json'
    path.write_text(json.dumps(go, indent=2) + '\n')
    path.chmod(0o600)
    subprocess.run(['systemctl','start','--no-block',unit],check=True)
print(json.dumps({'execution_id':go['execution_id']}))
''' % (go, remote_dir)
        self._json_py(self.TARGET_SSH, script, timeout=30)
        write_atomic(local_window_dir, 'systemd-launched.json', {
            'unit': f'sub2-nailong-inplace@{window}.service',
            'remote_window_dir': remote_dir,
            'action': action,
        })

        deadline = time.monotonic() + 1800
        while time.monotonic() < deadline:
            poll_script = r'''
import json, subprocess
from pathlib import Path
wdir = Path(%r)
status = json.loads((wdir / "status.json").read_text(encoding="utf8")) if (wdir / "status.json").is_file() else None
active = subprocess.run(["systemctl", "is-active", %r], stdout=subprocess.PIPE, text=True).stdout.strip()
print(json.dumps({"status": status, "unit_state": active}))
''' % (remote_dir, f'sub2-nailong-inplace@{window}.service')
            polled = self._json_py(self.TARGET_SSH, poll_script, timeout=30)
            status = polled.get('status')
            if status:
                write_atomic(local_window_dir, 'status.json', status)
                terminal = 'forward_terminal' if action == 'forward' else 'recovered_to_previous_image'
                if status.get('stage') == terminal and status.get('execution_id') == execution_id:
                    return status
            if polled.get('unit_state') in ('failed', 'inactive'):
                raise InplaceError((status or {}).get('error') or (status or {}).get('reason') or (status or {}).get('stage') or 'systemd_unit_failed')
            time.sleep(2)
        raise InplaceError('systemd_window_poll_timeout')


def host_run(go_path):
    """Entry point invoked on Nailong by `sub2-nailong-inplace@<window>.service`."""
    go_path = Path(go_path)
    go = json.loads(go_path.read_text(encoding='utf8'))
    action = go.get('action')
    directory = go_path.parent
    ops = LocalNailongOps()
    with release_lock():
        if action == 'forward':
            return forward(go, ops, template_name=go_path.name, directory=directory)
        if action == 'recover':
            return recover(go, ops, directory=directory)
    raise InplaceError('go_action_mismatch')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('ready', 'stage-carrier', 'forward', 'recover', 'host-run', 'retire'))
    parser.add_argument('--go', type=Path, help='Authorized GO file for forward/recover/host-run')
    parser.add_argument('--container-id')
    parser.add_argument('--started-at')
    parser.add_argument('--workers', default='')
    parser.add_argument('--directory', type=Path)
    args = parser.parse_args()

    if args.action == 'retire':
        LocalNailongOps().drain_retired_slot(args.container_id, [int(p) for p in args.workers.split(',') if p], args.directory, args.started_at)
        return

    if args.action == 'host-run':
        if args.go is None:
            raise InplaceError('go_file_required')
        result = host_run(args.go)
        print(json.dumps(result, indent=2))
        return

    template = json.loads((args.go or HERE / 'go.template.json').read_text(encoding='utf8'))
    ops = RemoteNailongOps()
    if args.action == 'ready':
        result = ready(ops, template)
        write_atomic(HERE, 'inplace-ready.json', result)
        print(json.dumps(result, indent=2))
        return
    if args.action == 'stage-carrier':
        result = ops.stage_host_carrier(template)
        write_atomic(HERE, 'carrier-stage-receipt.json', result)
        print(json.dumps(result, indent=2))
        return

    if args.go is None:
        raise InplaceError('go_file_required')
    go = json.loads(args.go.read_text(encoding='utf8'))
    validate_go(go, args.action, filename=args.go.name)
    local_dir = HERE / 'windows' / go['window']
    result = ops.launch_via_systemd(go, args.action, local_dir)
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print('inplace_failed=' + error_label(exc), file=sys.stderr)
        raise SystemExit(1) from None
