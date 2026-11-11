#!/usr/bin/env bash
# Block until Ghost Wispr's live-transcription dependency is reachable before
# the service starts.
#
# On a reboot the systemd user service can start before the network is ready.
# Deepgram attempts its connection only once at startup, so a boot-time race
# permanently disables live transcription until a manual restart. The Envoy
# webhook client opens no connection at startup and retries failed posts, so a
# relay outage must not delay recording.
set -u

readonly TIMEOUT_SECONDS="${GHOST_WISPR_NETWAIT_TIMEOUT:-60}"
readonly DEEPGRAM_HOST="api.deepgram.com"
readonly DEEPGRAM_PORT=443

reachable() { # host port
  timeout 3 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null
}

deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
until reachable "$DEEPGRAM_HOST" "$DEEPGRAM_PORT"; do
  if (( $(date +%s) >= deadline )); then
    echo "ghost-wispr: Deepgram not reachable after ${TIMEOUT_SECONDS}s" >&2
    exit 1
  fi
  echo "ghost-wispr: waiting for Deepgram..." >&2
  sleep 2
done
echo "ghost-wispr: network dependency ready" >&2
