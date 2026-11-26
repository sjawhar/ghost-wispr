#!/usr/bin/env bash
# Provision the office Pi from a Ghost Wispr release. Run it as ghost-wispr on the Pi, from the
# extracted ghost-wispr-pi-deploy/ directory of the release's ghost-wispr-pi-deploy.tar.gz, which
# holds this script, the release's deploy set and its manifest.json (docs/pi-setup.md).
#
#   provision.sh --key                 make the Pi's key, which never leaves it, and print its
#                                      certificate request for the bootstrap of the infrastructure
#   provision.sh --certificate FILE    install the Pi's first certificate, once its public key is
#                                      the key's
#   provision.sh                       write the Google credential files from pi.env where they are
#                                      missing, install the bootstrap files, start the nightly
#                                      update, apply serve.conf, and join the tailnet
#   provision.sh --models DRIVE_ID     install the release's config with the shared drive DRIVE_ID
#
# It reads no credential bundle: the nightly update reads the Pi's Deepgram key and Dispatch token
# from Secret Manager, and each other setting is public, in the release's pi.env. A run changes
# nothing that already is the release's, so one run of a later release's provision.sh applies a
# changed bootstrap file or Serve line, which the nightly update names in its drift lines. Sourced,
# it defines its functions and runs nothing: the runbook's step that accepts the tailnet's subnet
# routes prints route_overlap_check_script from it.
set -euo pipefail

# GHOST_WISPR_DEPLOY_DIR, GHOST_WISPR_UNIT_DIR, GHOST_WISPR_IDENTITY_KEY and GHOST_WISPR_JOIN_DIR
# exist for provision_run_test.sh, as update.sh's do for update_test.sh.
readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly UNIT_DIR="${GHOST_WISPR_UNIT_DIR:-/home/ghost-wispr/.config/systemd/user}"
readonly IDENTITY_KEY="${GHOST_WISPR_IDENTITY_KEY:-/home/ghost-wispr/.config/ghost-wispr-identity/key.pem}"
readonly JOIN_DIR="${GHOST_WISPR_JOIN_DIR:-/var/lib/ghost-wispr-join}"
readonly JOIN_ROUTE_CHECK="$JOIN_DIR/route-overlap-check"
readonly JOIN_TOKEN="$JOIN_DIR/token"
readonly SERVICE_USER=ghost-wispr
# The paths that the credential files name: where the recorder and pi-secrets find the certificate,
# its config and the key on the Pi, as gcloud's create-cred-config wrote them for the office Pi.
readonly CREDENTIAL_CERTIFICATE=/opt/ghost-wispr/device.crt
readonly CREDENTIAL_CONFIG=/opt/ghost-wispr/certificate_config.json
readonly CREDENTIAL_KEY=/home/ghost-wispr/.config/ghost-wispr-identity/key.pem
readonly CREDENTIAL_FILES=(google-credentials.json certificate_config.json)
# The bootstrap files (dispatch://INFRA-5935/spec R8): the launcher, the update unit and its timer
# install every later release, so only this script installs them, and the nightly update compares
# them with each release.
readonly BOOTSTRAP_FILES=(ghost-wispr-update-launcher.sh ghost-wispr-update.service ghost-wispr-update.timer)
readonly USAGE='usage: provision.sh [--key | --certificate FILE | --models <shared-drive-id>], as ghost-wispr, from the extracted ghost-wispr-pi-deploy/ directory of a release'
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir

fail() {
  printf 'ghost-wispr provision: %s\n' "$*" >&2
  exit 1
}

# pi-lib.sh, beside this script in the release and in a checkout's deploy/pi/, holds the override
# checks, the config renderer and the Serve parser that update.sh uses too.
[[ -r $script_dir/pi-lib.sh ]] || fail "cannot read $script_dir/pi-lib.sh"
# shellcheck source=/dev/null
source "$script_dir/pi-lib.sh"

require_service_user() {
  [[ $(id -un) == "$SERVICE_USER" ]] || fail "run as $SERVICE_USER"
}

