#!/usr/bin/env bash
# Runs the full deploy/pi/provision.sh on a credential bundle with fakes for sudo, id, ip and
# tailscale, and checks what the Pi is told to do: the service user joins the audio group, the
# update runs with the user's runtime directory, and the detached join brings the node up with the
# tailnet's subnet routes off, sets Tailscale Serve again, and accepts the routes only when the route
# check that provision.sh installs beside the token finds that no peer's route overlaps a network of
# the Pi, on a re-join too. It runs that check alone on the Pi's networks and on tailnet statuses of
# each kind, and checks that a Pi already joined gets no join and no route check.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'provision run test FAILED: %s\n' "$*" >&2
  exit 1
}

# The sha256 of the office Pi's recording_rules block in deploy/pi/files/ghost-wispr.yaml, which
# its web UI shows under Recording rules, each as written and in this order (docs/pi-setup.md,
# "Accept slice 1", A2). The test holds the hash, not the text, because the repository is public.
# After a rule change, set it to the awk below run over the template, piped to sha256sum.
readonly recording_rules_sha256=89f65b9df94d27abf6657ba13e183921b7c27e19d8ba5e90c189e4046c5c8040

# assert_recording_rules fails unless the configuration provision.sh installed holds exactly the
# office Pi's recording rules, once.
assert_recording_rules() {
  local config=$TEST_LOG.installed-ghost-wispr.yaml rules
  rules=$(awk '/^recording_rules:/ { print; inside = 1; next } inside && /^  - / { print; next } { inside = 0 }' "$config" | sha256sum)
  [[ $rules == "$recording_rules_sha256  -" ]] \
    || fail "the $1 configuration does not hold the office Pi's five recording rules: $(<"$config")"
}

# assert_office_pi_devices fails unless the configuration provision.sh installed turns the Jabra's
# Mute button on once, with no .env line to override it, and names the Jabra Speak2 75 by its ALSA
# card id as the microphone's and the speaker's sound card once (mic_device and speaker_device: J75),
# so another capture card, such as an ALSA loopback or a second Jabra device, never counts as the
# microphone. The full provisioner rewrites .env, so the template's settings are the Pi's.
assert_office_pi_devices() {
  local config=$TEST_LOG.installed-ghost-wispr.yaml env=$TEST_LOG.installed-.env settings
  settings=$(grep -E '^jabra_mute_button:' "$config") || true
  [[ $settings == 'jabra_mute_button: true' ]] \
    || fail "the $1 configuration does not turn the Jabra's Mute button on once: ${settings:-no jabra_mute_button line}"
  if grep -Eq '^GHOST_WISPR_JABRA_MUTE_BUTTON=' "$env"; then
    fail "the $1 .env sets the Jabra's Mute button: $(grep -E '^GHOST_WISPR_JABRA_MUTE_BUTTON=' "$env")"
  fi
  settings=$(grep -E '^mic_device:' "$config") || true
  [[ $settings == 'mic_device: J75' ]] \
    || fail "the $1 configuration does not name the Jabra as the microphone once: ${settings:-no mic_device line}"
  settings=$(grep -E '^speaker_device:' "$config") || true
  [[ $settings == 'speaker_device: J75' ]] \
    || fail "the $1 configuration does not name the Jabra as the speaker once: ${settings:-no speaker_device line}"
}

uid=$(id -u)
readonly uid runtime_dir="/run/user/$uid"
[[ -d $runtime_dir ]] || fail "this test needs $runtime_dir, where provision.sh stages the bundle"

readonly CLIENT_ID=k-test-client-id SUBJECT=123456789012345678901
export CLIENT_ID SUBJECT TEST_LOG="$work_dir/log" TEST_UID="$uid" TEST_ROUTE_ALL="$work_dir/route-all"
# The join's directory, where provision.sh installs the token and the route check: the fake sudo
# creates nothing.
mkdir -p "$work_dir/bin" "$work_dir/deploy" "$work_dir/bundle" "$work_dir/join"
touch "$work_dir/key.pem"

cat >"$work_dir/bin/id" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  -un) printf 'ghost-wispr\n' ;;
  '-u ghost-wispr') printf '%s\n' "$TEST_UID" ;;
  '-nG ghost-wispr') printf 'ghost-wispr sudo plugdev input\n' ;;
  *) printf 'fake id: %s\n' "$*" >&2; exit 1 ;;
esac
EOF

