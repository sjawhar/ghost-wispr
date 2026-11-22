#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
readonly update_script="$script_dir/update.sh"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

readonly commit=0123456789abcdef0123456789abcdef01234567
readonly binary_source="$work_dir/new-binary"

cp /bin/true "$binary_source"
binary_checksum=$(sha256sum "$binary_source" | cut -d' ' -f1)
readonly binary_checksum
mkdir -p "$work_dir/bin" "$work_dir/deploy"
cp /bin/true "$work_dir/deploy/ghost-wispr"
: >"$work_dir/systemctl.log"

cat >"$work_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
output=
url=
max_time=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output)
      output=$2
      shift 2
      ;;
    --max-time)
      max_time=$2
      shift 2
      ;;
    --connect-timeout)
      shift 2
      ;;
    --fail|--silent|--show-error|--location)
      shift
      ;;
    *)
      url=$1
      shift
      ;;
  esac
done
# TEST_SLOW=1: each call takes all of its --max-time on the test's clock, as a call at its worst.
if [[ ${TEST_SLOW:-} == 1 ]]; then
  echo $(($(<"$TEST_CLOCK") + max_time)) >"$TEST_CLOCK"
fi
case "$url" in
  *status)
    case ${TEST_SCENARIO:?} in
      active) printf '{"paused":false,"capture_state":"recording","active_session_id":"session"}\n' ;;
      busy) printf '{"paused":true,"capture_state":"paused","active_session_id":""}\n' ;;
      # A recorder paused and idle until the binary's download (resumed, unpaused) or the new
      # binary's start (recording-new), then recording a new session or capturing.
      resumed | recording-new)
        if [[ -e ${TEST_STATE:?}/changed ]]; then
          printf '{"paused":false,"capture_state":"recording","active_session_id":"new-session"}\n'
        else
          printf '{"paused":true,"capture_state":"paused","active_session_id":""}\n'
        fi
        ;;
      unpaused)
        if [[ -e ${TEST_STATE:?}/changed ]]; then
          printf '{"paused":false,"capture_state":"capturing","active_session_id":""}\n'
        else
          printf '{"paused":true,"capture_state":"paused","active_session_id":""}\n'
        fi
        ;;
      # A recorder paused and idle until the new binary's start, whose start job fails while the
      # unit stays active, and whose /api/status then does not answer (unreadable), answers no
      # status (garbage), answers again after one unanswered read (unreadable-then-idle), or is
      # never read, the unit being stopped (stopped-new). never-answers-then-idle starts, never
      # reports its commit, and then answers as unreadable-then-idle does.
      unreadable | garbage | unreadable-then-idle | stopped-new | killed-after-swap | never-answers | never-answers-then-idle)
        if [[ ! -e ${TEST_STATE:?}/changed ]]; then
          printf '{"paused":true,"capture_state":"paused","active_session_id":""}\n'
          exit 0
        fi
        reads=$(($(cat "$TEST_STATE/reads" 2>/dev/null || echo 0) + 1))
        echo "$reads" >"$TEST_STATE/reads"
        case $TEST_SCENARIO in
          unreadable) exit 7 ;;
          garbage) printf '<html>502 Bad Gateway</html>\n' ;;
          unreadable-then-idle | never-answers-then-idle)
            ((reads > 1)) || exit 7
            printf '{"paused":true,"capture_state":"paused","active_session_id":""}\n'
            ;;
          stopped-new) printf 'a stopped recorder was asked for its status\n' >&2; exit 1 ;;
        esac
        ;;
    esac
    ;;
  *releases/latest)
    printf '{"target_commitish":"%s","assets":[{"name":"ghost-wispr-linux-arm64","browser_download_url":"https://test.invalid/binary"},{"name":"ghost-wispr-linux-arm64.sha256","browser_download_url":"https://test.invalid/checksum"}]}' "$TEST_COMMIT"
    ;;
  *binary)
    cp "$TEST_BINARY" "$output"
    [[ $TEST_SCENARIO != resumed && $TEST_SCENARIO != unpaused ]] || : >"${TEST_STATE:?}/changed"
    ;;
  *checksum)
    printf '%s  ghost-wispr-linux-arm64\n' "$TEST_CHECKSUM" >"$output"
    ;;
  *version)
    if [[ ${TEST_SCENARIO:?} == down && ! -s ${TEST_SYSTEMCTL_LOG:?} ]]; then
      exit 7
    fi
    # busy, and the rest until a restart: never the release, so the install goes ahead.
    # never-answers: a new binary that starts but never reports its commit.
    if [[ $TEST_SCENARIO == busy || $TEST_SCENARIO == never-answers || $TEST_SCENARIO == never-answers-then-idle || ($TEST_SCENARIO != down && ! -s $TEST_SYSTEMCTL_LOG) ]]; then
      printf '{"commit":"%s"}\n' "previous"
      exit 0
    fi
    printf '{"commit":"%s"}\n' "$TEST_COMMIT"
    ;;
  *)
    printf 'unexpected curl URL: %s\n' "$url" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$work_dir/bin/curl"