# refuse_input fails while standard input holds data, such as the credential bundle that an older
# provisioner read there: this one reads none. It waits at most 1 s for a first byte, so a run whose
# input is an ssh session's terminal, or /dev/null, goes on.
refuse_input() {
  if IFS= read -r -t 1 -N 1 _; then
    fail "standard input holds data, such as a credential bundle, and provision.sh reads none: the nightly update reads the Pi's secrets from Secret Manager. $USAGE"
  fi
}

# put_file SOURCE TARGET MODE installs SOURCE as TARGET with MODE, by a copy to a temporary name
# beside TARGET and a rename, so a reader of the old file, such as a running update.sh, reads it to
# its end, and no reader sees half a file.
put_file() {
  local tmp
  tmp=$(mktemp -p "$(dirname -- "$2")" ".${2##*/}.provision.XXXXXX") || return 1
  if ! install -m "$3" -- "$1" "$tmp" || ! mv -f -- "$tmp" "$2"; then
    rm -f -- "$tmp"
    return 1
  fi
}

# check_release_directory fails, before it installs anything, unless this script's directory is a
# release's deploy set: a manifest.json that names each file of the set with its SHA-256 (the
# plan's C2), the files this script installs among them, and each file as the manifest names it.
# So a checkout's deploy/pi/, a directory cut short and a file changed after the build are refused.
check_release_directory() {
  local manifest=$script_dir/manifest.json names name want have
  [[ -f $manifest && ! -L $manifest ]] \
    || fail "$script_dir has no manifest.json, so it is no release's directory: run provision.sh from the extracted ghost-wispr-pi-deploy/ directory of a release's ghost-wispr-pi-deploy.tar.gz; nothing was installed"
  names=$(jq -er '.files | objects | keys[]' "$manifest" 2>/dev/null) \
    || fail "$manifest names no files; nothing was installed"
  for name in provision.sh pi-lib.sh update.sh pi.env ghost-wispr.yaml serve.conf "${BOOTSTRAP_FILES[@]}"; do
    grep -Fqx -- "$name" <<<"$names" || fail "$manifest does not name $name; nothing was installed"
  done
  while IFS= read -r name; do
    [[ $name =~ ^[A-Za-z0-9_][A-Za-z0-9._-]*$ ]] || fail "$manifest names a file that is no plain name: $name; nothing was installed"
    want=$(jq -r --arg name "$name" '.files[$name] | strings' "$manifest") || fail "cannot read $manifest"
    [[ -f $script_dir/$name && ! -L $script_dir/$name ]] \
      || fail "$name, which the release's manifest.json names, is not in $script_dir; nothing was installed: extract the release's tarball again"
    have=$(sha256sum -- "$script_dir/$name") || fail "cannot read $script_dir/$name"
    [[ ${have%% *} == "$want" ]] \
      || fail "$script_dir/$name is not the file the release's manifest.json names; nothing was installed: extract the release's tarball again"
  done <<<"$names"
}

# require_release_the_pi_runs fails, before anything changes, while DEPLOY_DIR/deploy.verified, the
# set the nightly update installed and verified last, names another commit than this release's
# manifest.json. provision.sh installs its update.sh, pi-lib.sh and pi.env before the update
# starts, and --models its config, so another release's would leave the Pi a set that
# deploy.verified does not list: the update would keep no .prev copy of the set the Pi runs, and a
# failed install would roll the Pi back past it. A Pi with no deploy.verified, a new Pi or the
# office Pi before its first update of this kind, passes. A Pi that has deploy.verified ran the
# update, which needs jq, so jq is there to read the manifest.
require_release_the_pi_runs() {
  local verified=$DEPLOY_DIR/deploy.verified line installed release
  [[ -e $verified ]] || return 0
  line=$(head -n 1 -- "$verified") || fail "cannot read $verified: nothing was changed"
  [[ $line =~ ^commit\ ([0-9a-f]{40})$ ]] || fail "$verified names no commit on its first line: nothing was changed"
  installed=${BASH_REMATCH[1]}
  release=$(jq -er '.commit | strings' "$script_dir/manifest.json" 2>/dev/null) \
    || fail "$script_dir/manifest.json names no commit, so it is no release's directory: nothing was changed"
  [[ $release == "$installed" ]] \
    || fail "the Pi runs commit $installed ($verified), and this is the release of commit $release: nothing was changed. provision.sh installs its own update.sh, pi-lib.sh and pi.env, so another release's would let a failed install roll the Pi back past the release it runs. Download the release of commit $installed (docs/pi-setup.md, \"The release the Pi runs\") and run its provision.sh"
}

