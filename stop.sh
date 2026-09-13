#!/usr/bin/env bash
set -uo pipefail

LOG="$HOME/multica-data/logs"

for name in nginx frontend backend; do
  pidfile="$LOG/$name.pid"
  if [ -f "$pidfile" ]; then
    pid="$(cat "$pidfile")"
    if kill -0 "$pid" 2>/dev/null; then
      echo "[$name] stopping (pid $pid)"
      kill "$pid"
    fi
    rm -f "$pidfile"
  fi
done

echo "postgres left running (data in ~/multica-data/pgdata)."
echo "To stop it: $HOME/apps/multica-pg/bin/pg_ctl -D $HOME/multica-data/pgdata stop"