cat >"$work_dir/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == '--user is-active --quiet ghost-wispr.service' ]]; then
  [[ ${TEST_SCENARIO:?} == down ]] && exit 3
  [[ $TEST_SCENARIO == stopped-new && -e $TEST_STATE/changed ]] && exit 3
  exit 0
fi
# The recorder's main process: TEST_MAIN_PID, whose image is the fake /proc's <pid>/exe.
if [[ "$*" == '--user show -p MainPID --value ghost-wispr.service' ]]; then
  echo "${TEST_MAIN_PID:-4242}"
  exit 0
fi
printf '%s\n' "$*" >>"$TEST_SYSTEMCTL_LOG"
# A restart takes TEST_RESTART_SECONDS of the test's clock: the recorder's stop and start.
if [[ "$*" == '--user restart ghost-wispr.service' ]]; then
  echo $(($(<"$TEST_CLOCK") + ${TEST_RESTART_SECONDS:-0})) >"$TEST_CLOCK"
fi
# killed-after-swap: systemd kills update.sh (SIGKILL, as a power cut or a stop past its grace
# would) during the new binary's restart.
if [[ $TEST_SCENARIO == killed-after-swap && "$*" == '--user restart ghost-wispr.service' ]]; then
  kill -KILL "$PPID"
  exit 1
fi
# busy: the first restart starts the new binary, which keeps running, and then fails, as a unit
# whose process started but whose start job failed; update.sh then puts the previous binary back
# over a file that is executing. A later restart (the rollback's) succeeds.
if [[ $TEST_SCENARIO == busy && "$*" == '--user restart ghost-wispr.service' && ! -e $TEST_BUSY_PID ]]; then
  "$GHOST_WISPR_DEPLOY_DIR/ghost-wispr" 60 >/dev/null 2>&1 &
  printf '%s\n' "$!" >"$TEST_BUSY_PID"
  sleep 0.2
  exit 1
fi
# recording-new, unreadable, garbage, unreadable-then-idle and stopped-new: the new binary's start
# job fails (it starts and records a session, or stops answering, or the unit stops).
if [[ $TEST_SCENARIO =~ ^(recording-new|unreadable|garbage|unreadable-then-idle|stopped-new)$ && "$*" == '--user restart ghost-wispr.service' && ! -e $TEST_STATE/changed ]]; then
  : >"$TEST_STATE/changed"
  exit 1
fi
# never-answers-then-idle: the new binary's start job succeeds.
if [[ $TEST_SCENARIO == never-answers-then-idle && "$*" == '--user restart ghost-wispr.service' && ! -e $TEST_STATE/changed ]]; then
  : >"$TEST_STATE/changed"
