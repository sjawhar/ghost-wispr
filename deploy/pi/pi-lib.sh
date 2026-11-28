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
# The jq filter that is true for an answer of the recorder's /api/status: an object with
# active_session_id, paused and capture_state.
# shellcheck disable=SC2034 # update.sh and provision.sh, which source this file, read it.
PI_LIB_RECORDER_STATUS_FILTER='type == "object" and (.active_session_id | type == "string") and (.paused | type == "boolean") and (.capture_state | type == "string")'
# The jq filter that prints, from such a status, why the recorder is busy: it records a session, or
# it holds a call whose state is not none (a Meet call that INFRA-5904's call bridge links to the
# Pi). It prints nothing when the recorder is free. A busy recorder may not restart (update.sh), and
# Serve, which carries the page and /call, may not be reset under it (provision.sh's Serve apply).
# shellcheck disable=SC2034 # update.sh and provision.sh, which source this file, read it.
PI_LIB_BUSY_REASON_FILTER='if .active_session_id != "" then "active session" elif .call == null then empty elif (.call | type) != "object" then "call unknown" elif .call.state != "none" then "call \(.call.state // "unknown")" else empty end'
# The transient system units that a provisioning run is (provision.sh): the run itself, and the
# detached tailnet join it starts. systemd starts a unit of each name once at a time, so a second
# run cannot start while the first runs, and the end of a run, however it comes, ends every process
# of its unit.
# shellcheck disable=SC2034 # provision.sh and provision_unit_test.sh, which source this file, read it.
PI_LIB_PROVISION_UNIT=ghost-wispr-provision
# shellcheck disable=SC2034 # provision.sh and provision_unit_test.sh, which source this file, read it.
PI_LIB_PROVISION_JOIN_UNIT=ghost-wispr-provision-join

# provision_unit_command UNIT USER sets the array pl_unit_command to the systemd-run command, less its
# settings and program, that runs a provisioning run as the transient system unit UNIT, as USER. The
# run's output reaches the caller (--pipe, which a plain SSH session takes, where --pty hangs), the
# caller waits for its end and gets its exit status (--wait), a failed run leaves no unit behind
# (--collect), and its end, by any cause, ends every process of the unit (KillMode=control-group),
# with SIGKILL 10 s after SIGTERM. The caller runs it as root: sudo "${pl_unit_command[@]}" ...
provision_unit_command() {
  # shellcheck disable=SC2034 # The caller reads it.
  pl_unit_command=(systemd-run "--unit=$1" --pipe --wait --collect --quiet --property=KillMode=control-group
    --property=TimeoutStopSec=10s "--uid=$2")
}

# The caller's settings that a run's unit gets: provision.sh's own and those of the nightly update it
# starts, never a credential. A unit's Environment property is readable by any local user, so a
# GHOST_WISPR_DISPATCH_TOKEN or GHOST_WISPR_DEEPGRAM_API_KEY that the caller exports stays out
# (GPT-6.1 Sol's review of #203, round 6).
# shellcheck disable=SC2034 # provision.sh, which sources this file, reads it.
PI_LIB_PROVISION_SETTINGS=(GHOST_WISPR_DEPLOY_DIR GHOST_WISPR_UNIT_DIR GHOST_WISPR_SYSTEM_ROOT GHOST_WISPR_IDENTITY_KEY
  GHOST_WISPR_JOIN_DIR GHOST_WISPR_STATUS_URL GHOST_WISPR_TAILSCALE_CALL_TIMEOUT GHOST_WISPR_RELEASE_API
  GHOST_WISPR_VERSION_URL)

# provision_unit_settings sets the array pl_unit_settings to the --setenv options of a run's unit:
# PATH, and each setting of PI_LIB_PROVISION_SETTINGS that the caller sets.
provision_unit_settings() {
  local pl_name
  # shellcheck disable=SC2034 # The caller reads it.
  pl_unit_settings=("--setenv=PATH=$PATH")
  for pl_name in "${PI_LIB_PROVISION_SETTINGS[@]}"; do
    if [[ -n ${!pl_name+set} ]]; then
      pl_unit_settings+=("--setenv=$pl_name=${!pl_name}")
    fi
  done
}