# sudo records each command and runs none, but systemd-run's command is kept to run below, with the
# whole systemd-run line, and what provision.sh installs from stdin or as the configuration is kept
# to read.
cat >"$work_dir/bin/sudo" <<'EOF'
#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >>"$TEST_LOG"
if [[ " $* " == *' /dev/stdin '* ]]; then
  cat >"$TEST_LOG.installed-$(basename -- "${!#}")"
elif [[ $1 == install && ${!#} == */ghost-wispr.yaml ]]; then
  cp -- "${@:$#-1:1}" "$TEST_LOG.installed-ghost-wispr.yaml"
fi
if [[ $1 == systemd-run ]]; then
  printf '%s' "${!#}" >"$TEST_LOG.join"
  printf '%s\n' "$*" >"$TEST_LOG.systemd-run"
fi
EOF

# tailscale logs each call and keeps the accept-routes setting, RouteAll, in $TEST_ROUTE_ALL (false
# when that file is absent):
# - `up` exits TEST_UP_EXIT (0 by default), and on 0 sets RouteAll as its --accept-routes flag says.
#   Without the flag, while RouteAll is true, it refuses as up.go's check of settings left out does
#   (Tailscale v1.104.1, cmd/tailscale/cli/up.go:1013-1105) and exits 1. It keeps its exit code in
#   $TEST_LOG.up-exit;
# - `set --accept-routes` exits TEST_SET_EXIT (0 by default), and on 0 sets RouteAll true;
#   `set --accept-routes=false` sets it false;
# - `status --self --json` prints the node's own reading, TEST_SELF_STATUS when it is set;
# - `status --json` prints the status fixture $TEST_STATUS, or fails with TEST_STATUS_EXIT;
# - `serve` exits TEST_SERVE_EXIT (0 by default), and `debug prefs` prints RouteAll.
cat >"$work_dir/bin/tailscale" <<'EOF'
#!/usr/bin/env bash
printf 'tailscale %s\n' "$*" >>"$TEST_LOG"
unexpected() {
  printf 'fake tailscale: unexpected call: tailscale %s\n' "$*" >&2
  exit 2
}
route_all=$(cat -- "$TEST_ROUTE_ALL" 2>/dev/null) || route_all=false
case $1 in
  up)
    code=${TEST_UP_EXIT:-0} flag=
    for argument in "${@:2}"; do
      case $argument in
        --accept-routes | --accept-routes=true) flag=true ;;
        --accept-routes=false) flag=false ;;
      esac
    done
    if [[ $code == 0 && -z $flag && $route_all == true ]]; then
      printf "Error: changing settings via 'tailscale up' requires mentioning all\n" >&2
      code=1
    elif [[ $code == 0 && -n $flag ]]; then
      printf '%s\n' "$flag" >"$TEST_ROUTE_ALL"
    fi
    printf '%s\n' "$code" >"$TEST_LOG.up-exit"
    exit "$code"
    ;;
  set)
    case ${*:2} in
      --accept-routes | --accept-routes=true)
        [[ ${TEST_SET_EXIT:-0} != 0 ]] || printf 'true\n' >"$TEST_ROUTE_ALL"
        exit "${TEST_SET_EXIT:-0}"
        ;;
      --accept-routes=false) printf 'false\n' >"$TEST_ROUTE_ALL" ;;
      *) unexpected "$@" ;;
    esac
    ;;
  status)
    case ${*:2} in
      '--self --json')
        if [[ -n ${TEST_SELF_STATUS:-} ]]; then
          printf '%s\n' "$TEST_SELF_STATUS"
        else
          printf '{"Self":{"Tags":null,"DNSName":"ghost-wispr.example-tailnet.ts.net."}}\n'
        fi
        ;;
      --json)
        if [[ ${TEST_STATUS_EXIT:-0} != 0 ]]; then
          printf 'failed to connect to the local Tailscale daemon\n' >&2
          exit "$TEST_STATUS_EXIT"
        fi
        cat -- "$TEST_STATUS"
        ;;
      *) unexpected "$@" ;;
    esac
    ;;
  serve) exit "${TEST_SERVE_EXIT:-0}" ;;
  debug)
    [[ $* == 'debug prefs' ]] || unexpected "$@"
    printf '{"RouteAll":%s}\n' "$route_all"
    ;;
  *) unexpected "$@" ;;
esac
EOF

# ip prints the case's networks, the fixture $TEST_NETWORKS, as `ip -4 -o addr` does, or fails with
# TEST_IP_EXIT.
cat >"$work_dir/bin/ip" <<'EOF'
#!/usr/bin/env bash
[[ $* == '-4 -o addr' ]] || { printf 'fake ip: unexpected call: ip %s\n' "$*" >&2; exit 2; }
if [[ ${TEST_IP_EXIT:-0} != 0 ]]; then
  printf 'Cannot open netlink socket: Permission denied\n' >&2
  exit "$TEST_IP_EXIT"
fi
cat -- "$TEST_NETWORKS"
EOF

# The release's update.sh and binary, as provision.sh runs them from the deploy directory. The real
# update.sh reads /api/status once, and fails when the recorder runs but does not answer yet, as
# it does right after a restart: so does this one when the recorder was restarted before it.
cat >"$work_dir/deploy/update.sh" <<'EOF'
#!/usr/bin/env bash
printf 'update.sh XDG_RUNTIME_DIR=%s\n' "${XDG_RUNTIME_DIR:-}" >>"$TEST_LOG"
if grep -q 'systemctl --user try-restart ghost-wispr.service$' "$TEST_LOG"; then
  printf 'ghost-wispr update: ghost-wispr is active but /api/status gave no status\n' >&2
  exit 1