fi
EOF
chmod +x "$work_dir/bin/systemctl"
# sleep notes each wait of update.sh and moves the test's clock on by it, at once; the fakes' own
# short waits still sleep.
cat >"$work_dir/bin/sleep" <<'EOF'
#!/usr/bin/env bash
case $1 in *.*) exec /bin/sleep "$@" ;; esac
printf 'sleep %s\n' "$*" >>"$TEST_SLEEP_LOG"
echo $(($(<"$TEST_CLOCK") + $1)) >"$TEST_CLOCK"
EOF
chmod +x "$work_dir/bin/sleep"
# date +%s reads the test's clock, which only sleep and restarts move.
cat >"$work_dir/bin/date" <<'EOF'
#!/usr/bin/env bash
[[ $* == +%s ]] || exec /bin/date "$@"
cat "$TEST_CLOCK"
EOF
chmod +x "$work_dir/bin/date"

# The fake /proc: the recorder's main process (4242) runs the installed binary, unless a test points
# its exe elsewhere (running_image).
mkdir -p "$work_dir/proc/4242"
running_image() { ln -sfn "$1" "$work_dir/proc/4242/exe"; }
running_image "$work_dir/deploy/ghost-wispr"

run_update() {
  echo 1000 >"$work_dir/clock"
  PATH="$work_dir/bin:$PATH" \
    TEST_SCENARIO=$1 \
    TEST_BINARY="${TEST_BINARY_SOURCE:-$binary_source}" \
    TEST_CHECKSUM="${TEST_BINARY_CHECKSUM:-$binary_checksum}" \
    TEST_COMMIT="$commit" \
    TEST_SYSTEMCTL_LOG="$work_dir/systemctl.log" \
    TEST_SLEEP_LOG="$work_dir/sleep.log" \
    TEST_BUSY_PID="$work_dir/busy.pid" \
    TEST_CLOCK="$work_dir/clock" \
    TEST_RESTART_SECONDS="${restart_seconds:-0}" \
    TEST_SLOW="${slow:-}" \
    TEST_MAIN_PID="${main_pid:-4242}" \
    TEST_STATE="$work_dir/state" \
    GHOST_WISPR_DEPLOY_DIR="$work_dir/deploy" \
    GHOST_WISPR_PROC_DIR="$work_dir/proc" \
    GHOST_WISPR_RELEASE_API=https://test.invalid/releases/latest \
    GHOST_WISPR_STATUS_URL=https://test.invalid/status \
    GHOST_WISPR_VERSION_URL=https://test.invalid/version \
    GHOST_WISPR_UPDATE_PAUSED_ONLY="${paused_only:-}" \
    "$update_script"
}

run_update active
[[ ! -s "$work_dir/systemctl.log" ]] || {
  printf 'active session unexpectedly restarted the service\n' >&2
  exit 1
}

# A recorder that is down answers no version, so update.sh cannot tell that its binary works: with
# no verified binary recorded it keeps no .prev from it, and says why.
rm -f "$work_dir/deploy/ghost-wispr.verified"
out=$(run_update down 2>&1)
[[ ! -e "$work_dir/deploy/ghost-wispr.prev" && $out == *'not the binary verified last'* ]] || {
  printf 'a binary nobody verified was kept as the previous one: %s\n' "$out" >&2
  exit 1
}
[[ $(<"$work_dir/deploy/ghost-wispr.verified") == "$binary_checksum" ]] || {
  printf 'the binary verified by the install is not recorded as verified\n' >&2
  exit 1
}
# The same, with the deployed binary the one verified last: .prev keeps it.
: >"$work_dir/systemctl.log"
run_update down
grep -qx -- '--user restart ghost-wispr.service' "$work_dir/systemctl.log"
[[ -f "$work_dir/deploy/ghost-wispr.prev" ]] || {
  printf 'rollback copy was not retained\\n' >&2
  exit 1
}