# provision_units_busy UNIT... prints a line for each UNIT.service that systemd has in another state
# than inactive or failed: "UNIT.service is STATE since TIME, main PID N". It fails when it cannot
# read a unit's state.
provision_units_busy() {
  local pl_unit pl_show pl_state pl_since pl_main
  for pl_unit; do
    pl_show=$(systemctl show -p ActiveState -p ActiveEnterTimestamp -p MainPID "$pl_unit.service") || return 1
    pl_state=$(sed -n 's/^ActiveState=//p' <<<"$pl_show")
    pl_since=$(sed -n 's/^ActiveEnterTimestamp=//p' <<<"$pl_show")
    pl_main=$(sed -n 's/^MainPID=//p' <<<"$pl_show")
    case $pl_state in
      inactive | failed) ;;
      '') return 1 ;;
      *) printf '%s.service is %s since %s, main PID %s\n' "$pl_unit" "$pl_state" "${pl_since:-an unknown time}" "${pl_main:-unknown}" ;;
    esac
  done
}

# provision_stop_traps makes a run that systemd stops end failed. systemd counts an end by SIGTERM,
# which systemctl stop sends, or by SIGHUP as clean, and systemd-run --wait then gives the caller 0;
# with these traps the run exits 143 or 129 instead, once the command it waits for has ended.
provision_stop_traps() {
  trap 'exit 143' TERM
  trap 'exit 129' HUP
}

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

# env_entries MODE ENV_FILE reads the .env file ENV_FILE as systemd's EnvironmentFile= parser does
# (parse_env_file_internal, src/basic/env-file.c), which the recorder's environment comes from, and
# which keeps the last value of a variable. An entry starts at its key, past spaces, tabs and line
# breaks, and ends at the line break after its value. A line break is a line feed or a carriage
# return, a key ends at its = with spaces and tabs stripped, a backslash escapes the next byte, a
# line break included, and a value in single or double quotes runs on across line breaks to its
# closing quote. A line that starts with # or ; is a comment, and so is a key with no =. With MODE
# kept it prints ENV_FILE byte for byte but the entries of the four variables update.sh owns and of
# the variables Ghost Wispr no longer reads, each from the start of its line to its line break,
# that included. With MODE secrets it prints the last entry of GHOST_WISPR_DEEPGRAM_API_KEY and
# of GHOST_WISPR_DISPATCH_TOKEN, in that order, each from its key to its value's end and then a
# line feed. It fails when it cannot read ENV_FILE.
env_entries() {
  local pl_content
  [[ -r $2 ]] || return 1
  # The x keeps the file's last line breaks, which $( ) would drop; awk drops it again.
  pl_content=$(cat -- "$2" && printf x) || return 1
  LC_ALL=C awk -v mode="$1" -v drop="^($PI_LIB_OWNED_ENV_VARIABLES|GHOST_WISPR_($PI_LIB_REMOVED_ENV_VARIABLES))\$" '
    function entry(end) {
      sub(/[ \t]+$/, "", key)
      count++
      entry_start[count] = start; entry_key_start[count] = key_start; entry_end[count] = end; entry_key[count] = key
    }
    { text = (NR > 1 ? text "\n" : "") $0 }
    END {
      text = substr(text, 1, length(text) - 1)
      n = length(text); state = "pre_key"; start = 1; count = 0
      for (i = 1; i <= n; i++) {
        c = substr(text, i, 1)
        line_break = (c == "\n" || c == "\r")
        if (state == "pre_key") {
          if (c == "#" || c == ";") state = "comment"
          else if (line_break) start = i + 1
          else if (c != " " && c != "\t") { state = "key"; key_start = i; key = c }
        } else if (state == "key") {
          if (line_break) { state = "pre_key"; start = i + 1 }
          else if (c == "=") state = "pre_value"
          else key = key c
        } else if (state == "pre_value" || state == "value") {
          if (line_break) { entry(i); state = "pre_key"; start = i + 1 }
          else if (c == "\\") state = "value_escape"
          else if (state == "pre_value" && c == "\047") state = "single_quote"
          else if (state == "pre_value" && c == "\"") state = "double_quote"
          else if (state == "pre_value" && c != " " && c != "\t") state = "value"
        } else if (state == "value_escape") state = "value"
        else if (state == "single_quote") { if (c == "\047") state = "pre_value" }
        else if (state == "double_quote") {
          if (c == "\"") state = "pre_value"
          else if (c == "\\") state = "double_quote_escape"
        } else if (state == "double_quote_escape") state = "double_quote"
        else if (state == "comment") {
          if (c == "\\") state = "comment_escape"
          else if (line_break) { state = "pre_key"; start = i + 1 }
        } else if (state == "comment_escape") {
          if (line_break) { state = "pre_key"; start = i + 1 }
          else state = "comment"
        }
      }
      if (state ~ /^(pre_value|value|value_escape|single_quote|double_quote|double_quote_escape)$/) entry(n + 1)
      if (mode == "kept") {
        out = ""; at = 1
        for (k = 1; k <= count; k++) {
          if (entry_key[k] ~ drop) {
            out = out substr(text, at, entry_start[k] - at)
            at = entry_end[k] + 1
          }
        }
        printf "%s", out substr(text, at)
      } else {
        split("GHOST_WISPR_DEEPGRAM_API_KEY GHOST_WISPR_DISPATCH_TOKEN", secret, " ")
        for (s = 1; s <= 2; s++) {
          last = 0
          for (k = 1; k <= count; k++) if (entry_key[k] == secret[s]) last = k
          if (last) printf "%s\n", substr(text, entry_key_start[last], entry_end[last] - entry_key_start[last])
        }
      }
    }' <<<"$pl_content"
}

