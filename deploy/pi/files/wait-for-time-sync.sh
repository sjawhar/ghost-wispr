#!/usr/bin/env bash
# Hold Ghost Wispr's start until the Pi's clock is synchronized.
#
# The Pi has no battery-backed clock: at boot its clock is set back to a time
# it saved earlier, and a time server sets it right a little later. A session
# started before that is named and stamped with the wrong time. On the office
# Pi on 2026-11-17 a start before the sync stamped the end of the session a
# power cut had stopped before that session's last audio, and started the next
# session 26 minutes too early.
#
# Ghost Wispr runs as a systemd user service, which cannot be ordered after
# the system's time-sync.target, so its start waits here instead, for the
# mark systemd-timesyncd leaves once it has synchronized the clock (the mark
# systemd-time-wait-sync waits for too). While it waits Ghost Wispr is not
# running: nothing is recorded, and the light is off. After
# GHOST_WISPR_TIMESYNC_TIMEOUT seconds (120) it lets Ghost Wispr start all the
# same, so a Pi that reaches no time server still records, with the light red
# while it does, and with times from a clock that may be wrong. A Pi with no
# network at all does not get this far: wait-for-network.sh, which runs
# before, fails and systemd starts the service again.
set -u

readonly TIMEOUT_SECONDS="${GHOST_WISPR_TIMESYNC_TIMEOUT:-120}"
readonly SYNCHRONIZED="${GHOST_WISPR_TIMESYNC_MARK:-/run/systemd/timesync/synchronized}"

# Count the wait in sleeps, not in clock readings: the clock this waits for
# can jump forward by minutes in the middle of the wait.
for ((waited = 0; waited < TIMEOUT_SECONDS; waited++)); do
  if [[ -e $SYNCHRONIZED ]]; then
    ((waited == 0)) || echo "ghost-wispr: clock synchronized after ${waited}s" >&2
    exit 0
  fi
  ((waited % 10 == 0)) && echo "ghost-wispr: waiting for the clock to be synchronized..." >&2
  sleep 1
done
if [[ -e $SYNCHRONIZED ]]; then
  exit 0
fi
echo "ghost-wispr: clock not synchronized after ${TIMEOUT_SECONDS}s; starting with it as it is, so session times may be wrong" >&2
exit 0
