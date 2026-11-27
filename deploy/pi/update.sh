#!/usr/bin/env bash
# Install the latest Ghost Wispr release's whole deploy set on the office Pi, as one unit, unless the
# recorder is busy; or refresh the secret lines of .env alone.
#
#   update.sh           the nightly run: ghost-wispr-update-launcher.sh runs it, under its time limit
#   update.sh secrets   ghost-wispr.service's ExecStartPre: write the four lines of .env that this
#                       script owns, the secrets from Secret Manager; install nothing else, and
#                       restart nothing
#
# Each release carries its deploy set in ghost-wispr-pi-deploy.tar.gz, beside its binary: nine files
# and the binary, which this script installs and rolls back together, and the five provisioning
# files, which only provision.sh installs and this script only compares (the launcher, the update
# unit and timer, and serve.conf; and provision.sh itself). manifest.json names the SHA-256 of the
# binary and of every other file. deploy.verified records the set this script installed and saw
# answer its commit last; the .prev copies are that set.
#
# Exit 0: installed, already current, held, or the recorder busy. Exit 10: a failure this script
# found and reported through fail, transient ones included. Any other exit means this script broke,
# and the launcher runs update.sh.prev.
set -euo pipefail

self_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
self_name=${BASH_SOURCE[0]##*/}
# update.sh sources pi-lib.sh, and update.sh.prev sources pi-lib.sh.prev.
lib="$self_dir/pi-lib.sh${self_name#update.sh}"
if [[ ! -r $lib ]]; then
  printf 'ghost-wispr update: cannot read its library %s\n' "$lib" >&2
  exit 2
fi
# shellcheck source=/dev/null
source "$lib"

readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly UNIT_DIR="${GHOST_WISPR_UNIT_DIR:-${HOME:-/home/ghost-wispr}/.config/systemd/user}"
# The root under which the udev rule and the logind setting are installed.
readonly SYSTEM_ROOT="${GHOST_WISPR_SYSTEM_ROOT:-/}"
# Where the running recorder's image is read (running_image_is_installed); a test points it at a
# fake.
readonly PROC_DIR="${GHOST_WISPR_PROC_DIR:-/proc}"
readonly IDENTITY_KEY="${GHOST_WISPR_IDENTITY_KEY:-/home/ghost-wispr/.config/ghost-wispr-identity/key.pem}"
readonly RELEASE_API="${GHOST_WISPR_RELEASE_API:-https://api.github.com/repos/sjawhar/ghost-wispr/releases/latest}"
readonly STATUS_URL="${GHOST_WISPR_STATUS_URL:-http://127.0.0.1:8080/api/status}"
readonly VERSION_URL="${GHOST_WISPR_VERSION_URL:-http://127.0.0.1:8080/api/version}"
readonly BINARY="$DEPLOY_DIR/ghost-wispr"
readonly VERIFIED="$DEPLOY_DIR/deploy.verified"
# The record of the update.sh before the deploy set, which installed the binary alone. The first
# install of a set removes it.
readonly OLD_VERIFIED="$DEPLOY_DIR/ghost-wispr.verified"
# The journal of a swap of the certificate pair, device.crt and device.crt.version: the version the
# pair gets (or none) and the SHA-256 of the certificate it installs (install_certificate_pair).
readonly CERTIFICATE_JOURNAL="$DEPLOY_DIR/device.crt.version.new"
# A rollback reads the status of a recorder whose state it could not read again every RECHECK_EVERY
# seconds, and at most ROLLBACK_READS times.
readonly RECHECK_EVERY=120 ROLLBACK_READS=3
# The deploy set, with the binary, ghost-wispr (C2).
readonly SET_FILES=(ghost-wispr.yaml pi.env update.sh pi-lib.sh wait-for-network.sh wait-for-time-sync.sh
  ghost-wispr.service 99-ghost-wispr.rules logind-power-key.conf)
readonly PROVISIONING_FILES=(provision.sh ghost-wispr-update-launcher.sh ghost-wispr-update.service
  ghost-wispr-update.timer serve.conf)
readonly NAME_PATTERN='^[A-Za-z0-9_][A-Za-z0-9._-]*$'
readonly COMMIT_PATTERN='^commit [0-9a-f]{40}$'
readonly CERTIFICATE_PATTERN='^pi-secrets: certificate version ([0-9]+), not after ([^ ]+): newer and signs in$'
# The states a call of /api/status can have (INFRA-5904's status contract), as a JSON array.
readonly CALL_STATES='["none","checking","live"]'
# Each step that waits has a bound of its own: each curl its --max-time, pi-secrets 60 s and a 5 s
# grace, each sudo -n call 30 s, the Serve read 10 s, the version wait 120 s, and a rollback's
# ROLLBACK_READS status reads. A restart of the recorder ends within its unit's own timeouts. The
# script has no limit on a whole run: the launcher's is the one.

# The staging directory of this run, in DEPLOY_DIR, so each staged file goes in place by a rename.
stage=''
trap 'if [[ -n $stage ]]; then rm -rf -- "$stage"; fi' EXIT
status_json='' release_json='' release_commit='' set_dir='' manifest_json='' binary_sha256=''
tarball_url='' tarball_checksum_url='' binary_url='' binary_checksum_url=''
secrets_file='' secrets_failure='' certificate_staged='' certificate_version='' certificate_end=''
swapped=() changed_system=() restore_problems='' restore_note=''
# A release install whose installed set is not the one deploy.verified lists (a run cut short)
# keeps no .prev copies, and a rollback of it restores every file that has one (restore_previous).
restore_all=false

fail() {
  printf 'ghost-wispr update: %s\n' "$*" >&2
  exit 10
}

sum() {
  local line
  line=$(sha256sum -- "$1") || return 1
  printf '%s\n' "${line%% *}"
}

# installed_path NAME prints where the Pi keeps the file NAME of the deploy set, or the binary
# (ghost-wispr), .env or device.crt.
installed_path() {
  case $1 in
    ghost-wispr.service) printf '%s\n' "$UNIT_DIR/$1" ;;
    99-ghost-wispr.rules) printf '%s\n' "${SYSTEM_ROOT%/}/etc/udev/rules.d/99-ghost-wispr.rules" ;;
    logind-power-key.conf) printf '%s\n' "${SYSTEM_ROOT%/}/etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf" ;;
    *) printf '%s\n' "$DEPLOY_DIR/$1" ;;
  esac
}

file_mode() {
  case $1 in
    update.sh | pi-lib.sh | wait-for-network.sh | wait-for-time-sync.sh | ghost-wispr) echo 0755 ;;
    ghost-wispr.yaml) echo 0640 ;;
    .env) echo 0600 ;;
    *) echo 0644 ;;
  esac
}

is_system_file() {
  [[ $1 == 99-ghost-wispr.rules || $1 == logind-power-key.conf ]]
}

make_stage() {
  stage=$(mktemp -d -p "$DEPLOY_DIR" .ghost-wispr-update.XXXXXX) || fail "cannot make a staging directory in $DEPLOY_DIR"
  mkdir -- "$stage/new" || fail "cannot make a staging directory in $DEPLOY_DIR"
}

