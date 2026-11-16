#!/usr/bin/env bash
# Checks accept-slice1.sh's Deepgram socket reading against what ss does on the Pi.
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
for name in deepgram_bytes_from_ss deepgram_bytes_sent; do
  [[ -n $(function_source "$name") ]] || fail "could not read $name from the driver"
done

# The Pi's ss: the Deepgram socket is to an address api.deepgram.com no longer resolves to, so a
# `dst api.deepgram.com` filter matches nothing. Any other query sees the socket.
readonly ss_output=$'ESTAB 0      0       192.0.2.10:34474  216.200.21.203:443   users:(("ghost-wispr",pid=17259,fd=15))\n\t cubic wscale:10,10 rto:212 bytes_sent:9378691 bytes_acked:9378692 bytes_received:186209'
result=$(
  eval "$(function_source deepgram_bytes_from_ss)"
  eval "$(function_source deepgram_bytes_sent)"
  # shellcheck disable=SC2329 # deepgram_bytes_sent, eval'd above, calls it.
  remote_run() {
    if [[ $* == *'dst api.deepgram.com'* ]]; then
      return 0
    fi
    printf '%s\n' "$ss_output"
  }
  deepgram_bytes_sent 2>&1
) || fail "deepgram_bytes_sent found no socket when api.deepgram.com resolves to another address than the one connected: $result"
[[ $result == 9378691 ]] || fail "deepgram_bytes_sent read $result, want 9378691"

printf 'slice 1 acceptance test passed\n'