# read_pi_env reads the release's pi.env, the Pi's public settings, and fails unless it sets the
# values that the credential files and the tailnet join need, each in the form they take.
read_pi_env() {
  # shellcheck source=/dev/null
  source "$script_dir/pi.env" || fail "cannot read the Pi's settings in $script_dir/pi.env"
  [[ ${PI_HOST:-} =~ ^[A-Za-z0-9.-]+$ ]] || fail "pi.env sets no tailnet name in PI_HOST"
  [[ ${PI_SERVICE_ACCOUNT:-} =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+$ ]] || fail "pi.env sets no service account in PI_SERVICE_ACCOUNT"
  [[ ${PI_SERVICE_ACCOUNT_UNIQUE_ID:-} =~ ^[0-9]+$ ]] \
    || fail "pi.env sets no unique ID of the service account in PI_SERVICE_ACCOUNT_UNIQUE_ID"
  [[ ${PI_WORKLOAD_PROVIDER:-} =~ ^projects/[0-9]+/locations/global/workloadIdentityPools/[A-Za-z0-9-]+/providers/[A-Za-z0-9-]+$ ]] \
    || fail "pi.env sets no workload identity provider in PI_WORKLOAD_PROVIDER"
  [[ ${PI_TAILNET_CLIENT_ID:-} =~ ^[A-Za-z0-9._:-]+$ ]] || fail "pi.env sets no tailnet client id in PI_TAILNET_CLIENT_ID"
}

