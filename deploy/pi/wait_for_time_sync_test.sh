#!/usr/bin/env bash
# Checks deploy/pi/files/wait-for-time-sync.sh, which holds Ghost Wispr's start until the Pi's
# clock is synchronized: it returns at once when the clock is, waits for the mark systemd-timesyncd
# leaves once it synchronizes, and past its time limit lets the service start anyway, saying so. It
# waits that limit once per boot: a restart in the same boot, after it gave up, does not wait again.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root script="$repo_root/deploy/pi/files/wait-for-time-sync.sh"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
readonly mark="$work_dir/synchronized" waited="$work_dir/waited"

fail() {
  printf 'wait-for-time-sync test FAILED: %s\n' "$*" >&2
  exit 1
}

[[ -x $script ]] || fail "$script is missing or not executable"

# run_wait runs the wait with time limit $1 and prints how many seconds it took.
run_wait() {
  local started
  started=$(date +%s)
  GHOST_WISPR_TIMESYNC_MARK="$mark" GHOST_WISPR_TIMESYNC_WAITED="$waited" GHOST_WISPR_TIMESYNC_TIMEOUT="$1" "$script" 2>"$work_dir/err" \
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

# Review of #76, P3: the service restarts in the same boot, the clock still not synchronized. The
# wait already gave up this boot, so the service starts at once, and says why.
elapsed=$(run_wait 30)
((elapsed <= 2)) || fail "a restart in the same boot waited ${elapsed}s again after the wait gave up"
grep -q 'gave up' "$work_dir/err" \
  || fail "the restart did not say it starts without waiting because the wait gave up this boot: $(<"$work_dir/err")"

# A new boot: /run is empty again, the mark of the earlier wait is gone, and it waits again.
rm -f -- "$waited"
elapsed=$(run_wait 2)
((elapsed >= 2 && elapsed <= 5)) || fail "after a new boot the wait took ${elapsed}s with a limit of 2 s, want it to wait again"

printf 'wait-for-time-sync test passed\n'
