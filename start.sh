#!/usr/bin/env bash
# Multica self-host operations script.
#
# Runs the app straight from this checkout (no Docker for the app itself):
# builds the Go backend + CLI, builds the Next.js web frontend, applies
# migrations, then supervises backend / frontend / CLI daemon and reloads the
# system nginx in front of them. The database is treated as an external
# dependency this script can either reach, start, or provision.
#
#   ./start.sh                 # same as "start"
#   ./start.sh preflight       # environment report, changes nothing
#   ./start.sh start           # build if needed, migrate, start, health-check
#   ./start.sh stop            # stop only what this script started
#   ./start.sh restart
#   ./start.sh status
#   ./start.sh health          # exit 0 only when everything answers
#   ./start.sh ensure          # idempotent: start only what is down (no build,
#                              # no migrate, no nginx) — for cron/systemd timers
#   ./start.sh logs backend -f
#   ./start.sh build           # force rebuild + migrate, keep serving
#   ./start.sh migrate         # apply DB migrations only
#   ./start.sh daemon restart
#   ./start.sh nginx reload
#   ./start.sh bootstrap       # first-time host prep, then build
#   ./start.sh autostart on    # install + enable the systemd boot units
#   ./start.sh autostart off   # disable and remove them
#
# Layout
#   ROOT        checkout                        (default: this script's dir)
#   DATA        state, logs, pids               (default: ~/multica-data)
#   LOG_DIR     $DATA/logs/<service>.log
#   RUN_DIR     $DATA/run/<service>.pid|.cmd
#   GO_HOME     private Go toolchain            (default: ~/go/go)
#
# The web frontend is optional. Upstream self-hosting serves the UI from
# Cloudflare Pages and only the API from the host (see
# docker-compose.selfhost.yml), which is also the cheapest option on a small
# box: `next build` wants several GB of RAM. So the frontend is started only
# when a .next build already exists, and MULTICA_WEB=1 forces build+serve,
# MULTICA_WEB=0 forces backend-only.
#
# Who runs the application process is a per-host decision:
#   MULTICA_APP_MODE=compose  the backend is the compose `backend` service, so
#                              Docker's restart policy owns crash recovery and
#                              this script only drives the container
#   MULTICA_APP_MODE=native   the backend is a host process under a pidfile
#   MULTICA_APP_MODE=auto     compose as soon as a backend container exists
# Put it in .env so cron, the boot unit and the self-heal timer all agree.
#
# Every knob is an environment variable; see "CONFIG" below.
set -euo pipefail

# ---------------------------------------------------------------------------
# CONFIG
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${MULTICA_ROOT:-$SCRIPT_DIR}"
DATA="${MULTICA_DATA:-$HOME/multica-data}"
LOG_DIR="$DATA/logs"
RUN_DIR="$DATA/run"
MARKER="$LOG_DIR/deployed-commit"
ENV_FILE="${MULTICA_ENV_FILE:-$ROOT/.env}"
SERVER_BIN="$ROOT/server/bin"
WEB_DIR="$ROOT/apps/web"
CLI_DEST="${MULTICA_CLI_DEST:-$HOME/.local/bin/multica}"
GO_HOME="${MULTICA_GO_HOME:-$HOME/go/go}"
PGDATA_DIR="${MULTICA_PGDATA:-$DATA/pgdata}"
NATIVE_PG_PORT="${MULTICA_PG_PORT:-5433}"
DB_MODE="${MULTICA_DB_MODE:-auto}"          # auto | container | native | external
AUTO_GO="${MULTICA_AUTO_INSTALL_GO:-1}"
LOG_MAX_BYTES="${MULTICA_LOG_MAX_BYTES:-52428800}"   # rotate a service log past 50 MB
HEALTH_TIMEOUT="${MULTICA_HEALTH_TIMEOUT:-90}"
SYSTEMD_DIR="${MULTICA_SYSTEMD_DIR:-/etc/systemd/system}"
STOP_TIMEOUT="${MULTICA_STOP_TIMEOUT:-20}"
PG_BOOTSTRAP="${MULTICA_DB_BOOTSTRAP:-0}"   # 1 = allow creating role/database
COMPOSE_FILES=(
  "$ROOT/docker-compose.selfhost.yml"
  "$ROOT/docker-compose.selfhost.build.yml"
  # Optional extra override; only passed to docker compose when it exists.
  "$ROOT/docker-compose.selfhost.hostport.yml"
)

# ---------------------------------------------------------------------------
# OUTPUT
# ---------------------------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_B=$'\033[1m'; C_OK=$'\033[32m'; C_WARN=$'\033[33m'
  C_ERR=$'\033[31m'; C_DIM=$'\033[2m'
else
  C_RESET=""; C_B=""; C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""
fi
step() { printf '%s==>%s %s%s%s\n' "$C_B" "$C_RESET" "$C_B" "$*" "$C_RESET"; }
info() { printf '    %s\n' "$*"; }
dim()  { printf '    %s%s%s\n' "$C_DIM" "$*" "$C_RESET"; }
ok()   { printf '    %s✓%s %s\n' "$C_OK" "$C_RESET" "$*"; }
warn() { printf '    %s!%s %s\n' "$C_WARN" "$C_RESET" "$*" >&2; }
fail() { printf '%s✗ %s%s\n' "$C_ERR" "$*" "$C_RESET" >&2; exit 1; }

# A private Go install must win over any distro Go: `make` shells out to plain
# `go`, so PATH order is what actually decides the toolchain (the distro's
# go1.18 cannot parse a patch-level `go` directive at all).
export PATH="$GO_HOME/bin:$PATH"

# ---------------------------------------------------------------------------
# SMALL UTILITIES
# ---------------------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

# version_ge A B -> true when A >= B (dotted numeric versions)
version_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

# url_component URL FIELD  (FIELD: host|port|user|db) — no external deps
url_component() {
  local url=$1 field=$2 rest creds
  rest="${url#*://}"                    # user:pass@host:port/db?query
  creds="${rest%%@*}"
  rest="${rest##*@}"
  case $field in
    user) printf '%s' "${creds%%:*}" ;;
    pass) creds="${creds#*:}"; printf '%s' "$creds" ;;
    host) rest="${rest%%/*}"; printf '%s' "${rest%%:*}" ;;
    port) rest="${rest%%/*}"; case $rest in *:*) printf '%s' "${rest##*:}" ;; *) printf 5432 ;; esac ;;
    db)
      case $rest in
        */*) rest="${rest#*/}"; printf '%s' "${rest%%\?*}" ;;
        *) printf '' ;;
      esac
      ;;
  esac
}

tcp_ready() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null; }

# A listening socket is not the same as a usable database: the port can belong
# to a cluster that does not have our role/database. When psql is around, ask
# the server itself.
PSQL=""
resolve_psql() {
  local cand
  for cand in "${MULTICA_PSQL:-}" /usr/lib/postgresql/*/bin/psql /usr/pgsql-*/bin/psql /usr/local/pgsql/bin/psql; do
    [ -n "$cand" ] && [ -x "$cand" ] && { PSQL="$cand"; break; }
  done
  [ -n "$PSQL" ] || PSQL="$(command -v psql 2>/dev/null || true)"
  [ -n "$PSQL" ] || return 1
  local psqldir; psqldir="$(dirname "$PSQL")"
  export PATH="$psqldir:$PATH"
  return 0
}

db_query_ok() {
  resolve_psql || return 2   # 2 = cannot tell (no psql anywhere)
  local rc=0
  # psql exits 2 on a connection error, so never pass its status through: map
  # every failure to 1 and keep 2 for "no client available".
  PGPASSWORD="$(url_component "$DB_URL" pass)" \
    "$PSQL" "postgresql://$DB_USER@$DB_HOST:$DB_PORT/$DB_NAME?sslmode=disable" \
    -tAc 'select 1' >/dev/null 2>&1 || rc=1
  return "$rc"
}

