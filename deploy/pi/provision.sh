#!/usr/bin/env bash
# Provision the Ghost Wispr Raspberry Pi from a release binary and a credential bundle on stdin.
set -euo pipefail

readonly DEPLOY_DIR=/opt/ghost-wispr
readonly JOIN_DIR=/var/lib/ghost-wispr-join
readonly JOIN_TOKEN="$JOIN_DIR/token"
readonly IDENTITY_KEY=/home/ghost-wispr/.config/ghost-wispr-identity/key.pem
readonly SERVICE_USER=ghost-wispr
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "$script_dir/../.." && pwd)"
readonly repo_root
readonly TAILSCALE_HOSTNAME=ghost-wispr.tail12ec1c.ts.net.
readonly CONFIG_TEMPLATE="$repo_root/deploy/pi/files/ghost-wispr.yaml"
readonly EMBEDDING_LOCATION_SETTING=GHOST_WISPR_EMBEDDING_LOCATION=global

fail() {
  printf 'ghost-wispr provision: %s\n' "$*" >&2
  exit 1
}

require_single_line_file() {
  local name=$1
  local value
  value=$(<"$staging_dir/$name")
  [[ -n "$value" && "$value" != *$'\n'* && "$value" != *$'\r'* ]] || fail "$name must contain one non-empty line"
  printf '%s' "$value"
}
valid_drive_id() {
  [[ $1 =~ ^[A-Za-z0-9_-]+$ ]]
}

