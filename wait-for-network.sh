#!/usr/bin/env bash
# Block until Ghost Wispr's network dependencies are reachable before the
# service starts.
#
# On a reboot the systemd *user* service can come up before Tailscale / the
# network is ready. Deepgram (live transcription) and the NATS broker (Envoy
# summary notifications) attempt their connection only once at startup, so a
# boot-time race permanently disables those features until a manual restart.
# Gating startup on real connectivity avoids that.
#
# Checks Deepgram (proves DNS + internet) and, when configured, the NATS broker
# from nats_url (proves Tailscale connectivity). Exits 0 as soon as the
# dependencies respond; exits non-zero after the timeout so systemd
# (Restart=always, StartLimitIntervalSec=0) retries until the network is up.
set -u

readonly TIMEOUT_SECONDS="${GHOST_WISPR_NETWAIT_TIMEOUT:-60}"
readonly DEEPGRAM_HOST="api.deepgram.com"
readonly DEEPGRAM_PORT=443

# Config lives next to this script in the deploy dir.
config_file="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)/ghost-wispr.yaml"

# First host:port from e.g. nats_url: "nats://host1:4222,nats://host2:4222".
nats_endpoint=""
if [[ -f "$config_file" ]]; then
  nats_endpoint=$(sed -nE 's#^nats_url:[[:space:]]*"?nats://([^,"[:space:]]+).*#\1#p' "$config_file" | head -n1)
fi

reachable() { # host port
  timeout 3 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null
}

deps_ready() {
  reachable "$DEEPGRAM_HOST" "$DEEPGRAM_PORT" || return 1
  if [[ -n "$nats_endpoint" ]]; then
    reachable "${nats_endpoint%:*}" "${nats_endpoint##*:}" || return 1
  fi
}

deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
until deps_ready; do
  if (( $(date +%s) >= deadline )); then
    echo "ghost-wispr: network deps not reachable after ${TIMEOUT_SECONDS}s (deepgram + ${nats_endpoint:-no nats configured})" >&2
    exit 1
  fi
  echo "ghost-wispr: waiting for network deps (deepgram${nats_endpoint:+ + nats})..." >&2
  sleep 2
done
echo "ghost-wispr: network deps ready" >&2
