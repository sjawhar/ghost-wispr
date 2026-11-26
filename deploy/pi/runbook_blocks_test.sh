#!/usr/bin/env bash
# Runs docs/pi-setup.md's own bash blocks, read from the runbook after their runbook_blocks_test.sh
# markers, against fakes, and checks the names in each:
#
#   download-latest  on the Pi: download the latest release's deploy set, check it, and enter the
#                    release directory
#   download-commit  on the Pi: the same for the release whose commit /api/version names
#   provision-key, provision-certificate, provision, models
#                    on the Pi, in the release directory: provision.sh's steps (names and flags
#                    only; provision_run_test.sh runs provision.sh)
#   accept-routes    on the Pi, in the release directory: the route check of the release's
#                    provision.sh, and `tailscale set --accept-routes` only after the check passed
#   claim-name       on a person's own device, in a checkout: claim-tailnet-name.sh with a Tailscale
#                    API access token read into its environment alone
#   deepgram-key     on a person's own device, in a checkout: add the Deepgram key, read without
#                    showing it, as a version of its Secret Manager secret
#   dispatch-token   on a person's own device, in a checkout: mint the Pi's Dispatch token and add it
#                    as a version of its Secret Manager secret
#   token-check      on a person's own device, in a checkout: the hashes of the Pi's token and the
#                    secret's, the recorder's last start of this boot after .env was written, and
#                    Dispatch's whoami with the Pi's token
#   running-settings on a person's own device, in a checkout: mic_device and jabra_mute_button as
#                    the recorder runs them, from its environment, the local settings file or the
#                    installed config
#   accept           on a person's own device, in a checkout: the slice 1 acceptance driver
#
# A block that downloads the release may name only the files the release publishes for the Pi, a
# block that runs in the release directory only files that the release tarball holds, and a block
# that runs in a checkout only files that the checkout tracks. Each PI_ variable a block reads must
# be set by the pi.env of where it runs and by deploy/pi/test-pi.env. Each `./provision.sh` command
# of a release block passes no argument or a flag of provision.sh's case statement. The release
# directory and the published files are what deploy/pi/build-deploy-tarball.sh writes. The blocks
# read the neutral deployment settings in deploy/pi/test-pi.env. The fakes stand in for GitHub, the
# Pi's ip, tailscale, sudo and recorder, the Tailscale API, Dispatch, Secret Manager and the driver,
# so a pass does not show that a procedure works on the Pi.
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

readonly blocks=(download-latest download-commit provision-key provision-certificate provision models
  accept-routes claim-name deepgram-key dispatch-token token-check running-settings accept)
# Where a person runs each block: after a download of the release's published files, in the release
# directory, or in a checkout.
declare -A runs_in=([download-latest]=assets [download-commit]=assets [provision-key]=release
  [provision-certificate]=release [provision]=release [models]=release [accept-routes]=release
  [claim-name]=checkout [deepgram-key]=checkout [dispatch-token]=checkout [token-check]=checkout
  [running-settings]=checkout [accept]=checkout)
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

# Two published releases, as GitHub serves their assets: commit A, the older, and commit B, the
# latest. $work_dir/assets/<tag> holds the files build-deploy-tarball.sh writes for each.
readonly commit_a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa commit_b=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly tag_a=v2026.11.25-aaaaaaa tag_b=v2026.11.26-bbbbbbb
for release in "$commit_a $tag_a" "$commit_b $tag_b"; do
  read -r commit tag <<<"$release"
  mkdir -p "$work_dir/assets/$tag"
  bash "$repo_root/deploy/pi/build-deploy-tarball.sh" "$commit" "$work_dir/ghost-wispr-linux-arm64" "$work_dir/assets/$tag" \
    >"$work_dir/build.out" 2>&1 || fail "deploy/pi/build-deploy-tarball.sh failed: $(<"$work_dir/build.out")"
done
cp -R -- "$work_dir/assets/$tag_b" "$work_dir/assets/latest"
asset_files=$(find "$work_dir/assets/latest" -mindepth 1 -maxdepth 1 -type f -printf '%f\n') \
  || fail "cannot list the published files"
grep -Fqx ghost-wispr-pi-deploy.tar.gz <<<"$asset_files" \
  || fail "UNKNOWN: build-deploy-tarball.sh wrote no ghost-wispr-pi-deploy.tar.gz, so the test cannot tell what a release publishes: $(paste -sd ' ' <<<"$asset_files")"
jq -n --arg a "$commit_a" --arg ta "$tag_a" --arg b "$commit_b" --arg tb "$tag_b" \
  '[{tag_name: $tb, target_commitish: $b}, {tag_name: $ta, target_commitish: $a}]' >"$work_dir/releases.json"

