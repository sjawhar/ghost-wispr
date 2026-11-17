#!/usr/bin/env bash
# Checks deploy/pi/files/wait-for-time-sync.sh, which holds Ghost Wispr's start until the Pi's
# clock is synchronized: it returns at once when the clock is, waits for the mark systemd-timesyncd
# leaves once it synchronizes, and past its time limit lets the service start anyway, saying so.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root script="$repo_root/deploy/pi/files/wait-for-time-sync.sh"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
readonly mark="$work_dir/synchronized"

fail() {
  printf 'wait-for-time-sync test FAILED: %s\n' "$*" >&2
  exit 1
}

[[ -x $script ]] || fail "$script is missing or not executable"

# run_wait runs the wait with time limit $1 and prints how many seconds it took.
run_wait() {
  local started
  started=$(date +%s)
  GHOST_WISPR_TIMESYNC_MARK="$mark" GHOST_WISPR_TIMESYNC_TIMEOUT="$1" "$script" 2>"$work_dir/err" \
    || fail "the wait exited $? with a limit of $1 s: $(<"$work_dir/err")"
  printf '%s\n' $(($(date +%s) - started))
}

# The clock is synchronized: the service starts at once.
touch "$mark"
elapsed=$(run_wait 30)
((elapsed <= 2)) || fail "the wait took ${elapsed}s with the clock synchronized"

# The clock synchronizes while it waits: the service starts soon after.
rm -f -- "$mark"
(sleep 2 && touch "$mark") &
elapsed=$(run_wait 30)
wait
((elapsed >= 2 && elapsed <= 6)) || fail "the wait took ${elapsed}s with the clock synchronized 2 s in"

# The clock never synchronizes (no time server reachable): the wait gives up at its limit and the
# service starts all the same, so the unit still records, and it says why its times may be wrong.
rm -f -- "$mark"
elapsed=$(run_wait 2)
((elapsed >= 2 && elapsed <= 5)) || fail "the wait took ${elapsed}s with a limit of 2 s and no synchronization"
grep -q 'not synchronized' "$work_dir/err" \
  || fail "the wait did not say the service starts with the clock not synchronized: $(<"$work_dir/err")"

printf 'wait-for-time-sync test passed\n'
