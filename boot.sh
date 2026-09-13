#!/usr/bin/env bash
set -euo pipefail
export HOME="/home/zhangjh"
export PATH="$HOME/.local/bin:$HOME/apps/multica-pg/bin:$PATH"
LOG="$HOME/multica-data/logs"
mkdir -p "$LOG"

"$HOME/multica/start.sh"

for _ in $(seq 1 60); do
  if curl -fsS -o /dev/null http://localhost:8081/healthz 2>/dev/null; then
    break
  fi
  sleep 2
done

if ~/.local/bin/multica daemon status >/dev/null 2>&1; then
  echo "[daemon] already running"
else
  echo "[daemon] starting"
  ~/.local/bin/multica daemon start >>"$LOG/daemon.log" 2>&1 || true
fi

echo "boot.sh done at $(date)"