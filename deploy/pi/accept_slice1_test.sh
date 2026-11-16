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
# Every deepgram_* function of the driver, so the test runs the driver's own helpers.
mapfile -t deepgram_functions < <(awk '/^deepgram_[a-z_]+\(\) \{$/ { sub(/\(\) \{$/, ""); print }' "$driver")
[[ " ${deepgram_functions[*]} " == *' deepgram_bytes_sent '* ]] || fail "could not read deepgram_bytes_sent from the driver"

# The Pi's ss: the Deepgram socket is to an address api.deepgram.com no longer resolves to, so a
# `dst api.deepgram.com` filter matches nothing. Ghost Wispr also holds a socket to Google and the
# Serve proxy's loopback socket, and neither is Deepgram's.
readonly ss_output=$'ESTAB 0      0       192.0.2.10:57538 142.251.218.234:443  users:(("ghost-wispr",pid=17259,fd=13))\n\t cubic wscale:8,10 rto:204 bytes_sent:3410 bytes_acked:3411 bytes_received:5763\nESTAB 0      0       192.0.2.10:34474  216.200.21.203:443   users:(("ghost-wispr",pid=17259,fd=15))\n\t cubic wscale:10,10 rto:212 bytes_sent:9378691 bytes_acked:9378692 bytes_received:186209\nESTAB 0      0           127.0.0.1:8080        127.0.0.1:39406 users:(("ghost-wispr",pid=17259,fd=10))\n\t cubic wscale:10,10 rto:204 bytes_sent:5312 bytes_acked:5312 bytes_received:4144'

# deepgram_bytes_sent_on_pi [LOCAL] runs the driver's deepgram_bytes_sent against that Pi.
deepgram_bytes_sent_on_pi() (
  for name in "${deepgram_functions[@]}"; do
    eval "$(function_source "$name")"
  done
  # shellcheck disable=SC2329 # deepgram_bytes_sent, eval'd above, calls it.
  remote_run() {
    case $* in
      *'dst api.deepgram.com'*) return 0 ;;
      *--connect-to*)
        # The Pi's curl: only Deepgram's address serves a certificate for api.deepgram.com.
        if [[ $* == *"'api.deepgram.com:443:216.200.21.203:443'"* ]]; then
          printf '216.200.21.203:443\n'
        fi
        ;;
      *) printf '%s\n' "$ss_output" ;;
    esac
  }
  deepgram_bytes_sent "$@" 2>&1
)

result=$(deepgram_bytes_sent_on_pi) \
  || fail "deepgram_bytes_sent found no socket when api.deepgram.com resolves to another address than the one connected: $result"
[[ $result == 9378691 ]] || fail "deepgram_bytes_sent read $result, want 9378691"
# The pause control follows the live socket by its local address.
result=$(deepgram_bytes_sent_on_pi 192.0.2.10:34474) || fail "deepgram_bytes_sent did not follow the live socket: $result"
[[ $result == 9378691 ]] || fail "deepgram_bytes_sent read $result for the live socket, want 9378691"
if result=$(deepgram_bytes_sent_on_pi 192.0.2.10:57538); then
  fail "deepgram_bytes_sent read $result from the socket to Google"
fi

bash "$driver" --self-test >/dev/null 2>&1 || fail "accept-slice1.sh --self-test failed: $(bash "$driver" --self-test 2>&1)"

printf 'slice 1 acceptance test passed\n'