# A rollback puts the previous binary back while the new one still runs: copying over a file that
# is executing fails with "Text file busy", which left the new binary in place.
busy_source="$work_dir/busy-binary"
cp /bin/sleep "$busy_source"
cp /bin/true "$work_dir/deploy/ghost-wispr"
rm -f "$work_dir/deploy/ghost-wispr.prev"
: >"$work_dir/systemctl.log"
status=0
out=$(TEST_BINARY_SOURCE="$busy_source" TEST_BINARY_CHECKSUM=$(sha256sum "$busy_source" | cut -d' ' -f1) \
  run_update busy 2>&1) || status=$?
busy_pid=$(<"$work_dir/busy.pid")
kill "$busy_pid" 2>/dev/null || true
[[ $status -ne 0 && $out == *'restored the previous binary'* ]] || {
  printf 'a rollback over the running new binary did not restore the previous one (exit %s): %s\n' "$status" "$out" >&2
  exit 1
}
cmp -s /bin/true "$work_dir/deploy/ghost-wispr" || {
  printf 'after the rollback the deployed binary is not the previous one\n' >&2
  exit 1
}

# A session, or (with GHOST_WISPR_UPDATE_PAUSED_ONLY=1, as deploy/pi/rollout.sh runs it) a resume,
# that starts during the download stops the update before the swap: the binary stays, and nothing
# restarts the recorder. A new binary whose start job fails, but that answers that it records a
# session, is not restarted for a rollback. Each scenario's new binary is /bin/false, so the
# deployed /bin/true shows whether the swap happened.
new_source="$work_dir/false-binary"
cp /bin/false "$new_source"
new_checksum=$(sha256sum "$new_source" | cut -d' ' -f1)
# update_with SCENARIO: runs update.sh on a fresh deployed /bin/true, with $paused_only; its output
# is in $out and its exit status in $status.
update_with() {
  rm -rf -- "$work_dir/state" "$work_dir/deploy/ghost-wispr.prev" "$work_dir/deploy/ghost-wispr.verified"
  mkdir -p "$work_dir/state"
  cp /bin/true "$work_dir/deploy/ghost-wispr"
  : >"$work_dir/systemctl.log"
  : >"$work_dir/sleep.log"
  status=0
  out=$(TEST_BINARY_SOURCE="$new_source" TEST_BINARY_CHECKSUM="$new_checksum" run_update "$1" 2>&1) || status=$?
}
untouched() {
  [[ $status -eq 0 && $out == *"$2; leaving the current binary in place"* ]] \
    || { printf '%s: exit %s, %s; want 0 and "%s; leaving the current binary in place"\n' "$1" "$status" "$out" "$2" >&2; exit 1; }
  [[ ! -s "$work_dir/systemctl.log" ]] || { printf '%s restarted the recorder\n' "$1" >&2; exit 1; }
  if ! cmp -s /bin/true "$work_dir/deploy/ghost-wispr" || [[ -e "$work_dir/deploy/ghost-wispr.prev" ]]; then
    printf '%s swapped the binary\n' "$1" >&2
    exit 1
  fi
}
paused_only='' update_with resumed
untouched 'a session that started during the download' 'active session'
paused_only=1 update_with unpaused
untouched 'a resume during the download, paused only' 'not paused'
# The control: without GHOST_WISPR_UPDATE_PAUSED_ONLY, a resume with no session installs.
paused_only='' update_with unpaused
if ! [[ $status -eq 0 && $out == *"installed commit $commit"* ]] || ! cmp -s "$new_source" "$work_dir/deploy/ghost-wispr"; then
  printf 'a resume with no session, nightly, did not install: exit %s, %s\n' "$status" "$out" >&2
  exit 1
fi
paused_only='' update_with recording-new
[[ $status -ne 0 && $out == *'active session'* && $out != *'restored the previous binary'* ]] \
  || { printf 'a new binary that records a session was rolled back: exit %s, %s\n' "$status" "$out" >&2; exit 1; }
