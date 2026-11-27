#!/usr/bin/env bash
# Tests the call bridge's two images (dispatch://INFRA-5904, the plan's task T3.6 and contract C7):
# deploy/bridge/Dockerfile, and the bridge-images job of .github/workflows/release.yml, which builds
# it twice. ghost-wispr-bridge is the release build. ghost-wispr-bridge-test is the callfault build
# that the test pool runs: its binary holds the run spec's reader, the fake Pi and the network probe.
#
# The job builds both images on each pull request and on the workflow_run of main, and pushes both
# to ghcr.io only on that workflow_run. The release job names the release's tag in one step, and the
# job's build and push and the publish job's gh release create read that step's output, so jobs
# that run on either side of midnight UTC give the release and its two images one tag. The job and
# the publish job each need only the release job: a failed push of the images does not hold back the
# Pi's release. The Dockerfile starts from images pinned by digest, builds with go.mod's Go, and
# runs the bridge as the user nonroot.
#
# docker, gh and date are fakes that record their arguments, so a pass shows no image built and none
# pushed. Each check also runs against a copy with the fault it must see, and must fail there.
set -euo pipefail
shopt -s nullglob

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly workflow="$repo_root/.github/workflows/release.yml"
readonly dockerfile="$repo_root/deploy/bridge/Dockerfile"
readonly commit=0123456789abcdef0123456789abcdef01234567
readonly token=test-token-for-ghcr
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

name=setup
fail() {
  printf 'release images test (%s): %s\n' "$name" "$*" >&2
  exit 1
}
# why REASON prints the reason a check fails, and fails.
why() {
  printf '%s\n' "$*" >&2
  return 1
}
# sorted_lines LINE... prints each LINE in byte order.
sorted_lines() { printf '%s\n' "$@" | LC_ALL=C sort; }

# The step that names the tag runs in the last second of 2026-11-26 UTC, and each step that reads it
# runs in the first second of 2026-11-27: a step that named its own tag would name another.
before_midnight=$(date -u -d 2026-11-26T23:59:59Z +%s)
after_midnight=$(date -u -d 2026-11-27T00:00:01Z +%s)
readonly before_midnight after_midnight
readonly expected_tag=v2026.11.26-0123456
# The owner as the runner could spell it; ghcr.io takes only lower case.
readonly owner=Sjawhar
readonly release_image=ghcr.io/sjawhar/ghost-wispr-bridge:$expected_tag
readonly test_image=ghcr.io/sjawhar/ghost-wispr-bridge-test:$expected_tag

