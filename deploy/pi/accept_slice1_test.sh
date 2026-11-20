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
# The driver reads neutral deployment settings, not the office Pi's.
export GHOST_WISPR_PI_ENV="$repo_root/deploy/pi/test-pi.env"

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

# The fake Pi's state across remote_run calls: the Deepgram socket's local port, which a reconnect
# changes, and the number of windows (sleeps) so far.
state_dir=$(mktemp -d)
trap 'rm -rf -- "$state_dir"' EXIT

# remote_run runs the command the driver sends, as the Pi's shell would, with the Pi's tools stubbed:
# ss prints the Pi's counters, sleep moves them on by its argument and returns at once, and curl
# verifies api.deepgram.com only at Deepgram's addresses. Real curl does the same: exit 0 at
# Deepgram's addresses and exit 60 at a Google address, where the name does not match. Over a sleep
# of S seconds the Deepgram socket sends deepgram_rate B/s (the stream's 32249 unless set) less one
# 8060 B chunk, as one window on the office Pi did, and the Google socket 40000 B/s of a Drive
# upload. In the first `reconnects` windows (0 unless set) the stream reconnects halfway: the later
# reading has a new Deepgram socket, to Deepgram's other address, which has sent half a window.
# shellcheck disable=SC2329 # The driver's functions, eval'd below, call remote_run, and eval calls the stubs.
remote_run() (
  slept=0
  windows=0
  port=$(cat "$state_dir/port" 2>/dev/null || printf 34474)
  sleep() {
    slept=$((slept + $1))
    windows=$(($(cat "$state_dir/windows" 2>/dev/null || printf 0) + 1))
    printf '%s\n' "$windows" >"$state_dir/windows"
  }
  ss() {
    # `ss dst HOST` resolves HOST now, and api.deepgram.com resolves to another address.
    [[ " $* " != *' dst '* ]] || return 0
    local deepgram=9378691 google=3410 reading
    if ((slept > 0)); then
      google=$((google + 40000 * slept))
      if ((windows <= ${reconnects:-0})); then
        port=$((port + 2))
        printf '%s\n' "$port" >"$state_dir/port"
        deepgram=$((${deepgram_rate:-32249} * slept / 2))
      else
        deepgram=$((deepgram + ${deepgram_rate:-32249} * slept - 8060))
      fi
    fi
    reading=${ss_output/bytes_sent:3410 /bytes_sent:$google }
    reading=${reading/192.0.2.10:34474 /192.0.2.10:$port }
    # A reconnect resolves api.deepgram.com again, and gets another of its addresses.
    [[ $port == 34474 ]] || reading=${reading/216.200.21.203:443/208.185.49.101:443}
    printf '%s\n' "${reading/bytes_sent:9378691 /bytes_sent:$deepgram }"
  }
  curl() {
    [[ $* == *'api.deepgram.com:443:216.200.21.203:443'* || $* == *'api.deepgram.com:443:208.185.49.101:443'* ]] || return 60
  }
  eval "$*"
)

