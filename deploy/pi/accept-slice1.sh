#!/usr/bin/env bash
# Exercise the slice 1 acceptance checks from an operator machine on the tailnet.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
readonly baseline_file="$script_dir/baseline.txt"
# The office Pi's deployment settings (PI_HOST, PI_DENIED_TAG); GHOST_WISPR_PI_ENV names another
# file, as the tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || { printf 'cannot read the Pi deployment settings in %s\n' "$pi_env" >&2; exit 1; }
host=${GHOST_WISPR_HOST:-$PI_HOST}
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
stopped_pid=

resume_stopped_process() {
  if [[ -n ${stopped_pid:-} ]]; then
    remote_run "kill -CONT $stopped_pid" >/dev/null 2>&1 || true
    stopped_pid=
  fi
}

cleanup() {
  local status=$?
  trap - EXIT ERR
  resume_stopped_process
  rm -rf -- "$scratch_dir"
  exit "$status"
}

trap cleanup EXIT
trap cleanup ERR

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

monotonic_milliseconds() {
  awk '{printf "%.0f\n", $1 * 1000}' /proc/uptime
}

wait_for_light() {
  local expected=$1 maximum_ms=$2 started now light_state
  started=$(monotonic_milliseconds)
  while :; do
    light_state=$(remote_run '/opt/ghost-wispr/ghost-wispr light-state')
    case "$light_state" in
      red|off) ;;
      *) fail "light reported an unexpected colour: $light_state" ;;
    esac
    [[ "$light_state" == "$expected" ]] && return
    now=$(monotonic_milliseconds)
    (( now - started < maximum_ms )) || return 1
    sleep 0.1
  done
}

assert_dark_until_capturing() {
  local current_status capture_state light_state
  for _ in $(seq 1 240); do
    if current_status=$(status 2>/dev/null); then
      capture_state=$(jq -r '.capture_state // empty' <<<"$current_status")
      light_state=$(remote_run '/opt/ghost-wispr/ghost-wispr light-state')
      case "$light_state" in
        red|off) ;;
        *) fail "Light power-cycle FAILED: unexpected colour $light_state" ;;
      esac
      if [[ "$capture_state" == capturing ]]; then
        [[ "$light_state" == red ]] || fail "Light power-cycle FAILED: light is not red while capturing"
        return
      fi
      [[ "$light_state" == off ]] || fail "Light power-cycle FAILED: light was $light_state before capture began"
    fi
    sleep 0.5
  done
  fail "Light power-cycle FAILED: recorder did not reach capturing within two minutes"
}

boot_id() {
  remote_run 'cat /proc/sys/kernel/random/boot_id'
}

boot_id_changed() {
  [[ $1 != "$2" ]]
}

wait_for_new_boot() {
  local before=$1 after
  for _ in $(seq 1 240); do
    if after=$(boot_id 2>/dev/null) && boot_id_changed "$before" "$after"; then
      return
    fi
    sleep 0.5
  done
  fail "boot ID did not change after the power cycle"
}

assert_listener_boundary_from() {
  local tailscale_ips=$1 listeners=$2 listener local_address allowed ip
  [[ -n "$listeners" ]] || unknown "TCP listener inventory is empty"
  while IFS= read -r listener; do
    local_address=$(awk '{print $4}' <<<"$listener")
    [[ -n "$local_address" ]] || unknown "TCP listener address is missing"
    allowed=false
    case "$local_address" in
      127.*:*|'[::1]:'*) allowed=true ;;
    esac
    while IFS= read -r ip; do
      [[ -n "$ip" ]] || continue
      if [[ "$local_address" == "$ip":* || "$local_address" == "$ip"%*:* || "$local_address" == "[$ip]"* ]]; then
        allowed=true
      fi
    done <<<"$tailscale_ips"
    [[ "$allowed" == true ]] || fail "TCP listener is outside loopback and Tailscale: $local_address"
  done <<<"$listeners"
}