fi
EOF
cat >"$work_dir/deploy/ghost-wispr" <<'EOF'
#!/usr/bin/env bash
payload=$(printf '{"sub":"%s","aud":"api.tailscale.com/%s"}' "$SUBJECT" "$CLIENT_ID" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJSUzI1NiJ9.%s.c2lnbmF0dXJl\n' "$payload"
EOF
chmod +x "$work_dir/bin/"* "$work_dir/deploy/update.sh" "$work_dir/deploy/ghost-wispr"

printf 'CERTIFICATE\n' >"$work_dir/bundle/device.crt"
printf '{"type":"external_account","credential_source":{"certificate":{"certificate_config_location":"/opt/ghost-wispr/certificate_config.json"}}}\n' \
  >"$work_dir/bundle/google-credentials.json"
printf '{}\n' >"$work_dir/bundle/certificate_config.json"
printf '%s\n' "$CLIENT_ID" >"$work_dir/bundle/tailscale-client-id"
printf '%s\n' "$SUBJECT" >"$work_dir/bundle/tailscale-subject"
printf 'deepgram-key' >"$work_dir/bundle/deepgram-key"

# run_provision runs provision.sh on a bundle of the named files, with neutral deployment settings
# and the join's directory in $work_dir/join.
run_provision() {
  tar -C "$work_dir/bundle" -cf - "$@" \
    | env -u XDG_RUNTIME_DIR PATH="$work_dir/bin:$PATH" GHOST_WISPR_DEPLOY_DIR="$work_dir/deploy" \
      GHOST_WISPR_IDENTITY_KEY="$work_dir/key.pem" GHOST_WISPR_PI_ENV="$repo_root/deploy/pi/test-pi.env" \
      GHOST_WISPR_JOIN_DIR="$work_dir/join" bash "$repo_root/deploy/pi/provision.sh" >"$work_dir/out" 2>&1
}
readonly bundle_files=(device.crt google-credentials.json certificate_config.json tailscale-client-id
  tailscale-subject deepgram-key)

# The bundle holds no Envoy file: the recorder publishes to the Envoy hub with no credential, and
# its config names the hub. A bundle that still carries a webhook file is refused before anything
# is installed.
for webhook_file in envoy-webhook-secret envoy-webhook-url; do
  printf 'webhook\n' >"$work_dir/bundle/$webhook_file"
  if run_provision "${bundle_files[@]}" "$webhook_file"; then
    fail "provision.sh accepted a bundle with $webhook_file"
  fi
  grep -Fq "credential bundle contains unexpected path: $webhook_file" "$work_dir/out" \
    || fail "provision.sh did not name $webhook_file when it refused the bundle: $(<"$work_dir/out")"
  [[ ! -e $TEST_LOG ]] || fail "provision.sh ran commands for a refused bundle: $(<"$TEST_LOG")"
done

run_provision "${bundle_files[@]}" || fail "provision.sh failed: $(<"$work_dir/out")"
# A run that works says no bash error.
if grep -E 'provision\.sh: line [0-9]+: ' "$work_dir/out" >&2; then
  fail "provision.sh printed a bash error (above)"
fi
assert_recording_rules 'installed'
assert_office_pi_devices 'installed'
# The full provisioner installs the template as it is: the Envoy hub it names, no placeholder.
cmp -s "$repo_root/deploy/pi/files/ghost-wispr.yaml" "$TEST_LOG.installed-ghost-wispr.yaml" \
  || fail "the installed configuration is not deploy/pi/files/ghost-wispr.yaml"

# The .env the recorder runs with holds the Deepgram key and no Envoy webhook signing key.
env_file="$TEST_LOG.installed-.env"
deepgram_key_name="DEEPGRAM""_API_KEY"
grep -Fxq "GHOST_WISPR_$deepgram_key_name=deepgram-key" "$env_file" || fail "the installed .env has no Deepgram key"
if grep -q 'ENVOY_WEBHOOK_SECRET' "$env_file"; then
  fail "the installed .env still holds an Envoy webhook secret"
fi
# The full provisioner writes .env anew, with only variables Ghost Wispr reads: no variable of a
# setting that providers replaced, such as the Vertex embedding location, which a start warns about.
env_names=$(cut -d= -f1 "$env_file" | LC_ALL=C sort | tr '\n' ' ')
[[ $env_names == "GHOST_WISPR_ADDR GHOST_WISPR_$deepgram_key_name GOOGLE_API_USE_CLIENT_CERTIFICATE " ]] \
  || fail "the installed .env sets $env_names, want GHOST_WISPR_ADDR, GHOST_WISPR_$deepgram_key_name and GOOGLE_API_USE_CLIENT_CERTIFICATE"
# Without a dispatch-token in the bundle, the .env gives the meeting voice no Dispatch token.
dispatch_token_name="DISPATCH""_TOKEN"
if grep -q "^GHOST_WISPR_$dispatch_token_name=" "$env_file"; then
  fail "the installed .env has a Dispatch token the bundle did not carry"
fi

# The unit's own Dispatch token, when the bundle carries one, reaches the .env for the Concierge,
# and a token that is not one line is refused. Each run starts a fresh log, as a first run does.
: >"$TEST_LOG"
printf 'dispatch-token-value' >"$work_dir/bundle/dispatch-token"
run_provision "${bundle_files[@]}" dispatch-token || fail "provision.sh failed with a dispatch-token: $(<"$work_dir/out")"
grep -Fxq "GHOST_WISPR_$dispatch_token_name=dispatch-token-value" "$TEST_LOG.installed-.env" \
  || fail "the installed .env has no Dispatch token from the bundle's dispatch-token"
printf 'two\nlines' >"$work_dir/bundle/dispatch-token"
if run_provision "${bundle_files[@]}" dispatch-token; then
  fail "provision.sh accepted a dispatch-token of two lines"
fi
rm "$work_dir/bundle/dispatch-token"
# A dispatch-token that is a link to nothing is refused, not skipped as absent.
ln -s "$work_dir/no-such-token" "$work_dir/bundle/dispatch-token"
if run_provision "${bundle_files[@]}" dispatch-token; then
  fail "provision.sh accepted a dispatch-token that links to nothing"
fi
grep -q 'dispatch-token' "$work_dir/out" || fail "provision.sh refused the linked dispatch-token for another reason: $(<"$work_dir/out")"
rm "$work_dir/bundle/dispatch-token"

# A lingering user service has no seat, so only the audio group opens the ALSA devices.
grep -Fxq 'sudo usermod -aG audio ghost-wispr' "$TEST_LOG" || fail "provision.sh does not add ghost-wispr to the audio group"
if grep -Eq '^sudo usermod -aG (plugdev|input) ' "$TEST_LOG"; then
  fail "provision.sh re-added a group ghost-wispr already has"
fi
# update.sh runs systemctl --user, which needs the user's runtime directory.
grep -Fxq "update.sh XDG_RUNTIME_DIR=$runtime_dir" "$TEST_LOG" || fail "update.sh ran without XDG_RUNTIME_DIR=$runtime_dir"
[[ -s $TEST_LOG.join ]] || fail "provision.sh started no detached join"

# The service starts only once the Pi's clock is synchronized, so no session is named or stamped
# with the time a Pi with no battery-backed clock restores at boot. A user unit cannot be ordered
# after the system's time-sync.target, so the unit waits in a start step: provision.sh installs it
# beside the binary, the unit runs it after the network wait and before Ghost Wispr, and the unit's
# start time limit is longer than both waits together, so a Pi whose clock never synchronizes still
# starts once the wait gives up instead of being killed and restarted for good.
grep -Fxq "sudo install -o ghost-wispr -g ghost-wispr -m 0755 $repo_root/deploy/pi/files/wait-for-time-sync.sh $work_dir/deploy/wait-for-time-sync.sh" "$TEST_LOG" \
  || fail "provision.sh does not install wait-for-time-sync.sh beside the binary"
unit="$repo_root/deploy/pi/files/ghost-wispr.service"
mapfile -t start_steps < <(grep -E '^Exec(StartPre|Start)=' "$unit")
want_steps=(ExecStartPre=/opt/ghost-wispr/wait-for-network.sh ExecStartPre=/opt/ghost-wispr/wait-for-time-sync.sh
  ExecStart=/opt/ghost-wispr/ghost-wispr)
[[ "${start_steps[*]}" == "${want_steps[*]}" ]] \
  || fail "ghost-wispr.service starts with [${start_steps[*]}], want [${want_steps[*]}]"
# The paused listener's ring of room audio must not reach disk through a core dump or swap.
for limit in LimitCORE=0 MemorySwapMax=0; do
  grep -qx -- "$limit" "$unit" || fail "ghost-wispr.service does not set $limit"
done
network_wait=$(sed -n 's/.*GHOST_WISPR_NETWAIT_TIMEOUT:-\([0-9][0-9]*\).*/\1/p' "$repo_root/wait-for-network.sh")
sync_wait=$(sed -n 's/.*GHOST_WISPR_TIMESYNC_TIMEOUT:-\([0-9][0-9]*\).*/\1/p' "$repo_root/deploy/pi/files/wait-for-time-sync.sh")
[[ -n $network_wait && -n $sync_wait ]] || fail "cannot read the network wait ($network_wait) or the clock wait ($sync_wait)"
start_limit=$(sed -n 's/^TimeoutStartSec=//p' "$unit")
case $start_limit in
  *min) start_limit=$((${start_limit%min} * 60)) ;;
  *s) start_limit=${start_limit%s} ;;
  '') fail "ghost-wispr.service sets no TimeoutStartSec: the user manager's 90 s default is shorter than its waits" ;;