cmp -s "$new_source" "$work_dir/deploy/ghost-wispr" || { printf 'the new binary that records a session was replaced\n' >&2; exit 1; }
[[ $(grep -cx -- '--user restart ghost-wispr.service' "$work_dir/systemctl.log") == 1 ]] \
  || { printf 'the recorder that records a session was restarted again: %s\n' "$(<"$work_dir/systemctl.log")" >&2; exit 1; }

# A new binary whose start job fails while its unit stays active, and whose /api/status then gives
# no status (no answer, or an answer that is not a status), may be recording: the rollback does not
# restart it. It reads the status again every 2 minutes, as pi-when-idle.sh does, and after the
# third read gives up with the new binary left running. One that answers again, idle, on two reads
# 2 minutes apart is rolled back; one whose unit has stopped is rolled back at once.
left_running() {
  [[ $status -ne 0 && $out == *'its state could not be read'* && $out == *'left running'* && $out != *'restored the previous binary'* ]] \
    || { printf '%s: exit %s, %s; want a failure that leaves it running\n' "$1" "$status" "$out" >&2; exit 1; }
  cmp -s "$new_source" "$work_dir/deploy/ghost-wispr" || { printf '%s: the running new binary was replaced\n' "$1" >&2; exit 1; }
  [[ $(grep -cx -- '--user restart ghost-wispr.service' "$work_dir/systemctl.log") == 1 ]] \
    || { printf '%s: the recorder was restarted again: %s\n' "$1" "$(<"$work_dir/systemctl.log")" >&2; exit 1; }
  [[ $(<"$work_dir/sleep.log") == $'sleep 120\nsleep 120' && $(<"$work_dir/state/reads") == 3 ]] \
    || { printf '%s: read the status %s times after waits %q; want 3 reads, 2 minutes apart\n' "$1" "$(<"$work_dir/state/reads")" "$(<"$work_dir/sleep.log")" >&2; exit 1; }
}
rolled_back() {
  [[ $status -ne 0 && $out == *'restored the previous binary'* ]] \
    || { printf '%s: exit %s, %s; want the previous binary restored\n' "$1" "$status" "$out" >&2; exit 1; }
  cmp -s /bin/true "$work_dir/deploy/ghost-wispr" || { printf '%s: the previous binary was not restored\n' "$1" >&2; exit 1; }
  [[ $(grep -cx -- '--user restart ghost-wispr.service' "$work_dir/systemctl.log") == 2 ]] \
    || { printf '%s: want the rollback'\''s restart: %s\n' "$1" "$(<"$work_dir/systemctl.log")" >&2; exit 1; }
  [[ $(<"$work_dir/sleep.log") == "$2" ]] \
    || { printf '%s: waited %q, want %q\n' "$1" "$(<"$work_dir/sleep.log")" "$2" >&2; exit 1; }
}
paused_only='' update_with unreadable
left_running 'a new binary that does not answer'
paused_only='' update_with garbage
left_running 'a new binary that answers no status'
paused_only='' update_with unreadable-then-idle
rolled_back 'a new binary that answers idle again' $'sleep 120\nsleep 120'
paused_only='' update_with stopped-new
rolled_back 'a new binary whose unit stopped' ''

# seconds UNIT KEY DEFAULT: KEY's time in the unit file UNIT, in seconds (5min, 90s, 90), or DEFAULT.
seconds() {
  local value
  value=$(sed -n "s/^$2=//p" "$1")
  case $value in
    '') value=$3 ;;
    *min) value=$((${value%min} * 60)) ;;
    *s) value=${value%s} ;;
  esac
  [[ $value =~ ^[0-9]+$ ]] || { printf 'cannot read %s from %s\n' "$2" "$1" >&2; exit 1; }
  printf '%s' "$value"
}

# The update unit (ghost-wispr-update.service) ends update.sh at its TimeoutStartSec, which update.sh
# knows as UNIT_LIMIT.
limit=$(seconds "$script_dir/files/ghost-wispr-update.service" TimeoutStartSec '')
script_limit=$(sed -n 's/^readonly UNIT_LIMIT=\([0-9][0-9]*\).*/\1/p' "$update_script")
[[ $script_limit == "$limit" ]] \
  || { printf "update.sh's UNIT_LIMIT (%s) is not the update unit's TimeoutStartSec (%s s)\n" "$script_limit" "$limit" >&2; exit 1; }
