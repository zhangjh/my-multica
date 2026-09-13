#!/usr/bin/env bash
set -euo pipefail

export PATH="$HOME/.local/bin:$HOME/apps/multica-pg/bin:$PATH"

PGRUN="$HOME/multica-data/pgdata"
LOG="$HOME/multica-data/logs"
MULTICA_BIN="$HOME/multica/server/bin"

mkdir -p "$LOG"

if ! pg_ctl -D "$PGRUN" status >/dev/null 2>&1; then
  echo "[postgres] starting on 5432"
  pg_ctl -D "$PGRUN" -l "$LOG/postgres.log" -o "-p 5432" start
else
  echo "[postgres] already running"
fi

set -a; source "$HOME/multica/.env"; set +a
export FRONTEND_ORIGIN=${FRONTEND_ORIGIN:-http://localhost:3000}
export CORS_ALLOWED_ORIGINS=${CORS_ALLOWED_ORIGINS:-http://localhost:3000}

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

echo "All services up. App: http://localhost:3000  API: http://localhost:8081"
echo "Public: https://multica.zhangjh.cn (via Cloudflare tunnel -> http://localhost:3000)"