#!/usr/bin/env bash
# Provision the office Pi from the operator's machine (docs/pi-setup.md, "Provision").
#
#   deploy/pi/provision-pi.sh --prepared DIR [--broker-host HOST] [--broker-key KEY]
#
#   --prepared DIR     the bundle's public files, made as docs/pi-setup.md says: device.crt,
#                      google-credentials.json, certificate_config.json, tailscale-client-id,
#                      tailscale-subject, envoy-webhook-url.
#   --broker-host HOST, --broker-key KEY
#                      passed to deploy/pi/webhook-secret.sh, which reads the webhook signing
#                      secret under one secrets-broker grant of one hour that Sami approves.
#
# The Deepgram key comes from `secrets DEEPGRAM_API_KEY` on this machine. The two secrets reach
# disk only in a mode-0700 directory under $XDG_RUNTIME_DIR, while the tar stream for provision.sh
# is made. That directory is removed as soon as provision.sh returns, whether it worked or not,
# and on any exit. No secret is put in an argument.
set -euo pipefail
umask 077

readonly PI=ghost-wispr@ghost-wispr.tail12ec1c.ts.net
readonly PREPARED_FILES=(device.crt google-credentials.json certificate_config.json tailscale-client-id
  tailscale-subject envoy-webhook-url)
readonly SECRET_FILES=(envoy-webhook-secret deepgram-key)
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "$script_dir/../.." && pwd)"
readonly repo_root

log() { printf 'provision-pi: %s\n' "$*" >&2; }
fail() {
  log "FAILED: $*"
  exit 1
}
usage() { sed -n '4,11p' "$0" | sed 's/^# \{0,1\}//'; }

prepared=
secret_options=()
while [[ $# -gt 0 ]]; do
  case $1 in
    --prepared | --broker-host | --broker-key)
      [[ $# -ge 2 && -n $2 ]] || fail "$1 needs a value"
      if [[ $1 == --prepared ]]; then
        prepared=$2
      else
        secret_options+=("$1" "$2")
      fi
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      fail "unknown argument: $1"
      ;;
  esac
done
[[ -n $prepared ]] || {
  usage >&2
  fail "--prepared is required"
}
for f in "${PREPARED_FILES[@]}"; do
  [[ -f $prepared/$f && ! -L $prepared/$f && -s $prepared/$f ]] || fail "missing $prepared/$f"
done
[[ -n ${XDG_RUNTIME_DIR:-} && -d $XDG_RUNTIME_DIR && -O $XDG_RUNTIME_DIR ]] \
  || fail "XDG_RUNTIME_DIR must name this user's runtime directory, which holds the secrets while the bundle is made"

stage=
remote_dir=
provision_log=
cleanup() {
  local rc=$?
  trap - EXIT
  [[ -z $stage ]] || rm -rf -- "$stage"
  [[ -z $provision_log ]] || rm -f -- "$provision_log"
  if [[ -n $remote_dir ]] && ! ssh -n "$PI" "rm -rf -- '$remote_dir'"; then
    log "could not remove $remote_dir on the Pi; reconnect with Tailscale SSH and run: ssh $PI \"rm -rf -- '$remote_dir'\""
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

webhook_secret=$("$script_dir/webhook-secret.sh" "${secret_options[@]}" \
  --reason "Ghost Wispr provisioning (docs/pi-setup.md): read envoy-ghostwispr-signing-secret once for the office Pi's credential bundle, sent over SSH stdin and not stored") \
  || fail "could not read the webhook signing secret; nothing went to the Pi"

log "copying deploy/pi and wait-for-network.sh to the Pi"
dir=$(ssh -n "$PI" 'umask 077 && mkdir -p ~/.local/share/ghost-wispr-provision && chmod 700 ~/.local/share/ghost-wispr-provision && mktemp -d ~/.local/share/ghost-wispr-provision/sources.XXXXXX') \
  || fail "cannot make the sources directory on the Pi"
[[ $dir =~ ^/[A-Za-z0-9._/-]+/sources\.[A-Za-z0-9]+$ ]] || fail "unexpected sources directory on the Pi: $dir"
remote_dir=$dir
# shellcheck disable=SC2029 # remote_dir is the path the Pi printed; it expands here on purpose.
tar -C "$repo_root" -cf - deploy/pi wait-for-network.sh | ssh "$PI" "tar -xf - -C '$remote_dir'" \
  || fail "cannot copy the sources to the Pi"

stage=$(mktemp -d -p "$XDG_RUNTIME_DIR" ghost-wispr-bundle.XXXXXX)
for f in "${PREPARED_FILES[@]}"; do
  cp -- "$prepared/$f" "$stage/$f"
done
printf '%s' "$webhook_secret" >"$stage/envoy-webhook-secret"
unset webhook_secret
# shellcheck disable=SC2016 # The child shell expands the key the secrets store gave it.
secrets DEEPGRAM_API_KEY -- sh -c 'printf %s "$DEEPGRAM_API_KEY" >"$1"' sh "$stage/deepgram-key"
[[ -s $stage/deepgram-key ]] || fail "DEEPGRAM_API_KEY came back empty"

log "running provision.sh on the Pi"
# provision.sh's output is kept to tell the expected drop from another one: Tailscale's
# re-authentication ends the SSH session (exit 255) only after the join has started.
provision_log=$(mktemp -p "$XDG_RUNTIME_DIR" ghost-wispr-provision-log.XXXXXX)
set +e
# shellcheck disable=SC2029 # remote_dir is the path the Pi printed; it expands here on purpose.
tar -C "$stage" -cf - "${PREPARED_FILES[@]}" "${SECRET_FILES[@]}" | ssh "$PI" "$remote_dir/deploy/pi/provision.sh" | tee "$provision_log"
provision_rc=${PIPESTATUS[1]}
set -e
rm -rf -- "$stage"
stage=
join_started=false
grep -Eq '^ghost-wispr provision: (starting the tagged tailnet join|already joined)' "$provision_log" && join_started=true
case $provision_rc in
  0) log "provision.sh finished; it started the tagged tailnet join as a detached unit" ;;
  255)
    [[ $join_started == true ]] \
      || fail "the SSH session ended (exit 255) before provision.sh started the tailnet join; read its output above and run this again"
    log "the SSH session ended (exit 255) after the tailnet join started, as it does while Tailscale re-authenticates; check the join as docs/pi-setup.md says"
    ;;
  *) fail "provision.sh failed (exit $provision_rc); read its output above" ;;
esac
