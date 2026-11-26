# shellcheck shell=bash
# pi-lib.sh: the functions that deploy/pi/update.sh and deploy/pi/provision.sh share: the config
# renderer, the .env filter, the override and drop-in checks, and the drift checks of the bootstrap
# files and Tailscale Serve. Each script sources the pi-lib.sh beside it, and update.sh.prev sources
# pi-lib.sh.prev, so an old script never runs on a new library. Sourcing it runs nothing. It defines
# no fail and makes no variable readonly, since each script that sources it defines its own fail and
# readonly settings. Each local variable of its functions starts with pl_: bash refuses a local
# whose name is readonly in the caller ("local: NAME: readonly variable"), and provision.sh makes
# lower-case names readonly. The checks that read ghost-wispr's user manager run systemctl --user,
# so they need the user's XDG_RUNTIME_DIR.

# The .env variables Ghost Wispr no longer reads, each of which a start warns about: the Envoy
# webhook signing key an earlier provisioner wrote, and the variables of the settings that providers
# replaced (internal/config, removedEnvironment), such as the Vertex embedding location an earlier
# provisioner wrote. Each is GHOST_WISPR_ and one of these names.
PI_LIB_REMOVED_ENV_VARIABLES='ENVOY_WEBHOOK_SECRET|GCP_PROJECT|GCP_LOCATION|GENAI_BACKEND|EMBEDDING_BACKEND|EMBEDDING_LOCATION|DEEPGRAM_MODEL|BATCH_TRANSCRIPTION_PROVIDER|TTS_PROVIDER|TTS_API_KEY|VOICE_TTS_ENGINE|VOICE_TTS_MODEL|VOICE_TTS_VOICE|VOICE_TTS_LOCATION'
# The four .env lines update.sh owns and writes first (R9).
PI_LIB_OWNED_ENV_VARIABLES='GHOST_WISPR_ADDR|GOOGLE_API_USE_CLIENT_CERTIFICATE|GHOST_WISPR_DEEPGRAM_API_KEY|GHOST_WISPR_DISPATCH_TOKEN'
# The variable that names the config file the recorder loads (internal/config, PathFromEnvironment);
# without it, the recorder loads ghost-wispr.yaml in its working directory, the deploy directory.
# Set in .env or in the user manager's environment, it would have the recorder load another file
# than the release's config.
PI_LIB_CONFIG_FILE_VARIABLE='GHOST_WISPR_CONFIG'
# The variables that would override the config's Envoy settings, envoy_enabled, envoy_hub_url and
# concierge.envoy_url: each variable internal/config's applyEnvOverrides reads for an Envoy setting.
PI_LIB_ENVOY_OVERRIDE_VARIABLES='GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_HUB_URL|GHOST_WISPR_CONCIERGE_ENVOY_URL'

valid_drive_id() {
  [[ $1 =~ ^[A-Za-z0-9_-]+$ ]]
}

# render_config_with_drive TEMPLATE DRIVE_ID prints TEMPLATE with the shared drive DRIVE_ID: every
# setting of TEMPLATE but the drive's three, then the quoted drive ID, with Drive sync and GC on.
# GC removes only local audio that Drive holds a copy of. It fails, and prints no partial config, on
# an invalid drive ID or a template it cannot read.
render_config_with_drive() {
  local pl_template=$1 pl_drive_id=$2
  valid_drive_id "$pl_drive_id" || return 1
  [[ -f $pl_template ]] || return 1
  awk '!/^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):/' "$pl_template" || return 1
  printf 'gdrive_folder_id: "%s"\n' "$pl_drive_id"
  printf '%s\n' 'gdrive_sync_enabled: true' 'gc_enabled: true'
}

