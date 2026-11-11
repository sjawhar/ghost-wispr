#!/usr/bin/env bash
# Exercise the slice 1 acceptance checks from an operator machine on the tailnet.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
readonly baseline_file="$script_dir/baseline.txt"
host=${GHOST_WISPR_HOST:-ghost-wispr.tail12ec1c.ts.net}
relay_url=${GHOST_WISPR_RELAY_URL:-}

usage() {
  cat <<'EOF'
Usage: deploy/pi/accept-slice1.sh [--host HOST] [--relay-url URL]

GHOST_WISPR_ENVOY_WEBHOOK_SECRET is optional. When present, the driver sends
an authenticated Envoy probe before it checks that an unsigned Pi request is refused.
EOF
}

fail() {
  printf 'slice 1 acceptance: %s\n' "$*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      [[ $# -ge 2 ]] || fail "--host needs a value"
      host=$2
      shift 2
      ;;
    --relay-url)
      [[ $# -ge 2 ]] || fail "--relay-url needs a value"
      relay_url=$2
      shift 2
      ;;
    --help)
      usage
      exit 0
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

for command in curl jq openssl ssh; do
  command -v "$command" >/dev/null || fail "missing required command: $command"
done
[[ -f "$baseline_file" ]] || fail "missing baseline file: $baseline_file"

readonly remote="ghost-wispr@$host"
readonly unit_url="https://$host"
scratch_dir=$(mktemp -d)
trap 'rm -rf -- "$scratch_dir"' EXIT

remote_run() {
  ssh -o BatchMode=yes "$remote" "$@"
}

status() {
  curl --fail --silent --show-error "$unit_url/api/status"
}

assert_capturing() {
  local current_status
  current_status=$(status)
  jq -e '.capture_state == "capturing" and .paused == false' >/dev/null <<<"$current_status" \
    || fail "expected capture_state=capturing and paused=false"
}

assert_paused() {
  local current_status
  current_status=$(status)
  jq -e '.paused == true' >/dev/null <<<"$current_status" || fail "expected paused=true"
}

wait_for_recorder() {
  for _ in {1..60}; do
    if status >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done
  fail "the recorder did not return within two minutes"
}

check_sshd_loopback() {
  local listeners listener
  listeners=$(remote_run 'sudo ss -ltnpH | grep "sshd" || true')
  [[ -n "$listeners" ]] || fail "sshd has no observable listener; check ssh.socket and ssh.service"
  while IFS= read -r listener; do
    [[ "$listener" =~ 127\.0\.0\.1:22([[:space:]]|$) ]] \
      || fail "sshd listens outside 127.0.0.1: $listener"
  done <<<"$listeners"
}

printf 'A4: check the tagged tailnet identity and automatic service.\n'
self_status=$(remote_run 'tailscale status --self --json')
jq -e --arg host "${host}." '
  .Self.Tags == ["tag:ghost-wispr"] and .Self.DNSName == $host
' >/dev/null <<<"$self_status" || fail "the Pi is not tag:ghost-wispr at the expected hostname"
remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user is-active --quiet ghost-wispr.service" \
  || fail "ghost-wispr.service is not active"
assert_capturing
printf '  pass\n'

printf 'A1: check the red light and pause button.\n'
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == red ]] || fail "light is not red while capturing"
audio_before=$(remote_run 'du -sb /opt/ghost-wispr/data/audio 2>/dev/null | cut -f1 || printf 0')
printf 'Tap the Pi power button once to pause, then press Enter. '
read -r _
assert_paused
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == off ]] || fail "light stays on after pause"
printf 'Observe ss -tin on the Pi now; it must show no audio flow beyond Deepgram keep-alives for 60 seconds.\n'
remote_run 'ss -tin'
sleep 60
audio_after=$(remote_run 'du -sb /opt/ghost-wispr/data/audio 2>/dev/null | cut -f1 || printf 0')
[[ "$audio_before" == "$audio_after" ]] || fail "audio grew while paused ($audio_before to $audio_after bytes)"
printf 'Tap the Pi power button once to resume, then press Enter. '
read -r _
assert_capturing
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == red ]] || fail "light is not red after resume"
printf '  pass\n'