usable=$((limit - 60))

# A restart of the recorder (ghost-wispr.service) at its worst: its main process's stop (TimeoutStopSec,
# the user manager's 90 s by default), each ExecStopPost (bounded by TimeoutStopSec of its own, or
# by the `timeout` it runs under), and its start (TimeoutStartSec, its ExecStartPre waits included).
recorder_unit="$script_dir/files/ghost-wispr.service"
stop=$(seconds "$recorder_unit" TimeoutStopSec 90)
restart_worst=$((stop + $(seconds "$recorder_unit" TimeoutStartSec '')))
while IFS= read -r post; do
  if [[ $post =~ ^/usr/bin/timeout\ -k\ ([0-9]+)s?\ ([0-9]+)s?\  ]]; then
    restart_worst=$((restart_worst + BASH_REMATCH[1] + BASH_REMATCH[2]))
  else
    restart_worst=$((restart_worst + stop))
  fi
done < <(sed -n 's/^ExecStopPost=//p' "$recorder_unit")

# The new binary starts but never reports its commit; the rollback's first status read gets no
# answer and the next two find it idle (GPT-6.1 Sol's delayed-idle case at 25ff1c5), each step at
# its worst, each restart at the recorder's worst: 420 s of reads and downloads, the restart, the
# 120 s version wait, a read, a 120 s wait, a read, a wait, a read and the rollback's restart. Its
# waits must reserve the true restart, and the whole must end 60 s before the unit's limit: with
# light-off unbounded (90 s more per restart) and update.sh reserving 390 s, it ended at 1770 s.
slow=1 restart_seconds=$restart_worst paused_only='' update_with never-answers-then-idle
elapsed=$(($(<"$work_dir/clock") - 1000))
[[ $status -ne 0 && $out == *'version verification timed out; restored the previous binary'* ]] && cmp -s /bin/true "$work_dir/deploy/ghost-wispr" \
  || { printf 'the delayed-idle case: exit %s, %s; want the rollback\n' "$status" "$out" >&2; exit 1; }
((elapsed == 810 + 2 * restart_worst)) \
  || { printf 'the delayed-idle case took %s s, want %s s: a step changed, so check this worst case again\n' "$elapsed" "$((810 + 2 * restart_worst))" >&2; exit 1; }
((elapsed <= usable)) \
  || { printf "the delayed-idle case, with %s s restarts, takes %s s, past the update unit's %s s limit less 60 s\n" "$restart_worst" "$elapsed" "$limit" >&2; exit 1; }
script_restart=$(sed -n 's/^readonly .*RESTART_WORST=\([0-9][0-9]*\).*/\1/p' "$update_script")
[[ $script_restart == "$restart_worst" ]] \
  || { printf "update.sh's RESTART_WORST (%s s) is not the recorder unit's restart at its worst (%s s)\n" "$script_restart" "$restart_worst" >&2; exit 1; }