# on_pi FUNCTION ARGS... runs the driver's FUNCTION against that Pi, from its first state.
on_pi() (
  rm -f -- "$state_dir/port" "$state_dir/windows"
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

# A stream 1 % below 32 kB/s fails, at once: a shorter sleep than the window, a longer one, or a
# lower bar would pass it.
if result=$(deepgram_rate=31680 on_pi deepgram_recording_socket "$window"); then
  fail "deepgram_recording_socket passed a stream of 31680 B/s: $result"
fi
[[ $result != *'measuring once more'* ]] || fail "deepgram_recording_socket measured a slow stream twice: $result"

# A reconnect in the window: the control measures once more, checks the new socket's peer, and
# follows the new socket. A second reconnect fails it.
result=$(reconnects=1 on_pi deepgram_recording_socket "$window") \
  || fail "deepgram_recording_socket failed when the stream reconnected once during the window: $result"
[[ $result == *'measuring once more'*$'\n''192.0.2.10:34476 208.185.49.101:443' ]] \
  || fail "after a reconnect deepgram_recording_socket did not measure again and follow the new socket: $result"
if result=$(reconnects=2 on_pi deepgram_recording_socket "$window"); then
  fail "deepgram_recording_socket passed when the stream reconnected in both windows: $result"
fi

# The pause control follows the live socket by its local address and peer.
result=$(on_pi deepgram_bytes_sent 192.0.2.10:34474 216.200.21.203:443) || fail "deepgram_bytes_sent did not follow the live socket: $result"
[[ $result == 9378691 ]] || fail "deepgram_bytes_sent read $result for the live socket, want 9378691"
if result=$(on_pi deepgram_bytes_sent 192.0.2.10:40000 216.200.21.203:443); then
  fail "deepgram_bytes_sent read $result from a socket the Pi does not hold"
fi

# The Light hang step stops the recorder only while it holds the blink(1)'s lock, the flock that every
# ghost-wispr command to the light holds across its exchange. A recorder stopped inside a command would
# keep the lock, and each light-state after it would print unknown. The driver's stop command runs
# against a fake Pi whose blink(1) is its second hidraw device; the first is another HID device.
eval "$(function_source light_locked_stop_command)"
sysfs=$state_dir/sys
dev=$state_dir/dev
mkdir -p "$sysfs/class/hidraw/hidraw0/device" "$sysfs/class/hidraw/hidraw1/device" "$dev"
printf 'HID_ID=0003:0000046D:0000C52B\n' >"$sysfs/class/hidraw/hidraw0/device/uevent"
printf 'HID_ID=0003:000027B8:000001ED\n' >"$sysfs/class/hidraw/hidraw1/device/uevent"
: >"$dev/hidraw0"
: >"$dev/hidraw1"
# The fake recorder outlives the whole test, whose CI step stops at 10 minutes, so the EXIT trap never
# meets a recorder that has already ended; and a kill the trap makes of a process that has ended does
# not stop the trap under set -e.
sleep 600 &
recorder=$!
trap 'kill -CONT "$recorder" 2>/dev/null || :; kill "$recorder" ${holder:-} ${stopper:-} 2>/dev/null || :; rm -rf -- "$state_dir"' EXIT
stopped() {
  local state
  for _ in {1..100}; do
    state=$(ps -o stat= -p "$recorder" | tr -d ' ')
    [[ $state == T* ]] && return
    sleep 0.01
  done
  return 1
}
# proc_state PID sets pstate to PID's state letter, or to "gone" once PID has exited; builtins only.
proc_state() {
  local stat
  if read -r stat 2>/dev/null <"/proc/$1/stat"; then
    stat=${stat##*) }
    pstate=${stat%% *}
  else
    pstate=gone
  fi
}
# scan_locks reads /proc/locks once, builtins only: it sets light_id to the MAJOR:MINOR:INODE of the
# lock the holder process holds, and light_waited to true if a process waits for that lock. It
# matches one regular expression over the whole file, so each read stays cheap on a loaded runner,
# whose /proc/locks can list hundreds of other locks.
scan_locks() {
  local locks='' re
  read -r -d '' locks </proc/locks || :
  if [[ -z $light_id ]]; then
    re="(^|${nl})[0-9]+: FLOCK[[:space:]]+[A-Z]+[[:space:]]+[A-Z]+[[:space:]]+${holder}[[:space:]]+([0-9a-f]+:[0-9a-f]+:[0-9]+)[[:space:]]"
    [[ $locks =~ $re ]] && light_id=${BASH_REMATCH[2]}
  fi
  light_waited=false
  if [[ -n $light_id ]]; then
    re="->[[:space:]]+FLOCK[^${nl}]*[[:space:]]${light_id}[[:space:]]"
    [[ $locks =~ $re ]] && light_waited=true
  fi
  return 0
}
nl=$'\n'
# The stop waits at most 5 s for the light's lock (flock -w 5), the Light hang step's own bound. While
# the light's lock is held, the test starts no process but the stop, however slowly processes start on
# a loaded runner: it waits with shell builtins only (reads of /proc, and timed-out reads of a pipe
# nothing writes to), so of those 5 s it takes only its 1 s watch and the release.
stop_command=$(light_locked_stop_command "$recorder" "$sysfs" "$dev")
mkfifo "$state_dir/tick" "$state_dir/release"
exec {tick}<>"$state_dir/tick" {release}<>"$state_dir/release"
# The other HID device's lock is not the light's: the test holds it throughout.
exec {other_lock}<"$dev/hidraw0"
flock "$other_lock"
# The recorder is inside a command to the light, so it holds the light's lock: a flock process holds
# it until the test kills it. It is a live process because a container's /proc/locks hides the lock
# of a process that has exited, with the processes that wait for it; -o keeps the lock from its
# command, which ends when the test closes the release pipe.
light_id=''
flock -o "$dev/hidraw1" head -c 1 "$state_dir/release" >/dev/null {tick}<&- {release}<&- {other_lock}<&- &
holder=$!
deadline=$((${EPOCHREALTIME//[!0-9]/} + 30000000))
until scan_locks; [[ -n $light_id ]]; do
  ((${EPOCHREALTIME//[!0-9]/} < deadline)) || fail "could not find the test's own hold on the blink(1)'s lock in /proc/locks"
  read -r -t 0.01 -u "$tick" _ || :
done
bash -c "$stop_command" {tick}<&- {release}<&- {other_lock}<&- &
stopper=$!
# The stop must wait for the light's lock, which /proc/locks shows whether or not its shell execs
# flock, and must not stop the recorder until the test lets the light go: neither before it waits,
# nor in the 1 s the test then holds the lock. A stop that does not wait for the lock fails however
# late it stops the recorder; one that waits for another device's lock fails within 30 s, even with
# no time limit of its own.
# Known limits: the fake recorder never takes the light's lock, so the test cannot see whether the stop
# holds that lock at the moment it stops the recorder. Two faulty stops pass: one that takes the lock
# and frees it before it stops the recorder (flock -w 5 ... true && exec kill -STOP PID), and one
# that leaves a separate process waiting for the lock and stops the recorder later without it
# (flock -w 5 ... true & sleep 3; exec kill -STOP PID). Catching them needs a fake recorder that
# itself takes the lock in short bursts.
deadline=$((${EPOCHREALTIME//[!0-9]/} + 30000000))
waited_since=''
while :; do
  proc_state "$recorder"
  [[ $pstate != T* ]] || fail "the Light hang step stopped the recorder while it held the blink(1)'s lock"
  now=${EPOCHREALTIME//[!0-9]/}
  if [[ -n $waited_since ]]; then
    ((now - waited_since < 1000000)) || break
  else
    scan_locks
    if [[ $light_waited == true ]]; then
      waited_since=$now
    else
      proc_state "$stopper"
      if [[ $pstate == gone || $pstate == Z ]]; then
        proc_state "$recorder"
        [[ $pstate != T* ]] || fail "the Light hang step stopped the recorder while it held the blink(1)'s lock"
        fail "the Light hang step's stop ended without waiting for the blink(1)'s lock"
      fi
      ((now < deadline)) || fail "the Light hang step's stop did not wait for the blink(1)'s lock within 30 s"
    fi
  fi
  read -r -t 0.01 -u "$tick" _ || :
done
# A stop that gave up waiting before the light is free would stop the recorder without its lock.
scan_locks
[[ $light_waited == true ]] || fail "the Light hang step's stop stopped waiting for the blink(1)'s lock before it was free"
# Let the light go: the holder dies, and its lock with it.
kill "$holder"
deadline=$((${EPOCHREALTIME//[!0-9]/} + 10000000))
until proc_state "$stopper"; [[ $pstate == gone || $pstate == Z ]]; do
  ((${EPOCHREALTIME//[!0-9]/} < deadline)) || fail "the Light hang step's stop did not end within 10 s of the blink(1) being free"
  read -r -t 0.01 -u "$tick" _ || :
done
wait "$stopper" || fail "the Light hang step's stop failed once the blink(1) was free"
wait "$holder" || :
stopper='' holder=''
stopped || fail "the Light hang step did not stop the recorder once the blink(1) was free"
flock -n "$dev/hidraw1" true || fail "the Light hang step's stop kept the blink(1)'s lock"
kill -CONT "$recorder"
exec {other_lock}<&- {tick}<&- {release}<&-
# With no blink(1), the stop fails and stops nothing.
rm -- "$sysfs/class/hidraw/hidraw1/device/uevent"
if bash -c "$(light_locked_stop_command "$recorder" "$sysfs" "$dev")"; then
  fail "the Light hang step's stop succeeded with no blink(1)"
fi
! stopped || fail "the Light hang step stopped the recorder with no blink(1)"

bash "$driver" --self-test >/dev/null 2>&1 || fail "accept-slice1.sh --self-test failed: $(bash "$driver" --self-test 2>&1)"

printf 'slice 1 acceptance test passed\n'