# named_files prints each file the text on stdin names: each word that ends in a file suffix, less a
# leading ./, and not an absolute path, a path under ~ or a path under a variable. The path in the
# default of ${VARIABLE:-path} counts. A word ending in .service or .timer with no / is a systemd
# unit's name, not a file, and the body of a here-document is the script ssh sends to the Pi, which
# names the Pi's files.
named_files() {
  awk '/<<.?EOF.?$/ { print; body = 1; next } body && $0 == "EOF" { body = 0; next } !body { print }' \
    | sed -E 's/\$\{[A-Za-z_][A-Za-z0-9_]*:-/ /g
    s/\$\{?[A-Za-z_][A-Za-z0-9_]*\}?\/[^[:space:]"'\'')]*//g
    s/\$\{?[A-Za-z_][A-Za-z0-9_]*\}?//g' \
    | { grep -oE '[~A-Za-z0-9._/-]+' || true; } \
    | { grep -E '^[A-Za-z0-9_.][A-Za-z0-9._/-]*\.(sh|env|conf|yaml|yml|json|service|timer|rules|gz|sha256|crt|pem|awk|txt)$' || true; } \
    | { grep -vE '^[^/]*\.(service|timer)$' || true; } \
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
      assets) [[ $file != */* ]] && grep -Fqx -- "$file" <<<"$asset_files" || missing+=("$file") ;;
      release) [[ $file != */* ]] && grep -Fqx -- "$file" <<<"$release_files" || missing+=("$file") ;;
      checkout) grep -Fqx -- "$file" <<<"$tracked" || missing+=("$file") ;;
      *) missing+=("$file") ;;
    esac
  done <<<"$files"
  ((${#missing[@]} == 0)) && return 0
  case $where in
    assets) where='release assets' ;;
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
  'checkout source deploy/pi/no-such-settings.env' 'release echo "$PI_HOST"' 'assets tar -xzf ghost-wispr-linux-amd64.tar.gz'; do
  read -r where text <<<"$control"
  if check_named control "$where" "$text" 2>/dev/null; then
    fail "the file check passed a $where block that it must refuse: $text"
  fi
done
# shellcheck disable=SC2016 # The text is block text, never expanded here.
check_named control release 'source "${GHOST_WISPR_PI_ENV:-pi.env}" && ./provision.sh --key' \
  || fail 'the file check refused a release block that names only pi.env and provision.sh'
# A unit's name and the script a here-document sends to the Pi are not the checkout's files; a name
# after the here-document is.
remote_text=$'source deploy/pi/files/pi.env && systemctl --user show ghost-wispr.service &&\nssh pi \'bash -s\' <<\'EOF\'\ncat ghost-wispr.local.yaml\nEOF'
check_named control checkout "$remote_text" || fail 'the file check refused a unit name or a remote script'
check_named control checkout "$remote_text"$'\nsource deploy/pi/no-such-settings.env' 2>/dev/null \
  && fail 'the file check passed a name that comes after a here-document'

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
  if [[ ${runs_in[$name]} == checkout ]]; then
    pi_env=$repo_root/deploy/pi/files/pi.env
  else
    pi_env=$release_dir/pi.env
  fi
  for env_file in "$pi_env" "$test_env"; do
    unset_names=$(unset_in "$env_file" "${variables[@]}")
    [[ -z $unset_names ]] || fail "the $name block reads $(paste -sd ' ' <<<"$unset_names"), which $env_file does not set"
  done
done

# The flags provision.sh takes as its first argument: the arms of its case statement.
# shellcheck disable=SC2016 # ${1:-} is provision.sh's text, which awk compares.
accepted_flags=$(awk '$0 == "case ${1:-} in" { inside = 1; next } inside && $0 == "esac" { exit }
  inside && /^  --[a-z-]+\)$/ { sub(/^  /, ""); sub(/\)$/, ""); print }' "$release_dir/provision.sh")
[[ -n $accepted_flags ]] || fail "UNKNOWN: found no flag in the case statement of the release's provision.sh"
# provision_calls prints one line for each ./provision.sh command in the text on stdin: its first
# argument, or (none) when it passes none. `source ./provision.sh` runs no command.
provision_calls() {
  { grep -oE '(source +)?\./provision\.sh( +[^ ;&|)]+)?' || true; } | { grep -v '^source ' || true; } \
    | sed -E 's#^\./provision\.sh *##; s#^$#(none)#'
}
# check_flags NAME TEXT fails when a ./provision.sh command of TEXT, the block NAME, passes a first
# argument that is not one of accepted_flags.
check_flags() {
  local argument bad=()
  while IFS= read -r argument; do
    [[ $argument == '(none)' ]] || grep -Fqx -- "$argument" <<<"$accepted_flags" || bad+=("$argument")
  done < <(provision_calls <<<"$2")
  ((${#bad[@]} == 0)) && return 0
  printf 'the %s block runs ./provision.sh %s, which is not a flag of provision.sh (%s)\n' "$1" "${bad[*]}" \
    "$(paste -sd ' ' <<<"$accepted_flags")" >&2
  return 1
}
check_flags control './provision.sh --bogus' 2>/dev/null && fail 'the flag check passed ./provision.sh --bogus'
check_flags control 'cd x && ./provision.sh --models abc && ./provision.sh' \
  || fail 'the flag check refused ./provision.sh --models and ./provision.sh with no argument'
[[ -z $(provision_calls <<<"check=\$(bash -c 'source ./provision.sh && f')") ]] \
  || fail 'the flag check counts source ./provision.sh as a command'
provision_commands=0
for name in "${blocks[@]}"; do
  [[ ${runs_in[$name]} == release ]] || continue
  text=$(block "$name")
  check_flags "$name" "$text" 2>"$work_dir/why" || fail "$(<"$work_dir/why")"
  calls=$(provision_calls <<<"$text")
  [[ -z $calls ]] || provision_commands=$((provision_commands + $(grep -c '' <<<"$calls")))
done
((provision_commands >= 4)) \
  || fail "the release blocks run ./provision.sh $provision_commands times, not at least 4 (--key, --certificate, no argument, --models), so the flag check saw too little"

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
# shellcheck source=/dev/null
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

# --- download-latest and download-commit: on the Pi, in any directory.

# curl answers as GitHub and the Pi's recorder do. With -O it writes a release asset into the
# working directory; otherwise it prints the answer. It serves the assets of a tag from
# $TEST_ASSETS/<tag> and those of the latest release from $TEST_ASSETS/latest, the release list
# from $TEST_RELEASES, and /api/version with the commit $TEST_RUNNING_COMMIT.
mkdir -p "$work_dir/download-bin"
cat >"$work_dir/download-bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"$TEST_ARGV_LOG"
write=false url=
for arg; do
  case $arg in
    -*O*) write=true ;;
    -*) ;;
    *) url=$arg ;;
  esac
done
github=https://github.com/sjawhar/ghost-wispr/releases
file='' body=''
case $url in
  http://127.0.0.1:8080/api/version) body=$(jq -cn --arg c "$TEST_RUNNING_COMMIT" '{commit: $c}') ;;
  'https://api.github.com/repos/sjawhar/ghost-wispr/releases?per_page=100') body=$(<"$TEST_RELEASES") ;;
  "$github"/latest/download/*) file=$TEST_ASSETS/latest/${url##*/} ;;
  "$github"/download/*/*) tag=${url#"$github"/download/}; file=$TEST_ASSETS/${tag%%/*}/${url##*/} ;;
  *) printf 'curl: (6) Could not resolve the host of %s\n' "$url" >&2; exit 6 ;;
