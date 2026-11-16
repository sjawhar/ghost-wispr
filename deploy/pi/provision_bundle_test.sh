#!/usr/bin/env bash
# Runs docs/pi-setup.md's own commands for the credential files, the bundle sources and the
# provisioning call, read from the runbook between its provision_bundle_test.sh markers, against
# fakes that answer only the sources the devbox identity can read: Secret Manager
# ghost-wispr-device-certificate, root/Pulumi.prod.yaml's ghostWisprAccountUniqueId on agent-infra
# main, the ghostWisprTailnetClientId the seed workflow's root job printed when it created
# ghost-wispr-x509, and the URL of the Cloud Run service envoy-webhook-relay-*. Pulumi answers
# 403, as root's and envoy-cluster's state buckets do for that identity. Provisioning reads no
# Envoy signing key: the recorder reads it from Secret Manager itself. The slice 1 acceptance
# block reads it, for its signed probe, only under a one-hour GCLOUD_SAMI broker grant (or a broker
# key), never to a plain gcloud read.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
# PROVISION_BUNDLE_TEST_DOC names another copy of the runbook, to show the test fails on it.
readonly doc="${PROVISION_BUNDLE_TEST_DOC:-$repo_root/docs/pi-setup.md}"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'provision bundle test FAILED: %s\n' "$*" >&2
  exit 1
}

# block prints the bash block that follows the runbook's marker for name.
block() {
  awk -v marker="<!-- provision_bundle_test.sh: $1 -->" '
    $0 == marker { found = 1; next }
    found && !capture && /^```bash$/ { capture = 1; next }
    capture && /^```$/ { exit }
    capture { print }
  ' "$doc"
}

for name in credentials prepare provision accept renew; do
  [[ -n $(block "$name") ]] || fail "docs/pi-setup.md has no bash block after <!-- provision_bundle_test.sh: $name -->"
done
runbook="$work_dir/runbook.sh"
{
  block credentials
  block prepare
  block provision
} >"$runbook"
# The slice 1 block, with the acceptance driver replaced by one that reports what it was given.
accept="$work_dir/accept.sh"
# shellcheck disable=SC2016 # The replacement is the runbook's text, expanded when the block runs.
block accept | sed 's#^deploy/pi/accept-slice1\.sh$#"$TEST_ACCEPT_DRIVER"#' >"$accept"
grep -q TEST_ACCEPT_DRIVER "$accept" || fail "the slice 1 block does not end by running deploy/pi/accept-slice1.sh"

readonly WEBHOOK_SECRET=webhook-signing-secret-value-0123456789abcdef
readonly DEEPGRAM_SECRET=deepgram-secret-value-0123456789abcdef
readonly CLIENT_ID=k-test-client-id
readonly SUBJECT=123456789012345678901
deepgram_key_name="DEEPGRAM""_API_KEY"
export WEBHOOK_SECRET DEEPGRAM_SECRET CLIENT_ID SUBJECT deepgram_key_name

mkdir -p "$work_dir/bin"
# A real certificate, so the runbook's openssl check runs for real.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=ghost-wispr -days 365 \
  -keyout "$work_dir/device.key" -out "$work_dir/device.pem" 2>/dev/null
export TEST_CERTIFICATE="$work_dir/device.pem" TEST_ARGV_LOG="$work_dir/argv.log"

# The devbox identity cannot list root's or envoy-cluster's Pulumi state bucket.
cat >"$work_dir/bin/pulumi" <<'EOF'
#!/usr/bin/env bash
printf 'pulumi %s\n' "$*" >>"$TEST_ARGV_LOG"
printf 'fake pulumi: 403: the devbox identity does not have storage.objects.list access to the Pulumi state bucket\n' >&2
exit 1
EOF

