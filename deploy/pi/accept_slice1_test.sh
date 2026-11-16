#!/usr/bin/env bash
# Checks accept-slice1.sh's Deepgram socket reading against what ss does on the Pi, and runs the
# driver's own self-test.
#
# `ss dst HOST` resolves HOST when it runs and keeps only sockets to the addresses it got then.
# api.deepgram.com resolves to more than one address, so the address of the socket Ghost Wispr
# opened earlier is often not among them. On the office Pi on 2026-11-16 the socket held by
# ghost-wispr was ESTAB to 216.200.21.203:443 and sent 32249 B/s, while api.deepgram.com resolved
# to 208.185.49.101. `ss -tinpH dst api.deepgram.com` printed nothing, and A1 failed with "no
# Deepgram socket held by ghost-wispr with bytes_sent was found".
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly driver="$repo_root/deploy/pi/accept-slice1.sh"

fail() {
  printf 'slice 1 acceptance test FAILED: %s\n' "$*" >&2
  exit 1
}

# function_source NAME prints the driver's definition of NAME.
function_source() {
  awk -v start="$1() {" '$0 == start { found = 1 } found { print } found && /^}$/ { exit }' "$driver"
}
# The driver's own functions: every deepgram_* one and the growth check they use.
mapfile -t driver_functions < <(awk '/^deepgram_[a-z_]+\(\) \{$/ { sub(/\(\) \{$/, ""); print }' "$driver")
[[ " ${driver_functions[*]} " == *' deepgram_bytes_sent '* ]] || fail "could not read deepgram_bytes_sent from the driver"
driver_functions+=(assert_minimum_growth)

# The Pi's ss: the Deepgram socket is to an address api.deepgram.com no longer resolves to, so a
# `dst api.deepgram.com` filter matches nothing. Ghost Wispr also holds a socket to Google and the
# Serve proxy's loopback socket, and neither is Deepgram's.
readonly ss_output=$'ESTAB 0      0       192.0.2.10:57538 142.251.218.234:443  users:(("ghost-wispr",pid=17259,fd=13))\n\t cubic wscale:8,10 rto:204 bytes_sent:3410 bytes_acked:3411 bytes_received:5763\nESTAB 0      0       192.0.2.10:34474  216.200.21.203:443   users:(("ghost-wispr",pid=17259,fd=15))\n\t cubic wscale:10,10 rto:212 bytes_sent:9378691 bytes_acked:9378692 bytes_received:186209\nESTAB 0      0           127.0.0.1:8080        127.0.0.1:39406 users:(("ghost-wispr",pid=17259,fd=10))\n\t cubic wscale:10,10 rto:204 bytes_sent:5312 bytes_acked:5312 bytes_received:4144'

# The driver's own recording window, so the test runs A1's measurement as A1 runs it.
window=$(sed -n 's/^readonly deepgram_control_seconds=\([0-9][0-9]*\)$/\1/p' "$driver")
[[ $window =~ ^[1-9][0-9]*$ ]] || fail "could not read deepgram_control_seconds from the driver"

# remote_run runs the command the driver sends, as the Pi's shell would, with the Pi's tools stubbed:
# ss prints the Pi's counters, sleep moves them on by its argument and returns at once, and curl
# verifies api.deepgram.com only at Deepgram's address. Real curl does the same: exit 0 at
# Deepgram's addresses and exit 60 at a Google address, where the name does not match. Over a sleep
# of S seconds the Deepgram socket sends the stream's 32249 B/s less one 8060 B chunk, as one window
# on the office Pi did, and the Google socket 40000 B/s of a Drive upload.
# shellcheck disable=SC2329 # The driver's functions, eval'd below, call remote_run, and eval calls the stubs.
remote_run() (
  slept=0
  sleep() { slept=$((slept + $1)); }
  ss() {
    # `ss dst HOST` resolves HOST now, and api.deepgram.com resolves to another address.
    [[ " $* " != *' dst '* ]] || return 0
    local deepgram=9378691 google=3410 reading
    if ((slept > 0)); then
      deepgram=$((deepgram + 32249 * slept - 8060))
      google=$((google + 40000 * slept))
    fi
    reading=${ss_output/bytes_sent:3410 /bytes_sent:$google }
    printf '%s\n' "${reading/bytes_sent:9378691 /bytes_sent:$deepgram }"
  }
  curl() { [[ $* == *'api.deepgram.com:443:216.200.21.203:443'* ]] || return 60; }
  eval "$*"
)

# on_pi FUNCTION ARGS... runs the driver's FUNCTION against that Pi.
on_pi() (
  for name in "${driver_functions[@]}"; do
    eval "$(function_source "$name")"
  done
  "$@" 2>&1
)

result=$(on_pi deepgram_bytes_sent) \
  || fail "deepgram_bytes_sent found no socket when api.deepgram.com resolves to another address than the one connected: $result"
[[ $result == 9378691 ]] || fail "deepgram_bytes_sent read $result, want 9378691"

# The recording control picks the Deepgram socket alone, from two readings the driver's window apart
# on the Pi: the Google socket grew more, but its peer has no certificate for api.deepgram.com.
result=$(on_pi deepgram_recording_socket "$window") \
  || fail "deepgram_recording_socket found no live Deepgram socket over the driver's ${window} s window: $result"
[[ $result == '192.0.2.10:34474 216.200.21.203:443' ]] || fail "deepgram_recording_socket chose $result, want the socket to 216.200.21.203"

# The pause control follows the live socket by its local address and peer.
result=$(on_pi deepgram_bytes_sent 192.0.2.10:34474 216.200.21.203:443) || fail "deepgram_bytes_sent did not follow the live socket: $result"
[[ $result == 9378691 ]] || fail "deepgram_bytes_sent read $result for the live socket, want 9378691"
if result=$(on_pi deepgram_bytes_sent 192.0.2.10:40000 216.200.21.203:443); then
  fail "deepgram_bytes_sent read $result from a socket the Pi does not hold"
fi

bash "$driver" --self-test >/dev/null 2>&1 || fail "accept-slice1.sh --self-test failed: $(bash "$driver" --self-test 2>&1)"

printf 'slice 1 acceptance test passed\n'
