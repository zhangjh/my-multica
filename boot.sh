#!/usr/bin/env bash
# Convenience entry point for the box: build if needed, start everything,
# health-check, done. Equivalent to `./start.sh start`.
set -euo pipefail
ROOT="${MULTICA_ROOT:-$HOME/multica}"
exec "$ROOT/start.sh" start "$@"
