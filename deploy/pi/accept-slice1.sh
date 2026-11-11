#!/usr/bin/env bash
# Exercise the slice 1 acceptance checks from an operator machine on the tailnet.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
readonly baseline_file="$script_dir/baseline.txt"
host=${GHOST_WISPR_HOST:-ghost-wispr.tail12ec1c.ts.net}
relay_url=${GHOST_WISPR_RELAY_URL:-}
self_test=false

usage() {
  cat <<'EOF'
Usage: deploy/pi/accept-slice1.sh [--host HOST] [--relay-url URL] [--self-test]

GHOST_WISPR_ENVOY_WEBHOOK_SECRET is optional. When present, the driver sends
an authenticated Envoy probe before it checks that an unsigned Pi request is refused.
EOF
}

fail() {
  printf 'slice 1 acceptance: %s\n' "$*" >&2
  exit 1
}

unknown() {
  printf 'slice 1 acceptance: UNKNOWN: %s\n' "$*" >&2
  exit 2
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
    --self-test)
      self_test=true
      shift
      ;;
    --help)
      usage
      exit 0
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

for command in curl jq ssh; do
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

wait_for_light() {
  local expected=$1 attempts=$2
  for _ in $(seq 1 "$attempts"); do
    if [[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == "$expected" ]]; then
      return
    fi
    sleep 0.5
  done
  return 1
}

boot_id() {
  remote_run 'cat /proc/sys/kernel/random/boot_id'
}

assert_new_boot() {
  local before=$1 after
  after=$(boot_id)
  [[ "$after" != "$before" ]] || fail "boot ID did not change after the power cycle"
}

assert_listener_boundary() {
  local tailscale_ips listeners listener local_address allowed ip
  tailscale_ips=$(remote_run 'tailscale ip -4; tailscale ip -6') || fail "could not read Tailscale addresses"
  listeners=$(remote_run 'sudo ss -ltnH; sudo ss -lunH') || fail "could not read listening sockets"
  [[ -n "$listeners" ]] || unknown "listener inventory is empty"
  while IFS= read -r listener; do
    local_address=$(awk '{print $4}' <<<"$listener")
    [[ -n "$local_address" ]] || unknown "listener address is missing"
    allowed=false
    case "$local_address" in
      127.0.0.1:*|'[::1]:'*) allowed=true ;;
    esac
    while IFS= read -r ip; do
      [[ -n "$ip" ]] || continue
      if [[ "$local_address" == "$ip":* || "$local_address" == "$ip"%*:* ]]; then
        allowed=true
      fi
    done <<<"$tailscale_ips"
    [[ "$allowed" == true ]] || fail "listener is outside loopback and Tailscale: $local_address"
  done <<<"$listeners"
}