http_code() { curl -s -o /dev/null -m 5 -w '%{http_code}' "$1" 2>/dev/null || echo 000; }

# Reports the database the way an operator cares about it: reachable AND
# usable, or the reason it is not.
db_report() {
  local q=0
  if ! tcp_ready "$DB_HOST" "$DB_PORT"; then
    printf '%s\n' "unreachable at $DB_HOST:$DB_PORT/$DB_NAME (mode $(db_mode_resolved))"
    return 1
  fi
  db_query_ok || q=$?
  case $q in
    0) printf '%s\n' "usable at $DB_HOST:$DB_PORT/$DB_NAME (mode $(db_mode_resolved))" ;;
    2) printf '%s\n' "listening on $DB_HOST:$DB_PORT/$DB_NAME, not query-checked (psql not found)" ;;
    *) printf '%s\n' "TCP open at $DB_HOST:$DB_PORT but role/database '$DB_USER'@'$DB_NAME' is unusable (is this the right cluster?)" ;;
  esac
  return "$q"
}

# Port owner, with the process name when the kernel exposes it.
# True when the pid still holds a listening socket on some *other* port, e.g.
# after BACKEND_PORT was changed under a running deployment. Such a process
# must never be killed as "unhealthy": it is serving something else.
pid_holds_other_port() {
  local pid="${1:-}" mine="${2:-}"
  [ -n "$pid" ] || return 1
  ss -ltnp 2>/dev/null | grep -F "pid=$pid," | awk '{print $4}' |
    grep -v ":${mine}\$" | grep -q .
}

port_owner() {
  ss -ltnp 2>/dev/null | awk -v p=":$1\$" '$4 ~ p {
      pid=""; if (match($0, /pid=[0-9]+/)) pid = substr($0, RSTART+4, RLENGTH-4);
      name=""; if (match($0, /users:\(\("[^"]+"/)) name = substr($0, RSTART+10, RLENGTH-11);
      print pid, name; exit }'
}

docker_container_on_port() {
  have docker || return 0
  docker ps --filter "publish=$1" --format '{{.Names}}' 2>/dev/null | head -n1
}

pid_alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

pid_cmdline() { tr '\0\n' '  ' <"/proc/$1/cmdline" 2>/dev/null || true; }

# ---------------------------------------------------------------------------
# TOOLCHAIN RESOLUTION
# ---------------------------------------------------------------------------
NODE_BIN=""; PNPM=""; GO=""; GO_VERSION=""

resolve_node() {
  local cand
  for cand in "$HOME"/.nvm/versions/node/*/bin; do
    [ -d "$cand" ] || continue
    if [ -z "$NODE_BIN" ] || [ "$cand" \> "$NODE_BIN" ]; then NODE_BIN="$cand"; fi
  done
  [ -n "$NODE_BIN" ] && export PATH="$NODE_BIN:$PATH"
  if have pnpm; then PNPM="$(command -v pnpm)"; return 0; fi
  if have corepack && [ -n "$NODE_BIN" ]; then
    PNPM="$NODE_BIN/pnpm"
    [ -x "$PNPM" ] && return 0
  fi
  return 1
}

node_version() { node -v 2>/dev/null | sed 's/^v//'; }

# Required Go version comes from the module itself, so a `git pull` that bumps
# go.mod re-triggers the toolchain check with no edit here.
go_required_version() {
  sed -n 's/^go[[:space:]]\{1,\}\([0-9][0-9.]*\).*/\1/p' "$ROOT/server/go.mod" 2>/dev/null | head -n1
}

resolve_go() {
  local cand
  for cand in "${MULTICA_GO:-}" "$GO_HOME/bin/go"; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then GO="$cand"; break; fi
  done
  if [ -z "$GO" ] && have go; then GO="$(command -v go)"; fi
  [ -n "$GO" ] || return 1
  GO_VERSION="$("$GO" env GOVERSION 2>/dev/null | sed 's/^go//')"
  [ -n "$GO_VERSION" ] || return 1
  # Whatever `go` we vetted has to be the one `make` will run.
  if [ "$(command -v go 2>/dev/null || true)" != "$GO" ]; then
    local godir; godir="$(dirname "$GO")"
    export PATH="$godir:$PATH"
  fi
  return 0
}

install_go_toolchain() {
  local want=$1 arch tarball base tmp want_sha got_sha stage
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail "unsupported architecture $(uname -m) for the Go toolchain" ;;
  esac
  base="${MULTICA_GO_MIRROR:-https://golang.google.cn/dl}"
  tarball="go${want}.linux-${arch}.tar.gz"
  tmp="$(mktemp -d)"
  stage="$GO_HOME.install.$$"
  step "installing Go $want into $GO_HOME"
  info "download $base/$tarball"
  curl -fsSL --retry 3 -o "$tmp/$tarball" "$base/$tarball" || fail "download failed: $base/$tarball"
  want_sha="$(curl -fsSL -m 30 "$base/$tarball.sha256" 2>/dev/null | awk '{print $1}')"
  got_sha="$(sha256sum "$tmp/$tarball" | awk '{print $1}')"
  if [ -n "$want_sha" ]; then
    [ "$want_sha" = "$got_sha" ] || { rm -rf "$tmp" "$stage"; fail "sha256 mismatch for $tarball"; }
    ok "sha256 verified"
  else
    warn "no .sha256 published for $tarball; continuing unverified"
  fi
  mkdir -p "$stage"
  tar -C "$stage" --strip-components=1 -xzf "$tmp/$tarball"
  rm -rf "$tmp"
  if [ -d "$GO_HOME" ] && [ ! -L "$GO_HOME" ]; then
    mv "$GO_HOME" "$GO_HOME.bak.$(date +%Y%m%d%H%M%S)"
    info "previous toolchain kept at $GO_HOME.bak.*"
  fi
  mkdir -p "$(dirname "$GO_HOME")"
  mv "$stage" "$GO_HOME"
  ok "Go $want installed ($GO_HOME/bin/go)"
}

ensure_go() {
  step "toolchain: Go"
  local want; want="$(go_required_version)"
  [ -n "$want" ] || fail "cannot read the go directive from $ROOT/server/go.mod"
  if resolve_go && version_ge "$GO_VERSION" "$want"; then
    ok "go $GO_VERSION ($GO) satisfies go.mod's $want"
    return 0
  fi
  if [ -n "${GO:-}" ]; then
    warn "found go $GO_VERSION ($GO) but go.mod needs >= $want"
  else
    warn "no go toolchain found; go.mod needs >= $want"
  fi
  if [ "$AUTO_GO" != "1" ]; then
    fail "set MULTICA_AUTO_INSTALL_GO=1 to let this script install Go ${MULTICA_GO_VERSION:-$want}"
  fi
  install_go_toolchain "${MULTICA_GO_VERSION:-$want}"
  resolve_go || fail "go still not usable after install"
  version_ge "$GO_VERSION" "$want" || fail "go $GO_VERSION still older than required $want"
  ok "go $GO_VERSION ready"
}

ensure_node() {
  step "toolchain: Node / pnpm"
  resolve_node || fail "pnpm not found; install Node >= 22 (nvm) plus pnpm ${MULTICA_PNPM_VERSION:-10}"
  local nv; nv="$(node_version)"
  if [ -n "$nv" ] && ! version_ge "$nv" "22.0.0"; then
    fail "node $nv is too old; this repo needs >= 22"
  fi
  ok "node ${nv:-?} ($(command -v node)), pnpm $("$PNPM" --version 2>/dev/null || echo '?')"
}

# ---------------------------------------------------------------------------
# ENVIRONMENT FILE
# ---------------------------------------------------------------------------
load_env() {
  [ -f "$ENV_FILE" ] || fail "missing $ENV_FILE — cp $ROOT/.env.example $ENV_FILE and fill it in"
  # .env provides the defaults, the environment overrides them: an operator
  # running `MULTICA_APP_MODE=native ./start.sh stop` must not be overruled by
  # whatever the same key is pinned to in .env. Remember the incoming values and
  # put them back after sourcing.
  local -A incoming=()
  local k
  while IFS='=' read -r k _; do
    case "$k" in
      [A-Za-z_]*) ;;
      *) continue ;;
    esac
    [ -n "${!k+set}" ] && incoming["$k"]="${!k}"
  done <"$ENV_FILE"
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
  for k in "${!incoming[@]}"; do
    # shellcheck disable=SC2163  # the name is dynamic on purpose
    printf -v "$k" '%s' "${incoming[$k]}"
    export "${k?}"
  done
  BACKEND_PORT="${BACKEND_PORT:-${API_PORT:-${SERVER_PORT:-${PORT:-8080}}}}"
  WEB_PORT="${FRONTEND_PORT:-3010}"
  DB_URL="${DATABASE_URL:-}"
  [ -n "$DB_URL" ] || fail "DATABASE_URL is not set in $ENV_FILE"
  DB_HOST="$(url_component "$DB_URL" host)"
  DB_PORT="$(url_component "$DB_URL" port)"
  DB_NAME="$(url_component "$DB_URL" db)"
  DB_USER="$(url_component "$DB_URL" user)"
  APP_ORIGIN="${FRONTEND_ORIGIN:-http://localhost:$WEB_PORT}"
  # The web app proxies /api, /ws, /uploads server-side (apps/web/next.config.ts),
  # so the frontend process needs to know where the backend is at build AND run
  # time. The browser keeps using the same origin (NEXT_PUBLIC_API_URL empty).
  REMOTE_API_URL="${REMOTE_API_URL:-http://127.0.0.1:$BACKEND_PORT}"
  APP_MODE="$(app_mode_resolved)"
  export REMOTE_API_URL APP_MODE
}