# gh answers as GitHub does for agent-infra's seed workflow: the runs newest first, two to a page
# here, and the jobs of each attempt.
# - 905, attempt 2: root was cancelled before it started (no steps, no log); attempt 1: skipped.
# - 904: root ran a preview, which prints the client id as [unknown].
# - 903, attempt 2 (a re-run): a preview again; attempt 1 replaced ghost-wispr-x509, so the
#   client id changed from the stale one to CLIENT_ID (with TEST_CREATION_FORM=create it created it).
# - 902: created the earlier identity, whose client id is stale.
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
readonly api=repos/theorem-labs/agent-infra/actions
recent=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ)
expired=$(date -u -d '100 days ago' +%Y-%m-%dT%H:%M:%SZ)
run_json() { printf '{"id":%s,"run_attempt":%s,"created_at":"%s"}' "$@"; }
# root_job ID STATUS CONCLUSION STEPS: one attempt's jobs, a select job and the root job.
root_job() {
  printf '{"total_count":2,"jobs":[{"id":%s0,"name":"select","status":"completed","conclusion":"success","steps":[{"started_at":"%s"}]},' "${1%1}" "$recent"
  printf '{"id":%s,"name":"root","status":"%s","conclusion":%s,"steps":[%s]}]}\n' "$1" "$2" "$3" "$4"
}
step=$(printf '{"started_at":"%s"}' "$recent")
preview='2026-11-16T06:20:21.4658172Z   + ghostWisprTailnetClientId                   : [unknown]'
created='2026-11-16T06:25:09.3203883Z  +  tailscale:index:FederatedIdentity ghost-wispr-x509 created (1s) '
created_id() { printf '2026-11-16T06:29:36.3829213Z   + ghostWisprTailnetClientId                   : "%s"\n' "$1"; }
preview_replace='2026-11-16T06:20:21.4658172Z   ~ ghostWisprTailnetClientId                   : "k-stale-client-id" => [unknown]'
replaced='2026-11-16T06:25:09.3203883Z  +-  tailscale:index:FederatedIdentity ghost-wispr-x509 replaced (1s) [diff: ~audience]'
replaced_id='2026-11-16T06:29:36.3829213Z   ~ ghostWisprTailnetClientId                   : "k-stale-client-id" => "'"$CLIENT_ID"'"'
case "${args[*]}" in
  "api $api/workflows/seed.yaml/runs?per_page=100&page="*)
    case ${args[1]##*page=} in
      1) runs="$(run_json 905 2 "$recent"),$(run_json 904 1 "$recent")" ;;
      2) runs="$(run_json 903 2 "$recent"),$(run_json 902 1 "$recent")" ;;
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
      905/2) root_job "$job" completed '"cancelled"' '' ;;
      905/1) root_job "$job" completed '"skipped"' '' ;;
      904/1)
        if [[ -n ${TEST_JOB_RUNNING:-} ]]; then root_job "$job" in_progress null "$step"; else root_job "$job" completed '"failure"' "$step"; fi
        ;;
      903/[12] | 902/1 | 901/1) root_job "$job" completed '"success"' "$step" ;;
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
  "api $api/jobs/90321/logs") printf '%s\n' "$preview_replace" ;;
  "api $api/jobs/90311/logs")
    if [[ -n ${TEST_CREATION_EXPIRED:-} ]]; then
      printf '%s\n' "$preview"
    elif [[ ${TEST_CREATION_FORM:-} == create ]]; then
      printf '%s\n' "$preview" "$created" "$(created_id "$CLIENT_ID")"
    else
      printf '%s\n' "$preview_replace" "$replaced" "$replaced_id"
    fi
    ;;
  "api $api/jobs/90211/logs")
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
  'run services list --project=theorem-agent-infra --filter=metadata.name ~ ^envoy-webhook-relay- --format=value(status.url)')
    # gcloud lists no service and exits 0 when the filter matches none.
    [[ -n ${TEST_RELAY_MISSING:-} ]] || printf 'https://relay.example.test\n'
    ;;
  'secrets versions access latest --secret=ghost-wispr-device-certificate --project=theorem-agent-infra')
    if [[ -n ${TEST_CERT_READ_FAIL:-} ]]; then
      printf 'fake gcloud: PERMISSION_DENIED reading ghost-wispr-device-certificate\n' >&2
      exit 1
    fi
    cat "$TEST_CERTIFICATE"
    ;;
  'secrets versions access latest --secret=envoy-ghostwispr-signing-secret --project=theorem-agent-infra')
    if [[ -n ${CLOUDSDK_AUTH_ACCESS_TOKEN_FILE:-} && $(<"$CLOUDSDK_AUTH_ACCESS_TOKEN_FILE") == broker-minted-token ]]; then
      printf '%s' "$WEBHOOK_SECRET"
    else
      printf 'fake gcloud: PERMISSION_DENIED: only a broker-granted login reads envoy-ghostwispr-signing-secret (dispatch://INFRA-3599)\n' >&2
      exit 1
    fi
    ;;
  *)
    printf 'fake gcloud: refused "%s"\n' "$*" >&2
    exit 1
    ;;
