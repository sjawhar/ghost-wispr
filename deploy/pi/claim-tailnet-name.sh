#!/usr/bin/env bash
# Give the recorder's tagged Tailscale device the name ghost-wispr (docs/pi-setup.md, "Provision").
#
#   secret-run --register --wait 12h --reason TEXT TAILSCALE_API_KEY -- deploy/pi/claim-tailnet-name.sh
#
# The tagged join makes a new device. While the temporary device the operator joined as their own
# still holds the name, Tailscale names the tagged one ghost-wispr-1, and the recorder is not at its
# tailnet name (PI_HOST in deploy/pi/files/pi.env). This script removes that temporary device and
# renames the tagged one. It removes a device only when that device is the one device other than
# the tagged one that holds the name, has an id that is a string of digits, has the hostname
# ghost-wispr, has a user owner and no tag, and is not connected. It refuses any other holder, or
# more than one, before it changes anything. It prints device ids and names only. TAILSCALE_API_KEY
# comes from the environment and reaches curl on stdin (curl -K -), never as an argument, and goes
# only to the Tailscale API host below.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
# The office Pi's deployment settings (PI_HOST); GHOST_WISPR_PI_ENV names another file, as the
# tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || { printf 'claim-tailnet-name: cannot read the Pi deployment settings in %s\n' "$pi_env" >&2; exit 1; }
readonly API=https://api.tailscale.com/api/v2
readonly HOSTNAME_WANTED=ghost-wispr
readonly NAME_WANTED=$PI_HOST
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
# Ids are checked in jq with \A and \z, not ^ and $: jq's $ also matches before a final newline,
# and $(…) drops that newline, so an id "222\n" would pass as 222.
jq -e '.[0].id | type == "string" and test("\\A[0-9]+\\z")' >/dev/null <<<"$tagged" \
  || fail "the tagged device has an id that is not a string of digits: $(jq -c '.[0].id' <<<"$tagged"); nothing was changed"
tagged_id=$(jq -r '.[0].id' <<<"$tagged")
tagged_name=$(jq -r '.[0].name' <<<"$tagged")
if [[ $tagged_name == "$NAME_WANTED" ]]; then
  log "device $tagged_id is $NAME_WANTED already; nothing to do"
  exit 0
fi

holders=$(jq -c --arg name "$NAME_WANTED" --arg id "$tagged_id" \
  '[.devices[] | select(.name == $name and .id != $id)]' <<<"$devices")
# The holder's id is checked as the tagged device's id is, before any DELETE.
if ! jq -e --arg host "$HOSTNAME_WANTED" \
  'length <= 1 and all(.[]; (.id | type == "string" and test("\\A[0-9]+\\z")) and .hostname == $host and .tags == null and (.user // "") != "" and .connectedToControl == false)' \
  >/dev/null <<<"$holders"; then
  fail "the name $NAME_WANTED is held by a device that is not the temporary operator-owned one (tagged, connected, another host, an id that is not a string of digits, or more than one holder): $(jq -c '[.[] | {id, name, hostname, user, tags, connectedToControl}]' <<<"$holders"); nothing was changed"
fi
holder_id=$(jq -r '.[0].id // empty' <<<"$holders")
if [[ -n $holder_id ]]; then
  log "removing the temporary operator-owned device $holder_id, which holds $NAME_WANTED"
  api DELETE "/device/$holder_id" >/dev/null
fi

log "renaming device $tagged_id from $tagged_name to $HOSTNAME_WANTED"
api POST "/device/$tagged_id/name" "{\"name\":\"$HOSTNAME_WANTED\"}" >/dev/null
renamed=$(api GET "/device/$tagged_id")
[[ $(jq -r '.name' <<<"$renamed") == "$NAME_WANTED" ]] \
  || fail "device $tagged_id is named $(jq -r '.name' <<<"$renamed") after the rename, not $NAME_WANTED"
log "device $tagged_id is $NAME_WANTED"