esac
if [[ -n $file ]]; then
  [[ -f $file ]] || { printf 'curl: (22) The requested URL returned error: 404\n' >&2; exit 22; }
  if $write; then cp -- "$file" "./${url##*/}"; else cat -- "$file"; fi
else
  printf '%s\n' "$body"
fi
EOF
chmod +x "$work_dir/download-bin/curl"

# run_download CASE NAME runs the block NAME as a person pastes it on the Pi, with plain bash, with
# TMPDIR in CASE, and then prints where it left the shell and the commit of the manifest there. It
# keeps the exit status in download_exit and the output in <case>/out. TEST_RUNNING names the
# commit the recorder runs (commit A by default), and TEST_ASSETS_DIR other assets.
run_download() {
  local case_dir=$1 name=$2
  mkdir -p "$case_dir/tmp" "$case_dir/home"
  : >"$case_dir/argv.log"
  # shellcheck disable=SC2016 # The lines are block text, expanded when the block runs.
  { block "$name"; printf '%s\n' 'rc=$?' '[ "$rc" = 0 ] || exit "$rc"' 'printf "at:%s\n" "$PWD"' \
    'jq -r "\"commit:\" + .commit" manifest.json'; } >"$case_dir/run.sh"
  download_exit=0
  (
    cd "$case_dir/home"
    HOME="$case_dir/home" TMPDIR="$case_dir/tmp" TEST_ARGV_LOG="$case_dir/argv.log" \
      TEST_ASSETS="${TEST_ASSETS_DIR:-$work_dir/assets}" TEST_RELEASES="$work_dir/releases.json" \
      TEST_RUNNING_COMMIT="${TEST_RUNNING:-$commit_a}" PATH="$work_dir/download-bin:$PATH" bash "$case_dir/run.sh"
  ) >"$case_dir/out" 2>&1 || download_exit=$?
}
# check_download CASE COMMIT: the block ran, checked the tarball, and left the shell in the release
# directory of COMMIT's release under the case's TMPDIR.
check_download() {
  local out
  out=$(<"$1/out")
  [[ $download_exit == 0 ]] || fail "the download block of $1 failed: $out"
  grep -Fxq 'ghost-wispr-pi-deploy.tar.gz: OK' <<<"$out" || fail "the download block of $1 did not check the tarball: $out"
  grep -Fxq "commit:$2" <<<"$out" || fail "the download block of $1 did not enter the release directory of commit $2: $out"
  [[ $(grep '^at:' <<<"$out") == "at:$1/tmp/"*/ghost-wispr-pi-deploy ]] \
    || fail "the download block of $1 did not leave the shell in a release directory under TMPDIR: $out"
}
run_download "$work_dir/download-latest" download-latest
check_download "$work_dir/download-latest" "$commit_b"
run_download "$work_dir/download-commit-a" download-commit
check_download "$work_dir/download-commit-a" "$commit_a"
TEST_RUNNING=$commit_b run_download "$work_dir/download-commit-b" download-commit
check_download "$work_dir/download-commit-b" "$commit_b"

# The recorder runs a commit that no release has: the block stops before it makes a directory.
TEST_RUNNING=cccccccccccccccccccccccccccccccccccccccc run_download "$work_dir/download-no-release" download-commit
[[ $download_exit != 0 ]] || fail "the download-commit block succeeded for a commit no release has: $(<"$work_dir/download-no-release/out")"
[[ -z $(find "$work_dir/download-no-release/tmp" -mindepth 1 -print -quit) ]] \
  || fail "the download-commit block made a directory for a commit no release has"