# The fakes. date prints the time FAKE_NOW, in seconds since the epoch, as the real date would.
# docker writes its arguments, one per line, to a new file in FAKE_DOCKER, numbered in call order.
# gh writes its arguments, one per line, to FAKE_GH.
real_date=$(command -v date)
readonly real_date
mkdir -- "$work/bin"
# shellcheck disable=SC2016 # The fake's own shell expands FAKE_NOW and its arguments.
printf '#!/usr/bin/env bash\nexec %q -d "@${FAKE_NOW:?}" "$@"\n' "$real_date" >"$work/bin/date"
cat >"$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
shopt -s nullglob
calls=("${FAKE_DOCKER:?}"/*.args)
printf '%s\n' "$@" >"$(printf '%s/%03d.args' "$FAKE_DOCKER" "$((${#calls[@]} + 1))")"
EOF
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"${FAKE_GH:?}"
EOF
chmod 0755 "$work/bin/date" "$work/bin/docker" "$work/bin/gh"
[[ $(FAKE_NOW=$before_midnight "$work/bin/date" -u +%Y.%m.%d) == 2026.11.26 &&
  $(FAKE_NOW=$after_midnight "$work/bin/date" -u +%Y.%m.%d) == 2026.11.27 ]] \
  || fail "the fake date does not print 2026.11.26 and 2026.11.27 for the two times"

# --- Reading the workflow. Each helper reads the text it is given, so a check reads a copy too.

# job_block WORKFLOW NAME prints the lines of the job NAME of WORKFLOW, after its "  NAME:" line.
job_block() {
  awk -v job="  $2:" '
    $0 == job { inside = 1; found = 1; next }
    /^  [A-Za-z0-9_-]+:$/ { inside = 0 }
    inside { print }
    END { exit !found }' "$1"
}
# job_key JOB KEY prints the line "    KEY: ..." of JOB and the lines that continue its value.
job_key() {
  awk -v key="    $2:" '
    index($0, key) == 1 { inside = 1; found = 1; print; next }
    inside && /^      / { print; next }
    { inside = 0 }
    END { exit !found }' <<<"$1"
}
# step_holding JOB TEXT prints the step of JOB that holds TEXT, from its "- " line.
step_holding() {
  awk -v text="$2" '
    /^      - / { if (found) exit; step = "" }
    { step = step $0 "\n" }
    index($0, text) { found = 1 }
    END { if (found) printf "%s", step; exit !found }' <<<"$1"
}
# step_key STEP KEY prints the value of the key KEY of STEP, and fails when it sets none.
step_key() {
  awk -v key="$2:" '
    { line = $0; sub(/^      - /, "        ", line) }
    line ~ /^        [^ ]/ && substr(line, 9, length(key)) == key {
      value = substr(line, 9 + length(key)); sub(/^ */, "", value); print value; found = 1 }
    END { exit !found }' <<<"$1"
}
# step_map STEP MAP KEY prints the value of KEY in the MAP: block (env or with) of STEP, and fails
# when it sets none.
step_map() {
  awk -v map="        $2:" -v key="$3:" '
    $0 == map { inside = 1; next }
    inside && !/^          / { inside = 0 }
    inside && $1 == key { sub(/^ *[^ ]+ */, ""); print; found = 1 }
    END { exit !found }' <<<"$1"
}
# workflow_block WORKFLOW MARKER prints the lines of WORKFLOW between "# MARKER:start" and
# "# MARKER:end", with the step's indentation removed: the command that the step runs.
workflow_block() {
  awk -v start="# $2:start" -v end="# $2:end" '
    index($0, start) { capture = 1; found = 1; next }
    capture && index($0, end) { ended = 1; exit }
    capture { sub(/^          /, ""); print }
    END { exit !(found && ended) }' "$1"
}
# run_block WORKFLOW MARKER DIR NAME=VALUE... runs the command of MARKER in DIR as the step's bash
# runs it, with the fakes first on PATH and only NAME=VALUE... besides PATH and HOME set.
run_block() {
  local wf=$1 marker=$2 dir=$3
  shift 3
  workflow_block "$wf" "$marker" >"$dir/$marker.sh" \
    || why "release.yml has no command between '# $marker:start' and '# $marker:end'" || return 1
  (cd -- "$dir" && env -i PATH="$work/bin:$PATH" HOME="$HOME" "$@" bash --noprofile --norc -eo pipefail "$marker.sh") \
    || why "the command $marker exited $?"
}

# --- Reading the docker calls.