# remove_stale_stages removes each staging directory of an earlier run that is older than 46
# minutes, the launcher's 45-minute limit and its 60 s grace: a run that limit killed left it, and
# it can hold the secrets that run read. A younger one may belong to a run still going, and stays.
# A failure here only leaves those directories, and the run goes on.
remove_stale_stages() {
  find "$DEPLOY_DIR" -mindepth 1 -maxdepth 1 -type d -name '.ghost-wispr-update.*' -mmin +46 -exec rm -rf -- {} + \
    || printf 'ghost-wispr update: could not remove each staging directory older than 46 minutes in %s\n' "$DEPLOY_DIR"
}

recorder_active() {
  systemctl --user is-active --quiet ghost-wispr.service
}

# recorder_status prints the recorder's /api/status, or nothing when it does not answer, or answers
# something that is not its status (pi-lib.sh's PI_LIB_RECORDER_STATUS_FILTER).
recorder_status() {
  local answer
  answer=$(curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$STATUS_URL" 2>/dev/null) || return 0
  jq -e "$PI_LIB_RECORDER_STATUS_FILTER" >/dev/null 2>&1 <<<"$answer" || return 0
  printf '%s' "$answer"
}

# busy_reason STATUS prints why the recorder may not restart, from its /api/status (STATUS), with
# pi-lib.sh's PI_LIB_BUSY_REASON_FILTER: it records a session, or it holds a call (call.state is not
# none). It prints nothing when it may restart, or when it gave no status.
busy_reason() {
  [[ -n $1 ]] || return 0
  jq -r "$PI_LIB_BUSY_REASON_FILTER" <<<"$1" 2>/dev/null || printf 'status unreadable'
}

# leave_if_busy reads the recorder's status into status_json. While the recorder records a session or
# holds a call, it ends the run with exit 0, changing nothing; while the recorder runs but gives no
# status, it fails. It runs before any change, and again just before each swap; after a release's
# swap, leave_if_busy_before_restart reads the status again just before its restart.
leave_if_busy() {
  local reason
  status_json=$(recorder_status)
  reason=$(busy_reason "$status_json")
  if [[ -n $reason ]]; then
    printf 'ghost-wispr update: %s; leaving the current release in place\n' "$reason"
    exit 0
  fi
  if [[ -z $status_json ]] && recorder_active; then
    fail "ghost-wispr.service is active but /api/status gave no status"
  fi
}

# restartable WHY returns once the recorder may restart for a rollback, whose files are restored: its
# status answers and busy_reason finds nothing, once, or, after a read that gave no status, on two
# reads RECHECK_EVERY seconds apart. A recorder that answers that it may not restart is left running,
# and so is one whose state no read of ROLLBACK_READS could read: it may be recording, and a restart
# would end that. It starts on the restored files at its next start.
restartable() {
  local why=$1 status reason reads=0 good=0 unread=false
  while :; do
    status=$(recorder_status)
    reads=$((reads + 1))
    if [[ -n $status ]]; then
      reason=$(busy_reason "$status")
      [[ -z $reason ]] \
        || fail "$why; restored the previous release's files, but the recorder reports $reason, so it is left running on the new release until its next start"
      good=$((good + 1))
      if ! $unread || ((good >= 2)); then
        return 0
      fi
    else
      unread=true good=0
    fi
    ((reads < ROLLBACK_READS)) \
      || fail "$why; restored the previous release's files, but ghost-wispr.service runs and its state could not be read on $reads reads $RECHECK_EVERY s apart (/api/status gave no status), so it may be recording and is left running on the new release until its next start"
    sleep "$RECHECK_EVERY"
  done
}

# wait_for_commit COMMIT returns once the recorder answers /api/version with COMMIT and /api/status
# with a status that recorder_status accepts, whose call is absent or an object whose state is one
# of CALL_STATES, whether or not a session is in progress (R8), within 120 s. A release whose state
# the update cannot read (no status, or a call it cannot read) would stop each later run at its
# first status read, before it could install a fix. It reads the call itself: busy_reason reads the
# session first, and so never reads the call during a session.
wait_for_commit() {
  local deadline answer commit status
  deadline=$(($(date +%s) + 120))
  while (($(date +%s) < deadline)); do
    answer=$(curl --connect-timeout 5 --max-time 10 --fail --silent --show-error "$VERSION_URL" 2>/dev/null) || answer=''
    if [[ -n $answer ]]; then
      commit=$(jq -r '.commit // empty' <<<"$answer" 2>/dev/null) || commit=''
      if [[ $commit == "$1" ]]; then
        status=$(recorder_status)
        if [[ -n $status ]] && jq -e --argjson states "$CALL_STATES" \
          '.call == null or ((.call | type) == "object" and (.call.state as $state | any($states[]; . == $state)))' \
          >/dev/null 2>&1 <<<"$status"; then
          return 0
        fi
      fi
    fi
    sleep 2
  done
  return 1
}

# load_pi_env reads the Pi's deployment settings from the installed pi.env: its Google project and
# the names of its secrets in Secret Manager.
load_pi_env() {
  local file=$DEPLOY_DIR/pi.env
  [[ -r $file ]] || fail "cannot read the Pi's settings in $file"
  # shellcheck source=/dev/null
  source "$file" || fail "cannot read the Pi's settings in $file"
  [[ -n ${PI_GCP_PROJECT:-} && -n ${PI_DEEPGRAM_SECRET:-} && -n ${PI_DISPATCH_TOKEN_SECRET:-} && -n ${PI_CERTIFICATE_SECRET:-} ]] \
    || fail "$file does not set PI_GCP_PROJECT, PI_DEEPGRAM_SECRET, PI_DISPATCH_TOKEN_SECRET and PI_CERTIFICATE_SECRET"
}

# secret_lines_valid FILE: FILE, the secrets.env pi-secrets wrote, holds the two secret lines, in
# order, each with a value.
secret_lines_valid() {
  local -a lines
  mapfile -t lines <"$1" || return 1
  ((${#lines[@]} == 2)) || return 1
  [[ ${lines[0]} == GHOST_WISPR_DEEPGRAM_API_KEY=?* && ${lines[1]} == GHOST_WISPR_DISPATCH_TOKEN=?* ]]
}

# read_secrets BINARY runs BINARY's pi-secrets (C4) under its bound, and passes its lines through. It
# sets secrets_file to the secrets.env it wrote, or secrets_failure to why there is none, and
# certificate_staged to a newer certificate that signs in, with its version and end date. A failed
# key read (exit 1) still lets pi-secrets' certificate step run (R10), so a certificate it staged
# and named counts after an exit 0 or 1; one that a timeout cut short never does. A failure here
# keeps the secret lines .env has; the caller decides what else it means. When device.crt.version
# holds the installed certificate's version, one positive integer, pi-secrets gets it (T1.1's
# --installed-certificate-version).
read_secrets() {
  local binary=$1 out=$stage/secrets output status=0 line version='' end='' installed_version='' version_args=()
  load_pi_env
  mkdir -m 0700 -- "$out" || fail "cannot make $out"
  if [[ -f $DEPLOY_DIR/device.crt.version ]]; then
    installed_version=$(<"$DEPLOY_DIR/device.crt.version") || installed_version=''
  fi
  if [[ $installed_version =~ ^[1-9][0-9]{0,9}$ ]]; then
    version_args=(--installed-certificate-version "$installed_version")
  fi
  output=$(timeout --kill-after=5s 60s "$binary" pi-secrets --project "$PI_GCP_PROJECT" \
    --credentials "$DEPLOY_DIR/google-credentials.json" \
    --env-secret GHOST_WISPR_DEEPGRAM_API_KEY="$PI_DEEPGRAM_SECRET" \
    --env-secret GHOST_WISPR_DISPATCH_TOKEN="$PI_DISPATCH_TOKEN_SECRET" \
    --certificate-secret "$PI_CERTIFICATE_SECRET" --key "$IDENTITY_KEY" \
    --installed-certificate "$DEPLOY_DIR/device.crt" "${version_args[@]}" --out "$out" 2>&1) || status=$?
  if [[ -n $output ]]; then
    printf '%s\n' "$output"
  fi
  case $status in
    0)
      if [[ -f $out/secrets.env ]] && secret_lines_valid "$out/secrets.env"; then
        secrets_file=$out/secrets.env
      else
        secrets_failure='pi-secrets wrote no secrets.env with the two secret lines'
      fi
      ;;
    124 | 137)
      secrets_failure='pi-secrets did not end within its 60 s bound'
      return 0
      ;;
    *) secrets_failure="pi-secrets failed with exit $status" ;;
  esac
  ((status <= 1)) || return 0
  while IFS= read -r line; do
    if [[ $line =~ $CERTIFICATE_PATTERN ]]; then
      version=${BASH_REMATCH[1]} end=${BASH_REMATCH[2]}
    fi
  done <<<"$output"
  if [[ -f $out/device.crt && ! -L $out/device.crt && -n $version ]]; then
    certificate_staged=$out/device.crt certificate_version=$version certificate_end=$end
  fi
}

# render_env OUT writes the .env to install (R9), mode 0600: GHOST_WISPR_ADDR and
# GOOGLE_API_USE_CLIENT_CERTIFICATE, then the two secret lines pi-secrets wrote, or the ones systemd
# reads from the current .env when it failed, then every other line of the current .env but the
# variables Ghost Wispr no longer reads.
render_env() {
  local out=$1 env=$DEPLOY_DIR/.env
  printf '%s\n' GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false >"$out" || return 1
  chmod 0600 "$out" || return 1
  if [[ -n $secrets_file ]]; then
    cat -- "$secrets_file" >>"$out" || return 1
  elif [[ -f $env ]]; then
    env_secret_lines "$env" >>"$out" || return 1
  fi
  if [[ -f $env ]]; then
    env_kept_lines "$env" >>"$out" || return 1
  fi
}

# stage_certificate puts the certificate pi-secrets staged in the staging directory as device.crt;
# swap_in installs it with its version, the one pi-secrets named, as device.crt.version.
stage_certificate() {
  install -m 0644 -- "$certificate_staged" "$stage/new/device.crt" || fail "could not stage the new certificate"
}

# asset_url VARIABLE NAME sets VARIABLE to the URL of the latest release's asset NAME.
asset_url() {
  local url
  url=$(jq -r --arg name "$2" '[.assets[]? | select(.name == $name) | .browser_download_url | strings]
    | if length == 1 then .[0] else empty end' <<<"$release_json" 2>/dev/null) || url=''
  [[ -n $url ]] || fail "the latest release has no asset $2"
  printf -v "$1" '%s' "$url"
}

download() {
  curl --connect-timeout 5 --max-time "$3" --fail --silent --show-error --location "$1" --output "$2" \
    || fail "could not download $1"
}

# check_asset FILE CHECKSUM_FILE NAME fails unless the release's checksum asset names FILE's SHA-256.
check_asset() {
  local want have
  want=$(awk 'NR == 1 { print $1 }' "$2") || want=''
  have=$(sum "$1") || fail "cannot read $1"
  [[ $want =~ ^[0-9a-f]{64}$ && $want == "$have" ]] || fail "the release's $3.sha256 does not match $3; nothing was installed"
}

# extract_set unpacks the release's tarball into the staging directory, after it checks that each
# entry is ghost-wispr-pi-deploy/ or a plain name directly in it.
extract_set() {
  local tarball=$stage/ghost-wispr-pi-deploy.tar.gz listing entry name
  listing=$(tar -tzf "$tarball") || fail "cannot read the release's ghost-wispr-pi-deploy.tar.gz; nothing was installed"
  while IFS= read -r entry; do
    name=${entry#ghost-wispr-pi-deploy/}
    if [[ $name == "$entry" || ($name != '' && ! $name =~ $NAME_PATTERN) ]]; then
      fail "the release's tarball holds $entry, which is not a file directly in ghost-wispr-pi-deploy/; nothing was installed"
    fi
  done <<<"$listing"
  tar -xzf "$tarball" -C "$stage" --no-same-owner --no-same-permissions \
    || fail "cannot unpack the release's ghost-wispr-pi-deploy.tar.gz; nothing was installed"
  set_dir=$stage/ghost-wispr-pi-deploy
  [[ -d $set_dir ]] || fail "the release's tarball has no ghost-wispr-pi-deploy/; nothing was installed"
}

# check_manifest fails unless manifest.json is format 1 of the release's commit, names the binary's
# SHA-256 and every other file of the directory, each regular file there matches its SHA-256, and
# the set and the provisioning files are all there.
check_manifest() {
  local file name want listed
  manifest_json=$set_dir/manifest.json
  [[ -f $manifest_json && ! -L $manifest_json ]] || fail "the release's tarball has no manifest.json; nothing was installed"
  jq -e --arg commit "$release_commit" 'type == "object" and .format == 1 and .commit == $commit
      and (.binary_sha256 | type == "string" and test("^[0-9a-f]{64}$"))
      and (.files | type == "object")
      and ((has("link_versions") | not) or (.link_versions | type == "array"
        and all(.[]; type == "number" and . > 0 and . == floor)))' "$manifest_json" >/dev/null 2>&1 \
    || fail "the release's manifest.json is not format 1 of commit $release_commit, with binary_sha256, files and valid link_versions; nothing was installed"
  listed=$(jq -r '.files | keys[]' "$manifest_json") || fail "cannot read the files of the release's manifest.json"
  for file in "$set_dir"/*; do
    name=${file##*/}
    if [[ $name != manifest.json ]] && ! grep -Fqx -- "$name" <<<"$listed"; then
      fail "the release's $name is not in its manifest.json; nothing was installed"
    fi
  done
  while IFS= read -r name; do
    [[ $name =~ $NAME_PATTERN && $name != manifest.json ]] || fail "the release's manifest.json names the file $name; nothing was installed"
    file=$set_dir/$name
    [[ -f $file && ! -L $file ]] || fail "the release's manifest.json names $name, which its tarball does not hold; nothing was installed"
    want=$(jq -r --arg name "$name" '.files[$name]' "$manifest_json")
    [[ $(sum "$file") == "$want" ]] || fail "the release's $name does not match its manifest.json; nothing was installed"
  done <<<"$listed"
  for name in "${SET_FILES[@]}" "${PROVISIONING_FILES[@]}"; do
    grep -Fqx -- "$name" <<<"$listed" || fail "the release's manifest.json does not name $name; nothing was installed"
  done
  binary_sha256=$(jq -r .binary_sha256 "$manifest_json")
}

# installed_set_is_verified: deploy.verified lists the set and the binary, and each installed file
# of them but ghost-wispr.yaml, which is rendered, has the SHA-256 it lists.
installed_set_is_verified() {
  local -a lines
  local line want name path have seen=' '
  [[ -f $VERIFIED ]] || return 1
  mapfile -t lines <"$VERIFIED" || return 1
  [[ ${lines[0]:-} =~ $COMMIT_PATTERN ]] || return 1
  for line in "${lines[@]:1}"; do
    want=${line%%  *} name=${line#*  }
    [[ $want =~ ^[0-9a-f]{64}$ && $line == "$want  $name" ]] || return 1
    case " ${SET_FILES[*]} ghost-wispr " in
      *" $name "*) ;;
      *) return 1 ;;
    esac
    seen+="$name "
    [[ $name != ghost-wispr.yaml ]] || continue
    path=$(installed_path "$name")
    [[ -f $path ]] || return 1
    have=$(sum "$path") || return 1
    [[ $have == "$want" ]] || return 1
  done
  for name in "${SET_FILES[@]}" ghost-wispr; do
    [[ $seen == *" $name "* ]] || return 1
  done
}

release_is_current() {
  installed_set_is_verified || return 1
  [[ $(head -n1 -- "$VERIFIED") == "commit $release_commit" ]]
}

write_verified() {
  local name
  {
    printf 'commit %s\n' "$release_commit"
    for name in "${SET_FILES[@]}"; do
      printf '%s  %s\n' "$(jq -r --arg name "$name" '.files[$name]' "$manifest_json")" "$name"
    done
    printf '%s  ghost-wispr\n' "$binary_sha256"
  } >"$VERIFIED.tmp" || return 1
  mv -f -- "$VERIFIED.tmp" "$VERIFIED"
}

# running_image_is_installed: the image the recorder's main process runs is the installed binary (by
# SHA-256), so that binary answered its commit. Otherwise, or when the image cannot be read, it says
# why.
running_image_is_installed() {
  local pid running installed
  pid=$(systemctl --user show -p MainPID --value ghost-wispr.service 2>/dev/null) || pid=''
  if [[ $pid =~ ^[1-9][0-9]*$ && -f $BINARY ]] && running=$(sha256sum <"$PROC_DIR/$pid/exe" 2>/dev/null) \
    && installed=$(sha256sum <"$BINARY") && [[ ${running%% *} == "${installed%% *}" ]]; then
    return 0
  fi
  printf 'ghost-wispr update: %s is not the image the recorder runs (main process %s), or that image cannot be read, so the installed files are not kept as the previous release\n' \
    "$BINARY" "${pid:-unknown}"
  return 1
}

# old_record_names_binary: ghost-wispr.verified, the record of the update.sh before the deploy set,
# names the installed binary by its SHA-256: that update.sh saw the binary run and answer its commit.
old_record_names_binary() {
  local want have
  [[ -f $OLD_VERIFIED && -f $BINARY ]] || return 1
  want=$(<"$OLD_VERIFIED") || return 1
  have=$(sum "$BINARY") || return 1
  [[ $want =~ ^[0-9a-f]{64}$ && $want == "$have" ]]
}

# keep_previous NAME... copies each installed file NAME to NAME.prev in DEPLOY_DIR, by a copy to a
# temporary name and a rename, so a kill leaves the old copy or the new one. A NAME the Pi does not
# have loses its old copy, so the .prev copies are one set. While this script runs as
# update.sh.prev, the launcher's fallback after a release's update.sh broke, it keeps
# update.sh.prev and pi-lib.sh.prev as they are: they are the scripts that run, and a copy of the
# broken update.sh there would leave no update that runs once a second such release installs.
keep_previous() {
  local name path
  for name in "$@"; do
    if [[ $self_name == update.sh.prev && ($name == update.sh || $name == pi-lib.sh) ]]; then
      continue
    fi
    path=$(installed_path "$name")
    if [[ -f $path ]]; then
      cp --preserve=mode -- "$path" "$DEPLOY_DIR/.$name.prev.tmp" || fail "could not keep $path as $DEPLOY_DIR/$name.prev"
      mv -f -- "$DEPLOY_DIR/.$name.prev.tmp" "$DEPLOY_DIR/$name.prev" || fail "could not keep $path as $DEPLOY_DIR/$name.prev"
    else
      rm -f -- "$DEPLOY_DIR/$name.prev" || fail "could not remove $DEPLOY_DIR/$name.prev"
    fi
  done
}

# put_in_place FILE PATH installs FILE at PATH by a rename, through a copy in PATH's directory when
# FILE is on another filesystem: a copy over a binary that runs fails with "Text file busy", and a
# rename leaves a reader of the old file, such as a running update.sh, reading it to its end.
put_in_place() {
  local file=$1 path=$2 dir
  dir=$(dirname -- "$path")
  mkdir -p -- "$dir" || return 1
  if [[ $(stat -c %d -- "$(dirname -- "$file")") == "$(stat -c %d -- "$dir")" ]]; then
    mv -f -- "$file" "$path"
    return
  fi
  cp --preserve=mode -- "$file" "$dir/.${path##*/}.new" || return 1
  mv -f -- "$dir/.${path##*/}.new" "$path"
}

# install_certificate_pair FILE VERSION installs FILE as device.crt, and VERSION (a number, or none
# for no version file) as device.crt.version, as one change that a kill cannot split: it writes the
# journal CERTIFICATE_JOURNAL first, then renames FILE over device.crt, then writes or removes the
# version file (finish_certificate_pair). A run cut short in between leaves the journal, which the
# next run finishes or drops (reconcile_certificate_journal). Each step is a rename or a removal.
install_certificate_pair() {
  local file=$1 version=$2 sha
  sha=$(sum "$file") || return 1
  printf 'version %s\nsha256 %s\n' "$version" "$sha" >"$DEPLOY_DIR/.device.crt.version.new.tmp" || return 1
  chmod 0644 "$DEPLOY_DIR/.device.crt.version.new.tmp" || return 1
  mv -f -- "$DEPLOY_DIR/.device.crt.version.new.tmp" "$CERTIFICATE_JOURNAL" || return 1
  put_in_place "$file" "$DEPLOY_DIR/device.crt" || return 1
  finish_certificate_pair "$version"
}

# finish_certificate_pair VERSION makes device.crt.version VERSION, mode 0644, by a rename, or
# removes it for none, then removes the journal.
finish_certificate_pair() {
  local tmp=$DEPLOY_DIR/.device.crt.version.tmp
  if [[ $1 == none ]]; then
    rm -f -- "$DEPLOY_DIR/device.crt.version" || return 1
  elif ! { printf '%s\n' "$1" >"$tmp" && chmod 0644 "$tmp" && mv -f -- "$tmp" "$DEPLOY_DIR/device.crt.version"; }; then
    rm -f -- "$tmp"
    return 1
  fi
  rm -f -- "$CERTIFICATE_JOURNAL"
}

# reconcile_certificate_journal puts right a certificate pair that a run cut short left: with the
# journal's certificate installed, it finishes the pair; with any other, the certificate never went
# in, and it removes the journal, so the pair before stays. A journal it cannot read is removed too.
# Each run does this first: pi-secrets stages no certificate that does not end later than the
# installed device.crt, so no later read would correct a mismatched pair.
reconcile_certificate_journal() {
  local -a lines=()
  local version='' sha=''
  [[ -e $CERTIFICATE_JOURNAL ]] || return 0
  mapfile -t lines <"$CERTIFICATE_JOURNAL" || lines=()
  if ((${#lines[@]} == 2)) && [[ ${lines[0]} =~ ^version\ ([0-9]+|none)$ ]]; then
    version=${BASH_REMATCH[1]}
    if [[ ${lines[1]} =~ ^sha256\ ([0-9a-f]{64})$ ]]; then
      sha=${BASH_REMATCH[1]}
    fi
  fi
  if [[ -n $sha && -f $DEPLOY_DIR/device.crt && $(sum "$DEPLOY_DIR/device.crt") == "$sha" ]]; then
    finish_certificate_pair "$version" \
      || fail "could not finish the certificate swap that a run cut short ($CERTIFICATE_JOURNAL)"
    printf 'ghost-wispr update: finished the certificate swap that a run cut short: device.crt.version is %s\n' "$version"
    return 0
  fi
  rm -f -- "$CERTIFICATE_JOURNAL" || fail "could not remove $CERTIFICATE_JOURNAL"
  printf 'ghost-wispr update: removed %s, of a run cut short before its certificate went in; device.crt and device.crt.version stay as they are\n' \
    "$CERTIFICATE_JOURNAL"
}

# sudo_install SOURCE PATH installs a system file with sudo -n, under its bound.
sudo_install() {
  timeout 30s sudo -n install -D -m 0644 -- "$1" "$2"
}

# reload_system NAME... reloads udev after the udev rule changed, and restarts logind after the
# logind setting changed, each sudo -n call under its bound: udevadm control --reload, then udevadm
# trigger.
reload_system() {
  local name
  for name in "$@"; do
    case $name in
      99-ghost-wispr.rules)
        timeout 30s sudo -n udevadm control --reload || return 1
        timeout 30s sudo -n udevadm trigger || return 1
        ;;
      logind-power-key.conf)
        timeout 30s sudo -n systemctl restart systemd-logind || return 1
        ;;
    esac
  done
}

# swap_in NAME... puts each staged file NAME ($stage/new/NAME) in place, and notes it in swapped,
# and each system file that changed in changed_system. A system file equal to the installed one is
# left as it is. device.crt goes in with its version file, device.crt.version, as one change
# (install_certificate_pair). It returns non-zero at the first file it could not install.
swap_in() {
  local name path
  for name in "$@"; do
    path=$(installed_path "$name")
    if [[ $name == device.crt.version ]]; then
      continue
    elif [[ $name == device.crt ]]; then
      swapped+=(device.crt device.crt.version)
      install_certificate_pair "$stage/new/device.crt" "$certificate_version" || return 1
    elif is_system_file "$name"; then
      if [[ -f $path ]] && cmp -s -- "$stage/new/$name" "$path"; then
        continue
      fi
      swapped+=("$name")
      sudo_install "$stage/new/$name" "$path" || return 1
      changed_system+=("$name")
    else
      swapped+=("$name")
      put_in_place "$stage/new/$name" "$path" || return 1
    fi
  done
}

# restore_previous WHY puts back each file this run swapped in from its .prev copy, byte for byte,
# and, when restore_all is set, every other file of the set and .env that has a .prev copy. The
# certificate and its version file go back only when this run swapped them in: one that an earlier
# run installed passed pi-secrets' checks, and its .prev copy is an older certificate. The two go
# back as one change (install_certificate_pair), from device.crt.prev and the number in
# device.crt.version.prev; device.crt.prev with no device.crt.version.prev means the Pi had no
# version file (provision.sh --certificate writes none), and the version file this run swapped in
# goes. Then it reloads the user units, and reloads udev or restarts logind when their file
# changed. It sets restore_problems to what it could not do: a reload, or a file swapped in with no
# .prev copy, which stays as installed; and restore_note to what went back. When no file has a
# .prev copy it fails with WHY: no previous release to restore (R6). It writes no deploy.verified.
restore_previous() {
  local why=$1 name path prev version names=("${swapped[@]}") restored=() missing=() system=()
  restore_problems='' restore_note='restored the previous release'
  if $restore_all; then
    for name in "${SET_FILES[@]}" ghost-wispr .env; do
      [[ " ${names[*]} " == *" $name "* ]] || names+=("$name")
    done
  fi
  for name in "${names[@]}"; do
    path=$(installed_path "$name")
    prev=$DEPLOY_DIR/$name.prev
    if [[ $name == device.crt.version ]]; then
      continue
    fi
    if [[ ! -f $prev ]]; then
      if [[ " ${swapped[*]} " == *" $name "* ]]; then
        missing+=("$name")
        if [[ $name == device.crt ]]; then
          missing+=(device.crt.version)
        fi
      fi
      continue
    fi
    if [[ $name == device.crt ]]; then
      # A version file that does not hold one number goes back as none: pi-secrets passes no
      # version for either.
      version=none
      if [[ -f $DEPLOY_DIR/device.crt.version.prev ]]; then
        version=$(<"$DEPLOY_DIR/device.crt.version.prev") || version=none
        [[ $version =~ ^[0-9]+$ ]] || version=none
      fi
      cp --preserve=mode -- "$prev" "$DEPLOY_DIR/.device.crt.rollback" || fail "$why; could not restore $path from $prev"
      install_certificate_pair "$DEPLOY_DIR/.device.crt.rollback" "$version" \
        || fail "$why; could not restore $path and its version file from $prev and $DEPLOY_DIR/device.crt.version.prev"
      restored+=(device.crt device.crt.version)
      continue
    fi
    if is_system_file "$name"; then
      if ! { [[ -f $path ]] && cmp -s -- "$prev" "$path"; }; then
        sudo_install "$prev" "$path" || fail "$why; could not restore $path from $prev"
        system+=("$name")
      fi
    else
      cp --preserve=mode -- "$prev" "$DEPLOY_DIR/.$name.rollback" || fail "$why; could not restore $path from $prev"
      put_in_place "$DEPLOY_DIR/.$name.rollback" "$path" || fail "$why; could not restore $path from $prev"
    fi
    restored+=("$name")
  done
  if ((${#restored[@]} == 0)); then
    fail "$why; no previous release to restore, so the new files stay installed"
  fi
  systemctl --user daemon-reload || restore_problems+='; could not reload the user manager'
  reload_system "${system[@]}" || restore_problems+='; could not reload udev or restart logind'
  if ((${#missing[@]})); then
    restore_problems+="; no previous release to restore for ${missing[*]}, which stay as installed"
  fi
  if $restore_all; then
    if installed_set_is_verified; then
      restore_note='restored the last verified set, the one deploy.verified lists'
    else
      restore_note='restored each file that has a .prev copy, but they are not the set deploy.verified lists'
    fi
  fi
}

# rollback WHY restores the previous release's files (restore_previous), then restarts the recorder
# when restartable allows it, and fails with WHY.
rollback() {
  local why=$1
  restore_previous "$why"
  why+=$restore_problems
  if recorder_active; then
    restartable "$why"
  fi
  systemctl --user restart ghost-wispr.service || fail "$why; $restore_note but could not restart ghost-wispr.service"
  fail "$why; $restore_note"
}

# leave_if_busy_before_restart reads the recorder's status again just before a restart on files
# this run swapped in (R7): a session or a call can start during the swap and the reloads after it.
# While the recorder records a session or holds a call, it puts back the previous files
# (restore_previous) and leaves the recorder running, unrestarted, on the files it started on. So
# the Pi is as it was, and the run ends with exit 0, as leave_if_busy ends it, unless a file could
# not go back, or the run found the installed set not the verified one (restore_all), which it
# reports. While the recorder runs but gives no status, it does the same and fails.
leave_if_busy_before_restart() {
  local reason kept
  status_json=$(recorder_status)
  reason=$(busy_reason "$status_json")
  if [[ -z $reason ]]; then
    if [[ -n $status_json ]] || ! recorder_active; then
      return 0
    fi
    reason='ghost-wispr.service is active but /api/status gave no status'
  fi
  restore_previous "$reason"
  if $restore_all; then kept=$restore_note; else kept="restored the previous release's files"; fi
  kept+=', and left the recorder running on the files it started on'
  if [[ -z $status_json || -n $restore_problems ]] || $restore_all; then
    fail "$reason$restore_problems; $kept"
  fi
  printf 'ghost-wispr update: %s; leaving the current release in place\n' "$reason"
  exit 0
}

# timestamp_us TIMESTAMP prints a time that systemctl --timestamp=us+utc printed ("Wed 2026-11-25
# 16:00:16.493752 UTC") in microseconds since the epoch, and file_time_us FILE the time FILE was
# last written. Each reads the fraction itself, with date for the whole seconds alone: uutils date
# (Ubuntu 26.04's date) prints %6N wrong for a fraction under 0.1 s, without its leading zeros
# (0.050 s prints 500000).
timestamp_us() {
  local fraction seconds
  [[ $1 =~ ^[A-Za-z]{3}\ ([0-9]{4}-[0-9]{2}-[0-9]{2}\ [0-9]{2}:[0-9]{2}:[0-9]{2})\.([0-9]{6})\ UTC$ ]] || return 1
  fraction=${BASH_REMATCH[2]}
  seconds=$(date -u -d "${BASH_REMATCH[1]} UTC" +%s 2>/dev/null) || return 1
  [[ $seconds =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "$((seconds * 1000000 + 10#$fraction))"
}
file_time_us() {
  local stamp seconds fraction
  stamp=$(LC_ALL=C find "$1" -maxdepth 0 -printf '%T@' 2>/dev/null) || return 1
  [[ $stamp =~ ^([0-9]+)(\.([0-9]+))?$ ]] || return 1
  seconds=${BASH_REMATCH[1]} fraction=${BASH_REMATCH[3]}000000
  printf '%s\n' "$((seconds * 1000000 + 10#${fraction:0:6}))"
}

# restart_for_files_since_start restarts the recorder, once it is free, when .env or device.crt is
# newer than the start of its main process (systemd's ExecMainStartTimestamp), to the microsecond:
# a run cut short after a refresh's swap and before its restart left the recorder on the old ones,
# and the next runs find the files current. It checks the recorder's version as a refresh does, and
# rolls those files back from their .prev copies when the recorder does not answer.
restart_for_files_since_start() {
  local started start_us name path mtime newer=()
  recorder_active || return 0
  started=$(systemctl --user show -p ExecMainStartTimestamp --timestamp=us+utc --value ghost-wispr.service 2>/dev/null) \
    || started=''
  if ! start_us=$(timestamp_us "$started"); then
    printf 'ghost-wispr update: could not read when ghost-wispr.service started (%s), so it is not restarted for an .env or a certificate installed since\n' \
      "${started:-no ExecMainStartTimestamp}"
    return 0
  fi
  for name in .env device.crt; do
    path=$(installed_path "$name")
    if [[ -f $path ]] && mtime=$(file_time_us "$path") && ((mtime > start_us)); then
      newer+=("$name")
    fi
  done
  ((${#newer[@]})) || return 0
  leave_if_busy
  printf 'ghost-wispr update: the recorder started before the installed %s; restarting it\n' "${newer[*]}"
  swapped=("${newer[@]}")
  if [[ " ${newer[*]} " == *' device.crt '* ]]; then
    swapped+=(device.crt.version)
  fi
  systemctl --user restart ghost-wispr.service || rollback "the recorder did not start on the installed ${newer[*]}"
  wait_for_commit "$release_commit" \
    || rollback "the recorder did not answer /api/version with commit $release_commit and /api/status with a status it could read within 120 s of its restart on the installed ${newer[*]}"
}

# refuse_override WHERE NAMES fails while NAMES, the override variables WHERE sets, is not empty.
refuse_override() {
  [[ -z $2 ]] || fail "$1 sets $(paste -sd ' ' <<<"$2"), which would override the release's config; nothing was installed"
}

# require_no_override fails, before anything changes, while .env or the user manager's environment
# sets GHOST_WISPR_CONFIG or an Envoy override variable, or while ghost-wispr.service or
# ghost-wispr-update.service has a drop-in (R7).
require_no_override() {
  local names unit paths
  systemctl --user daemon-reload || fail "could not reload the user manager: nothing was installed"
  if [[ -e $DEPLOY_DIR/.env ]]; then
    names=$(config_override_names "$DEPLOY_DIR/.env") || fail "could not read $DEPLOY_DIR/.env: nothing was installed"
    refuse_override "$DEPLOY_DIR/.env" "$names"
  fi
  names=$(manager_override_names) \
    || fail "could not read the environment of the user manager (systemctl --user show-environment), or it was empty: nothing was installed"
  refuse_override "the user manager's environment" "$names"
  for unit in ghost-wispr.service ghost-wispr-update.service; do
    paths=$(unit_drop_ins "$unit") || fail "could not read the drop-ins of $unit: nothing was installed"
    [[ -z $paths ]] || fail "$unit has a drop-in ($paths); nothing was installed"
  done
}

# join_lines LINES prints LINES joined by "; ", or none when there is none.
join_lines() {
  if [[ -z $1 ]]; then
    printf 'none'
  else
    printf '%s' "${1//$'\n'/; }"
  fi
}

# report_drift prints a line for each bootstrap file that is not the release's, and for a Serve
# setting that is not the release's serve.conf for the device's tailnet name (D4, R16). It changes
# nothing, and no exit code.
report_drift() {
  local names name want have serve self device
  if names=$(bootstrap_drift "$manifest_json" "$DEPLOY_DIR" "$UNIT_DIR"); then
    while IFS= read -r name; do
      if [[ -n $name ]]; then
        printf "ghost-wispr update: provisioning file %s differs from the release; run the release's provision.sh\n" "$name"
      fi
    done <<<"$names"
  else
    printf 'ghost-wispr update: could not compare the provisioning files with the release\n'
  fi
  if ! want=$(serve_conf_lines "$set_dir/serve.conf"); then
    printf "ghost-wispr update: Serve: UNKNOWN (cannot read the release's serve.conf)\n"
    return 0
  fi
  if ! serve=$(timeout 10s sudo -n tailscale serve status --json 2>/dev/null); then
    printf 'ghost-wispr update: Serve: UNKNOWN (tailscale serve status --json failed or did not answer within 10 s)\n'
    return 0
  fi
  # The device's name tells its own Web entries from those of another name, such as the one a
  # rename leaves.
  if ! self=$(timeout 10s tailscale status --self --json 2>/dev/null) || ! device=$(device_tailnet_name "$self"); then
    printf "ghost-wispr update: Serve: UNKNOWN (tailscale status --self --json failed, did not answer within 10 s, or gave no DNSName, the device's name)\n"
    return 0
  fi
  if ! have=$(serve_lines_from_status "$serve" "$device"); then
    printf 'ghost-wispr update: Serve: UNKNOWN (tailscale serve status --json gave no Serve setting it could read)\n'
    return 0
  fi
  if [[ $have != "$want" ]]; then
    printf 'ghost-wispr update: Serve differs from the release: want %s, have %s\n' "$(join_lines "$want")" "$(join_lines "$have")"
  fi
}

print_installed_certificate() {
  if [[ -n $certificate_staged ]]; then
    printf 'ghost-wispr update: installed certificate version %s, not after %s\n' "$certificate_version" "$certificate_end"
  fi
}

# refresh_current: the release is current. A change of the secret lines, or a newer certificate that
# signs in, installs, with its .prev copy kept, and the recorder restarts on it while it is not busy.
# With neither, a recorder that started before the installed .env or certificate restarts on them
# (restart_for_files_since_start).
refresh_current() {
  local changes=()
  read_secrets "$BINARY"
  render_env "$stage/new/.env" || fail "could not write the new .env"
  if ! cmp -s -- "$stage/new/.env" "$DEPLOY_DIR/.env"; then
    changes+=(.env)
  fi
  if [[ -n $certificate_staged ]]; then
    stage_certificate
    changes+=(device.crt device.crt.version)
  fi
  if ((${#changes[@]} == 0)); then
    restart_for_files_since_start
    [[ -z $secrets_failure ]] || fail "$secrets_failure; .env keeps its secret lines"
    return 0
  fi
  leave_if_busy
  keep_previous "${changes[@]}"
  swap_in "${changes[@]}" || rollback "could not install ${changes[*]}"
  # A session or a call that started during the swap stops the run here, with the files back.
  leave_if_busy_before_restart
  systemctl --user restart ghost-wispr.service || rollback "the recorder did not start on the new ${changes[*]}"
  wait_for_commit "$release_commit" \
    || rollback "the recorder did not answer /api/version with commit $release_commit and /api/status with a status it could read within 120 s of its restart on the new ${changes[*]}"
  if [[ " ${changes[*]} " == *' .env '* ]]; then
    printf 'ghost-wispr update: installed secrets\n'
  fi
  print_installed_certificate
  [[ -z $secrets_failure ]] || fail "$secrets_failure; .env keeps its secret lines"
}

# install_release installs the release's whole set, with the staged binary, .env and certificate, and
# rolls it back as one unit when the new release does not answer its commit.
install_release() {
  local name keep=false env_changed=false install_names=()
  download "$binary_url" "$stage/ghost-wispr-linux-arm64" 300
  download "$binary_checksum_url" "$stage/ghost-wispr-linux-arm64.sha256" 60
  check_asset "$stage/ghost-wispr-linux-arm64" "$stage/ghost-wispr-linux-arm64.sha256" ghost-wispr-linux-arm64
  [[ $(sum "$stage/ghost-wispr-linux-arm64") == "$binary_sha256" ]] \
    || fail "the release's binary is not the one its manifest.json names; nothing was installed"
  chmod 0755 "$stage/ghost-wispr-linux-arm64" || fail "cannot make the staged binary executable"
  read_secrets "$stage/ghost-wispr-linux-arm64"

  for name in "${SET_FILES[@]}"; do
    [[ $name != ghost-wispr.yaml ]] || continue
    install -m "$(file_mode "$name")" -- "$set_dir/$name" "$stage/new/$name" || fail "could not stage $name"
  done
  render_config "$set_dir/ghost-wispr.yaml" "$DEPLOY_DIR/ghost-wispr.yaml" >"$stage/new/ghost-wispr.yaml" \
    || fail "could not render the release's config with the shared drive of the installed config (its gdrive_folder_id line); nothing was installed"
  chmod 0640 "$stage/new/ghost-wispr.yaml" || fail "could not stage ghost-wispr.yaml"
  mv -f -- "$stage/ghost-wispr-linux-arm64" "$stage/new/ghost-wispr" || fail "could not stage the binary"
  render_env "$stage/new/.env" || fail "could not write the new .env"
  if ! cmp -s -- "$stage/new/.env" "$DEPLOY_DIR/.env"; then
    env_changed=true
  fi
  install_names=("${SET_FILES[@]}" ghost-wispr .env)
  if [[ -n $certificate_staged ]]; then
    stage_certificate
    install_names+=(device.crt device.crt.version)
  fi

  # A session or a call that started during the download stops the run here, and so does a bridge
  # that stopped accepting the release while it downloaded.
  leave_if_busy
  hold_if_bridge_rejects
  if [[ -f $VERIFIED ]]; then
    if installed_set_is_verified; then
      keep=true
    else
      printf 'ghost-wispr update: the installed files are not the set %s lists, so the .prev copies stay as they are\n' "$VERIFIED"
    fi
  elif recorder_active; then
    if running_image_is_installed; then
      keep=true
    fi
  elif old_record_names_binary; then
    # The recorder is stopped, so no image shows which binary ran; the old update.sh's record does.
    printf 'ghost-wispr update: ghost-wispr.service is stopped, and %s names the installed binary, so the installed files are kept as the previous release\n' \
      "$OLD_VERIFIED"
    keep=true
  elif [[ -e $DEPLOY_DIR/ghost-wispr.prev ]]; then
    # Nothing shows that the old update.sh's ghost-wispr.prev is not older than the installed
    # binary, so no rollback may put it back.
    printf 'ghost-wispr update: ghost-wispr.service is stopped, and %s does not name the installed binary, so %s, which may be older than that binary, is removed, and the installed files are not kept as the previous release\n' \
      "$OLD_VERIFIED" "$DEPLOY_DIR/ghost-wispr.prev"
    rm -f -- "$DEPLOY_DIR/ghost-wispr.prev" || fail "could not remove $DEPLOY_DIR/ghost-wispr.prev; nothing was installed"
  elif [[ -f $BINARY ]]; then
    printf 'ghost-wispr update: ghost-wispr.service is stopped, and %s does not name the installed binary, so the installed files are not kept as the previous release\n' \
      "$OLD_VERIFIED"
  fi
  # With the set, the certificate and its version file are kept, staged or not. Without it, a staged
  # certificate keeps the installed one, the certificate before this run, for its rollback; a
  # rollback puts the certificate back only to undo this run's swap of it (restore_previous).
  if $keep; then
    keep_previous "${SET_FILES[@]}" ghost-wispr .env device.crt device.crt.version
  else
    restore_all=true
    if [[ -n $certificate_staged ]]; then
      keep_previous device.crt device.crt.version
    fi
  fi
  # The first install by this script keeps the config it replaces, where a person finds a setting
  # saved on the page before the local settings file (D13). A second try keeps the first copy.
  if [[ ! -f $VERIFIED && -f $DEPLOY_DIR/ghost-wispr.yaml && ! -e $DEPLOY_DIR/ghost-wispr.yaml.before-release-config ]]; then
    cp --preserve=mode -- "$DEPLOY_DIR/ghost-wispr.yaml" "$DEPLOY_DIR/.ghost-wispr.yaml.before-release-config.tmp" \
      || fail "could not keep $DEPLOY_DIR/ghost-wispr.yaml as ghost-wispr.yaml.before-release-config; nothing was installed"
    mv -f -- "$DEPLOY_DIR/.ghost-wispr.yaml.before-release-config.tmp" "$DEPLOY_DIR/ghost-wispr.yaml.before-release-config" \
      || fail "could not keep $DEPLOY_DIR/ghost-wispr.yaml as ghost-wispr.yaml.before-release-config; nothing was installed"
  fi

  swap_in "${install_names[@]}" || rollback "could not install the release's files"
  systemctl --user daemon-reload || rollback "could not reload the user manager"
  reload_system "${changed_system[@]}" || rollback "could not reload udev or restart logind"
  # A session or a call that started during the swap or the reloads stops the run here, with the
  # previous release's files back.
  leave_if_busy_before_restart
  systemctl --user restart ghost-wispr.service || rollback "the new release did not start"
  wait_for_commit "$release_commit" \
    || rollback "the new release did not answer /api/version with commit $release_commit and /api/status with a status it could read within 120 s"
  write_verified || fail "the new release runs, but $VERIFIED could not be written; the next run installs it again"
  rm -f -- "$OLD_VERIFIED"
  printf 'ghost-wispr update: installed commit %s (invocation %s)\n' "$release_commit" "${INVOCATION_ID:-none}"
  print_installed_certificate
  if $env_changed; then
    printf 'ghost-wispr update: installed secrets\n'
  fi
}

# hold_if_bridge_rejects compares the call bridge's link versions, in the status read last
# (status_json), with the release's (C6). A release that shares none with the bridge could not link
# to it, and waits: the run prints both lists, refreshes the secrets and a renewed certificate of
# the release the Pi runs (R7), as when it is current, when the installed set is the one
# deploy.verified lists, and ends with exit 0. It runs at the first status read, and again at the
# read just before the swap, since the bridge can change its list while the release downloads; the
# refresh drops what the run staged for the release before it reads the secrets again with the
# installed binary.
hold_if_bridge_rejects() {
  local held verified_line
  held=$(jq -nr --argjson status "${status_json:-null}" --slurpfile manifest "$manifest_json" '
    ($status.call.link_versions? // null) as $bridge | ($manifest[0].link_versions // null) as $release
    | if ($bridge | type) == "array" and ($bridge | length) > 0 and ($release | type) == "array"
        and ([$bridge[] as $b | $release[] | select(. == $b)] | length) == 0
      then "ghost-wispr update: held: the bridge accepts link versions \($bridge | tojson), this release \($release | tojson)"
      else empty end') || fail "could not compare the bridge's link versions with the release's"
  [[ -n $held ]] || return 0
  printf '%s\n' "$held"
  if installed_set_is_verified; then
    # The commit deploy.verified names is the one the running release answers.
    verified_line=$(head -n1 -- "$VERIFIED")
    release_commit=${verified_line#commit }
    if ! { rm -rf -- "$stage/secrets" "$stage/new" && mkdir -- "$stage/new"; }; then
      fail "could not empty the staging directory $stage for the refresh of the release the Pi runs"
    fi
    secrets_file='' secrets_failure='' certificate_staged='' certificate_version='' certificate_end=''
    require_no_override
    refresh_current
  fi
  exit 0
}

nightly() {
  reconcile_certificate_journal
  remove_stale_stages
  leave_if_busy

  release_json=$(curl --connect-timeout 5 --max-time 30 --fail --silent --show-error "$RELEASE_API") \
    || fail "could not read the latest release from $RELEASE_API"
  release_commit=$(jq -r '.target_commitish // empty' <<<"$release_json" 2>/dev/null) || release_commit=''
  [[ $release_commit =~ ^[0-9a-f]{40}$ ]] || fail "the latest release has no immutable target commit"
  asset_url tarball_url ghost-wispr-pi-deploy.tar.gz
  asset_url tarball_checksum_url ghost-wispr-pi-deploy.tar.gz.sha256
  asset_url binary_url ghost-wispr-linux-arm64
  asset_url binary_checksum_url ghost-wispr-linux-arm64.sha256

  make_stage
  download "$tarball_url" "$stage/ghost-wispr-pi-deploy.tar.gz" 60
  download "$tarball_checksum_url" "$stage/ghost-wispr-pi-deploy.tar.gz.sha256" 30
  check_asset "$stage/ghost-wispr-pi-deploy.tar.gz" "$stage/ghost-wispr-pi-deploy.tar.gz.sha256" ghost-wispr-pi-deploy.tar.gz
  extract_set
  check_manifest

  if release_is_current; then
    printf 'ghost-wispr update: already current at commit %s\n' "$release_commit"
    report_drift
    # A refresh installs files and restarts the recorder too: the refusal holds for it (R7).
    require_no_override
    refresh_current
    return 0
  fi

  # The call bridge's link versions and the release's, from the first status read.
  hold_if_bridge_rejects

  require_no_override
  install_release
  report_drift
  [[ -z $secrets_failure ]] || fail "$secrets_failure; .env kept its secret lines"
}

secrets_mode() {
  reconcile_certificate_journal
  remove_stale_stages
  [[ -x $BINARY ]] || fail "there is no installed binary at $BINARY to read the secrets with"
  make_stage
  read_secrets "$BINARY"
  [[ -z $secrets_failure ]] || fail "$secrets_failure; .env keeps its secret lines"
  render_env "$stage/new/.env" || fail "could not write the new .env"
  if cmp -s -- "$stage/new/.env" "$DEPLOY_DIR/.env"; then
    return 0
  fi
  mv -f -- "$stage/new/.env" "$DEPLOY_DIR/.env" || fail "could not install the new .env"
  printf 'ghost-wispr update: installed secrets\n'
}

main() {
  case "$#:${1:-}" in
    0:) nightly ;;
    1:secrets) secrets_mode ;;
    *)
      printf 'usage: update.sh [secrets]\n' >&2
      exit 2
      ;;
  esac
}

main "$@"
