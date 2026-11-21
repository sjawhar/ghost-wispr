#!/usr/bin/env bash
# Install the newest verified Ghost Wispr release unless a session is active.
set -euo pipefail

readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly BINARY="$DEPLOY_DIR/ghost-wispr"
readonly PREVIOUS_BINARY="$DEPLOY_DIR/ghost-wispr.prev"
readonly RELEASE_API="${GHOST_WISPR_RELEASE_API:-https://api.github.com/repos/sjawhar/ghost-wispr/releases/latest}"
readonly STATUS_URL="${GHOST_WISPR_STATUS_URL:-http://127.0.0.1:8080/api/status}"
readonly VERSION_URL="${GHOST_WISPR_VERSION_URL:-http://127.0.0.1:8080/api/version}"
# 1 (deploy/pi/rollout.sh sets it): install only while the recorder is paused, not only while it
# records no session.
readonly PAUSED_ONLY="${GHOST_WISPR_UPDATE_PAUSED_ONLY:-}"

fail() {
  printf 'ghost-wispr update: %s\n' "$*" >&2
  exit 1
}

# recorder_status prints the recorder's /api/status, or nothing when it does not answer.
recorder_status() {
  curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$STATUS_URL" 2>/dev/null || true
}

# busy_reason STATUS prints why a restart must wait, from the recorder's /api/status (STATUS): it
# records a session, or, with GHOST_WISPR_UPDATE_PAUSED_ONLY=1, it is not paused. It prints nothing
# when the recorder may restart, or when it did not answer.
busy_reason() {
  [[ -n $1 ]] || return 0
  if jq -e '.active_session_id != null and .active_session_id != ""' >/dev/null 2>&1 <<<"$1"; then
    printf 'active session'
  elif [[ $PAUSED_ONLY == 1 ]] && ! jq -e '.paused == true and .capture_state == "paused"' >/dev/null 2>&1 <<<"$1"; then
    printf 'not paused'
  fi
}

# hold_while_busy ends the update, leaving the binary in place, while the recorder may not restart,
# and fails while it runs but does not answer. It runs before the download, and again just before
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
    fail "ghost-wispr is active but /api/status did not answer"
  fi
}

# rollback WHY puts the previous binary back and restarts the recorder, then fails with WHY. A new
# binary that runs and answers that it may not restart (a session it records) is left running:
# the restart would end that session.
rollback() {
  local why=$1 reason
  [[ -f "$PREVIOUS_BINARY" ]] || fail "$why; no previous binary is available for rollback"
  if systemctl --user is-active --quiet ghost-wispr.service; then
    reason=$(busy_reason "$(recorder_status)")
    [[ -z $reason ]] \
      || fail "$why; the new binary runs and reports $reason, so it is left running; $PREVIOUS_BINARY holds the previous binary for a rollback once nothing records"
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
if [[ "$current_commit" == "$release_commit" ]]; then
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
if [[ -f "$BINARY" ]]; then
  cp --preserve=mode "$BINARY" "$PREVIOUS_BINARY"
fi
mv "$staging_dir/ghost-wispr-linux-arm64" "$BINARY"

if ! systemctl --user restart ghost-wispr.service; then
  rollback "new binary did not start"
fi

installed_commit=
deadline=$((SECONDS + 120))
while (( SECONDS < deadline )); do
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

printf 'ghost-wispr update: installed commit %s\n' "$installed_commit"