esac
EOF

# gcloud-broker as the dotfiles script runs it inside secret-run: the minted GCLOUD_SAMI token
# reaches gcloud through a token file.
cat >"$work_dir/bin/gcloud-broker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gcloud-broker %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ ${GCLOUD_BROKER_INNER:-} == 1 && -n ${GCLOUD_SAMI:-} ]] || { printf 'fake gcloud-broker: no GCLOUD_SAMI grant\n' >&2; exit 1; }
[[ $1 == -- ]] && shift
dir=$(mktemp -d)
trap 'rm -rf -- "$dir"' EXIT
printf '%s' "$GCLOUD_SAMI" >"$dir/token"
unset GCLOUD_SAMI GCLOUD_BROKER_INNER
CLOUDSDK_AUTH_ACCESS_TOKEN_FILE="$dir/token" "$@"
EOF

# secret-run as the secrets broker serves it: only a registered one-hour grant releases a key.
cat >"$work_dir/bin/secret-run" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'secret-run %s\n' "$*" >>"$TEST_ARGV_LOG"
register=false ttl=
while [[ $1 == --* ]]; do
  case $1 in
    --register) register=true; shift ;;
    --ttl) ttl=$2; shift 2 ;;
    --reason | --wait) shift 2 ;;
    *) printf 'fake secret-run: unknown option %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ $register == true && $ttl == 1h ]] || { printf 'fake secret-run: wants --register --ttl 1h\n' >&2; exit 1; }
[[ -z ${TEST_SECRET_READ_FAIL:-} ]] || { printf 'fake secret-run: the grant was denied\n' >&2; exit 1; }
key=$1
[[ $2 == -- ]] || exit 2
shift 2
printf '%s\n' "$key" >>"$TEST_GRANTS"
case $key in
  GCLOUD_SAMI) exec env GCLOUD_SAMI=broker-minted-token "$@" ;;
  ENVOY_GHOSTWISPR_SIGNING_SECRET) exec env "$key=$WEBHOOK_SECRET" "$@" ;;
  *) printf 'fake secret-run: no broker key %s\n' "$key" >&2; exit 1 ;;
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

# ssh reaches the fake Pi, or runs a broker host's command here.
cat >"$work_dir/bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'ssh %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $1 == -n ]] && shift
host=$1 command=$2
if [[ $host == broker.example ]]; then
  exec bash -c "$command"
fi
[[ $host == ghost-wispr@ghost-wispr.tail12ec1c.ts.net ]] || { printf 'fake ssh: unknown host %s\n' "$host" >&2; exit 255; }
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