# A tarball that does not match its .sha256: each block stops before it unpacks the tarball.
cp -R -- "$work_dir/assets" "$work_dir/tampered-assets"
for tag in latest "$tag_a"; do
  printf 'x' >>"$work_dir/tampered-assets/$tag/ghost-wispr-pi-deploy.tar.gz"
done
for name in download-latest download-commit; do
  TEST_ASSETS_DIR="$work_dir/tampered-assets" run_download "$work_dir/$name-tampered" "$name"
  [[ $download_exit != 0 ]] || fail "the $name block succeeded with a tarball that does not match its .sha256: $(<"$work_dir/$name-tampered/out")"
  [[ -z $(find "$work_dir/$name-tampered/tmp" -name ghost-wispr-pi-deploy -print -quit) ]] \
    || fail "the $name block unpacked a tarball that does not match its .sha256"
done

# --- claim-name: on a person's own device, in a checkout.

# The fake Tailscale API is claim_tailnet_name_test.sh's own curl, read from that test.
mkdir -p "$work_dir/claim-bin"
awk -v start="cat >\"\$work_dir/bin/curl\" <<'EOF'" '$0 == start { capture = 1; next } capture && $0 == "EOF" { exit }
  capture { print }' "$repo_root/deploy/pi/claim_tailnet_name_test.sh" >"$work_dir/claim-bin/curl"
grep -q 'api.tailscale.com' "$work_dir/claim-bin/curl" \
  || fail "UNKNOWN: found no fake Tailscale API curl in deploy/pi/claim_tailnet_name_test.sh"
chmod +x "$work_dir/claim-bin/curl"
readonly TAILSCALE_KEY=fake-tailscale-api-key-for-the-runbook-test
readonly claim_tagged='{"id":"222","name":"ghost-wispr-1.example-tailnet.ts.net","hostname":"ghost-wispr","user":"","tags":["tag:ghost-wispr"],"connectedToControl":true}'
readonly claim_temporary='{"id":"111","name":"ghost-wispr.example-tailnet.ts.net","hostname":"ghost-wispr","user":"operator@example.test","tags":null,"connectedToControl":false}'
block claim-name >"$work_dir/claim-block.sh"
# run_claim CASE INPUT runs the block as a person pastes it, with plain bash, in the checkout, with
# INPUT as what the person types, on a tailnet of the tagged device and the temporary one. Then it
# prints each shell variable but TEST_KEY that still holds the token. It keeps the exit status in
# claim_exit, the output in <case>/out, curl's arguments in <case>/argv and the tailnet in
# <case>/state.json.
run_claim() {
  local case_dir=$1 input=$2
  mkdir -p "$case_dir"
  jq -cn --argjson t "$claim_tagged" --argjson o "$claim_temporary" '{devices: [$t, $o]}' >"$case_dir/state.json"
  cp -- "$case_dir/state.json" "$case_dir/before.json"
  : >"$case_dir/argv"
  # shellcheck disable=SC2016 # The lines are block text, expanded when the block runs.
  { cat "$work_dir/claim-block.sh"; printf '%s\n' 'rc=$?' \
    'for v in $(compgen -v); do [ "$v" = TEST_KEY ] && continue; case ${!v-} in *"$TEST_KEY"*) echo "left:$v" ;; esac; done' \
    'exit "$rc"'; } >"$case_dir/run.sh"
  claim_exit=0
  (
    cd "$repo_root"
    unset TAILSCALE_API_KEY
    TEST_STATE="$case_dir/state.json" TEST_ARGV="$case_dir/argv" TEST_KEY="$TAILSCALE_KEY" GHOST_WISPR_PI_ENV="$test_env" \
      PATH="$work_dir/claim-bin:$PATH" bash "$case_dir/run.sh" <<<"$input"
  ) >"$case_dir/out" 2>&1 || claim_exit=$?
}
run_claim "$work_dir/claim" "$TAILSCALE_KEY"
[[ $claim_exit == 0 ]] || fail "the claim-name block failed: $(grep -vF "$TAILSCALE_KEY" "$work_dir/claim/out")"
[[ $(jq -c '[.devices[] | {id, name}]' "$work_dir/claim/state.json") == '[{"id":"222","name":"ghost-wispr.example-tailnet.ts.net"}]' ]] \
  || fail "the claim-name block left the tailnet as $(jq -c '[.devices[] | {id, name}]' "$work_dir/claim/state.json")"
if grep -qF "$TAILSCALE_KEY" "$work_dir/claim/argv" "$work_dir/claim/out"; then
  fail "the claim-name block put the Tailscale token in an argument or in its output"
fi
! grep -q '^left:' "$work_dir/claim/out" \
  || fail "the claim-name block left the token in the shell: $(grep '^left:' "$work_dir/claim/out" | paste -sd ' ')"
# The person types nothing: the script refuses before it calls the Tailscale API.
run_claim "$work_dir/claim-empty" ''
[[ $claim_exit != 0 ]] || fail "the claim-name block succeeded with no token"
[[ ! -s $work_dir/claim-empty/argv ]] || fail "the claim-name block called the Tailscale API with no token"
cmp -s "$work_dir/claim-empty/before.json" "$work_dir/claim-empty/state.json" || fail "the claim-name block changed the tailnet with no token"

# --- token-check: on a person's own device, in a checkout.

