#!/usr/bin/env bash
# Runs docs/pi-setup.md's own commands for the credential files, the bundle sources and the
# provisioning call, read from the runbook between its provision_bundle_test.sh markers, against
# fakes that answer only the sources the devbox identity can read: the device certificate's Secret
# Manager secret, the service account's unique ID in the infrastructure repository's root stack
# configuration on its main, and the Tailscale client ID the seed workflow's root job printed when
# it created the identity. Pulumi answers 403, as the infrastructure stacks' state buckets do for
# that identity. Nothing reads an Envoy credential: the recorder publishes to the Envoy hub with
# none. The runbook and the scripts read the neutral deployment settings in deploy/pi/test-pi.env,
# not the office Pi's.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
export GHOST_WISPR_PI_ENV="$repo_root/deploy/pi/test-pi.env"
# PROVISION_BUNDLE_TEST_DOC names another copy of the runbook, to show the test fails on it.
readonly doc="${PROVISION_BUNDLE_TEST_DOC:-$repo_root/docs/pi-setup.md}"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'provision bundle test FAILED: %s\n' "$*" >&2
  exit 1
}

# block prints the bash block that follows the runbook's marker for name. The marker and the block
# may be indented, as in a list item.
block() {
  awk -v marker="<!-- provision_bundle_test.sh: $1 -->" '
    { line = $0; sub(/^ +/, "", line) }
    line == marker { found = 1; next }
    found && !capture && line == "```bash" { capture = 1; next }
    capture && line == "```" { exit }
    capture { print }
  ' "$doc"
}

for name in credentials prepare dispatch-token provision accept renew; do
  [[ -n $(block "$name") ]] || fail "docs/pi-setup.md has no bash block after <!-- provision_bundle_test.sh: $name -->"
