#!/usr/bin/env bash
# Covers the self-host daemon supervision path that start.sh and the systemd
# units own: the boot unit's readiness contract, and what `start` reports when
# the daemon it starts cannot be started.
#
# Run directly: bash scripts/selfhost-daemon-supervision.test.sh
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

pass() { printf 'ok  %s\n' "$*"; }

require_contains() {
  local file=$1 expected=$2
  grep -Fq "$expected" "$file" || fail "$(basename "$file") does not contain: $expected"
}

require_absent() {
  local file=$1 unexpected=$2
  if grep -Fq "$unexpected" "$file"; then
    echo "Observed:" >&2
    cat "$file" >&2
    fail "$(basename "$file") unexpectedly contains: $unexpected"
  fi
}

# start.sh ends with an unconditional `main "$@"`, which would run a whole
# deploy. Source a copy with that call removed so individual functions can be
# driven directly against a stub CLI.
load_start_sh() {
  local dst=$1
  sed '/^main "\$@"$/d' "$root_dir/start.sh" >"$dst"
  # shellcheck disable=SC1090
  MULTICA_DATA="$tmp_dir/data" MULTICA_CLI_DEST="$stub_cli" . "$dst"
}

# A CLI stub whose exit status is what each case needs.
write_stub_cli() {
  cat >"$stub_cli" <<STUB
#!/usr/bin/env bash
case "\$1 \$2" in
  "daemon status")  printf '%s\n' '${1:-{"status":"stopped"}}' ;;
  "daemon start")   exit ${2:-0} ;;
  "daemon restart") exit ${3:-0} ;;
  *) exit 0 ;;
esac
STUB
  chmod +x "$stub_cli"
}

stub_cli="$tmp_dir/bin/multica"
mkdir -p "$(dirname "$stub_cli")" "$tmp_dir/data/logs"

###############################################################################
# The boot units must keep the daemon alive once they have started it.
###############################################################################

# multica-ensure.service starts the daemon and then exits. Without
# RemainAfterExit systemd tears the unit down on that exit and SIGTERMs
# everything left in its control group — and `multica daemon start` only
# Setsid()s, which moves the child out of the session and process group but
# leaves it in the cgroup. Observed: a daemon that reported itself started was
# dead a few hundred milliseconds later, restarted by every self-heal tick, and
# never settling until someone restarted it by hand.
ensure_unit="$root_dir/deploy/systemd/multica-ensure.service"
require_contains "$ensure_unit" "RemainAfterExit=yes"
pass "multica-ensure.service keeps its control group after the check finishes"

# The user-scope install rewrites the rendered unit (dropping User=/Group=), so
# the property has to survive rendering too — not just sit in the template.
rendered="$tmp_dir/rendered.txt"
"$root_dir/start.sh" autostart render user >"$rendered" 2>/dev/null ||
  fail "start.sh autostart render user exited non-zero"
for unit in multica.service multica-ensure.service; do
  section="$tmp_dir/$unit.rendered"
  sed -n "/^# ===== $unit /,/^# ===== /p" "$rendered" | sed '$d' >"$section"
  require_contains "$section" "RemainAfterExit=yes"
done
pass "the rendered user units keep RemainAfterExit=yes"

###############################################################################
# start must not report a daemon it failed to start.
###############################################################################

# `multica daemon start` failing used to print "! daemon start failed" and then
# "✓ daemon started" anyway, and still exited 0 — so a boot that came up with no
# agent runtime at all was logged, and reported to systemd by multica.service, as
# a successful start.
out="$tmp_dir/start_daemon.out"
write_stub_cli '{"status":"stopped"}' 1 0
(
  load_start_sh "$tmp_dir/start.sh"
  start_daemon
) >"$out" 2>&1 && rc=0 || rc=$?
[ "$rc" -ne 0 ] || fail "start_daemon returned 0 after the daemon failed to start"
require_contains "$out" "daemon start failed"
require_absent "$out" "✓ daemon started"
pass "a failed start is reported as a failure and returns non-zero"

write_stub_cli '{"status":"stopped"}' 0 0
out="$tmp_dir/start_daemon_ok.out"
(
  load_start_sh "$tmp_dir/start.sh"
  start_daemon
) >"$out" 2>&1 || fail "start_daemon returned non-zero for a successful start"
require_contains "$out" "✓ daemon started"
pass "a successful start is still reported as one"

# The restart branch had the same defect, and this one is worse for the operator
# to diagnose: it claims the new build is loaded when the old build is still the
# one running.
out="$tmp_dir/restart.out"
write_stub_cli '{"status":"running"}' 0 1
(
  load_start_sh "$tmp_dir/start.sh"
  MULTICA_RESTART_DAEMON=1 start_daemon
) >"$out" 2>&1 && rc=0 || rc=$?
[ "$rc" -ne 0 ] || fail "start_daemon returned 0 after the daemon failed to restart"
require_contains "$out" "daemon restart failed"
require_absent "$out" "✓ daemon restarted"
pass "a failed restart is reported as a failure and returns non-zero"

# Without the opt-in, a running daemon is left alone and that is still success.
out="$tmp_dir/left_alone.out"
write_stub_cli '{"status":"running"}' 0 0
(
  load_start_sh "$tmp_dir/start.sh"
  start_daemon
) >"$out" 2>&1 || fail "leaving a running daemon alone must succeed"
require_contains "$out" "daemon already running"
pass "a running daemon is still left alone by a plain start"

###############################################################################
# ... and cmd_start has to carry that non-zero out to the caller, or multica.service
# keeps reporting success.
###############################################################################

# Everything cmd_start touches before the daemon is stubbed out: the deploy and
# the app processes are not what this test is about.
stub_deploy_path() {
  load_env() { :; }
  db_ensure() { :; }
  ensure_go() { :; }
  ensure_node() { :; }
  needs_build() { return 1; }
  head_commit() { echo deadbeef; }
  deploy() { fail "deploy must not run in this test"; }
  stop_all() { fail "stop_all must not run in this test"; }
  start_backend() { :; }
  web_enabled() { return 1; }
  web_skip_notice() { :; }
  nginx_reload() { :; }
  acquire_lock() { :; }
  cmd_status() { echo "status block printed"; }
}

out="$tmp_dir/cmd_start.out"
write_stub_cli '{"status":"stopped"}' 1 0
(
  load_start_sh "$tmp_dir/start.sh"
  stub_deploy_path
  APP_MODE=native
  cmd_start
) >"$out" 2>&1 && rc=0 || rc=$?
[ "$rc" -ne 0 ] || fail "cmd_start returned 0 with no agent runtime on the box"
require_contains "$out" "daemon start failed"
# The status block still prints: it is what tells the operator what did come up.
require_contains "$out" "status block printed"
pass "cmd_start exits non-zero but still reports what came up"

out="$tmp_dir/cmd_start_ok.out"
write_stub_cli '{"status":"stopped"}' 0 0
(
  load_start_sh "$tmp_dir/start.sh"
  stub_deploy_path
  APP_MODE=native
  cmd_start
) >"$out" 2>&1 || fail "cmd_start returned non-zero for a clean start"
require_contains "$out" "status block printed"
pass "cmd_start still exits 0 on a clean start"

echo "All self-host daemon supervision checks passed."
