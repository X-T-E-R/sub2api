#!/usr/bin/env bash
# Sub2API Strict Verification Crucible (Gate A + Gate B)
# Runs inside the Sub2API repository root (or passes REPO_DIR as $1).
#   Gate A.1: Zero-skip real PostgreSQL + Redis integration suite (-tags=integration)
#   Gate A.2: Default `go test -race` across integration, handler, service, pkg/antigravity
#   Gate B:   Contract-verifying Request-Rewrite & Chaos Failover Crucible (-race -count=2)
set -euo pipefail

REPO_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
BACKEND_DIR="${REPO_DIR}/backend"
OUT_DIR="${CRUCIBLE_OUT_DIR:-${REPO_DIR}/.crucible-out}"
mkdir -p "$OUT_DIR"

export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
export GOTOOLCHAIN=auto
export CI=true
export SUB2API_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:5432/sub2api_test?sslmode=disable&TimeZone=UTC"
export CODEX_DAILY_SESSION_TEST_DSN="postgres://postgres:postgres@127.0.0.1:5432/codex_daily_session_test?sslmode=disable&TimeZone=UTC"
export CODEX_TELEMETRY_TEST_DSN="postgres://postgres:postgres@127.0.0.1:5432/codex_telemetry_test?sslmode=disable&TimeZone=UTC"
export SUB2API_TEST_REDIS_ADDR="127.0.0.1:6379"

log() { printf '[ccp-crucible] %s\n' "$*"; }

reset_test_infra() {
  log "Resetting disposable PostgreSQL databases (sub2api_test, codex_daily_session_test, codex_telemetry_test) and Redis..."
  for dbname in sub2api_test codex_daily_session_test codex_telemetry_test; do
    su - postgres -c "psql -v ON_ERROR_STOP=1 -c \"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${dbname}' AND pid <> pg_backend_pid();\"" >/dev/null 2>&1 || true
    su - postgres -c "dropdb --if-exists ${dbname} && createdb -O postgres ${dbname}" >/dev/null
  done
  redis-cli -h 127.0.0.1 -p 6379 FLUSHALL >/dev/null
}

summarize_log() {
  local label="$1"
  local logfile="$2"
  local strict_zero_skip="$3"
  local pass_cnt fail_cnt skip_cnt race_cnt
  pass_cnt="$(grep -Ec '^[[:space:]]*--- PASS:' "$logfile" || true)"
  fail_cnt="$(grep -Ec '^[[:space:]]*--- FAIL:' "$logfile" || true)"
  skip_cnt="$(grep -Ec '^[[:space:]]*--- SKIP:' "$logfile" || true)"
  race_cnt="$(grep -Ec 'WARNING: DATA RACE' "$logfile" || true)"
  printf '%-36s PASS=%-5s FAIL=%-3s SKIP=%-3s RACES=%-3s\n' \
    "$label" "$pass_cnt" "$fail_cnt" "$skip_cnt" "$race_cnt" | tee -a "${OUT_DIR}/crucible_summary.txt"
  if [ "$fail_cnt" -ne 0 ] || [ "$race_cnt" -ne 0 ]; then
    return 1
  fi
  if [ "$strict_zero_skip" = "1" ] && [ "$skip_cnt" -ne 0 ]; then
    log "ERROR: ${label} had ${skip_cnt} skipped test(s); zero-skip gate failed!"
    grep -E '^[[:space:]]*--- SKIP:' "$logfile" || true
    return 1
  fi
  return 0
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
log "Running Gate A.1: real PostgreSQL + Redis integration tests..."
GATE_A1_LOG="${OUT_DIR}/gate_a1_db_redis.log"
set +e
go test -tags=integration -count=1 -v -timeout 15m \
  ./internal/repository/... \
  ./internal/middleware/... \
  ./internal/server/routes/... >"$GATE_A1_LOG" 2>&1
rc_a1=$?
set -e
summarize_log "Gate A.1 (DB+Redis integration)" "$GATE_A1_LOG" "1" || rc_a1=1
if [ "$rc_a1" -ne 0 ]; then
  log "Gate A.1 FAILED (rc=${rc_a1}). Tail of ${GATE_A1_LOG}:"
  tail -n 60 "$GATE_A1_LOG"
  exit "$rc_a1"
fi

# ── Gate A.2: Default `go test -race` across core packages ───────────────────
log "Running Gate A.2: default go test -race across integration, handler, service, pkg/antigravity..."
GATE_A2_LOG="${OUT_DIR}/gate_a2_race.log"
set +e
go test -race -count=1 -v -timeout 15m \
  ./internal/integration/... \
  ./internal/handler/... \
  ./internal/service/... \
  ./internal/pkg/antigravity/... >"$GATE_A2_LOG" 2>&1
rc_a2=$?
set -e
summarize_log "Gate A.2 (default -race)" "$GATE_A2_LOG" "0" || rc_a2=1
if [ "$rc_a2" -ne 0 ]; then
  log "Gate A.2 FAILED (rc=${rc_a2}). Failures in ${GATE_A2_LOG}:"
  grep -nE '(--- FAIL:|WARNING: DATA RACE|FAIL[[:space:]]+github\.com)' "$GATE_A2_LOG" | head -n 60 || true
  exit "$rc_a2"
fi

# ── Gate B: Contract-Verifying Request-Rewrite & Chaos Failover Crucible ─────
log "Running Gate B: Chaos Failover & Rewrite Contract Crucible (-race -count=2)..."
GATE_B_LOG="${OUT_DIR}/gate_b_chaos_crucible.log"
set +e
go test -race -count=2 -v -timeout 10m \
  ./internal/integration \
  -run "TestChaosCrucible_|TestAntigravityMessages_|TestOpenAI|TestE2E_" >"$GATE_B_LOG" 2>&1
rc_b=$?
set -e
summarize_log "Gate B (Chaos Crucible -race x2)" "$GATE_B_LOG" "1" || rc_b=1
if [ "$rc_b" -ne 0 ]; then
  log "Gate B FAILED (rc=${rc_b}). Tail of ${GATE_B_LOG}:"
  tail -n 60 "$GATE_B_LOG"
  exit "$rc_b"
fi

log "ALL CRUCIBLE GATES PASSED!"
cat "${OUT_DIR}/crucible_summary.txt"