# A rollback waits for a recorder whose state it cannot read only while that wait, the read after
# it and a restart at the recorder's worst still end 60 s before the unit's limit; otherwise it
# gives up at once, the new binary left running, and says why. With an instant restart it waits
# twice (above). No real restart is slow enough to make it stop early, so these restarts take what
# puts the clock where the guard binds: one that leaves room for one wait and not a second, and one
# for none.
out_of_time() {
  [[ $status -ne 0 && $out == *'its state could not be read'* && $out == *"before the update unit's $limit s limit"* && $out != *'restored the previous binary'* ]] \
    || { printf '%s: exit %s, %s; want a failure that leaves it running for lack of time\n' "$1" "$status" "$out" >&2; exit 1; }
  cmp -s "$new_source" "$work_dir/deploy/ghost-wispr" || { printf '%s: the running new binary was replaced\n' "$1" >&2; exit 1; }
  [[ $(grep -cx -- '--user restart ghost-wispr.service' "$work_dir/systemctl.log") == 1 ]] \
    || { printf '%s: the recorder was restarted again: %s\n' "$1" "$(<"$work_dir/systemctl.log")" >&2; exit 1; }
  [[ $(<"$work_dir/sleep.log") == "$2" && $(<"$work_dir/state/reads") == "$3" ]] \
    || { printf '%s: %s reads after waits %q; want %s reads after %q\n' "$1" "$(<"$work_dir/state/reads")" "$(<"$work_dir/sleep.log")" "$3" "$2" >&2; exit 1; }
  (($(<"$work_dir/clock") - 1000 <= usable)) || { printf '%s: ended at %s s, past %s\n' "$1" "$(($(<"$work_dir/clock") - 1000))" "$usable" >&2; exit 1; }
}
restart_seconds=$((usable - script_restart - 190)) paused_only='' update_with unreadable
out_of_time 'a slow restart, then no status' 'sleep 120' 2
restart_seconds=$((usable - script_restart - 100)) paused_only='' update_with unreadable
out_of_time 'a slower restart, then no status' '' 1

