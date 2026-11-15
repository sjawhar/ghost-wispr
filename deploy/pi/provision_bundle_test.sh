#!/usr/bin/env bash
# Runs docs/pi-setup.md's own commands for the credential files, the bundle sources and the
# provisioning call, read from the runbook between its provision_bundle_test.sh markers, against
# fakes that answer only the sources agent-infra main has: Secret Manager
# ghost-wispr-device-certificate, root's stack output ghostWisprTailnetClientId, root's prod config
# ghostWisprAccountUniqueId and envoy-cluster's relayUrl. The webhook signing secret answers only
# under a one-hour GCLOUD_SAMI broker grant (or a broker key), never to a plain gcloud read.
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

for name in credentials prepare provision accept; do
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

cat >"$work_dir/bin/pulumi" <<'EOF'
#!/usr/bin/env bash
printf 'pulumi %s\n' "$*" >>"$TEST_ARGV_LOG"
case "$PWD:$*" in
  */agent-infra/root:'stack output ghostWisprTailnetClientId --stack prod') printf '%s\n' "$CLIENT_ID" ;;
  */agent-infra/root:'config get ghostWisprAccountUniqueId --stack prod') printf '%s\n' "$SUBJECT" ;;
  */agent-infra/envoy-cluster:'stack output relayUrl --stack prod') printf 'https://relay.example.test\n' ;;
  *)
    printf 'fake pulumi: agent-infra main has no source for "%s" in %s\n' "$*" "$PWD" >&2
    exit 1
    ;;
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
  'secrets versions access latest --secret=ghost-wispr-device-certificate --project=theorem-agent-infra')
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
    exit "${TEST_PROVISION_EXIT:-0}"
    ;;
  *'rm -rf --'*) printf '%s\n' "$command" >>"$TEST_PI/removed" ;;
  *) printf 'fake ssh: unexpected command %s\n' "$command" >&2; exit 1 ;;
esac
EOF
chmod +x "$work_dir/bin/"*

# run_runbook runs the runbook's blocks in a fresh home with broker_host and broker_key as given.
run_runbook() {
  local case_dir=$1 broker_host=$2 broker_key=$3
  mkdir -p "$case_dir/home/src/agent-infra/bootstrap" "$case_dir/home/src/agent-infra/root" \
    "$case_dir/home/src/agent-infra/envoy-cluster" "$case_dir/tmp" "$case_dir/pi"
  mkdir -m 700 "$case_dir/run"
  : >"$case_dir/grants"
  (
    cd "$repo_root"
    HOME="$case_dir/home" TMPDIR="$case_dir/tmp" XDG_RUNTIME_DIR="$case_dir/run" TEST_PI="$case_dir/pi" \
      TEST_GRANTS="$case_dir/grants" PATH="$work_dir/bin:$PATH" broker_host="$broker_host" broker_key="$broker_key" \
      bash -euo pipefail "$runbook"
  )
}