esac
# The network wait's last try can run 5 s past its limit (a 3 s connect, then a 2 s sleep).
((start_limit >= network_wait + 5 + sync_wait + 30)) \
  || fail "ghost-wispr.service's TimeoutStartSec (${start_limit}s) does not cover the network wait (${network_wait}s) and the clock wait (${sync_wait}s)"

# The detached join re-authenticates the node with `tailscale up` and the tailnet's subnet routes
# off, sets Serve again, and runs the route check that provision.sh installed beside the token. It
# accepts the routes (`tailscale set --accept-routes`) only when the check passed. The check compares
# each IPv4 network of the Pi, tailscale0's excepted, with each peer's PrimaryRoutes: an accepted
# route that overlaps a network of the Pi would take the Pi's own traffic for that network.
readonly fixtures="$work_dir/fixtures"
mkdir -p "$fixtures"

# The Pi's networks, as `ip -4 -o addr` prints them: field 2 is the interface, field 4 the address
# and its prefix.
# F-ip-pi-posted: the office Pi's `ip -4 -o addr`, captured on the Pi on 2026-11-20 at 06:2xZ and
# posted on dispatch://INFRA-4587/comment/214b89f6-725d-4230-9e03-f94c8eae1187, with a neutral
# tailscale0 address.
cat >"$fixtures/F-ip-pi-posted" <<'EOF'
1: lo    inet 127.0.0.1/8 scope host lo
3: wlan0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0
4: tailscale0    inet 100.64.0.2/32 scope global tailscale0
EOF
# F-ip-pi: the same lines in the form iproute2 prints with -o for a real kernel, which always sends
# the address's lifetimes: the posted reading was cut at the `\` that comes before them (iproute2
# 6.15.0, ip/ipaddress.c:1674-1684 and ip/ip.c:302).
cat >"$fixtures/F-ip-pi" <<'EOF'
1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
3: wlan0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0\       valid_lft 85520sec preferred_lft 85520sec
4: tailscale0    inet 100.64.0.2/32 scope global tailscale0\       valid_lft forever preferred_lft forever
EOF
# F-ip-pod: iproute2 6.15.0's `ip -4 -o addr` under gVisor, captured verbatim in INFRA-4599's pod on
# 2026-11-20: no label and no lifetimes, and each line ends in one space.
printf '%s \n' '1: lo    inet 127.0.0.1/8 scope global dynamic' '2: eth0    inet 10.1.50.246/32 scope global dynamic' \
  >"$fixtures/F-ip-pod"
# F-ip-secondary: F-ip-pi with a secondary address on wlan0, in the shape ip/ipaddress.c prints it:
# the secondary flag, then the address's label.
{
  sed -n '1,2p' "$fixtures/F-ip-pi"
  printf '%s\n' '3: wlan0    inet 10.20.0.5/24 brd 10.20.0.255 scope global secondary wlan0:1\       valid_lft forever preferred_lft forever'
  sed -n '3p' "$fixtures/F-ip-pi"
} >"$fixtures/F-ip-secondary"
# F-ip-ptp: F-ip-pi with a point-to-point address, in the form ip/ipaddress.c prints when the local
# address and the peer address differ: field 4 is the local address, with no prefix.
{
  cat -- "$fixtures/F-ip-pi"
  printf '%s\n' '5: wg0    inet 10.0.0.1 peer 10.0.0.2/32 scope global wg0\       valid_lft forever preferred_lft forever'
} >"$fixtures/F-ip-ptp"
# F-ip-lo-only: only the lo and tailscale0 lines of F-ip-pi. F-ip-empty: no line.
sed -n '1p;3p' "$fixtures/F-ip-pi" >"$fixtures/F-ip-lo-only"
: >"$fixtures/F-ip-empty"
# F-ip-ptp-control: F-ip-ptp with an ESC in the interface name of its wg0 line.
{
  cat -- "$fixtures/F-ip-pi"
  printf '%s\033%s\n' '5: wg' '0    inet 10.0.0.1 peer 10.0.0.2/32 scope global wg0\       valid_lft forever preferred_lft forever'
} >"$fixtures/F-ip-ptp-control"
# F-ip-control-name: F-ip-pi with an ESC in the interface name of its wlan0 line.
{
  sed -n '1p' "$fixtures/F-ip-pi"
  printf '%s\033%s\n' '3: wl' 'an0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0\       valid_lft 85520sec preferred_lft 85520sec'
  sed -n '3p' "$fixtures/F-ip-pi"
} >"$fixtures/F-ip-control-name"

# The tailnet's statuses, as `tailscale status --json` prints them.
# F-status-router: a status in the shape of Tailscale v1.104.1's ipn/ipnstate/ipnstate.go, with
# neutral names and addresses: BackendState and Self, then Peer keyed by node key (line 81), each
# peer with its HostName and DNSName (lines 242 and 246), and PrimaryRoutes only when it routes
# (line 266, omitempty). router-a routes 10.1.0.0/16, INFRA-3709's Pod range, the route that
# INFRA-4587 adds. svc-frontdoor routes a service /32 and an IPv6 /128, the shape of the
# office Pi's reading on dispatch://INFRA-4587/comment/214b89f6-725d-4230-9e03-f94c8eae1187. devbox
# routes nothing.
cat >"$fixtures/F-status-router" <<'EOF'
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
      "HostName": "devbox",
      "DNSName": "devbox.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.5", "fd7a:115c:a1e0::5"],
      "Online": true
    }
  }
}
EOF
# F-status-overlap: F-status-router with one more peer, office-router, which routes 192.168.0.0/16.
jq '("nodekey:" + "4d" * 32) as $key | .Peer[$key] = {ID: "n104", PublicKey: $key, HostName: "office-router",
    DNSName: "office-router.example-tailnet.ts.net.", OS: "linux", TailscaleIPs: ["100.64.0.6", "fd7a:115c:a1e0::6"],
    PrimaryRoutes: ["192.168.0.0/16"], Online: true}' "$fixtures/F-status-router" >"$fixtures/F-status-overlap"
