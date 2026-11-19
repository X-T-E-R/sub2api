#!/usr/bin/env bash
# Sub2API Strict Verification Crucible (Gate A + Gate B)
# Runs inside the Sub2API repository root (or passes REPO_DIR as $1).
#   Gate A.1: Zero-skip real PostgreSQL + Redis integration suite (-tags=integration)
#   Gate A.2: Default + unit-tagged `go test -tags=unit -race` across integration, handler, service, pkg/antigravity
#   Gate B:   Contract-verifying Request-Rewrite & Chaos Failover Crucible (-race -count=2)
set -euo pipefail

REPO_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
BACKEND_DIR="${REPO_DIR}/backend"
OUT_DIR="${CRUCIBLE_OUT_DIR:-${REPO_DIR}/.crucible-out}"
mkdir -p "$OUT_DIR"

export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
export GOTOOLCHAIN=auto
export CI=true

# Hermetic offline guard: strip live external API credentials and ambient GOFLAGS
# so no test can hit a real third-party endpoint or have -run/-skip silently prune tests.
unset OPENAI_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY SUB2API_RUN_LIVE_OPENAI_COMPARE GOFLAGS || true

export SUB2API_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/sub2api_test?sslmode=disable&TimeZone=UTC"
export CODEX_DAILY_SESSION_TEST_DSN="postgres://postgres:postgres@127.0.0.1:5432/codex_daily_session_test?sslmode=disable&TimeZone=UTC"
export CODEX_TELEMETRY_TEST_DSN="postgres://postgres:postgres@127.0.0.1:5432/codex_telemetry_test?sslmode=disable&TimeZone=UTC"
export SUB2API_TEST_REDIS_ADDR="127.0.0.1:6379"

log() { printf '[ccp-crucible] %s\n' "$*"; }

# Safety guard: reset_test_infra drops test PostgreSQL databases and runs Redis FLUSHALL.
if [ "${SUB2API_CRUCIBLE_EPHEMERAL_VM:-}" != "1" ]; then
  log "REFUSED: set SUB2API_CRUCIBLE_EPHEMERAL_VM=1 to confirm this host is a disposable crucible VM."
  exit 1
fi

reset_test_infra() {
  log "Resetting disposable PostgreSQL databases (sub2api_test, codex_daily_session_test, codex_telemetry_test) and Redis..."
  for dbname in sub2api_test codex_daily_session_test codex_telemetry_test; do
    su - postgres -c "psql -v ON_ERROR_STOP=1 -c \"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${dbname}' AND pid <> pg_backend_pid();\"" >/dev/null 2>&1 || true
    su - postgres -c "dropdb --if-exists ${dbname} && createdb -O postgres ${dbname}" >/dev/null
  done
  redis-cli -h 127.0.0.1 -p 6379 FLUSHALL >/dev/null
}

