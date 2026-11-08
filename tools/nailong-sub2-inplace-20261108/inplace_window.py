#!/usr/bin/env python3
"""Same-host in-place Sub2 release & recovery carrier on Nailong (136.66.61.218 / 10.77.48.3).

Invariants enforced:
- Single Sub2 container (`sub2api-gcp`) stop/start only; never touches old GCP (`136.118.187.9`),
  Worker, Axon (`task51-gcp-nailong-usw1`), CPR, Xray, or DNS.
- Preserves live PostgreSQL (`sub2api-nailong-postgres`), Redis (`sub2api-nailong-redis`),
  appdata (`/opt/sub2api-nailong/runs/nailong-fc899-20261108t0659z/appdata`), and env/credentials
  (including `JWT_SECRET` and `TOTP_ENCRYPTION_KEY`) in place — zero snapshot/dump re-import.
- Preserves `antigravity_thought_policy` (`suppress_thought_only_stop=true`,
  `continue_thought_only=false`, `continue_zero_output=false`).
- Dynamically rebinds `/etc/sub2-core-backup/source.json` to the actual active container's
  (`id`, `started_at`) after container swap (and after same-host recovery to the previous image),
  verifying `core_backup.py source-check --binding /etc/sub2-core-backup/source.json` and
  `sub2-core-backup.timer == active`.
"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
SCHEMA = 'nailong-sub2-inplace-go-1'
TARGET_HOST = '136.66.61.218'
APP_NAME = 'sub2api-gcp'
ALIAS = 'sub2api-gcp'
LOOPBACK = '127.0.0.1:17861'
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
CURRENT_CORE_IDENTITY_SHA = '7b7449ea4309216cde4a72432fb867073989635cfea207e8d7ab517e0800991d'
CANDIDATE_CORE_IDENTITY = HERE / 'core_identity_candidate.py'
EXPECTED_THOUGHT_POLICY = {
    'suppress_thought_only_stop': True,
    'continue_thought_only': False,
    'continue_zero_output': False,
}
WINDOW_NAME_RE = re.compile(r'^[a-z0-9][a-z0-9._-]{5,63}$')
HEX64_RE = re.compile(r'^[0-9a-f]{64}$')
HEX40_RE = re.compile(r'^[0-9a-f]{40}$')


class InplaceError(Exception):
    pass


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
    if go.get('alias') != ALIAS or go.get('loopback') != LOOPBACK:
        raise InplaceError('endpoint_contract_mismatch')
    if go.get('app_network') != APP_NETWORK or go.get('dependency_network') != DEPENDENCY_NETWORK:
        raise InplaceError('network_contract_mismatch')
    if (go.get('single_app_only') is not True or go.get('stop_only_sub2') is not True
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


def ready(ops, template):
    live = ops.observe_live(template['target_revision'])
    deps_ok = True
    try:
        verify_dependencies_and_policy(live, template)
        verify_live_current_app(live, template)
    except InplaceError:
        deps_ok = False
    source_check = live.get('source_check') or {}
    backup_ok = (live.get('backup_timer') == 'active'
                 and source_check.get('stage') == 'core_backup_source_bound'
                 and ((source_check.get('source') or {}).get('app') or {}).get('id') == template['current_container_id'])
    candidate_bytes = CANDIDATE_CORE_IDENTITY.read_bytes()
    candidate_sha = hashlib.sha256(candidate_bytes).hexdigest()
    identity_admitted_for_target = (
        live.get('nailong_identity_sha') == candidate_sha
        and live.get('lag_identity_sha') == candidate_sha
    )
    return {
        'stage': 'inplace_stage_verified_awaiting_parent_go' if (deps_ok and backup_ok) else 'inplace_not_ready',
        'go_executed': False,
        'app_restarted': False,
        'dependencies_and_current_app_match': deps_ok,
        'current_backup_bound_and_active': backup_ok,
        'target_image_staged_on_nailong': live.get('loaded_target'),
        'core_identity_status': {
            'nailong_sha256': live.get('nailong_identity_sha'),
            'lag_sha256': live.get('lag_identity_sha'),
            'candidate_sha256': candidate_sha,
            'target_pair_already_admitted': identity_admitted_for_target,
        },
        'live': live,
    }


def recover(go, ops, directory=None):
    validate_go(go, go.get('action', 'recover'))
    directory = Path(directory) if directory is not None else HERE / 'windows' / go['window']
    directory.mkdir(parents=True, exist_ok=True)
    live = ops.observe_live(go['target_revision'])
    verify_dependencies_and_policy(live, go)

    retained_name = archive_container_name(go['window'], go['current_container_id'])
    state = ops.observe_containers(go['current_container_id'], retained_name)
    active = state.get('active_sub2')
    retained = state.get('retained')

    record_event(directory, 'recover_started', {
        'window': go['window'],
        'current_container_id': go['current_container_id'],
        'active_sub2_id': active.get('id') if active else None,
    })

    # If a new container was created under `sub2api-gcp` (and is not the original container), stop & remove/archive it.
    if active and active['id'] != go['current_container_id']:
        assert_stoppable(APP_NAME)
        stopped_new = ops.stop_and_remove_new_container(active['id'], f"sub2api-gcp-failed-{go['window']}-{active['id'][:12]}")
        write_atomic(directory, 'new-container-removed.json', stopped_new)

    # Restore original container name to `sub2api-gcp` if it was renamed to retained_name, then start it.
    restored_app = ops.restore_and_start_original_container(go['current_container_id'], retained_name)
    write_atomic(directory, 'original-container-restored.json', restored_app)

    # Re-verify dependencies & thought policy after container recovery.
    after_live = ops.observe_live(go['target_revision'])
    verify_dependencies_and_policy(after_live, go)

    # Rebind `/etc/sub2-core-backup/source.json` to the recovered original container's new StartedAt!
    backup_receipt = rebind_daily_backup(
        ops,
        directory,
        expected_app_id=restored_app['id'],
        expected_image=go['current_image'],
        expected_revision=go['current_revision'],
        expected_appdata=go['appdata'],
        pg_pin=go['pg'],
        redis_pin=go['redis'],
    )
    body = record_event(directory, 'recovered_to_previous_image', {
        'window': go['window'],
        'app': restored_app,
        'pg_preserved': go['pg']['id'],
        'redis_preserved': go['redis']['id'],
        'appdata_preserved': go['appdata'],
        'thought_policy': after_live['thought_policy'],
        'backup': backup_receipt,
        'old_gcp_touched': False,
        'dns_applied_by_runner': False,
        'worker_applied_by_runner': False,
    })
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

    if go.get('admit_core_identity_pair') is True:
        admit_receipt = ops.admit_core_identity_pair(
            CANDIDATE_CORE_IDENTITY.read_bytes(),
            go['target_image'],
            go['target_revision'],
        )
        claim(directory, 'core-identity-admitted.json', admit_receipt)

    runtime_spec = ops.capture_current_container_spec(go['current_container_id'], go['appdata'])
    claim(directory, 'forward-started.json', {
        'window': go['window'],
        'previous_container_id': go['current_container_id'],
        'previous_image': go['current_image'],
        'target_image': go['target_image'],
        'target_revision': go['target_revision'],
        'appdata': go['appdata'],
    })

    retained_name = archive_container_name(go['window'], go['current_container_id'])
    try:
        assert_stoppable(APP_NAME)
        claim(directory, 'stop-intent.json', {'container_id': go['current_container_id'], 'retained_name': retained_name})
        stopped = ops.stop_and_rename_current(go['current_container_id'], retained_name)
        claim(directory, 'current-stopped.json', stopped)
        record_event(directory, 'current_stopped', stopped)

        claim(directory, 'start-intent.json', {'target_image': go['target_image'], 'target_revision': go['target_revision']})
        started = ops.create_and_start_target(
            window=go['window'],
            target_image=go['target_image'],
            target_revision=go['target_revision'],
            env_list=runtime_spec['env'],
            appdata=go['appdata'],
        )
        claim(directory, 'target-healthy.json', started)
        record_event(directory, 'target_healthy', started)

        after_live = ops.observe_live(go['target_revision'])
        verify_dependencies_and_policy(after_live, go)

        backup_receipt = rebind_daily_backup(
            ops,
            directory,
            expected_app_id=started['id'],
            expected_image=go['target_image'],
            expected_revision=go['target_revision'],
            expected_appdata=go['appdata'],
            pg_pin=go['pg'],
            redis_pin=go['redis'],
        )
        body = record_event(directory, 'forward_terminal', {
            'window': go['window'],
            'app': started,
            'retained_previous_container': {'id': go['current_container_id'], 'name': retained_name},
            'pg_preserved': go['pg']['id'],
            'redis_preserved': go['redis']['id'],
            'axon_preserved': go['axon_nailong']['id'],
            'appdata_preserved': go['appdata'],
            'thought_policy': after_live['thought_policy'],
            'backup': backup_receipt,
            'old_gcp_touched': False,
            'dns_applied_by_runner': False,
            'worker_applied_by_runner': False,
        })
        claim(directory, 'forward-terminal.json', body)
        return body
    except Exception as error:
        label = str(error) if isinstance(error, InplaceError) else type(error).__name__
        write_atomic(directory, 'forward-error.json', {'error': label})
        record_event(directory, 'forward_error_triggering_recover', {'error': label})
        if (directory / 'current-stopped.json').exists():
            recover_go = dict(go, action='recover')
            recover(recover_go, ops, directory=directory)
        raise


class RemoteNailongOps:
    """Executes bounded SSH calls to Nailong (and lag only for core_identity admission when requested)."""

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
import hashlib, json, subprocess
from pathlib import Path

def inspect(name):
    p = subprocess.run(["docker", "inspect", name], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    if p.returncode != 0:
        return None
    d = json.loads(p.stdout)[0]
    mounts = [m["Source"] for m in (d.get("Mounts") or []) if m.get("Destination") == "/app/data"]
    return {
        "id": d["Id"],
        "name": d["Name"].lstrip("/"),
        "image": d["Image"],
        "revision": ((d["Config"].get("Labels") or {}).get("org.opencontainers.image.revision") or ""),
        "running": bool(d["State"].get("Running")),
        "health": (d["State"].get("Health") or {}).get("Status"),
        "started_at": d["State"].get("StartedAt"),
        "restart_count": d.get("RestartCount", 0),
        "appdata": mounts[0] if len(mounts) == 1 else None,
    }

want_rev = %r
ids = subprocess.check_output(["docker", "image", "ls", "-q", "--no-trunc"], text=True).split()
loaded = None
seen = set()
for img_id in ids:
    if img_id in seen:
        continue
    seen.add(img_id)
    p = subprocess.run(["docker", "image", "inspect", img_id], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    if p.returncode != 0:
        continue
    info = json.loads(p.stdout)[0]
    rev = ((info["Config"].get("Labels") or {}).get("org.opencontainers.image.revision") or "")
    if rev == want_rev:
        loaded = {"id": info["Id"], "revision": rev, "repo_tags": info.get("RepoTags") or []}
        break

sql = "SELECT value FROM settings WHERE key = 'antigravity_thought_policy';"
p_sql = subprocess.run(["docker", "exec", "-i", "sub2api-nailong-postgres", "psql", "-X", "-qAt", "-U", "sub2api", "-d", "sub2api"],
                       input=sql.encode(), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
thought_policy = json.loads(p_sql.stdout.decode().strip()) if p_sql.returncode == 0 and p_sql.stdout.strip() else None

timer = subprocess.run(["systemctl", "is-active", "sub2-core-backup.timer"], stdout=subprocess.PIPE, text=True).stdout.strip()
sc = subprocess.run(["python3", "-B", "/opt/sub2-core-backup/core-backup/core_backup.py", "source-check", "--binding", "/etc/sub2-core-backup/source.json"],
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
sc_out = json.loads(sc.stdout) if sc.returncode == 0 and sc.stdout.strip() else {"error": sc.stderr.strip()}
id_path = Path("/opt/sub2-core-backup/core-backup/core_identity.py")
id_sha = hashlib.sha256(id_path.read_bytes()).hexdigest() if id_path.is_file() else None

print(json.dumps({
    "app": inspect("sub2api-gcp"),
    "pg": inspect("sub2api-nailong-postgres"),
    "redis": inspect("sub2api-nailong-redis"),
    "axon_nailong": inspect("task51-gcp-nailong-usw1"),
    "loaded_target": loaded,
    "thought_policy": thought_policy,
    "backup_timer": timer,
    "source_check": sc_out,
    "nailong_identity_sha": id_sha,
}))
''' % (target_revision,)
        data = self._json_py(self.TARGET_SSH, script, timeout=60)
        lag_sha_script = (
            'import hashlib, json; from pathlib import Path; p=Path("/opt/sub2-core-backup/core-backup/core_identity.py"); '
            'print(json.dumps(hashlib.sha256(p.read_bytes()).hexdigest() if p.is_file() else None))'
        )
        data['lag_identity_sha'] = self._json_py(self.LAG_SSH, lag_sha_script, timeout=30)
        return data

    def admit_core_identity_pair(self, candidate_bytes, target_image, target_revision):
        digest = hashlib.sha256(candidate_bytes).hexdigest()
        b64 = base64.b64encode(candidate_bytes).decode('ascii')
        script = r'''
import base64, hashlib, json, os
from pathlib import Path

payload = base64.b64decode(%r)
want_sha = %r
old_sha = %r
want_pair = (%r, %r)
if hashlib.sha256(payload).hexdigest() != want_sha:
    raise SystemExit("candidate_sha_mismatch")

path = Path("/opt/sub2-core-backup/core-backup/core_identity.py")
current_sha = hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None
if current_sha not in (old_sha, want_sha):
    raise SystemExit("unexpected_existing_identity_sha")

if current_sha != want_sha:
    tmp = path.with_suffix(".py.tmp")
    tmp.write_bytes(payload)
    os.chmod(tmp, 0o644)
    tmp.replace(path)
    os.chmod(path, 0o644)

ns = {}
exec(compile(path.read_text(encoding="utf8"), str(path), "exec"), ns)
if not ns["admits"](*want_pair) or not ns["admits"](*ns["FC899_NAILONG_PAIR"]):
    raise SystemExit("admits_check_failed")
print(json.dumps({"sha256": hashlib.sha256(path.read_bytes()).hexdigest()}))
''' % (b64, digest, CURRENT_CORE_IDENTITY_SHA, target_image, target_revision)
        nailong_res = self._json_py(self.TARGET_SSH, script, timeout=30)
        lag_res = self._json_py(self.LAG_SSH, script, timeout=30)
        return {'nailong_sha256': nailong_res['sha256'], 'lag_sha256': lag_res['sha256'], 'pair': [target_image, target_revision]}

    def capture_current_container_spec(self, expected_id, expected_appdata):
        script = r'''
import json, subprocess
d = json.loads(subprocess.check_output(["docker", "inspect", "sub2api-gcp"], text=True))[0]
if d["Id"] != %r:
    raise SystemExit("current_container_id_mismatch")
mounts = [m["Source"] for m in (d.get("Mounts") or []) if m.get("Destination") == "/app/data"]
if len(mounts) != 1 or mounts[0] != %r:
    raise SystemExit("current_appdata_mismatch")
env = list(d["Config"].get("Env") or [])
env_map = dict(item.split("=", 1) for item in env if "=" in item)
if not env_map.get("JWT_SECRET") or not env_map.get("TOTP_ENCRYPTION_KEY"):
    raise SystemExit("jwt_or_totp_missing")
print(json.dumps({"id": d["Id"], "env": env, "appdata": mounts[0]}))
''' % (expected_id, expected_appdata)
        return self._json_py(self.TARGET_SSH, script, timeout=30)

    def stop_and_rename_current(self, expected_id, retained_name):
        script = r'''
import json, time
import sys
sys.path.insert(0, "/opt/sub2api-nailong/tools")
import migration

app = migration.inspect("sub2api-gcp")
if app["Id"] != %r:
    raise SystemExit("stop_identity_mismatch")
migration.api("POST", "/containers/" + app["Id"] + "/update", {"RestartPolicy": {"Name": "no"}})
if app["State"]["Running"]:
    migration.api("POST", "/containers/" + app["Id"] + "/kill?signal=SIGTERM")
deadline = time.monotonic() + 180
while time.monotonic() < deadline:
    cur = migration.inspect(app["Id"])
    if not cur["State"]["Running"]:
        migration.api("POST", "/containers/" + app["Id"] + "/rename?name=" + %r)
        print(json.dumps({"id": app["Id"], "retained_name": %r, "exit_code": cur["State"]["ExitCode"]}))
        raise SystemExit(0)
    time.sleep(2)
raise SystemExit("graceful_stop_timeout")
''' % (expected_id, retained_name, retained_name)
        return self._json_py(self.TARGET_SSH, script, timeout=240)

    def create_and_start_target(self, window, target_image, target_revision, env_list, appdata):
        script = r'''
import json, time, urllib.request
import sys
sys.path.insert(0, "/opt/sub2api-nailong/tools")
import contract, migration

image = %r
want_rev = %r
window = %r
env_list = %r
appdata = %r

img_info = json.loads(migration.api("GET", "/images/" + image + "/json"))
rev = ((img_info.get("Config") or {}).get("Labels") or {}).get("org.opencontainers.image.revision")
if img_info.get("Id") != image or rev != want_rev:
    raise SystemExit("target_image_verify_failed")

mount = appdata + ":/app/data:rw"
body = contract.api141_create_body(
    image,
    env_list,
    {"task53.inplace_window": window},
    [mount],
    "sub2api-nailong-app",
    "unless-stopped",
)
created = json.loads(migration.api("POST", "/containers/create?name=sub2api-gcp", body))
cid = created["Id"]
migration.api("POST", "/networks/sub2api-nailong/connect", {
    "Container": cid,
    "EndpointConfig": {"Aliases": ["sub2api-gcp"]},
})
migration.api("POST", "/containers/" + cid + "/start")

deadline = time.monotonic() + 180
while time.monotonic() < deadline:
    cur = migration.inspect(cid)
    if not cur["State"]["Running"] or cur.get("RestartCount", 0) > 0:
        raise SystemExit("target_exited_or_restarted")
    if (cur["State"].get("Health") or {}).get("Status") == "healthy":
        status = urllib.request.urlopen("http://127.0.0.1:17861/health", timeout=5).status
        if status == 200:
            print(json.dumps({
                "id": cur["Id"],
                "name": "sub2api-gcp",
                "image": cur["Image"],
                "revision": rev,
                "started_at": cur["State"]["StartedAt"],
                "health": "healthy",
                "loopback_http": status,
            }))
            raise SystemExit(0)
    time.sleep(2)
raise SystemExit("target_health_timeout")
''' % (target_image, target_revision, window, env_list, appdata)
        return self._json_py(self.TARGET_SSH, script, timeout=240)

    def observe_containers(self, original_id, retained_name):
        script = r'''
import json, subprocess

def inspect(ref):
    p = subprocess.run(["docker", "inspect", ref], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
    if p.returncode != 0:
        return None
    d = json.loads(p.stdout)[0]
    return {
        "id": d["Id"],
        "name": d["Name"].lstrip("/"),
        "image": d["Image"],
        "running": bool(d["State"].get("Running")),
        "health": (d["State"].get("Health") or {}).get("Status"),
        "started_at": d["State"].get("StartedAt"),
    }

print(json.dumps({
    "active_sub2": inspect("sub2api-gcp"),
    "original_by_id": inspect(%r),
    "retained": inspect(%r),
}))
''' % (original_id, retained_name)
        return self._json_py(self.TARGET_SSH, script, timeout=40)

    def stop_and_remove_new_container(self, new_container_id, failed_archive_name):
        script = r'''
import json, time
import sys
sys.path.insert(0, "/opt/sub2api-nailong/tools")
import migration

cid = %r
archive_name = %r
cur = migration.inspect(cid)
migration.api("POST", "/containers/" + cid + "/update", {"RestartPolicy": {"Name": "no"}})
if cur["State"]["Running"]:
    migration.api("POST", "/containers/" + cid + "/kill?signal=SIGTERM")
deadline = time.monotonic() + 120
while time.monotonic() < deadline:
    info = migration.inspect(cid)
    if not info["State"]["Running"]:
        break
    time.sleep(2)
if info["Name"].lstrip("/") == "sub2api-gcp":
    migration.api("POST", "/containers/" + cid + "/rename?name=" + archive_name)
print(json.dumps({"stopped_id": cid, "archived_as": archive_name}))
''' % (new_container_id, failed_archive_name)
        return self._json_py(self.TARGET_SSH, script, timeout=180)

    def restore_and_start_original_container(self, original_id, retained_name):
        script = r'''
import json, time, urllib.request
import sys
sys.path.insert(0, "/opt/sub2api-nailong/tools")
import migration

oid = %r
info = migration.inspect(oid)
if info["Name"].lstrip("/") != "sub2api-gcp":
    migration.api("POST", "/containers/" + oid + "/rename?name=sub2api-gcp")
migration.api("POST", "/containers/" + oid + "/update", {"RestartPolicy": {"Name": "unless-stopped"}})
if not info["State"]["Running"]:
    migration.api("POST", "/containers/" + oid + "/start")

deadline = time.monotonic() + 180
while time.monotonic() < deadline:
    cur = migration.inspect(oid)
    if not cur["State"]["Running"]:
        raise SystemExit("original_container_not_running")
    if (cur["State"].get("Health") or {}).get("Status") == "healthy":
        status = urllib.request.urlopen("http://127.0.0.1:17861/health", timeout=5).status
        if status == 200:
            labels = cur["Config"].get("Labels") or {}
            print(json.dumps({
                "id": cur["Id"],
                "name": cur["Name"].lstrip("/"),
                "image": cur["Image"],
                "revision": labels.get("org.opencontainers.image.revision") or "",
                "started_at": cur["State"]["StartedAt"],
                "health": "healthy",
                "loopback_http": status,
            }))
            raise SystemExit(0)
    time.sleep(2)
raise SystemExit("original_container_health_timeout")
''' % (original_id,)
        return self._json_py(self.TARGET_SSH, script, timeout=240)

    def inspect_backup_roles(self):
        script = r'''
import json, subprocess
from pathlib import Path

path = Path("/etc/sub2-core-backup/source.json")
if not path.is_file():
    raise SystemExit("source_binding_missing")
binding = json.loads(path.read_text(encoding="utf8"))

def role_info(name):
    d = json.loads(subprocess.check_output(["docker", "inspect", name], text=True))[0]
    return {
        "name": name,
        "id": d["Id"],
        "started_at": d["State"]["StartedAt"],
        "running": bool(d["State"]["Running"]),
        "health": (d["State"].get("Health") or {}).get("Status"),
    }

roles = {r: role_info(binding[r]["name"]) for r in ("app", "pg", "redis")}
app_d = json.loads(subprocess.check_output(["docker", "inspect", roles["app"]["id"]], text=True))[0]
mounts = [m["Source"] for m in (app_d.get("Mounts") or []) if m.get("Destination") == "/app/data"]
if len(mounts) != 1:
    raise SystemExit("appdata_mount_invalid")
print(json.dumps({"binding": binding, "roles": roles, "appdata": mounts[0]}))
'''
        return self._json_py(self.TARGET_SSH, script, timeout=40)

    def write_source_binding(self, binding):
        payload = (json.dumps(binding, indent=2) + '\n').encode('utf8')
        cmd = (
            f'umask 077; tee {BACKUP_SOURCE_JSON}.new >/dev/null && '
            f'chmod 600 {BACKUP_SOURCE_JSON}.new && mv {BACKUP_SOURCE_JSON}.new {BACKUP_SOURCE_JSON}'
        )
        self._call(self.TARGET_SSH, cmd, data=payload, timeout=30)

    def run_source_check(self):
        out = self._call(
            self.TARGET_SSH,
            f'python3 -B {CORE_BACKUP_PY} source-check --binding {BACKUP_SOURCE_JSON}',
            timeout=60,
        )
        return json.loads(out.decode('utf8'))

    def ensure_backup_timer_active(self):
        self._call(self.TARGET_SSH, f'systemctl enable --now {BACKUP_TIMER}', timeout=30)

    def backup_timer_state(self):
        out = self._call(self.TARGET_SSH, f'systemctl is-active {BACKUP_TIMER} || true', timeout=20)
        text = out.decode('utf8').strip()
        return text.splitlines()[-1] if text else 'inactive'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('ready', 'forward', 'recover'))
    parser.add_argument('--go', type=Path, help='Signed parent GO file for forward/recover')
    args = parser.parse_args()
    ops = RemoteNailongOps()
    if args.action == 'ready':
        template = json.loads((HERE / 'go.template.json').read_text(encoding='utf8'))
        result = ready(ops, template)
        write_atomic(HERE, 'inplace-ready.json', result)
        print(json.dumps(result, indent=2))
        return
    if args.go is None:
        raise InplaceError('go_file_required')
    go = json.loads(args.go.read_text(encoding='utf8'))
    if args.action == 'forward':
        result = forward(go, ops, template_name=args.go.name)
    else:
        result = recover(go, ops)
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        label = str(exc) if isinstance(exc, InplaceError) else type(exc).__name__
        print('inplace_failed=' + label, file=sys.stderr)
        raise SystemExit(1) from None