hmac_signature() {
  local body=$1 helper="$scratch_dir/ghost-wispr-hmac.go"
  cat >"$helper" <<'EOF'
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

func main() {
	secret, err := io.ReadAll(os.Stdin)
	if err != nil {
		panic(err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(os.Args[1]))
	fmt.Printf("%x", mac.Sum(nil))
}
EOF
  printf '%s' "$GHOST_WISPR_ENVOY_WEBHOOK_SECRET" | go run "$helper" "$body"
}

assert_set_subset() {
  local subject=$1 baseline=$2 current=$3 allowed=$4 unexpected
  [[ -s "$baseline" ]] || unknown "$subject baseline is empty"
  [[ -s "$current" ]] || unknown "$subject inventory is empty"
  unexpected="$scratch_dir/${subject// /-}-unexpected.txt"
  LC_ALL=C comm -13 "$allowed" "$current" >"$unexpected"
  if [[ -s "$unexpected" ]]; then
    printf 'unexpected %s:\n' "$subject" >&2
    cat "$unexpected" >&2
    return 1
  fi
}

dependency_packages_from_apt_cache() {
  sed -nE '/^[^[:space:]<]/p' | sed -E 's/:[[:alnum:]]+$//' | LC_ALL=C sort -u
}

deepgram_bytes_from_ss() {
  awk '
    /^[A-Z][A-Z0-9-]*[[:space:]]/ { held_by_ghost_wispr = /users:\(\("ghost-wispr"/ }
    held_by_ghost_wispr && match($0, /bytes_sent:[0-9]+/) {
      total += substr($0, RSTART + 11, RLENGTH - 11)
      found = 1
    }
    END {
      if (!found) exit 1
      print total
    }
  '
}

deepgram_bytes_sent() {
  local socket_state bytes
  socket_state=$(remote_run 'ss -tinpH dst api.deepgram.com') \
    || fail "could not read the Deepgram socket"
  if ! bytes=$(printf '%s\n' "$socket_state" | deepgram_bytes_from_ss); then
    fail "no Deepgram socket held by ghost-wispr with bytes_sent was found"
  fi
  [[ "$bytes" =~ ^[0-9]+$ ]] || fail "Deepgram bytes_sent is not numeric"
  printf '%s\n' "$bytes"
}

assert_minimum_growth() {
  local start=$1 end=$2 seconds=$3
  local minimum=$((32000 * seconds))
  (( end >= start && end - start >= minimum ))
}

assert_maximum_growth() {
  local start=$1 end=$2 maximum=$3
  (( end >= start && end - start <= maximum ))
}

run_control_tests() {
  local controls_dir
  controls_dir=$(mktemp -d)
  trap 'rm -rf -- "$controls_dir"' RETURN

  printf 'base-package\n' >"$controls_dir/baseline-packages.txt"
  printf 'base-package\nunexpected-package\n' >"$controls_dir/current-packages.txt"
  printf 'base-package\nexpected-package\n' >"$controls_dir/allowed-packages.txt"
  if assert_set_subset package "$controls_dir/baseline-packages.txt" "$controls_dir/current-packages.txt" "$controls_dir/allowed-packages.txt"; then
    fail "self-test expected the package control to reject a doctored baseline"
  fi
  printf 'self-test: package control rejected a doctored baseline\n'

  dependency_packages=$(printf '%s\n' 'libportaudio2' ' |Depends: libjack-jackd2-0' 'libjack-jackd2-0:arm64' | dependency_packages_from_apt_cache)
  [[ "$dependency_packages" == $'libjack-jackd2-0\nlibportaudio2' ]] \
    || fail "self-test expected the dependency parser to keep first-choice packages"
  printf 'self-test: dependency parser retained first-choice packages\n'

  printf 'base.service\n' >"$controls_dir/baseline-services.txt"
  printf 'base.service\nunexpected.service\n' >"$controls_dir/current-services.txt"
  printf 'base.service\nghost-wispr.service\n' >"$controls_dir/allowed-services.txt"
  if assert_set_subset service "$controls_dir/baseline-services.txt" "$controls_dir/current-services.txt" "$controls_dir/allowed-services.txt"; then
    fail "self-test expected the service control to reject an unexpected service"
  fi
  printf 'self-test: service control rejected an unexpected service\n'

  if printf '%s\n' 'ESTAB 0 0 10.0.0.2:12345 34.120.1.2:443 users:(("ghost-wispr",pid=7,fd=9))' | deepgram_bytes_from_ss; then
    fail "self-test expected the Deepgram socket control to reject fake ss output without bytes_sent"
  fi
  printf 'self-test: Deepgram socket control rejected fake ss output\n'

  parsed_bytes=$(printf '%s\n' 'ESTAB 0 0 10.0.0.2:12345 34.120.1.2:443 users:(("ghost-wispr",pid=7,fd=9))' ' bbr bytes_sent:160000' | deepgram_bytes_from_ss)
  [[ "$parsed_bytes" == 160000 ]] || fail "self-test expected the Deepgram parser to accept a ghost-wispr socket"
  printf 'self-test: Deepgram socket control accepted valid fake ss output\n'

  if assert_minimum_growth 100 101 5; then
    fail "self-test expected the Deepgram recording control to reject low growth"
  fi
  printf 'self-test: Deepgram recording control rejected low growth\n'
}

if [[ "$self_test" == true ]]; then
  run_control_tests
  exit 0
fi

printf 'A4 RAN: check the tagged tailnet identity and automatic service.\n'
self_status=$(remote_run 'tailscale status --self --json')
jq -e --arg host "${host}." '
  .Self.Tags == ["tag:ghost-wispr"] and .Self.DNSName == $host
' >/dev/null <<<"$self_status" || fail "A4 FAILED: the Pi is not tag:ghost-wispr at the expected hostname"
remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user is-active --quiet ghost-wispr.service" \
  || fail "A4 FAILED: ghost-wispr.service is not active"
assert_capturing
a4_boot_before=$(boot_id)
printf 'A4 NEEDS-PERSON: leave the recorder capturing, pull power, reconnect it, then press Enter. '
read -r _
wait_for_recorder
assert_new_boot "$a4_boot_before"
assert_capturing
printf 'A4 RAN: a new boot ID came back capturing.\n'
printf 'Light power-cycle NEEDS-PERSON: confirm the light stayed dark until Ghost Wispr began capturing.\n'

printf 'A1 RAN: check the red light, Deepgram control, and pause button.\n'
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == red ]] || fail "A1 FAILED: light is not red while capturing"
recording_bytes_before=$(deepgram_bytes_sent)
readonly deepgram_control_seconds=5
sleep "$deepgram_control_seconds"
recording_bytes_after=$(deepgram_bytes_sent)
assert_minimum_growth "$recording_bytes_before" "$recording_bytes_after" "$deepgram_control_seconds" \
  || fail "A1 FAILED: Deepgram recording control grew by less than 32 kB/s"
audio_before=$(remote_run 'du -sb /opt/ghost-wispr/data/audio 2>/dev/null | cut -f1 || printf 0')
printf 'A1 NEEDS-PERSON: tap the Pi power button once to pause, then press Enter. '
read -r _
assert_paused
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == off ]] || fail "A1 FAILED: light stays on after pause"
paused_bytes_before=$(deepgram_bytes_sent)
sleep 60
paused_bytes_after=$(deepgram_bytes_sent)
readonly deepgram_paused_max_bytes=4096
assert_maximum_growth "$paused_bytes_before" "$paused_bytes_after" "$deepgram_paused_max_bytes" \
  || fail "A1 FAILED: Deepgram bytes_sent grew by more than ${deepgram_paused_max_bytes} bytes while paused"