# with_routes writes, into the fixture named first, the status fixture named third (F-status-router
# by default) with router-a's PrimaryRoutes replaced by the JSON list given second.
with_routes() {
  jq --argjson routes "$2" '(.Peer[] | select(.HostName == "router-a") | .PrimaryRoutes) = $routes' \
    "$fixtures/${3:-F-status-router}" >"$fixtures/$1"
}
# The other statuses change only the routes of F-status-router, but the last five:
# F-status-two-overlaps gives router-a of F-status-overlap a route that overlaps lo;
# F-status-peer-null has no peer yet (ipnstate.go:467-469 adds the map with the first peer);
# F-status-needs-login is a node logged out; F-status-empty and F-status-not-json are no status.
with_routes F-status-host-route '["192.168.1.10/32"]'
with_routes F-status-adjacent '["192.168.2.0/24"]'
with_routes F-status-tailscale-range '["100.64.0.0/10"]'
with_routes F-status-loopback '["127.0.0.0/8"]'
with_routes F-status-default-route '["0.0.0.0/0"]'
with_routes F-status-secondary '["10.20.0.0/16"]'
jq 'del(.Peer[].PrimaryRoutes)' "$fixtures/F-status-router" >"$fixtures/F-status-no-routes"
with_routes F-status-two-overlaps '["127.0.0.0/8"]' F-status-overlap
jq '.Peer = null' "$fixtures/F-status-router" >"$fixtures/F-status-peer-null"
jq '.BackendState = "NeedsLogin"' "$fixtures/F-status-router" >"$fixtures/F-status-needs-login"
: >"$fixtures/F-status-empty"
printf 'not json\n' >"$fixtures/F-status-not-json"
# F-status-large: F-status-router with its peers replaced by 1,000 peers, each with a long HostName
# and one /32 route in Tailscale's 100.64.0.0/10. It is more than Linux's 128 KiB limit for one
# argument, so the check reads it whole only when it does not pass the status as an argument.
# F-status-large-overlap adds a peer, office-router, which routes 192.168.0.0/16.
jq '.Peer = ([range(1000) | (10000 + . | tostring | .[1:]) as $n | {
    key: ("nodekey:" + "e" * 60 + $n),
    value: {ID: ("n2" + $n), PublicKey: ("nodekey:" + "e" * 60 + $n),
      HostName: ("peer-with-a-long-host-name-in-a-large-tailnet-" + $n),
      DNSName: ("peer-with-a-long-host-name-in-a-large-tailnet-" + $n + ".example-tailnet.ts.net."),
      OS: "linux", PrimaryRoutes: ["100.\(64 + (. / 256 | floor)).\(. % 256).1/32"], Online: true}
  }] | from_entries)' "$fixtures/F-status-router" >"$fixtures/F-status-large"
jq '("nodekey:" + "f" * 64) as $key | .Peer[$key] = {ID: "n3000", PublicKey: $key, HostName: "office-router",
    PrimaryRoutes: ["192.168.0.0/16"]}' "$fixtures/F-status-large" >"$fixtures/F-status-large-overlap"
# F-status-control-host: F-status-router with router-a's HostName holding control characters, the
# terminal escape sequence ESC ]0; that sets a window's title, ended by a BEL.
jq '(.Peer[] | select(.HostName == "router-a") | .HostName) = "router-a\u001b]0;pwned\u0007"' \
  "$fixtures/F-status-router" >"$fixtures/F-status-control-host"
# F-status-control-overlap: F-status-overlap with ESC, BEL and U+202E in office-router's HostName.
jq '(.Peer[] | select(.HostName == "office-router") | .HostName) = "office-router\u001b]0;pwned\u0007\u202e"' \
  "$fixtures/F-status-overlap" >"$fixtures/F-status-control-overlap"
# F-status-control-state: F-status-router with an ESC in its BackendState.
jq '.BackendState = "Needs\u001bLogin"' "$fixtures/F-status-router" >"$fixtures/F-status-control-state"
# F-status-control-route: F-status-router with an ESC at the end of router-a's route.
with_routes F-status-control-route '["10.1.0.0/16\u001b"]'

readonly accepted_prefix='ghost-wispr routes: no route of a peer overlaps a network of the Pi: '
readonly not_accepting="ghost-wispr routes: not accepting the tailnet's subnet routes: an accepted route would take the Pi's own traffic for that network"
readonly wlan0_overlap='wlan0 192.168.1.136/24 overlaps 192.168.0.0/16 of office-router'

# manager_expansion prints the command given as the service manager runs a systemd-run command when
# --expand-environment=no did not turn its expansion off (systemd v257, replace_env_full with no
# flags): each ${NAME} becomes the value of NAME in the environment, empty when NAME is unset, and
# each $$ becomes $. A ${ with a : before its }, and a ${ with no }, stay as written.
manager_expansion() {
  local text=$1 out='' word='' state=word char i
  for ((i = 0; i < ${#text}; i++)); do
    char=${text:i:1}
    case $state in
      word)
        word+=$char
        [[ $char != '$' ]] || state=dollar
        ;;
      dollar)
        case $char in
          '{') out+=${word%?} word='${' state=variable ;;
          '$') out+=$word word='' state=word ;;
          *) word+=$char state=word ;;
        esac
        ;;
      variable)
        case $char in
          '}') out+=$(printenv -- "${word:2}" || true) word='' state=word ;;
          :) word+=$char state=word ;;
          *) word+=$char ;;
        esac
        ;;
    esac
  done
  printf '%s' "$out$word"
}