# ---------------------------------------------------------------------------
# DATABASE
# ---------------------------------------------------------------------------
compose() {
  local args=() f
  for f in "${COMPOSE_FILES[@]}"; do [ -f "$f" ] && args+=(-f "$f"); done
  [ "${#args[@]}" -gt 0 ] || { warn "no compose file found in $ROOT"; return 1; }
  docker compose "${args[@]}" "$@"
}

db_mode_resolved() {
  case "$DB_MODE" in
    auto)
      if tcp_ready "$DB_HOST" "$DB_PORT"; then echo external
      elif have docker && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^multica-postgres'; then echo container
      elif [ -f "$PGDATA_DIR/PG_VERSION" ]; then echo native
      else echo external
      fi
      ;;
    *) echo "$DB_MODE" ;;
  esac
}

db_container_published_port() {
  compose port postgres 5432 2>/dev/null | awk -F: 'NR==1{print $NF}'
}

db_container_ensure() {
  have docker || { warn "docker not available; cannot manage the database container"; return 1; }
  local name; name="$(docker ps -a --format '{{.Names}}' | grep -E '(^|[-_])postgres([-_]1)?$' | head -n1)"
  if [ -z "$name" ]; then
    step "database (container)"
    info "starting the postgres service from docker-compose.selfhost.yml"
    compose up -d postgres || return 1
  elif [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" != "true" ]; then
    step "database (container)"
    docker start "$name" >/dev/null || return 1
    ok "container $name started"
  else
    step "database (container)"
    ok "container $name already running"
  fi
  local pub; pub="$(db_container_published_port)"
  if [ -z "$pub" ]; then
    fail "postgres container $name publishes no host port, so host processes cannot reach it.
       Publish it on $DB_PORT once, then re-run this script:
         printf 'services:\\n  postgres:\\n    ports:\\n      - \"127.0.0.1:$DB_PORT:5432\"\\n' \\
           > $ROOT/docker-compose.selfhost.hostport.yml
         docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml \\
           -f docker-compose.selfhost.hostport.yml up -d postgres
       and point DATABASE_URL at 127.0.0.1:$DB_PORT in $ENV_FILE"
  fi
  [ "$pub" = "$DB_PORT" ] || warn "postgres is published on host port $pub but DATABASE_URL says $DB_PORT"
  db_wait_ready
}

db_native_ensure() {
  step "database (native cluster at $PGDATA_DIR)"
  local pgctl="" initdb_bin cand
  for cand in "${MULTICA_PG_CTL:-}" "${PGHOME:-/nonexistent}/bin/pg_ctl" "$HOME/apps/multica-pg/bin/pg_ctl" \
              /usr/lib/postgresql/*/bin/pg_ctl /usr/local/pgsql/bin/pg_ctl; do
    [ -n "$cand" ] && [ -x "$cand" ] && { pgctl="$cand"; break; }
  done
  [ -n "$pgctl" ] || pgctl="$(command -v pg_ctl 2>/dev/null || true)"
  [ -n "$pgctl" ] || fail "no pg_ctl found; set MULTICA_PG_CTL or install PostgreSQL >= 15
       (migrations use NULLS NOT DISTINCT, which needs 15+; upstream ships pg17)"
  if [ ! -f "$PGDATA_DIR/PG_VERSION" ]; then
    [ "$PG_BOOTSTRAP" = "1" ] || fail "no cluster at $PGDATA_DIR. Create one with:
       $(dirname "$pgctl")/initdb -D $PGDATA_DIR -U $DB_USER --auth=md5
       $pgctl -D $PGDATA_DIR -l $LOG_DIR/postgres.log -o '-p $NATIVE_PG_PORT' start
       (or re-run with MULTICA_DB_BOOTSTRAP=1 to let this script run initdb for you)"
    step "initialising cluster at $PGDATA_DIR"
    initdb_bin="$(dirname "$pgctl")/initdb"
    [ -x "$initdb_bin" ] || fail "initdb not found next to $pgctl"
    "$initdb_bin" -D "$PGDATA_DIR" -U "$DB_USER" --auth=md5 >/dev/null || fail "initdb failed"
    "$pgctl" -D "$PGDATA_DIR" -l "$LOG_DIR/postgres.log" \
      -o "-p $NATIVE_PG_PORT -c listen_addresses=127.0.0.1" start >/dev/null
    ok "cluster initialised on 127.0.0.1:$NATIVE_PG_PORT"
  else
    "$pgctl" -D "$PGDATA_DIR" status >/dev/null 2>&1 \
      || "$pgctl" -D "$PGDATA_DIR" -l "$LOG_DIR/postgres.log" \
         -o "-p $NATIVE_PG_PORT -c listen_addresses=127.0.0.1" start >/dev/null
    ok "cluster running on 127.0.0.1:$DB_PORT"
  fi
  db_wait_ready
}

db_wait_ready() {
  local _
  for _ in $(seq 1 60); do
    if tcp_ready "$DB_HOST" "$DB_PORT"; then ok "database reachable at $DB_HOST:$DB_PORT/$DB_NAME"; return 0; fi
    sleep 1
  done
  fail "database not reachable at $DB_HOST:$DB_PORT after 60s"
}

db_ensure() {
  local mode; mode="$(db_mode_resolved)"
  if tcp_ready "$DB_HOST" "$DB_PORT"; then
    step "database"
    ok "reachable at $DB_HOST:$DB_PORT/$DB_NAME (mode: $mode)"
    return 0
  fi
  case "$mode" in
    container) db_container_ensure ;;
    native) db_native_ensure ;;
    *) fail "cannot reach the database at $DB_HOST:$DB_PORT/$DB_NAME
       check DATABASE_URL in $ENV_FILE, or set MULTICA_DB_MODE=container|native" ;;
  esac
}

# ---------------------------------------------------------------------------
# PORTS
# ---------------------------------------------------------------------------
assert_port_free() {
  local port=$1 name=$2 owner pid cname
  owner="$(port_owner "$port")"
  [ -n "$owner" ] || return 0
  pid="${owner%% *}"
  if pidfile_owned "$name" "$pid"; then return 0; fi
  cname="$(docker_container_on_port "$port")"
  fail "port $port is already taken by ${cname:-pid $pid} ($(pid_cmdline "$pid" | cut -c1-80))
       stop it first, or move this deployment's port (BACKEND_PORT / FRONTEND_PORT in $ENV_FILE)"
}

# ---------------------------------------------------------------------------
# PROCESS SUPERVISION
# ---------------------------------------------------------------------------
log_rotate() {
  local name=$1 f="$LOG_DIR/$1.log"
  [ -f "$f" ] || return 0
  local size; size="$(wc -c <"$f")"
  [ "$size" -gt "$LOG_MAX_BYTES" ] || return 0
  mv -f "$f" "$f.1"
  info "rotated $name.log ($size bytes -> $(basename "$f").1)"
}

# launch NAME WORKDIR PATTERN CMD...
#   Starts CMD in its own session (setsid) so the whole process tree can be
#   stopped as a group, and records both the pid and the cmdline pattern used
#   later to prove the pid is still ours before killing anything.
launch() {
  local name=$1 dir=$2 pattern=$3; shift 3
  log_rotate "$name"
  mkdir -p "$LOG_DIR" "$RUN_DIR"
  ( cd "$dir" && setsid bash -c 'printf "%s\n" "$$" >"$1"; shift; exec "$@"' \
      _ "$RUN_DIR/$name.pid" "$@" >>"$LOG_DIR/$name.log" 2>&1 </dev/null & )
  printf '%s\n' "$pattern" >"$RUN_DIR/$name.cmd"
  # The child writes its own pid, so wait for that instead of guessing, and
  # fail loudly when the process dies during startup (bad flag, port taken...).
  local _
  for _ in $(seq 1 50); do
    [ -s "$RUN_DIR/$name.pid" ] && break
    sleep 0.1
  done
  [ -s "$RUN_DIR/$name.pid" ] || { warn "$name: no pid appeared in $RUN_DIR/$name.pid"; return 1; }
  sleep 0.3
  pid_alive "$(cat "$RUN_DIR/$name.pid")" || { dump_log "$name"; return 1; }
  return 0
}

pidfile_owned() {
  local name=$1 pid=$2 pattern f
  f="$RUN_DIR/$name.cmd"
  [ -f "$f" ] || return 1
  pattern="$(cat "$f")"
  [ -n "$pattern" ] || return 1
  case "$(pid_cmdline "$pid")" in *"$pattern"*) return 0 ;; *) return 1 ;; esac
}

svc_pid() { cat "$RUN_DIR/$1.pid" 2>/dev/null || true; }

svc_running() {
  local pid; pid="$(svc_pid "$1")"
  pid_alive "$pid" && pidfile_owned "$1" "$pid"
}

# Waits for a URL to answer, but only while the process we launched is still
# alive: without that check a stale listener on the same port can make a
# freshly started (and dying) service look healthy.
wait_svc_http() {
  local name=$1 url=$2 want=$3 deadline code
  deadline=$((SECONDS + HEALTH_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! svc_running "$name"; then
      warn "$name exited while waiting for $url"
      return 1
    fi
    code="$(http_code "$url")"
    case ",$want," in *",$code,"*) ok "$name answers $code at $url"; return 0 ;; esac
    sleep 2
  done
  return 1
}

dump_log() {
  local name=$1
  warn "last 30 lines of $LOG_DIR/$name.log:"
  tail -n 30 "$LOG_DIR/$name.log" 2>/dev/null | sed 's/^/      /' >&2 || true
}

# Is the local web frontend part of this deployment?
#   MULTICA_WEB=1   build and serve it
#   MULTICA_WEB=0   backend only (UI comes from Cloudflare Pages)
#   unset/auto      serve it when a .next build already exists
web_enabled() {
  case "${MULTICA_WEB:-auto}" in
    0|off|no|false) return 1 ;;
    1|on|yes|true) return 0 ;;
    *) [ -d "$WEB_DIR/.next" ] ;;
  esac
}

web_skip_notice() {
  info "web frontend not started (MULTICA_WEB=${MULTICA_WEB:-auto}, no $WEB_DIR/.next)."
  info "Serve the UI from Cloudflare Pages, or build it here: MULTICA_WEB=1 ./start.sh build"
}

# ---------------------------------------------------------------------------
# BUILD / DEPLOY
# ---------------------------------------------------------------------------
head_commit() { git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown; }

build_backend() {
  step "build: Go backend + CLI"
  have make || fail "make not found"
  ( cd "$ROOT" && make build )
  ok "binaries in $SERVER_BIN"
  mkdir -p "$(dirname "$CLI_DEST")"
  install -m 0755 "$SERVER_BIN/multica" "$CLI_DEST"
  ok "CLI installed at $CLI_DEST"
}

build_frontend() {
  step "build: web frontend (apps/web)"
  ( cd "$ROOT" && "$PNPM" install --frozen-lockfile --prefer-offline ) \
    || warn "pnpm install failed (offline?); continuing with the existing node_modules"
  local rc=0
  set +e
  ( cd "$WEB_DIR" && "$PNPM" build ) 2>&1 | tee "$LOG_DIR/web-build.log"
  rc="${PIPESTATUS[0]}"
  set -e
  [ "$rc" = 0 ] || fail "frontend build failed (full log: $LOG_DIR/web-build.log)"
  [ -d "$WEB_DIR/.next" ] || fail "frontend build produced no .next (see $LOG_DIR/web-build.log)"
  ok "frontend built (.next)"
}

migrate_db() {
  step "migrations"
  [ -x "$SERVER_BIN/migrate" ] || fail "$SERVER_BIN/migrate is missing; run './start.sh build'"
  ( cd "$ROOT" && "$SERVER_BIN/migrate" up )
  ok "migrations applied"
}

deploy() {
  build_backend
  if web_enabled; then
    build_frontend
  else
    dim "skipping the web build (MULTICA_WEB=${MULTICA_WEB:-auto})"
  fi
  migrate_db
  mkdir -p "$LOG_DIR"
  head_commit >"$MARKER"
  ok "deployed $(head_commit)"
}

needs_build() {
  [ "${MULTICA_SKIP_BUILD:-}" = "1" ] && return 1
  [ "${MULTICA_FORCE_BUILD:-}" = "1" ] && return 0
  [ -x "$SERVER_BIN/server" ] && [ -x "$SERVER_BIN/migrate" ] || return 0
  if web_enabled && [ ! -d "$WEB_DIR/.next" ]; then return 0; fi
  [ "$(head_commit)" = "$(cat "$MARKER" 2>/dev/null || echo none)" ] && return 1
  return 0
}

# ---------------------------------------------------------------------------
# SERVICES
# ---------------------------------------------------------------------------
# Who owns the application process?
#   compose  the backend runs as the compose `backend` service; Docker restarts
#            it on exit and start.sh only drives the container
#   native   the backend is a host process this script launched from a pidfile
#   auto     compose as soon as a backend container exists for this project,
#            native otherwise
MULTICA_APP_MODE_DEFAULT="auto"
app_mode_resolved() {
  local want="${MULTICA_APP_MODE:-$MULTICA_APP_MODE_DEFAULT}"
  case "$want" in
    compose|native) printf '%s\n' "$want"; return 0 ;;
    auto) ;;
    *) fail "MULTICA_APP_MODE must be auto, compose or native (got '$want')" ;;
  esac
  if have docker && [ -n "$(compose ps -q backend 2>/dev/null)" ]; then
    printf 'compose\n'
  else
    printf 'native\n'
  fi
}

app_compose_cid() { compose ps -q backend 2>/dev/null | head -1; }

# "healthy" only when a healthcheck exists; the stock backend service has none,
# so running + an answer from /health is the real signal.
app_compose_state() {
  local cid; cid="$(app_compose_cid)"
  [ -n "$cid" ] || { printf 'absent\n'; return 1; }
  docker inspect -f '{{.State.Status}}{{if .State.Health}}/{{.State.Health.Status}}{{end}}' "$cid" 2>/dev/null ||
    printf 'unknown\n'
}

app_compose_running() {
  local cid; cid="$(app_compose_cid)"
  [ -n "$cid" ] || return 1
  [ "$(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null)" = "true" ]
}

app_compose_up() {
  step "service: backend (docker compose)"
  local cid out
  if app_compose_running && [ "$(http_code "http://127.0.0.1:$BACKEND_PORT/health")" = "200" ]; then
    ok "backend container already running and healthy ($(app_compose_cid | cut -c1-12))"
    return 0
  fi
  if ! out="$(compose up -d backend 2>&1)"; then
    printf '%s\n' "$out" >&2
    fail "docker compose up -d backend failed"
  fi
  printf '%s\n' "$out" | sed 's/^/    /'
  cid="$(app_compose_cid)"
  [ -n "$cid" ] || fail "compose reported success but no backend container exists"
  ok "backend container $(printf '%s' "$cid" | cut -c1-12) $(app_compose_state)"
}

app_compose_stop() {
  step "service: backend (docker compose)"
  if [ -z "$(app_compose_cid)" ]; then
    dim "no backend container; nothing to stop"
    return 0
  fi
  compose stop backend >/dev/null 2>&1 || warn "compose stop backend failed"
  ok "backend container stopped (postgres and the CLI daemon were left alone)"
}

app_compose_restart() {
  step "service: backend (docker compose)"
  compose restart backend >/dev/null 2>&1 || fail "compose restart backend failed"
  ok "backend container restarted"
}

# Waits for the API to answer; the container itself reports running/unhealthy.
app_compose_wait_healthy() {
  local deadline code
  deadline=$((SECONDS + HEALTH_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(http_code "http://127.0.0.1:$BACKEND_PORT/health")"
    if [ "$code" = "200" ]; then
      ok "backend /health -> 200 (container $(app_compose_state))"
      return 0
    fi
    case "$(app_compose_state)" in
      exited*|dead*|absent) warn "backend container $(app_compose_state) while waiting for /health" ;;
    esac
    sleep 2
  done
  return 1
}

# ---------------------------------------------------------------------------
start_backend() {
  step "service: backend"
  if svc_running backend; then
    if [ "$(http_code "http://127.0.0.1:$BACKEND_PORT/health")" = "200" ]; then
      ok "backend already running and healthy (pid $(svc_pid backend)) — kept"
      return 0
    fi
    if pid_holds_other_port "$(svc_pid backend)" "$BACKEND_PORT"; then
      fail "the pid this script started (pid $(svc_pid backend)) is listening on a
       different port than :$BACKEND_PORT — BACKEND_PORT changed underneath a
       running deployment. Nothing was killed; stop it yourself or restore the
       port in $ENV_FILE"
    fi
    warn "backend is running but not answering /health; restarting it"
    stop_service backend
  fi
  assert_port_free "$BACKEND_PORT" backend
  # LOCAL_UPLOAD_DIR and friends are relative in .env, so the server has to run
  # from the checkout root; the uploads dir also has to exist and be writable.
  mkdir -p "$ROOT/data/uploads"
  ( cd "$ROOT" && launch backend "$ROOT" "$SERVER_BIN/server" "$SERVER_BIN/server" ) ||
    fail "backend process did not stay up — see $LOG_DIR/backend.log"
  local pid; pid="$(svc_pid backend)"
  if ! wait_svc_http backend "http://127.0.0.1:$BACKEND_PORT/health" "200"; then
    dump_log backend
    stop_service backend
    fail "backend did not become healthy on :$BACKEND_PORT"
  fi
  ok "backend pid $pid on 127.0.0.1:$BACKEND_PORT"
  if ! ss -ltn 2>/dev/null | awk -v p=":$BACKEND_PORT\$" '$4 ~ p {print $4}' | grep -q '^127\.0\.0\.1:'; then
    warn "backend is listening on all interfaces (the Go server has no bind-address option).
       Put a firewall rule in front of :$BACKEND_PORT or front it with nginx only."
  fi
}

start_frontend() {
  step "service: frontend (Next.js)"
  if svc_running frontend; then
    case "$(http_code "http://127.0.0.1:$WEB_PORT/")" in
      200|301|302|307|401|403)
        ok "frontend already running (pid $(svc_pid frontend)) — kept"
        return 0 ;;
    esac
    if pid_holds_other_port "$(svc_pid frontend)" "$WEB_PORT"; then
      fail "the frontend pid $(svc_pid frontend) is listening on a port other than
       :$WEB_PORT — nothing was killed; fix FRONTEND_PORT in $ENV_FILE"
    fi
    warn "frontend is running but not answering; restarting it"
    stop_service frontend
  fi
  assert_port_free "$WEB_PORT" frontend
  [ -d "$WEB_DIR/.next" ] || fail "no .next build; run './start.sh build' first"
  ( cd "$WEB_DIR" && PORT="$WEB_PORT" HOSTNAME=127.0.0.1 REMOTE_API_URL="$REMOTE_API_URL" \
      launch frontend "$WEB_DIR" "next start" "$PNPM" exec next start -p "$WEB_PORT" ) ||
    fail "frontend process did not stay up — see $LOG_DIR/frontend.log"
  local pid; pid="$(svc_pid frontend)"
  if ! wait_svc_http frontend "http://127.0.0.1:$WEB_PORT/" "200,301,302,307,401,403"; then
    dump_log frontend
    stop_service frontend
    fail "frontend did not answer on :$WEB_PORT"
  fi
  ok "frontend pid $pid on 127.0.0.1:$WEB_PORT"
}

# System nginx keeps its TLS keys root-only, so both `nginx -t` and the reload
# need privileges. Use sudo only when a plain invocation cannot work, and only
# if it is available without a password prompt (an interactive prompt inside a
# deploy script would hang it).
NGINX=""
resolve_nginx_cmd() {
  local bin cand
  for cand in /usr/sbin/nginx /usr/local/nginx/sbin/nginx /usr/local/sbin/nginx; do
    [ -x "$cand" ] && { bin="$cand"; break; }
  done
  [ -n "$bin" ] || return 1
  if [ "$(id -u)" = "0" ] || "$bin" -t >/dev/null 2>&1; then
    NGINX="$bin"
    return 0
  fi
  if have sudo && sudo -n true 2>/dev/null; then
    NGINX="sudo -n $bin"
    return 0
  fi
  return 1
}

nginx_reload() {
  step "service: nginx"
  if ! resolve_nginx_cmd; then
    warn "nginx not found (or its keys are unreadable without sudo); skipping the reverse
       proxy. The API is still reachable on 127.0.0.1:${BACKEND_PORT:-8080}"
    return 0
  fi
  if [ "${MULTICA_SKIP_NGINX:-}" = "1" ]; then
    dim "MULTICA_SKIP_NGINX=1 -> not touching nginx"
    return 0
  fi
  dim "using: $NGINX -t"
  $NGINX -t >/dev/null 2>&1 || fail "nginx config test failed; fix it or re-run with MULTICA_SKIP_NGINX=1"
  if pgrep -x nginx >/dev/null 2>&1; then
    if $NGINX -s reload; then ok "nginx reloaded"; else warn "nginx reload failed"; fi
  else
    if $NGINX; then ok "nginx started"; else warn "nginx failed to start"; fi
  fi
  # Report on the hostnames this deployment is supposed to answer for: the API
  # origin always, the app origin only when the web runs on this host.
  local hosts="" host
  [ -n "${MULTICA_PUBLIC_URL:-}" ] && hosts="$(url_component "$MULTICA_PUBLIC_URL" host)"
  if web_enabled; then
    host="$(url_component "$APP_ORIGIN" host)"
    case " $hosts " in *" $host "*) ;; *) hosts="${hosts:+$hosts }$host" ;; esac
  fi
  # Dump the config once: `nginx -T | grep -q` is a race under `set -o pipefail`
  # (grep exits on the first match, nginx dies on SIGPIPE, the pipeline reports
  # failure and every vhost looks missing).
  local conf; conf="$($NGINX -T 2>/dev/null || true)"
  for host in $hosts; do
    if printf '%s' "$conf" | grep -q "server_name.*[[:space:]]$host\(\|[[:space:];]\)"; then
      ok "vhost for $host present"
    else
      warn "no nginx vhost serves $host — that URL will not work until you add one
       (a ready template for the web origin lives at $ROOT/nginx-multica.conf)"
    fi
  done
}

start_daemon() {
  step "service: multica daemon"
  [ -x "$CLI_DEST" ] || { warn "CLI not installed at $CLI_DEST; skipping daemon"; return 0; }
  if "$CLI_DEST" daemon status >/dev/null 2>&1; then
    # The daemon is the CLI agent runtime: restarting it interrupts whatever it
    # is running, so a plain `start` leaves it alone. Opt in explicitly.
    if [ "${MULTICA_RESTART_DAEMON:-0}" = "1" ]; then
      "$CLI_DEST" daemon restart >>"$LOG_DIR/daemon.log" 2>&1 || warn "daemon restart failed (see $LOG_DIR/daemon.log)"
      ok "daemon restarted with the new build"
    else
      ok "daemon already running (left as-is; MULTICA_RESTART_DAEMON=1 to reload it)"
    fi
  else
    "$CLI_DEST" daemon start >>"$LOG_DIR/daemon.log" 2>&1 || warn "daemon start failed (see $LOG_DIR/daemon.log)"
    ok "daemon started"
  fi
}

stop_daemon() {
  [ -x "$CLI_DEST" ] || return 0
  "$CLI_DEST" daemon status >/dev/null 2>&1 || return 0
  "$CLI_DEST" daemon stop >>"$LOG_DIR/daemon.log" 2>&1 || warn "daemon stop failed"
  ok "daemon stopped"
}

stop_service() {
  local name=$1 pid pattern
  local pidfile="$RUN_DIR/$name.pid"
  if [ ! -f "$pidfile" ]; then
    dim "$name: no pidfile, nothing to stop"
    return 0
  fi
  pid="$(cat "$pidfile" 2>/dev/null || true)"
  rm -f "$pidfile"
  [ -n "$pid" ] || return 0
  if ! pid_alive "$pid"; then
    dim "$name: pid $pid is gone"
    rm -f "$RUN_DIR/$name.cmd"
    return 0
  fi
  if ! pidfile_owned "$name" "$pid"; then
    warn "$name: pid $pid is not ours (cmdline: $(pid_cmdline "$pid" | cut -c1-80)); refusing to kill it"
    return 0
  fi
  pattern="$(cat "$RUN_DIR/$name.cmd" 2>/dev/null || echo '')"
  printf '    %s stopping (pid %s)%s\n' "$name" "$pid" "${pattern:+ [$pattern]}"
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  local _
  for _ in $(seq 1 $((STOP_TIMEOUT * 2))); do
    pid_alive "$pid" || break
    sleep 0.5
  done
  if pid_alive "$pid"; then
    warn "$name: still alive after ${STOP_TIMEOUT}s, sending SIGKILL"
    kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$RUN_DIR/$name.cmd"
}

stop_all() {
  step "stopping services"
  stop_service frontend
  stop_service backend
  ok "app stopped; the database and the CLI daemon were left running (./start.sh daemon stop for the daemon)"
}

# ---------------------------------------------------------------------------
# COMMANDS
# ---------------------------------------------------------------------------
# A systemd timer, a deploy and an operator can all reach this script at once;
# without a lock two runs fight over the same pidfile and port.
acquire_lock() {
  mkdir -p "$RUN_DIR"
  exec 9>"$RUN_DIR/multica.lock"
  if ! flock -w "${MULTICA_LOCK_TIMEOUT:-300}" 9; then
    fail "another start.sh run is holding $RUN_DIR/multica.lock (waited ${MULTICA_LOCK_TIMEOUT:-300}s)"
  fi
}

cmd_start() {
  acquire_lock
  mkdir -p "$LOG_DIR" "$RUN_DIR"
  load_env
  db_ensure
  if [ "$APP_MODE" = "compose" ]; then
    step "deploy: docker compose owns the app process (MULTICA_APP_MODE=compose)"
    dim "image: $(compose config --images 2>/dev/null | grep -i backend | head -1 || echo 'multica-backend:dev')"
    dim "rebuild the image with './start.sh build' when the source moved"
  else
  ensure_go
  ensure_node
  if needs_build; then
    step "deploy: source changed since the last deploy ($(cat "$MARKER" 2>/dev/null || echo none) -> $(head_commit))"
    deploy
    stop_all
  else
    step "deploy: no source change since $(cat "$MARKER" 2>/dev/null || echo 'never'); reusing the build"
  fi
  fi
  if [ "$APP_MODE" = "compose" ]; then
    app_compose_up
  else
    start_backend
  fi
  if web_enabled; then
    start_frontend
  else
    web_skip_notice
  fi
  nginx_reload
  start_daemon
  echo
  cmd_status
}

cmd_stop() {
  acquire_lock
  mkdir -p "$RUN_DIR"
  load_env
  if [ "$APP_MODE" = "compose" ]; then
    app_compose_stop
  else
    stop_all
  fi
}

cmd_restart() {
  cmd_stop
  cmd_start
}

cmd_build() {
  mkdir -p "$LOG_DIR" "$RUN_DIR"
  load_env
  db_ensure
  if [ "$APP_MODE" = "compose" ]; then
    step "build: docker image for the backend service"
    # Keep the version metadata the Dockerfile bakes in, so the API reports
    # the same commit the checkout is on.
    VERSION="${VERSION:-v0.5.0-$(git -C "$ROOT" rev-list --count HEAD 2>/dev/null || echo 0)-g$(head_commit)}"
    COMMIT="${COMMIT:-$(head_commit)}"
    DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
    export VERSION COMMIT DATE
    dim "VERSION=$VERSION COMMIT=$COMMIT"
    compose build backend || fail "docker compose build backend failed"
    app_compose_up
    app_compose_wait_healthy || fail "the rebuilt image did not become healthy on :$BACKEND_PORT"
    ok "build finished and serving"
    return 0
  fi
  ensure_go
  ensure_node
  deploy
  ok "build finished; restart to serve it: ./start.sh restart"
}

cmd_migrate() {
  load_env
  db_ensure
  migrate_db
}

cmd_health() {
  load_env
  local rc=0
  step "health"
  if db_report >/dev/null; then ok "database $(db_report)"; else warn "database $(db_report || true)"; rc=1; fi
  if [ "$APP_MODE" = "compose" ]; then
    local code; code="$(http_code "http://127.0.0.1:$BACKEND_PORT/health")"
    if [ "$code" = "200" ]; then
      ok "backend /health -> $code (container $(app_compose_state))"
    else
      warn "backend /health -> $code (container $(app_compose_state 2>/dev/null || echo absent))"
      rc=1
    fi
  elif svc_running backend; then
    local code; code="$(http_code "http://127.0.0.1:$BACKEND_PORT/health")"
    if [ "$code" = "200" ]; then ok "backend /health -> $code"; else warn "backend /health -> $code"; rc=1; fi
  else
    warn "backend not running"; rc=1
  fi
  if web_enabled; then
    if svc_running frontend; then
      local code; code="$(http_code "http://127.0.0.1:$WEB_PORT/")"
      case "$code" in 200|301|302|307|401|403) ok "frontend / -> $code" ;; *) warn "frontend / -> $code"; rc=1 ;; esac
    else
      warn "frontend not running"; rc=1
    fi
  else
    dim "frontend not part of this deployment (MULTICA_WEB=${MULTICA_WEB:-auto})"
  fi
  if [ "$rc" = 0 ]; then ok "all green"; else fail "unhealthy"; fi
}

# Deliberately cheap and side-effect-free unless something is actually down:
# this is what a systemd timer calls every couple of minutes, so it must not
# rebuild, migrate, or reload nginx. `start` does the heavy lifting.
cmd_ensure() {
  acquire_lock
  step "ensure: app and CLI daemon up (no build / no migrate / no nginx)"
  load_env
  local fixed=0

  if [ "$APP_MODE" = "compose" ]; then
    # Docker restarts the container when the process exits; this covers the
    # failure mode it cannot see — a container that is up but no longer
    # answering, e.g. wedged on the database.
    if [ "$(http_code "http://127.0.0.1:$BACKEND_PORT/health")" = "200" ]; then
      dim "backend: container $(app_compose_state) answering on 127.0.0.1:$BACKEND_PORT"
    elif app_compose_running; then
      warn "backend container $(app_compose_state) is not answering /health — restarting it"
      app_compose_restart
      app_compose_wait_healthy || fail "backend did not answer /health after a restart"
      fixed=1
    else
      warn "backend container is not running — starting it"
      app_compose_up
      app_compose_wait_healthy || fail "backend did not answer /health after start"
      fixed=1
    fi
  elif svc_running backend && [ "$(http_code "http://127.0.0.1:$BACKEND_PORT/health")" = "200" ]; then
    dim "backend: up (pid $(svc_pid backend)) on 127.0.0.1:$BACKEND_PORT"
  elif [ "$(http_code "http://127.0.0.1:$BACKEND_PORT/health")" = "200" ]; then
    # Something this script never started (a recreated compose backend, most
    # likely) is answering. Never touch a foreign process: report it instead of
    # silently adopting it, or the deployment would drift back to Docker.
    fail "127.0.0.1:$BACKEND_PORT is served by a process this script did not start
       (pid $(port_owner "$BACKEND_PORT") $(port_owner "$BACKEND_PORT" | xargs -r -I{} sh -c 'tr "\\0\\n" "  " </proc/{}/cmdline 2>/dev/null' 2>/dev/null))
       Stop it (or 'docker rm -f multica-backend-1') if the native backend should own the port"
  else
    warn "backend is down or not answering /health — starting it"
    db_ensure >/dev/null 2>&1 || warn "database is not reachable; the backend may fail to start"
    start_backend
    fixed=1
  fi

  if web_enabled; then
    case "$(http_code "http://127.0.0.1:$WEB_PORT/")" in
      200|301|302|307|401|403) dim "frontend: up on 127.0.0.1:$WEB_PORT" ;;
      *)
        warn "frontend is down — starting it"
        start_frontend
        fixed=1 ;;
    esac
  fi

  if [ -x "$CLI_DEST" ]; then
    if "$CLI_DEST" daemon status >/dev/null 2>&1; then
      dim "daemon: running"
    else
      # Only reachable outside a daemon-hosted task: the CLI refuses to start a
      # second daemon from inside one, and a task can only exist while the
      # daemon it runs on is alive.
      warn "CLI daemon is down — starting it"
      if "$CLI_DEST" daemon start >>"$LOG_DIR/daemon.log" 2>&1; then
        ok "daemon started"
        fixed=1
      else
        fail "daemon start failed (see $LOG_DIR/daemon.log)"
      fi
    fi
  else
    warn "CLI not installed at $CLI_DEST — run ./start.sh start once to install it"
  fi

  if [ "$fixed" = 1 ]; then
    ok "ensure: something was down and has been restarted"
  else
    dim "ensure: nothing to do, everything is already up"
  fi
}

cmd_status() {
  load_env 2>/dev/null || true
  step "status"
  info "root      $ROOT @ $(head_commit)"
  info "env       $ENV_FILE"
  info "build     $(cat "$MARKER" 2>/dev/null || echo 'never deployed')"
  if [ -n "${DB_URL:-}" ]; then
    info "database  $(db_report || true)"
  fi
  local entry name pid port path
  for entry in "backend:${BACKEND_PORT:-8080}:/health" "frontend:${WEB_PORT:-3010}:/"; do
    name="${entry%%:*}"; entry="${entry#*:}"; port="${entry%%:*}"; path="${entry#*:}"
    pid="$(svc_pid "$name" 2>/dev/null || true)"
    if [ "$name" = backend ] && [ "$APP_MODE" = "compose" ]; then
      local bstate bhttp
      bstate="$(app_compose_state 2>/dev/null || echo absent)"
      bhttp="$(http_code "http://127.0.0.1:$port$path")"
      info "$(printf '%-9s' "$name") container $bstate  127.0.0.1:$port  http $bhttp  [$(app_compose_cid | cut -c1-12)]"
    elif svc_running "$name"; then
      info "$(printf '%-9s' "$name") pid $pid  127.0.0.1:$port  http $(http_code "http://127.0.0.1:$port$path")"
    elif [ "$name" = frontend ] && ! web_enabled; then
      info "$(printf '%-9s' "$name") not deployed here (MULTICA_WEB=${MULTICA_WEB:-auto})"
    else
      info "$(printf '%-9s' "$name") stopped"
    fi
  done
  if pgrep -x nginx >/dev/null 2>&1; then
    info "$(printf '%-9s' nginx) running"
  else
    info "$(printf '%-9s' nginx) not running"
  fi
  if [ -x "$CLI_DEST" ] && "$CLI_DEST" daemon status >/dev/null 2>&1; then
    info "$(printf '%-9s' daemon) running"
  else
    info "$(printf '%-9s' daemon) stopped"
  fi
  echo
  if web_enabled; then info "app   http://127.0.0.1:${WEB_PORT:-3010}"; fi
  info "api   http://127.0.0.1:${BACKEND_PORT:-8080}"
  if [ -n "${MULTICA_PUBLIC_URL:-}" ]; then info "public ${MULTICA_PUBLIC_URL}"; fi
  if web_enabled && [ -n "${APP_ORIGIN:-}" ]; then info "ui    ${APP_ORIGIN}"; fi
  info "logs  $LOG_DIR"
  return 0
}

cmd_logs() {
  local name="${1:-}" follow=""
  [ "${2:-}" = "-f" ] && follow="-F"
  mkdir -p "$LOG_DIR"
  if [ -z "$name" ]; then
    for f in "$LOG_DIR"/*.log; do
      [ -f "$f" ] || continue
      printf '\n===== %s =====\n' "$(basename "$f")"
      tail -n 10 "$f"
    done
    return 0
  fi
  [ -f "$LOG_DIR/$name.log" ] || fail "no $LOG_DIR/$name.log (have: $(find "$LOG_DIR" -maxdepth 1 -name '*.log' -printf '%f ' 2>/dev/null))"
  # shellcheck disable=SC2086
  tail -n 50 $follow "$LOG_DIR/$name.log"
}

cmd_daemon() {
  load_env 2>/dev/null || true
  case "${1:-status}" in
    start|stop|restart|status) "$CLI_DEST" daemon "${1}" ;;
    *) fail "usage: $0 daemon start|stop|restart|status" ;;
  esac
}

# Boot + self-heal wiring. The unit files live in the repo (deploy/systemd) and
# are copied into /etc/systemd/system, so a re-clone can re-apply them.
install_units() {
  local src="$ROOT/deploy/systemd" unit
  [ -d "$src" ] || fail "missing unit source directory $src"
  have sudo || fail "sudo is required to install systemd units"
  mkdir -p "$SYSTEMD_DIR"
  for unit in multica.service multica-ensure.service multica-ensure.timer; do
    [ -f "$src/$unit" ] || fail "missing $src/$unit"
    sudo -n install -m 0644 "$src/$unit" "$SYSTEMD_DIR/$unit"
    ok "installed $SYSTEMD_DIR/$unit"
  done
  sudo -n systemctl daemon-reload
  ok "systemd units reloaded"
}

cmd_autostart() {
  case "${1:-status}" in
    on|install|enable)
      step "autostart: boot units"
      install_units
      # enable only, never start: a boot unit that also starts now would fight
      # the process this script is already running from.
      sudo -n systemctl enable multica.service multica-ensure.timer >/dev/null
      ok "enabled multica.service + multica-ensure.timer (take effect on next boot)"
      dim "run 'sudo systemctl start multica.service' to wire it up immediately"
      ;;
    off|uninstall)
      step "autostart: removing boot units"
      have sudo || fail "sudo is required"
      sudo -n systemctl disable --now multica-ensure.timer >/dev/null 2>&1 || true
      sudo -n systemctl disable multica.service >/dev/null 2>&1 || true
      sudo -n rm -f "$SYSTEMD_DIR/multica.service" "$SYSTEMD_DIR/multica-ensure.service" \
        "$SYSTEMD_DIR/multica-ensure.timer"
      sudo -n systemctl daemon-reload
      ok "boot units removed (the running app was left alone)"
      ;;
    status|"")
      step "autostart: boot units"
      if have systemctl; then
        for unit in multica.service multica-ensure.timer; do
          printf '    %-24s enabled=%-10s active=%s\n' "$unit" \
            "$(systemctl is-enabled "$unit" 2>/dev/null || echo no)" \
            "$(systemctl is-active "$unit" 2>/dev/null || echo inactive)"
        done
      else
        warn "no systemctl on this host; boot autostart is not available"
      fi
      dim "unit source: $ROOT/deploy/systemd"
      ;;
    *) fail "usage: $0 autostart on|off|status" ;;
  esac
}

cmd_nginx() {
  resolve_nginx_cmd || fail "no usable nginx binary"
  case "${1:-reload}" in
    check) $NGINX -t ;;
    reload) nginx_reload ;;
    *) fail "usage: $0 nginx check|reload" ;;
  esac
}

cmd_preflight() {
  mkdir -p "$LOG_DIR" "$RUN_DIR"
  step "preflight"
  info "root           $ROOT"
  info "data           $DATA"
  info "go.mod needs   $(go_required_version 2>/dev/null || echo '?')"
  if resolve_go; then
    if version_ge "$GO_VERSION" "$(go_required_version)"; then
      ok "go $GO_VERSION ($GO)"
    else
      warn "go $GO_VERSION is older than the required $(go_required_version) ($GO)"
    fi
  else
    warn "no go toolchain on PATH (this script installs one on 'start')"
  fi
  if resolve_node; then
    ok "node $(node_version), pnpm $("$PNPM" --version 2>/dev/null || echo '?')"
  else
    warn "pnpm not found"
  fi
  local c
  for c in git make curl rsync; do
    if have "$c"; then ok "$c $(command -v "$c")"; else warn "$c missing"; fi
  done
  if have docker; then ok "docker $(docker --version 2>/dev/null | head -n1)"; else dim "docker not installed"; fi
  if [ -f "$ENV_FILE" ]; then ok "env file $ENV_FILE"; else fail "env file $ENV_FILE missing"; fi
  load_env
  if [ -n "${JWT_SECRET:-}" ]; then ok "JWT_SECRET set"; else fail "JWT_SECRET is empty in $ENV_FILE — generate one: openssl rand -hex 32"; fi
  info "DATABASE_URL   $DB_USER@$DB_HOST:$DB_PORT/$DB_NAME"
  if db_report >/dev/null; then
    ok "database $(db_report)"
  else
    warn "database $(db_report || true)"
    info "start brings it up when it is a local container/cluster, otherwise fix DATABASE_URL"
  fi
  local p
  for p in "$BACKEND_PORT" "$WEB_PORT"; do
    local owner; owner="$(port_owner "$p")"
    if [ -z "$owner" ]; then
      ok "port $p free"
    else
      warn "port $p busy: docker=$(docker_container_on_port "$p") pid=$(printf '%s' "$owner" | cut -d' ' -f1)"
    fi
  done
  local need=0 f
  for f in "$SERVER_BIN/server" "$SERVER_BIN/migrate" "$SERVER_BIN/multica" "$WEB_DIR/.next"; do
    [ -e "$f" ] || { warn "missing $f (a build is required)"; need=1; }
  done
  local avail; avail="$(df -Pk "$ROOT" | awk 'NR==2{print $4}')"
  if [ "${avail:-0}" -ge 1048576 ]; then
    ok "free disk $((avail / 1024)) MB"
  else
    warn "less than 1 GB free on $ROOT"
  fi
  if [ "$need" = 0 ]; then ok "build artifacts present"; else info "run './start.sh build' to create them"; fi
  return 0
}

cmd_bootstrap() {
  mkdir -p "$LOG_DIR" "$RUN_DIR"
  step "bootstrap"
  ensure_go
  ensure_node
  load_env
  db_ensure
  deploy
  ok "host is ready; start the app with './start.sh start'"
}

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
}

main() {
  local cmd="${1:-start}"
  if [ $# -gt 0 ]; then shift; fi
  case "$cmd" in
    start)     cmd_start "$@" ;;
    stop)      cmd_stop "$@" ;;
    restart)   cmd_restart "$@" ;;
    status)    cmd_status "$@" ;;
    health)    cmd_health "$@" ;;
    preflight) cmd_preflight "$@" ;;
    build)     cmd_build "$@" ;;
    migrate)   cmd_migrate "$@" ;;
    bootstrap) cmd_bootstrap "$@" ;;
    logs)      cmd_logs "$@" ;;
    daemon)    cmd_daemon "$@" ;;
    nginx)     cmd_nginx "$@" ;;
    ensure)    cmd_ensure "$@" ;;
    autostart) cmd_autostart "$@" ;;
    help|-h|--help) usage ;;
    *) fail "unknown command '$cmd' (try '$0 help')" ;;
  esac
}

main "$@"