valid_relay_url() {
  [[ $1 =~ ^https://[^[:space:]]+/webhook/ghostwispr$ ]]
}

# render_base_config writes the template with the Envoy webhook URL: Drive sync and GC stay off.
render_base_config() {
  local template=$1 relay_url=$2 escaped_relay_url
  valid_relay_url "$relay_url" || return 1
  escaped_relay_url=${relay_url//\\/\\\\}
  escaped_relay_url=${escaped_relay_url//&/\\&}
  escaped_relay_url=${escaped_relay_url//|/\\|}
  sed "s|__ENVOY_WEBHOOK_URL__|$escaped_relay_url|" "$template"
}

# render_models_config also syncs to the shared drive and turns GC on. GC
# removes audio only after Drive confirms a durable copy.
render_models_config() {
  local template=$1 relay_url=$2 drive_id=$3
  valid_drive_id "$drive_id" || return 1
  render_base_config "$template" "$relay_url" \
    | awk '!/^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):/' || return 1
  printf 'gdrive_folder_id: "%s"\n' "$drive_id"
  printf '%s\n' 'gdrive_sync_enabled: true' 'gc_enabled: true'
}

# deployed_drive_id reads the shared-drive ID written by render_models_config
# or yaml.v3. It prints nothing when the configuration has none, including the
# gdrive_folder_id: "" that every config API save writes without a folder, and
# fails on any other gdrive_folder_id line.
deployed_drive_id() {
  local config=$1 line status=0
  local quoted_pattern='^gdrive_folder_id: "([A-Za-z0-9_-]+)"$'
  local unquoted_pattern='^gdrive_folder_id: ([A-Za-z0-9_-]+)$'
  line=$(grep '^gdrive_folder_id:' "$config") || status=$?
  (( status != 1 )) || return 0
  (( status == 0 )) || return 1
  [[ $line != 'gdrive_folder_id: ""' ]] || return 0
  if [[ $line =~ $quoted_pattern || $line =~ $unquoted_pattern ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
    return
  fi
  return 1
}

deployed_relay_url() {
  local config=$1 line url pattern='^envoy_webhook_url: (.+)$'
  line=$(grep '^envoy_webhook_url:' "$config") || return 1
  [[ $line =~ $pattern ]] || return 1
  url=${BASH_REMATCH[1]}
  valid_relay_url "$url" || return 1
  printf '%s' "$url"
}

render_models_env() {
  local env_file=$1
  awk '!/^GHOST_WISPR_EMBEDDING_LOCATION=/' "$env_file" || return 1
  printf '%s\n' "$EMBEDDING_LOCATION_SETTING"
}

# run_user_systemctl reads runtime_dir from its caller: the global, or provision_models' local.
run_user_systemctl() {
  sudo -u "$SERVICE_USER" XDG_RUNTIME_DIR="$runtime_dir" systemctl --user "$@"
}

provision_models() {
  [[ $# -eq 1 ]] || fail "usage: deploy/pi/provision.sh --models <shared-drive-id>"
  local drive_id=$1
  valid_drive_id "$drive_id" \
    || fail "shared-drive ID must be non-empty and contain only letters, numbers, hyphens, and underscores"
  if [[ $(id -un) != "$SERVICE_USER" ]]; then
    fail "run as $SERVICE_USER"
  fi

  local service_uid runtime_dir relay_url
  service_uid=$(id -u "$SERVICE_USER")
  runtime_dir="/run/user/$service_uid"
  [[ -d "$runtime_dir" ]] || fail "$runtime_dir is unavailable; log in as $SERVICE_USER before provisioning"
  [[ -f "$DEPLOY_DIR/ghost-wispr.yaml" && -f "$DEPLOY_DIR/.env" ]] \
    || fail "missing $DEPLOY_DIR/ghost-wispr.yaml or $DEPLOY_DIR/.env; run the full provisioner first"
  relay_url=$(deployed_relay_url "$DEPLOY_DIR/ghost-wispr.yaml") \
    || fail "$DEPLOY_DIR/ghost-wispr.yaml has no valid envoy_webhook_url; run the full provisioner first"

  # Global, not local: the EXIT trap runs after this function returns.
  staging_dir=$(mktemp -d -p "$runtime_dir" ghost-wispr-models.XXXXXX)
  chmod 0700 "$staging_dir"
  trap 'rm -rf -- "$staging_dir"' EXIT
  render_models_config "$CONFIG_TEMPLATE" "$relay_url" "$drive_id" >"$staging_dir/ghost-wispr.yaml" \
    || fail "could not render the models configuration from $CONFIG_TEMPLATE"
  render_models_env "$DEPLOY_DIR/.env" >"$staging_dir/env" || fail "could not read $DEPLOY_DIR/.env"
  printf 'ghost-wispr provision: --models replaces settings saved in the UI with the approved models configuration.\n'
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$staging_dir/env" "$DEPLOY_DIR/.env"
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0640 "$staging_dir/ghost-wispr.yaml" "$DEPLOY_DIR/ghost-wispr.yaml"
  run_user_systemctl restart ghost-wispr.service
  printf 'ghost-wispr provision: installed models configuration and restarted ghost-wispr.service\n'
}

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi
if [[ ${1:-} == --models ]]; then
  shift
  provision_models "$@"
  exit
fi

if [[ $(id -un) != "$SERVICE_USER" ]]; then
  fail "run as $SERVICE_USER"
fi
if [[ $# -ne 0 ]]; then
  fail "usage: deploy/pi/provision.sh < credential-bundle.tar"
fi

service_uid=$(id -u "$SERVICE_USER")
readonly service_uid
readonly runtime_dir="/run/user/$service_uid"
[[ -d "$runtime_dir" ]] || fail "$runtime_dir is unavailable; log in as $SERVICE_USER before provisioning"
[[ -f "$IDENTITY_KEY" ]] || fail "missing device private key at $IDENTITY_KEY; complete the identity bootstrap first"

staging_dir=$(mktemp -d -p "$runtime_dir" ghost-wispr-provision.XXXXXX)
chmod 0700 "$staging_dir"
trap 'rm -rf -- "$staging_dir"' EXIT

archive="$staging_dir/credentials.tar"
cat >"$archive"

mapfile -t archive_entries < <(tar --list --file="$archive")
readonly required_entries=(
  device.crt
  google-credentials.json
  certificate_config.json
  tailscale-client-id
  tailscale-subject
  envoy-webhook-url
  envoy-webhook-secret
  deepgram-key
)
for entry in "${archive_entries[@]}"; do
  case "$entry" in
    device.crt|google-credentials.json|certificate_config.json|tailscale-client-id|tailscale-subject|envoy-webhook-url|envoy-webhook-secret|deepgram-key) ;;
    *) fail "credential bundle contains unexpected path: $entry" ;;
  esac
done

tar --extract --file="$archive" --directory="$staging_dir" --no-same-owner --no-same-permissions
for entry in "${required_entries[@]}"; do
  [[ -f "$staging_dir/$entry" && ! -L "$staging_dir/$entry" ]] || fail "credential bundle is missing regular file: $entry"
done

relay_url=$(require_single_line_file envoy-webhook-url)
readonly relay_url
tailscale_client_id=$(require_single_line_file tailscale-client-id)
readonly tailscale_client_id
tailscale_subject=$(require_single_line_file tailscale-subject)
readonly tailscale_subject
deepgram_key=$(require_single_line_file deepgram-key)
readonly deepgram_key
webhook_secret=$(require_single_line_file envoy-webhook-secret)
readonly webhook_secret
valid_relay_url "$relay_url" || fail "envoy-webhook-url must be an HTTPS Ghost Wispr webhook URL"
[[ -n "$tailscale_client_id" && "$tailscale_client_id" != null && "$tailscale_client_id" =~ ^[A-Za-z0-9._:-]+$ ]] || fail "tailscale-client-id is empty or invalid"
[[ "$tailscale_subject" =~ ^[0-9]+$ ]] || fail "tailscale-subject must be the service account unique ID"

sudo apt-get update
sudo apt-get install -y --no-install-recommends libportaudio2 ffmpeg lame jq curl
if ! certificate_config_location=$(jq -r '.credential_source.certificate.certificate_config_location // empty' "$staging_dir/google-credentials.json"); then
  fail "google-credentials.json is not valid JSON"
fi
[[ "$certificate_config_location" == /opt/ghost-wispr/certificate_config.json ]] \
  || fail "google-credentials.json must use /opt/ghost-wispr/certificate_config.json"

sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DEPLOY_DIR" "$DEPLOY_DIR/data"
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0644 "$staging_dir/device.crt" "$DEPLOY_DIR/device.crt"
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0644 "$staging_dir/google-credentials.json" "$DEPLOY_DIR/google-credentials.json"
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0644 "$staging_dir/certificate_config.json" "$DEPLOY_DIR/certificate_config.json"
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0644 "$staging_dir/tailscale-client-id" "$DEPLOY_DIR/tailscale-client-id"

deployed_drive=""
if [[ -f "$DEPLOY_DIR/ghost-wispr.yaml" ]]; then
  deployed_drive=$(deployed_drive_id "$DEPLOY_DIR/ghost-wispr.yaml") \
    || fail "$DEPLOY_DIR/ghost-wispr.yaml has an invalid gdrive_folder_id; run provision.sh --models <shared-drive-id> after fixing it"
fi
readonly deployed_drive
# A rerun keeps the shared drive provision.sh --models configured, and with it sync and GC.
if [[ -n "$deployed_drive" ]]; then
  render_models_config "$CONFIG_TEMPLATE" "$relay_url" "$deployed_drive" >"$staging_dir/ghost-wispr.yaml" \
    || fail "could not render $CONFIG_TEMPLATE"
else
  render_base_config "$CONFIG_TEMPLATE" "$relay_url" >"$staging_dir/ghost-wispr.yaml" \
    || fail "could not render $CONFIG_TEMPLATE"
fi
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0640 "$staging_dir/ghost-wispr.yaml" "$DEPLOY_DIR/ghost-wispr.yaml"
{
  printf '%s\n' 'GHOST_WISPR_ADDR=127.0.0.1:8080'
  printf '%s\n' 'GOOGLE_API_USE_CLIENT_CERTIFICATE=false'
  printf '%s\n' "$EMBEDDING_LOCATION_SETTING"
  printf 'GHOST_WISPR_DEEPGRAM_API_KEY=%s\n' "$deepgram_key"
  printf 'GHOST_WISPR_ENVOY_WEBHOOK_SECRET=%s\n' "$webhook_secret"
} | sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 /dev/stdin "$DEPLOY_DIR/.env"

sudo install -m 0644 "$repo_root/deploy/pi/files/99-ghost-wispr.rules" /etc/udev/rules.d/99-ghost-wispr.rules
sudo install -D -m 0644 "$repo_root/deploy/pi/files/logind-power-key.conf" /etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf
sudo udevadm control --reload
sudo udevadm trigger
sudo systemctl restart systemd-logind

for group in plugdev input; do
  if ! id -nG "$SERVICE_USER" | tr ' ' '\n' | grep -Fxq "$group"; then
    sudo usermod -aG "$group" "$SERVICE_USER"
    groups_changed=true
  fi
done

readonly user_unit_dir="/home/$SERVICE_USER/.config/systemd/user"
sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$user_unit_dir"
for unit in ghost-wispr.service ghost-wispr-update.service ghost-wispr-update.timer; do
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0644 \
    "$repo_root/deploy/pi/files/$unit" "$user_unit_dir/$unit"
done
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$repo_root/wait-for-network.sh" "$DEPLOY_DIR/wait-for-network.sh"
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$repo_root/deploy/pi/update.sh" "$DEPLOY_DIR/update.sh"

if [[ ${groups_changed:-false} == true ]]; then
  sudo systemctl restart "user@$service_uid.service"
fi
sudo loginctl enable-linger "$SERVICE_USER"
sudo systemctl start "user@$service_uid.service"
run_user_systemctl daemon-reload
run_user_systemctl enable ghost-wispr.service
run_user_systemctl enable --now ghost-wispr-update.timer
"$DEPLOY_DIR/update.sh"
sudo tailscale serve --bg --https 443 http://127.0.0.1:8080

self_status=$(tailscale status --self --json)
if jq -e --arg hostname "$TAILSCALE_HOSTNAME" '
  .Self.Tags == ["tag:ghost-wispr"] and .Self.DNSName == $hostname
' <<<"$self_status" >/dev/null; then
  printf 'ghost-wispr provision: already joined as tag:ghost-wispr at %s\n' "$TAILSCALE_HOSTNAME"
  exit 0
fi

readonly token_audience="api.tailscale.com/$tailscale_client_id"
id_token=$(env \
  GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  "$DEPLOY_DIR/ghost-wispr" google-id-token --credentials "$DEPLOY_DIR/google-credentials.json" --audience "$token_audience")
[[ "$id_token" != *$'\n'* && "$id_token" == *.*.* ]] || fail "google-id-token did not return a JWT"
jwt_payload=${id_token#*.}
jwt_payload=${jwt_payload%%.*}
jwt_payload=${jwt_payload//-/+}
jwt_payload=${jwt_payload//_/\/}
case $((${#jwt_payload} % 4)) in
  2) jwt_payload+='==' ;;
  3) jwt_payload+='=' ;;
  1) fail "google-id-token returned an invalid JWT payload" ;;
esac
if ! decoded_payload=$(printf '%s' "$jwt_payload" | base64 --decode 2>/dev/null); then
  fail "google-id-token returned an undecodable JWT payload"
fi
if ! jq -e --arg subject "$tailscale_subject" --arg audience "$token_audience" '
  .sub == $subject and (.aud == $audience or (.aud | type == "array" and index($audience)))
' <<<"$decoded_payload" >/dev/null; then
  fail "Google ID token subject or audience does not match the expected Tailscale identity"
fi

sudo install -d -o root -g root -m 0700 "$JOIN_DIR"
printf '%s' "$id_token" | sudo install -m 0600 /dev/stdin "$JOIN_TOKEN"
join_unit="ghost-wispr-tailnet-join-$(date +%s)"
join_command="tailscale up --force-reauth --client-id='$tailscale_client_id?ephemeral=false&preauthorized=true' --id-token=file:$JOIN_TOKEN --advertise-tags=tag:ghost-wispr --hostname=ghost-wispr --ssh; rc=\$?; rm -f $JOIN_TOKEN; exit \$rc"
sudo systemd-run --unit="$join_unit" /bin/sh -c "$join_command"
printf 'ghost-wispr provision: reconnect with Tailscale SSH, then check tailscale status and journalctl -u %s.\n' "$join_unit"