# agent-infra as the runbook reads it: its origin's main holds root/Pulumi.prod.yaml with the
# service account's unique id. The commit before it held another id, and each case's checkout is
# left at that commit, so only a read of the fetched origin/main gives the right one.
agent_infra_origin="$work_dir/agent-infra.git"
agent_infra_seed="$work_dir/agent-infra-seed"
git init --quiet --bare --initial-branch=main "$agent_infra_origin"
git init --quiet --initial-branch=main "$agent_infra_seed"
mkdir "$agent_infra_seed/root"
for subject in 987654321098765432109 "$SUBJECT"; do
  printf 'config:\n  agent-infra-root:answerLoopProofTailnetIdentity: "true"\n  agent-infra-root:ghostWisprAccountUniqueId: "%s"\n' \
    "$subject" >"$agent_infra_seed/root/Pulumi.prod.yaml"
  git -C "$agent_infra_seed" add root/Pulumi.prod.yaml
  git -C "$agent_infra_seed" -c user.name=test -c user.email=test@example.invalid commit --quiet -m "root: subject $subject"
done
git -C "$agent_infra_seed" push --quiet "$agent_infra_origin" main

# run_runbook runs the runbook's blocks in a fresh home.
run_runbook() {
  local case_dir=$1
  mkdir -p "$case_dir/home/src" "$case_dir/tmp" "$case_dir/pi"
  git clone --quiet "$agent_infra_origin" "$case_dir/home/src/agent-infra"
  git -C "$case_dir/home/src/agent-infra" reset --quiet --hard HEAD~1
  git -C "$case_dir/home/src/agent-infra" update-ref refs/remotes/origin/main HEAD
  mkdir -m 700 "$case_dir/run"
  : >"$case_dir/grants"
  (
    cd "$repo_root"
    HOME="$case_dir/home" TMPDIR="$case_dir/tmp" XDG_RUNTIME_DIR="$case_dir/run" TEST_PI="$case_dir/pi" \
      TEST_GRANTS="$case_dir/grants" PATH="$work_dir/bin:$PATH" bash -euo pipefail "$runbook"
  )
}