done
# compose prints the runbook's credential, prepare and provision blocks in order, with the lines
# given as arguments between the prepare block and the provision block.
compose() {
  block credentials
  block prepare
  [[ $# -eq 0 ]] || printf '%s\n' "$@"
  block provision
}
runbook="$work_dir/runbook.sh"
compose >"$runbook"
# The meeting voice's step ("Turn on the meeting voice") mints the unit's own Dispatch token into
# $prepared/dispatch-token before the provision block runs. The other token runbooks put a link to
# nothing there, and an empty file.
compose "$(block dispatch-token)" >"$work_dir/runbook-token.sh"
# shellcheck disable=SC2016 # The lines are runbook text, expanded when the runbook runs.
compose 'ln -s "$prepared/no-such-token" "$prepared/dispatch-token"' >"$work_dir/runbook-token-link.sh"
# shellcheck disable=SC2016
compose '(umask 077 && : >"$prepared/dispatch-token")' >"$work_dir/runbook-token-empty.sh"
# The slice 1 block, with the acceptance driver replaced by one that reports what it was given.
accept="$work_dir/accept.sh"
# shellcheck disable=SC2016 # The replacement is the runbook's text, expanded when the block runs.
block accept | sed 's#^deploy/pi/accept-slice1\.sh$#"$TEST_ACCEPT_DRIVER"#' >"$accept"
grep -q TEST_ACCEPT_DRIVER "$accept" || fail "the slice 1 block does not end by running deploy/pi/accept-slice1.sh"

readonly DEEPGRAM_SECRET=deepgram-secret-value-0123456789abcdef
# The unit's Dispatch token that the fake Dispatch mints, and the operator's own token it takes.
readonly DISPATCH_SECRET=dispatch-token-secret-value-0123456789abcdef
readonly OPERATOR_DISPATCH_SECRET=operator-dispatch-secret-value-0123456789abcdef
readonly CLIENT_ID=k-test-client-id
readonly SUBJECT=123456789012345678901
deepgram_key_name="DEEPGRAM""_API_KEY"
export DEEPGRAM_SECRET DISPATCH_SECRET OPERATOR_DISPATCH_SECRET CLIENT_ID SUBJECT deepgram_key_name

mkdir -p "$work_dir/bin"
# A real certificate, so the runbook's openssl check runs for real.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=ghost-wispr -days 365 \
  -keyout "$work_dir/device.key" -out "$work_dir/device.pem" 2>/dev/null
export TEST_CERTIFICATE="$work_dir/device.pem" TEST_ARGV_LOG="$work_dir/argv.log"

# The devbox identity cannot list the infrastructure stacks' Pulumi state buckets.
cat >"$work_dir/bin/pulumi" <<'EOF'
#!/usr/bin/env bash
printf 'pulumi %s\n' "$*" >>"$TEST_ARGV_LOG"
printf 'fake pulumi: 403: the devbox identity does not have storage.objects.list access to the Pulumi state bucket\n' >&2
exit 1
EOF

# gh answers as GitHub does for the infrastructure repository's seed workflow: the runs newest
# first, two to a page here, and the jobs of each attempt.
# - 905, attempt 3: root waits for its approval (no steps, no log); attempt 2: root was cancelled
#   before it started (no steps, no log); attempt 1: skipped.
# - 904: root ran a preview, which prints the client id as [unknown].
# - 903, re-run twice: attempt 3 ran a preview again; attempt 2 replaced example-x509, so the
#   client id changed from the stale one to CLIENT_ID (with TEST_CREATION_FORM=create it created
#   it); attempt 1 created the earlier identity, whose client id is stale.
# - 902: created the earlier identity too.
# - 901: older than the 90-day log retention; it created another identity.
# TEST_JOB_RUNNING: 904's root job is still running. TEST_LOG_READ_FAIL: GitHub does not serve
# 904's log. TEST_CREATION_EXPIRED: 903 and 902 made no identity, so only 901 did.
cat >"$work_dir/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh %s\n' "$*" >>"$TEST_ARGV_LOG"
jq_filter=
args=()
while [[ $# -gt 0 ]]; do
  case $1 in
    --jq) jq_filter=$2; shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
answer() {
  if [[ -n $jq_filter ]]; then jq -r "$jq_filter"; else cat; fi
}
not_found() {
  printf 'fake gh: HTTP 404 for "%s"\n' "${args[*]}" >&2
  exit 1
}
readonly api=repos/example-org/example-infra/actions
recent=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)
expired=$(date -u -d '100 days ago' +%Y-%m-%dT%H:%M:%SZ)
run_json() { printf '{"id":%s,"run_attempt":%s,"created_at":"%s"}' "$@"; }
# root_job ID STATUS CONCLUSION STEPS: one attempt's jobs, a select job and the root job.
root_job() {
  printf '{"total_count":2,"jobs":[{"id":%s0,"name":"select","status":"completed","conclusion":"success","steps":[{"started_at":"%s"}]},' "${1%1}" "$recent"
  printf '{"id":%s,"name":"root","status":"%s","conclusion":%s,"steps":[%s]}]}\n' "$1" "$2" "$3" "$4"
}
step=$(printf '{"started_at":"%s"}' "$recent")
preview='2026-11-16T06:20:21.4658172Z   + exampleTailnetClientId                   : [unknown]'
created='2026-11-16T06:25:09.3203883Z  +  tailscale:index:FederatedIdentity example-x509 created (1s) '
created_id() { printf '2026-11-16T06:29:36.3829213Z   + exampleTailnetClientId                   : "%s"\n' "$1"; }
preview_replace='2026-11-16T06:20:21.4658172Z   ~ exampleTailnetClientId                   : "k-stale-client-id" => [unknown]'
replaced='2026-11-16T06:25:09.3203883Z  +-  tailscale:index:FederatedIdentity example-x509 replaced (1s) [diff: ~audience]'
replaced_id='2026-11-16T06:29:36.3829213Z   ~ exampleTailnetClientId                   : "k-stale-client-id" => "'"$CLIENT_ID"'"'
case "${args[*]}" in
  "api $api/workflows/seed.yaml/runs?per_page=100&page="*)
    case ${args[1]##*page=} in
      1) runs="$(run_json 905 3 "$recent"),$(run_json 904 1 "$recent")" ;;
      2) runs="$(run_json 903 3 "$recent"),$(run_json 902 1 "$recent")" ;;
      3) runs=$(run_json 901 1 "$expired") ;;
      *) runs= ;;
    esac
    printf '{"total_count":5,"workflow_runs":[%s]}\n' "$runs" | answer
    ;;
  "api $api/runs/"*"/attempts/"*"/jobs?per_page=100")
    attempt=${args[1]#"$api/runs/"}
    run=${attempt%%/*}
    attempt=${attempt#*/attempts/}
    attempt=${attempt%%/*}
    job="$run${attempt}1"
    case $run/$attempt in
      905/3) root_job "$job" waiting null '' ;;
      905/2) root_job "$job" completed '"cancelled"' '' ;;
      905/1) root_job "$job" completed '"skipped"' '' ;;
      904/1)
        if [[ -n ${TEST_JOB_RUNNING:-} ]]; then root_job "$job" in_progress null "$step"; else root_job "$job" completed '"failure"' "$step"; fi
        ;;
      903/[123] | 902/1 | 901/1) root_job "$job" completed '"success"' "$step" ;;
      *) not_found ;;
    esac | answer
    ;;
  "api $api/jobs/90411/logs")
    if [[ -n ${TEST_LOG_READ_FAIL:-} ]]; then
      printf 'fake gh: HTTP 502: Server Error\n' >&2
      exit 1
    fi
    printf '%s\n' "$preview"
    ;;
  "api $api/jobs/90331/logs") printf '%s\n' "$preview_replace" ;;
  "api $api/jobs/90321/logs")
    if [[ -n ${TEST_CREATION_EXPIRED:-} ]]; then
      printf '%s\n' "$preview"
    elif [[ ${TEST_CREATION_FORM:-} == create ]]; then
      printf '%s\n' "$preview" "$created" "$(created_id "$CLIENT_ID")"
    else
      printf '%s\n' "$preview_replace" "$replaced" "$replaced_id"
    fi
    ;;
  "api $api/jobs/90311/logs" | "api $api/jobs/90211/logs")
    if [[ -n ${TEST_CREATION_EXPIRED:-} ]]; then
      printf '%s\n' "$preview"
    else
      printf '%s\n' "$preview" "$created" "$(created_id k-stale-client-id)"
    fi
    ;;
  "api $api/jobs/90111/logs") printf '%s\n' "$preview" "$created" "$(created_id k-expired-client-id)" ;;
  *) not_found ;;
esac
EOF

cat >"$work_dir/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gcloud %s\n' "$*" >>"$TEST_ARGV_LOG"
certificate_config=
credentials=
for argument in "$@"; do
  case "$argument" in
    --credential-cert-configuration-output-file=*) certificate_config=${argument#*=} ;;
    --output-file=*) credentials=${argument#*=} ;;
  esac
done
if [[ $1 == iam && $2 == workload-identity-pools && $3 == create-cred-config && -n $certificate_config && -n $credentials ]]; then
  printf '{"cert_configs":{"workload":{"cert_path":"/opt/ghost-wispr/device.crt"}}}\n' >"$certificate_config"
  printf '{"type":"external_account","credential_source":{"certificate":{"certificate_config_location":"relative.json"}}}\n' >"$credentials"
  exit 0
fi
case "$*" in
  'secrets versions access latest --secret=example-device-certificate --project=example-project')
    if [[ -n ${TEST_CERT_READ_FAIL:-} ]]; then
      printf 'fake gcloud: PERMISSION_DENIED reading example-device-certificate\n' >&2
      exit 1
    fi
    cat "$TEST_CERTIFICATE"
    ;;
  *)
    printf 'fake gcloud: refused "%s"\n' "$*" >&2
    exit 1
    ;;
esac
EOF
cat >"$work_dir/bin/secrets" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'secrets %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $1 == "$deepgram_key_name" && $2 == -- ]]
shift 2
env "$deepgram_key_name=$DEEPGRAM_SECRET" "$@"
EOF

# curl answers as Dispatch does to the mint of an agent token, whose answer is the one place the
# new token shows. It answers only the operator's own token, given as a header on stdin.
cat >"$work_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"$TEST_ARGV_LOG"
expected=(-fsS -H @- -H 'Content-Type: application/json' --data '{"name":"ghost-wispr-office-pi","scopes":[]}'
  https://dispatch.example-tailnet.ts.net/api/v1/me/agent-tokens)
[[ "$*" == "${expected[*]}" ]] || { printf 'fake curl: unexpected call: %s\n' "$*" >&2; exit 2; }
[[ $(cat) == "Authorization: Bearer $OPERATOR_DISPATCH_SECRET" ]] || { printf 'fake curl: 401\n' >&2; exit 22; }
jq -n --arg token "$DISPATCH_SECRET" \
  '{id: "00000000-0000-4000-8000-000000000001", name: "ghost-wispr-office-pi", prefix: "dispatch", scopes: [], token: $token}'
EOF

# ssh reaches the fake Pi.
cat >"$work_dir/bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'ssh %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $1 == -n ]] && shift
host=$1 command=$2
[[ $host == ghost-wispr@ghost-wispr.example-tailnet.ts.net ]] || { printf 'fake ssh: unknown host %s\n' "$host" >&2; exit 255; }
case "$command" in
  *'mktemp -d'*) printf '%s\n' "$TEST_PI/sources.abc123" ; mkdir -p "$TEST_PI/sources.abc123" ;;
  *'tar -xf - -C'*) tar -xf - -C "$TEST_PI/sources.abc123" ;;
  *'/deploy/pi/provision.sh')
    received="$TEST_PI/bundle"
    rm -rf -- "$received" && mkdir -p "$received"
    tar -xf - -C "$received"
    find "$received" -mindepth 1 -printf '%P\n' | LC_ALL=C sort >"$TEST_PI/bundle.list"
    # Where the operator's machine keeps the secrets while provision.sh runs.
    find "$XDG_RUNTIME_DIR" -mindepth 1 -maxdepth 1 -name 'ghost-wispr-bundle.*' -printf '%m %P\n' >"$TEST_PI/stage.during"
    [[ -z ${TEST_PROVISION_SAYS:-} ]] || printf '%s\n' "$TEST_PROVISION_SAYS"
    exit "${TEST_PROVISION_EXIT:-0}"
    ;;
  *'rm -rf --'*) printf '%s\n' "$command" >>"$TEST_PI/removed" ;;
  *'/opt/ghost-wispr/device.crt'*) cat >"$TEST_PI/renewed.crt" ;;
  *) printf 'fake ssh: unexpected command %s\n' "$command" >&2; exit 1 ;;
esac
EOF
chmod +x "$work_dir/bin/"*

# The infrastructure repository as the runbook reads it: its origin's main holds root/stack.yaml
# with the service account's unique id. The commit before it held another id, and each case's
# checkout is left at that commit, so only a read of the fetched origin/main gives the right one.
infra_origin="$work_dir/example-infra.git"
infra_seed="$work_dir/example-infra-seed"
git init --quiet --bare --initial-branch=main "$infra_origin"
git init --quiet --initial-branch=main "$infra_seed"
mkdir "$infra_seed/root"
for subject in 987654321098765432109 "$SUBJECT"; do
  printf 'config:\n  example-root:otherSetting: "true"\n  example-root:accountUniqueId: "%s"\n' \
    "$subject" >"$infra_seed/root/stack.yaml"
  git -C "$infra_seed" add root/stack.yaml
  git -C "$infra_seed" -c user.name=test -c user.email=test@example.invalid commit --quiet -m "root: subject $subject"
done
git -C "$infra_seed" push --quiet "$infra_origin" main

# run_runbook runs the runbook's blocks in a fresh home: TEST_RUNBOOK names another composition.
run_runbook() {
  local case_dir=$1
  mkdir -p "$case_dir/home/src" "$case_dir/tmp" "$case_dir/pi"
  git clone --quiet "$infra_origin" "$case_dir/home/src/example-infra"
  git -C "$case_dir/home/src/example-infra" reset --quiet --hard HEAD~1
  git -C "$case_dir/home/src/example-infra" update-ref refs/remotes/origin/main HEAD
  mkdir -m 700 "$case_dir/run"
  (
    cd "$repo_root"
    HOME="$case_dir/home" TMPDIR="$case_dir/tmp" XDG_RUNTIME_DIR="$case_dir/run" TEST_PI="$case_dir/pi" \
      PATH="$work_dir/bin:$PATH" bash -euo pipefail "${TEST_RUNBOOK:-$runbook}"
  )
}

# check_case checks one finished run: the bundle the Pi received, where the secrets were kept,
# and that no secret is left on disk or was ever an argument. A second argument, dispatch-token,
# says $prepared held the unit's Dispatch token, which the bundle must then carry.
check_case() {
  local case_dir=$1 with=${2:-}
  local received="$case_dir/pi/bundle"
  printf '%s\n' certificate_config.json deepgram-key device.crt google-credentials.json \
    tailscale-client-id tailscale-subject ${with:+"$with"} | LC_ALL=C sort >"$case_dir/expected.list"
  cmp -s "$case_dir/expected.list" "$case_dir/pi/bundle.list" \
    || fail "$case_dir: the Pi received $(tr '\n' ' ' <"$case_dir/pi/bundle.list")"
  if [[ $with == dispatch-token ]]; then
    [[ $(<"$received/dispatch-token") == "$DISPATCH_SECRET" ]] || fail "dispatch-token is not the token \$prepared held"
  fi
  cmp -s "$received/device.crt" "$TEST_CERTIFICATE" || fail "device.crt is not the latest version of the certificate secret"
  [[ $(<"$received/tailscale-client-id") == "$CLIENT_ID" ]] \
    || fail "tailscale-client-id is not the client ID of the identity's newest creation: $(<"$received/tailscale-client-id")"
  [[ $(<"$received/tailscale-subject") == "$SUBJECT" ]] \
    || fail "tailscale-subject is not the unique ID on the infrastructure repository's origin/main: $(<"$received/tailscale-subject")"
  [[ $(<"$received/deepgram-key") == "$DEEPGRAM_SECRET" ]] || fail "deepgram-key is wrong"
  jq -e '.credential_source.certificate.certificate_config_location == "/opt/ghost-wispr/certificate_config.json"' \
    "$received/google-credentials.json" >/dev/null || fail "google-credentials.json does not name the Pi's certificate config"
  [[ $(wc -l <"$case_dir/pi/stage.during") == 1 && $(cut -d' ' -f1 "$case_dir/pi/stage.during") == 700 ]] \
    || fail "while provision.sh ran, the secrets were not in one mode-0700 directory under XDG_RUNTIME_DIR: $(<"$case_dir/pi/stage.during")"
  [[ -z $(find "$case_dir/run" -mindepth 1 -print -quit) ]] || fail "the bundle directory is left in XDG_RUNTIME_DIR"
  grep -q "rm -rf -- '$case_dir/pi/sources.abc123'" "$case_dir/pi/removed" || fail "the sources directory on the Pi was not removed"
  if grep -rlF -e "$DEEPGRAM_SECRET" "$case_dir/home" "$case_dir/tmp" "$case_dir/run" >/dev/null 2>&1; then
    fail "a secret is left on disk: $(grep -rlF -e "$DEEPGRAM_SECRET" "$case_dir/home" "$case_dir/tmp" "$case_dir/run")"
  fi
}

export TEST_ARGV_LOG
run_runbook "$work_dir/local" >"$work_dir/local.out" \
  || { cat "$work_dir/local.out"; fail "the runbook's commands failed against the sources the devbox identity reads"; }
check_case "$work_dir/local"
# The runbook says openssl must print subject=CN=ghost-wispr. OpenSSL 3.0 prints its default
# form, subject=CN = ghost-wispr, unless the command asks for RFC 2253.
grep -qx 'subject=CN=ghost-wispr' "$work_dir/local.out" \
  || fail "the runbook's certificate check did not print subject=CN=ghost-wispr: $(grep '^subject=' "$work_dir/local.out")"

# The runbook mints the unit's Dispatch token with the operator's own, and it reaches the Pi in the
# bundle, through the same mode-0700 directory under XDG_RUNTIME_DIR as the Deepgram key. Nothing
# prints either token, and the unit's is on disk only as $prepared/dispatch-token, mode 0600.
(umask 077 && printf '%s\n' "$OPERATOR_DISPATCH_SECRET" >"$work_dir/operator-dispatch-token")
DISPATCH_TOKEN_FILE="$work_dir/operator-dispatch-token" TEST_RUNBOOK="$work_dir/runbook-token.sh" \
  run_runbook "$work_dir/token" >"$work_dir/token.out" 2>&1 \
  || { cat "$work_dir/token.out"; fail "the runbook failed to mint the Dispatch token into \$prepared and provision with it"; }
check_case "$work_dir/token" dispatch-token
grep -Fqx '{"id":"00000000-0000-4000-8000-000000000001","name":"ghost-wispr-office-pi","scopes":[]}' "$work_dir/token.out" \
  || fail "the runbook did not print the new token's id, name and scopes: $(<"$work_dir/token.out")"
if grep -F -e "$DISPATCH_SECRET" -e "$OPERATOR_DISPATCH_SECRET" "$work_dir/token.out" >/dev/null; then
  fail "the runbook's output holds a Dispatch token"
fi
left=$(grep -rlF -e "$DISPATCH_SECRET" "$work_dir/token/home" "$work_dir/token/tmp" "$work_dir/token/run" || true)
[[ $left == "$work_dir/token/tmp/"*/dispatch-token && $left != *$'\n'* ]] \
  || fail "the Dispatch token is on disk outside \$prepared/dispatch-token: $left"
