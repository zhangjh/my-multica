#!/usr/bin/env bash
set -euo pipefail

ROOT="${MULTICA_ROOT:-$HOME/multica}"
PGRUN="${MULTICA_PGDATA:-$HOME/multica-data/pgdata}"
LOG="$HOME/multica-data/logs"
MULTICA_BIN="$ROOT/server/bin"
MARKER="$LOG/deployed-commit"
CLI_DEST="$HOME/.local/bin/multica"
PGPORT="${MULTICA_PGPORT:-5432}"

export PATH="$HOME/.local/bin:$HOME/apps/multica-pg/bin:$HOME/go/go/bin:$PATH"
node_bin=""
for d in "$HOME"/.nvm/versions/node/*/bin; do
  [ -d "$d" ] || continue
  if [ -z "$node_bin" ] || [ "$d" \> "$node_bin" ]; then node_bin="$d"; fi
done
[ -n "$node_bin" ] && export PATH="$node_bin:$PATH"
export GOTOOLCHAIN=auto

mkdir -p "$LOG"

postgres_up() {
  if command -v pg_isready >/dev/null 2>&1; then
    pg_isready -q -h localhost -p "$PGPORT" >/dev/null 2>&1 && return 0
  fi
  command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":$PGPORT " && return 0
  return 1
}

resolve_pgctl() {
  PGCTL="${MULTICA_PG_CTL:-}"
  [ -n "$PGCTL" ] && [ -x "$PGCTL" ] && return 0
  PGCTL="$(command -v pg_ctl 2>/dev/null || true)"
  [ -n "$PGCTL" ] && return 0
  for cand in "$HOME/apps/multica-pg/bin/pg_ctl" "$HOME/.local/bin/pg_ctl" /usr/lib/postgresql/*/bin/pg_ctl /usr/local/pgsql/bin/pg_ctl; do
    if [ -x "$cand" ]; then PGCTL="$cand"; return 0; fi
  done
  return 1
}

if postgres_up; then
  echo "[postgres] already running on :$PGPORT"
elif resolve_pgctl; then
  echo "[postgres] starting on :$PGPORT"
  "$PGCTL" -D "$PGRUN" -l "$LOG/postgres.log" -o "-p $PGPORT" start
else
  echo "[postgres] nothing on :$PGPORT and no pg_ctl found"
  echo "          install postgres, or set MULTICA_PG_CTL=/path/to/pg_ctl (MULTICA_PGDATA for the data dir)"
  exit 1
fi

set -a; source "$ROOT/.env"; set +a
export FRONTEND_ORIGIN=${FRONTEND_ORIGIN:-http://localhost:3000}
export CORS_ALLOWED_ORIGINS=${CORS_ALLOWED_ORIGINS:-http://localhost:3000}
export REMOTE_API_URL=${REMOTE_API_URL:-http://localhost:8081}

head_commit=$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo unknown)
prev_commit=$(cat "$MARKER" 2>/dev/null || echo none)

DEPLOYED=0
if [ "${MULTICA_SKIP_BUILD:-}" = "1" ]; then
  echo "[deploy] MULTICA_SKIP_BUILD=1 -> starting existing build"
elif [ "${MULTICA_FORCE_BUILD:-}" = "1" ] || [ "$head_commit" != "$prev_commit" ]; then
  echo "[deploy] source changed since last deploy ($prev_commit -> $head_commit); rebuilding"
  cd "$ROOT"
  make build
  echo "[deploy] installing refreshed CLI to $CLI_DEST"
  cp -f "$MULTICA_BIN/multica" "$CLI_DEST"
  echo "[deploy] refreshing frontend .next (apps/web)"
  (
    pnpm install --frozen-lockfile --prefer-offline >/dev/null 2>&1 \
      || echo "[deploy][warn] pnpm install failed (offline?); continuing with existing node_modules"
  )
  pnpm --dir "$ROOT/apps/web" build 2>&1 | tee "$LOG/web-build.log"
  echo "[db] applying migrations"
  "$MULTICA_BIN/migrate" up
  echo "$head_commit" >"$MARKER"
  DEPLOYED=1
  echo "[deploy] build applied (release: $(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo $head_commit))"
else
  echo "[deploy] no source changes since last deploy (HEAD=$head_commit); starting existing build"
fi

if [ "$DEPLOYED" = "1" ]; then
  echo "[deploy] stopping existing services, then starting the new build"
  "$ROOT/stop.sh" || true
fi

start_svc() {
  local name="$1"; shift
  local port="${PORT_OVERRIDE:-}"; PORT_OVERRIDE=
  local pidfile="$LOG/$name.pid"
  local pid="" running=""
  if [ -f "$pidfile" ]; then pid=$(cat "$pidfile"); fi
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then running="$pid"; fi
  if [ -z "$running" ] && [ -n "$port" ]; then
    running=$(ss -ltnp 2>/dev/null | awk -v p=":$port" '$4 ~ p { if (match($0, /pid=[0-9]+/)) { print substr($0, RSTART+4, RLENGTH-4); exit } }')
  fi
  if [ -n "$running" ]; then
    echo "[$name] already running (pid $running)"
    echo "$running" >"$pidfile"
    return
  fi
  setsid nohup "$@" >>"$LOG/$name.log" 2>&1 < /dev/null &
  echo $! >"$pidfile"
  echo "[$name] starting (pid $(cat "$pidfile"))"
}

start_svc backend "$MULTICA_BIN/server"
PORT_OVERRIDE=3001 start_svc frontend bash -c 'cd "$HOME/multica/apps/web" && PORT=3001 REMOTE_API_URL=http://localhost:8081 exec pnpm start'
start_svc nginx /home/zhangjh/apps/nginx/sbin/nginx -c "$HOME/multica-data/nginx.conf"

if "$CLI_DEST" daemon status >/dev/null 2>&1; then
  echo "[daemon] already running (kept as-is; restart later to load the new build)"
else
  echo "[daemon] starting"
  "$CLI_DEST" daemon start >>"$LOG/daemon.log" 2>&1 || true
fi

echo "All services up. App: http://localhost:3000  API: http://localhost:8081"
echo "Public: https://multica.zhangjh.cn (via Cloudflare tunnel -> http://localhost:3000)"