#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root

work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

cat >"$work_dir/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${GH_REPO:?} == sjawhar/ghost-wispr ]]
[[ $1 == release && $2 == create ]]
[[ $4 == ghost-wispr-linux-arm64 && $5 == ghost-wispr-linux-arm64.sha256 ]]
# The binary links third-party code statically (the paused listener's spotter), so its notices ship beside it.
[[ $6 == THIRD_PARTY_NOTICES.md ]]
[[ " $* " == *' --target 0123456789abcdef0123456789abcdef01234567 '* ]]
EOF
chmod +x "$work_dir/bin/gh"

touch "$work_dir/ghost-wispr-linux-arm64" "$work_dir/ghost-wispr-linux-arm64.sha256" "$work_dir/THIRD_PARTY_NOTICES.md"
awk '
  /release-publish-command:start/ { capture = 1; next }
  /release-publish-command:end/ { exit }
  capture { sub(/^          /, ""); print }
' "$repo_root/.github/workflows/release.yml" >"$work_dir/publish.sh"
(
  cd "$work_dir"
  GH_REPO=sjawhar/ghost-wispr GH_TOKEN=test BUILD_SHA=0123456789abcdef0123456789abcdef01234567 PATH="$work_dir/bin:$PATH" \
    bash "$work_dir/publish.sh"
)
printf 'release publish environment test passed\n'