# deployed_drive_id CONFIG prints the shared-drive ID of CONFIG, as render_config_with_drive or
# yaml.v3 writes it. It prints nothing when CONFIG names none, including the gdrive_folder_id: ""
# that every config API save writes without a folder, and fails on any other gdrive_folder_id
# line, or two.
deployed_drive_id() {
  local pl_config=$1 pl_line pl_status=0
  local pl_quoted='^gdrive_folder_id: "([A-Za-z0-9_-]+)"$'
  local pl_unquoted='^gdrive_folder_id: ([A-Za-z0-9_-]+)$'
  pl_line=$(grep '^gdrive_folder_id:' "$pl_config") || pl_status=$?
  ((pl_status != 1)) || return 0
  ((pl_status == 0)) || return 1
  [[ $pl_line != 'gdrive_folder_id: ""' ]] || return 0
  if [[ $pl_line =~ $pl_quoted || $pl_line =~ $pl_unquoted ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

# render_config TEMPLATE DEPLOYED_CONFIG prints the config to install from TEMPLATE: rendered with
# the shared drive DEPLOYED_CONFIG names, so a release keeps Drive sync and GC on, or TEMPLATE as it
# is when DEPLOYED_CONFIG names no drive or does not exist (a new Pi). It fails on a
# gdrive_folder_id line it cannot read, rather than install a config that drops or changes the
# drive.
render_config() {
  local pl_template=$1 pl_deployed=$2 pl_drive_id
  [[ -f $pl_template ]] || return 1
  if [[ ! -e $pl_deployed ]]; then
    cat -- "$pl_template"
    return
  fi
  pl_drive_id=$(deployed_drive_id "$pl_deployed") || return 1
  if [[ -z $pl_drive_id ]]; then
    cat -- "$pl_template"
    return
  fi
  render_config_with_drive "$pl_template" "$pl_drive_id"
}

# env_kept_lines ENV_FILE prints each line of the .env file ENV_FILE, in order, but the four lines
# update.sh owns and the lines of variables Ghost Wispr no longer reads. It fails when it cannot read
# ENV_FILE.
env_kept_lines() {
  [[ -r $1 ]] || return 1
  awk -v removed="^GHOST_WISPR_($PI_LIB_REMOVED_ENV_VARIABLES)=" -v owned="^($PI_LIB_OWNED_ENV_VARIABLES)=" \
    '$0 !~ removed && $0 !~ owned' "$1"
}

# config_override_names ENV_FILE prints, one a line, each variable of PI_LIB_CONFIG_FILE_VARIABLE
# and PI_LIB_ENVOY_OVERRIDE_VARIABLES that a line of the .env file ENV_FILE could set: a line that,
# past leading space and an export, starts with its name and =. A line ends at a carriage return as
# at a line feed, as systemd's EnvironmentFile= parser reads one (NEWLINE "\n\r",
# src/basic/env-file.c), so tr turns each CR into a line feed first. A match inside a multi-line
# value counts too. It reads no value, and fails when it cannot read ENV_FILE.
config_override_names() {
  local pl_found pl_status=0
  [[ -r $1 ]] || return 1
  pl_found=$(tr '\r' '\n' <"$1" | grep -oE "^[[:space:]]*(export[[:space:]]+)?($PI_LIB_CONFIG_FILE_VARIABLE|$PI_LIB_ENVOY_OVERRIDE_VARIABLES)[[:space:]]*=") || pl_status=$?
  ((pl_status <= 1)) || return 1
  [[ -z $pl_found ]] || grep -oE "$PI_LIB_CONFIG_FILE_VARIABLE|$PI_LIB_ENVOY_OVERRIDE_VARIABLES" <<<"$pl_found" | sort -u
}

# manager_override_names prints, one a line, each variable of PI_LIB_CONFIG_FILE_VARIABLE and
# PI_LIB_ENVOY_OVERRIDE_VARIABLES that ghost-wispr's user manager sets in its own environment
# (systemctl --user set-environment or import-environment, an environment.d file,
# DefaultEnvironment=, the manager's own start-up environment), which each of its units inherits.
# systemctl --user show-environment prints that environment one variable a line, its name first; a
# value with a line break is quoted onto the line as $'...' (src/systemctl/systemctl-set-environment.c).
# It reads no value, and fails when it cannot read the environment or reads none: a user manager
# always sets PATH, so an empty reading is a blind one. Read it after a reload of the manager, so
# that an environment.d or user.conf line written since the last reload counts.
manager_override_names() {
  local pl_environment pl_found pl_status=0
  pl_environment=$(systemctl --user show-environment) || return 1
  [[ -n $pl_environment ]] || return 1
  pl_found=$(grep -oE "^($PI_LIB_CONFIG_FILE_VARIABLE|$PI_LIB_ENVOY_OVERRIDE_VARIABLES)=" <<<"$pl_environment") || pl_status=$?
  ((pl_status <= 1)) || return 1
  [[ -z $pl_found ]] || grep -oE "$PI_LIB_CONFIG_FILE_VARIABLE|$PI_LIB_ENVOY_OVERRIDE_VARIABLES" <<<"$pl_found" | sort -u
}

# unit_drop_ins UNIT prints the drop-ins of ghost-wispr's user unit UNIT, space-separated, as
# systemctl prints DropInPaths, or nothing when it has none. It reads the drop-ins alone, so a unit
# that is not-found (a new Pi's ghost-wispr.service, before its first install) with no drop-in prints
# nothing. A drop-in can change how the unit runs, such as set the recorder's environment past every
# check of .env, or give the update a time limit. Read it after a reload of the manager.
unit_drop_ins() {
  systemctl --user show -p DropInPaths --value "$1"
}

# serve_conf_lines SERVE_CONF prints the Serve lines of SERVE_CONF (deploy/pi/files/serve.conf),
# "<port> <proxy target>", sorted, with no comment or blank line.
serve_conf_lines() {
  local pl_lines
  pl_lines=$(awk 'NF && $1 !~ /^#/ { print $1, $2 }' "$1") || return 1
  if [[ -n $pl_lines ]]; then
    LC_ALL=C sort <<<"$pl_lines"
  fi
}

# serve_lines_from_status JSON prints the Serve setting that JSON, the answer of
# `tailscale serve status --json`, holds, as serve_conf_lines prints serve.conf: one
# "<port> <proxy target>" line for each Web entry ("<host>:<port>") whose handler / names a Proxy,
# sorted. JSON is tailscale's ipn.ServeConfig. It fails on an empty answer or on one that is not a
# JSON object, so a caller can tell an answer it cannot read from a Serve that serves nothing.
serve_lines_from_status() {
  local pl_lines
  [[ -n $1 ]] || return 1
  pl_lines=$(jq -r 'if type != "object" then error("not a ServeConfig") else
      (.Web // {}) | to_entries[]
      | select(.value | type == "object")
      | select(.value.Handlers | type == "object")
      | select(.value.Handlers["/"] | type == "object")
      | select(.value.Handlers["/"].Proxy | type == "string")
      | "\(.key | split(":") | last) \(.value.Handlers["/"].Proxy)"
    end' <<<"$1" 2>/dev/null) || return 1
  if [[ -n $pl_lines ]]; then
    LC_ALL=C sort <<<"$pl_lines"
  fi
}

# bootstrap_drift MANIFEST [DEPLOY_DIR [UNIT_DIR]] prints, one a line, each bootstrap file (the
# launcher, ghost-wispr-update.service and ghost-wispr-update.timer, which only provision.sh
# installs) whose installed copy is missing or is not the file that MANIFEST, a release's
# manifest.json, names: the launcher in DEPLOY_DIR (/opt/ghost-wispr), the unit and the timer in
# UNIT_DIR (~/.config/systemd/user). It fails when MANIFEST names no SHA-256 for one of them, or
# when it cannot read an installed copy.
bootstrap_drift() {
  local pl_manifest=$1 pl_deploy_dir=${2:-/opt/ghost-wispr} pl_unit_dir=${3:-$HOME/.config/systemd/user}
  local pl_name pl_path pl_want pl_have
  for pl_name in ghost-wispr-update-launcher.sh ghost-wispr-update.service ghost-wispr-update.timer; do
    pl_want=$(jq -er --arg name "$pl_name" '.files[$name] | strings' "$pl_manifest") || return 1
    pl_path=$pl_unit_dir/$pl_name
    if [[ $pl_name == *.sh ]]; then
      pl_path=$pl_deploy_dir/$pl_name
    fi
    pl_have=''
    if [[ -f $pl_path ]]; then
      pl_have=$(sha256sum -- "$pl_path") || return 1
      pl_have=${pl_have%% *}
    fi
    if [[ $pl_have != "$pl_want" ]]; then
      printf '%s\n' "$pl_name"
    fi
  done
}