assert_listener_boundary() {
  local tailscale_ips listeners
  tailscale_ips=$(remote_run 'tailscale ip -4; tailscale ip -6') || fail "could not read Tailscale addresses"
  listeners=$(remote_run 'sudo ss -ltnH') || fail "could not read TCP listening sockets"
  assert_listener_boundary_from "$tailscale_ips" "$listeners"
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

# deepgram_sockets_from_ss reads `ss -tinpH` and prints "LOCAL PEER BYTES_SENT" for each socket
# held by ghost-wispr that reports bytes_sent. It fails when there is none.
deepgram_sockets_from_ss() {
  awk '
    function flush() {
      if (held_by_ghost_wispr && bytes != "") {
        print local_address, peer, bytes
        found = 1
      }
    }
    /^[A-Z][A-Z0-9-]*[[:space:]]/ {
      flush()
      held_by_ghost_wispr = /users:\(\("ghost-wispr"/
      local_address = $4
      peer = $5
      bytes = ""
    }
    held_by_ghost_wispr && match($0, /bytes_sent:[0-9]+/) { bytes = substr($0, RSTART + 11, RLENGTH - 11) }
    END {
      flush()
      if (!found) exit 1
    }
  '
}

# deepgram_peers PEER... prints each PEER (an address:443 from ss) that serves a valid certificate
# for api.deepgram.com, checked from the Pi. The name is not resolved to find the socket: it has
# several addresses, and a new lookup often returns none of the one the recorder is connected to.
# On the office Pi on 2026-11-16 the recorder's socket was to 216.200.21.203 while the name
# resolved to 208.185.49.101, and a later read found the reverse.
deepgram_peers() {
  local peer check=''
  for peer in "$@"; do
    [[ $peer =~ ^(\[[0-9A-Fa-f:.]+\]|[0-9.]+):443$ ]] || fail "unexpected peer address from ss: $peer"
    check+="curl --silent --output /dev/null --max-time 10 --connect-to 'api.deepgram.com:443:$peer' https://api.deepgram.com/ && printf '%s\\n' '$peer'; "
  done
  remote_run "${check}true"
}

# deepgram_sockets_to PEERS reads `ss -tinpH` and prints "LOCAL PEER BYTES_SENT" for each socket
# held by ghost-wispr to one of PEERS (address:port, one per line). It prints nothing when none is.
deepgram_sockets_to() {
  { deepgram_sockets_from_ss || true; } | awk -v peers="$1" '
    BEGIN { n = split(peers, list, "\n"); for (i = 1; i <= n; i++) if (list[i] != "") wanted[list[i]] }
    $2 in wanted
  '
}

# deepgram_sockets prints "LOCAL PEER BYTES_SENT" for each socket ghost-wispr holds to
# api.deepgram.com: one `ss` read, then the certificate check of its port-443 peers.
deepgram_sockets() {
  local socket_state sockets verified_peers
  local -a peers
  socket_state=$(remote_run 'ss -tinpH') || fail "could not read the Pi's TCP sockets"
  sockets=$(printf '%s\n' "$socket_state" | deepgram_sockets_from_ss | awk '$2 ~ /:443$/') \
    || fail "no socket held by ghost-wispr with bytes_sent was found"
  [[ -n "$sockets" ]] || fail "no socket held by ghost-wispr to port 443 was found"
  mapfile -t peers < <(awk '{ print $2 }' <<<"$sockets" | LC_ALL=C sort -u)
  verified_peers=$(deepgram_peers "${peers[@]}") || fail "could not check the peers' certificates on the Pi"
  sockets=$(printf '%s\n' "$socket_state" | deepgram_sockets_to "$verified_peers")
  [[ -n "$sockets" ]] || fail "no socket held by ghost-wispr to api.deepgram.com was found"
  printf '%s\n' "$sockets"
}

# deepgram_bytes_sent [LOCAL PEER] prints bytes_sent of ghost-wispr's socket from LOCAL to PEER, a
# socket to api.deepgram.com that deepgram_recording_socket found, from one `ss` read. With no
# arguments it prints the sum over all ghost-wispr's sockets to api.deepgram.com.
deepgram_bytes_sent() {
  local sockets socket_state bytes
  if (( $# == 0 )); then
    sockets=$(deepgram_sockets) || return 1
  else
    socket_state=$(remote_run 'ss -tinpH') || fail "could not read the Pi's TCP sockets"
    sockets=$(printf '%s\n' "$socket_state" | deepgram_sockets_to "$2" | awk -v local_address="$1" '$1 == local_address')
  fi
  if ! bytes=$(awk 'NF { total += $3; found = 1 } END { if (!found) exit 1; print total }' <<<"$sockets"); then
    fail "the Deepgram socket from ${1:-} to ${2:-} is gone"
  fi
  [[ "$bytes" =~ ^[0-9]+$ ]] || fail "Deepgram bytes_sent is not numeric"
  printf '%s\n' "$bytes"
}

# deepgram_live_socket BEFORE AFTER SECONDS prints "LOCAL PEER" of the one Deepgram socket that grew
# by at least 32 kB/s between two readings ("LOCAL PEER BYTES_SENT" lines) SECONDS apart: the live
# stream. A batch transcription upload opens another socket to api.deepgram.com, so the pause
# control follows this one socket. It fails when no socket, or more than one, grew that much.
deepgram_live_socket() {
  local before=$1 after=$2 seconds=$3 local_address peer bytes start
  local -a live=()
  while read -r local_address peer bytes; do
    [[ -n $local_address ]] || continue
    start=$(awk -v local_address="$local_address" -v peer="$peer" '$1 == local_address && $2 == peer { print $3 }' <<<"$before")
    if [[ -n "$start" ]] && assert_minimum_growth "$start" "$bytes" "$seconds"; then
      live+=("$local_address $peer")
    fi
  done <<<"$after"
  (( ${#live[@]} == 1 )) || return 1
  printf '%s\n' "${live[0]}"
}

# deepgram_recording_socket SECONDS prints "LOCAL PEER" of the live Deepgram socket: the one that grew
# by at least 32 kB/s over SECONDS. It checks the peers' certificates first. Then one remote command
# reads `ss`, sleeps SECONDS and reads `ss` again, so only the sleep lies between the two readings:
# no SSH session and no certificate check, either of which would lower the bar below 32 kB/s.
# A socket to api.deepgram.com that opened or closed between the two readings (a reconnect of the
# stream, or a batch upload) leaves no socket, or two, that grew over the whole window; then it
# measures once more. A stream that is too slow keeps its socket, and fails at once.
deepgram_recording_socket() {
  local seconds=$1 attempt peers readings before after live
  [[ $seconds =~ ^[1-9][0-9]*$ ]] || fail "the Deepgram recording window must be whole seconds: $seconds"
  for attempt in 1 2; do
    peers=$(deepgram_sockets | awk '{ print $2 }' | LC_ALL=C sort -u) || return 1
    readings=$(remote_run "ss -tinpH && printf '%s\\n' '==' && sleep $seconds && ss -tinpH") \
      || fail "could not read the Pi's TCP sockets"
    before=$(sed '/^==$/,$d' <<<"$readings" | deepgram_sockets_to "$peers")
    after=$(sed '1,/^==$/d' <<<"$readings" | deepgram_sockets_to "$peers")
    if live=$(deepgram_live_socket "$before" "$after" "$seconds"); then
      printf '%s\n' "$live"
      return 0
    fi
    printf 'not exactly one socket to api.deepgram.com grew by 32 kB/s over %s s (local, peer, bytes_sent before: %s; after: %s)\n' \
      "$seconds" "$(tr '\n' ' ' <<<"$before")" "$(tr '\n' ' ' <<<"$after")" >&2
    [[ $attempt == 1 && $(awk '{ print $1, $2 }' <<<"$before" | LC_ALL=C sort) != "$(awk '{ print $1, $2 }' <<<"$after" | LC_ALL=C sort)" ]] \
      || return 1
    printf 'a socket to api.deepgram.com opened or closed during the window; measuring once more\n' >&2
  done
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

  if printf '%s\n' 'ESTAB 0 0 10.0.0.2:12345 34.120.1.2:443 users:(("ghost-wispr",pid=7,fd=9))' | deepgram_sockets_from_ss; then
    fail "self-test expected the Deepgram socket control to reject fake ss output without bytes_sent"
  fi
  printf 'self-test: Deepgram socket control rejected fake ss output\n'

  parsed_sockets=$(printf '%s\n' 'ESTAB 0 0 10.0.0.2:12345 34.120.1.2:443 users:(("ghost-wispr",pid=7,fd=9))' ' bbr bytes_sent:160000' \
    'ESTAB 0 0 10.0.0.2:23456 34.120.1.3:443 users:(("other",pid=8,fd=9))' ' bbr bytes_sent:7' | deepgram_sockets_from_ss)
  [[ "$parsed_sockets" == '10.0.0.2:12345 34.120.1.2:443 160000' ]] \
    || fail "self-test expected the Deepgram parser to accept a ghost-wispr socket alone"
  printf 'self-test: Deepgram socket control accepted valid fake ss output\n'

  # remote_run is a stub here: a peer that slipped through would run nothing on the Pi.
  if (remote_run() { :; }; deepgram_peers "34.120.1.2:443';reboot;'") >/dev/null 2>&1; then
    fail "self-test expected the Deepgram certificate check to refuse a peer that is not an address"
  fi
  printf 'self-test: Deepgram certificate check refused a peer that is not an address\n'

  live_socket=$(deepgram_live_socket $'10.0.0.2:1 34.120.1.2:443 100\n10.0.0.2:2 34.120.1.2:443 100' \
    $'10.0.0.2:1 34.120.1.2:443 200000\n10.0.0.2:2 34.120.1.2:443 150' 5) \
    || fail "self-test expected the live Deepgram socket to be the one that grew by 32 kB/s"
  [[ "$live_socket" == '10.0.0.2:1 34.120.1.2:443' ]] || fail "self-test chose $live_socket as the live Deepgram socket"
  if deepgram_live_socket $'10.0.0.2:1 34.120.1.2:443 100\n10.0.0.2:2 34.120.1.2:443 100' \
    $'10.0.0.2:1 34.120.1.2:443 200000\n10.0.0.2:2 34.120.1.2:443 200000' 5 >/dev/null; then
    fail "self-test expected two Deepgram sockets that grew by 32 kB/s to be refused"
  fi
  printf 'self-test: Deepgram live socket control followed the one socket that carried audio\n'

  if assert_minimum_growth 100 101 5; then
    fail "self-test expected the Deepgram recording control to reject low growth"
  fi
  printf 'self-test: Deepgram recording control rejected low growth\n'

  if boot_id_changed old-boot old-boot; then
    fail "self-test expected unchanged boot ID to be rejected"
  fi
  boot_id_changed old-boot new-boot || fail "self-test expected a new boot ID to be accepted"
  printf 'self-test: boot control rejected an old still-capturing boot ID\n'

  (
    stopped_pid=17
    remote_run() {
      printf '%s\n' "$*" >"$controls_dir/recovery-command.txt"
      return 255
    }
    resume_stopped_process
  )
  grep -qx 'kill -CONT 17' "$controls_dir/recovery-command.txt" \
    || fail "self-test expected cleanup to resume a stopped process after an SSH failure"
  printf 'self-test: cleanup attempted kill -CONT after a stubbed SSH failure\n'

  tailscale_fixture=$'100.100.134.41\nfd7a:115c:a1e0::836:862b'
  listener_fixture=$'LISTEN 0 4096 100.100.134.41%tailscale0:40271 0.0.0.0:*\nLISTEN 0 4096 [fd7a:115c:a1e0::836:862b]%tailscale0:34493 [::]:*\nLISTEN 0 128 127.0.0.1:22 0.0.0.0:*'
  assert_listener_boundary_from "$tailscale_fixture" "$listener_fixture"
  if (assert_listener_boundary_from "$tailscale_fixture" "$listener_fixture"$'\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*'); then
    fail "self-test expected a public TCP listener to be rejected"
  fi
  printf 'self-test: TCP listener fixture accepted tailnet IPv6 and rejected 0.0.0.0:22\n'
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
printf 'A4 NEEDS-PERSON: press Enter, then leave the recorder capturing, pull power, and reconnect it. '
read -r _
wait_for_new_boot "$a4_boot_before"
assert_dark_until_capturing
assert_capturing
printf 'A4 RAN: a new boot ID came back capturing.\n'
printf 'Light power-cycle NEEDS-PERSON: confirm the light stayed dark until Ghost Wispr began capturing.\n'

printf 'A1 RAN: check the red light, Deepgram control, and pause button.\n'
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == red ]] || fail "A1 FAILED: light is not red while capturing"
# 60 s, not 5 s: the stream sends one 250 ms chunk (8060 B on the wire) at a time, and it runs only
# 0.8 % above 32 kB/s. A 5 s window can hold one chunk fewer; on the office Pi 1 of 24 grew by
# 153188 B, below the 160000 B bar. Over 60 s one chunk is 0.4 % of the bar.
readonly deepgram_control_seconds=60
deepgram_socket=$(deepgram_recording_socket "$deepgram_control_seconds") \
  || fail "A1 FAILED: Deepgram recording control grew by less than 32 kB/s"
read -r deepgram_local deepgram_peer <<<"$deepgram_socket"
audio_before=$(remote_run 'du -sb /opt/ghost-wispr/data/audio 2>/dev/null | cut -f1 || printf 0')
printf 'A1 NEEDS-PERSON: tap the Pi power button once to pause, then press Enter. '
read -r _
assert_paused
[[ $(remote_run '/opt/ghost-wispr/ghost-wispr light-state') == off ]] || fail "A1 FAILED: light stays on after pause"
paused_bytes_before=$(deepgram_bytes_sent "$deepgram_local" "$deepgram_peer")
sleep 60
paused_bytes_after=$(deepgram_bytes_sent "$deepgram_local" "$deepgram_peer")
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
stopped_pid=$main_pid
if ! wait_for_light off 3000; then
  fail "Light hang FAILED: light did not turn off within 3 seconds"
fi
remote_run "kill -CONT $main_pid"
stopped_pid=
wait_for_light red 2000 || fail "Light hang FAILED: light did not turn red after continue"
printf 'Light hang RAN: dark within 3 seconds and red after continue.\n'

printf 'Light correction RAN: force the light off and require correction while capturing.\n'
remote_run '/opt/ghost-wispr/ghost-wispr light-off'
wait_for_light red 2000 || fail "Light correction FAILED: recorder did not restore red within 2 seconds"
printf 'Light correction RAN: recorder restored red within 2 seconds.\n'

printf 'A3 RAN: pause persists through a power cycle.\n'
a3_boot_before=$(boot_id)
printf 'A3 NEEDS-PERSON: tap once to pause, pull power, reconnect it, then press Enter. '
read -r _
wait_for_new_boot "$a3_boot_before"
wait_for_recorder
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
printf 'A5 RAN: local listener boundary passed. A5 NEEDS-PERSON: test a phone off-tailnet and %s; both must fail.\n' "$PI_DENIED_TAG"

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
    printf 'Envoy NEEDS-PERSON: inject GHOST_WISPR_ENVOY_WEBHOOK_SECRET from deploy/pi/webhook-secret.sh to run the signed probe (docs/pi-setup.md, "Accept slice 1").\n'
  fi
  unsigned_code=$(remote_run "curl --silent --output /dev/null --write-out '%{http_code}' --request POST '$relay_url' --header 'X-GhostWispr-Delivery: unsigned-probe' --header 'X-GhostWispr-Event: session.ended' --data '{}'")
  [[ "$unsigned_code" == 401 ]] || fail "Envoy FAILED: unsigned Pi request returned HTTP $unsigned_code, expected 401"
  printf 'Envoy RAN: unsigned Pi request was refused.\n'
else
  printf 'Envoy NEEDS-PERSON: rerun with --relay-url, the relay URL docs/pi-setup.md, "Accept slice 1", reads.\n'
fi

printf 'A2 NEEDS-PERSON: open %s and choose Recording rules; each rule under recording_rules in the Pi'\''s config, /opt/ghost-wispr/ghost-wispr.yaml (rendered from deploy/pi/files/ghost-wispr.yaml), must appear verbatim and in order.\n' "$unit_url"
printf 'A7 RAN: compare the Pi inventory with deploy/pi/baseline.txt.\n'
readonly expected_package_roots=(libportaudio2 ffmpeg lame jq)
{
  printf '%s\n' "${expected_package_roots[@]}"
  remote_run 'apt-cache depends --recurse --no-recommends --no-suggests --no-enhances --no-conflicts --no-breaks --no-replaces libportaudio2 ffmpeg lame jq'
} | dependency_packages_from_apt_cache >"$scratch_dir/allowed-packages.txt"
remote_run "dpkg-query -W -f='\${Package}\\n' | LC_ALL=C sort -u" >"$scratch_dir/current-packages.txt"
awk '/^---$/{exit} NF {print}' "$baseline_file" | LC_ALL=C sort -u >"$scratch_dir/baseline-packages.txt"
{
  cat "$scratch_dir/baseline-packages.txt"
  cat "$scratch_dir/allowed-packages.txt"
} | LC_ALL=C sort -u >"$scratch_dir/permitted-packages.txt"
assert_set_subset package "$scratch_dir/baseline-packages.txt" "$scratch_dir/current-packages.txt" "$scratch_dir/permitted-packages.txt" \
  || fail "A7 FAILED: unexpected installed package"

system_services=$(remote_run "systemctl list-units --type=service --state=running --plain --no-legend | awk '{print \$1}'") \
  || fail "A7 FAILED: could not read running system services"
user_services=$(remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user list-units --type=service --state=running --plain --no-legend | awk '{print \$1}'") \
  || fail "A7 FAILED: could not read running user services"
[[ -n "$system_services" ]] || unknown "system service inventory is empty"
[[ -n "$user_services" ]] || unknown "user service inventory is empty"
printf '%s\n' "$system_services" | LC_ALL=C sort -u >"$scratch_dir/current-system-services.txt"
printf '%s\n' "$user_services" | LC_ALL=C sort -u >"$scratch_dir/current-user-services.txt"
awk 'section == 1 && NF {print} /^---$/ {section += 1}' "$baseline_file" | LC_ALL=C sort -u >"$scratch_dir/baseline-system-services.txt"
awk 'section >= 2 && NF {print} /^---$/ {section += 1}' "$baseline_file" | LC_ALL=C sort -u >"$scratch_dir/baseline-user-services.txt"
assert_set_subset system-service "$scratch_dir/baseline-system-services.txt" "$scratch_dir/current-system-services.txt" "$scratch_dir/baseline-system-services.txt" \
  || fail "A7 FAILED: unexpected running system service"
readonly expected_user_service_additions=(ghost-wispr.service ghost-wispr-update.service)
{
  cat "$scratch_dir/baseline-user-services.txt"
  printf '%s\n' "${expected_user_service_additions[@]}"
} | LC_ALL=C sort -u >"$scratch_dir/permitted-user-services.txt"
assert_set_subset user-service "$scratch_dir/baseline-user-services.txt" "$scratch_dir/current-user-services.txt" "$scratch_dir/permitted-user-services.txt" \
  || fail "A7 FAILED: unexpected running user service"
timer_state=$(remote_run "XDG_RUNTIME_DIR=\"/run/user/\$(id -u)\" systemctl --user is-enabled ghost-wispr-update.timer" || true)
[[ "$timer_state" == enabled ]] || fail "A7 FAILED: ghost-wispr-update.timer is not enabled"
printf 'A7 RAN: package and service inventories match the allowed sets.\n'
printf 'Slice 1 driver RAN automated rows; record every NEEDS-PERSON observation before acceptance.\n'
