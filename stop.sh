#!/usr/bin/env bash
set -uo pipefail

LOG="$HOME/multica-data/logs"

kill_tree() {
  local pid="$1"
  kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.5
  done
  kill -9 "$pid" 2>/dev/null || true
}

for name in nginx frontend backend; do
  pidfile="$LOG/$name.pid"
  if [ -f "$pidfile" ]; then
    pid="$(cat "$pidfile")"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      echo "[$name] stopping (pid $pid)"
      kill_tree "$pid"
    fi
    rm -f "$pidfile"
  fi
done

port_pid=$(ss -ltnp 2>/dev/null | grep ':3001 ' | grep -oP 'pid=\K[0-9]+' | tail -1)
if [ -n "$port_pid" ]; then
  echo "[frontend] scavenging leftover listener on :3001 (pid $port_pid)"
  kill_tree "$port_pid"
fi

echo "postgres left running (data in ~/multica-data/pgdata)."
echo "To stop it: $HOME/apps/multica-pg/bin/pg_ctl -D $HOME/multica-data/pgdata stop"