audio_after=$(remote_run 'du -sb /opt/ghost-wispr/data/audio 2>/dev/null | cut -f1 || printf 0')
[[ "$audio_before" == "$audio_after" ]] || fail "A1 FAILED: audio grew while paused ($audio_before to $audio_after bytes)"
printf 'A1 NEEDS-PERSON: tap the Pi power button once to resume, then press Enter. '
read -r _
assert_capturing
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == red ]] || fail "A1 FAILED: light is not red after resume"
printf 'A1 RAN: pause stopped audio and Deepgram growth, then resume restored capture.\n'

printf 'Light hang RAN: stop the recorder process and require darkness within 3 seconds.\n'
main_pid=$(remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user show --property=MainPID --value ghost-wispr.service")
[[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || fail "Light hang FAILED: no Ghost Wispr main PID"
remote_run "kill -STOP $main_pid"
if ! wait_for_light off 6; then
  remote_run "kill -CONT $main_pid"
  fail "Light hang FAILED: light did not turn off within 3 seconds"
fi
remote_run "kill -CONT $main_pid"
wait_for_light red 4 || fail "Light hang FAILED: light did not turn red after continue"
printf 'Light hang RAN: dark within 3 seconds and red after continue.\n'

printf 'Light correction RAN: force the light off and require correction while capturing.\n'
remote_run '/opt/ghost-wispr/ghost-wispr light-off'
wait_for_light red 4 || fail "Light correction FAILED: recorder did not restore red within 2 seconds"
printf 'Light correction RAN: recorder restored red within 2 seconds.\n'

printf 'A3 RAN: pause persists through a power cycle.\n'
a3_boot_before=$(boot_id)
printf 'A3 NEEDS-PERSON: tap once to pause, pull power, reconnect it, then press Enter. '
read -r _
wait_for_recorder
assert_new_boot "$a3_boot_before"
assert_paused
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == off ]] || fail "A3 FAILED: light is not off after paused reboot"
printf 'A3 NEEDS-PERSON: tap once to resume, then press Enter. '
read -r _
assert_capturing
printf 'A3 RAN: a new boot ID retained pause and resumed only after a tap.\n'

printf 'A5 RAN: confirm the service only listens on loopback or Tailscale.\n'
remote_run 'sudo ss -ltnpH' >"$scratch_dir/tcp-listeners-with-process.txt"
app_listeners=$(awk '$4 ~ /:8080$/ && /users:\(\("ghost-wispr"/ { print $4 }' "$scratch_dir/tcp-listeners-with-process.txt")
[[ -n "$app_listeners" ]] || fail "A5 FAILED: Ghost Wispr has no observable listener on port 8080"
while IFS= read -r app_listener; do
  [[ "$app_listener" == 127.0.0.1:8080 ]] \
    || fail "A5 FAILED: Ghost Wispr listens on a non-loopback address: $app_listener"
done <<<"$app_listeners"
assert_listener_boundary
status >/dev/null
check_sshd_loopback
printf 'A5 RAN: local listener boundary passed. A5 NEEDS-PERSON: test a phone off-tailnet and tag:envoy-host; both must fail.\n'

if [[ -n "$relay_url" ]]; then
  printf 'Envoy RAN: check a signed probe and an unsigned Pi request.\n'
  [[ "$relay_url" =~ ^https://[A-Za-z0-9.-]+/webhook/ghostwispr$ ]] || fail "Envoy FAILED: invalid relay URL"
  if [[ -n ${GHOST_WISPR_ENVOY_WEBHOOK_SECRET:-} ]]; then
    command -v go >/dev/null || fail "Envoy FAILED: Go is required to sign the probe without exposing its secret"
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    delivery="acceptance-probe.session_ended.$(date +%s)"
    body="{\"event_type\":\"session_ended\",\"created_at\":\"$now\",\"payload\":{\"session_id\":\"acceptance-probe\",\"type\":\"session_ended\",\"title\":\"Acceptance probe\",\"summary\":\"\",\"duration\":0}}"
    signature=$(hmac_signature "$body")
    signed_code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
      --request POST "$relay_url" \
      --header 'Content-Type: application/json' \
      --header 'X-GhostWispr-Event: session.ended' \
      --header "X-GhostWispr-Delivery: $delivery" \
      --header "X-GhostWispr-Signature: sha256=$signature" \
      --data "$body")
    [[ "$signed_code" =~ ^2 ]] || fail "Envoy FAILED: signed probe returned HTTP $signed_code"
    printf 'Envoy RAN: signed probe accepted; verify its notification with nats sub '\''notifications.ghostwispr.>'\''.\n'
  else
    printf 'Envoy NEEDS-PERSON: set GHOST_WISPR_ENVOY_WEBHOOK_SECRET through secrets to run the signed probe.\n'
  fi
  unsigned_code=$(remote_run "curl --silent --output /dev/null --write-out '%{http_code}' --request POST '$relay_url' --header 'X-GhostWispr-Delivery: unsigned-probe' --header 'X-GhostWispr-Event: session.ended' --data '{}'")
  [[ "$unsigned_code" == 401 ]] || fail "Envoy FAILED: unsigned Pi request returned HTTP $unsigned_code, expected 401"
  printf 'Envoy RAN: unsigned Pi request was refused.\n'
else
  printf 'Envoy NEEDS-PERSON: rerun with --relay-url from envoy-cluster Pulumi output.\n'
fi

printf 'A2 NEEDS-PERSON: open %s and choose Recording rules; the four rules must appear verbatim.\n' "$unit_url"
printf 'A7 RAN: compare the Pi inventory with deploy/pi/baseline.txt.\n'
readonly expected_package_roots=(libportaudio2 ffmpeg lame jq)
{
  printf '%s\n' "${expected_package_roots[@]}"
  remote_run 'apt-cache depends --recurse --no-recommends --no-suggests --no-conflicts --no-breaks --no-replaces libportaudio2 ffmpeg lame jq'
} | dependency_packages_from_apt_cache >"$scratch_dir/allowed-packages.txt"
remote_run "dpkg-query -W -f='\${Package}\\n' | LC_ALL=C sort -u" >"$scratch_dir/current-packages.txt"
awk '/^---$/{exit} NF {print}' "$baseline_file" | LC_ALL=C sort -u >"$scratch_dir/baseline-packages.txt"
{
  cat "$scratch_dir/baseline-packages.txt"
  cat "$scratch_dir/allowed-packages.txt"
} | LC_ALL=C sort -u >"$scratch_dir/permitted-packages.txt"
assert_set_subset package "$scratch_dir/baseline-packages.txt" "$scratch_dir/current-packages.txt" "$scratch_dir/permitted-packages.txt" \
  || fail "A7 FAILED: unexpected installed package"

readonly expected_service_additions=(ghost-wispr.service ghost-wispr-update.service)
system_services=$(remote_run "systemctl list-units --type=service --state=running --plain --no-legend | awk '{print \$1}'") \
  || fail "A7 FAILED: could not read running system services"
user_services=$(remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user list-units --type=service --state=running --plain --no-legend | awk '{print \$1}'") \
  || fail "A7 FAILED: could not read running user services"
[[ -n "$system_services" ]] || unknown "system service inventory is empty"
[[ -n "$user_services" ]] || unknown "user service inventory is empty"
printf '%s\n%s\n' "$system_services" "$user_services" | LC_ALL=C sort -u >"$scratch_dir/current-services.txt"
awk 'seen && NF {print} /^---$/ {seen=1}' "$baseline_file" | LC_ALL=C sort -u >"$scratch_dir/baseline-services.txt"
{
  cat "$scratch_dir/baseline-services.txt"
  printf '%s\n' "${expected_service_additions[@]}"
} | LC_ALL=C sort -u >"$scratch_dir/permitted-services.txt"
assert_set_subset service "$scratch_dir/baseline-services.txt" "$scratch_dir/current-services.txt" "$scratch_dir/permitted-services.txt" \
  || fail "A7 FAILED: unexpected running service"
timer_state=$(remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user is-enabled ghost-wispr-update.timer" || true)
[[ "$timer_state" == enabled ]] || fail "A7 FAILED: ghost-wispr-update.timer is not enabled"
printf 'A7 RAN: package and service inventories match the allowed sets.\n'
printf 'Slice 1 driver RAN automated rows; record every NEEDS-PERSON observation before acceptance.\n'
