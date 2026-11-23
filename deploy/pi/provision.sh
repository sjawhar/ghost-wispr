#!/usr/bin/env bash
# Provision the Ghost Wispr Raspberry Pi from a release binary and a credential bundle on stdin.
set -euo pipefail

# GHOST_WISPR_DEPLOY_DIR, GHOST_WISPR_IDENTITY_KEY, GHOST_WISPR_JOIN_DIR and GHOST_WISPR_PI_ENV exist
# for provision_run_test.sh, as update.sh's GHOST_WISPR_DEPLOY_DIR does for update_test.sh.
readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly JOIN_DIR="${GHOST_WISPR_JOIN_DIR:-/var/lib/ghost-wispr-join}"
readonly JOIN_ROUTE_CHECK="$JOIN_DIR/route-overlap-check"
readonly JOIN_TOKEN="$JOIN_DIR/token"
readonly IDENTITY_KEY="${GHOST_WISPR_IDENTITY_KEY:-/home/ghost-wispr/.config/ghost-wispr-identity/key.pem}"
readonly SERVICE_USER=ghost-wispr
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "$script_dir/../.." && pwd)"
readonly repo_root
readonly CONFIG_TEMPLATE="$repo_root/deploy/pi/files/ghost-wispr.yaml"
# The .env variables Ghost Wispr no longer reads, each of which a start warns about: the Envoy
# webhook signing key an earlier provisioner wrote, and the variables of the settings that providers
# replaced (internal/config, removedEnvironment), such as the Vertex embedding location an earlier
# provisioner wrote.
readonly REMOVED_ENV_VARIABLES='ENVOY_WEBHOOK_SECRET|GCP_PROJECT|GCP_LOCATION|GENAI_BACKEND|EMBEDDING_BACKEND|EMBEDDING_LOCATION|DEEPGRAM_MODEL|BATCH_TRANSCRIPTION_PROVIDER|TTS_PROVIDER|TTS_API_KEY|VOICE_TTS_ENGINE|VOICE_TTS_MODEL|VOICE_TTS_VOICE|VOICE_TTS_LOCATION'
# The .env variables that would override the template's Envoy settings, envoy_enabled, envoy_hub_url
# and concierge.envoy_url: each variable internal/config's applyEnvOverrides reads for an Envoy
# setting. A .env line overrides the YAML, and --models keeps .env, so such a line would change what
# the template says. deploy/pi/rollout.sh install holds the same check, so that it stops before it
# changes anything.
readonly ENVOY_OVERRIDE_VARIABLES='GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_HUB_URL|GHOST_WISPR_CONCIERGE_ENVOY_URL'

fail() {
  printf 'ghost-wispr provision: %s\n' "$*" >&2
  exit 1
}

