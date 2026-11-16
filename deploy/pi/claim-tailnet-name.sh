#!/usr/bin/env bash
# Give the recorder's tagged Tailscale device the name ghost-wispr (docs/pi-setup.md, "Provision").
#
#   secret-run --register --wait 12h --reason TEXT TAILSCALE_API_KEY -- deploy/pi/claim-tailnet-name.sh
#
# The tagged join makes a new device. While the temporary device the operator joined as their own
# still holds the name, Tailscale names the tagged one ghost-wispr-1, and the recorder is not at
# https://ghost-wispr.tail12ec1c.ts.net. This script removes that temporary device and renames the
# tagged one. It removes a device only when that device holds the name, has the hostname
# ghost-wispr, has a user owner and no tag, and is not connected; any other holder is refused and
# nothing changes. It prints device ids and names only. TAILSCALE_API_KEY comes from the
# environment and reaches curl on stdin (curl -K -), never as an argument.
set -euo pipefail

readonly API="${GHOST_WISPR_TAILSCALE_API:-https://api.tailscale.com/api/v2}"
readonly HOSTNAME_WANTED=ghost-wispr
readonly NAME_WANTED=ghost-wispr.tail12ec1c.ts.net
readonly TAG=tag:ghost-wispr

fail() {
  printf 'claim-tailnet-name: %s\n' "$*" >&2
  exit 1
}
log() { printf 'claim-tailnet-name: %s\n' "$*"; }

[[ -n ${TAILSCALE_API_KEY:-} ]] || fail "TAILSCALE_API_KEY is not set; run this under secret-run TAILSCALE_API_KEY"
for command in curl jq; do
  command -v "$command" >/dev/null || fail "missing required command: $command"
done

# api METHOD PATH [BODY] calls the Tailscale API with the key on stdin and prints the response
# body. It fails, naming the call and its HTTP status, on anything but 2xx.
api() {
  local method=$1 path=$2 body=${3:-} response status
  local args=(-K - -sS -X "$method" -w '\n%{http_code}')
  [[ -z $body ]] || args+=(-H 'Content-Type: application/json' --data "$body")
  response=$(printf 'header = "Authorization: Bearer %s"\n' "$TAILSCALE_API_KEY" | curl "${args[@]}" "$API$path") \
    || fail "$method $path did not answer"
  status=${response##*$'\n'}
  response=${response%$'\n'*}
  [[ $status =~ ^2[0-9][0-9]$ ]] || fail "$method $path answered HTTP $status"
  printf '%s' "$response"
}

devices=$(api GET '/tailnet/-/devices?fields=all')
jq -e '.devices | type == "array"' >/dev/null <<<"$devices" || fail "the device list is not a list"

tagged=$(jq -c --arg host "$HOSTNAME_WANTED" --arg tag "$TAG" \
  '[.devices[] | select(.hostname == $host and .tags == [$tag])]' <<<"$devices")
case $(jq length <<<"$tagged") in
  1) ;;
  0) fail "no device with hostname $HOSTNAME_WANTED holds $TAG; run provision.sh's tagged join first" ;;
  *) fail "more than one device with hostname $HOSTNAME_WANTED holds $TAG: $(jq -c '[.[] | {id, name}]' <<<"$tagged"); remove the stale ones by hand" ;;
esac
tagged_id=$(jq -r '.[0].id' <<<"$tagged")
tagged_name=$(jq -r '.[0].name' <<<"$tagged")
[[ $tagged_id =~ ^[0-9]+$ ]] || fail "the tagged device has no numeric id"
if [[ $tagged_name == "$NAME_WANTED" ]]; then
  log "device $tagged_id is $NAME_WANTED already; nothing to do"
  exit 0
fi

holders=$(jq -c --arg name "$NAME_WANTED" --arg id "$tagged_id" \
  '[.devices[] | select(.name == $name and .id != $id)]' <<<"$devices")
if ! jq -e --arg host "$HOSTNAME_WANTED" \
  'all(.[]; .hostname == $host and .tags == null and (.user // "") != "" and .connectedToControl == false)' \
  >/dev/null <<<"$holders"; then
  fail "the name $NAME_WANTED is held by a device that is not the temporary operator-owned one (tagged, connected, or another host): $(jq -c '[.[] | {id, name, hostname, user, tags, connectedToControl}]' <<<"$holders"); nothing was changed"
fi
for holder_id in $(jq -r '.[].id' <<<"$holders"); do
  [[ $holder_id =~ ^[0-9]+$ ]] || fail "a device holding $NAME_WANTED has no numeric id"
  log "removing the temporary operator-owned device $holder_id, which holds $NAME_WANTED"
  api DELETE "/device/$holder_id" >/dev/null
done

log "renaming device $tagged_id from $tagged_name to $HOSTNAME_WANTED"
api POST "/device/$tagged_id/name" "{\"name\":\"$HOSTNAME_WANTED\"}" >/dev/null
renamed=$(api GET "/device/$tagged_id")
[[ $(jq -r '.name' <<<"$renamed") == "$NAME_WANTED" ]] \
  || fail "device $tagged_id is named $(jq -r '.name' <<<"$renamed") after the rename, not $NAME_WANTED"
log "device $tagged_id is $NAME_WANTED"
