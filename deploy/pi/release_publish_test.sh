#!/usr/bin/env bash
# Tests deploy/pi/build-deploy-tarball.sh, the one script that builds the Pi's deploy tarball, and
# the steps of .github/workflows/release.yml that build and publish it (dispatch://INFRA-5935/spec
# R5; the plan's contracts C2 and C6). Each expectation is written out here, not read from the
# script: the 14 files, the source and the mode of each, and the fields of the manifest.
#
# The tarball has one top directory, ghost-wispr-pi-deploy/, and each file is directly in it: the
# nine files that the nightly update installs, the five provisioning files, and manifest.json. Each
# script (*.sh) has mode 0755 and each other file 0644. manifest.json holds format 1, the commit, the
# SHA-256 of the binary and of each other file. It holds link_versions when the binary answers
# `link-versions` with exit 0 and a JSON array of positive integers, and no such key when the binary
# answers with exit 2 and the line `unknown subcommand "link-versions"`. Any other answer fails the
# build.
#
# The binary is a fake, and gh is a fake that records its arguments, so a pass does not show a
# published release.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly build="$repo_root/deploy/pi/build-deploy-tarball.sh"
readonly workflow="$repo_root/.github/workflows/release.yml"
readonly commit=0123456789abcdef0123456789abcdef01234567
readonly unknown_answer=$'unknown subcommand "link-versions"\nusage: ghost-wispr <command>'
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

name=setup
fail() {
  printf 'release publish test (%s): %s\n' "$name" "$*" >&2
  exit 1
}

# The 14 files of the tarball: the name, the source in the repository, and the mode.
readonly expected_files=(
  # The nine files that the nightly update installs.
  'ghost-wispr.yaml deploy/pi/files/ghost-wispr.yaml 644'
  'pi.env deploy/pi/files/pi.env 644'
  'update.sh deploy/pi/update.sh 755'
  'pi-lib.sh deploy/pi/pi-lib.sh 755'
  'wait-for-network.sh wait-for-network.sh 755'
  'wait-for-time-sync.sh deploy/pi/files/wait-for-time-sync.sh 755'
  'ghost-wispr.service deploy/pi/files/ghost-wispr.service 644'
  '99-ghost-wispr.rules deploy/pi/files/99-ghost-wispr.rules 644'
  'logind-power-key.conf deploy/pi/files/logind-power-key.conf 644'
  # The five provisioning files, which only provision.sh installs.
  'provision.sh deploy/pi/provision.sh 755'
  'ghost-wispr-update-launcher.sh deploy/pi/files/ghost-wispr-update-launcher.sh 755'
  'ghost-wispr-update.service deploy/pi/files/ghost-wispr-update.service 644'
  'ghost-wispr-update.timer deploy/pi/files/ghost-wispr-update.timer 644'
  'serve.conf deploy/pi/files/serve.conf 644'
)
# The five assets of each release.
readonly expected_assets=(ghost-wispr-linux-arm64 ghost-wispr-linux-arm64.sha256 ghost-wispr-pi-deploy.tar.gz
  ghost-wispr-pi-deploy.tar.gz.sha256 THIRD_PARTY_NOTICES.md)

declare -A source_of=() mode_of=()
names=()
for entry in "${expected_files[@]}"; do
  read -r file source mode <<<"$entry"
  names+=("$file")
  source_of[$file]=$source
  mode_of[$file]=$mode