# check_case checks one finished run: the bundle the Pi received, where the secrets were kept,
# and that no secret is left on disk or was ever an argument.
check_case() {
  local case_dir=$1 want_grant=$2
  local received="$case_dir/pi/bundle"
  printf '%s\n' certificate_config.json deepgram-key device.crt envoy-webhook-secret envoy-webhook-url \
    google-credentials.json tailscale-client-id tailscale-subject >"$case_dir/expected.list"
  cmp -s "$case_dir/expected.list" "$case_dir/pi/bundle.list" \
    || fail "$case_dir: the Pi received $(tr '\n' ' ' <"$case_dir/pi/bundle.list")"
  cmp -s "$received/device.crt" "$TEST_CERTIFICATE" || fail "device.crt is not Secret Manager's ghost-wispr-device-certificate"
  [[ $(<"$received/tailscale-client-id") == "$CLIENT_ID" ]] || fail "tailscale-client-id is not root's ghostWisprTailnetClientId"
  [[ $(<"$received/tailscale-subject") == "$SUBJECT" ]] || fail "tailscale-subject is not root's ghostWisprAccountUniqueId"
  [[ $(<"$received/envoy-webhook-url") == https://relay.example.test/webhook/ghostwispr ]] || fail "envoy-webhook-url is wrong"
  [[ $(<"$received/envoy-webhook-secret") == "$WEBHOOK_SECRET" ]] || fail "envoy-webhook-secret is wrong"
  [[ $(<"$received/deepgram-key") == "$DEEPGRAM_SECRET" ]] || fail "deepgram-key is wrong"
  jq -e '.credential_source.certificate.certificate_config_location == "/opt/ghost-wispr/certificate_config.json"' \
    "$received/google-credentials.json" >/dev/null || fail "google-credentials.json does not name the Pi's certificate config"
  [[ $(<"$case_dir/grants") == "$want_grant" ]] || fail "broker grants were $(tr '\n' ' ' <"$case_dir/grants"), want $want_grant"
  [[ $(wc -l <"$case_dir/pi/stage.during") == 1 && $(cut -d' ' -f1 "$case_dir/pi/stage.during") == 700 ]] \
    || fail "while provision.sh ran, the secrets were not in one mode-0700 directory under XDG_RUNTIME_DIR: $(<"$case_dir/pi/stage.during")"
  [[ -z $(find "$case_dir/run" -mindepth 1 -print -quit) ]] || fail "the bundle directory is left in XDG_RUNTIME_DIR"
  grep -q "rm -rf -- '$case_dir/pi/sources.abc123'" "$case_dir/pi/removed" || fail "the sources directory on the Pi was not removed"
  if grep -rlF -e "$WEBHOOK_SECRET" -e "$DEEPGRAM_SECRET" "$case_dir/home" "$case_dir/tmp" "$case_dir/run" >/dev/null 2>&1; then
    fail "a secret is left on disk: $(grep -rlF -e "$WEBHOOK_SECRET" -e "$DEEPGRAM_SECRET" "$case_dir/home" "$case_dir/tmp" "$case_dir/run")"
  fi
}

export TEST_ARGV_LOG
run_runbook "$work_dir/local" "" "" || fail "the runbook's commands failed against agent-infra main's sources"
check_case "$work_dir/local" GCLOUD_SAMI

run_runbook "$work_dir/broker-host" broker.example "" || fail "the runbook's commands failed with --broker-host"
check_case "$work_dir/broker-host" GCLOUD_SAMI
grep -q '^ssh -n broker.example secret-run --register ' "$TEST_ARGV_LOG" || fail "--broker-host did not run secret-run on the broker host"

run_runbook "$work_dir/broker-key" "" ENVOY_GHOSTWISPR_SIGNING_SECRET || fail "the runbook's commands failed with --broker-key"
check_case "$work_dir/broker-key" ENVOY_GHOSTWISPR_SIGNING_SECRET

if TEST_PROVISION_EXIT=1 run_runbook "$work_dir/failed" "" "" 2>/dev/null; then
  fail "the runbook succeeded although provision.sh failed"
fi
[[ -s "$work_dir/failed/pi/stage.during" ]] || fail "the failure case never reached provision.sh"
[[ -z $(find "$work_dir/failed/run" -mindepth 1 -print -quit) ]] || fail "a failed provisioning left the bundle directory in XDG_RUNTIME_DIR"
grep -q 'rm -rf --' "$work_dir/failed/pi/removed" || fail "a failed provisioning left the sources directory on the Pi"

# Slice 1 acceptance: the secret reaches the driver's environment alone, read under a grant.
cat >"$work_dir/accept-driver" <<'EOF'
#!/usr/bin/env bash
[[ $GHOST_WISPR_RELAY_URL == https://relay.example.test/webhook/ghostwispr ]] || exit 3
[[ $GHOST_WISPR_ENVOY_WEBHOOK_SECRET == "$WEBHOOK_SECRET" ]] || exit 4
touch "$TEST_ACCEPT_RAN"
EOF
chmod +x "$work_dir/accept-driver"
for broker_host in "" broker.example; do
  accept_dir="$work_dir/accept${broker_host:+-host}"
  mkdir -p "$accept_dir/home/src/agent-infra/envoy-cluster"
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

if grep -F -e "$WEBHOOK_SECRET" -e "$DEEPGRAM_SECRET" "$TEST_ARGV_LOG" >/dev/null; then
  fail "a secret was passed as a command argument"
fi
if grep -E 'ghostWisprCertificates|tailscaleIds|service-accounts describe' "$TEST_ARGV_LOG" >/dev/null; then
  fail "the runbook still reads a source agent-infra main does not have"
fi
printf 'provision bundle test passed\n'