# run_join runs the join's command as its unit does, on F-ip-pi and the status fixture given second,
# with RouteAll set to the first argument beforehand. It starts a fresh log, writes a token, and puts
# the route check provision.sh installed back, each at the path the command names, because the
# command removes both. When the systemd-run line lacks --expand-environment=no, the command first
# gets the service manager's expansion. The exit status is kept in join_exit, stdout and stderr in
# $work_dir/join.out.
run_join() {
  local command options
  printf '%s\n' "$1" >"$TEST_ROUTE_ALL"
  [[ -f $TEST_LOG.installed-route-overlap-check ]] \
    || fail "provision.sh installed no route check: $TEST_LOG.installed-route-overlap-check is missing"
  cp -- "$TEST_LOG.installed-route-overlap-check" "$work_dir/join/route-overlap-check"
  printf 'id-token' >"$work_dir/join/token"
  rm -f -- "$TEST_LOG.up-exit"
  : >"$TEST_LOG"
  command=$(<"$TEST_LOG.join")
  options=$(<"$TEST_LOG.systemd-run")
  options=${options%"$command"}
  [[ " $options " == *' --expand-environment=no '* ]] || command=$(manager_expansion "$command")
  join_exit=0
  TEST_NETWORKS="$fixtures/F-ip-pi" TEST_STATUS=$2 PATH="$work_dir/bin:$PATH" \
    /bin/sh -c "$command" >"$work_dir/join.out" 2>&1 || join_exit=$?
}

