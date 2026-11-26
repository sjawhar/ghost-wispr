#!/usr/bin/env bash
# Runs three of docs/pi-setup.md's own bash blocks, read from the runbook after their
# runbook_blocks_test.sh markers, against fakes:
#
#   accept-routes   on the Pi, in the release directory: the route check of the release's
#                   provision.sh, and `tailscale set --accept-routes` only after the check passed
#   dispatch-token  on a person's own device, in a checkout: mint the Pi's Dispatch token and add it
#                   as a version of its Secret Manager secret
#   accept          on a person's own device, in a checkout: the slice 1 acceptance driver
#
# A block that runs in the release directory may name only files that the release tarball holds,
# and a block that runs in a checkout only files that the checkout tracks; each PI_ variable a block
# reads must be set by the pi.env of where it runs and by deploy/pi/test-pi.env. The release
# directory is what deploy/pi/build-deploy-tarball.sh --stage writes: the directory the tarball
# holds. The blocks read the neutral deployment settings in deploy/pi/test-pi.env. The fakes stand
# in for the Pi's ip, tailscale and sudo, and for Dispatch, Secret Manager and the driver, so a pass
# does not show that a procedure works on the Pi.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly test_env="$repo_root/deploy/pi/test-pi.env"
# RUNBOOK_BLOCKS_TEST_DOC names another copy of the runbook, to show the test fails on it.
readonly doc="${RUNBOOK_BLOCKS_TEST_DOC:-$repo_root/docs/pi-setup.md}"
work_dir=$(mktemp -d)
readonly work_dir
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'runbook blocks test FAILED: %s\n' "$*" >&2
  exit 1
}

# block prints the bash block that follows the runbook's marker for name. The marker and the block
# may be indented, as in a list item.
block() {
  awk -v marker="<!-- runbook_blocks_test.sh: $1 -->" '
    { line = $0; sub(/^ +/, "", line) }
    line == marker { found = 1; next }
    found && !capture && line == "```bash" { capture = 1; next }
    capture && line == "```" { exit }
    capture { print }
  ' "$doc"
}

readonly blocks=(accept-routes dispatch-token accept)
# Where a person runs each block: in the release directory, or in a checkout.
declare -A runs_in=([accept-routes]=release [dispatch-token]=checkout [accept]=checkout)
for name in "${blocks[@]}"; do
  [[ -n $(block "$name") ]] || fail "docs/pi-setup.md has no bash block after <!-- runbook_blocks_test.sh: $name -->"
done

# The release directory. The release binary is a fake that answers link-versions as a binary
# without that subcommand does.
cat >"$work_dir/ghost-wispr-linux-arm64" <<'EOF'
#!/usr/bin/env bash
printf 'unknown subcommand "%s"\n' "${1:-}" >&2
exit 2
EOF
chmod +x "$work_dir/ghost-wispr-linux-arm64"
readonly release_dir="$work_dir/release"
bash "$repo_root/deploy/pi/build-deploy-tarball.sh" --stage 0123456789abcdef0123456789abcdef01234567 \
  "$work_dir/ghost-wispr-linux-arm64" "$release_dir" >"$work_dir/build.out" 2>&1 \
  || fail "deploy/pi/build-deploy-tarball.sh --stage failed: $(<"$work_dir/build.out")"
release_files=$(find "$release_dir" -mindepth 1 -maxdepth 1 -type f -printf '%f\n') \
  || fail "cannot list the release directory $release_dir"
[[ -n $release_files ]] || fail "UNKNOWN: the release directory $release_dir holds no file, so the test cannot tell what the tarball holds"
grep -Fqx manifest.json <<<"$release_files" \
  || fail "the release directory holds no manifest.json, so it is not the tarball's directory: $(paste -sd ' ' <<<"$release_files")"
tracked=$(git -C "$repo_root" ls-files) || fail "cannot list the files the checkout tracks"
[[ -n $tracked ]] || fail "UNKNOWN: git ls-files listed no file in $repo_root, so the test cannot tell what the checkout holds"