# check_case checks one finished run: the bundle the Pi received, where the secrets were kept,
# and that no secret is left on disk or was ever an argument.
check_case() {
  local case_dir=$1
  local received="$case_dir/pi/bundle"
  printf '%s\n' certificate_config.json deepgram-key device.crt envoy-webhook-url \
    google-credentials.json tailscale-client-id tailscale-subject >"$case_dir/expected.list"
  cmp -s "$case_dir/expected.list" "$case_dir/pi/bundle.list" \
    || fail "$case_dir: the Pi received $(tr '\n' ' ' <"$case_dir/pi/bundle.list")"
  cmp -s "$received/device.crt" "$TEST_CERTIFICATE" || fail "device.crt is not Secret Manager's ghost-wispr-device-certificate"
  [[ $(<"$received/tailscale-client-id") == "$CLIENT_ID" ]] \
    || fail "tailscale-client-id is not the ghostWisprTailnetClientId of the newest ghost-wispr-x509 creation: $(<"$received/tailscale-client-id")"
  [[ $(<"$received/tailscale-subject") == "$SUBJECT" ]] \
    || fail "tailscale-subject is not ghostWisprAccountUniqueId on agent-infra's origin/main: $(<"$received/tailscale-subject")"
  [[ $(<"$received/envoy-webhook-url") == https://relay.example.test/webhook/ghostwispr ]] \
    || fail "envoy-webhook-url is not the relay service's URL: $(<"$received/envoy-webhook-url")"
  [[ $(<"$received/deepgram-key") == "$DEEPGRAM_SECRET" ]] || fail "deepgram-key is wrong"
  jq -e '.credential_source.certificate.certificate_config_location == "/opt/ghost-wispr/certificate_config.json"' \
    "$received/google-credentials.json" >/dev/null || fail "google-credentials.json does not name the Pi's certificate config"
  [[ ! -s "$case_dir/grants" ]] || fail "provisioning asked for broker grants: $(tr '\n' ' ' <"$case_dir/grants")"
  if grep -rqF "$WEBHOOK_SECRET" "$received"; then
    fail "the Envoy signing key reached the Pi in the bundle"
  fi
  [[ $(wc -l <"$case_dir/pi/stage.during") == 1 && $(cut -d' ' -f1 "$case_dir/pi/stage.during") == 700 ]] \
    || fail "while provision.sh ran, the secrets were not in one mode-0700 directory under XDG_RUNTIME_DIR: $(<"$case_dir/pi/stage.during")"
  [[ -z $(find "$case_dir/run" -mindepth 1 -print -quit) ]] || fail "the bundle directory is left in XDG_RUNTIME_DIR"
  grep -q "rm -rf -- '$case_dir/pi/sources.abc123'" "$case_dir/pi/removed" || fail "the sources directory on the Pi was not removed"
  if grep -rlF -e "$WEBHOOK_SECRET" -e "$DEEPGRAM_SECRET" "$case_dir/home" "$case_dir/tmp" "$case_dir/run" >/dev/null 2>&1; then
    fail "a secret is left on disk: $(grep -rlF -e "$WEBHOOK_SECRET" -e "$DEEPGRAM_SECRET" "$case_dir/home" "$case_dir/tmp" "$case_dir/run")"
  fi
}

export TEST_ARGV_LOG
run_runbook "$work_dir/local" || fail "the runbook's commands failed against the sources the devbox identity reads"
check_case "$work_dir/local"

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
TEST_PROVISION_EXIT=255 TEST_PROVISION_SAYS='ghost-wispr provision: starting the tagged tailnet join as ghost-wispr-tailnet-join-1' \
  run_runbook "$work_dir/dropped-after-join" 2>/dev/null \
  || fail "the runbook failed although the SSH session ended after the tailnet join started"

# A creation prints the client id as `+ … : "ID"`; the default case's replacement prints `~ … : "OLD" => "ID"`.
TEST_CREATION_FORM=create run_runbook "$work_dir/create-form" >/dev/null 2>&1 \
  || fail "the runbook failed when the newest seed run created ghost-wispr-x509"
[[ $(<"$work_dir/create-form/pi/bundle/tailscale-client-id") == "$CLIENT_ID" ]] \
  || fail "tailscale-client-id is not the id the creation printed: $(<"$work_dir/create-form/pi/bundle/tailscale-client-id")"

# The search never reads past a root job it could not read, and never past 90 days: each case
# stops before provisioning, with its reason, although an older run's log holds a client id.
for case_name in TEST_JOB_RUNNING TEST_LOG_READ_FAIL TEST_CREATION_EXPIRED; do
  case $case_name in
    TEST_JOB_RUNNING) reason='/job/90411 is in_progress: run this again once it has finished' ;;
    TEST_LOG_READ_FAIL) reason='could not read the log of the root job https://github.com/theorem-labs/agent-infra/actions/runs/904/job/90411' ;;
    TEST_CREATION_EXPIRED) reason='no seed run of the last 90 days made ghost-wispr-x509' ;;
  esac
  if (export "$case_name=1" && run_runbook "$work_dir/$case_name") >"$work_dir/$case_name.out" 2>&1; then
    fail "the runbook succeeded with $case_name: $(grep -h . "$work_dir/$case_name/pi/bundle/tailscale-client-id" 2>/dev/null)"
  fi
  grep -Fq "$reason" "$work_dir/$case_name.out" \
    || fail "with $case_name the runbook did not stop with '$reason': $(<"$work_dir/$case_name.out")"
  [[ ! -e $work_dir/$case_name/pi/bundle.list ]] || fail "with $case_name the runbook provisioned the Pi"
done

