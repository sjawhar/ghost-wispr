#!/usr/bin/env bash
# Install the newest verified Ghost Wispr release unless a session is active.
set -euo pipefail

readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly BINARY="$DEPLOY_DIR/ghost-wispr"
readonly PREVIOUS_BINARY="$DEPLOY_DIR/ghost-wispr.prev"
readonly RELEASE_API="${GHOST_WISPR_RELEASE_API:-https://api.github.com/repos/sjawhar/ghost-wispr/releases/latest}"
readonly STATUS_URL="${GHOST_WISPR_STATUS_URL:-http://127.0.0.1:8080/api/status}"
readonly VERSION_URL="${GHOST_WISPR_VERSION_URL:-http://127.0.0.1:8080/api/version}"

fail() {
  printf 'ghost-wispr update: %s\n' "$*" >&2
  exit 1
}

rollback() {
  [[ -f "$PREVIOUS_BINARY" ]] || fail "no previous binary is available for rollback"
  cp --preserve=mode "$PREVIOUS_BINARY" "$BINARY"
  if ! systemctl --user restart ghost-wispr.service; then
    fail "restored the previous binary but could not restart ghost-wispr.service"
  fi
}

status_json=$(curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$STATUS_URL" 2>/dev/null || true)
if [[ -n "$status_json" ]] && jq -e '.active_session_id != null and .active_session_id != ""' >/dev/null <<<"$status_json"; then
  printf 'ghost-wispr update: active session; leaving the current binary in place\n'
  exit 0
fi
if [[ -z "$status_json" ]] && systemctl --user is-active --quiet ghost-wispr.service; then
  fail "ghost-wispr is active but /api/status did not answer"
fi

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
curl --connect-timeout 5 --max-time 60 --fail --silent --show-error --location "$binary_url" --output "$staging_dir/ghost-wispr-linux-arm64"
curl --connect-timeout 5 --max-time 60 --fail --silent --show-error --location "$checksum_url" --output "$staging_dir/ghost-wispr-linux-arm64.sha256"
(
  cd "$staging_dir"
  sha256sum --check --status ghost-wispr-linux-arm64.sha256
) || fail "release checksum does not match the arm64 binary"
chmod 0755 "$staging_dir/ghost-wispr-linux-arm64"

if [[ -f "$BINARY" ]]; then
  cp --preserve=mode "$BINARY" "$PREVIOUS_BINARY"
fi
mv "$staging_dir/ghost-wispr-linux-arm64" "$BINARY"

if ! systemctl --user restart ghost-wispr.service; then
  rollback
  fail "new binary did not start; restored the previous binary"
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
  rollback
  fail "version verification timed out; restored the previous binary"
fi

printf 'ghost-wispr update: installed commit %s\n' "$installed_commit"
