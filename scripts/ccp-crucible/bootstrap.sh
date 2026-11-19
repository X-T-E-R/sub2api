#!/usr/bin/env bash
# Bootstrap a clean Debian/Ubuntu Linux CCP VM for Sub2API strict verification:
#   - Go toolchain (>= 1.27.3, with gcc/libc6-dev for -race)
#   - Disposable local PostgreSQL (sub2api_test, postgres:postgres on 127.0.0.1:5432)
#   - Disposable local Redis (127.0.0.1:6379)
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

log() { printf '[ccp-bootstrap] %s\n' "$*"; }

# Safety guard: this script mutates local pg_hba.conf (trust auth on loopback),
# resets the postgres superuser password, and provisions disposable test DBs.
if [ "${SUB2API_CRUCIBLE_EPHEMERAL_VM:-}" != "1" ]; then
  log "REFUSED: set SUB2API_CRUCIBLE_EPHEMERAL_VM=1 to confirm this host is a disposable crucible VM."
  exit 1
fi

# 1. Base packages: gcc (for Go -race), postgresql, redis-server, curl, ca-certificates.
log "Checking system packages (gcc, postgresql, redis-server)..."
if ! command -v gcc >/dev/null 2>&1 || ! command -v psql >/dev/null 2>&1 || ! command -v redis-server >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends \
    ca-certificates curl gcc libc6-dev postgresql postgresql-client redis-server >/dev/null
fi

# 2. Go toolchain (go.mod requires >= 1.27.3).
export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
need_go=1
if command -v go >/dev/null 2>&1; then
  if go version | grep -Eq 'go1\.(2[6-9]|[3-9][0-9])'; then
    need_go=0
  fi
fi

if [ "$need_go" -eq 1 ]; then
  log "Installing Go toolchain into /usr/local/go..."
  rm -rf /usr/local/go
  installed=0
  for ver in 1.27.3 1.26.3 1.26.2 1.25.5; do
    url="https://go.dev/dl/go${ver}.linux-amd64.tar.gz"
    if curl -fsSL "$url" -o /tmp/go.tar.gz 2>/dev/null; then
      tar -C /usr/local -xzf /tmp/go.tar.gz
      rm -f /tmp/go.tar.gz
      installed=1
      log "Installed Go ${ver} from ${url}"
      break
    fi
  done
  if [ "$installed" -ne 1 ]; then
    log "ERROR: failed to download Go tarball from go.dev/dl"
    exit 1
  fi
fi

export GOTOOLCHAIN=auto
log "Go version: $(go version)"

# 3. Start local PostgreSQL & provision sub2api_test database.
log "Starting local PostgreSQL..."
pg_ver="$(ls /etc/postgresql 2>/dev/null | sort -V | tail -n 1 || true)"
if [ -n "$pg_ver" ]; then
  pg_hba="/etc/postgresql/${pg_ver}/main/pg_hba.conf"
  if [ -f "$pg_hba" ] && ! grep -q '# ccp-crucible-trust' "$pg_hba"; then
    {
      echo "# ccp-crucible-trust"
      echo "host all all 127.0.0.1/32 trust"
      echo "host all all ::1/128 trust"
      cat "$pg_hba"
    } > "${pg_hba}.new"
    mv "${pg_hba}.new" "$pg_hba"
  fi
  pg_ctlcluster "$pg_ver" main restart || service postgresql restart
else
  service postgresql start
fi

for i in $(seq 1 30); do
  if pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
pg_isready -h 127.0.0.1 -p 5432

su - postgres -c "psql -v ON_ERROR_STOP=1 -c \"ALTER USER postgres WITH PASSWORD 'postgres';\"" >/dev/null
for dbname in sub2api_test codex_daily_session_test codex_telemetry_test; do
  su - postgres -c "psql -tc \"SELECT 1 FROM pg_database WHERE datname = '${dbname}'\" | grep -q 1 || createdb -O postgres ${dbname}" >/dev/null
done
log "PostgreSQL ready (127.0.0.1:5432/{sub2api_test,codex_daily_session_test,codex_telemetry_test}, $(psql -h 127.0.0.1 -U postgres -d sub2api_test -tAc 'SELECT version()' | cut -d',' -f1))"

# 4. Start local Redis on 127.0.0.1:6379.
log "Starting local Redis..."
if ! redis-cli -h 127.0.0.1 -p 6379 ping >/dev/null 2>&1; then
  redis-server --bind 127.0.0.1 --port 6379 --daemonize yes --save "" --appendonly no
fi
for i in $(seq 1 20); do
  if redis-cli -h 127.0.0.1 -p 6379 ping 2>/dev/null | grep -q PONG; then
    break
  fi
  sleep 0.2
done
redis-cli -h 127.0.0.1 -p 6379 ping >/dev/null
log "Redis ready (127.0.0.1:6379, $(redis-server --version))"
log "Bootstrap complete."
