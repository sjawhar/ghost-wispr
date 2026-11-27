#!/usr/bin/env bash
# Builds the Pi's deploy tarball, ghost-wispr-pi-deploy.tar.gz, that each release carries
# (dispatch://INFRA-5935/spec R5). It is the one script that builds it: release.yml runs it, and
# the tests run provision.sh from its --stage directory.
#
#   build-deploy-tarball.sh COMMIT BINARY OUTDIR
#       writes OUTDIR/ghost-wispr-pi-deploy.tar.gz and OUTDIR/ghost-wispr-pi-deploy.tar.gz.sha256.
#   build-deploy-tarball.sh --stage COMMIT BINARY DIR
#       writes the tarball's directory alone into DIR, which must be absent or empty.
#
# The tarball has one top directory, ghost-wispr-pi-deploy/, with the 14 files below and
# manifest.json directly in it. COMMIT is the release's commit. BINARY is the release's
# ghost-wispr-linux-arm64, which stays its own release asset: the manifest names its SHA-256, and
# the build runs `BINARY link-versions`, so it runs where the binary runs. An exit 0 with a JSON
# array of positive integers sets link_versions to that array. The exit 2 with the line
# `unknown subcommand "link-versions"` sets no key. Any other answer fails the build. A failed
# build writes nothing.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly set_name=ghost-wispr-pi-deploy
readonly unknown_line='unknown subcommand "link-versions"'

# Each file of the tarball: its name, its source in the repository, and its mode.
readonly files=(
  # The nine files that the nightly update installs with the binary.
  'ghost-wispr.yaml deploy/pi/files/ghost-wispr.yaml 0644'
  'pi.env deploy/pi/files/pi.env 0644'
  'update.sh deploy/pi/update.sh 0755'
  'pi-lib.sh deploy/pi/pi-lib.sh 0755'
  'ghost-wispr.service deploy/pi/files/ghost-wispr.service 0644'
  'wait-for-network.sh wait-for-network.sh 0755'
  'wait-for-time-sync.sh deploy/pi/files/wait-for-time-sync.sh 0755'
  '99-ghost-wispr.rules deploy/pi/files/99-ghost-wispr.rules 0644'
  'logind-power-key.conf deploy/pi/files/logind-power-key.conf 0644'
  # The five provisioning files, which only provision.sh installs.
  'provision.sh deploy/pi/provision.sh 0755'
  'ghost-wispr-update-launcher.sh deploy/pi/files/ghost-wispr-update-launcher.sh 0755'
  'ghost-wispr-update.service deploy/pi/files/ghost-wispr-update.service 0644'
  'ghost-wispr-update.timer deploy/pi/files/ghost-wispr-update.timer 0644'
  'serve.conf deploy/pi/files/serve.conf 0644'
)

fail() {
  printf 'build-deploy-tarball.sh: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf 'usage: build-deploy-tarball.sh [--stage] COMMIT BINARY OUTDIR\n' >&2
  exit 2
}

stage=false
if [[ ${1:-} == --stage ]]; then
  stage=true
  shift
fi
(($# == 3)) || usage
commit=$1 binary=$2 out=$3

[[ $commit =~ ^[0123456789abcdef]{40}$ ]] || fail "the commit '$commit' is not 40 lowercase hex digits"
[[ -f $binary && -x $binary ]] || fail "the binary $binary is no executable file"
# A bare name would run a ghost-wispr from PATH.
[[ $binary == */* ]] || binary=./$binary
if [[ $stage == true ]]; then
  if [[ -e $out || -L $out ]]; then
    [[ -d $out ]] || fail "the stage $out is not a directory"
    [[ -z $(find "$out" -mindepth 1 -maxdepth 1 -print -quit) ]] || fail "the stage $out is not empty"
  fi
else
  [[ -d $out ]] || fail "the output directory $out is not a directory"
fi

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

# The link versions the binary accepts (the plan's C6), as JSON, or null for no key.
answer=0
"$binary" link-versions </dev/null >"$work/link-versions.out" 2>"$work/link-versions.err" || answer=$?
if ((answer == 0)) && link_versions=$(jq -cs '
  if length == 1 and (.[0] | type == "array" and length > 0
    and all(.[]; type == "number" and . > 0 and . == floor))
  then .[0] else error("no array") end' "$work/link-versions.out" 2>/dev/null); then
  :
elif ((answer == 2)) && grep -qxF -- "$unknown_line" "$work/link-versions.err"; then
  link_versions=null
else
  fail "$binary link-versions exited $answer with stdout [$(head -c 1000 "$work/link-versions.out")] and stderr [$(head -c 1000 "$work/link-versions.err")]; want exit 0 with a JSON array of positive integers, or exit 2 with the line '$unknown_line'"
fi

set_dir=$work/$set_name
mkdir -m 0755 -- "$set_dir"
sums=()
for entry in "${files[@]}"; do
  read -r name source mode <<<"$entry"
  [[ -f $repo_root/$source ]] || fail "the repository has no $source"
  cp -- "$repo_root/$source" "$set_dir/$name"
  chmod -- "$mode" "$set_dir/$name"
  sum=$(sha256sum <"$set_dir/$name")
  sums+=("$name" "${sum%% *}")
done
binary_sum=$(sha256sum <"$binary")
jq -n --arg commit "$commit" --arg binary_sha256 "${binary_sum%% *}" --argjson link_versions "$link_versions" '
  {format: 1, commit: $commit, binary_sha256: $binary_sha256,
   files: ($ARGS.positional | [range(0; length; 2) as $i | {(.[$i]): .[$i + 1]}] | add)}
  + (if $link_versions == null then {} else {link_versions: $link_versions} end)' \
  --args "${sums[@]}" >"$set_dir/manifest.json"
chmod -- 0644 "$set_dir/manifest.json"

if [[ $stage == true ]]; then
  mkdir -p -- "$out"
  cp -p -- "$set_dir"/* "$out/"
  exit 0
fi

tar --sort=name --owner=0 --group=0 --numeric-owner -C "$work" -czf "$work/$set_name.tar.gz" "$set_name"
(cd -- "$work" && sha256sum -- "$set_name.tar.gz" >"$set_name.tar.gz.sha256")
mv -f -- "$work/$set_name.tar.gz" "$work/$set_name.tar.gz.sha256" "$out/"