printf 'A3: pause persists through a power cycle.\n'
printf 'Tap once to pause, pull power, reconnect it, then press Enter. '
read -r _
wait_for_recorder
assert_paused
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == off ]] || fail "light is not off after paused reboot"
printf 'Tap once to resume, then press Enter. '
read -r _
assert_capturing
printf '  pass\n'

printf 'A5: confirm the service only listens on loopback and Tailscale serves HTTPS.\n'
remote_run 'sudo ss -ltnpH' | tee "$scratch_dir/listeners.txt"
app_listeners=$(awk '$4 ~ /:8080$/ && /users:\(\("ghost-wispr"/ { print $4 }' "$scratch_dir/listeners.txt")
[[ -n "$app_listeners" ]] || fail "Ghost Wispr has no observable listener on port 8080"
while IFS= read -r app_listener; do
  [[ "$app_listener" == 127.0.0.1:8080 ]] \
    || fail "Ghost Wispr listens on a non-loopback address: $app_listener"
done <<<"$app_listeners"
status >/dev/null
check_sshd_loopback
printf '  pass from this tailnet member. Test a phone on mobile data and a tag:envoy-host separately: both must fail to connect.\n'

if [[ -n "$relay_url" ]]; then
  printf 'Envoy: check a signed probe and an unsigned Pi request.\n'
  [[ "$relay_url" =~ ^https://[A-Za-z0-9.-]+/webhook/ghostwispr$ ]] || fail "invalid relay URL"
  if [[ -n ${GHOST_WISPR_ENVOY_WEBHOOK_SECRET:-} ]]; then
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    delivery="acceptance-probe.session_ended.$(date +%s)"
    body="{\"event_type\":\"session_ended\",\"created_at\":\"$now\",\"payload\":{\"session_id\":\"acceptance-probe\",\"type\":\"session_ended\",\"title\":\"Acceptance probe\",\"summary\":\"\",\"duration\":0}}"
    signature=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$GHOST_WISPR_ENVOY_WEBHOOK_SECRET" -hex | cut -d' ' -f2)
    signed_code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
      --request POST "$relay_url" \
      --header 'Content-Type: application/json' \
      --header 'X-GhostWispr-Event: session.ended' \
      --header "X-GhostWispr-Delivery: $delivery" \
      --header "X-GhostWispr-Signature: sha256=$signature" \
      --data "$body")
    [[ "$signed_code" =~ ^2 ]] || fail "signed Envoy probe returned HTTP $signed_code"
    printf '  signed probe accepted; verify its notification with nats sub '\''notifications.ghostwispr.>'\''.\n'
  else
    printf '  skipped signed probe: set GHOST_WISPR_ENVOY_WEBHOOK_SECRET through secrets to run it.\n'
  fi
  unsigned_code=$(remote_run "curl --silent --output /dev/null --write-out '%{http_code}' --request POST '$relay_url' --header 'X-GhostWispr-Delivery: unsigned-probe' --header 'X-GhostWispr-Event: session.ended' --data '{}'")
  [[ "$unsigned_code" == 401 ]] || fail "unsigned Pi request returned HTTP $unsigned_code, expected 401"
  printf '  pass\n'
else
  printf 'Envoy: skipped; rerun with --relay-url from envoy-cluster Pulumi output.\n'
fi

printf 'A2: open %s and choose Recording rules. The four rules must appear verbatim.\n' "$unit_url"
printf 'A7: compare the package inventory below with deploy/pi/baseline.txt; only libportaudio2, ffmpeg, lame, jq, and their dependencies may be new.\n'
# shellcheck disable=SC2016 # ${Package} expands in the remote shell.
remote_run 'dpkg-query -W -f="${Package}\n" | LC_ALL=C sort' >"$scratch_dir/current-packages.txt"
sed '/^---$/,$d' "$baseline_file" | LC_ALL=C sort >"$scratch_dir/baseline-packages.txt"
comm -13 "$scratch_dir/baseline-packages.txt" "$scratch_dir/current-packages.txt"
printf 'A7: running services (only Ghost Wispr units are new):\n'
remote_run "systemctl list-units --type=service --state=running; XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user list-units --type=service"
printf 'slice 1 acceptance checks completed.\n'
