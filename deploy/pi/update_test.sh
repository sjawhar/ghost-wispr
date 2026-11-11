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
    [[ ${TEST_SCENARIO:?} == active ]] && printf '{"active_session_id":"session"}\n'
    ;;
  *releases/latest)
    printf '{"target_commitish":"%s","assets":[{"name":"ghost-wispr-linux-arm64","browser_download_url":"https://test.invalid/binary"},{"name":"ghost-wispr-linux-arm64.sha256","browser_download_url":"https://test.invalid/checksum"}]}' "$TEST_COMMIT"
    ;;
  *binary)
    cp "$TEST_BINARY" "$output"
    ;;
  *checksum)
    printf '%s  ghost-wispr-linux-arm64\n' "$TEST_CHECKSUM" >"$output"
    ;;
  *version)
    if [[ ${TEST_SCENARIO:?} == down && ! -s ${TEST_SYSTEMCTL_LOG:?} ]]; then
      exit 7
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
EOF
chmod +x "$work_dir/bin/systemctl"

run_update() {
  PATH="$work_dir/bin:$PATH" \
    TEST_SCENARIO=$1 \
    TEST_BINARY="$binary_source" \
    TEST_CHECKSUM="$binary_checksum" \
    TEST_COMMIT="$commit" \
    TEST_SYSTEMCTL_LOG="$work_dir/systemctl.log" \
    GHOST_WISPR_DEPLOY_DIR="$work_dir/deploy" \
    GHOST_WISPR_RELEASE_API=https://test.invalid/releases/latest \
    GHOST_WISPR_STATUS_URL=https://test.invalid/status \
    GHOST_WISPR_VERSION_URL=https://test.invalid/version \
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
printf 'update tests passed\n'