# env_kept_lines ENV_FILE prints the .env file ENV_FILE, byte for byte, but the entries systemd
# reads of the four lines update.sh owns and of the variables Ghost Wispr no longer reads (an
# indented one and one after a carriage return too: see env_entries). It fails when it cannot read
# ENV_FILE.
env_kept_lines() {
  env_entries kept "$1"
}

# env_secret_lines ENV_FILE prints the two secret lines that systemd reads from the .env file
# ENV_FILE, each the last of its variable, the Deepgram key's first: the lines a failed pi-secrets
# read keeps (R9). It fails when it cannot read ENV_FILE.
env_secret_lines() {
  env_entries secrets "$1"
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

# device_tailnet_name STATUS prints the device's tailnet name that STATUS, the answer of
# `tailscale status --self --json`, gives as Self.DNSName, without its final dot. It fails on an
# answer it cannot read and on a name that is empty or not a DNS name.
device_tailnet_name() {
  jq -er '.Self.DNSName | strings | select(test("^[A-Za-z0-9-]+(\\.[A-Za-z0-9-]+)*\\.$")) | rtrimstr(".")' \
    <<<"$1" 2>/dev/null
}

# serve_lines_from_status JSON NAME prints the Serve setting that JSON, the answer of
# `tailscale serve status --json` (tailscale's ipn.ServeConfig), holds: one line for each handler of
# each HTTPS port, sorted. NAME is the device's tailnet name (device_tailnet_name). A proxy at / of
# NAME prints as serve_conf_lines prints a line of serve.conf, "<port> <proxy target>". Each other
# handler (files, a text, a redirect, a proxy at another path, or a proxy that forwards peer
# capabilities) prints as "<port> <path> <kind> <value>", the path and the value as JSON strings:
# three fields or more, which no line of serve.conf equals. A Web entry of another name than NAME,
# such as the one a device renamed after its Serve was set keeps under its name before, prints each
# of its lines with " on <name>" after it, so that it differs from serve.conf and names the name,
# and never reads as a second line of NAME's port. {} serves nothing, and prints nothing. It fails
# on an empty answer or NAME, on one that is not one JSON object, and on a Serve of a shape it does
# not know: a key, a port, a handler or a value of another type, a Web entry on a port that serves
# no HTTPS, an HTTPS port with no handler, a TCP forward or plain HTTP, Funnel, a service or a
# foreground Serve. So a caller can tell a Serve it cannot read from one that matches serve.conf,
# and never reads a part it does not know as a match.
#
# serve_lines_from_status JSON NAME shape prints the same lines with no handler's value, for output
# a person reads: "<port> <path> <kind>", with " accept-app-caps" when the handler forwards peer
# capabilities, and the " on <name>" of another name when that name is a DNS name, else " on a
# name that is no DNS name". A Text handler's body, a redirect, a proxy target, a file path and
# the capabilities are what someone set in Serve, which provision.sh never copies into its output
# (GPT-6.1 Sol's review of #208, round 3).
serve_lines_from_status() {
  local pl_lines
  [[ -n $1 && -n ${2:-} ]] || return 1
  pl_lines=$(jq -rs --arg name "$2" --arg shape "${3:-}" '
    def check(cond; why): if cond then . else error(why) end;
    def kinds: "Path", "Proxy", "Text", "Redirect";
    check(length == 1; "not one JSON value") | .[0]
    | check(type == "object"; "not a ServeConfig")
    | check(keys - ["TCP", "Web", "AllowFunnel", "Services", "Foreground"] == []; "a key it does not know")
    | check(all(.Services, .Foreground; . == null or . == {}); "a service or a foreground Serve")
    | check(.AllowFunnel | . == null or (type == "object" and all(.[]; . == false)); "Funnel")
    | (.TCP // {}) as $tcp | (.Web // {}) as $web
    | check(($tcp | type) == "object" and ($web | type) == "object"; "TCP or Web is no object")
    | check(all($tcp | to_entries[]; (.key | test("^[1-9][0-9]{0,4}$")) and .value == {HTTPS: true});
        "a TCP port that does not serve HTTPS")
    | [$web | to_entries[]
        | check(.key | test("^[^:]+:[1-9][0-9]{0,4}$"); "a Web key that is not <host>:<port>")
        | (.key | split(":")) as [$host, $port]
        | (if $host == $name then ""
           elif $shape == "shape" and ($host | test("^[A-Za-z0-9.-]+$") | not) then " on a name that is no DNS name"
           else " on \($host)" end) as $on
        | check($tcp | has($port); "a Web entry on a port that serves no HTTPS")
        | check(.value | type == "object" and keys == ["Handlers"] and (.Handlers | type == "object" and length > 0);
            "a Web entry that is not a set of handlers")
        | .value.Handlers | to_entries[]
        | .key as $path | .value as $handler
        | check(($path | startswith("/")) and ($handler | type == "object"); "a handler it does not know")
        | [$handler | to_entries[] | select(.key != "AcceptAppCaps")] as $kind
        | check(($kind | length == 1) and any(kinds; . == $kind[0].key)
            and ($kind[0].value | type == "string" and length > 0)
            and ($handler.AcceptAppCaps | . == null or (type == "array" and all(.[]; type == "string")));
            "a handler it does not know")
        | $kind[0]
        | if $shape == "shape"
          then "\($port) \($path | @json) \(.key | ascii_downcase)"
            + if $handler.AcceptAppCaps == null then "" else " accept-app-caps" end
          elif $path == "/" and .key == "Proxy" and $handler.AcceptAppCaps == null and (.value | test("^\\S+$"))
          then "\($port) \(.value)"
          else "\($port) \($path | @json) \(.key | ascii_downcase) \(.value | @json)"
            + if $handler.AcceptAppCaps == null then "" else " accept-app-caps \($handler.AcceptAppCaps | @json)" end
          end
        | . + $on]
    | check(all($tcp | keys[]; . as $port | any($web | keys[]; endswith(":" + $port))); "an HTTPS port with no handler")
    | .[]' <<<"$1" 2>/dev/null) || return 1
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

# The MESSAGE_ID of the line that journald logs on its own storage, "<name> (<path>) is <use>, max
# <max>, <free> free.", from its own PID (_TRANSPORT=driver), at each flush of the journal and each
# rotation (systemd v255 to v257, src/journal/journald-server.c, server_space_usage_message). Its
# JOURNAL_PATH is the journal it writes, /var/log/journal/<machine-id> or
# /run/log/journal/<machine-id>, and its MAX_USE and DISK_KEEP_FREE are the limits that this journald
# runs, which it read from its setting at its start: journald reads its setting at no other time.
# shellcheck disable=SC2034 # update.sh and check-deploy.sh, which source this file, read it.
PI_LIB_JOURNALD_USAGE_ID=ec387f577b844b8fa948f33cad9a75e6

# journald_conf_values FILE reads a journald.conf on standard input, or `systemd-analyze cat-config
# systemd/journald.conf`'s output of all of journald's setting files, and prints each key of its
# [Journal] section as "<key>=<value><TAB><file>": its last value, which journald runs, and the file
# that gives it (FILE for a lone file). In cat-config's output a file starts with a line "# <path>",
# first or after a blank line; any other line that starts with # or ; is a comment.
journald_conf_values() {
  awk -v file="$1" '
    { line = $0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line) }
    (NR == 1 || previous == "") && line ~ /^# \/[^[:space:]]+$/ { file = substr(line, 3); section = ""; previous = line; next }
    { previous = line }
    line == "" || line ~ /^[#;]/ { next }
    line ~ /^\[.*\]$/ { section = line; next }
    section == "[Journal]" && index(line, "=") > 1 {
      key = substr(line, 1, index(line, "=") - 1); sub(/[[:space:]]+$/, "", key)
      value = substr(line, index(line, "=") + 1); sub(/^[[:space:]]+/, "", value)
      if (!(key in got)) keys[++n] = key
      got[key] = value; from[key] = file
    }
    END { for (i = 1; i <= n; i++) printf "%s=%s\t%s\n", keys[i], got[keys[i]], from[keys[i]] }'
}

# journald_value VALUES KEY prints the value of KEY in VALUES, journald_conf_values's lines, or
# nothing when they do not set it.
journald_value() {
  awk -F '\t' -v key="$2" 'index($1, key "=") == 1 { print substr($1, length(key) + 2); exit }' <<<"$1"
}

# journald_size VALUE prints VALUE, a size as journald.conf gives it, in bytes: a whole number, with
# K, M, G, T, P or E for each power of 1024. It fails on any other form, such as a fraction.
journald_size() {
  local pl_units=KMGTPE pl_power=0
  [[ $1 =~ ^([0-9]+)([KMGTPE]?)$ ]] || return 1
  if [[ -n ${BASH_REMATCH[2]} ]]; then
    pl_power=${pl_units%%"${BASH_REMATCH[2]}"*}
    pl_power=$((${#pl_power} + 1))
  fi
  printf '%s\n' "$((10#${BASH_REMATCH[1]} << (10 * pl_power)))"
}

# journald_expected VALUES MACHINE_ID CARD prints, a field a line, what the usage line of a journald
# that runs the setting VALUES (journald_conf_values's lines) holds: JOURNAL_PATH, the journal on
# the card, /var/log/journal/MACHINE_ID, for Storage=persistent, or for Storage=auto, journald's
# default, when CARD is yes (/var/log/journal is a directory), else the runtime one in
# /run/log/journal; then MAX_USE and DISK_KEEP_FREE from SystemMaxUse and SystemKeepFree for the
# journal on the card, RuntimeMaxUse and RuntimeKeepFree for the runtime one, each only when the
# setting gives it in a form journald_size reads. journald works out a limit that the setting leaves
# out from the file system, and rounds MAX_USE up to a whole page, so a setting of a whole number of
# 64 KiB compares as given. It prints nothing for Storage=none, which keeps no journal, and fails on
# a Storage it does not know.
journald_expected() {
  local pl_prefix pl_path pl_value pl_bytes
  case $(journald_value "$1" Storage) in
    none) return 0 ;;
    persistent) pl_prefix=System pl_path=/var/log/journal/$2 ;;
    volatile) pl_prefix=Runtime pl_path=/run/log/journal/$2 ;;
    auto | '')
      pl_prefix=Runtime pl_path=/run/log/journal/$2
      if [[ $3 == yes ]]; then
        pl_prefix=System pl_path=/var/log/journal/$2
      fi
      ;;
    *) return 1 ;;
  esac
  printf 'JOURNAL_PATH=%s\n' "$pl_path"
  pl_value=$(journald_value "$1" "${pl_prefix}MaxUse")
  if [[ -n $pl_value ]] && pl_bytes=$(journald_size "$pl_value"); then
    printf 'MAX_USE=%s\n' "$pl_bytes"
  fi
  pl_value=$(journald_value "$1" "${pl_prefix}KeepFree")
  if [[ -n $pl_value ]] && pl_bytes=$(journald_size "$pl_value"); then
    printf 'DISK_KEEP_FREE=%s\n' "$pl_bytes"
  fi
}

# journald_running USAGE prints the fields of journald's usage line USAGE, one entry that
# `journalctl -o json` prints, as journald_expected prints them. It fails on an entry it cannot read.
journald_running() {
  jq -er 'select(type == "object") | "JOURNAL_PATH=\(.JOURNAL_PATH // "")", "MAX_USE=\(.MAX_USE // "")",
    "DISK_KEEP_FREE=\(.DISK_KEEP_FREE // "")"' <<<"$1"
}

# journald_differences EXPECTED RUNNING prints, one a line, each field of EXPECTED
# (journald_expected's lines) that RUNNING (journald_running's) does not hold as "<field> is
# <running's>, not <expected's>", and nothing when they agree.
journald_differences() {
  local pl_line pl_field pl_have
  while IFS= read -r pl_line; do
    [[ -n $pl_line ]] || continue
    pl_field=${pl_line%%=*}
    pl_have=$(awk -v field="$pl_field=" 'index($0, field) == 1 { print substr($0, length(field) + 1); exit }' <<<"$2")
    if [[ $pl_line != "$pl_field=$pl_have" ]]; then
      printf '%s is %s, not %s\n' "$pl_field" "${pl_have:-none}" "${pl_line#*=}"
    fi
  done <<<"$1"
}