# Slice 1 acceptance: the secret reaches the driver's environment alone, read under a grant.
cat >"$work_dir/accept-driver" <<'EOF'
#!/usr/bin/env bash
[[ -z ${TEST_ACCEPT_INVOKED:-} ]] || touch "$TEST_ACCEPT_INVOKED"
[[ $GHOST_WISPR_RELAY_URL == https://relay.example.test/webhook/ghostwispr ]] || exit 3
[[ $GHOST_WISPR_ENVOY_WEBHOOK_SECRET == "$WEBHOOK_SECRET" ]] || exit 4
touch "$TEST_ACCEPT_RAN"
EOF
chmod +x "$work_dir/accept-driver"
for broker_host in "" broker.example; do
  accept_dir="$work_dir/accept${broker_host:+-host}"
  mkdir -p "$accept_dir/home"
  : >"$accept_dir/grants"
  (
    cd "$repo_root"
    HOME="$accept_dir/home" TEST_GRANTS="$accept_dir/grants" TEST_ACCEPT_DRIVER="$work_dir/accept-driver" \
      TEST_ACCEPT_RAN="$accept_dir/ran" PATH="$work_dir/bin:$PATH" broker_host="$broker_host" broker_key="" \
      bash -euo pipefail "$accept"
  ) || fail "the slice 1 block failed${broker_host:+ with --broker-host}"
  [[ -e $accept_dir/ran && $(<"$accept_dir/grants") == GCLOUD_SAMI ]] \
    || fail "the slice 1 driver did not run with the secret read under a GCLOUD_SAMI grant"
done

# A person pastes the block into a shell without set -e: a failed secret read must run no
# acceptance, which would otherwise sign nothing and report the signed probe as skipped.
accept_dir="$work_dir/accept-failed-read"
mkdir -p "$accept_dir/home"
: >"$accept_dir/grants"
(
  cd "$repo_root"
  HOME="$accept_dir/home" TEST_GRANTS="$accept_dir/grants" TEST_ACCEPT_DRIVER="$work_dir/accept-driver" \
    TEST_ACCEPT_RAN="$accept_dir/ran" TEST_ACCEPT_INVOKED="$accept_dir/invoked" TEST_SECRET_READ_FAIL=1 \
    PATH="$work_dir/bin:$PATH" broker_host="" broker_key="" bash "$accept"
) >/dev/null 2>&1 || true
[[ ! -e $accept_dir/invoked ]] || fail "slice 1 acceptance ran although the secret read failed"

# The same with no relay service found: gcloud prints nothing and exits 0.
accept_dir="$work_dir/accept-no-relay"
mkdir -p "$accept_dir/home"
: >"$accept_dir/grants"
(
  cd "$repo_root"
  HOME="$accept_dir/home" TEST_GRANTS="$accept_dir/grants" TEST_ACCEPT_DRIVER="$work_dir/accept-driver" \
    TEST_ACCEPT_RAN="$accept_dir/ran" TEST_ACCEPT_INVOKED="$accept_dir/invoked" TEST_RELAY_MISSING=1 \
    PATH="$work_dir/bin:$PATH" broker_host="" broker_key="" bash "$accept"
) >/dev/null 2>&1 || true
[[ ! -e $accept_dir/invoked ]] || fail "slice 1 acceptance ran although no relay service was found"

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
  ) >/dev/null 2>&1 || true
}
run_renew "$work_dir/renew"
cmp -s "$work_dir/renew/pi/renewed.crt" "$TEST_CERTIFICATE" || fail "renewal did not install Secret Manager's certificate on the Pi"
TEST_CERT_READ_FAIL=1 run_renew "$work_dir/renew-failed"
[[ ! -e $work_dir/renew-failed/pi/renewed.crt ]] || fail "renewal installed a certificate on the Pi although the read failed"
[[ -z $(find "$work_dir/renew/tmp" "$work_dir/renew-failed/tmp" -mindepth 1 -print -quit) ]] \
  || fail "renewal left its certificate file behind"

if grep -F -e "$WEBHOOK_SECRET" -e "$DEEPGRAM_SECRET" "$TEST_ARGV_LOG" >/dev/null; then
  fail "a secret was passed as a command argument"
fi
if grep -E 'ghostWisprCertificates|tailscaleIds|service-accounts describe' "$TEST_ARGV_LOG" >/dev/null; then
  fail "the runbook still reads a source agent-infra main does not have, or the devbox identity cannot read"
fi
if grep -E '^pulumi ' "$TEST_ARGV_LOG" >/dev/null; then
  fail "the runbook still reads Pulumi state, which the devbox identity cannot read: $(grep -E '^pulumi ' "$TEST_ARGV_LOG")"
fi
printf 'provision bundle test passed\n'
