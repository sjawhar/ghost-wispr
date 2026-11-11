#!/usr/bin/env bash
set -euo pipefail

work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
mkdir -p "$work_dir/bin"

cat >"$work_dir/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${GH_REPO:?} == sjawhar/ghost-wispr ]]
[[ $1 == release && $2 == create ]]
[[ $4 == ghost-wispr-linux-arm64 && $5 == ghost-wispr-linux-arm64.sha256 ]]
[[ " $* " == *' --target 0123456789abcdef0123456789abcdef01234567 '* ]]
EOF
chmod +x "$work_dir/bin/gh"

touch "$work_dir/ghost-wispr-linux-arm64" "$work_dir/ghost-wispr-linux-arm64.sha256"
(
  cd "$work_dir"
  tag=v2026.11.11-0123456
  GH_REPO=sjawhar/ghost-wispr GH_TOKEN=test PATH="$work_dir/bin:$PATH" \
    gh release create "$tag" \
      ghost-wispr-linux-arm64 \
      ghost-wispr-linux-arm64.sha256 \
      --target 0123456789abcdef0123456789abcdef01234567 \
      --title "$tag" \
      --generate-notes
)
printf 'release publish environment test passed\n'
