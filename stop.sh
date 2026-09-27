#!/usr/bin/env bash
# Thin wrapper kept for backwards compatibility — the logic now lives in
# start.sh, which refuses to kill anything it did not start itself.
set -uo pipefail
ROOT="${MULTICA_ROOT:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)}"
exec "$ROOT/start.sh" stop "$@"
