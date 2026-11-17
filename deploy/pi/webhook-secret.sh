#!/usr/bin/env bash
# Print Envoy's Ghost Wispr webhook signing secret on stdout, for a caller that puts it straight
# into a process's environment: the slice 1 acceptance driver's signed probe (docs/pi-setup.md,
# "Accept slice 1"). The recorder reads the key from Secret Manager itself.
#
#   deploy/pi/webhook-secret.sh [--broker-host HOST] [--broker-key KEY] [--reason TEXT]
#
#   --broker-host HOST make the read on HOST over SSH, for a machine whose secret-run has a
#                      secrets-broker identity when this one has none.
#   --broker-key KEY   read broker key KEY, which holds a copy of the secret. Without it the read is
#                      Secret Manager's PI_WEBHOOK_SECRET in PI_GCP_PROJECT (the Pi's deployment
#                      settings) through gcloud-broker, as the admin login GCLOUD_SAMI mints.
#   --reason TEXT      what the broker page shows Sami as the reason.
#
# The read runs under one secrets-broker grant of one hour that Sami approves on the broker page
# (secret-run --register --ttl 1h), never as a box identity: dispatch://INFRA-3599 keeps the
# secret from agent boxes, since a process that holds it can sign events Envoy takes as the
# recorder's. secret-run and gcloud-broker are Sami's dotfiles scripts. Nothing is written to disk
# and the secret is never an argument.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
# The office Pi's deployment settings; GHOST_WISPR_PI_ENV names another file, as the tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || { printf 'webhook-secret: cannot read the Pi deployment settings in %s\n' "$pi_env" >&2; exit 1; }
readonly WEBHOOK_SECRET_ID=$PI_WEBHOOK_SECRET
readonly GCP_PROJECT=$PI_GCP_PROJECT

fail() {
  printf 'webhook-secret: %s\n' "$*" >&2
  exit 1
}
usage() { sed -n '6,13p' "$0" | sed 's/^# \{0,1\}//'; }

broker_host=
broker_key=
reason="Ghost Wispr (docs/pi-setup.md): read $WEBHOOK_SECRET_ID once, into one process, not stored"
while [[ $# -gt 0 ]]; do
  case $1 in
    --broker-host | --broker-key | --reason)
      [[ $# -ge 2 && -n $2 ]] || fail "$1 needs a value"
      case $1 in
        --broker-host) broker_host=$2 ;;
        --broker-key) broker_key=$2 ;;
        --reason) reason=$2 ;;
      esac
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
[[ -z $broker_key || $broker_key =~ ^[A-Z][A-Z0-9_]*$ ]] || fail "--broker-key takes an upper-case broker key name"

if [[ -n $broker_key ]]; then
  grant_key=$broker_key
  reader=(printenv "$broker_key")
else
  grant_key=GCLOUD_SAMI
  reader=(env GCLOUD_BROKER_INNER=1 gcloud-broker -- gcloud secrets versions access latest
    "--secret=$WEBHOOK_SECRET_ID" "--project=$GCP_PROJECT")
fi
command=(secret-run --register --reason "$reason" --ttl 1h --wait 12h "$grant_key" -- "${reader[@]}")

printf 'webhook-secret: reading %s under a one-hour secrets-broker grant of %s%s; approve it on the broker page\n' \
  "$WEBHOOK_SECRET_ID" "$grant_key" "${broker_host:+ on $broker_host}" >&2
if [[ -n $broker_host ]]; then
  secret=$(ssh -n "$broker_host" "$(printf '%q ' "${command[@]}")") || fail "the broker read on $broker_host failed"
else
  secret=$("${command[@]}") || fail "the broker read failed"
fi
[[ -n $secret && $secret != *[[:space:]]* ]] || fail "the broker read returned something other than one token (value not shown)"
printf '%s' "$secret"
