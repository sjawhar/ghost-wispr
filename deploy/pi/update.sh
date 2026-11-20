#!/usr/bin/env bash
# Install the newest verified Ghost Wispr release unless a session is active.
set -euo pipefail

readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly BINARY="$DEPLOY_DIR/ghost-wispr"
readonly PREVIOUS_BINARY="$DEPLOY_DIR/ghost-wispr.prev"
# The sha256 of the binary update.sh saw run and answer its commit last (record_verified). Only
# that binary becomes the previous binary: an update killed after its swap (a power cut, or the
# update unit's limit) leaves a binary nobody verified, which a later rollback must not restore.
readonly VERIFIED="$DEPLOY_DIR/ghost-wispr.verified"
readonly RELEASE_API="${GHOST_WISPR_RELEASE_API:-https://api.github.com/repos/sjawhar/ghost-wispr/releases/latest}"
readonly STATUS_URL="${GHOST_WISPR_STATUS_URL:-http://127.0.0.1:8080/api/status}"
readonly VERSION_URL="${GHOST_WISPR_VERSION_URL:-http://127.0.0.1:8080/api/version}"
# 1 (deploy/pi/rollout.sh sets it): install only while the recorder is paused, not only while it
# records no session.
readonly PAUSED_ONLY="${GHOST_WISPR_UPDATE_PAUSED_ONLY:-}"
# A rollback reads the status of a recorder whose state it could not read again every RECHECK_EVERY
# seconds, as pi-when-idle.sh does, and at most ROLLBACK_READS times.
readonly RECHECK_EVERY=120 ROLLBACK_READS=3
# The update unit (ghost-wispr-update.service) ends this script UNIT_LIMIT seconds after its start
# (TimeoutStartSec=30min; deploy/pi/update_test.sh checks the two agree). Every step at its worst
# ends well inside it: 10 s status read, 30 s release lookup, 10 s version read, 300 + 60 s
# download, 10 s status read, a 390 s restart (RESTART_WORST: the recorder unit's 90 s stop, the
# user manager's DefaultTimeoutStopSec, and its TimeoutStartSec=5min start), the 120 s version wait
# (its last read can start just before the end: 132 s), a 10 s status read and the rollback's 390 s
# restart: about 1342 s. A rollback whose recorder's state it cannot read waits for another read
# only while that wait (RECHECK_EVERY), the read (STATUS_READ, curl's --max-time) and a restart at
# its worst still end MARGIN seconds before the limit; otherwise it gives up, the new binary left
# running. With 15 min, as before, the steps alone could outlast the limit, and systemd killed
# update.sh with the new binary in place and no rollback.
readonly UNIT_LIMIT=1800 MARGIN=60 STATUS_READ=10 RESTART_WORST=390
STARTED=$(date +%s)
readonly STARTED

fail() {
  printf 'ghost-wispr update: %s\n' "$*" >&2
  exit 1
}

# record_verified records the installed binary as the one verified last (VERIFIED), by a rename,
# so a kill leaves the old record or the new one.
record_verified() {
  local sum
  sum=$(sha256sum "$BINARY" | cut -d' ' -f1)
  printf '%s\n' "$sum" >"$VERIFIED.tmp"
  mv -f "$VERIFIED.tmp" "$VERIFIED"
}

# keep_previous copies the installed binary to PREVIOUS_BINARY, by a rename, only when it is the
# binary verified last; otherwise PREVIOUS_BINARY stays as it is, and it says why.
keep_previous() {
  [[ -f "$BINARY" ]] || return 0
  if [[ ! -f "$VERIFIED" || $(sha256sum "$BINARY" | cut -d' ' -f1) != "$(<"$VERIFIED")" ]]; then
    printf 'ghost-wispr update: %s is not the binary verified last, so %s stays as it is\n' "$BINARY" "$PREVIOUS_BINARY"
    return 0
  fi
  cp --preserve=mode "$BINARY" "$PREVIOUS_BINARY.tmp"
  mv -f "$PREVIOUS_BINARY.tmp" "$PREVIOUS_BINARY"
}