# build_line FILE prints the docker build call recorded in FILE as "IMAGE BUILD_TAGS DOCKERFILE
# PLATFORM CONTEXT", with "-" for a value it does not set. It fails for a call that is not a build,
# that tags two images, or that would push or export the image.
build_line() {
  local -a args
  mapfile -t args <"$1"
  [[ ${args[0]:-} == build ]] || why "the build ran docker ${args[*]}, which is not a docker build" || return 1
  local image=- tags=- file=- platform=- context=- i
  for ((i = 1; i < ${#args[@]}; i++)); do
    case ${args[i]} in
      --tag)
        [[ $image == - ]] || why "docker ${args[*]} tags two images" || return 1
        image=${args[++i]}
        ;;
      --build-arg)
        [[ ${args[++i]} == BUILD_TAGS=* ]] || why "docker ${args[*]} sets the build argument ${args[i]}" || return 1
        tags=${args[i]#BUILD_TAGS=} && tags=${tags:--}
        ;;
      --file) file=${args[++i]} ;;
      --platform) platform=${args[++i]} ;;
      --label) i=$((i + 1)) ;;
      --push | --output | --output=* | --load) why "docker ${args[*]} would push or export the image" || return 1 ;;
      -*) why "docker ${args[*]} has the option ${args[i]}, which this test does not read" || return 1 ;;
      *)
        [[ $context == - ]] || why "docker ${args[*]} names two contexts" || return 1
        context=${args[i]}
        ;;
    esac
  done
  printf '%s %s %s %s %s\n' "$image" "$tags" "$file" "$platform" "$context"
}
# calls DIR prints each docker call recorded in DIR on one line, in call order.
calls() {
  local call
  for call in "$1"/*.args; do paste -sd ' ' "$call"; done
}

# --- The checks.

# job_needs JOB prints the jobs that JOB needs, separated by commas, or nothing.
job_needs() {
  local needs
  needs=$(grep -x '    needs: .*' <<<"$1") || return 0
  needs=${needs#*needs: } && needs=${needs#'['} && needs=${needs%']'}
  printf '%s\n' "${needs// /}"
}

# check_wiring WORKFLOW: the release job names the release's tag in one step, from the commit it
# builds and on each run of the job, and gives that step's output as its own. The bridge-images job
# and the publish job each need the release job and read that output, and the publish job does not
# need the bridge-images job. The bridge-images job builds the commit the release job builds.
check_wiring() {
  local wf=$1 job release publish step id want_sha want_ref value marker needs
  # shellcheck disable=SC2016 # The workflow's expression, which GitHub expands.
  local -r want_tag='${{ needs.release.outputs.tag }}'
  job=$(job_block "$wf" bridge-images) || why 'release.yml has no bridge-images job' || return 1
  release=$(job_block "$wf" release) || why 'release.yml has no release job' || return 1
  publish=$(job_block "$wf" publish) || why 'release.yml has no publish job' || return 1
  want_sha=$(step_map "$(step_holding "$release" 'go build')" env BUILD_SHA) \
    || why "the release job's go build step sets no BUILD_SHA" || return 1
  want_ref=$(step_map "$(step_holding "$release" 'uses: actions/checkout@')" with ref) \
    || why 'the release job checks out no ref' || return 1
  step=$(step_holding "$job" 'uses: actions/checkout@') || why 'the bridge-images job checks out nothing' || return 1
  value=$(step_map "$step" with ref) || value=
  [[ $value == "$want_ref" ]] || why "the bridge-images job checks out [$value], not the release job's [$want_ref]" || return 1

  step=$(step_holding "$release" '# release-tag-command:start') \
    || why "the release job has no step that names the release's tag (# release-tag-command)" || return 1
  id=$(step_key "$step" id) || why "the step that names the release's tag has no id" || return 1
  if value=$(step_key "$step" if); then
    why "the step that names the release's tag runs only on [$value], not on every run of the release job" || return 1
  fi
  value=$(step_map "$step" env BUILD_SHA) || value=
  [[ $value == "$want_sha" ]] \
    || why "the step that names the release's tag sets BUILD_SHA [$value], not the release job's [$want_sha]" || return 1
  value=$(job_key "$release" outputs) || value=
  [[ $value == "    outputs:"$'\n'"      tag: \${{ steps.$id.outputs.tag }}" ]] \
    || why "the release job's outputs are [${value//$'\n'/ }], not the tag of its step $id alone" || return 1

  needs=$(job_needs "$job")
  [[ ",$needs," == *,release,* ]] || why "the bridge-images job needs [$needs], not the release job, whose tag it reads" || return 1
  for marker in bridge-images-build-command bridge-images-push-command; do
    step=$(step_holding "$job" "# $marker:start") \
      || why "the bridge-images job has no command between '# $marker:start' and '# $marker:end'" || return 1
    value=$(step_map "$step" env TAG) || value=
    [[ $value == "$want_tag" ]] || why "the step of $marker sets TAG [$value], not the release job's output" || return 1
  done
  value=$(step_map "$(step_holding "$job" '# bridge-images-build-command:start')" env BUILD_SHA) || value=
  [[ $value == "$want_sha" ]] || why "the build step sets BUILD_SHA [$value], not the release job's [$want_sha]" || return 1

  needs=$(job_needs "$publish")
  [[ ",$needs," == *,release,* ]] || why "the publish job needs [$needs], not the release job" || return 1
  [[ ",$needs," != *,bridge-images,* ]] \
    || why "the publish job needs [$needs]: a failed push of the images would hold back the Pi's release" || return 1
  step=$(step_holding "$publish" '# release-publish-command:start') || why 'the publish job has no publish command' || return 1
  value=$(step_map "$step" env TAG) || value=
  [[ $value == "$want_tag" ]] || why "the publish step sets TAG [$value], not the release job's output" || return 1
}

# run_commands WORKFLOW DIR runs the tag command, the commands of the bridge-images job and the
# publish command in DIR, each with what its step sets: the tag's in the last second of 2026-11-26
# UTC, each other in the first second of 2026-11-27, given the tag the first wrote, as check_wiring
# requires. DIR/tag holds the tag, DIR/build and DIR/push the docker calls of the build and of the
# push, and DIR/gh.args gh's arguments.
run_commands() {
  local wf=$1 dir=$2 tag
  mkdir -p -- "$dir/build" "$dir/push"
  : >"$dir/output"
  run_block "$wf" release-tag-command "$dir" FAKE_NOW="$before_midnight" BUILD_SHA="$commit" GITHUB_OUTPUT="$dir/output" \
    || return 1
  tag=$(sed -n 's/^tag=//p' "$dir/output")
  [[ $(grep -c '' "$dir/output") == 1 && -n $tag ]] \
    || why "the step that names the release's tag wrote [$(paste -sd ' ' "$dir/output")], not one line tag=..." || return 1
  printf '%s\n' "$tag" >"$dir/tag"
  local -a runner=(FAKE_NOW="$after_midnight" TAG="$tag" BUILD_SHA="$commit" GITHUB_SERVER_URL=https://github.com
    GITHUB_REPOSITORY="$owner/ghost-wispr" GITHUB_REPOSITORY_OWNER="$owner" GITHUB_ACTOR=sjawhar-agent)
  run_block "$wf" bridge-images-build-command "$dir" "${runner[@]}" FAKE_DOCKER="$dir/build" || return 1
  run_block "$wf" bridge-images-push-command "$dir" "${runner[@]}" FAKE_DOCKER="$dir/push" GH_TOKEN="$token" || return 1
  run_block "$wf" release-publish-command "$dir" "${runner[@]}" GH_REPO="$owner/ghost-wispr" GH_TOKEN="$token" \
    FAKE_GH="$dir/gh.args" || return 1
}

# check_tag DIR: the tag is the release's tag of the commit, and gh release create names and titles
# the release with it.
check_tag() {
  local dir=$1 tag
  local -a gh_args
  tag=$(<"$dir/tag")
  [[ $tag == "$expected_tag" ]] || why "the job named the release's tag [$tag], not [$expected_tag]" || return 1
  mapfile -t gh_args <"$dir/gh.args"
  [[ ${gh_args[0]:-} == release && ${gh_args[1]:-} == create ]] || why "the publish command ran gh ${gh_args[*]}, not gh release create" || return 1
  [[ ${gh_args[2]:-} == "$tag" ]] || why "gh release create names the release [${gh_args[2]:-}], not the images' tag [$tag]" || return 1
  [[ " ${gh_args[*]} " == *" --title $tag "* ]] || why "gh release create does not title the release $tag: gh ${gh_args[*]}" || return 1
}

# check_builds DIR: the build made two images, from deploy/bridge/Dockerfile for linux/amd64 with the
# repository as the context, and pushed none: the release image with no build tags, and the test
# image with callfault alone, each tagged with the release's tag.
check_builds() {
  local dir=$1 call line have want
  local -a lines=()
  for call in "$dir"/build/*.args; do
    line=$(build_line "$call") || return 1
    lines+=("$line")
  done
  have=$(sorted_lines "${lines[@]}")
  want=$(sorted_lines "$release_image - deploy/bridge/Dockerfile linux/amd64 ." \
    "$test_image callfault deploy/bridge/Dockerfile linux/amd64 .")
  [[ $have == "$want" ]] || why "the build made [${have//$'\n'/; }], not [${want//$'\n'/; }]" || return 1
}

# check_push WORKFLOW DIR: the job runs on the release job's condition, so on pull requests and on
# the workflow_run of main alone. It builds on both, and pushes only on the workflow_run: its steps
# are the checkout and the two commands, and only the push step has an if:. The push signs in to
# ghcr.io with the token on stdin, and pushes the two images the build made.
check_push() {
  local wf=$1 dir=$2 job release step value have want
  job=$(job_block "$wf" bridge-images) || why 'release.yml has no bridge-images job' || return 1
  release=$(job_block "$wf" release) || why 'release.yml has no release job' || return 1
  have=$(job_key "$job" if) || have=
  want=$(job_key "$release" if) || want=
  [[ $have == "$want" ]] || why "the bridge-images job runs on [${have//$'\n'/ }], not the release job's [${want//$'\n'/ }]" || return 1
  step=$(step_holding "$job" '# bridge-images-push-command:start') || why 'the bridge-images job has no push command' || return 1
  value=$(step_key "$step" if) || value=
  [[ $value == "github.event_name == 'workflow_run'" ]] || why "the push step runs on [$value], not only on the workflow_run" || return 1
  step=$(step_holding "$job" '# bridge-images-build-command:start') || why 'the bridge-images job has no build command' || return 1
  if value=$(step_key "$step" if); then
    why "the build step runs only on [$value], not on every run of the job" || return 1
  fi
  # Each step is "- uses: ..." or "- name: ..." with a run: below it.
  have=$(awk '/^      - / { n++ } /^      - uses: |^        uses: / { sub(/@.*/, ""); sub(/^ *-? *uses: /, ""); print "uses " $0 }
    /^        run: / { print "run" } END { print n " steps" }' <<<"$job" | paste -sd ' ')
  [[ $have == 'uses actions/checkout run run 3 steps' ]] \
    || why "the bridge-images job's steps are [$have], not the checkout and the two commands" || return 1

  have=$(calls "$dir/push")
  want="login ghcr.io --username sjawhar-agent --password-stdin"$'\n'"push $release_image"$'\n'"push $test_image"
  [[ $have == "$want" ]] || why "the push ran [${have//$'\n'/; }], not [${want//$'\n'/; }]" || return 1
  if grep -rqF -- "$token" "$dir/build" "$dir/push"; then
    why 'a docker call has the token on its command line' || return 1
  fi
}

