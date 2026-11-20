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
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output)
      output=$2
      shift 2
      ;;
    --connect-timeout|--max-time)
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
case "$url" in
  *status)
    case ${TEST_SCENARIO:?} in
      active) printf '{"active_session_id":"session"}\n' ;;
      busy) printf '{"active_session_id":""}\n' ;;
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
    if [[ $TEST_SCENARIO == busy || ($TEST_SCENARIO != down && ! -s $TEST_SYSTEMCTL_LOG) ]]; then
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
  exit 0
fi
printf '%s\n' "$*" >>"$TEST_SYSTEMCTL_LOG"
# busy: the first restart starts the new binary, which keeps running, and then fails, as a unit
# whose process started but whose start job failed; update.sh then puts the previous binary back
# over a file that is executing. A later restart (the rollback's) succeeds.
if [[ $TEST_SCENARIO == busy && "$*" == '--user restart ghost-wispr.service' && ! -e $TEST_BUSY_PID ]]; then
  "$GHOST_WISPR_DEPLOY_DIR/ghost-wispr" 60 >/dev/null 2>&1 &
  printf '%s\n' "$!" >"$TEST_BUSY_PID"
  sleep 0.2
  exit 1
fi
# recording-new: the new binary starts and records a session, but its start job fails.
if [[ $TEST_SCENARIO == recording-new && "$*" == '--user restart ghost-wispr.service' && ! -e $TEST_STATE/changed ]]; then
  : >"$TEST_STATE/changed"
  exit 1
fi
EOF
chmod +x "$work_dir/bin/systemctl"

run_update() {
  PATH="$work_dir/bin:$PATH" \
    TEST_SCENARIO=$1 \
    TEST_BINARY="${TEST_BINARY_SOURCE:-$binary_source}" \
    TEST_CHECKSUM="${TEST_BINARY_CHECKSUM:-$binary_checksum}" \
    TEST_COMMIT="$commit" \
    TEST_SYSTEMCTL_LOG="$work_dir/systemctl.log" \
    TEST_BUSY_PID="$work_dir/busy.pid" \
    TEST_STATE="$work_dir/state" \
    GHOST_WISPR_DEPLOY_DIR="$work_dir/deploy" \
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
  rm -rf -- "$work_dir/state" "$work_dir/deploy/ghost-wispr.prev"
  mkdir -p "$work_dir/state"
  cp /bin/true "$work_dir/deploy/ghost-wispr"
  : >"$work_dir/systemctl.log"
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
printf 'update tests passed\n'