# J1: no route overlaps a network of the Pi, so the join accepts the routes once the check passed.
# It removes the route check and the token.
run_join false "$fixtures/F-status-router"
j1="the join did not accept the tailnet's subnet routes although no route overlaps the Pi's networks: $(<"$work_dir/join.out")"
mapfile -t calls <"$TEST_LOG"
[[ $join_exit == 0 && ${#calls[@]} == 4 && ${calls[0]} == 'tailscale up '* \
  && ${calls[1]} == 'tailscale serve --bg --https 443 http://127.0.0.1:8080' \
  && ${calls[2]} == 'tailscale status --json' && ${calls[3]} == 'tailscale set --accept-routes' ]] || fail "$j1"
[[ $(<"$TEST_ROUTE_ALL") == true ]] || fail "$j1"
accepted=$(grep -e "^$accepted_prefix" "$work_dir/join.out") || fail "$j1"
[[ $accepted == *'wlan0 192.168.1.136/24'* && $accepted == *'10.1.0.0/16'* ]] || fail "$j1"
[[ ! -e $work_dir/join/route-overlap-check ]] || fail "$j1"
[[ ! -e $work_dir/join/token ]] || fail "the join left the token in the join directory (J1)"

# J2: office-router's route holds wlan0's network, so the join refuses: it runs no `tailscale set`,
# leaves RouteAll false, ends failed, and names the overlap.
run_join false "$fixtures/F-status-overlap"
j2="the join accepted the tailnet's subnet routes although 192.168.0.0/16 of office-router overlaps wlan0 192.168.1.136/24"
if grep -q '^tailscale set' "$TEST_LOG" || [[ $(<"$TEST_ROUTE_ALL") != false ]]; then
  fail "$j2"
fi
[[ $join_exit != 0 ]] || fail "$j2"
grep -Fq "$wlan0_overlap" "$work_dir/join.out" || fail "the join's refusal does not name the overlap: $(<"$work_dir/join.out")"
grep -Fq "$not_accepting" "$work_dir/join.out" || fail "the join's refusal does not name the overlap: $(<"$work_dir/join.out")"

# J3: a re-join of a Pi that accepts the routes, while a route now overlaps wlan0. `tailscale up`
# turns the routes off and so passes up.go's check of settings left out, and the check keeps them
# off.
run_join true "$fixtures/F-status-overlap"
up_exit='not run'
[[ ! -f $TEST_LOG.up-exit ]] || up_exit=$(<"$TEST_LOG.up-exit")
j3="a re-join with an overlapping route left RouteAll $(<"$TEST_ROUTE_ALL") (tailscale up: $up_exit)"
[[ $(<"$TEST_ROUTE_ALL") == false ]] || fail "$j3"
[[ $up_exit == 0 ]] || fail "$j3"
if grep -q '^tailscale set' "$TEST_LOG"; then
  fail "$j3"
fi
grep -Fq "$wlan0_overlap" "$work_dir/join.out" || fail "$j3"

# J4: a re-join of a Pi that accepts the routes, while no route overlaps: the check turns them on
# again.
run_join true "$fixtures/F-status-router"
[[ $(<"$TEST_ROUTE_ALL") == true ]] || fail "a re-join with no overlapping route left RouteAll false"
grep -Fxq 'tailscale set --accept-routes' "$TEST_LOG" || fail "a re-join with no overlapping route left RouteAll false"
[[ $join_exit == 0 ]] || fail "a re-join with no overlapping route left RouteAll false"

# J5: when `tailscale up` fails, the join stops there and ends failed. It still removes the token.
TEST_UP_EXIT=1 run_join false "$fixtures/F-status-router"
if [[ $join_exit == 0 ]] || grep -q -e '^tailscale serve' -e '^tailscale status --json$' -e '^tailscale set' "$TEST_LOG"; then
  fail "the join went on after tailscale up failed: $(<"$TEST_LOG")"
fi
[[ ! -e $work_dir/join/token ]] || fail "the join left the token in the join directory after tailscale up failed (J5)"

# J6: accepting the routes does not depend on Serve, so a failed `tailscale serve` still runs the
# check and accepts the routes, and the join ends failed.
TEST_SERVE_EXIT=1 run_join false "$fixtures/F-status-router"
if ! grep -Fxq 'tailscale set --accept-routes' "$TEST_LOG" || [[ $(<"$TEST_ROUTE_ALL") != true || $join_exit == 0 ]]; then
  fail "a failed tailscale serve stopped the route check, or the join reported success: $(<"$TEST_LOG")"
fi

# J7: when the check cannot read the tailnet's status, it refuses, and the join says which read failed.
TEST_STATUS_EXIT=1 run_join false "$fixtures/F-status-router"
if [[ $join_exit == 0 ]] || grep -q '^tailscale set' "$TEST_LOG" || [[ $(<"$TEST_ROUTE_ALL") != false ]] \
  || ! grep -Fq 'cannot compare' "$work_dir/join.out" || ! grep -Fq 'tailscale status' "$work_dir/join.out"; then
  fail "the join accepted routes it could not compare: $(<"$work_dir/join.out")"
fi

# J8: a failed `tailscale set` ends the join failed.
TEST_SET_EXIT=1 run_join false "$fixtures/F-status-router"
[[ $join_exit != 0 ]] || fail "the join reported success although tailscale set failed"

# route_row runs the route check provision.sh installed as the join unit runs it, /bin/sh on the
# file, on the network fixture and the status fixture given third and fourth. It fails unless the
# check exits with the code given second and its output names each text given after the fixtures.
# The first argument names the row.
route_row() {
  local row=$1 want=$2 got=0 text
  TEST_NETWORKS="$fixtures/$3" TEST_STATUS="$fixtures/$4" PATH="$work_dir/bin:$PATH" \
    /bin/sh "$TEST_LOG.installed-route-overlap-check" >"$work_dir/check.out" 2>&1 || got=$?
  [[ $got == "$want" ]] || fail "route check $row: exit $got, want $want: $(<"$work_dir/check.out")"
  shift 4
  for text in "$@"; do
    grep -Fq -- "$text" "$work_dir/check.out" || fail "route check $row does not name $text: $(<"$work_dir/check.out")"
  done
}

# clean_route_row runs route_row on its arguments, then fails when the check's output holds an ESC,
# a BEL or a U+202E: a terminal runs an escape sequence, and shows the text after a U+202E in
# another order.
clean_route_row() {
  route_row "$@"
  if grep -q -e $'\033' -e $'\007' -e $'\xe2\x80\xae' "$work_dir/check.out"; then
    fail "route check $1 prints a control or format character: $(cat -v -- "$work_dir/check.out")"
  fi
}

route_row 'G1 route holds the network' 1 F-ip-pi F-status-overlap 'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16'
route_row 'G2 network holds the route' 1 F-ip-pi F-status-host-route 'overlaps 192.168.1.10/32'
route_row 'G3 adjacent' 0 F-ip-pi F-status-adjacent "$accepted_prefix"
route_row "G4 the tree's route, a service /32, an IPv6 /128" 0 F-ip-pi F-status-router 'routes compared:' \
  '10.1.0.0/16' '100.64.10.1/32'
route_row "G5 Tailscale's range" 0 F-ip-pi F-status-tailscale-range "$accepted_prefix"
accepted=$(grep -F -- "$accepted_prefix" "$work_dir/check.out")
networks=${accepted#*"$accepted_prefix"}
networks=${networks%%'; routes compared:'*}
[[ $networks != *tailscale0* ]] \
  || fail "route check G5 Tailscale's range names tailscale0 among the Pi's networks: $(<"$work_dir/check.out")"
route_row 'G6 loopback' 1 F-ip-pi F-status-loopback 'lo 127.0.0.1/8 overlaps 127.0.0.0/8'
route_row 'G7 no peers' 0 F-ip-pi F-status-peer-null 'routes compared: none'
route_row 'G8 no route key' 0 F-ip-pi F-status-no-routes 'routes compared: none'
route_row 'G9 the posted form' 1 F-ip-pi-posted F-status-overlap 'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16'
route_row 'G10 another interface, gVisor form' 1 F-ip-pod F-status-router 'eth0 10.1.50.246/32 overlaps 10.1.0.0/16'
route_row 'G11 a secondary address' 1 F-ip-secondary F-status-secondary 'wlan0 10.20.0.5/24 overlaps 10.20.0.0/16'
route_row 'G12 two overlaps' 1 F-ip-pi F-status-two-overlaps "$wlan0_overlap" 'lo 127.0.0.1/8 overlaps 127.0.0.0/8 of router-a'
route_row 'G13 a default route' 1 F-ip-pi F-status-default-route 'overlaps 0.0.0.0/0'
large_size=$(wc -c <"$fixtures/F-status-large")
((large_size > 131072)) || fail "F-status-large is $large_size bytes, not more than 128 KiB"
route_row 'G14 a large status' 0 F-ip-pi F-status-large "$accepted_prefix"
route_row 'G14 a large status, with an overlap' 1 F-ip-pi F-status-large-overlap 'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16'
TEST_IP_EXIT=1 route_row 'G15 ip fails' 2 F-ip-pi F-status-router 'cannot compare'
route_row 'G16 ip prints nothing' 2 F-ip-empty F-status-router 'cannot compare'
route_row 'G17 only lo and tailscale0' 2 F-ip-lo-only F-status-router 'cannot compare'
route_row 'G18 point-to-point line' 2 F-ip-ptp F-status-router 'cannot compare' 'wg0' '10.0.0.1 peer 10.0.0.2/32'
TEST_STATUS_EXIT=1 route_row 'G19 status fails' 2 F-ip-pi F-status-router 'cannot compare'
route_row 'G20 status prints nothing' 2 F-ip-pi F-status-empty 'cannot compare'
route_row 'G21 status is not JSON' 2 F-ip-pi F-status-not-json 'cannot compare'
route_row 'G22 Tailscale not running' 2 F-ip-pi F-status-needs-login 'cannot compare' 'NeedsLogin'
clean_route_row 'G23 control characters in a host name' 0 F-ip-pi F-status-control-host 'router-a?]0;pwned?'
clean_route_row 'G24 control and format characters in a refusal' 1 F-ip-pi F-status-control-overlap \
  'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16 of office-router?]0;pwned??'
clean_route_row 'G25 control characters in an ip line with no prefix' 2 F-ip-ptp-control F-status-router 'cannot compare' 'wg?0'
clean_route_row 'G26 control characters in BackendState' 2 F-ip-pi F-status-control-state 'Needs?Login'
clean_route_row 'G27 control characters in a route that is no prefix' 2 F-ip-pi F-status-control-route 'cannot compare' \
  '10.1.0.0/16?'
clean_route_row 'G28 control characters in an interface name' 1 F-ip-control-name F-status-overlap \
  'wl?an0 192.168.1.136/24 overlaps'

# J0: provision.sh on a Pi already joined as tag:ghost-wispr at $PI_HOST. exits before the join, so
# it installs no route check and leaves the accept-routes setting as it is.
pi_host=$(source "$repo_root/deploy/pi/test-pi.env" && printf '%s' "$PI_HOST")
: >"$TEST_LOG"
joined_exit=0
TEST_SELF_STATUS=$(jq -nc --arg name "$pi_host." '{Self: {Tags: ["tag:ghost-wispr"], DNSName: $name}}') \
  run_provision "${bundle_files[@]}" || joined_exit=$?
if [[ $joined_exit != 0 ]] || ! grep -Fq 'already joined' "$work_dir/out" \
  || grep -Eq 'systemd-run|route-overlap-check|tailscale set' "$TEST_LOG"; then
  cat -- "$work_dir/out" >&2
  fail "provision.sh on a Pi already joined started a join or a route check: $(<"$TEST_LOG")"
fi

# A rerun on a Pi that has a shared drive renders through render_models_config: it must keep the
# drive and the template's Envoy hub, and say no bash error either.
printf 'gdrive_folder_id: "drive-0123"\n' >"$work_dir/deploy/ghost-wispr.yaml"
: >"$TEST_LOG"
run_provision "${bundle_files[@]}" || fail "provision.sh failed on a rerun: $(<"$work_dir/out")"
if grep -E 'provision\.sh: line [0-9]+: ' "$work_dir/out" >&2; then
  fail "provision.sh printed a bash error on a rerun with a shared drive (above)"
fi
hub_line=$(grep '^envoy_hub_url: ' "$repo_root/deploy/pi/files/ghost-wispr.yaml")
for line in 'gdrive_folder_id: "drive-0123"' 'gdrive_sync_enabled: true' "$hub_line"; do
  grep -Fxq "$line" "$TEST_LOG.installed-ghost-wispr.yaml" \
    || fail "the rerun's configuration lacks '$line': $(<"$TEST_LOG.installed-ghost-wispr.yaml")"
done
assert_recording_rules "rerun's"
assert_office_pi_devices "rerun's"
# The recorder reads its configuration only when it starts, and update.sh restarts nothing when the
# Pi already runs the newest release: a rerun restarts a running recorder once the configuration is
# installed, and try-restart starts none that is stopped.
# grep finding no line is a failure this check reports, not one that stops the test silently.
config_at=$(grep -n -E '^sudo install .*/ghost-wispr\.yaml$' "$TEST_LOG" | cut -d: -f1 | head -n 1) || true
restart_at=$(grep -n -F -x -- "sudo -u ghost-wispr XDG_RUNTIME_DIR=$runtime_dir systemctl --user try-restart ghost-wispr.service" \
  "$TEST_LOG" | cut -d: -f1 | head -n 1) || true
[[ -n $config_at && -n $restart_at && $restart_at -gt $config_at ]] \
  || fail "the rerun did not restart a running recorder after it installed the configuration: $(<"$TEST_LOG")"

# provision.sh --models, run by hand, refuses before it writes anything while a line of the deployed
# .env could set a variable that would turn Envoy on over the template, which keeps it off while the
# hub moves (dispatch://INFRA-3862): a line that, past leading space and an export, starts with the
# name and =. It names the variable, prints no value, and leaves every file byte for byte as it was.
# A match inside a multi-line quoted value refuses too: a false refusal only stops the step. systemd's
# EnvironmentFile= parser ends a line at a carriage return as at a line feed (NEWLINE "\n\r",
# src/basic/env-file.c), so a bare CR starts a line too; a case whose text holds CR LF is a CRLF file.
models_dir=$work_dir/models-deploy
mkdir -p "$models_dir"
run_models() {
  : >"$TEST_LOG"
  env -u XDG_RUNTIME_DIR PATH="$work_dir/bin:$PATH" GHOST_WISPR_DEPLOY_DIR="$models_dir" \
    bash "$repo_root/deploy/pi/provision.sh" --models 0AAbCdEf_Gh-Ij >"$work_dir/out" 2>&1
}
models_files() { sha256sum "$models_dir/ghost-wispr.yaml" "$models_dir/.env"; }
for case in \
  'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_ENABLED=true' \
  'GHOST_WISPR_CONCIERGE_ENVOY_URL|  export GHOST_WISPR_CONCIERGE_ENVOY_URL = http://hub.example.test:9020' \
  $'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_DISPATCH_TOKEN="first line\nGHOST_WISPR_ENVOY_ENABLED=true\nlast line"' \
  $'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ADDR=127.0.0.1:8080\rGHOST_WISPR_ENVOY_ENABLED=true' \
  $'GHOST_WISPR_CONCIERGE_ENVOY_URL|GHOST_WISPR_ADDR=127.0.0.1:8080\r\nGHOST_WISPR_CONCIERGE_ENVOY_URL=http://hub.example.test:9020\r'; do
  name=${case%%|*} content=${case#*|} eol=''
  [[ $content != *$'\r\n'* ]] || eol=$'\r'
  printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
  printf "%s$eol\n%s\n" GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 "$content" >"$models_dir/.env"
  before=$(models_files)
  if run_models; then
    fail "--models installed with $name in .env: $(<"$work_dir/out")"
  fi
  grep -qF "can set $name" "$work_dir/out" || fail "--models did not name $name: $(<"$work_dir/out")"
  ! grep -qE 'hub\.example\.test|first line|dg-1' "$work_dir/out" || fail "--models printed a value from .env: $(<"$work_dir/out")"
  [[ ! -s $TEST_LOG ]] || fail "--models changed the Pi before it refused $name: $(<"$TEST_LOG")"
  [[ $(models_files) == "$before" ]] || fail "--models changed a file when it refused $name"
done
# The control: a .env that names neither installs, with the hub's URL, a commented-out override and
# a longer name that starts with one.
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 'GHOST_WISPR_ENVOY_HUB_URL=http://hub.example.test:9020' \
  '# GHOST_WISPR_ENVOY_ENABLED=true' 'GHOST_WISPR_ENVOY_ENABLED_BEFORE=true' >"$models_dir/.env"
run_models || fail "--models refused a .env that sets no Envoy override: $(<"$work_dir/out")"
grep -qE '^sudo install .*/ghost-wispr\.yaml$' "$TEST_LOG" || fail "--models did not install the config: $(<"$TEST_LOG")"
printf 'provision run test passed\n'