# Parse `go test -json` streams strictly so compile errors, zero-executed tests,
# data races, unexpected skips, or missing -count=2 chaos runs can never slip past.
verify_go_test_json() {
  local label="$1"
  local logfile="$2"
  local mode="$3" # zero_skip | allowlist_a2 | gate_b
  python3 - "$label" "$logfile" "$mode" "${OUT_DIR}/crucible_summary.txt" <<'PY'
import json, sys
from collections import Counter

label, logfile, mode, summary_path = sys.argv[1:5]
allowlist_a2 = {
    "TestDingTalkOAuthStart_Disabled",
    "TestAuthPendingIdentityService_UpsertAdoptionDecision_ClearsLegacyNullSessionReference",
    "TestEstimateOpenAIInputTokens_CompareWithOpenAIAPI",
    "TestPluginRuntimeIntegration",
}
required_gate_b = (
    "TestChaosCrucible_AntigravityConcurrentRewriteFailoverAndDisconnects",
    "TestChaosCrucible_OpenAIConcurrentResponsesChatFailoverAndDisconnects",
    "TestAntigravityMessages_Stage2MalformedReapplyOn400SignatureAndBudgetRetries",
    "TestOpenAI_UnifiedChannelAndAccountModelMappingMatrix",
    "TestOpenAIResponses_LoopbackWebSocketUpstreamRewriteParity",
)
expected_pkgs = {
    "zero_skip": (
        "github.com/Wei-Shaw/sub2api/internal/repository",
        "github.com/Wei-Shaw/sub2api/internal/middleware",
        "github.com/Wei-Shaw/sub2api/internal/server/routes",
    ),
    "gate_b": (
        "github.com/Wei-Shaw/sub2api/internal/integration",
    ),
}

pass_cnt = 0
fail_cnt = 0
skip_cnt = 0
race_cnt = 0
pkg_fails = []
pkg_skips = []
pkg_passes = set()
failed_tests = []
skipped_tests = []
passed_by_name = Counter()
passed_by_pkg = Counter()
non_json_errors = []

with open(logfile, "r", encoding="utf-8", errors="replace") as fh:
    for line in fh:
        raw = line.strip()
        if not raw:
            continue
        if "WARNING: DATA RACE" in raw:
            race_cnt += 1
        try:
            ev = json.loads(raw)
        except json.JSONDecodeError:
            # Non-JSON compiler or runtime stderr lines
            if any(tok in raw for tok in ("FAIL", "build failed", "cannot find", "undefined:", "panic:")):
                non_json_errors.append(raw)
            continue
        action = ev.get("Action", "")
        test_name = ev.get("Test", "")
        pkg = ev.get("Package", "")
        if test_name:
            if action == "pass":
                pass_cnt += 1
                passed_by_name[test_name] += 1
                if pkg:
                    passed_by_pkg[pkg] += 1
            elif action == "fail":
                fail_cnt += 1
                failed_tests.append(f"{pkg}:{test_name}")
            elif action == "skip":
                skip_cnt += 1
                skipped_tests.append((pkg, test_name))
        else:
            if action == "fail":
                pkg_fails.append(pkg or "<unknown-pkg>")
            elif action == "skip":
                pkg_skips.append(pkg or "<unknown-pkg>")
            elif action == "pass" and pkg:
                pkg_passes.add(pkg)

summary_line = f"{label:<36} PASS={pass_cnt:<5} FAIL={fail_cnt:<3} SKIP={skip_cnt:<3} RACES={race_cnt:<3}\n"
sys.stdout.write(summary_line)
with open(summary_path, "a", encoding="utf-8") as sf:
    sf.write(summary_line)

errors = []
if pass_cnt == 0:
    errors.append(f"{label}: zero tests passed (possible build error or bad -run filter)")
if fail_cnt > 0:
    errors.append(f"{label}: {fail_cnt} test(s) failed: {failed_tests[:10]}")
if pkg_fails:
    errors.append(f"{label}: package failure(s): {pkg_fails[:10]}")
if pkg_skips:
    errors.append(f"{label}: package-level skip(s) detected: {pkg_skips[:10]}")
if race_cnt > 0:
    errors.append(f"{label}: {race_cnt} DATA RACE warning(s) detected")
if non_json_errors:
    errors.append(f"{label}: non-JSON error lines: {non_json_errors[:5]}")

for exp_pkg in expected_pkgs.get(mode, ()):
    if exp_pkg not in pkg_passes:
        errors.append(f"{label}: expected package did not record Action==pass: {exp_pkg}")
    elif passed_by_pkg[exp_pkg] == 0:
        errors.append(f"{label}: expected package ran 0 passing tests: {exp_pkg}")

if mode in ("zero_skip", "gate_b"):
    if skipped_tests:
        errors.append(f"{label}: zero-skip violated by {len(skipped_tests)} test(s): {skipped_tests[:10]}")
elif mode == "allowlist_a2":
    unexpected = [(p, t) for (p, t) in skipped_tests if t not in allowlist_a2]
    if unexpected:
        errors.append(f"{label}: unexpected skipped test(s) outside allowlist: {unexpected[:10]}")

if mode == "gate_b":
    for req_test in required_gate_b:
        if passed_by_name[req_test] < 2:
            errors.append(f"{label}: expected >=2 passes (-count=2) for {req_test}, got {passed_by_name[req_test]}")

if errors:
    for e in errors:
        sys.stderr.write(f"[ccp-crucible] ERROR: {e}\n")
    sys.exit(1)
PY
}