done
((${#names[@]} == 14)) || fail "the test's own table holds ${#names[@]} files, not the 14 of R5"
for file in "${names[@]}"; do
  [[ -f $repo_root/${source_of[$file]} ]] || fail "the test's own table names ${source_of[$file]}, which the repository does not hold"
done
# sorted_lines LINE... prints each LINE in byte order, as jq's keys and comm sort.
sorted_lines() { printf '%s\n' "$@" | LC_ALL=C sort; }
expected_directory=$(sorted_lines "${names[@]}" manifest.json)
readonly expected_directory
expected_members=$(sorted_lines ghost-wispr-pi-deploy/ "${names[@]/#/ghost-wispr-pi-deploy/}" ghost-wispr-pi-deploy/manifest.json)
readonly expected_members

# fake_binary FILE STDOUT STDERR EXIT writes a release binary. Its `link-versions` prints STDOUT and
# STDERR, each when not empty, and exits EXIT. Each run appends its arguments to FILE.args, and any
# other subcommand exits 3: the build runs `link-versions` alone.
fake_binary() {
  {
    printf '#!/usr/bin/env bash\n'
    printf 'printf "%%s\\n" "$*" >>%q\n' "$1.args"
    printf '[[ $* == link-versions ]] || exit 3\n'
    if [[ -n $2 ]]; then
      printf 'printf "%%s\\n" %q\n' "$2"
    fi
    if [[ -n $3 ]]; then
      printf 'printf "%%s\\n" %q >&2\n' "$3"
    fi
    printf 'exit %d\n' "$4"
  } >"$1"
  chmod 0755 "$1"
}

# entries DIR prints each path under DIR, relative to DIR, in byte order.
entries() {
  local listing
  listing=$(find "$1" -mindepth 1 -printf '%P\n') || return 1
  [[ -z $listing ]] || LC_ALL=C sort <<<"$listing"
}

# differences WANT HAVE prints what the sorted list WANT lacks in HAVE and what HAVE adds.
differences() {
  printf 'lacks [%s], and holds [%s] besides' \
    "$(LC_ALL=C comm -23 <(printf '%s\n' "$1") <(printf '%s\n' "$2") | paste -sd ' ')" \
    "$(LC_ALL=C comm -13 <(printf '%s\n' "$1") <(printf '%s\n' "$2") | paste -sd ' ')"
}

name="the test's table is update.sh's SET_FILES and PROVISIONING_FILES"
# update.sh's two lists, from a shell that runs their declarations alone: update.sh itself is not
# sourced. The table's first nine files are the deploy set and its last five the provisioning files.
declarations=$(awk '
  /^readonly (SET_FILES|PROVISIONING_FILES)=\(/ { take = 1 }
  take { print }
  take && /\)/ { take = 0 }' "$repo_root/deploy/pi/update.sh")
update_lists=$(bash --noprofile --norc -euc "$declarations"'
  ((${#SET_FILES[@]} > 0 && ${#PROVISIONING_FILES[@]} > 0))
  printf "set %s\n" "${SET_FILES[@]}"
  printf "provisioning %s\n" "${PROVISIONING_FILES[@]}"' 2>&1) \
  || fail "cannot read SET_FILES and PROVISIONING_FILES from deploy/pi/update.sh: $update_lists"
update_lists=$(LC_ALL=C sort <<<"$update_lists")
table_lists=$(printf 'set %s\n' "${names[@]:0:9}" && printf 'provisioning %s\n' "${names[@]:9}")
table_lists=$(LC_ALL=C sort <<<"$table_lists")
[[ $update_lists == "$table_lists" ]] || fail "update.sh's lists $(differences "$table_lists" "$update_lists")"

# check_members TARBALL: the entries of TARBALL are ghost-wispr-pi-deploy/, the 14 files in it, and
# its manifest.json, and nothing else.
check_members() {
  local listing
  listing=$(tar -tzf "$1") || { printf 'cannot list %s\n' "$1" >&2; return 1; }
  listing=$(LC_ALL=C sort <<<"$listing")
  [[ $listing == "$expected_members" ]] && return 0
  printf 'the tarball %s\n' "$(differences "$expected_members" "$listing")" >&2
  return 1
}

# check_sum_asset DIR: DIR/ghost-wispr-pi-deploy.tar.gz.sha256 is the line that sha256sum prints for
# DIR/ghost-wispr-pi-deploy.tar.gz.
check_sum_asset() {
  local want have
  want=$(cd -- "$1" && sha256sum ghost-wispr-pi-deploy.tar.gz) || { printf 'cannot read %s/ghost-wispr-pi-deploy.tar.gz\n' "$1" >&2; return 1; }
  have=$(<"$1/ghost-wispr-pi-deploy.tar.gz.sha256") || { printf 'cannot read %s/ghost-wispr-pi-deploy.tar.gz.sha256\n' "$1" >&2; return 1; }
  [[ $have == "$want" ]] && return 0
  printf 'ghost-wispr-pi-deploy.tar.gz.sha256 holds [%s], not [%s]\n' "$have" "$want" >&2
  return 1
}

# check_set DIR: DIR holds the 14 files and manifest.json and nothing else. Each is a regular file
# with its mode, and each of the 14 has the bytes of its source.
check_set() {
  local dir=$1 listing file mode
  listing=$(entries "$dir") || { printf 'cannot list %s\n' "$dir" >&2; return 1; }
  if [[ $listing != "$expected_directory" ]]; then
    printf 'ghost-wispr-pi-deploy/ %s\n' "$(differences "$expected_directory" "$listing")" >&2
    return 1
  fi
  for file in "${names[@]}" manifest.json; do
    if [[ ! -f $dir/$file || -L $dir/$file ]]; then
      printf '%s is not a regular file\n' "$file" >&2
      return 1
    fi
    mode=$(stat -c %a -- "$dir/$file") || return 1
    if [[ $mode != "${mode_of[$file]:-644}" ]]; then
      printf '%s has mode %s, not %s\n' "$file" "$mode" "${mode_of[$file]:-644}" >&2
      return 1
    fi
  done
  for file in "${names[@]}"; do
    if ! cmp -s -- "$dir/$file" "$repo_root/${source_of[$file]}"; then
      printf '%s does not hold the bytes of %s\n' "$file" "${source_of[$file]}" >&2
      return 1
    fi
  done
}

# check_manifest DIR COMMIT BINARY: DIR/manifest.json is format 1 of COMMIT, names the SHA-256 of
# BINARY, and names each of the 14 files with the SHA-256 of its copy in DIR, and no other file. Its
# keys are those and link_versions, which each case checks itself.
check_manifest() {
  local dir=$1 manifest=$1/manifest.json binary_sum keys file want have
  binary_sum=$(sha256sum -- "$3") || { printf 'cannot read %s\n' "$3" >&2; return 1; }
  binary_sum=${binary_sum%% *}
  keys=$(jq -r 'keys - ["link_versions"] | join(" ")' "$manifest") || { printf 'manifest.json is no JSON object\n' >&2; return 1; }
  if [[ $keys != 'binary_sha256 commit files format' ]]; then
    printf 'manifest.json has the keys [%s] besides link_versions, not [binary_sha256 commit files format]\n' "$keys" >&2
    return 1
  fi
  if ! jq -e '.format == 1' "$manifest" >/dev/null; then
    printf 'manifest.json has format %s, not 1\n' "$(jq -c .format "$manifest")" >&2
    return 1
  fi
  have=$(jq -r .commit "$manifest") || return 1
  if [[ $have != "$2" ]]; then
    printf 'manifest.json has commit %s, not %s\n' "$have" "$2" >&2
    return 1
  fi
  have=$(jq -r .binary_sha256 "$manifest") || return 1
  if [[ $have != "$binary_sum" ]]; then
    printf 'manifest.json has binary_sha256 %s, but the binary has SHA-256 %s\n' "$have" "$binary_sum" >&2
    return 1
  fi
  have=$(jq -r '.files | keys[]' "$manifest") || { printf 'manifest.json has no files object\n' >&2; return 1; }
  if [[ $have != "$(sorted_lines "${names[@]}")" ]]; then
    printf 'the files of manifest.json %s\n' "$(differences "$(sorted_lines "${names[@]}")" "$have")" >&2
    return 1
  fi
  for file in "${names[@]}"; do
    want=$(sha256sum -- "$dir/$file") || return 1
    want=${want%% *}
    have=$(jq -r --arg name "$file" '.files[$name]' "$manifest") || return 1
    if [[ $have != "$want" ]]; then
      printf 'manifest.json gives %s the SHA-256 %s, but the file has %s\n' "$file" "$have" "$want" >&2
      return 1
    fi
  done
}

# unpack TARBALL DIR extracts TARBALL into the new directory DIR with the modes it holds, and prints
# DIR/ghost-wispr-pi-deploy.
unpack() {
  mkdir -- "$2"
  tar -xpzf "$1" -C "$2" || fail "cannot unpack $1"
  printf '%s/ghost-wispr-pi-deploy\n' "$2"
}

# build_into NAME STDOUT STDERR EXIT runs the build on a fake binary NAME that answers so, into the
# new directory $work/NAME.out. It fails the case when the build fails.
build_into() {
  fake_binary "$work/$1" "$2" "$3" "$4"
  mkdir -- "$work/$1.out"
  bash "$build" "$commit" "$work/$1" "$work/$1.out" || fail "the build exited $?"
}

# --- The tarball.

name='the tarball holds the deploy set'
build_into versions '[1,2]' '' 0
listing=$(entries "$work/versions.out")
[[ $listing == $'ghost-wispr-pi-deploy.tar.gz\nghost-wispr-pi-deploy.tar.gz.sha256' ]] \
  || fail "the build wrote [${listing//$'\n'/ }], not the tarball and its .sha256"
check_sum_asset "$work/versions.out" || fail 'the .sha256 does not name the tarball'
check_members "$work/versions.out/ghost-wispr-pi-deploy.tar.gz" || fail 'the tarball does not hold the deploy set'
set_dir=$(unpack "$work/versions.out/ghost-wispr-pi-deploy.tar.gz" "$work/versions.unpacked")
check_set "$set_dir" || fail 'the files of the tarball are not the deploy set'
check_manifest "$set_dir" "$commit" "$work/versions" || fail 'the manifest does not match the tarball'
[[ $(<"$work/versions.args") == link-versions ]] \
  || fail "the build ran the binary as [$(paste -sd '|' "$work/versions.args")], not once as 'link-versions'"

name='link-versions: a JSON array of positive integers gives link_versions'
jq -e '.link_versions == [1, 2]' "$set_dir/manifest.json" >/dev/null \
  || fail "the binary answered [1,2], and link_versions is $(jq -c '.link_versions' "$set_dir/manifest.json")"

# The checks themselves, against the fault each must see.
name='control: a file edited after the manifest'
cp -pR -- "$set_dir" "$work/edited"
printf '# edited after the manifest\n' >>"$work/edited/ghost-wispr.yaml"
if check_manifest "$work/edited" "$commit" "$work/versions" 2>"$work/why"; then
  fail 'the manifest check passed a ghost-wispr.yaml edited after the manifest'
fi
grep -q 'ghost-wispr.yaml' "$work/why" || fail "the manifest check failed, but not for ghost-wispr.yaml: $(<"$work/why")"

name='control: the manifest of another binary'
fake_binary "$work/other" '[1,2]' '' 0
printf '# another build\n' >>"$work/other"
if check_manifest "$set_dir" "$commit" "$work/other" 2>"$work/why"; then
  fail 'the manifest check passed a binary that is not the one the manifest names'
fi
grep -q 'binary_sha256' "$work/why" || fail "the manifest check failed, but not for binary_sha256: $(<"$work/why")"

name='control: a tarball with the local settings file'
mkdir -- "$work/extra"
cp -pR -- "$set_dir" "$work/extra/"
printf 'silence_timeout: 45s\n' >"$work/extra/ghost-wispr-pi-deploy/ghost-wispr.local.yaml"
tar -C "$work/extra" -czf "$work/extra.tar.gz" ghost-wispr-pi-deploy
if check_members "$work/extra.tar.gz" 2>"$work/why"; then
  fail 'the member check passed a tarball that holds ghost-wispr.local.yaml'
fi
grep -q 'ghost-wispr.local.yaml' "$work/why" || fail "the member check failed, but not for ghost-wispr.local.yaml: $(<"$work/why")"

name='control: a tarball without serve.conf'
mkdir -- "$work/missing"
cp -pR -- "$set_dir" "$work/missing/"
rm -- "$work/missing/ghost-wispr-pi-deploy/serve.conf"
tar -C "$work/missing" -czf "$work/missing.tar.gz" ghost-wispr-pi-deploy
if check_members "$work/missing.tar.gz" 2>"$work/why"; then
  fail 'the member check passed a tarball without serve.conf'
fi
grep -q 'serve.conf' "$work/why" || fail "the member check failed, but not for serve.conf: $(<"$work/why")"

name='link-versions: the unknown-subcommand answer gives no key'
build_into unknown '' "$unknown_answer" 2
check_sum_asset "$work/unknown.out" || fail 'the .sha256 does not name the tarball'
check_members "$work/unknown.out/ghost-wispr-pi-deploy.tar.gz" || fail 'the tarball does not hold the deploy set'
unknown_dir=$(unpack "$work/unknown.out/ghost-wispr-pi-deploy.tar.gz" "$work/unknown.unpacked")
check_set "$unknown_dir" || fail 'the files of the tarball are not the deploy set'
check_manifest "$unknown_dir" "$commit" "$work/unknown" || fail 'the manifest does not match the tarball'
jq -e 'has("link_versions") | not' "$unknown_dir/manifest.json" >/dev/null \
  || fail "the binary does not know link-versions, and link_versions is $(jq -c '.link_versions' "$unknown_dir/manifest.json")"

# expect_answer_fails CASE STDOUT STDERR EXIT: a binary whose link-versions answers so fails the build,
# which names link-versions and writes nothing.
expect_answer_fails() {
  name="link-versions: $1 fails the build"
  local bin=$work/fails-$((${#failed_cases[@]} + 1))
  failed_cases+=("$1")
  fake_binary "$bin" "$2" "$3" "$4"
  mkdir -- "$bin.out"
  if bash "$build" "$commit" "$bin" "$bin.out" >"$bin.log" 2>&1; then
    fail "the build exited 0: $(<"$bin.log")"
  fi
  [[ -s $bin.args ]] || fail "the build failed before it ran the binary: $(<"$bin.log")"
  grep -q 'link-versions' "$bin.log" || fail "the build failed, but its message does not name link-versions: $(<"$bin.log")"
  [[ -z $(entries "$bin.out") ]] || fail "the failed build wrote [$(entries "$bin.out" | paste -sd ' ')]"
}
failed_cases=()
expect_answer_fails 'another error line on exit 2' '' $'flag provided but not defined: -json\nusage: ghost-wispr link-versions' 2
expect_answer_fails 'the unknown-subcommand words inside another line on exit 2' '' \
  'ghost-wispr: unknown subcommand "link-versions" in the config' 2
expect_answer_fails 'the unknown-subcommand answer with exit 1' '' "$unknown_answer" 1
expect_answer_fails 'exit 1' '[1,2]' 'link-versions: cannot read the link versions' 1
expect_answer_fails 'the array [0]' '[0]' '' 0
expect_answer_fails 'the array [1.5]' '[1.5]' '' 0
expect_answer_fails 'an array of a string' '["1"]' '' 0
expect_answer_fails 'an empty array' '[]' '' 0
expect_answer_fails 'an object' '{"link_versions": [1]}' '' 0
expect_answer_fails 'two arrays' $'[1]\n[2]' '' 0
expect_answer_fails 'text that is no JSON' '1,2' '' 0
expect_answer_fails 'nothing on exit 0' '' '' 0

for bad_commit in '' 0123456 0123456789ABCDEF0123456789ABCDEF01234567 "${commit}0"; do
  name="the commit '$bad_commit', which is not 40 lowercase hex digits, fails the build"
  mkdir -- "$work/bad-commit.out"
  if bash "$build" "$bad_commit" "$work/versions" "$work/bad-commit.out" >"$work/bad-commit.log" 2>&1; then
    fail "the build exited 0: $(<"$work/bad-commit.log")"
  fi
  [[ -z $(entries "$work/bad-commit.out") ]] || fail "the failed build wrote [$(entries "$work/bad-commit.out" | paste -sd ' ')]"
  rm -rf -- "$work/bad-commit.out"
done

name='a local settings file beside the sources stays out of the tarball'
mkdir -p -- "$work/copy/deploy"
cp -pR -- "$repo_root/deploy/pi" "$work/copy/deploy/pi"
cp -p -- "$repo_root/wait-for-network.sh" "$work/copy/"
for dir in "$work/copy" "$work/copy/deploy/pi" "$work/copy/deploy/pi/files"; do
  printf 'silence_timeout: 45s\n' >"$dir/ghost-wispr.local.yaml"
done
mkdir -- "$work/copy.out"
bash "$work/copy/deploy/pi/build-deploy-tarball.sh" "$commit" "$work/versions" "$work/copy.out" \
  || fail "the build of the copy exited $?"
check_members "$work/copy.out/ghost-wispr-pi-deploy.tar.gz" || fail 'the tarball of the copy does not hold the deploy set'

# --- The stage, the directory the tarball holds, for the tests that run provision.sh from it.

name='--stage writes the directory that the tarball holds'
bash "$build" --stage "$commit" "$work/versions" "$work/stage" || fail "the build exited $?"
check_set "$work/stage" || fail 'the stage is not the deploy set'
check_manifest "$work/stage" "$commit" "$work/versions" || fail 'the manifest does not match the stage'
diff -r -- "$set_dir" "$work/stage" >"$work/why" 2>&1 || fail "the stage differs from the tarball's directory: $(<"$work/why")"

name='--stage writes into an empty directory'
mkdir -- "$work/empty-stage"
bash "$build" --stage "$commit" "$work/versions" "$work/empty-stage" || fail "the build exited $?"
diff -r -- "$set_dir" "$work/empty-stage" >"$work/why" 2>&1 || fail "the stage differs from the tarball's directory: $(<"$work/why")"

name='--stage refuses a directory that holds a file'
mkdir -- "$work/used-stage"
printf 'keep\n' >"$work/used-stage/keep.txt"
if bash "$build" --stage "$commit" "$work/versions" "$work/used-stage" >"$work/why" 2>&1; then
  fail "the build exited 0: $(<"$work/why")"
fi
[[ $(entries "$work/used-stage") == keep.txt ]] || fail "the refused stage holds [$(entries "$work/used-stage" | paste -sd ' ')], not keep.txt alone"

# --- The workflow.

# workflow_block MARKER prints the lines of release.yml between "# MARKER:start" and "# MARKER:end",
# with the step's indentation removed: the command that the step runs.
workflow_block() {
  awk -v start="# $1:start" -v end="# $1:end" '
    index($0, start) { capture = 1; found = 1; next }
    capture && index($0, end) { ended = 1; exit }
    capture { sub(/^          /, ""); print }
    END { exit !(found && ended) }' "$workflow"
}

# job_block NAME prints the lines of the job NAME of release.yml, after its "  NAME:" line.
job_block() {
  awk -v job="  $1:" '
    $0 == job { inside = 1; found = 1; next }
    /^  [A-Za-z0-9_-]+:$/ { inside = 0 }
    inside { print }
    END { exit !found }' "$workflow"
}

name='the release job runs the build on the arm64 runner'
workflow_block deploy-tarball-command >"$work/build-step.sh" \
  || fail "release.yml has no command between '# deploy-tarball-command:start' and '# deploy-tarball-command:end'"
release_job=$(job_block release) || fail 'release.yml has no release job'
[[ $release_job == *'# deploy-tarball-command:start'* ]] || fail 'the build command is not a step of the release job'
grep -qx '    runs-on: ubuntu-24.04-arm' <<<"$release_job" || fail 'the release job does not run on ubuntu-24.04-arm'

# step_holding TEXT [JOB] prints the step of JOB, the release job by default, that holds TEXT, from
# its "- " line.
step_holding() {
  awk -v text="$1" '
    /^      - / { if (found) exit; step = "" }
    { step = step $0 "\n" }
    index($0, text) { found = 1 }
    END { if (found) printf "%s", step; exit !found }' <<<"${2:-$release_job}"
}
# line_of TEXT prints the number of the first line of the release job that holds TEXT.
line_of() { grep -nF -m1 -- "$1" <<<"$release_job" | cut -d: -f1; }
# with_value STEP KEY prints the value of KEY in the with: block of STEP, and fails when it sets none.
with_value() {
  awk -v key="$2:" '
    /^ *with:$/ { match($0, /^ */); indent = RLENGTH; inside = 1; next }
    inside { match($0, /^ */); if (RLENGTH <= indent) inside = 0 }
    inside && $1 == key { sub(/^ *[^ ]+ */, ""); print; found = 1 }
    END { exit !found }' <<<"$1"
}

name="the release job builds the tarball after the binary's self-test and uploads it after"
selftest_line=$(line_of 'kws-selftest') || fail 'the release job runs no kws-selftest'
build_line=$(line_of '# deploy-tarball-command:start') || fail 'the release job has no build command'
upload_line=$(line_of 'uses: actions/upload-artifact@') || fail 'the release job has no upload step'
((selftest_line < build_line && build_line < upload_line)) \
  || fail "the self-test is at line $selftest_line, the build at $build_line and the upload at $upload_line of the job"

name="the release job's build step names the commit that the binary is built from"
binary_step=$(step_holding 'go build') || fail 'the release job has no step that runs go build'
tarball_step=$(step_holding '# deploy-tarball-command:start') || fail 'the release job has no build command'
binary_commit=$(grep -x ' *BUILD_SHA: .*' <<<"$binary_step") || fail "the binary's step sets no BUILD_SHA"
tarball_commit=$(grep -x ' *BUILD_SHA: .*' <<<"$tarball_step") || fail 'the build step sets no BUILD_SHA'
[[ $tarball_commit == "$binary_commit" ]] \
  || fail "the build step sets [${tarball_commit#"${tarball_commit%%[! ]*}"}], not the binary's [${binary_commit#"${binary_commit%%[! ]*}"}]"

name="the release job's step builds the tarball of the release's commit and binary"
# A checkout after the binary's build and self-test: the repository, the binary and its .sha256.
checkout=$work/checkout
mkdir -- "$checkout"
ln -s -- "$repo_root/deploy" "$checkout/deploy"
ln -s -- "$repo_root/wait-for-network.sh" "$checkout/wait-for-network.sh"
cp -p -- "$repo_root/THIRD_PARTY_NOTICES.md" "$checkout/"
fake_binary "$checkout/ghost-wispr-linux-arm64" '[1,2]' '' 0
(cd -- "$checkout" && sha256sum ghost-wispr-linux-arm64 >ghost-wispr-linux-arm64.sha256)
(cd -- "$checkout" && BUILD_SHA=$commit bash --noprofile --norc -eo pipefail "$work/build-step.sh") \
  || fail "the step exited $?"
check_sum_asset "$checkout" || fail "the step's .sha256 does not name its tarball"
check_members "$checkout/ghost-wispr-pi-deploy.tar.gz" || fail "the step's tarball does not hold the deploy set"
step_dir=$(unpack "$checkout/ghost-wispr-pi-deploy.tar.gz" "$work/step.unpacked")
check_set "$step_dir" || fail "the files of the step's tarball are not the deploy set"
check_manifest "$step_dir" "$commit" "$checkout/ghost-wispr-linux-arm64" || fail "the step's manifest does not match"

name="the publish job needs the release job and downloads its artifact"
publish_job=$(job_block publish) || fail 'release.yml has no publish job'
needs=$(grep -x '    needs: .*' <<<"$publish_job") || fail 'the publish job has no needs: line'
# One job (needs: release) or a flow list (needs: [lint, release]).
needs=${needs#*needs: } && needs=${needs#'['} && needs=${needs%']'}
[[ ",${needs// /}," == *,release,* ]] || fail "the publish job needs [$needs], not the release job"
uploads=$(grep -cF 'uses: actions/upload-artifact@' <<<"$release_job") || uploads=0
downloads=$(grep -cF 'uses: actions/download-artifact@' <<<"$publish_job") || downloads=0
((uploads == 1 && downloads == 1)) \
  || fail "the release job has $uploads upload-artifact steps and the publish job $downloads download-artifact steps, not one each"
upload_step=$(step_holding 'uses: actions/upload-artifact@')
download_step=$(step_holding 'uses: actions/download-artifact@' "$publish_job")
upload_name=$(with_value "$upload_step" name) || fail 'the upload step names no artifact'
download_name=$(with_value "$download_step" name) \
  || fail 'the download step names no artifact, so it would put each artifact in a directory of its own'
[[ $download_name == "$upload_name" ]] \
  || fail "the publish job downloads the artifact $download_name, but the release job uploads $upload_name"
if download_path=$(with_value "$download_step" path); then
  fail "the download step sets path: $download_path, but the publish command reads the assets from the job's working directory"
fi
if grep -q 'working-directory:' <<<"$publish_job"; then
  fail "the publish job sets a working-directory, but the download step puts the assets in the job's working directory"
fi

name='the release job uploads the five assets to the publish job'
upload_paths=$(awk '
  /^ *path: [|]$/ { paths = 1; match($0, /^ */); indent = RLENGTH; next }
  paths { match($0, /^ */); if (RLENGTH <= indent) exit; sub(/^ +/, ""); print }' <<<"$upload_step")
[[ $(sorted_lines "$upload_paths") == "$(sorted_lines "${expected_assets[@]}")" ]] \
  || fail "the upload step $(differences "$(sorted_lines "${expected_assets[@]}")" "$(sorted_lines "$upload_paths")")"
# This copy stands in for the handoff between the two jobs, which the test cannot run: the release
# job's upload-artifact step and the publish job's download-artifact step. The checks above are what
# makes the copy faithful: the publish job needs the release job, downloads the artifact of the
# upload's name with no path, and runs the publish command in the directory the download fills.
publish=$work/publish
mkdir -- "$publish"
while IFS= read -r path; do
  [[ -f $checkout/$path ]] || fail "the upload step names $path, which the release job did not write"
  cp -p -- "$checkout/$path" "$publish/"
done <<<"$upload_paths"

name='the publish job releases the five assets'
mkdir -- "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$FAKE_GH_ARGS"
EOF
chmod 0755 "$work/bin/gh"
workflow_block release-publish-command >"$work/publish.sh" \
  || fail "release.yml has no command between '# release-publish-command:start' and '# release-publish-command:end'"
# The publish command creates the release with the tag that the release job names, which
# deploy/bridge/release_images_test.sh tests.
readonly release_tag=v2026.11.26-0123456
(
  cd -- "$publish"
  GH_REPO=sjawhar/ghost-wispr GH_TOKEN=test BUILD_SHA=$commit TAG=$release_tag FAKE_GH_ARGS=$work/gh.args \
    PATH="$work/bin:$PATH" bash --noprofile --norc -eo pipefail "$work/publish.sh"
) || fail "the publish command exited $?"
mapfile -t gh_args <"$work/gh.args"
[[ ${gh_args[0]:-} == release && ${gh_args[1]:-} == create ]] || fail "the publish command ran gh ${gh_args[*]}, not gh release create"
[[ ${gh_args[2]:-} == "$release_tag" ]] || fail "the publish command names the release [${gh_args[2]:-}], not its TAG $release_tag"
published=()
for ((i = 3; i < ${#gh_args[@]}; i++)); do
  [[ ${gh_args[i]} != -* ]] || break
  published+=("${gh_args[i]}")
done
[[ $(sorted_lines "${published[@]}") == "$(sorted_lines "${expected_assets[@]}")" ]] \
  || fail "the release's assets $(differences "$(sorted_lines "${expected_assets[@]}")" "$(sorted_lines "${published[@]}")")"
for asset in "${published[@]}"; do
  [[ -f $publish/$asset ]] || fail "the release names $asset, which the publish job does not have"
done
[[ " ${gh_args[*]} " == *" --target $commit "* ]] || fail "the release does not target $commit: gh ${gh_args[*]}"

name="the published tarball's manifest names the published binary"
check_sum_asset "$publish" || fail "the published .sha256 does not name the published tarball"
published_dir=$(unpack "$publish/ghost-wispr-pi-deploy.tar.gz" "$work/published.unpacked")
check_manifest "$published_dir" "$commit" "$publish/ghost-wispr-linux-arm64" || fail "the published manifest does not match"

printf 'release publish test passed: the deploy set, %d failing link-versions answers, the stage, and the workflow\n' \
  "${#failed_cases[@]}"