# check_job WORKFLOW: the bridge-images job runs on ubuntu-24.04, which builds linux/amd64 without
# emulation, and is the one job that may write packages; the workflow's own permissions stay read.
check_job() {
  local wf=$1 job value
  job=$(job_block "$wf" bridge-images) || why 'release.yml has no bridge-images job' || return 1
  value=$(job_key "$job" runs-on) || value=
  [[ $value == '    runs-on: ubuntu-24.04' ]] || why "the bridge-images job has [${value# *}], not runs-on: ubuntu-24.04" || return 1
  value=$(job_key "$job" permissions) || value=
  [[ $value == $'    permissions:\n      contents: read\n      packages: write' ]] \
    || why "the bridge-images job's permissions are [${value//$'\n'/ }], not contents: read and packages: write" || return 1
  [[ $(grep -c 'packages:' "$wf") == 1 ]] || why "release.yml grants packages: outside the bridge-images job" || return 1
  value=$(awk '/^permissions:$/ { inside = 1; next } inside && /^  / { print; next } { inside = 0 }' "$wf" | paste -sd ' ')
  [[ $value == '  contents: read' ]] || why "the workflow's own permissions are [$value], not contents: read" || return 1
}

# check_workflow WORKFLOW DIR runs each check of the workflow, in order.
check_workflow() {
  check_wiring "$1" && run_commands "$1" "$2" && check_tag "$2" && check_builds "$2" && check_push "$1" "$2" && check_job "$1"
}

