#!/bin/sh
# The fixed start of the nightly update, which ghost-wispr-update.service runs and only provision.sh
# installs, so no release can break it. It runs update.sh under a time limit, the one limit on a
# whole run, and counts the runs that failed in a row. It runs update.sh.prev at once after an
# update.sh that broke (an exit other than 0 or 10, the limit's 124 and 137 included), and once when
# 11 runs in a row failed, one night of the timer's runs; then the count starts again at 0.
dir=${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}
limit=${GHOST_WISPR_UPDATE_LIMIT:-45m}
count_file=$dir/update.failed-runs

run() {
  timeout --kill-after=60s "$limit" "$1"
}

echo "ghost-wispr launcher: invocation ${INVOCATION_ID:-none} trigger ${TRIGGER_UNIT:-none} at ${TRIGGER_TIMER_REALTIME_USEC:-none}"
run "$dir/update.sh"
status=$?
ran_prev=false
if [ "$status" -ne 0 ] && [ "$status" -ne 10 ] && [ -f "$dir/update.sh.prev" ]; then
  echo "ghost-wispr launcher: update.sh exited $status; running update.sh.prev"
  run "$dir/update.sh.prev"
  status=$?
  ran_prev=true
fi
count=$(cat "$count_file" 2>/dev/null) || count=0
case $count in '' | *[!0-9]*) count=0 ;; esac
if [ "$status" -eq 0 ]; then count=0; else count=$((count + 1)); fi
if [ "$count" -ge 11 ] && [ -f "$dir/update.sh.prev" ]; then
  if [ "$ran_prev" = false ]; then
    echo "ghost-wispr launcher: $count runs in a row failed; running update.sh.prev"
    run "$dir/update.sh.prev"
    status=$?
  fi
  count=0
fi
echo "$count" >"$count_file.tmp" && mv -f "$count_file.tmp" "$count_file"
exit "$status"