# ssh reaches the fake Pi: it runs the command with the Pi's .env at $TEST_PI_ENV_FILE and the Pi's
# curl, sudo and journalctl first on PATH. gcloud prints the latest version of the Pi's Dispatch
# token secret, $TEST_SECRET_VALUE. The Pi's curl answers as Dispatch's whoami does, for the Pi's
# token alone, given as a header on stdin. The Pi's journalctl prints $TEST_JOURNAL, the recorder's
# journal of this boot.
export PI_HOST
mkdir -p "$work_dir/check-bin" "$work_dir/check-pi-bin"
cat >"$work_dir/check-bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'ssh %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $# == 2 && $1 == "ghost-wispr@$PI_HOST" ]] || { printf 'fake ssh: unexpected call: ssh %s\n' "$*" >&2; exit 255; }
PATH="$TEST_PI_BIN:$PATH" bash -c "${2//\/opt\/ghost-wispr\/.env/$TEST_PI_ENV_FILE}"
EOF
cat >"$work_dir/check-bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gcloud %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $* == "secrets versions access latest --secret=$PI_DISPATCH_TOKEN_SECRET --project=$PI_GCP_PROJECT" ]] \
  || { printf 'fake gcloud: unexpected call: gcloud %s\n' "$*" >&2; exit 2; }
printf '%s' "$TEST_SECRET_VALUE"
EOF
cat >"$work_dir/check-pi-bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'pi curl %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ ${*: -1} == "$PI_DISPATCH_URL/api/v1/whoami" && " $* " == *' -H @- '* ]] \
  || { printf 'fake curl: unexpected call: curl %s\n' "$*" >&2; exit 2; }
[[ $(cat) == "Authorization: Bearer $DISPATCH_SECRET" ]] || { printf 'curl: (22) The requested URL returned error: 401\n' >&2; exit 22; }
printf '{"id":"00000000-0000-4000-8000-000000000002","scopes":[]}\n'
EOF
cat >"$work_dir/check-pi-bin/sudo" <<'EOF'
#!/usr/bin/env bash
[[ $1 == -n ]] || { printf 'fake sudo: no -n\n' >&2; exit 1; }
shift
exec "$@"
EOF
cat >"$work_dir/check-pi-bin/journalctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'pi journalctl %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ " $* " == *' _SYSTEMD_USER_UNIT=ghost-wispr.service '* && " $* " == *' --boot=0 '* && " $* " == *' -o short-unix '* ]] \
  || { printf 'fake journalctl: unexpected call: journalctl %s\n' "$*" >&2; exit 2; }