# check_dockerfile DOCKERFILE: each image a stage starts from is pinned by digest, and each other
# reference to a stage names a stage. The builder is golang of go.mod's Go, and builds the bridge
# with no cgo for linux/amd64 with the build tags of the build argument BUILD_TAGS, which sets none
# by default. The image is distroless/static-debian12, runs as the user nonroot and starts the bridge.
check_dockerfile() {
  local df=$1 text line ref stage first='' last='' go_version from want i
  local -a words stages=()
  [[ -f $df ]] || why "${df#"$repo_root"/} does not exist" || return 1
  # The instructions, each on one line: a line ending in a backslash is joined to the next.
  text=$(awk '{ if (sub(/\\$/, "")) { held = held $0; next } print held $0; held = "" }' "$df")
  if line=$(grep -iE '^# *syntax=' <<<"$text"); then
    [[ $line =~ @sha256:[0-9a-f]{64}$ ]] || why "the frontend [$line] is not pinned by digest" || return 1
  fi
  while IFS= read -r line; do
    [[ $line =~ ^[Ff][Rr][Oo][Mm][[:space:]] ]] || continue
    read -r -a words <<<"$line"
    ref='' stage=''
    for ((i = 1; i < ${#words[@]}; i++)); do
      if [[ -z $ref && ${words[i]} == --* ]]; then continue; fi
      if [[ -z $ref ]]; then ref=${words[i]}; elif [[ ${words[i]^^} == AS ]]; then stage=${words[i + 1]:-}; break; fi
    done
    if [[ " ${stages[*]} " != *" $ref "* ]]; then
      [[ $ref =~ ^[^@]+@sha256:[0-9a-f]{64}$ ]] || why "the stage FROM $ref is not pinned by digest" || return 1
    fi
    if [[ -n $stage ]]; then stages+=("$stage"); fi
    first=${first:-$ref}
    last=$ref
  done <<<"$text"
  [[ -n $first ]] || why "${df#"$repo_root"/} has no FROM" || return 1
  while IFS= read -r from; do
    [[ " ${stages[*]} " == *" ${from#*from=} "* ]] || why "the Dockerfile reads $from, which is no stage" || return 1
  done < <(grep -oiE '(--from=|,from=)[^ ,]+' <<<"$text")

  go_version=$(awk '$1 == "go" { print $2 }' "$repo_root/go.mod") || go_version=
  [[ $go_version =~ ^([0-9]+\.[0-9]+) ]] || why "go.mod names no go version: [$go_version]" || return 1
  go_version=${BASH_REMATCH[1]}
  [[ $first =~ ^golang:${go_version//./\\.}(\.[0-9]+)?(-[a-z0-9.]+)?@ ]] \
    || why "the builder is $first, not golang of go.mod's Go $go_version" || return 1
  [[ $last =~ ^gcr\.io/distroless/static-debian12(:[a-z0-9.-]+)?@ ]] \
    || why "the image starts from $last, not gcr.io/distroless/static-debian12" || return 1

  grep -qxE 'ARG BUILD_TAGS(=)?' <<<"$text" || why 'the Dockerfile has no line ARG BUILD_TAGS that sets no build tags by default' || return 1
  line=$(grep -E '(^|[ ;&])go build ' <<<"$text") || why 'the Dockerfile runs no go build' || return 1
  # shellcheck disable=SC2016 # Each is the Dockerfile's text, which its shell expands.
  for want in 'CGO_ENABLED=0 ' 'GOOS=linux ' 'GOARCH=amd64 ' ' -tags "$BUILD_TAGS" ' ' -o /ghost-wispr-bridge ' ' ./cmd/ghost-wispr-bridge'; do
    [[ "$line " == *"$want"* ]] || why "the go build [$line] has no [${want# }]" || return 1
  done
  # The instructions of the last stage, after its FROM.
  text=$(awk 'toupper($1) == "FROM" { stage = "" ; next } { stage = stage $0 "\n" } END { printf "%s", stage }' <<<"$text")
  grep -qx 'USER nonroot' <<<"$text" || why 'the image does not run as USER nonroot' || return 1
  grep -qxF 'ENTRYPOINT ["/ghost-wispr-bridge"]' <<<"$text" || why 'the image does not start ENTRYPOINT ["/ghost-wispr-bridge"]' || return 1
}

# --- The workflow and the Dockerfile.

name="the release job names the release's tag in one step, which the images' build and push and the publish job read"
check_wiring "$workflow" || fail 'the tag does not come from one step'

name="the tag command, the job's commands and the publish command run"
run_commands "$workflow" "$work/run" || fail 'a command failed'

name='the release and both images carry the tag named before midnight, though they run after it'
check_tag "$work/run" || fail 'the release does not carry the tag'

name='the release image has no build tags, and the test image -tags callfault alone'
check_builds "$work/run" || fail 'the build did not make the two images'

name='the job pushes both images only on the workflow_run of main'
check_push "$workflow" "$work/run" || fail 'the push is not the workflow_run of main'

name='only the bridge-images job writes packages, and it runs on ubuntu-24.04'
check_job "$workflow" || fail 'the job is not the one that writes packages'

name='the Dockerfile pins each image by digest and builds the bridge'
check_dockerfile "$dockerfile" || fail 'the Dockerfile is not the bridge image'

# --- The checks themselves, against the fault each must see.

controls=0
# expect_control KIND CASE REASON SED_SCRIPT: a copy of the workflow (KIND workflow) or of the
# Dockerfile (KIND dockerfile) that SED_SCRIPT changes fails its checks, for a reason that holds
# REASON.
expect_control() {
  local kind=$1 case=$2 reason=$3 script=$4 source copy
  name="control: $case"
  controls=$((controls + 1))
  case $kind in
    workflow) source=$workflow ;;
    dockerfile) source=$dockerfile ;;
  esac
  copy=$work/control-$controls
  mkdir -- "$copy"
  sed -e "$script" "$source" >"$copy/file"
  ! cmp -s -- "$source" "$copy/file" || fail "the control's edit [$script] changed nothing"
  if [[ $kind == workflow ]]; then
    if check_workflow "$copy/file" "$copy" 2>"$copy/why"; then fail 'the checks passed the copy'; fi
  elif check_dockerfile "$copy/file" 2>"$copy/why"; then
    fail 'the checks passed the copy'
  fi
  grep -qF -- "$reason" "$copy/why" || fail "the checks failed the copy, but not for [$reason]: $(<"$copy/why")"
}

# shellcheck disable=SC2016 # Each edit is the workflow's text, which its step's shell expands.
expect_control workflow 'the publish job names its own tag after midnight' 'gh release create names the release [v2026.11.27-' \
  's|# release-publish-command:start|&\n          TAG="v$(date -u +%Y.%m.%d)-${BUILD_SHA:0:7}"|'
# shellcheck disable=SC2016 # Each edit is the workflow's text, which its step's shell expands.
expect_control workflow 'the build names its own tag after midnight' 'ghost-wispr-bridge:v2026.11.27-0123456' \
  's|# bridge-images-build-command:start|&\n          TAG="v$(date -u +%Y.%m.%d)-${BUILD_SHA:0:7}"|'
# shellcheck disable=SC2016 # Each edit is the workflow's text, which its step's shell expands.
expect_control workflow 'the release image built with callfault' "$release_image callfault" \
  's|--tag "$images/ghost-wispr-bridge:|--build-arg BUILD_TAGS=callfault &|'
expect_control workflow 'the test image built without callfault' "$test_image - " \
  's|--build-arg BUILD_TAGS=callfault ||'
# shellcheck disable=SC2016 # Each edit is the workflow's text, which its step's shell expands.
expect_control workflow 'a build step that pushes' 'which is not a docker build' \
  's|# bridge-images-build-command:start|&\n          docker push "ghcr.io/sjawhar/ghost-wispr-bridge:$TAG"|'
expect_control workflow 'a push step on every run' 'the push step runs on []' \
  "/^        if: github.event_name == 'workflow_run'\$/d"
expect_control workflow 'a publish job that waits for the images' 'would hold back the Pi' \
  '/^  publish:$/,/^    needs:/ s|^    needs: release$|    needs: [release, bridge-images]|'
expect_control workflow 'an images job that does not need the release job' 'not the release job, whose tag it reads' \
  '/^  bridge-images:$/,/^    needs:/ { /^    needs: release$/d }'
expect_control workflow 'packages: write for the whole workflow' 'outside the bridge-images job' \
  's|^  contents: read$|&\n  packages: write|'
expect_control dockerfile 'an image not pinned by digest' 'is not pinned by digest' \
  's|^\(FROM gcr\.io/distroless/static-debian12[^@]*\)@sha256:[0-9a-f]*|\1|'
expect_control dockerfile 'a builder of another Go' "not golang of go.mod's Go" \
  's|^FROM golang:[0-9][0-9.]*|FROM golang:1.0.0|'
expect_control dockerfile 'the callfault build by default' 'ARG BUILD_TAGS' \
  's|^ARG BUILD_TAGS.*|ARG BUILD_TAGS=callfault|'

printf 'release images test passed: the bridge-images job, the Dockerfile, and %d controls\n' "$controls"
