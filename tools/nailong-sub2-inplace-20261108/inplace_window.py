#!/usr/bin/env python3
"""Same-host in-place Sub2 release & recovery carrier on Nailong (136.66.61.218 / 10.77.48.3).

Supports two execution modes using the exact same state machine:
1. Host-resident autonomous mode on Nailong (`LocalNailongOps`, invoked by
   `sub2-nailong-inplace@<window>.service` so a controller / SSH / Kiki disconnect
   never truncates an in-flight forward or recovery).
2. Controller launcher / preflight mode on Windows (`RemoteNailongOps`, which stages
   the runner + systemd unit + dual-end `core_identity.py` admission before any stop,
   triggers the Nailong systemd unit for `forward`/`recover`, and tails status).

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
import urllib.request

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
PARENT_CORE_IDENTITY_SHA = '7b7449ea4309216cde4a72432fb867073989635cfea207e8d7ab517e0800991d'
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
TimeoutStartSec=600
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


def recover(go, ops, directory=None):
    """Converge Nailong back to the previous container image on the live PG/Redis/appdata.

    Never assumes a missing `current-stopped.json` means the original container wasn't stopped;
    always inspects actual container states first and refuses to start the original container
    unless any new candidate container is verified Exited (`running == False`).
    """
    validate_go(go, go.get('action', 'recover'))
    directory = Path(directory) if directory is not None else HERE / 'windows' / go['window']
    directory.mkdir(parents=True, exist_ok=True)
    live = ops.observe_live(go['target_revision'])
    verify_dependencies_and_policy(live, go)

    retained_name = archive_container_name(go['window'], go['current_container_id'])
    try:
        state = ops.observe_containers(go['current_container_id'], retained_name)
    except Exception as obs_err:
        label = error_label(obs_err)
        record_event(directory, 'recovery_requires_operator', {
            'reason': 'container_state_unknown',
            'error': label,
        })
        raise InplaceError('container_state_unknown') from None

    if not isinstance(state, dict) or state.get('original_by_id') is None:
        record_event(directory, 'recovery_requires_operator', {
            'reason': 'original_container_missing_or_unobservable',
        })
        raise InplaceError('original_container_missing_or_unobservable')

    active = state.get('active_sub2')
    original = state.get('original_by_id')

    record_event(directory, 'recover_started', {
        'window': go['window'],
        'current_container_id': go['current_container_id'],
        'active_sub2_id': active.get('id') if active else None,
        'original_running': original.get('running'),
        'original_name': original.get('name'),
    })

    # If a new container occupies `sub2api-gcp`, it MUST be stopped and verified Exited (`running == False`)
    # before we ever rename or start the original container (prevents dual-active Sub2).
    if active and active['id'] != go['current_container_id']:
        assert_stoppable(APP_NAME)
        failed_archive = f"sub2api-gcp-failed-{go['window']}-{active['id'][:12]}"
        try:
            stopped_new = ops.stop_and_remove_new_container(active['id'], failed_archive)
        except Exception as stop_err:
            label = error_label(stop_err)
            record_event(directory, 'recovery_requires_operator', {
                'reason': 'new_container_still_running_or_stop_unknown',
                'new_container_id': active['id'],
                'error': label,
                'original_started': False,
            })
            raise InplaceError('new_container_still_running_or_stop_unknown') from None

        if stopped_new.get('running') is not False:
            record_event(directory, 'recovery_requires_operator', {
                'reason': 'new_container_not_exited_refusing_dual_active',
                'new_container_id': active['id'],
                'original_started': False,
            })
            raise InplaceError('new_container_not_exited_refusing_dual_active')
        write_atomic(directory, 'new-container-removed.json', stopped_new)

    restored_app = ops.restore_and_start_original_container(go['current_container_id'], retained_name)
    write_atomic(directory, 'original-container-restored.json', restored_app)

    after_live = ops.observe_live(go['target_revision'])
    verify_dependencies_and_policy(after_live, go)

    # Even if the original container was only stopped and restarted (or never renamed),
    # its StartedAt may have changed or stayed put; rebind_daily_backup syncs source.json
    # to the actual live (id, started_at) and verifies `core_backup.py source-check`.
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


def _reconcile_stop_after_lost_ack(ops, directory, current_id, retained_name):
    """When stop_and_rename_current raises (e.g. lost SSH ACK), inspect live containers to record what happened."""
    try:
        state = ops.observe_containers(current_id, retained_name)
    except Exception:
        return None
    orig = (state or {}).get('original_by_id')
    if not isinstance(orig, dict):
        return None
    observed = {
        'id': orig.get('id'),
        'name': orig.get('name'),
        'running': orig.get('running'),
        'reconciled_from_live_observation': True,
    }
    write_atomic(directory, 'stop-observed-after-error.json', observed)
    if orig.get('running') is False and orig.get('name') == retained_name and not (directory / 'current-stopped.json').exists():
        claim(directory, 'current-stopped.json', {
            'id': current_id,
            'retained_name': retained_name,
            'reconciled_after_lost_ack': True,
        })
    return observed


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
        label = error_label(error)
        write_atomic(directory, 'forward-error.json', {'error': label})
        record_event(directory, 'forward_error_triggering_recover', {'error': label})
        # Key fix (1): If stop-intent.json was written, never skip recover merely because
        # current-stopped.json is absent (e.g. lost SSH ACK after stop/rename succeeded).
        if (directory / 'stop-intent.json').exists() or (directory / 'current-stopped.json').exists():
            if not (directory / 'current-stopped.json').exists():
                _reconcile_stop_after_lost_ack(ops, directory, go['current_container_id'], retained_name)
            recover_go = dict(go, action='recover')
            recover(recover_go, ops, directory=directory)
        raise


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
    host_carrier = live.get('host_carrier') or {}
    carrier_staged = (
        host_carrier.get('runner_sha256') == hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        and host_carrier.get('unit_present') is True
    )
    docker_api141 = live.get('docker_api141_probe') or {}
    api141_ok = (
        docker_api141.get('ok') is True
        and docker_api141.get('app_id') == template['current_container_id']
        and docker_api141.get('target_image_id') == template['target_image']
    )
    return {
        'stage': (
            'inplace_stage_verified_awaiting_parent_go'
            if (deps_ok and backup_ok and identity_admitted_for_target and carrier_staged and api141_ok)
            else 'inplace_not_ready'
        ),
        'go_executed': False,
        'app_restarted': False,
        'dependencies_and_current_app_match': deps_ok,
        'current_backup_bound_and_active': backup_ok,
        'docker_api141_verified': api141_ok,
        'target_image_staged_on_nailong': live.get('loaded_target'),
        'core_identity_status': {
            'nailong_sha256': live.get('nailong_identity_sha'),
            'lag_sha256': live.get('lag_identity_sha'),
            'candidate_sha256': candidate_sha,
            'target_pair_already_admitted': identity_admitted_for_target,
        },
        'host_carrier_status': {
            'staged_on_nailong': carrier_staged,
            'root': NAILONG_INPLACE_ROOT,
            'systemd_unit': 'sub2-nailong-inplace@.service',
            **host_carrier,
        },
        'live': live,
    }


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
        return {'id': d['Id'], 'env': env, 'appdata': mounts[0]}

    def stop_and_rename_current(self, expected_id, retained_name):
        app = self.inspect_via_api141(APP_NAME)
        if app['Id'] != expected_id:
            raise InplaceError('stop_identity_mismatch')
        self.api141('POST', '/containers/' + app['Id'] + '/update', {'RestartPolicy': {'Name': 'no'}})
        if app['State']['Running']:
            self.api141('POST', '/containers/' + app['Id'] + '/kill?signal=SIGTERM')
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            cur = self.inspect_via_api141(app['Id'])
            if not cur['State']['Running']:
                self.api141('POST', '/containers/' + app['Id'] + '/rename?name=' + retained_name)
                return {'id': app['Id'], 'retained_name': retained_name, 'exit_code': cur['State']['ExitCode']}
            time.sleep(2)
        raise InplaceError('graceful_stop_timeout')

    def create_and_start_target(self, window, target_image, target_revision, env_list, appdata):
        img_info = json.loads(self.api141('GET', '/images/' + target_image + '/json'))
        rev = ((img_info.get('Config') or {}).get('Labels') or {}).get('org.opencontainers.image.revision')
        if img_info.get('Id') != target_image or rev != target_revision:
            raise InplaceError('target_image_verify_failed')

        mount = appdata + ':/app/data:rw'
        body = self._contract.api141_create_body(
            target_image,
            env_list,
            {'task53.inplace_window': window},
            [mount],
            APP_NETWORK,
            'unless-stopped',
        )
        created = json.loads(self.api141('POST', '/containers/create?name=' + APP_NAME, body))
        cid = created['Id']
        self.api141('POST', '/networks/' + DEPENDENCY_NETWORK + '/connect', {
            'Container': cid,
            'EndpointConfig': {'Aliases': [ALIAS]},
        })
        self.api141('POST', '/containers/' + cid + '/start')

        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            cur = self.inspect_via_api141(cid)
            if not cur['State']['Running'] or cur.get('RestartCount', 0) > 0:
                raise InplaceError('target_exited_or_restarted')
            if (cur['State'].get('Health') or {}).get('Status') == 'healthy':
                try:
                    status = urllib.request.urlopen('http://127.0.0.1:17861/health', timeout=5).status
                except Exception:
                    status = 0
                if status == 200:
                    return {
                        'id': cur['Id'],
                        'name': APP_NAME,
                        'image': cur['Image'],
                        'revision': rev,
                        'started_at': cur['State']['StartedAt'],
                        'health': 'healthy',
                        'loopback_http': status,
                    }
            time.sleep(2)
        raise InplaceError('target_health_timeout')

    def observe_containers(self, original_id, retained_name):
        return {
            'active_sub2': self._inspect_container(APP_NAME),
            'original_by_id': self._inspect_container(original_id),
            'retained': self._inspect_container(retained_name),
        }

    def stop_and_remove_new_container(self, new_container_id, failed_archive_name):
        cur = self.inspect_via_api141(new_container_id)
        self.api141('POST', '/containers/' + new_container_id + '/update', {'RestartPolicy': {'Name': 'no'}})
        if cur['State']['Running']:
            self.api141('POST', '/containers/' + new_container_id + '/kill?signal=SIGTERM')
        deadline = time.monotonic() + 120
        info = cur
        while time.monotonic() < deadline:
            info = self.inspect_via_api141(new_container_id)
            if not info['State']['Running']:
                break
            time.sleep(2)
        if info['State']['Running']:
            raise InplaceError('new_container_stop_timeout_still_running')
        if info['Name'].lstrip('/') == APP_NAME:
            self.api141('POST', '/containers/' + new_container_id + '/rename?name=' + failed_archive_name)
        return {
            'stopped_id': new_container_id,
            'running': False,
            'exit_code': info['State'].get('ExitCode'),
            'archived_as': failed_archive_name,
        }

    def restore_and_start_original_container(self, original_id, retained_name):
        active = self._inspect_container(APP_NAME)
        if active is not None and active['id'] != original_id:
            raise InplaceError('active_sub2_occupied_by_other_container')
        info = self.inspect_via_api141(original_id)
        if info['Name'].lstrip('/') != APP_NAME:
            self.api141('POST', '/containers/' + original_id + '/rename?name=' + APP_NAME)
        self.api141('POST', '/containers/' + original_id + '/update', {'RestartPolicy': {'Name': 'unless-stopped'}})
        if not info['State']['Running']:
            self.api141('POST', '/containers/' + original_id + '/start')

        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            cur = self.inspect_via_api141(original_id)
            if not cur['State']['Running']:
                raise InplaceError('original_container_not_running')
            if (cur['State'].get('Health') or {}).get('Status') == 'healthy':
                try:
                    status = urllib.request.urlopen('http://127.0.0.1:17861/health', timeout=5).status
                except Exception:
                    status = 0
                if status == 200:
                    labels = cur['Config'].get('Labels') or {}
                    return {
                        'id': cur['Id'],
                        'name': cur['Name'].lstrip('/'),
                        'image': cur['Image'],
                        'revision': labels.get('org.opencontainers.image.revision') or '',
                        'started_at': cur['State']['StartedAt'],
                        'health': 'healthy',
                        'loopback_http': status,
                    }
            time.sleep(2)
        raise InplaceError('original_container_health_timeout')

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
        subprocess.run(['systemctl', 'enable', '--now', BACKUP_TIMER], check=True, timeout=30)

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
checkpoint_dir = Path("/opt/sub2-core-backup/compat-nailong-config-before")
checkpoint_dir.mkdir(mode=0o700, exist_ok=True)
os.chmod(checkpoint_dir, 0o700)
current_sha = hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None
if current_sha not in (old_sha, want_sha):
    raise SystemExit("unexpected_existing_identity_sha")

if current_sha == old_sha:
    saved = checkpoint_dir / "core_identity.before-1d14bbdc3.py"
    if not saved.exists():
        saved.write_bytes(path.read_bytes())
        os.chmod(saved, 0o600)
    tmp = path.with_suffix(".py.tmp")
    tmp.write_bytes(payload)
    os.chmod(tmp, 0o644)
    tmp.replace(path)
    os.chmod(path, 0o644)

ns = {}
exec(compile(path.read_text(encoding="utf8"), str(path), "exec"), ns)
if not ns["admits"](*want_pair) or not ns["admits"](*ns["FC899_NAILONG_PAIR"]) or not ns["admits"](*ns["NAILONG_CONFIG_PAIR"]):
    raise SystemExit("admits_check_failed")
fd = os.open(path, os.O_RDONLY)
os.fsync(fd)
os.close(fd)
print(json.dumps({"sha256": hashlib.sha256(path.read_bytes()).hexdigest()}))
''' % (b64, digest, PARENT_CORE_IDENTITY_SHA, target_image, target_revision)
        nailong_res = self._json_py(self.TARGET_SSH, script, timeout=30)
        lag_res = self._json_py(self.LAG_SSH, script, timeout=30)
        return {'nailong_sha256': nailong_res['sha256'], 'lag_sha256': lag_res['sha256'], 'pair': [target_image, target_revision]}

    def stage_host_carrier(self, template):
        """Pre-stage the host-resident runner, systemd unit, and dual-end core_identity admission without touching live Sub2."""
        candidate_bytes = CANDIDATE_CORE_IDENTITY.read_bytes()
        admit_res = self.admit_core_identity_pair(candidate_bytes, template['target_image'], template['target_revision'])

        runner_bytes = Path(__file__).read_bytes()
        runner_sha = hashlib.sha256(runner_bytes).hexdigest()
        template_bytes = (HERE / 'go.template.json').read_bytes()

        install_script = r'''
import base64, hashlib, json, os, subprocess
from pathlib import Path

runner_b64 = %r
runner_sha = %r
candidate_b64 = %r
template_b64 = %r
unit_b64 = %r

root = Path("/opt/sub2api-nailong/inplace")
(root / "windows").mkdir(mode=0o700, parents=True, exist_ok=True)
os.chmod(root, 0o700)
os.chmod(root / "windows", 0o700)

runner = base64.b64decode(runner_b64)
if hashlib.sha256(runner).hexdigest() != runner_sha:
    raise SystemExit("runner_sha_mismatch")

for name, raw, mode in (
    ("inplace_window.py", runner, 0o700),
    ("core_identity_candidate.py", base64.b64decode(candidate_b64), 0o644),
    ("go.template.json", base64.b64decode(template_b64), 0o600),
):
    target = root / name
    tmp = root / (name + ".tmp")
    tmp.write_bytes(raw)
    os.chmod(tmp, mode)
    tmp.replace(target)

unit_text = base64.b64decode(unit_b64).decode("utf8")
unit_path = Path("/etc/systemd/system/sub2-nailong-inplace@.service")
if not unit_path.is_file() or unit_path.read_text(encoding="utf8") != unit_text:
    unit_path.write_text(unit_text, encoding="utf8", newline="\n")
    os.chmod(unit_path, 0o644)
subprocess.run(["systemctl", "daemon-reload"], check=True)
print(json.dumps({
    "runner_sha256": hashlib.sha256((root / "inplace_window.py").read_bytes()).hexdigest(),
    "unit_present": unit_path.is_file(),
}))
''' % (
            base64.b64encode(runner_bytes).decode('ascii'),
            runner_sha,
            base64.b64encode(candidate_bytes).decode('ascii'),
            base64.b64encode(template_bytes).decode('ascii'),
            base64.b64encode(SYSTEMD_UNIT_CONTENT.encode('utf8')).decode('ascii'),
        )
        staged_info = self._json_py(self.TARGET_SSH, install_script, timeout=60)

        # Verify live Sub2 container, daily backup source-check, and LocalNailongOps /v1.41 API probe are healthy.
        live_after = self.observe_live(template['target_revision'])
        verify_dependencies_and_policy(live_after, template)
        verify_live_current_app(live_after, template)
        if (live_after.get('source_check') or {}).get('stage') != 'core_backup_source_bound':
            raise InplaceError('source_check_broken_after_stage')
        if (live_after.get('docker_api141_probe') or {}).get('ok') is not True:
            raise InplaceError('docker_api141_probe_failed')

        return {
            'stage': 'host_carrier_and_identity_staged_without_restart',
            'admitted_identity': admit_res,
            'host_carrier': staged_info,
            'docker_api141_probe': live_after['docker_api141_probe'],
            'live_app_unchanged': live_after['app'],
            'backup_timer': live_after['backup_timer'],
            'source_check': live_after['source_check'],
        }

    def launch_via_systemd(self, go, action, local_window_dir):
        """Upload signed GO to Nailong and start `sub2-nailong-inplace@<window>.service`."""
        validate_go(go, action)
        window = go['window']
        local_window_dir = Path(local_window_dir)
        local_window_dir.mkdir(parents=True, exist_ok=True)

        payload = (json.dumps(go, indent=2) + '\n').encode('utf8')
        remote_dir = f'{NAILONG_INPLACE_ROOT}/windows/{window}'
        cmd = (
            f'umask 077; mkdir -p {remote_dir} && '
            f'tee {remote_dir}/go.json >/dev/null && chmod 600 {remote_dir}/go.json && '
            f'systemctl start --no-block sub2-nailong-inplace@{window}.service'
        )
        self._call(self.TARGET_SSH, cmd, data=payload, timeout=30)
        write_atomic(local_window_dir, 'systemd-launched.json', {
            'unit': f'sub2-nailong-inplace@{window}.service',
            'remote_window_dir': remote_dir,
            'action': action,
        })

        deadline = time.monotonic() + 360
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
                if status.get('stage') in TERMINAL_STAGES:
                    return status
            if polled.get('unit_state') in ('failed', 'inactive') and status and status.get('stage') not in TERMINAL_STAGES:
                raise InplaceError(status.get('error') or status.get('reason') or status.get('stage') or 'systemd_unit_failed')
            time.sleep(2)
        raise InplaceError('systemd_window_poll_timeout')


def host_run(go_path):
    """Entry point invoked on Nailong by `sub2-nailong-inplace@<window>.service`."""
    go_path = Path(go_path)
    go = json.loads(go_path.read_text(encoding='utf8'))
    action = go.get('action')
    directory = go_path.parent
    ops = LocalNailongOps()
    if action == 'forward':
        return forward(go, ops, template_name=go_path.name, directory=directory)
    if action == 'recover':
        return recover(go, ops, directory=directory)
    raise InplaceError('go_action_mismatch')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('ready', 'stage-carrier', 'forward', 'recover', 'host-run'))
    parser.add_argument('--go', type=Path, help='Signed parent GO file for forward/recover/host-run')
    parser.add_argument('--direct-ssh', action='store_true', help='Run forward/recover steps directly over SSH instead of Nailong systemd unit')
    args = parser.parse_args()

    if args.action == 'host-run':
        if args.go is None:
            raise InplaceError('go_file_required')
        result = host_run(args.go)
        print(json.dumps(result, indent=2))
        return

    template = json.loads((HERE / 'go.template.json').read_text(encoding='utf8'))
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
    if not args.direct_ssh:
        result = ops.launch_via_systemd(go, args.action, local_dir)
    else:
        result = forward(go, ops, template_name=args.go.name, directory=local_dir) if args.action == 'forward' else recover(go, ops, directory=local_dir)
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print('inplace_failed=' + error_label(exc), file=sys.stderr)
        raise SystemExit(1) from None