cat -- "$TEST_JOURNAL"
EOF
chmod +x "$work_dir/check-bin/"* "$work_dir/check-pi-bin/"*
block token-check >"$work_dir/token-check.sh"
readonly empty_sha256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
# The Pi's .env is written at env_written; the recorder's journal of this boot has a start line
# (its "web UI on" line) after that, in journal-started, and only one before it in journal-stale.
readonly env_written=1764214200
printf '%s\n' '1764214100.250000 ghost-wispr ghost-wispr[900]: {"level":"INFO","msg":"ghost-wispr v1 (commit=aaa built=x): web UI on http://127.0.0.1:8080"}' \
  '1764214205.000000 ghost-wispr ghost-wispr[900]: {"level":"INFO","msg":"microphone started at 16000 Hz"}' >"$work_dir/journal-stale"
{ cat "$work_dir/journal-stale"
  printf '%s\n' '1764214260.500000 ghost-wispr ghost-wispr[950]: {"level":"INFO","msg":"ghost-wispr v1 (commit=bbb built=x): web UI on http://127.0.0.1:8080"}'
} >"$work_dir/journal-started"
printf '%s\n' '1764214205.000000 ghost-wispr ghost-wispr[900]: {"level":"INFO","msg":"microphone started at 16000 Hz"}' >"$work_dir/journal-none"
# run_token_check CASE SECRET ENV_LINE runs the block as a person pastes it, with plain bash, in the
# checkout: SECRET is the secret's latest version, and ENV_LINE the Pi's token line of its .env.
# TEST_JOURNAL_FILE names the recorder's journal (journal-started by default).
run_token_check() {
  local case_dir=$1
  mkdir -p "$case_dir"
  : >"$case_dir/argv.log"
  printf 'GHOST_WISPR_ADDR=127.0.0.1:8080\n%s\nGHOST_WISPR_JABRA_MUTE_BUTTON=true\n' "$3" >"$case_dir/pi.env-file"
  touch -d "@$env_written" "$case_dir/pi.env-file"
  token_check_exit=0
  (
    cd "$repo_root"
    GHOST_WISPR_PI_ENV="$test_env" TEST_ARGV_LOG="$case_dir/argv.log" TEST_SECRET_VALUE="$2" \
      TEST_PI_ENV_FILE="$case_dir/pi.env-file" TEST_PI_BIN="$work_dir/check-pi-bin" \
      TEST_JOURNAL="${TEST_JOURNAL_FILE:-$work_dir/journal-started}" PATH="$work_dir/check-bin:$PATH" \
      bash "$work_dir/token-check.sh"
  ) >"$case_dir/out" 2>&1 || token_check_exit=$?
}
run_token_check "$work_dir/token-check" "$DISPATCH_SECRET" "GHOST_WISPR_DISPATCH_TOKEN=$DISPATCH_SECRET"
mapfile -t check_lines <"$work_dir/token-check/out"
[[ $token_check_exit == 0 && ${#check_lines[@]} == 4 ]] \
  || fail "the token-check block did not print two hashes, the recorder's start and the whoami answer: $(<"$work_dir/token-check/out")"
[[ ${check_lines[0]} =~ ^[0-9a-f]{64}\ \ -$ && ${check_lines[0]} == "${check_lines[1]}" && ${check_lines[0]} != "$empty_sha256"* ]] \
  || fail "the token-check block's hashes of the same token are not equal, or are the hash of nothing: $(<"$work_dir/token-check/out")"
[[ ${check_lines[2]} == "the recorder started at 1764214260.500000, after .env was written at $env_written"* ]] \
  || fail "the token-check block did not say that the recorder's last start of this boot came after .env was written: ${check_lines[2]}"
[[ ${check_lines[3]} == '{"scopes":[]}' ]] || fail "the token-check block's whoami printed ${check_lines[3]}, not {\"scopes\":[]}"
# The secret holds another token: the two hashes differ, so the reading can fail.
run_token_check "$work_dir/token-check-other" other-token-value "GHOST_WISPR_DISPATCH_TOKEN=$DISPATCH_SECRET"
mapfile -t check_lines <"$work_dir/token-check-other/out"
[[ ${check_lines[0]-} != "${check_lines[1]-}" ]] || fail "the token-check block printed equal hashes for two tokens"
# The Pi has no token line: its hash is the hash of nothing, and whoami prints no scopes.
run_token_check "$work_dir/token-check-no-token" "$DISPATCH_SECRET" ''
[[ $(head -n 1 "$work_dir/token-check-no-token/out") == "$empty_sha256"* ]] \
  || fail "the token-check block did not print the hash of nothing for a Pi with no token: $(<"$work_dir/token-check-no-token/out")"
! grep -Fqx '{"scopes":[]}' "$work_dir/token-check-no-token/out" \
  || fail "the token-check block's whoami printed scopes for a Pi with no token"
# The recorder's last start of this boot came before .env was written, or the journal of this boot
# has no start: the block stops with a line that says so, and reads no whoami.
for journal in stale none; do
  TEST_JOURNAL_FILE="$work_dir/journal-$journal" run_token_check "$work_dir/token-check-$journal" "$DISPATCH_SECRET" \
    "GHOST_WISPR_DISPATCH_TOKEN=$DISPATCH_SECRET"
  [[ $token_check_exit != 0 ]] || fail "the token-check block passed with a recorder journal that is $journal: $(<"$work_dir/token-check-$journal/out")"
  ! grep -Fqx '{"scopes":[]}' "$work_dir/token-check-$journal/out" \
    || fail "the token-check block went on to whoami with a recorder journal that is $journal"
done
grep -Fq "the recorder started at 1764214100.250000, before .env was written at $env_written" "$work_dir/token-check-stale/out" \
  || fail "the token-check block did not say the recorder started before .env was written: $(<"$work_dir/token-check-stale/out")"
grep -Fq 'found no recorder start in the journal of this boot' "$work_dir/token-check-none/out" \
  || fail "the token-check block did not say it found no recorder start: $(<"$work_dir/token-check-none/out")"

# --- deepgram-key: on a person's own device, in a checkout.

# gcloud adds a version of the Pi's Deepgram key secret as Secret Manager does, from a file alone:
# it records its arguments, refuses an empty file, and keeps the file's bytes and mode in
# $TEST_SECRETS. TEST_GCLOUD_FAIL refuses every call.
export PI_DEEPGRAM_SECRET
readonly DEEPGRAM_SECRET=deepgram-key-value-0123456789abcdef
mkdir -p "$work_dir/deepgram-bin"
cat >"$work_dir/deepgram-bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gcloud %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $# == 6 && $1 == secrets && $2 == versions && $3 == add && $4 == "$PI_DEEPGRAM_SECRET" \
  && $5 == "--project=$PI_GCP_PROJECT" && $6 == --data-file=* ]] \
  || { printf 'fake gcloud: unexpected call: gcloud %s\n' "$*" >&2; exit 2; }
[[ -z ${TEST_GCLOUD_FAIL:-} ]] || { printf 'ERROR: (gcloud.secrets.versions.add) PERMISSION_DENIED\n' >&2; exit 1; }
file=${6#--data-file=}
[[ -s $file ]] || { printf 'ERROR: (gcloud.secrets.versions.add) INVALID_ARGUMENT: the data file %s is empty\n' "$file" >&2; exit 1; }
version=$(($(find "$TEST_SECRETS" -name "$4.*.data" | wc -l) + 1))
cp -- "$file" "$TEST_SECRETS/$4.$version.data"
stat -c %a -- "$file" >"$TEST_SECRETS/$4.$version.mode"
printf 'Created version [%s] of the secret [%s].\n' "$version" "$4" >&2
EOF
chmod +x "$work_dir/deepgram-bin/gcloud"
block deepgram-key >"$work_dir/deepgram-block.sh"
# run_deepgram CASE INPUT runs the block as a person pastes it, with plain bash, in the checkout,
# with INPUT as what the person types and HOME and TMPDIR in CASE, then prints each shell variable
# that still holds the key. It keeps the exit status in deepgram_exit, the output in <case>/out,
# the calls in <case>/argv.log and the secret's versions in <case>/secrets.
run_deepgram() {
  local case_dir=$1 input=$2
  mkdir -p "$case_dir/home" "$case_dir/tmp" "$case_dir/secrets"
  : >"$case_dir/argv.log"
  # shellcheck disable=SC2016 # The lines are block text, expanded when the block runs.
  { cat "$work_dir/deepgram-block.sh"; printf '%s\n' 'rc=$?' \
    'for v in $(compgen -v); do [ "$v" = TEST_DEEPGRAM ] && continue; case ${!v-} in *"$TEST_DEEPGRAM"*) echo "left:$v" ;; esac; done' \
    'exit "$rc"'; } >"$case_dir/run.sh"
  deepgram_exit=0
  (
    cd "$repo_root"
    HOME="$case_dir/home" TMPDIR="$case_dir/tmp" GHOST_WISPR_PI_ENV="$test_env" TEST_ARGV_LOG="$case_dir/argv.log" \
      TEST_SECRETS="$case_dir/secrets" TEST_DEEPGRAM="$DEEPGRAM_SECRET" PATH="$work_dir/deepgram-bin:$PATH" \
      bash "$case_dir/run.sh" <<<"$input"
  ) >"$case_dir/out" 2>&1 || deepgram_exit=$?
}
run_deepgram "$work_dir/deepgram" "$DEEPGRAM_SECRET"
[[ $deepgram_exit == 0 ]] || fail "the deepgram-key block failed: $(grep -vF "$DEEPGRAM_SECRET" "$work_dir/deepgram/out")"
[[ $(versions "$work_dir/deepgram") == 1 ]] || fail "the deepgram-key block added $(versions "$work_dir/deepgram") versions, not 1"
cmp -s "$work_dir/deepgram/secrets/$PI_DEEPGRAM_SECRET.1.data" <(printf '%s' "$DEEPGRAM_SECRET") \
  || fail "the Deepgram key's new version is not the typed key, byte for byte"
[[ $(<"$work_dir/deepgram/secrets/$PI_DEEPGRAM_SECRET.1.mode") == 600 ]] \
  || fail "the Deepgram key reached gcloud in a file of mode $(<"$work_dir/deepgram/secrets/$PI_DEEPGRAM_SECRET.1.mode"), not 0600"
grep -Fq "Created version [1] of the secret [$PI_DEEPGRAM_SECRET]" "$work_dir/deepgram/out" \
  || fail "the deepgram-key block did not show the version it added"
if grep -qF "$DEEPGRAM_SECRET" "$work_dir/deepgram/argv.log" "$work_dir/deepgram/out"; then
  fail "the deepgram-key block put the key in an argument or in its output"
fi
[[ -z $(left_behind "$work_dir/deepgram") ]] || fail "the deepgram-key block left files behind: $(left_behind "$work_dir/deepgram")"
# The person types nothing, or Secret Manager refuses: no version, and nothing left behind. An empty
# read reaches no gcloud call at all.
for failure in empty gcloud; do
  case_dir="$work_dir/deepgram-$failure"
  case $failure in
    empty) run_deepgram "$case_dir" '' ;;
    gcloud) TEST_GCLOUD_FAIL=1 run_deepgram "$case_dir" "$DEEPGRAM_SECRET" ;;
  esac
  [[ $deepgram_exit != 0 ]] || fail "the deepgram-key block succeeded although $failure"
  [[ $(versions "$case_dir") == 0 ]] || fail "the deepgram-key block added a version although $failure"
  [[ -z $(left_behind "$case_dir") ]] || fail "the deepgram-key block left files behind when $failure: $(left_behind "$case_dir")"
done
! grep -q '^gcloud ' "$work_dir/deepgram-empty/argv.log" \
  || fail "the deepgram-key block called gcloud with an empty key: $(<"$work_dir/deepgram-empty/argv.log")"
if grep -q '^left:' "$work_dir"/deepgram*/out; then
  fail "the deepgram-key block left the key in the shell: $(grep -h '^left:' "$work_dir"/deepgram*/out | paste -sd ' ')"
fi

# --- running-settings: on a person's own device, in a checkout.

# ssh reaches the fake Pi: it runs the script on stdin with /opt/ghost-wispr at $TEST_PI_DIR and
# /proc at $TEST_PROC, and the Pi's systemctl first on PATH, which names $TEST_MAIN_PID as the
# recorder's main process.
mkdir -p "$work_dir/settings-bin" "$work_dir/settings-pi-bin"
cat >"$work_dir/settings-bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'ssh %s\n' "$*" >>"$TEST_ARGV_LOG"
[[ $# == 2 && $1 == "ghost-wispr@$PI_HOST" && $2 == 'bash -s' ]] \
  || { printf 'fake ssh: unexpected call: ssh %s\n' "$*" >&2; exit 255; }
sed -e "s#/opt/ghost-wispr#$TEST_PI_DIR#g" -e "s#/proc/#$TEST_PROC/#g" | PATH="$TEST_PI_BIN:$PATH" bash -s
EOF
cat >"$work_dir/settings-pi-bin/systemctl" <<'EOF'
#!/usr/bin/env bash
[[ $* == '--user show -p MainPID --value ghost-wispr.service' ]] || { printf 'fake systemctl: unexpected call: systemctl %s\n' "$*" >&2; exit 2; }
printf '%s\n' "$TEST_MAIN_PID"
EOF
chmod +x "$work_dir/settings-bin/ssh" "$work_dir/settings-pi-bin/systemctl"
block running-settings >"$work_dir/running-settings.sh"
# run_settings CASE LOCAL ENVIRONMENT... runs the block as a person pastes it, with plain bash, in
# the checkout, on a Pi whose installed config is the template, whose local settings file holds
# LOCAL (none when empty), and whose recorder (main process 4242) runs with ENVIRONMENT, which
# always holds the Pi's Deepgram key. TEST_MAIN_PID_VALUE names another main process.
run_settings() {
  local case_dir=$1 local_settings=$2
  shift 2
  mkdir -p "$case_dir/opt" "$case_dir/proc/4242"
  : >"$case_dir/argv.log"
  cp -- "$repo_root/deploy/pi/files/ghost-wispr.yaml" "$case_dir/opt/ghost-wispr.yaml"
  [[ -z $local_settings ]] || printf '%s\n' "$local_settings" >"$case_dir/opt/ghost-wispr.local.yaml"
  printf '%s\0' "GHOST_WISPR_DEEPGRAM_API_KEY=$DEEPGRAM_SECRET" "$@" >"$case_dir/proc/4242/environ"
  settings_exit=0
  (
    cd "$repo_root"
    GHOST_WISPR_PI_ENV="$test_env" TEST_ARGV_LOG="$case_dir/argv.log" TEST_PI_DIR="$case_dir/opt" \
      TEST_PROC="$case_dir/proc" TEST_PI_BIN="$work_dir/settings-pi-bin" TEST_MAIN_PID="${TEST_MAIN_PID_VALUE:-4242}" \
      PATH="$work_dir/settings-bin:$PATH" bash "$work_dir/running-settings.sh"
  ) >"$case_dir/out" 2>&1 || settings_exit=$?
}
# check_settings CASE LINE...: the block passed and printed exactly LINE..., and not the key.
check_settings() {
  local case_dir=$1
  shift
  [[ $settings_exit == 0 ]] || fail "the running-settings block failed in $case_dir: $(grep -vF "$DEEPGRAM_SECRET" "$case_dir/out")"
  ! grep -qF "$DEEPGRAM_SECRET" "$case_dir/out" || fail "the running-settings block printed the recorder's Deepgram key"
  [[ $(<"$case_dir/out") == "$(printf '%s\n' "$@")" ]] \
    || fail "the running-settings block printed [$(<"$case_dir/out")] in $case_dir, not [$(printf '%s\n' "$@")]"
}
run_settings "$work_dir/settings-template" '' HOME=/home/ghost-wispr
check_settings "$work_dir/settings-template" 'mic_device: J75 (from ghost-wispr.yaml)' \
  'jabra_mute_button: true (from ghost-wispr.yaml)'
run_settings "$work_dir/settings-local" $'mic_device: Loopback\njabra_mute_button: false'
check_settings "$work_dir/settings-local" 'mic_device: Loopback (from ghost-wispr.local.yaml)' \
  'jabra_mute_button: false (from ghost-wispr.local.yaml)'
run_settings "$work_dir/settings-environment" $'mic_device: Loopback\njabra_mute_button: false' \
  GHOST_WISPR_MIC_DEVICE=plughw:CARD=Loopback,DEV=0 GHOST_WISPR_JABRA_MUTE_BUTTON=1
check_settings "$work_dir/settings-environment" \
  'mic_device: plughw:CARD=Loopback,DEV=0 (from GHOST_WISPR_MIC_DEVICE in the recorder'"'"'s environment)' \
  'jabra_mute_button: true (from GHOST_WISPR_JABRA_MUTE_BUTTON in the recorder'"'"'s environment)'
# An empty variable sets nothing, as the recorder reads it.
run_settings "$work_dir/settings-empty-variable" '' GHOST_WISPR_MIC_DEVICE= GHOST_WISPR_JABRA_MUTE_BUTTON=
check_settings "$work_dir/settings-empty-variable" 'mic_device: J75 (from ghost-wispr.yaml)' \
  'jabra_mute_button: true (from ghost-wispr.yaml)'
# The recorder runs no process: the block stops and says so.
TEST_MAIN_PID_VALUE=0 run_settings "$work_dir/settings-stopped" ''
[[ $settings_exit != 0 ]] || fail "the running-settings block passed with no recorder process: $(<"$work_dir/settings-stopped/out")"
grep -Fq 'ghost-wispr.service runs no process' "$work_dir/settings-stopped/out" \
  || fail "the running-settings block did not say the recorder runs no process: $(<"$work_dir/settings-stopped/out")"

if grep -F -e "$DISPATCH_SECRET" -e "$OPERATOR_DISPATCH_SECRET" "$work_dir"/token*/argv.log >/dev/null; then
  fail "a Dispatch token was passed as a command argument"
fi
printf 'runbook blocks test passed: %d blocks, %d release files, %d provision.sh commands, 6 download cases, 6 route cases, 2 claim cases, 3 Deepgram cases, 5 token cases, 5 token-check cases, 5 settings cases, the slice 1 driver\n' \
  "${#blocks[@]}" "$(wc -l <<<"$release_files")" "$provision_commands"