# read_serve_conf reads the release's serve.conf into serve_lines, "<port> <proxy target>" sorted
# (pi-lib.sh's serve_conf_lines). It fails on a line of another form, on a port twice, and on a
# port or a target that it would not write into the join's command as it is.
read_serve_conf() {
  local conf=$script_dir/serve.conf bad port target
  bad=$(awk 'NF && $1 !~ /^#/ && NF != 2' "$conf") || fail "cannot read $conf"
  [[ -z $bad ]] || fail "serve.conf has a line that is not '<port> <proxy target>': $bad; nothing was installed"
  serve_lines=$(serve_conf_lines "$conf") || fail "cannot read $conf"
  while read -r port target; do
    if [[ -n $port ]] && ! [[ $port =~ ^[1-9][0-9]{0,4}$ && $target =~ ^https?://[A-Za-z0-9.:/_-]+$ ]]; then
      fail "serve.conf has the line '$port $target', not an HTTPS port and a proxy target; nothing was installed"
    fi
  done <<<"$serve_lines"
  bad=$(cut -d' ' -f1 <<<"$serve_lines" | uniq -d)
  [[ -z $bad ]] || fail "serve.conf names the port $bad more than once; nothing was installed"
}

# render_credentials DIR writes into DIR the two credential files that pi.env renders: the fields
# that gcloud's create-cred-config writes for an X.509 credential, with the paths of the Pi. With an
# X.509 credential, gcloud impersonates through the mTLS endpoint, iamcredentials.mtls.googleapis.com
# (the office Pi's google-credentials.json holds it so).
render_credentials() {
  jq -n --arg audience "//iam.googleapis.com/$PI_WORKLOAD_PROVIDER" \
    --arg impersonation "https://iamcredentials.mtls.googleapis.com/v1/projects/-/serviceAccounts/$PI_SERVICE_ACCOUNT:generateAccessToken" \
    --arg config "$CREDENTIAL_CONFIG" '{
      universe_domain: "googleapis.com",
      type: "external_account",
      audience: $audience,
      subject_token_type: "urn:ietf:params:oauth:token-type:mtls",
      token_url: "https://sts.mtls.googleapis.com/v1/token",
      credential_source: {certificate: {certificate_config_location: $config}},
      service_account_impersonation_url: $impersonation}' >"$1/google-credentials.json" || return 1
  jq -n --arg certificate "$CREDENTIAL_CERTIFICATE" --arg key "$CREDENTIAL_KEY" \
    '{cert_configs: {workload: {cert_path: $certificate, key_path: $key}}}' >"$1/certificate_config.json"
}

# credential_differences RENDERED INSTALLED prints, one a line, each field (its dotted path) whose
# value differs between the two files, or that one of them lacks. It fails when INSTALLED is not
# one JSON object.
credential_differences() {
  jq -nr --slurpfile want "$1" --slurpfile have "$2" '
    def leaves: [paths(scalars) as $path | {key: ($path | map(tostring) | join(".")), value: getpath($path)}] | from_entries;
    if ($have | length) != 1 or ($have[0] | type) != "object" then error("not one JSON object") else
      ($want[0] | leaves) as $w | ($have[0] | leaves) as $h
      | ($w + $h | keys[]) as $key | select($w[$key] != $h[$key]) | $key
    end' 2>/dev/null
}

# install_credentials writes each credential file that the Pi lacks from pi.env, and compares each
# that it has with what pi.env renders, field by field, and keeps it: gcloud wrote the office Pi's,
# in its own layout. It prints what it wrote, and whether the files it kept are the same or which
# fields differ. A field's value is public, but it prints only the names.
install_credentials() {
  local name fields field compared=false differences=()
  render_credentials "$staging_dir" || fail "could not render the credential files from pi.env"
  for name in "${CREDENTIAL_FILES[@]}"; do
    if [[ ! -e $DEPLOY_DIR/$name && ! -L $DEPLOY_DIR/$name ]]; then
      put_file "$staging_dir/$name" "$DEPLOY_DIR/$name" 0644 || fail "could not install $DEPLOY_DIR/$name"
      printf 'ghost-wispr provision: credentials: wrote %s from pi.env\n' "$name"
      continue
    fi
    compared=true
    fields=$(credential_differences "$staging_dir/$name" "$DEPLOY_DIR/$name") || fields='(not JSON)'
    while IFS= read -r field; do
      if [[ -n $field ]]; then
        differences+=("$name:$field")
      fi
    done <<<"$fields"
  done
  if ! $compared; then
    return 0
  fi
  if ((${#differences[@]} > 0)); then
    printf 'ghost-wispr provision: credentials: differ: %s; the installed files were kept\n' "${differences[*]}"
  else
    printf 'ghost-wispr provision: credentials: same as pi.env renders\n'
  fi
}

# compare_client_id says, where the Pi keeps the tailnet client id of an earlier join, whether it is
# pi.env's PI_TAILNET_CLIENT_ID. It keeps the file.
compare_client_id() {
  local file=$DEPLOY_DIR/tailscale-client-id installed
  [[ -e $file ]] || return 0
  installed=$(<"$file") || fail "cannot read $file"
  if [[ $installed == "$PI_TAILNET_CLIENT_ID" ]]; then
    printf 'ghost-wispr provision: tailnet client id: %s matches PI_TAILNET_CLIENT_ID\n' "$file"
  else
    printf 'ghost-wispr provision: tailnet client id: %s differs from PI_TAILNET_CLIENT_ID\n' "$file"
  fi
}

# live_https_ports JSON prints each HTTPS port that JSON, the answer of `tailscale serve status
# --json` (tailscale's ipn.ServeConfig), serves: a TCP entry with HTTPS, or a Web entry
# "<host>:<port>". It fails on an answer that is not a JSON object.
live_https_ports() {
  [[ -n $1 ]] || return 1
  jq -r 'if type != "object" then error("not a ServeConfig") else
      [((.TCP // {}) | to_entries[] | select((.value | type) == "object" and .value.HTTPS == true) | .key),
       ((.Web // {}) | keys[] | split(":") | last)] | unique[]
    end' <<<"$1" 2>/dev/null
}

# apply_serve gives Tailscale Serve serve.conf's setting: it applies each line, then turns off each
# live HTTPS port that serve.conf does not list. The nightly update only compares Serve with the
# release's serve.conf.
apply_serve() {
  local port target live ports
  while read -r port target; do
    if [[ -n $port ]]; then
      sudo tailscale serve --bg --https "$port" "$target"
    fi
  done <<<"$serve_lines"
  live=$(sudo tailscale serve status --json) \
    || fail "could not read Tailscale Serve's setting (tailscale serve status --json), so the ports that serve.conf does not list may still be served"
  ports=$(live_https_ports "$live") \
    || fail "tailscale serve status --json gave no Serve setting that provision.sh can read, so the ports that serve.conf does not list may still be served"
  while IFS= read -r port; do
    if [[ -n $port ]] && ! grep -q "^$port " <<<"$serve_lines"; then
      sudo tailscale serve --https="$port" off
      printf 'ghost-wispr provision: turned off Serve port %s, which serve.conf does not list\n' "$port"
    fi
  done <<<"$ports"
}

# refuse_overrides WHERE HOW NAMES: fails while NAMES, the variables of PI_LIB_CONFIG_FILE_VARIABLE
# and PI_LIB_ENVOY_OVERRIDE_VARIABLES one a line, is not empty: "WHERE <names>, which would <why>",
# then what to do (HOW). PI_LIB_CONFIG_FILE_VARIABLE's reason comes first.
refuse_overrides() {
  local where=$1 how=$2 names=$3
  [[ -n $names ]] || return 0
  ! grep -qx "$PI_LIB_CONFIG_FILE_VARIABLE" <<<"$names" \
    || fail "$where $PI_LIB_CONFIG_FILE_VARIABLE, which would replace the installed config with another file: nothing was changed; $how"
  fail "$where $(paste -sd ' ' - <<<"$names"), which would override the template's envoy_enabled, envoy_hub_url or concierge.envoy_url: nothing was changed; $how"
}

# require_no_manager_override AGAIN fails, before anything changes, while ghost-wispr's user manager
# sets PI_LIB_CONFIG_FILE_VARIABLE or one of PI_LIB_ENVOY_OVERRIDE_VARIABLES (pi-lib.sh's
# manager_override_names), or its environment cannot be read. AGAIN says which step to run again.
require_no_manager_override() {
  local names
  names=$(manager_override_names) \
    || fail "could not read the environment of $SERVICE_USER's user manager (systemctl --user show-environment), or it was empty: nothing was changed"
  refuse_overrides "$SERVICE_USER's user manager sets" \
    "unset each (systemctl --user unset-environment NAME; for an environment.d or user.conf line, remove it, then systemctl --user daemon-reload) and $1" \
    "$names"
}

# require_unit_files: after a reload of ghost-wispr's user manager, ghost-wispr.service and
# ghost-wispr-update.service are each loaded and have no drop-in, which could change how they run,
# such as set the recorder's environment (Environment=, EnvironmentFile=) past every file --models
# checks, or give the update a time limit. Otherwise it fails, naming the unit and its drop-ins,
# before anything changes.
require_unit_files() {
  local unit out load paths
  systemctl --user daemon-reload || fail "could not reload $SERVICE_USER's user manager: nothing was changed"
  for unit in ghost-wispr.service ghost-wispr-update.service; do
    out=$(systemctl --user show -p LoadState -p DropInPaths "$unit") \
      || fail "cannot read whether $unit is loaded: nothing was changed"
    load=$(sed -n 's/^LoadState=//p' <<<"$out")
    paths=$(sed -n 's/^DropInPaths=//p' <<<"$out")
    [[ $load == loaded ]] \
      || fail "$unit is ${load:-of no load state}, not loaded: nothing was changed; run provision.sh first"
    [[ -z $paths ]] \
      || fail "$unit has a drop-in ($paths), which can change how it runs, such as its environment or its time limits: nothing was changed; remove it and run --models again"
  done
}

# use_runtime_dir points the user manager's calls, this script's and pi-lib.sh's, at ghost-wispr's
# runtime directory, which holds the manager's socket, and fails while it does not exist.
use_runtime_dir() {
  local uid
  uid=$(id -u "$SERVICE_USER")
  runtime_dir="/run/user/$uid"
  [[ -d $runtime_dir ]] || fail "$runtime_dir is unavailable; log in as $SERVICE_USER before provisioning"
  XDG_RUNTIME_DIR=$runtime_dir
  export XDG_RUNTIME_DIR
}

# make_staging_dir makes a private directory in the runtime directory for this run, removed on exit.
make_staging_dir() {
  staging_dir=$(mktemp -d -p "$runtime_dir" ghost-wispr-provision.XXXXXX)
  chmod 0700 "$staging_dir"
  trap 'rm -rf -- "$staging_dir"' EXIT
}

# provision_key makes the Pi's P-256 key, mode 0600 in a directory of mode 0700, and prints its
# certificate request for CN=ghost-wispr on standard output. The key never leaves the Pi, and it
# refuses while the key exists.
provision_key() {
  (($# == 0)) || fail 'usage: provision.sh --key'
  require_service_user
  local dir
  [[ ! -e $IDENTITY_KEY && ! -L $IDENTITY_KEY ]] \
    || fail "$IDENTITY_KEY exists: the Pi has its key, and nothing was changed. A new key needs a new certificate request in the infrastructure's bootstrap"
  dir=$(dirname -- "$IDENTITY_KEY")
  if ! (umask 077 && mkdir -p -- "$dir" && openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$IDENTITY_KEY" -subj /CN=ghost-wispr); then
    rm -f -- "$IDENTITY_KEY"
    fail 'openssl could not make the key and its certificate request'
  fi
  chmod 0700 "$dir"
  chmod 0600 "$IDENTITY_KEY"
  printf 'ghost-wispr provision: made %s; its certificate request is above\n' "$IDENTITY_KEY" >&2
}

# provision_certificate FILE installs the Pi's first certificate as DEPLOY_DIR/device.crt, once FILE
# is one PEM certificate whose public key is the key's. It refuses while device.crt exists: the
# nightly update installs each renewed certificate.
provision_certificate() {
  (($# == 1)) || fail 'usage: provision.sh --certificate FILE'
  require_service_user
  local file=$1 count certificate_key key end
  [[ -f $IDENTITY_KEY ]] || fail "there is no key at $IDENTITY_KEY: run provision.sh --key first"
  [[ ! -e $DEPLOY_DIR/device.crt ]] \
    || fail "$DEPLOY_DIR/device.crt exists: the nightly update installs each renewed certificate; nothing was changed"
  [[ -f $file && -r $file ]] || fail "cannot read the certificate $file"
  count=$(grep -c -- '-----BEGIN CERTIFICATE-----' "$file") || count=0
  ((count == 1)) || fail "$file holds $count PEM certificates, not one; nothing was changed"
  certificate_key=$(openssl x509 -in "$file" -noout -pubkey 2>/dev/null) \
    || fail "$file is no PEM certificate; nothing was changed"
  key=$(openssl pkey -in "$IDENTITY_KEY" -pubout 2>/dev/null) || fail "cannot read the public key of $IDENTITY_KEY"
  [[ -n $certificate_key && $certificate_key == "$key" ]] \
    || fail "the certificate in $file is for another key than $IDENTITY_KEY; nothing was changed"
  end=$(openssl x509 -in "$file" -noout -enddate) || fail "cannot read the end date of $file"
  sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DEPLOY_DIR" "$DEPLOY_DIR/data"
  put_file "$file" "$DEPLOY_DIR/device.crt" 0644 || fail "could not install $DEPLOY_DIR/device.crt"
  printf 'ghost-wispr provision: installed %s, not after %s\n' "$DEPLOY_DIR/device.crt" "${end#notAfter=}"
}

# provision_models DRIVE_ID installs the release's config with the shared drive DRIVE_ID, which
# syncs to the drive and turns GC on (pi-lib.sh's render_config_with_drive), and restarts the
# recorder. GC removes only local audio that Drive holds a copy of. The nightly update keeps the
# drive in each later release's config. .env is the nightly update's, and this step writes none: the
# restart refreshes it (ghost-wispr.service's update.sh secrets).
provision_models() {
  [[ $# -eq 1 ]] || fail "usage: provision.sh --models <shared-drive-id>"
  local drive_id=$1 overrides
  valid_drive_id "$drive_id" \
    || fail "shared-drive ID must be non-empty and contain only letters, numbers, hyphens, and underscores"
  require_service_user
  check_release_directory
  require_release_the_pi_runs
  use_runtime_dir
  [[ -f "$DEPLOY_DIR/ghost-wispr.yaml" && -f "$DEPLOY_DIR/.env" ]] \
    || fail "missing $DEPLOY_DIR/ghost-wispr.yaml or $DEPLOY_DIR/.env; run provision.sh first"
  overrides=$(config_override_names "$DEPLOY_DIR/.env") || fail "could not read $DEPLOY_DIR/.env"
  refuse_overrides "$DEPLOY_DIR/.env can set" 'remove each such line and run --models again' "$overrides"
  require_unit_files
  require_no_manager_override 'run --models again'

  make_staging_dir
  render_config_with_drive "$script_dir/ghost-wispr.yaml" "$drive_id" >"$staging_dir/ghost-wispr.yaml" \
    || fail "could not render the models configuration from $script_dir/ghost-wispr.yaml"
  printf 'ghost-wispr provision: --models installs the approved models configuration. Settings saved on the page stay in the local settings file, %s, and apply over it.\n' "$DEPLOY_DIR/ghost-wispr.local.yaml"
  put_file "$staging_dir/ghost-wispr.yaml" "$DEPLOY_DIR/ghost-wispr.yaml" 0640 || fail "could not install $DEPLOY_DIR/ghost-wispr.yaml"
  systemctl --user restart ghost-wispr.service
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
case ${1:-} in
  --models)
    shift
    provision_models "$@"
    exit
    ;;
  --key)
    shift
    provision_key "$@"
    exit
    ;;
  --certificate)
    shift
    provision_certificate "$@"
    exit
    ;;
esac

(($# == 0)) || fail "$USAGE"
require_service_user
refuse_input
use_runtime_dir
[[ -f $IDENTITY_KEY ]] || fail "there is no key at $IDENTITY_KEY: run provision.sh --key, then provision.sh --certificate FILE, first"
[[ -f $DEPLOY_DIR/device.crt ]] || fail "there is no certificate at $DEPLOY_DIR/device.crt: run provision.sh --certificate FILE first"
require_release_the_pi_runs

# The recorder inherits its user manager's environment. Reload the manager first, so that an
# environment.d or user.conf line written since its last reload counts.
systemctl --user daemon-reload \
  || fail "could not reload $SERVICE_USER's user manager: nothing was changed; start it (sudo systemctl start user@$(id -u "$SERVICE_USER").service) and run provision.sh again"
require_no_manager_override 'run provision.sh again'

# jq, which reads the manifest, comes with the packages.
sudo apt-get update
sudo apt-get install -y --no-install-recommends libportaudio2 ffmpeg lame jq curl
check_release_directory
read_pi_env
read_serve_conf
readonly TAILSCALE_HOSTNAME="$PI_HOST."
make_staging_dir

sudo install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$DEPLOY_DIR" "$DEPLOY_DIR/data"
install_credentials
compare_client_id

# plugdev opens the blink(1), input the power button and the Jabra's mute button and light, and
# audio the microphone and speaker: a lingering user service has no seat, so no seat rule gives it
# the ALSA devices.
groups_changed=false
for group in plugdev input audio; do
  if ! id -nG "$SERVICE_USER" | tr ' ' '\n' | grep -Fxq "$group"; then
    sudo usermod -aG "$group" "$SERVICE_USER"
    groups_changed=true
  fi
done

# The bootstrap files, and the release's update.sh, pi-lib.sh and pi.env, which the update that
# provision.sh starts below runs on. That update installs the rest of the release's deploy set, the
# udev rule, the logind setting and .env among them. The update's journal is printed from the time
# before the release's timer is installed: its schedule can have a time the installed timer lacks,
# such as 03:45, and the reload that loads it can start the update at once (systemd#16732).
update_started=$(date +%s)
install -d -m 0755 -- "$UNIT_DIR"
put_file "$script_dir/ghost-wispr-update-launcher.sh" "$DEPLOY_DIR/ghost-wispr-update-launcher.sh" 0755 \
  || fail "could not install the launcher"
for unit in ghost-wispr-update.service ghost-wispr-update.timer; do
  put_file "$script_dir/$unit" "$UNIT_DIR/$unit" 0644 || fail "could not install $unit"
done
put_file "$script_dir/update.sh" "$DEPLOY_DIR/update.sh" 0755 || fail "could not install update.sh"
put_file "$script_dir/pi-lib.sh" "$DEPLOY_DIR/pi-lib.sh" 0755 || fail "could not install pi-lib.sh"
put_file "$script_dir/pi.env" "$DEPLOY_DIR/pi.env" 0644 || fail "could not install pi.env"

if $groups_changed; then
  sudo systemctl restart "user@$(id -u "$SERVICE_USER").service"
fi
sudo loginctl enable-linger "$SERVICE_USER"
sudo systemctl start "user@$(id -u "$SERVICE_USER").service"
systemctl --user daemon-reload
systemctl --user enable --now ghost-wispr-update.timer

# The nightly update, now: it installs the latest release's whole deploy set and its secrets, and
# restarts the recorder on them. A start of the unit, never a run of the launcher or update.sh, so
# systemd joins it with a start of the timer at the same moment. The unit's lines are in the system
# journal: the Pi keeps no user journal.
printf 'ghost-wispr provision: starting the nightly update now (systemctl --user start ghost-wispr-update.service)\n'
update_status=0
systemctl --user start ghost-wispr-update.service || update_status=$?
printf 'ghost-wispr provision: the journal of ghost-wispr-update.service for this run:\n'
sudo journalctl _SYSTEMD_USER_UNIT=ghost-wispr-update.service --since="@$update_started" --no-pager --output=cat \
  || printf 'ghost-wispr provision: could not read the journal of ghost-wispr-update.service\n'
((update_status == 0)) \
  || fail "the nightly update failed (systemctl --user start ghost-wispr-update.service exited $update_status): its lines are above; fix the cause and run provision.sh again"
systemctl --user enable ghost-wispr.service

apply_serve

self_status=$(tailscale status --self --json)
if jq -e --arg hostname "$TAILSCALE_HOSTNAME" '
  .Self.Tags == ["tag:ghost-wispr"] and .Self.DNSName == $hostname
' <<<"$self_status" >/dev/null; then
  printf 'ghost-wispr provision: already joined as tag:ghost-wispr at %s\n' "$TAILSCALE_HOSTNAME"
  exit 0
fi

readonly token_audience="api.tailscale.com/$PI_TAILNET_CLIENT_ID"
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
if ! jq -e --arg subject "$PI_SERVICE_ACCOUNT_UNIQUE_ID" --arg audience "$token_audience" '
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
# The command holds each line of serve.conf as a tailscale serve call: the join runs after P2's
# command has removed the release's directory, so it reads no file of it.
join_serve=''
while read -r port target; do
  if [[ -n $port ]]; then
    join_serve+=" tailscale serve --bg --https $port $target || rc=\$?;"
  fi
done <<<"$serve_lines"
join_command="tailscale up --force-reauth --client-id='$PI_TAILNET_CLIENT_ID?ephemeral=false&preauthorized=true'"
join_command+=" --id-token=file:$JOIN_TOKEN --advertise-tags=tag:ghost-wispr --hostname=ghost-wispr --ssh --accept-routes=false;"
join_command+=" rc=\$?; rm -f $JOIN_TOKEN;"
join_command+=" if [ \$rc -eq 0 ]; then"
join_command+="$join_serve"
join_command+=" if /bin/sh $JOIN_ROUTE_CHECK; then tailscale set --accept-routes || rc=\$?; else rc=\$?; fi;"
join_command+=" fi; rm -f $JOIN_ROUTE_CHECK; exit \$rc"
printf 'ghost-wispr provision: starting the tagged tailnet join as %s\n' "$join_unit"
# systemd-run(1): the service manager expands ${NAME} and $$ in the command's arguments unless
# --expand-environment=no, which systemd 254 added (Raspberry Pi OS Trixie has 257). The join
# command is shell code for the unit's /bin/sh, which must get it as written.
sudo systemd-run --expand-environment=no --unit="$join_unit" /bin/sh -c "$join_command"
printf 'ghost-wispr provision: reconnect with Tailscale SSH, then check tailscale status and sudo journalctl -u %s.\n' "$join_unit"