# named_files prints each file the text on stdin names: each word that ends in a file suffix, less a
# leading ./, and not an absolute path, a path under ~ or a path under a variable. The path in the
# default of ${VARIABLE:-path} counts.
named_files() {
  sed -E 's/\$\{[A-Za-z_][A-Za-z0-9_]*:-/ /g
    s/\$\{?[A-Za-z_][A-Za-z0-9_]*\}?\/[^[:space:]"'\'')]*//g
    s/\$\{?[A-Za-z_][A-Za-z0-9_]*\}?//g' \
    | { grep -oE '[~A-Za-z0-9._/-]+' || true; } \
    | { grep -E '^[A-Za-z0-9_.][A-Za-z0-9._/-]*\.(sh|env|conf|yaml|yml|json|service|timer|rules|gz|sha256|crt|pem|awk|txt)$' || true; } \
    | sed 's#^\./##' | LC_ALL=C sort -u
}

# check_named NAME WHERE TEXT fails when TEXT, the block NAME, names a file that WHERE does not
# hold: the release directory (a file directly in it), or the checkout (a file it tracks). It fails
# too when it finds no file the block names, since it then checked nothing.
check_named() {
  local name=$1 where=$2 text=$3 files file missing=()
  files=$(named_files <<<"$text")
  if [[ -z $files ]]; then
    printf 'UNKNOWN: found no file that the %s block names, so nothing was checked\n' "$name" >&2
    return 1
  fi
  while IFS= read -r file; do
    case $where in
      release) [[ $file != */* ]] && grep -Fqx -- "$file" <<<"$release_files" || missing+=("$file") ;;
      checkout) grep -Fqx -- "$file" <<<"$tracked" || missing+=("$file") ;;
      *) missing+=("$file") ;;
    esac
  done <<<"$files"
  ((${#missing[@]} == 0)) && return 0
  case $where in
    release) where='release tarball' ;;
  esac
  printf 'the %s block names %s, which the %s does not hold\n' "$name" "${missing[*]}" "$where" >&2
  return 1
}

# The check's own controls: it refuses a release block that names a checkout path or a file the
# tarball does not hold, a checkout block that names a file the checkout does not track, and a block
# in which it finds no file at all; and it accepts a release block that names a file of the tarball.
# shellcheck disable=SC2016 # Each text is block text, never expanded here.
for control in 'release bash deploy/pi/provision.sh --models' 'release ./provision-pi.sh --prepared "$prepared"' \
  'checkout source deploy/pi/no-such-settings.env' 'release echo "$PI_HOST"'; do
  read -r where text <<<"$control"
  if check_named control "$where" "$text" 2>/dev/null; then
    fail "the file check passed a $where block that it must refuse: $text"
  fi
done
check_named control release 'source "${GHOST_WISPR_PI_ENV:-pi.env}" && ./provision.sh --key' \
  || fail 'the file check refused a release block that names only pi.env and provision.sh'

# pi_variables prints each PI_ variable the text on stdin reads.
pi_variables() {
  { grep -oE '\$\{?PI_[A-Z0-9_]+' || true; } | sed -E 's/^\$\{?//' | LC_ALL=C sort -u
}
# unset_in FILE NAME... prints each NAME that FILE, sourced, leaves unset or empty.
unset_in() {
  bash -c 'file=$1; shift; source "$file" >/dev/null 2>&1 || { printf "cannot source %s\n" "$file"; exit 0; }
    for name; do [[ -n ${!name:-} ]] || printf "%s\n" "$name"; done' _ "$@"
}

for name in "${blocks[@]}"; do
  text=$(block "$name")
  check_named "$name" "${runs_in[$name]}" "$text" 2>"$work_dir/why" || fail "$(<"$work_dir/why")"
  mapfile -t variables < <(pi_variables <<<"$text")
  ((${#variables[@]} > 0)) || continue
  if [[ ${runs_in[$name]} == release ]]; then
    pi_env=$release_dir/pi.env
  else
    pi_env=$repo_root/deploy/pi/files/pi.env
  fi
  for env_file in "$pi_env" "$test_env"; do
    missing=$(unset_in "$env_file" "${variables[@]}")
    [[ -z $missing ]] || fail "the $name block reads $(paste -sd ' ' <<<"$missing"), which $env_file does not set"
  done
done

# --- accept-routes: on the Pi, in the release directory.

# The fake Pi's ip, tailscale and sudo, first on the block's PATH. ip prints F-ip-pi. tailscale keeps
# the accept-routes setting, RouteAll, in $TEST_PI/route-all (false when that file is absent), sets
# it only as root (under the fake sudo), and logs each call in $TEST_PI/log. Its `status --json`
# prints $TEST_PI_STATUS (F-status-router by default), or fails under TEST_PI_STATUS_EXIT.
readonly pi_bin="$work_dir/pi-bin" pi_fixtures="$work_dir/pi-fixtures"
mkdir -p "$pi_bin" "$pi_fixtures"
cat >"$pi_bin/ip" <<'EOF'
#!/usr/bin/env bash
[[ $* == '-4 -o addr' ]] || { printf 'fake Pi ip: unexpected call: ip %s\n' "$*" >&2; exit 2; }
cat -- "$TEST_PI_FIXTURES/F-ip-pi"
EOF
cat >"$pi_bin/tailscale" <<'EOF'
#!/usr/bin/env bash
printf 'tailscale %s\n' "$*" >>"$TEST_PI/log"
route_all=$(cat -- "$TEST_PI/route-all" 2>/dev/null) || route_all=false
case $* in
  'status --json')
    if [[ -n ${TEST_PI_STATUS_EXIT:-} ]]; then
      printf 'failed to connect to the local Tailscale daemon\n' >&2
      exit "$TEST_PI_STATUS_EXIT"
    fi
    cat -- "$TEST_PI_FIXTURES/${TEST_PI_STATUS:-F-status-router}"
    ;;
  'set --accept-routes')
    [[ ${TEST_PI_ROOT:-} == 1 ]] || { printf 'fake Pi tailscale: changing preferences needs root\n' >&2; exit 1; }
    [[ -z ${TEST_PI_SET_FAIL:-} ]] || { printf 'fake Pi tailscale: set failed\n' >&2; exit 1; }
    printf 'true\n' >"$TEST_PI/route-all"
    ;;
  'debug prefs') printf '{"RouteAll":%s}\n' "$route_all" ;;
  *) printf 'fake Pi tailscale: unexpected call: tailscale %s\n' "$*" >&2; exit 2 ;;
esac
EOF
cat >"$pi_bin/sudo" <<'EOF'
#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >>"$TEST_PI/log"
TEST_PI_ROOT=1 exec "$@"
EOF
# A sh that reads the check program and runs none of it, as when the program reaches sh cut short.
mkdir -p "$work_dir/cut-bin"
cat >"$work_dir/cut-bin/sh" <<'EOF'
#!/usr/bin/env bash
cat >/dev/null
EOF
chmod +x "$pi_bin/"* "$work_dir/cut-bin/sh"
# F-ip-pi: the office Pi's `ip -4 -o addr`, captured on the Pi on 2026-11-20 at 06:2xZ and posted on
# dispatch://INFRA-4587/comment/214b89f6-725d-4230-9e03-f94c8eae1187, with a neutral tailscale0
# address, in the form iproute2 prints with -o for a real kernel, which always sends the address's
# lifetimes: the posted reading was cut at the `\` that comes before them (iproute2 6.15.0,
# ip/ipaddress.c:1674-1684 and ip/ip.c:302).
cat >"$pi_fixtures/F-ip-pi" <<'EOF'
1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
3: wlan0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0\       valid_lft 85520sec preferred_lft 85520sec
4: tailscale0    inet 100.64.0.2/32 scope global tailscale0\       valid_lft forever preferred_lft forever
EOF
# F-status-router: a `tailscale status --json` in the shape of Tailscale v1.104.1's
# ipn/ipnstate/ipnstate.go, with neutral names and addresses: BackendState and Self, then Peer keyed
# by node key (line 81), each peer with its HostName and DNSName (lines 242 and 246), and
# PrimaryRoutes only when it routes (line 266, omitempty). router-a routes 10.1.0.0/16, INFRA-3709's
# Pod range, the route that INFRA-4587 adds. svc-frontdoor routes a service /32 and an IPv6
# /128, the shape of the office Pi's reading on
# dispatch://INFRA-4587/comment/214b89f6-725d-4230-9e03-f94c8eae1187. member-a routes nothing.
cat >"$pi_fixtures/F-status-router" <<'EOF'
{
  "Version": "1.104.1",
  "BackendState": "Running",
  "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"],
  "Self": {
    "ID": "n100",
    "PublicKey": "nodekey:0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f",
    "HostName": "ghost-wispr",
    "DNSName": "ghost-wispr.example-tailnet.ts.net.",
    "OS": "linux",
    "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"],
    "Tags": ["tag:ghost-wispr"],
    "Online": true
  },
  "MagicDNSSuffix": "example-tailnet.ts.net",
  "Peer": {
    "nodekey:1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a": {
      "ID": "n101",
      "PublicKey": "nodekey:1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a",
      "HostName": "router-a",
      "DNSName": "router-a.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.3", "fd7a:115c:a1e0::3"],
      "PrimaryRoutes": ["10.1.0.0/16"],
      "Online": true
    },
    "nodekey:2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b": {
      "ID": "n102",
      "PublicKey": "nodekey:2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b",
      "HostName": "svc-frontdoor",
      "DNSName": "svc-frontdoor.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.4", "fd7a:115c:a1e0::4"],
      "PrimaryRoutes": ["100.64.10.1/32", "fd7a:115c:a1e0::a/128"],
      "Online": true
    },
    "nodekey:3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c": {
      "ID": "n103",
      "PublicKey": "nodekey:3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c",
      "HostName": "member-a",
      "DNSName": "member-a.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.5", "fd7a:115c:a1e0::5"],
      "Online": true
    }
  }
}
EOF
# F-status-overlap: the same tailnet, where router-a also routes the office network of wlan0.
jq '(.Peer[] | select(.HostName == "router-a") | .PrimaryRoutes) += ["192.168.0.0/16"]' \
  "$pi_fixtures/F-status-router" >"$pi_fixtures/F-status-overlap"

block accept-routes >"$work_dir/accept-routes.sh"
readonly accepted_prefix='ghost-wispr routes: no route of a peer overlaps a network of the Pi: '
# run_route_step runs the block as a person pastes it on the Pi, with plain bash (no -e, no
# pipefail), in the directory given second (the release directory by default), against the fake Pi
# of the case directory given first. It keeps the exit status in route_step_exit, stdout and stderr
# in <case>/out, and the Pi's tailscale and sudo calls in <case>/log. TEST_PATH_FIRST puts another
# directory before the fake Pi's on PATH.
run_route_step() {
  local case_dir=$1 dir=${2:-$release_dir}
  mkdir -p "$case_dir"
  : >"$case_dir/log"
  route_step_exit=0
  (
    cd "$dir"
    TEST_PI="$case_dir" TEST_PI_FIXTURES="$pi_fixtures" PATH="${TEST_PATH_FIRST:+$TEST_PATH_FIRST:}$pi_bin:$PATH" \
      bash "$work_dir/accept-routes.sh"
  ) >"$case_dir/out" 2>&1 || route_step_exit=$?
}
# set_ran CASE: the block ran tailscale set in CASE.
set_ran() { grep -q 'tailscale set' "$1/log"; }

# No route overlaps a network of the Pi: the block accepts the routes, as root, and ends by printing
# RouteAll, true.
run_route_step "$work_dir/routes-none"
d1="the route block did not accept the routes on a Pi whose networks no route overlaps: $(<"$work_dir/routes-none/out")"
[[ $route_step_exit == 0 ]] || fail "$d1"
grep -Fxq 'sudo tailscale set --accept-routes' "$work_dir/routes-none/log" || fail "$d1"
grep -q -e "^$accepted_prefix" "$work_dir/routes-none/out" || fail "$d1"
[[ $(tail -n 1 -- "$work_dir/routes-none/out") == true ]] || fail "$d1"

# A peer's route overlaps the office network: the check refuses, and the block does not accept.
TEST_PI_STATUS=F-status-overlap run_route_step "$work_dir/routes-overlap"
if [[ $route_step_exit == 0 ]] || set_ran "$work_dir/routes-overlap"; then
  fail "the route block accepted the routes although a route overlaps the Pi's network: $(<"$work_dir/routes-overlap/out")"
fi
grep -q 'overlaps 192.168.0.0/16 of router-a' "$work_dir/routes-overlap/out" \
  || fail "the route block did not print the overlap the check found: $(<"$work_dir/routes-overlap/out")"

# The check cannot read the tailnet's status: the block does not accept.
TEST_PI_STATUS_EXIT=1 run_route_step "$work_dir/routes-unread"
if [[ $route_step_exit == 0 ]] || set_ran "$work_dir/routes-unread"; then
  fail "the route block accepted the routes although the check could not read the status: $(<"$work_dir/routes-unread/out")"
fi

# A release whose provision.sh defines no route_overlap_check_script: the block runs no check and
# calls no tailscale.
older_release="$work_dir/older-release"
cp -R -- "$release_dir" "$older_release"
sed 's/route_overlap_check_script/no_route_overlap_check_script/g' "$release_dir/provision.sh" >"$older_release/provision.sh"
run_route_step "$work_dir/routes-older" "$older_release"
if [[ $route_step_exit == 0 ]] || [[ -s $work_dir/routes-older/log ]]; then
  fail "the route block called tailscale with no check to run: $(<"$work_dir/routes-older/log")"
fi

# The check program runs none of its body and prints nothing: the block does not accept.
TEST_PATH_FIRST="$work_dir/cut-bin" run_route_step "$work_dir/routes-cut"
if [[ $route_step_exit == 0 ]] || set_ran "$work_dir/routes-cut"; then
  fail "the route block accepted the routes although the check printed no acceptance line: $(<"$work_dir/routes-cut/out")"
fi

# tailscale set fails: the block fails, and prints no RouteAll.
TEST_PI_SET_FAIL=1 run_route_step "$work_dir/routes-set-failed"
if [[ $route_step_exit == 0 ]] || grep -qx true "$work_dir/routes-set-failed/out"; then
  fail "the route block reported success although tailscale set failed: $(<"$work_dir/routes-set-failed/out")"
fi

# --- dispatch-token: on a person's own device, in a checkout.

# The neutral settings the block reads, which the fakes check its calls against.
# shellcheck source=test-pi.env
source "$test_env"
export PI_DISPATCH_URL PI_GCP_PROJECT PI_DISPATCH_TOKEN_SECRET
readonly DISPATCH_SECRET=dispatch-token-secret-value-0123456789abcdef
readonly OPERATOR_DISPATCH_SECRET=operator-dispatch-secret-value-0123456789abcdef
readonly TOKEN_NAME=ghost-wispr-office-pi-secret-manager
export DISPATCH_SECRET OPERATOR_DISPATCH_SECRET TOKEN_NAME

mkdir -p "$work_dir/bin"
# curl answers as Dispatch does to the mint of an agent token, whose answer is the one place the new
# token shows: only for the person's own token, given as a header on stdin, and only for a no-scope
# token named TOKEN_NAME. TEST_MINT_TOKEN replaces the token in the answer.
cat >"$work_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"$TEST_ARGV_LOG"
args=("$@")
data=''
for ((i = 0; i < ${#args[@]} - 1; i++)); do
  [[ ${args[i]} != --data ]] || data=${args[i + 1]}
done
[[ ${args[-1]} == "$PI_DISPATCH_URL/api/v1/me/agent-tokens" ]] || { printf 'fake curl: unexpected URL %s\n' "${args[-1]}" >&2; exit 2; }
[[ " $* " == *' -H @- '* && " $* " =~ \ -[A-Za-z]*f ]] || { printf 'fake curl: no -f or no header on stdin: %s\n' "$*" >&2; exit 2; }
jq -e --arg name "$TOKEN_NAME" '. == {name: $name, scopes: []}' >/dev/null 2>&1 <<<"$data" \
  || { printf 'fake curl: the mint asks for %s, not a no-scope token named %s\n' "$data" "$TOKEN_NAME" >&2; exit 2; }
[[ $(cat) == "Authorization: Bearer $OPERATOR_DISPATCH_SECRET" ]] || { printf 'curl: (22) The requested URL returned error: 401\n' >&2; exit 22; }
jq -n --arg token "${TEST_MINT_TOKEN-$DISPATCH_SECRET}" --arg name "$TOKEN_NAME" \
  '{id: "00000000-0000-4000-8000-000000000002", name: $name, prefix: "dispatch", scopes: [], token: $token}'
EOF
# gcloud adds a version of the Pi's Dispatch token secret as Secret Manager does, from a file alone:
# it keeps the file's bytes and mode in $TEST_SECRETS. TEST_GCLOUD_FAIL refuses.
cat >"$work_dir/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gcloud %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $# == 6 && $1 == secrets && $2 == versions && $3 == add && $4 == "$PI_DISPATCH_TOKEN_SECRET" \
  && $5 == "--project=$PI_GCP_PROJECT" && $6 == --data-file=* ]] \
  || { printf 'fake gcloud: unexpected call: gcloud %s\n' "$*" >&2; exit 2; }
[[ -z ${TEST_GCLOUD_FAIL:-} ]] || { printf 'ERROR: (gcloud.secrets.versions.add) PERMISSION_DENIED\n' >&2; exit 1; }
file=${6#--data-file=}
[[ -f $file ]] || { printf 'fake gcloud: no file %s\n' "$file" >&2; exit 1; }
count=$(find "$TEST_SECRETS" -name "$4.*.data" | wc -l)
version=$((count + 1))
cp -- "$file" "$TEST_SECRETS/$4.$version.data"
stat -c %a -- "$file" >"$TEST_SECRETS/$4.$version.mode"
printf 'Created version [%s] of the secret [%s].\n' "$version" "$4" >&2
EOF
chmod +x "$work_dir/bin/"*

block dispatch-token >"$work_dir/dispatch-token.sh"
# The person's own Dispatch token, in the file DISPATCH_TOKEN_FILE names.
(umask 077 && printf '%s\n' "$OPERATOR_DISPATCH_SECRET" >"$work_dir/operator-dispatch-token")
(umask 077 && printf '%s\n' wrong-operator-token >"$work_dir/wrong-dispatch-token")
# run_token_step CASE TOKEN_FILE runs the block as a person pastes it, with plain bash, in the
# checkout, with HOME and TMPDIR in CASE and the token file given second ('' leaves
# DISPATCH_TOKEN_FILE unset). It keeps the exit status in token_step_exit, the output in
# <case>/out, the calls in <case>/argv.log, and the secret's versions in <case>/secrets.
run_token_step() {
  local case_dir=$1 token_file=$2
  mkdir -p "$case_dir/home" "$case_dir/tmp" "$case_dir/secrets"
  : >"$case_dir/argv.log"
  token_step_exit=0
  (
    cd "$repo_root"
    unset DISPATCH_TOKEN_FILE
    [[ -z $token_file ]] || export DISPATCH_TOKEN_FILE=$token_file
    HOME="$case_dir/home" TMPDIR="$case_dir/tmp" GHOST_WISPR_PI_ENV="$test_env" TEST_ARGV_LOG="$case_dir/argv.log" \
      TEST_SECRETS="$case_dir/secrets" PATH="$work_dir/bin:$PATH" bash "$work_dir/dispatch-token.sh"
  ) >"$case_dir/out" 2>&1 || token_step_exit=$?
}
# versions CASE prints how many versions the block added in CASE.
versions() { find "$1/secrets" -name '*.data' | wc -l; }
# left_behind CASE prints each file the block left in CASE's TMPDIR and HOME.
left_behind() { find "$1/tmp" "$1/home" -mindepth 1 -print; }

run_token_step "$work_dir/token" "$work_dir/operator-dispatch-token"
token_out=$(<"$work_dir/token/out")
[[ $token_step_exit == 0 ]] || fail "the dispatch-token block failed: $token_out"
[[ $(versions "$work_dir/token") == 1 ]] \
  || fail "the dispatch-token block added $(versions "$work_dir/token") versions of the secret, not 1: $token_out"
version_file="$work_dir/token/secrets/$PI_DISPATCH_TOKEN_SECRET.1.data"
cmp -s "$version_file" <(printf '%s' "$DISPATCH_SECRET") || cmp -s "$version_file" <(printf '%s\n' "$DISPATCH_SECRET") \
  || fail "the secret's new version is not the minted token with at most one final line feed"
[[ $(<"$work_dir/token/secrets/$PI_DISPATCH_TOKEN_SECRET.1.mode") == 600 ]] \
  || fail "the token reached gcloud in a file of mode $(<"$work_dir/token/secrets/$PI_DISPATCH_TOKEN_SECRET.1.mode"), not 0600"
grep -Fqx "{\"id\":\"00000000-0000-4000-8000-000000000002\",\"name\":\"$TOKEN_NAME\",\"scopes\":[]}" <<<"$token_out" \
  || fail "the dispatch-token block did not print the new token's id, name and scopes: $token_out"
grep -Fq "Created version [1] of the secret [$PI_DISPATCH_TOKEN_SECRET]" <<<"$token_out" \
  || fail "the dispatch-token block did not show the version it added: $token_out"
if grep -F -e "$DISPATCH_SECRET" -e "$OPERATOR_DISPATCH_SECRET" <<<"$token_out" >/dev/null; then
  fail "the dispatch-token block printed a Dispatch token"
fi
[[ -z $(left_behind "$work_dir/token") ]] || fail "the dispatch-token block left files behind: $(left_behind "$work_dir/token")"

# Dispatch refuses the person's token, the mint answers an empty token, the person's token file is
# not named, or Secret Manager refuses: no version is added, and nothing is left behind.
for failure in refused empty unnamed gcloud; do
  case_dir="$work_dir/token-$failure"
  case $failure in
    refused) run_token_step "$case_dir" "$work_dir/wrong-dispatch-token" ;;
    empty) TEST_MINT_TOKEN='' run_token_step "$case_dir" "$work_dir/operator-dispatch-token" ;;
    unnamed) run_token_step "$case_dir" '' ;;
    gcloud) TEST_GCLOUD_FAIL=1 run_token_step "$case_dir" "$work_dir/operator-dispatch-token" ;;
  esac
  [[ $token_step_exit != 0 ]] || fail "the dispatch-token block succeeded although $failure: $(<"$case_dir/out")"
  [[ $(versions "$case_dir") == 0 ]] || fail "the dispatch-token block added a version although $failure"
  [[ -z $(left_behind "$case_dir") ]] || fail "the dispatch-token block left files behind when $failure: $(left_behind "$case_dir")"
done
if grep -q '^gcloud ' "$work_dir/token-refused/argv.log" "$work_dir/token-empty/argv.log" "$work_dir/token-unnamed/argv.log"; then
  fail "the dispatch-token block called gcloud with no token to add"
fi
if grep -q '^curl ' "$work_dir/token-unnamed/argv.log"; then
  fail "the dispatch-token block called Dispatch with no token file named"
fi

# --- accept: on a person's own device, in a checkout.

block accept >"$work_dir/accept-block.sh"
read -r driver <<<"$(named_files <"$work_dir/accept-block.sh")"
[[ $(git -C "$repo_root" ls-files -s -- "$driver") == 100755\ * ]] \
  || fail "the slice 1 block runs $driver, which the checkout does not track as an executable file"
# The block with its driver replaced by one that reports where it ran and what it was given.
# shellcheck disable=SC2016 # The replacement is block text, expanded when the block runs.
sed "s#^${driver//./\\.}\$#\"\$TEST_ACCEPT_DRIVER\"#" "$work_dir/accept-block.sh" >"$work_dir/accept.sh"
grep -q TEST_ACCEPT_DRIVER "$work_dir/accept.sh" || fail "the slice 1 block does not run $driver as a command of its own"
cat >"$work_dir/accept-driver" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$PWD" "$#" >"$TEST_ACCEPT_RAN"
EOF
chmod +x "$work_dir/accept-driver"
mkdir -p "$work_dir/accept/home"
(
  cd "$repo_root"
  HOME="$work_dir/accept/home" TEST_ACCEPT_DRIVER="$work_dir/accept-driver" TEST_ACCEPT_RAN="$work_dir/accept/ran" \
    PATH="$work_dir/bin:$PATH" bash "$work_dir/accept.sh"
) >"$work_dir/accept/out" 2>&1 || fail "the slice 1 block failed: $(<"$work_dir/accept/out")"
[[ -s $work_dir/accept/ran ]] || fail "the slice 1 block did not run its driver"
[[ $(<"$work_dir/accept/ran") == "$repo_root"$'\n'0 ]] \
  || fail "the slice 1 block ran its driver as [$(paste -sd ' ' "$work_dir/accept/ran")], not in the checkout with no argument"

if grep -F -e "$DISPATCH_SECRET" -e "$OPERATOR_DISPATCH_SECRET" "$work_dir"/token*/argv.log >/dev/null; then
  fail "a Dispatch token was passed as a command argument"
fi
printf 'runbook blocks test passed: %d blocks, %d release files, 6 route cases, 5 token cases, the slice 1 driver\n' \
  "${#blocks[@]}" "$(wc -l <<<"$release_files")"