[[ $(find "$left" -printf '%m') == 600 ]] || fail "\$prepared/dispatch-token is mode $(find "$left" -printf '%m'), not 0600"
# A dispatch-token that is a link or empty is refused before anything reaches the Pi.
for kind in link empty; do
  if TEST_RUNBOOK="$work_dir/runbook-token-$kind.sh" run_runbook "$work_dir/token-$kind" >"$work_dir/token-$kind.out" 2>&1; then
    fail "the runbook succeeded with a dispatch-token that is $kind"
  fi
  grep -Fq 'dispatch-token' "$work_dir/token-$kind.out" \
    || fail "a dispatch-token that is $kind was refused for another reason: $(<"$work_dir/token-$kind.out")"
  [[ ! -e $work_dir/token-$kind/pi/bundle.list && ! -e $work_dir/token-$kind/pi/sources.abc123 ]] \
    || fail "with a dispatch-token that is $kind, the runbook reached the Pi"
done

if TEST_PROVISION_EXIT=1 run_runbook "$work_dir/failed" 2>/dev/null; then
  fail "the runbook succeeded although provision.sh failed"
fi
[[ -s "$work_dir/failed/pi/stage.during" ]] || fail "the failure case never reached provision.sh"
[[ -z $(find "$work_dir/failed/run" -mindepth 1 -print -quit) ]] || fail "a failed provisioning left the bundle directory in XDG_RUNTIME_DIR"
grep -q 'rm -rf --' "$work_dir/failed/pi/removed" || fail "a failed provisioning left the sources directory on the Pi"