# Every step at its worst: each curl takes its whole --max-time (status 10 s, release 30 s, version
# 10 s, download 300 s, checksum 60 s, status 10 s), the restart at the recorder's worst, the new
# binary never reports its commit (the 120 s version wait), the rollback reads the status (10 s)
# and restarts. Under the old 15 min limit it did not end in time, and systemd killed update.sh
# with the unverified binary in place. GPT-6.1 Sol's case at 1db0cbc (each HTTP call just under
# its --max-time, a 365 s restart, version reads that time out) is the second run.
for restart in "$restart_worst" 365; do
  slow=1 restart_seconds=$restart paused_only='' update_with never-answers
  elapsed=$(($(<"$work_dir/clock") - 1000))
  [[ $status -ne 0 && $out == *'version verification timed out; restored the previous binary'* ]] && cmp -s /bin/true "$work_dir/deploy/ghost-wispr" \
    || { printf 'every step at its worst, a %s s restart: exit %s, %s; want the rollback\n' "$restart" "$status" "$out" >&2; exit 1; }
  [[ $(grep -cx -- '--user restart ghost-wispr.service' "$work_dir/systemctl.log") == 2 ]] \
    || { printf 'every step at its worst, a %s s restart: want the rollback'\''s restart: %s\n' "$restart" "$(<"$work_dir/systemctl.log")" >&2; exit 1; }
  # 420 s of reads and downloads, the restart, the 120 s version wait, a 10 s status read, the
  # rollback's restart.
  want=$((550 + 2 * restart))
  ((elapsed == want)) || { printf 'every step at its worst, a %s s restart, took %s s, want %s s: a step changed, so check this worst case again\n' "$restart" "$elapsed" "$want" >&2; exit 1; }
  ((elapsed <= usable)) \
    || { printf "every step at its worst, a %s s restart, takes %s s, past the update unit's %s s limit less 60 s: systemd kills update.sh first\n" "$restart" "$elapsed" "$limit" >&2; exit 1; }
done

# An update killed after the swap (a power cut, or systemd's kill) leaves the new binary in place
# unverified. The next run must not take it for the previous binary: with a broken release, its
# rollback must put back the binary verified last.
broken_source="$work_dir/broken-binary"
printf '#!/bin/sh\nexit 3\n' >"$broken_source"
chmod +x "$broken_source"
rm -rf -- "$work_dir/state" "$work_dir/deploy/ghost-wispr.prev" "$work_dir/deploy/ghost-wispr.verified"
mkdir -p "$work_dir/state"
cp /bin/true "$work_dir/deploy/ghost-wispr"
: >"$work_dir/systemctl.log"
: >"$work_dir/sleep.log"
status=0
out=$(TEST_BINARY_SOURCE="$new_source" TEST_BINARY_CHECKSUM="$new_checksum" run_update killed-after-swap 2>&1) || status=$?
[[ $status -ne 0 ]] && cmp -s "$new_source" "$work_dir/deploy/ghost-wispr" && cmp -s /bin/true "$work_dir/deploy/ghost-wispr.prev" \
  || { printf 'control: the killed update did not leave the new binary in place and the old one as .prev: exit %s, %s\n' "$status" "$out" >&2; exit 1; }
rm -rf -- "$work_dir/state"
mkdir -p "$work_dir/state"
: >"$work_dir/systemctl.log"
status=0
out=$(TEST_BINARY_SOURCE="$broken_source" TEST_BINARY_CHECKSUM=$(sha256sum "$broken_source" | cut -d' ' -f1) \
  run_update stopped-new 2>&1) || status=$?
cmp -s /bin/true "$work_dir/deploy/ghost-wispr.prev" \
  || { printf 'after an update killed after its swap, the next run kept the unverified binary as .prev: %s\n' "$out" >&2; exit 1; }
[[ $status -ne 0 && $out == *'restored the previous binary'* && $out == *'not the binary verified last'* ]] && cmp -s /bin/true "$work_dir/deploy/ghost-wispr" \
  || { printf 'the rollback after an update killed after its swap did not restore the binary verified last: exit %s, %s\n' "$status" "$out" >&2; exit 1; }

# The first run with no record of a verified binary records the installed binary only when it is
# the image the recorder runs. An interrupted update can leave unverified B installed while A still
# runs and answers /api/version (GPT-6.1 Sol's case at 25ff1c5): B must not be recorded, and a
# later rollback must not restore it.
a_image="$work_dir/a-binary"
cp /bin/true "$a_image"
rm -rf -- "$work_dir/state" "$work_dir/deploy/ghost-wispr.verified"
mkdir -p "$work_dir/state"
cp /bin/true "$work_dir/deploy/ghost-wispr.prev"
cp "$new_source" "$work_dir/deploy/ghost-wispr" # B, installed and never verified
running_image "$a_image"
: >"$work_dir/systemctl.log"
status=0
out=$(TEST_BINARY_SOURCE="$broken_source" TEST_BINARY_CHECKSUM=$(sha256sum "$broken_source" | cut -d' ' -f1) \
  run_update stopped-new 2>&1) || status=$?
running_image "$work_dir/deploy/ghost-wispr"
if [[ -f "$work_dir/deploy/ghost-wispr.verified" && $(<"$work_dir/deploy/ghost-wispr.verified") == "$new_checksum" ]]; then
  printf 'the first run recorded installed B as verified while the recorder ran A: %s\n' "$out" >&2
  exit 1
fi
cmp -s /bin/true "$work_dir/deploy/ghost-wispr.prev" \
  || { printf 'with B installed and A running, .prev became B: %s\n' "$out" >&2; exit 1; }
[[ $out == *'is not the image the recorder runs'* ]] \
  || { printf 'the first run did not say why it recorded no verified binary: %s\n' "$out" >&2; exit 1; }
# The same when the running image cannot be read (systemd reports no main process): no record.
rm -rf -- "$work_dir/state" "$work_dir/deploy/ghost-wispr.verified"
mkdir -p "$work_dir/state"
cp /bin/true "$work_dir/deploy/ghost-wispr"
: >"$work_dir/systemctl.log"
status=0
out=$(main_pid=0 TEST_BINARY_SOURCE="$broken_source" TEST_BINARY_CHECKSUM=$(sha256sum "$broken_source" | cut -d' ' -f1) \
  run_update stopped-new 2>&1) || status=$?
[[ ! -e "$work_dir/deploy/ghost-wispr.verified" && $out == *'is not the image the recorder runs (main process 0)'* ]] \
  || { printf 'with no main process to read, the first run recorded a verified binary: %s\n' "$out" >&2; exit 1; }
printf 'update tests passed\n'