# recorder_status prints the recorder's /api/status, or nothing when it does not answer, or answers
# something that is not its status (an object with active_session_id, paused and capture_state).
recorder_status() {
  local status
  status=$(curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$STATUS_URL" 2>/dev/null || true)
  jq -e 'type == "object" and (.active_session_id | type == "string") and (.paused | type == "boolean") and (.capture_state | type == "string")' \
    >/dev/null 2>&1 <<<"$status" || return 0
  printf '%s' "$status"
}

# busy_reason STATUS prints why a restart must wait, from the recorder's /api/status (STATUS): it
# records a session, or, with GHOST_WISPR_UPDATE_PAUSED_ONLY=1, it is not paused. It prints nothing
# when the recorder may restart, or when it gave no status (recorder_status).
busy_reason() {
  [[ -n $1 ]] || return 0
  if jq -e '.active_session_id != null and .active_session_id != ""' >/dev/null 2>&1 <<<"$1"; then
    printf 'active session'
  elif [[ $PAUSED_ONLY == 1 ]] && ! jq -e '.paused == true and .capture_state == "paused"' >/dev/null 2>&1 <<<"$1"; then
    printf 'not paused'
  fi
}

# hold_while_busy ends the update, leaving the binary in place, while the recorder may not restart,
# and fails while it runs but gives no status. It runs before the download, and again just before
# the swap: a meeting can start while the release downloads.
hold_while_busy() {
  local status reason
  status=$(recorder_status)
  reason=$(busy_reason "$status")
  if [[ -n $reason ]]; then
    printf 'ghost-wispr update: %s; leaving the current binary in place\n' "$reason"
    exit 0
  fi
  if [[ -z $status ]] && systemctl --user is-active --quiet ghost-wispr.service; then
    fail "ghost-wispr is active but /api/status gave no status"
  fi
}

# restartable WHY returns once the running recorder may restart for a rollback: its status answers
# and busy_reason finds nothing, once, or, after a read that gave no status, on two reads
# RECHECK_EVERY seconds apart, as pi-when-idle.sh's rule has it. A recorder that answers that it may
# not restart is left running, and so is one whose state no read of ROLLBACK_READS could read: it
# may be recording, and the restart would end that.
restartable() {
  local why=$1 status reason reads=0 good=0 unread=false
  while :; do
    status=$(recorder_status)
    reads=$((reads + 1))
    if [[ -n $status ]]; then
      reason=$(busy_reason "$status")
      [[ -z $reason ]] \
        || fail "$why; the new binary runs and reports $reason, so it is left running; $PREVIOUS_BINARY holds the previous binary for a rollback once nothing records"
      good=$((good + 1))
      if ! $unread || ((good >= 2)); then
        return 0
      fi
    else
      unread=true good=0
    fi
    ((reads < ROLLBACK_READS)) \
      || fail "$why; ghost-wispr.service runs, but its state could not be read on $reads reads ${RECHECK_EVERY} s apart (/api/status gave no status), so it may be recording and is left running; $PREVIOUS_BINARY holds the previous binary for a rollback once it answers that nothing records"
    (($(date +%s) - STARTED + RECHECK_EVERY + STATUS_READ + RESTART_WORST <= UNIT_LIMIT - MARGIN)) \
      || fail "$why; ghost-wispr.service runs, but its state could not be read on $reads reads (/api/status gave no status), and another read ${RECHECK_EVERY} s on and a restart would not end before the update unit's ${UNIT_LIMIT} s limit, so it may be recording and is left running; $PREVIOUS_BINARY holds the previous binary for a rollback once it answers that nothing records"
    sleep "$RECHECK_EVERY"
  done
}

# rollback WHY puts the previous binary back and restarts the recorder, then fails with WHY. It
# restarts a stopped recorder at once, and a running one only when it may restart (restartable).
rollback() {
  local why=$1
  [[ -f "$PREVIOUS_BINARY" ]] || fail "$why; no previous binary is available for rollback"
  if systemctl --user is-active --quiet ghost-wispr.service; then
    restartable "$why"
  fi
  # A copy over the binary fails with "Text file busy" while the new one still runs; a rename
  # replaces it, and the running process keeps the file it opened until the restart.
  cp --preserve=mode "$PREVIOUS_BINARY" "$BINARY.rollback"
  mv -f "$BINARY.rollback" "$BINARY"
  if ! systemctl --user restart ghost-wispr.service; then
    fail "$why; restored the previous binary but could not restart ghost-wispr.service"
  fi
  fail "$why; restored the previous binary"
}

hold_while_busy

release_json=$(curl --connect-timeout 5 --max-time 30 --fail --silent --show-error "$RELEASE_API")
release_commit=$(jq -r '.target_commitish // empty' <<<"$release_json")
[[ "$release_commit" =~ ^[0-9a-f]{40}$ ]] || fail "latest release has no immutable target commit"
binary_url=$(jq -r '.assets[] | select(.name == "ghost-wispr-linux-arm64") | .browser_download_url' <<<"$release_json")
checksum_url=$(jq -r '.assets[] | select(.name == "ghost-wispr-linux-arm64.sha256") | .browser_download_url' <<<"$release_json")
[[ -n "$binary_url" && "$binary_url" != null ]] || fail "latest release has no arm64 binary"
[[ -n "$checksum_url" && "$checksum_url" != null ]] || fail "latest release has no arm64 checksum"

current_commit=$(curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$VERSION_URL" 2>/dev/null \
  | jq -r '.commit // empty' 2>/dev/null || true)
# The first run of this update.sh finds no record: the binary that runs and answers its commit
# now is the one verified last.
if [[ -n $current_commit && ! -f "$VERIFIED" ]]; then
  record_verified
fi
if [[ "$current_commit" == "$release_commit" ]]; then
  record_verified
  printf 'ghost-wispr update: already on commit %s\n' "$current_commit"
  exit 0
fi

staging_dir=$(mktemp -d -p "$DEPLOY_DIR" .ghost-wispr-update.XXXXXX)
trap 'rm -rf -- "$staging_dir"' EXIT
# The binary links the paused listener's spotter and embeds its model, about twice its old size:
# 300 s needs about 0.3 MB/s.
curl --connect-timeout 5 --max-time 300 --fail --silent --show-error --location "$binary_url" --output "$staging_dir/ghost-wispr-linux-arm64"
curl --connect-timeout 5 --max-time 60 --fail --silent --show-error --location "$checksum_url" --output "$staging_dir/ghost-wispr-linux-arm64.sha256"
(
  cd "$staging_dir"
  sha256sum --check --status ghost-wispr-linux-arm64.sha256
) || fail "release checksum does not match the arm64 binary"
chmod 0755 "$staging_dir/ghost-wispr-linux-arm64"

hold_while_busy
keep_previous
mv "$staging_dir/ghost-wispr-linux-arm64" "$BINARY"

if ! systemctl --user restart ghost-wispr.service; then
  rollback "new binary did not start"
fi

installed_commit=
deadline=$(($(date +%s) + 120))
while (($(date +%s) < deadline)); do
  version_json=$(curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$VERSION_URL" 2>/dev/null || true)
  if [[ -n "$version_json" ]]; then
    installed_commit=$(jq -r '.commit // empty' <<<"$version_json" 2>/dev/null || true)
    if [[ "$installed_commit" == "$release_commit" ]]; then
      break
    fi
  fi
  sleep 2
done

if [[ "$installed_commit" != "$release_commit" ]]; then
  rollback "version verification timed out"
fi

record_verified
printf 'ghost-wispr update: installed commit %s\n' "$installed_commit"