# The SSH session ends (exit 255) as Tailscale re-authenticates, only after provision.sh says the
# join started; an exit 255 before that is a failure.
if TEST_PROVISION_EXIT=255 run_runbook "$work_dir/dropped-early" 2>/dev/null; then
  fail "the runbook succeeded although the SSH session ended before the tailnet join started"
fi

# provision-pi.sh's last line says what provision.sh did: it started the join as the unit it named,
# or found the Pi already joined and started none.
readonly join_said='ghost-wispr provision: starting the tagged tailnet join as ghost-wispr-tailnet-join-1'
readonly joined_said='ghost-wispr provision: already joined as tag:ghost-wispr at ghost-wispr'
for outcome in started:0 started:255 joined:0 joined:255; do
  said=$join_said
  [[ ${outcome%:*} == joined ]] && said=$joined_said
  case_dir="$work_dir/outcome-${outcome/:/-}"
  TEST_PROVISION_EXIT=${outcome#*:} TEST_PROVISION_SAYS=$said run_runbook "$case_dir" >/dev/null 2>"$case_dir.err" \
    || { cat "$case_dir.err"; fail "$outcome: the runbook failed although provision.sh said: $said"; }
  last=$(grep '^provision-pi: ' "$case_dir.err" | tail -n 1)
  if [[ ${outcome%:*} == started ]]; then
    [[ $last == *'started the tagged tailnet join as the detached unit ghost-wispr-tailnet-join-1'* ]] \
      || fail "$outcome: provision-pi.sh's last line does not name the join provision.sh started: $last"
  else
    [[ $last == *'already joined'* && $last == *'started no tailnet join'* && $last != *'started the tagged tailnet join'* ]] \
      || fail "$outcome: provision-pi.sh's last line does not say the Pi was already joined and no join started: $last"
  fi
done

# A creation prints the client id as `+ … : "ID"`; the default case's replacement prints `~ … : "OLD" => "ID"`.
TEST_CREATION_FORM=create run_runbook "$work_dir/create-form" >/dev/null 2>&1 \
  || fail "the runbook failed when the newest seed run created the identity"
[[ $(<"$work_dir/create-form/pi/bundle/tailscale-client-id") == "$CLIENT_ID" ]] \
  || fail "tailscale-client-id is not the id the creation printed: $(<"$work_dir/create-form/pi/bundle/tailscale-client-id")"

# The search never reads past a root job it could not read, and never past 90 days: each case
# stops before provisioning, with its reason, although an older run's log holds a client id.
for case_name in TEST_JOB_RUNNING TEST_LOG_READ_FAIL TEST_CREATION_EXPIRED; do
  case $case_name in
    TEST_JOB_RUNNING) reason='/job/90411 is in_progress: run this again once it has finished' ;;
    TEST_LOG_READ_FAIL) reason='could not read the log of the root job https://github.com/example-org/example-infra/actions/runs/904/job/90411' ;;
    TEST_CREATION_EXPIRED) reason='no seed run of the last 90 days made example-x509' ;;
  esac
  if (export "$case_name=1" && run_runbook "$work_dir/$case_name") >"$work_dir/$case_name.out" 2>&1; then
    fail "the runbook succeeded with $case_name: $(grep -h . "$work_dir/$case_name/pi/bundle/tailscale-client-id" 2>/dev/null)"
  fi
  grep -Fq "$reason" "$work_dir/$case_name.out" \
    || fail "with $case_name the runbook did not stop with '$reason': $(<"$work_dir/$case_name.out")"
  [[ ! -e $work_dir/$case_name/pi/bundle.list ]] || fail "with $case_name the runbook provisioned the Pi"