rm -f "${OUT_DIR}/crucible_summary.txt"
{
  echo "Sub2API CCP Crucible Summary ($(date -u '+%Y-%m-%dT%H:%M:%SZ'))"
  echo "Host: $(uname -srm) | CPUs: $(nproc) | Go: $(go version)"
  echo "------------------------------------------------------------------------"
} > "${OUT_DIR}/crucible_summary.txt"

cd "$BACKEND_DIR"

# ── Gate A.1: Zero-Skip Real Postgres + Redis Integration ──────────────────────
reset_test_infra
log "Running Gate A.1: real PostgreSQL + Redis integration tests (-json)..."
GATE_A1_LOG="${OUT_DIR}/gate_a1_db_redis.jsonl"
set +e
go test -json -tags=integration -count=1 -timeout 15m \
  ./internal/repository/... \
  ./internal/middleware/... \
  ./internal/server/routes/... >"$GATE_A1_LOG" 2>&1
rc_a1=$?
set -e
verify_go_test_json "Gate A.1 (DB+Redis integration)" "$GATE_A1_LOG" "zero_skip" || rc_a1=1
if [ "$rc_a1" -ne 0 ]; then
  log "Gate A.1 FAILED (rc=${rc_a1}). Tail of ${GATE_A1_LOG}:"
  tail -n 60 "$GATE_A1_LOG"
  exit "$rc_a1"
fi

# ── Gate A.2: Default + unit-tagged `go test -tags=unit -race` ────────────────
log "Running Gate A.2: go test -json -tags=unit -race across integration, handler, service, pkg/antigravity..."
GATE_A2_LOG="${OUT_DIR}/gate_a2_race.jsonl"
set +e
go test -json -tags=unit -race -count=1 -timeout 15m \
  ./internal/integration/... \
  ./internal/handler/... \
  ./internal/service/... \
  ./internal/pkg/antigravity/... >"$GATE_A2_LOG" 2>&1
rc_a2=$?
set -e
verify_go_test_json "Gate A.2 (-tags=unit -race)" "$GATE_A2_LOG" "allowlist_a2" || rc_a2=1
if [ "$rc_a2" -ne 0 ]; then
  log "Gate A.2 FAILED (rc=${rc_a2}). Failing lines in ${GATE_A2_LOG}:"
  grep -nE '("Action":"fail"|WARNING: DATA RACE|FAIL[[:space:]]+github\.com)' "$GATE_A2_LOG" | head -n 60 || true
  exit "$rc_a2"
fi

# ── Gate B: Contract-Verifying Request-Rewrite & Chaos Failover Crucible ─────
log "Running Gate B: Chaos Failover & Rewrite Contract Crucible (-json -race -count=2)..."
GATE_B_LOG="${OUT_DIR}/gate_b_chaos_crucible.jsonl"
set +e
go test -json -race -count=2 -timeout 10m \
  ./internal/integration \
  -run "TestChaosCrucible_|TestAntigravityMessages_|TestOpenAI|TestE2E_" >"$GATE_B_LOG" 2>&1
rc_b=$?
set -e
verify_go_test_json "Gate B (Chaos Crucible -race x2)" "$GATE_B_LOG" "gate_b" || rc_b=1
if [ "$rc_b" -ne 0 ]; then
  log "Gate B FAILED (rc=${rc_b}). Tail of ${GATE_B_LOG}:"
  tail -n 60 "$GATE_B_LOG"
  exit "$rc_b"
fi

log "ALL CRUCIBLE GATES PASSED!"
cat "${OUT_DIR}/crucible_summary.txt"