# envoy_override_names prints, one a line, each variable of ENVOY_OVERRIDE_VARIABLES that a line of
# the .env file ENV_FILE could set: a line that, past leading space and an export, starts with its
# name and =. A line ends at a carriage return as at a line feed, as systemd's EnvironmentFile=
# parser reads one (NEWLINE "\n\r", src/basic/env-file.c), so tr turns each CR into a line feed
# first. A match inside a multi-line value counts too. It reads no value, and fails when it cannot
# read ENV_FILE.
envoy_override_names() {
  local found status=0
  [[ -r $1 ]] || return 1
  found=$(tr '\r' '\n' <"$1" | grep -oE "^[[:space:]]*(export[[:space:]]+)?($ENVOY_OVERRIDE_VARIABLES)[[:space:]]*=") || status=$?
  ((status <= 1)) || return 1
  [[ -z $found ]] || grep -oE "$ENVOY_OVERRIDE_VARIABLES" <<<"$found" | sort -u
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

# render_models_config writes the template with the shared drive: it also syncs to the drive and
# turns GC on. GC removes only local audio that Drive holds a copy of. The full provisioner installs
# the template as it is when the Pi has no shared drive yet: Drive sync and GC stay off.
render_models_config() {
  local template=$1 drive_id=$2
  valid_drive_id "$drive_id" || return 1
  [[ -f $template ]] || return 1
  awk '!/^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):/' "$template" || return 1
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

# render_models_env keeps the deployed .env but the lines of variables Ghost Wispr no longer reads,
# so none is left on disk to warn at each start.
render_models_env() {
  local env_file=$1
  awk -v removed="^GHOST_WISPR_($REMOVED_ENV_VARIABLES)=" '$0 !~ removed' "$env_file"
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

  local service_uid runtime_dir
  service_uid=$(id -u "$SERVICE_USER")
  runtime_dir="/run/user/$service_uid"
  [[ -d "$runtime_dir" ]] || fail "$runtime_dir is unavailable; log in as $SERVICE_USER before provisioning"
  [[ -f "$DEPLOY_DIR/ghost-wispr.yaml" && -f "$DEPLOY_DIR/.env" ]] \
    || fail "missing $DEPLOY_DIR/ghost-wispr.yaml or $DEPLOY_DIR/.env; run the full provisioner first"
  local envoy_override
  envoy_override=$(envoy_override_names "$DEPLOY_DIR/.env") || fail "could not read $DEPLOY_DIR/.env"
  [[ -z $envoy_override ]] \
    || fail "$DEPLOY_DIR/.env can set $(paste -sd ' ' - <<<"$envoy_override"), which would override the template's envoy_enabled, envoy_hub_url or concierge.envoy_url: nothing was changed; remove each such line and run --models again"

  # Global, not local: the EXIT trap runs after this function returns.
  staging_dir=$(mktemp -d -p "$runtime_dir" ghost-wispr-models.XXXXXX)
  chmod 0700 "$staging_dir"
  trap 'rm -rf -- "$staging_dir"' EXIT
  render_models_config "$CONFIG_TEMPLATE" "$drive_id" >"$staging_dir/ghost-wispr.yaml" \
    || fail "could not render the models configuration from $CONFIG_TEMPLATE"
  render_models_env "$DEPLOY_DIR/.env" >"$staging_dir/env" || fail "could not read $DEPLOY_DIR/.env"
  printf 'ghost-wispr provision: --models replaces settings saved in the UI with the approved models configuration.\n'
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 "$staging_dir/env" "$DEPLOY_DIR/.env"
  sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0640 "$staging_dir/ghost-wispr.yaml" "$DEPLOY_DIR/ghost-wispr.yaml"
  run_user_systemctl restart ghost-wispr.service
  printf 'ghost-wispr provision: installed models configuration and restarted ghost-wispr.service\n'
}

# route_overlap_check_script prints the route check, a POSIX sh program. It compares each IPv4
# network of the Pi (`ip -4 -o addr`, with tailscale0 left out) with each peer's PrimaryRoutes in
# `tailscale status --json` (IPv6 routes left out), because an accepted route that overlaps a network
# of the Pi would take the Pi's own traffic for that network. It exits 0 when it compared at least
# one network and no route overlaps one, and prints its one line on stdout only then; 1 when a route
# overlaps, naming each overlap on stderr; and 2 when it cannot compare, naming the step that failed.
# Two callers run it: the detached join, from $JOIN_ROUTE_CHECK, before it accepts the routes, and
# docs/pi-setup.md's "Accept the tailnet's subnet routes", on the Pi over ssh. It runs under dash,
# with no pipefail, so it checks each command's status itself. Its whole body is one function that
# its last line calls, so a program cut short runs nothing.
route_overlap_check_script() {
  cat <<'CHECK'
# Ghost Wispr's route check, printed by route_overlap_check_script in deploy/pi/provision.sh.
route_overlap_check() {
  not_accepting="ghost-wispr routes: not accepting the tailnet's subnet routes: an accepted route would take the Pi's own traffic for that network"
  cannot='ghost-wispr routes: cannot compare the routes with the networks of the Pi'
  newline='
'
  cannot_compare() { printf '%s\n' "$cannot: $1" "$not_accepting" >&2; exit 2; }
  networks=$(ip -4 -o addr) || cannot_compare "ip -4 -o addr failed (exit $?)"
  status=$(tailscale status --json) || cannot_compare "tailscale status --json failed (exit $?)"
  [ -n "$status" ] || cannot_compare "tailscale status --json printed nothing"
  # The status of a large tailnet can pass the 128 KiB limit for one argument, so jq reads it on
  # stdin, from the printf builtin.
  # jq prints the exit code on its first line, then the lines to print. Where a text from ip or
  # tailscale enters a value that a line prints, clean replaces each control or format character in
  # it with ?: the owner of a peer sets its HostName, a terminal runs the escape sequences in a line,
  # and a format character such as U+202E changes the order in which a terminal shows the text after
  # it. prefix and the tailscale0 filter read the raw texts.
  verdict=$(printf '%s\n' "$status" | jq -r -s --arg ip_lines "$networks" --arg cannot "$cannot" '
    def prefix:
      if test("^[0-9]{1,3}([.][0-9]{1,3}){3}/[0-9]{1,2}$") then
        split("/") as [$address, $bits]
        | ($address | split(".") | map(tonumber)) as $octets
        | ($bits | tonumber) as $bits
        | if all($octets[]; . <= 255) and $bits <= 32
          then {bits: $bits, number: ($octets[0] * 16777216 + $octets[1] * 65536 + $octets[2] * 256 + $octets[3])}
          else null end
      else null end;
    def size($bits): reduce range(32 - $bits) as $bit (1; . * 2);
    def overlap($a; $b): size([$a.bits, $b.bits] | min) as $size
      | ($a.number / $size | floor) == ($b.number / $size | floor);
    def refuse($step): "2\n\($cannot): \($step)";
    def clean: tostring | gsub("[[:cntrl:]\\p{Cf}]"; "?");
    [$ip_lines | split("\n")[] | select(test("[^ \t]"))
      | {line: clean, fields: [splits("[ \t]+") | select(. != "")]}
      | select(.fields[1] != "tailscale0")
      | . + {name: (.fields[1] | clean), address: (.fields[3] | clean), prefix: (.fields[3] // "" | prefix)}] as $networks
    | ($networks | map(select(.prefix == null)) | first) as $unread
    | if $unread != null then refuse("ip -4 -o addr gives no IPv4 address/prefix in field 4 of: \($unread.line)")
      elif ($networks | map(select(.name != "lo")) | length) == 0 then
        refuse("ip -4 -o addr gives no network other than lo and tailscale0")
      elif length != 1 then refuse("tailscale status --json gave \(length) JSON values, not one")
      elif .[0].BackendState != "Running" then
        refuse("BackendState is \(.[0].BackendState | clean), not Running")
      else
        [.[0].Peer // {} | .[] | (.HostName // "a peer with no HostName") as $host
          | .PrimaryRoutes // [] | .[] | select(contains(":") | not)
          | {route: clean, host: ($host | clean), prefix: prefix}] as $routes
        | ($routes | map(select(.prefix == null)) | first) as $unknown
        | if $unknown != null then refuse("the route \($unknown.route) of \($unknown.host) is no IPv4 or IPv6 prefix")
          else
            [$networks[] as $network | $routes[] as $route | select(overlap($network.prefix; $route.prefix))
              | "ghost-wispr routes: \($network.name) \($network.address) overlaps \($route.route) of \($route.host)"]
            | if length > 0 then "1\n" + join("\n")
              else "0\nghost-wispr routes: no route of a peer overlaps a network of the Pi: "
                + ($networks | map("\(.name) \(.address)") | join(", ")) + "; routes compared: "
                + (if $routes == [] then "none" else $routes | map("\(.route) of \(.host)") | join(", ") end)
              end
          end
      end
  ') || cannot_compare "jq could not read tailscale status --json (exit $?)"
  case $verdict in
    [012]"$newline"?*) ;;
    *) cannot_compare "jq gave no verdict" ;;
  esac
  code=${verdict%%"$newline"*}
  lines=${verdict#*"$newline"}
  if [ "$code" = 0 ]; then
    printf '%s\n' "$lines"
    exit 0
  fi
  printf '%s\n' "$lines" "$not_accepting" >&2
  exit "$code"
}
route_overlap_check
CHECK
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
# The office Pi's deployment settings, which provision-pi.sh copies with the rest of deploy/pi; the
# models step above needs none of them.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || fail "cannot read the Pi's deployment settings in $pi_env"
readonly TAILSCALE_HOSTNAME="$PI_HOST."

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
# The bundle holds no Envoy credential: the recorder publishes to the Envoy hub over the tailnet
# with none, so a bundle that carries an Envoy file is refused.
readonly required_entries=(
  device.crt
  google-credentials.json
  certificate_config.json
  tailscale-client-id
  tailscale-subject
  deepgram-key
)
for entry in "${archive_entries[@]}"; do
  case "$entry" in
    device.crt|google-credentials.json|certificate_config.json|tailscale-client-id|tailscale-subject|deepgram-key) ;;
    # The unit's own no-scope Dispatch token, for the meeting voice's Concierge; optional.
    dispatch-token) ;;
    *) fail "credential bundle contains unexpected path: $entry" ;;
  esac
done

tar --extract --file="$archive" --directory="$staging_dir" --no-same-owner --no-same-permissions
for entry in "${required_entries[@]}"; do
  [[ -f "$staging_dir/$entry" && ! -L "$staging_dir/$entry" ]] || fail "credential bundle is missing regular file: $entry"
done

tailscale_client_id=$(require_single_line_file tailscale-client-id)
readonly tailscale_client_id
tailscale_subject=$(require_single_line_file tailscale-subject)
readonly tailscale_subject
deepgram_key=$(require_single_line_file deepgram-key)
readonly deepgram_key
dispatch_token=""
if [[ -e "$staging_dir/dispatch-token" || -L "$staging_dir/dispatch-token" ]]; then
  [[ -f "$staging_dir/dispatch-token" && ! -L "$staging_dir/dispatch-token" ]] || fail "dispatch-token must be a regular file"
  dispatch_token=$(require_single_line_file dispatch-token)
fi
readonly dispatch_token
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
  render_models_config "$CONFIG_TEMPLATE" "$deployed_drive" >"$staging_dir/ghost-wispr.yaml" \
    || fail "could not render $CONFIG_TEMPLATE"
else
  cp -- "$CONFIG_TEMPLATE" "$staging_dir/ghost-wispr.yaml" || fail "could not read $CONFIG_TEMPLATE"
fi
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0640 "$staging_dir/ghost-wispr.yaml" "$DEPLOY_DIR/ghost-wispr.yaml"
{
  printf '%s\n' 'GHOST_WISPR_ADDR=127.0.0.1:8080'
  printf '%s\n' 'GOOGLE_API_USE_CLIENT_CERTIFICATE=false'
  printf 'GHOST_WISPR_DEEPGRAM_API_KEY=%s\n' "$deepgram_key"
  if [[ -n "$dispatch_token" ]]; then
    printf 'GHOST_WISPR_DISPATCH_TOKEN=%s\n' "$dispatch_token"
  fi
} | sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0600 /dev/stdin "$DEPLOY_DIR/.env"

sudo install -m 0644 "$repo_root/deploy/pi/files/99-ghost-wispr.rules" /etc/udev/rules.d/99-ghost-wispr.rules
sudo install -D -m 0644 "$repo_root/deploy/pi/files/logind-power-key.conf" /etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf
sudo udevadm control --reload
sudo udevadm trigger
sudo systemctl restart systemd-logind

# plugdev opens the blink(1), input the power button and the Jabra's mute button and light, and
# audio the microphone and speaker: a lingering user service has no seat, so no seat rule gives it
# the ALSA devices.
for group in plugdev input audio; do
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
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$repo_root/deploy/pi/files/wait-for-time-sync.sh" "$DEPLOY_DIR/wait-for-time-sync.sh"
sudo install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$repo_root/deploy/pi/update.sh" "$DEPLOY_DIR/update.sh"

if [[ ${groups_changed:-false} == true ]]; then
  sudo systemctl restart "user@$service_uid.service"
fi
sudo loginctl enable-linger "$SERVICE_USER"
sudo systemctl start "user@$service_uid.service"
run_user_systemctl daemon-reload
run_user_systemctl enable ghost-wispr.service
run_user_systemctl enable --now ghost-wispr-update.timer
# update.sh runs systemctl --user, which needs the user's runtime directory.
XDG_RUNTIME_DIR="$runtime_dir" "$DEPLOY_DIR/update.sh"
# The recorder reads its configuration only when it starts, and update.sh restarts nothing when the
# Pi already runs the newest release: restart a running recorder so it reads the configuration
# installed above. The restart follows update.sh, which fails when the recorder runs but does not
# answer /api/status yet. try-restart starts no recorder that is stopped, as on a first provision.
run_user_systemctl try-restart ghost-wispr.service
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
route_overlap_check_script | sudo install -o root -g root -m 0700 /dev/stdin "$JOIN_ROUTE_CHECK"
join_unit="ghost-wispr-tailnet-join-$(date +%s)"
# The join brings the node up with the tailnet's subnet routes off, `--accept-routes=false`:
# `tailscale up` refuses to leave out a setting that is not the default (tailscale v1.104.1,
# cmd/tailscale/cli/up.go, checkForAccidentalSettingReverts), and a re-join must not accept a route
# that now overlaps a network of the Pi. So only the route check turns the routes on, through
# `tailscale set --accept-routes` once it passed. The check runs even when Serve failed, as the
# routes do not depend on Serve, and the unit ends failed when up, serve or set failed or the check
# refused. Serve is set above for a node that is joined already, and again after the
# re-authentication: the join can start a new login profile, which would not carry the setting over.
join_command="tailscale up --force-reauth --client-id='$tailscale_client_id?ephemeral=false&preauthorized=true'"
join_command+=" --id-token=file:$JOIN_TOKEN --advertise-tags=tag:ghost-wispr --hostname=ghost-wispr --ssh --accept-routes=false;"
join_command+=" rc=\$?; rm -f $JOIN_TOKEN;"
join_command+=" if [ \$rc -eq 0 ]; then"
join_command+=" tailscale serve --bg --https 443 http://127.0.0.1:8080 || rc=\$?;"
join_command+=" if /bin/sh $JOIN_ROUTE_CHECK; then tailscale set --accept-routes || rc=\$?; else rc=\$?; fi;"
join_command+=" fi; rm -f $JOIN_ROUTE_CHECK; exit \$rc"
printf 'ghost-wispr provision: starting the tagged tailnet join as %s\n' "$join_unit"
# systemd-run(1): the service manager expands ${NAME} and $$ in the command's arguments unless
# --expand-environment=no, which systemd 254 added (Raspberry Pi OS Trixie has 257). The join
# command is shell code for the unit's /bin/sh, which must get it as written.
sudo systemd-run --expand-environment=no --unit="$join_unit" /bin/sh -c "$join_command"
printf 'ghost-wispr provision: reconnect with Tailscale SSH, then check tailscale status and sudo journalctl -u %s.\n' "$join_unit"