done

# Slice 1 acceptance: the block runs the driver with no Envoy credential, which the hub needs none
# of.
cat >"$work_dir/accept-driver" <<'EOF'
#!/usr/bin/env bash
[[ -z ${GHOST_WISPR_ENVOY_WEBHOOK_SECRET:-} && -z ${GHOST_WISPR_RELAY_URL:-} ]] || exit 4
touch "$TEST_ACCEPT_RAN"
EOF
chmod +x "$work_dir/accept-driver"
accept_dir="$work_dir/accept"
mkdir -p "$accept_dir/home"
(
  cd "$repo_root"
  HOME="$accept_dir/home" TEST_ACCEPT_DRIVER="$work_dir/accept-driver" TEST_ACCEPT_RAN="$accept_dir/ran" \
    PATH="$work_dir/bin:$PATH" bash -euo pipefail "$accept"
) || fail "the slice 1 block failed"
[[ -e $accept_dir/ran ]] || fail "the slice 1 block did not run the driver"

# Certificate renewal, pasted the same way: the Pi gets the certificate Secret Manager holds,
# and nothing at all when the read fails.
renew="$work_dir/renew.sh"
block renew >"$renew"
run_renew() {
  local case_dir=$1
  mkdir -p "$case_dir/pi" "$case_dir/tmp"
  (
    cd "$repo_root"
    TMPDIR="$case_dir/tmp" TEST_PI="$case_dir/pi" PATH="$work_dir/bin:$PATH" bash "$renew"
  ) >"$case_dir/out" 2>/dev/null || true
}
run_renew "$work_dir/renew"
cmp -s "$work_dir/renew/pi/renewed.crt" "$TEST_CERTIFICATE" || fail "renewal did not install Secret Manager's certificate on the Pi"
grep -qx 'subject=CN=ghost-wispr' "$work_dir/renew/out" \
  || fail "the renewal's certificate check did not print subject=CN=ghost-wispr: $(grep '^subject=' "$work_dir/renew/out")"
TEST_CERT_READ_FAIL=1 run_renew "$work_dir/renew-failed"
[[ ! -e $work_dir/renew-failed/pi/renewed.crt ]] || fail "renewal installed a certificate on the Pi although the read failed"
[[ -z $(find "$work_dir/renew/tmp" "$work_dir/renew-failed/tmp" -mindepth 1 -print -quit) ]] \
  || fail "renewal left its certificate file behind"

if grep -F -e "$DEEPGRAM_SECRET" -e "$DISPATCH_SECRET" -e "$OPERATOR_DISPATCH_SECRET" "$TEST_ARGV_LOG" >/dev/null; then
  fail "a secret was passed as a command argument"
fi
if grep -E 'ghostWisprCertificates|tailscaleIds|service-accounts describe' "$TEST_ARGV_LOG" >/dev/null; then
  fail "the runbook still reads a source the infrastructure repository's main does not have, or the devbox identity cannot read"
fi
if grep -E '^pulumi ' "$TEST_ARGV_LOG" >/dev/null; then
  fail "the runbook still reads Pulumi state, which the devbox identity cannot read: $(grep -E '^pulumi ' "$TEST_ARGV_LOG")"
fi
printf 'provision bundle test passed\n'
