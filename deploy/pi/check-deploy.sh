#!/usr/bin/env bash
# The acceptance driver of the office Pi's deployment: does the Pi run a release whole, as the
# nightly timer installed it? A person runs it from a tailnet member device. It reads the release
# from GitHub, the Pi's recorder over Serve (https://PI_HOST), and the Pi itself over SSH as
# ghost-wispr, and changes nothing on the Pi.
#
#   deploy/pi/check-deploy.sh [--release COMMIT]
#
# It checks the latest release, or the release of COMMIT, and prints one line for each check:
#
#   check N PASS: ...      it read its input, and the check holds
#   check N FAIL: ...      it read its input, and the check does not hold; the line names each
#                          difference
#   check N UNKNOWN: ...   it could not read its input; the line names what it could not read
#
#   1  GET /api/version over Serve answers the release's commit.
#   2  Each setting of MODEL_SETTINGS, and recording_rules, runs as the release's template gives it.
#      The template is the ghost-wispr.yaml of the release's ghost-wispr-pi-deploy.tar.gz, checked
#      against its .sha256 and its manifest. A setting the Pi's local settings file sets (the
#      settings saved on the page) is compared with that file's value instead, and the line names it.
#   3  GET /api/providers?refresh=1 over Serve lists the template's providers, and those the local
#      settings file adds, and each signed in.
#   4  GET /api/status over Serve has no warning.
#   5  In the update unit's journal, ghost-wispr-update.timer started the run that printed
#      "installed commit <commit> (invocation <id>)": the launcher line of the same invocation id
#      names it as its trigger. FAIL when that line names none (a run started by hand).
#   6  Each protected recording (PI_PROTECTED_RECORDINGS) has its sha256.
#   7  The three bootstrap files on the Pi have the sha256 the release's manifest gives them, and
#      Serve's setting matches the release's serve.conf.
#
# It exits 0 when all seven PASS, 1 when any does not, and 2 for a usage error.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
usage() { sed -n '7,31p' "$0" | sed 's/^# \{0,1\}//'; }
usage_error() {
  printf 'check-deploy: %s\n' "$*" >&2
  usage >&2
  exit 2
}

release_commit=
case $# in
  0) ;;
  1)
    [[ $1 == -h || $1 == --help ]] || usage_error "unknown argument: $1"
    usage
    exit 0
    ;;
  2)
    [[ $1 == --release ]] || usage_error "unknown argument: $1"
    [[ $2 =~ ^[0-9a-f]{40}$ ]] || usage_error "--release takes a commit, 40 hex digits, not $2"
    release_commit=$2
    ;;
  *) usage_error 'too many arguments' ;;
esac

for command in awk curl jq sha256sum ssh tar; do
  command -v "$command" >/dev/null || usage_error "missing required command: $command"
done

# The office Pi's deployment settings (PI_HOST, PI_PROTECTED_RECORDINGS); GHOST_WISPR_PI_ENV names
# another file, as the tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
readonly pi_env
# shellcheck source=files/pi.env
source "$pi_env" || usage_error "cannot read the Pi deployment settings in $pi_env"
readonly PI="ghost-wispr@$PI_HOST" SERVE="https://$PI_HOST"
# GHOST_WISPR_DEPLOY_DIR and GHOST_WISPR_RELEASES_API exist for check_deploy_test.sh.
readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly RELEASES_API="${GHOST_WISPR_RELEASES_API:-https://api.github.com/repos/sjawhar/ghost-wispr/releases}"
readonly LOCAL_SETTINGS="$DEPLOY_DIR/ghost-wispr.local.yaml"
# The bootstrap files: the three files that install every later release, which only provision.sh
# installs, so a release that changes one reaches the Pi only when a person runs its provision.sh.
readonly BOOTSTRAP_FILES=(ghost-wispr-update-launcher.sh ghost-wispr-update.service ghost-wispr-update.timer)

# pi runs a command on the Pi with no input; pi_in gives it this script's input. Each gives up on a
# Pi it cannot reach in 10 s, or that stops answering for a minute (ssh exits 255), and never waits
# on a password prompt.
readonly SSH_OPTIONS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
pi() { ssh -n "${SSH_OPTIONS[@]}" "$PI" "$@"; }
pi_in() { ssh "${SSH_OPTIONS[@]}" "$PI" "$@"; }

tmp=$(mktemp -d)
readonly tmp
trap 'rm -rf -- "$tmp"' EXIT
# error_of FILE: the first line of a command's error output, for a check line.
error_of() {
  local line
  line=$(awk 'NF { print; exit }' "$1" 2>/dev/null) || line=
  printf '%s' "${line:-no error output}"
}

# report N VERDICT TEXT prints check N's one line. Every verdict but PASS fails the run.
not_passed=0
report() {
  local text=${3//$'\n'/; }
  printf 'check %s %s: %s\n' "$1" "$2" "$text"
  [[ $2 == PASS ]] || not_passed=$((not_passed + 1))
}
pass() { report "$1" PASS "$2"; }
fails() { report "$1" FAIL "$2"; }
unknown() { report "$1" UNKNOWN "$2"; }
# joined ITEMS...: the items, separated by "; ".
joined() {
  local IFS=$'\x1f'
  local text="$*"
  printf '%s' "${text//$'\x1f'/; }"
}

# flatten_yaml prints each setting of a YAML file written one setting a line as its dotted path and
# value, as deploy/pi/accept-slice2.sh's does; a setting with settings under it prints as "path:".
flatten_yaml() {
  awk '
    /^[[:space:]]*(#|-|$)/ { next }
    {
      indent = match($0, /[^[:space:]]/) - 1
      line = substr($0, indent + 1)
      if (line !~ /^[^:]+:([[:space:]]|$)/) next
      while (depth > 0 && indents[depth] >= indent) depth--
      key = line; sub(/:.*/, "", key)
      value = line; sub(/^[^:]*:[[:space:]]*/, "", value); sub(/[[:space:]]+#.*$/, "", value)
      path = depth > 0 ? paths[depth] "." key : key
      print (value == "" ? path ":" : path ": " value)
      depth++; indents[depth] = indent; paths[depth] = path
    }
  '
}
# has_setting FLAT PATH: FLAT (flatten_yaml's output) sets PATH.
has_setting() { awk -v path="$2" 'index($0, path ": ") == 1 || $0 == path ":" { found = 1; exit } END { exit !found }' <<<"$1"; }
# setting_value FLAT PATH: the value FLAT gives PATH, as written.
setting_value() { awk -v prefix="$2: " 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1); exit }' <<<"$1"; }
# yaml_scalar_json VALUE: a YAML scalar as written, as a JSON string: a double-quoted one decoded
# (its escapes are JSON's for the text a setting holds), a single-quoted one with '' as ', a plain
# one as it is.
yaml_scalar_json() {
  local value=$1
  case $value in
    \"*\")
      jq -cn --arg value "$value" '$value | fromjson | if type == "string" then . else error("not a string") end' 2>/dev/null \
        && return 0
      ;;
    \'*\')
      value=${value:1:${#value}-2}
      value=${value//\'\'/\'}
      ;;
  esac
  jq -cn --arg value "$value" '$value'
}
# yaml_list KEY: the items of the top-level list KEY in the YAML on stdin, as a JSON list, less the
# blank ones, which the recorder drops (internal/config). It reads a block list ("- item" lines
# under KEY, at any indent, as the recorder's YAML writer indents them) or an empty flow list ([]).
# It exits 3 when there is no KEY, and 2 on anything under KEY it cannot read item by item: a flow
# list with items, or an item that runs onto a second line.
yaml_list() {
  local items status=0 item
  items=$(awk -v key="$1" '
    function plain(s) { if (s !~ /^["'\'']/) sub(/[[:space:]]+#.*$/, "", s); return s }
    !on {
      if (index($0, key ":") != 1) next
      rest = substr($0, length(key) + 2)
      sub(/^[[:space:]]+/, "", rest)
      rest = plain(rest)
      found = 1
      if (rest == "[]") exit
      if (rest != "") { bad = 1; exit }
      on = 1
      next
    }
    /^[[:space:]]*(#.*)?$/ { next }
    /^[^[:space:]#-]/ { exit }
    /^[[:space:]]*- / { item = $0; sub(/^[[:space:]]*- /, "", item); print plain(item); next }
    { bad = 1; exit }
    END { if (bad) exit 2; if (!found) exit 3 }
  ') || status=$?
  ((status == 0)) || return "$status"
  while IFS= read -r item; do
    yaml_scalar_json "$item"
  done <<<"$items" | jq -cs 'map(select(test("^\\s*$") | not))'
}

# rules_difference RUNNING WANT SOURCE: the first rule that differs between two JSON lists of
# rules, the running one and the one SOURCE gives, and how many rules each holds.
rules_difference() {
  jq -rn --argjson got "$1" --argjson want "$2" --arg source "$3" '
    ([range(0; [($got | length), ($want | length)] | max)] | map(select($got[.] != $want[.])) | .[0] // 0) as $i
    | "rule \($i + 1) runs \($got[$i] // "<none>" | tojson), the \($source) has \($want[$i] // "<none>" | tojson); "
      + "the Pi runs \($got | length) rules, the \($source) names \($want | length)"'
}

# Each role /api/config reports under "models", and the template setting that names it.
readonly MODEL_SETTINGS=(
  live_transcription=transcription.model batch_transcription=batch_transcription.model
  summarization=summarization.model embedding=embedding.model concierge=concierge.model
  tts=tts.model tts_voice=tts.voice gptlive=voice.gptlive.model dgagent_provider=voice.dgagent.provider
)

# --- The release: its commit, and its deploy set, checked against its .sha256 and its manifest.

commit=$release_commit release_json='' release_error='' tag=''
if [[ -n $release_commit ]]; then
  if list=$(curl -fsS --max-time 30 "$RELEASES_API?per_page=100" 2>"$tmp/release.err"); then
    release_json=$(jq -c --arg commit "$release_commit" 'if type == "array" then (map(select(.target_commitish == $commit)) | .[0] // empty) else error("not a list") end' \
      <<<"$list" 2>/dev/null) || release_json=
    [[ -n $release_json ]] || release_error="no release of commit $release_commit among the latest 100 at $RELEASES_API"
  else
    release_error="cannot read the releases at $RELEASES_API: $(error_of "$tmp/release.err")"
  fi
elif release_json=$(curl -fsS --max-time 30 "$RELEASES_API/latest" 2>"$tmp/release.err"); then
  commit=$(jq -r 'if type == "object" then (.target_commitish // "") else "" end' <<<"$release_json" 2>/dev/null) || commit=
  if [[ ! $commit =~ ^[0-9a-f]{40}$ ]]; then
    release_error="the latest release at $RELEASES_API/latest names no commit (target_commitish ${commit:-<none>})"
    commit=
  fi
else
  release_json=
  release_error="cannot read the latest release at $RELEASES_API/latest: $(error_of "$tmp/release.err")"
fi
if [[ -z $release_error ]]; then
  tag=$(jq -r '.tag_name // ""' <<<"$release_json" 2>/dev/null) || tag=
fi
readonly release_name="release ${tag:-of commit ${commit:-<unknown>}}"

# load_deploy_set sets set_dir to the release's deploy set, unpacked and checked, or set_error to
# why there is none.
set_dir='' set_error=''
asset_url() { jq -r --arg name "$1" 'first(.assets[]? | select(.name == $name) | .browser_download_url) // ""' <<<"$release_json" 2>/dev/null; }
load_deploy_set() {
  local tarball_url sum_url want got dir sum name bad=()
  tarball_url=$(asset_url ghost-wispr-pi-deploy.tar.gz) || tarball_url=
  sum_url=$(asset_url ghost-wispr-pi-deploy.tar.gz.sha256) || sum_url=
  if [[ -z $tarball_url || -z $sum_url ]]; then
    set_error="$release_name has no ghost-wispr-pi-deploy.tar.gz and .sha256 among its assets"
    return 0
  fi
  if ! curl -fsSL --max-time 60 --output "$tmp/deploy.tar.gz" "$tarball_url" 2>"$tmp/deploy.err"; then
    set_error="cannot download $release_name's ghost-wispr-pi-deploy.tar.gz from $tarball_url: $(error_of "$tmp/deploy.err")"
    return 0
  fi
  if ! curl -fsSL --max-time 30 --output "$tmp/deploy.tar.gz.sha256" "$sum_url" 2>"$tmp/deploy.err"; then
    set_error="cannot download $release_name's ghost-wispr-pi-deploy.tar.gz.sha256 from $sum_url: $(error_of "$tmp/deploy.err")"
    return 0
  fi
  want=$(awk 'NR == 1 { print $1 }' "$tmp/deploy.tar.gz.sha256")
  got=$(sha256sum <"$tmp/deploy.tar.gz")
  got=${got%% *}
  if [[ ! $want =~ ^[0-9a-f]{64}$ || $got != "$want" ]]; then
    set_error="$release_name's ghost-wispr-pi-deploy.tar.gz has sha256 $got, not the ${want:-<none>} its .sha256 gives"
    return 0
  fi
  mkdir -- "$tmp/set"
  if ! tar -xzf "$tmp/deploy.tar.gz" -C "$tmp/set" 2>"$tmp/deploy.err"; then
    set_error="cannot unpack $release_name's ghost-wispr-pi-deploy.tar.gz: $(error_of "$tmp/deploy.err")"
    return 0
  fi
  dir=$tmp/set/ghost-wispr-pi-deploy
  if ! jq -e --arg commit "$commit" '.format == 1 and .commit == $commit and (.files | type == "object" and length > 0)' \
    "$dir/manifest.json" >/dev/null 2>&1; then
    set_error="the manifest of $release_name's deploy tarball (ghost-wispr-pi-deploy/manifest.json) is not one of format 1 for commit $commit: $(jq -c '{format, commit}' "$dir/manifest.json" 2>/dev/null || echo 'no manifest')"
    return 0
  fi
  while read -r sum name; do
    if [[ ! $name =~ ^[A-Za-z0-9._-]+$ || $name == . || $name == .. || $name == manifest.json ]]; then
      bad+=("$name, which is no file of the deploy set")
    elif [[ ! -f $dir/$name ]]; then
      bad+=("$name, which the tarball does not hold")
    else
      got=$(sha256sum <"$dir/$name")
      [[ ${got%% *} == "$sum" ]] || bad+=("$name, whose sha256 is ${got%% *}, not $sum")
    fi
  done < <(jq -r '.files | to_entries[] | "\(.value) \(.key)"' "$dir/manifest.json")
  if ((${#bad[@]} > 0)); then
    set_error="$release_name's deploy tarball does not match its manifest: $(joined "${bad[@]}")"
    return 0
  fi
  set_dir=$dir
}
if [[ -n $release_error ]]; then
  set_error=$release_error
else
  load_deploy_set
fi
# manifest_sum NAME: the sha256 the release's manifest gives NAME, or nothing.
manifest_sum() { jq -r --arg name "$1" '.files[$name] // ""' "$set_dir/manifest.json"; }
# template prints the release's template, or fails when the manifest does not name one.
template() {
  [[ -n $(manifest_sum ghost-wispr.yaml) ]] || return 1
  cat -- "$set_dir/ghost-wispr.yaml"
}

# --- The Pi's local settings file, which checks 2 and 3 read: local_state is read, absent or error.

local_settings='' local_state='' local_error=''
read_local_settings() {
  local status=0
  local_settings=$(pi "f=$(printf %q "$LOCAL_SETTINGS"); [ -e \"\$f\" ] || exit 3; cat -- \"\$f\"" 2>"$tmp/local.err") || status=$?
  case $status in
    0) local_state=read ;;
    3)
      local_state=absent
      local_settings=
      ;;
    255)
      local_state=error
      local_error="cannot reach $PI over SSH to read the local settings file $LOCAL_SETTINGS: $(error_of "$tmp/local.err")"
      ;;
    *)
      local_state=error
      local_error="cannot read the local settings file $LOCAL_SETTINGS on the Pi over SSH (exit $status): $(error_of "$tmp/local.err")"
      ;;
  esac
}

# --- The checks.

check_commit() {
  local body got
  if [[ -z $commit ]]; then
    unknown 1 "no release commit to compare the Pi's with: $release_error"
    return 0
  fi
  if ! body=$(curl -fsS --max-time 10 "$SERVE/api/version" 2>"$tmp/version.err"); then
    unknown 1 "cannot read GET $SERVE/api/version over Serve: $(error_of "$tmp/version.err")"
    return 0
  fi
  got=$(jq -r 'if type == "object" then (.commit // "") else "" end' <<<"$body" 2>/dev/null) || got=
  if [[ ! $got =~ ^[0-9a-f]{40}$ ]]; then
    unknown 1 "GET $SERVE/api/version over Serve answered no commit (${got:-none})"
  elif [[ $got == "$commit" ]]; then
    pass 1 "the Pi answers commit $got over Serve, the commit of $release_name"
  else
    fails 1 "the Pi answers commit $got over Serve, not $commit, the commit of $release_name"
  fi
}

check_settings() {
  local config template_text template_flat local_flat entry role setting raw source want got status=0
  local differences=() unread=() from_local=()
  if [[ -z $set_dir ]]; then
    unknown 2 "no release template to compare the running settings with: $set_error"
    return 0
  fi
  if ! template_text=$(template); then
    unknown 2 "the manifest of $release_name's deploy tarball names no ghost-wispr.yaml"
    return 0
  fi
  if ! config=$(curl -fsS --max-time 10 "$SERVE/api/config" 2>"$tmp/config.err"); then
    unknown 2 "cannot read GET $SERVE/api/config over Serve: $(error_of "$tmp/config.err")"
    return 0
  fi
  if ! jq -e 'type == "object" and (.models | type == "object") and (.recording_rules | type == "array")' >/dev/null 2>&1 <<<"$config"; then
    unknown 2 "GET $SERVE/api/config over Serve answered no models and recording_rules"
    return 0
  fi
  if [[ $local_state == error ]]; then
    unknown 2 "$local_error"
    return 0
  fi
  template_flat=$(flatten_yaml <<<"$template_text")
  local_flat=$(flatten_yaml <<<"$local_settings")

  # Each model setting, from the local settings file when it sets it, else from the template.
  for entry in "${MODEL_SETTINGS[@]}"; do
    role=${entry%%=*} setting=${entry#*=}
    if [[ -n $local_flat ]] && has_setting "$local_flat" "$setting"; then
      raw=$(setting_value "$local_flat" "$setting")
      source='local settings file'
      from_local+=("$setting")
    else
      raw=$(setting_value "$template_flat" "$setting")
      source=template
      if [[ -z $raw ]]; then
        unread+=("$release_name's template sets no $setting to compare the running $role with")
        continue
      fi
    fi
    want=$(yaml_scalar_json "$raw")
    got=$(jq -c --arg role "$role" '.models[$role] // null' <<<"$config")
    [[ $got == "$want" ]] || differences+=("$role runs $got, but the $source names $want ($setting)")
  done

  # The recording rules, from the local settings file when it sets them, else from the template.
  source='local settings file'
  want=$(yaml_list recording_rules <<<"$local_settings") || status=$?
  if ((status == 3)); then
    source=template status=0
    want=$(yaml_list recording_rules <<<"$template_text") || status=$?
  fi
  if ((status != 0)); then
    unread+=("cannot read recording_rules in the $source item by item, one plain or quoted rule a line")
  elif [[ $source == template && $want == '[]' ]]; then
    unread+=("$release_name's template has no recording rule to compare the running ones with")
  else
    [[ $source == template ]] || from_local+=(recording_rules)
    got=$(jq -c '.recording_rules' <<<"$config")
    [[ $got == "$want" ]] \
      || differences+=("recording_rules differ from the $source's: $(rules_difference "$got" "$want" "$source")")
  fi

  # A difference is a FAIL even when another setting could not be read: the line names both, as
  # check 7's does. UNKNOWN only when nothing it read differs.
  local named='' also=''
  ((${#from_local[@]} == 0)) || named=", with $(joined "${from_local[@]}") from the local settings file"
  ((${#unread[@]} == 0)) || also="; and $(joined "${unread[@]}")"
  if ((${#differences[@]} > 0)); then
    fails 2 "$(joined "${differences[@]}")$named$also"
  elif ((${#unread[@]} > 0)); then
    unknown 2 "$(joined "${unread[@]}")$named"
  else
    pass 2 "each model setting and the recording rules run as $release_name's template gives them$named"
  fi
}

check_providers() {
  local template_text providers want names not_ok missing extra local_added local_removed unexplained
  if [[ -z $set_dir ]]; then
    unknown 3 "no release template to compare the providers with: $set_error"
    return 0
  fi
  if ! template_text=$(template); then
    unknown 3 "the manifest of $release_name's deploy tarball names no ghost-wispr.yaml"
    return 0
  fi
  if ! providers=$(curl -fsS --max-time 180 "$SERVE/api/providers?refresh=1" 2>"$tmp/providers.err"); then
    unknown 3 "cannot read GET $SERVE/api/providers?refresh=1 over Serve: $(error_of "$tmp/providers.err")"
    return 0
  fi
  if ! jq -e 'type == "object" and (.providers | type == "array") and all(.providers[]; (.name | type == "string"))' >/dev/null 2>&1 <<<"$providers"; then
    unknown 3 "GET $SERVE/api/providers?refresh=1 over Serve answered no list of named providers"
    return 0
  fi
  want=$(flatten_yaml <<<"$template_text" | sed -nE 's/^providers\.([^.:]+):$/\1/p' | LC_ALL=C sort -u)
  if [[ -z $want ]]; then
    unknown 3 "$release_name's template names no provider"
    return 0
  fi
  # The recorder reads the local settings file over the template (D13), so a provider it adds runs,
  # and a null takes one away.
  if [[ $local_state == read ]]; then
    local_added=$(flatten_yaml <<<"$local_settings" | sed -nE 's/^providers\.([^.:]+):$/\1/p' | LC_ALL=C sort -u)
    local_removed=$(flatten_yaml <<<"$local_settings" | sed -nE 's/^providers\.([^.:]+): (null|~)$/\1/p' | LC_ALL=C sort -u)
    want=$(printf '%s\n' "$want" "$local_added" | grep . | LC_ALL=C sort -u | LC_ALL=C comm -23 - <(printf '%s\n' "$local_removed" | grep . || true))
  fi
  # An empty set to compare with is no pass, even when the Pi lists no provider either.
  if [[ -z $want ]]; then
    unknown 3 "no provider is left to check: the local settings file $LOCAL_SETTINGS sets to null each provider $release_name's template names"
    return 0
  fi
  names=$(jq -r '.providers[].name' <<<"$providers" | LC_ALL=C sort -u)
  not_ok=$(jq -r '.providers[] | select(.ok != true) | "\(.name) did not sign in (\(.message // "no message"))"' <<<"$providers")
  missing=$(LC_ALL=C comm -23 <(printf '%s\n' "$want") <(printf '%s\n' "$names") | paste -sd ' ' -)
  extra=$(LC_ALL=C comm -13 <(printf '%s\n' "$want") <(printf '%s\n' "$names") | grep . | paste -sd ' ' - || true)
  if [[ -n $not_ok ]]; then
    fails 3 "$not_ok"
  elif [[ $local_state == error && ( -n $missing || -n $extra ) ]]; then
    unknown 3 "the Pi's providers ($(paste -sd ' ' - <<<"$names")) are not the template's ($(paste -sd ' ' - <<<"$want")), and the local settings file that could add or remove one cannot be read: $local_error"
  elif [[ -n $missing || -n $extra ]]; then
    unexplained=()
    [[ -z $missing ]] || unexplained+=("the Pi lists no $missing, which the template names")
    [[ -z $extra ]] || unexplained+=("the Pi lists $extra, which neither the template nor the local settings file names")
    fails 3 "$(joined "${unexplained[@]}")"
  else
    pass 3 "the Pi lists the $(grep -c . <<<"$names") providers of $release_name's template and its local settings file ($(paste -sd ' ' - <<<"$names")), each signed in"
  fi
}

check_status() {
  local status
  if ! status=$(curl -fsS --max-time 10 "$SERVE/api/status" 2>"$tmp/status.err"); then
    unknown 4 "cannot read GET $SERVE/api/status over Serve: $(error_of "$tmp/status.err")"
  elif ! jq -e 'type == "object" and (.warnings | type == "array")' >/dev/null 2>&1 <<<"$status"; then
    unknown 4 "GET $SERVE/api/status over Serve answered no warnings list"
  elif [[ $(jq '.warnings | length' <<<"$status") == 0 ]]; then
    pass 4 "GET $SERVE/api/status over Serve has no warning"
  else
    fails 4 "GET $SERVE/api/status over Serve warns: $(jq -c '.warnings' <<<"$status")"
  fi
}

# How check 5 reads the update unit's lines: the Pi keeps no user journal, so they are in the
# system journal, which ghost-wispr reads with its passwordless sudo.
readonly JOURNAL_READ='sudo -n journalctl _SYSTEMD_USER_UNIT=ghost-wispr-update.service --no-pager --quiet --output=cat'
check_timer_install() {
  local journal status=0 lines install id launch rest trigger at
  if [[ -z $commit ]]; then
    unknown 5 "no release commit to look for in the update unit's journal: $release_error"
    return 0
  fi
  journal=$(pi "$JOURNAL_READ" 2>"$tmp/journal.err") || status=$?
  if ((status == 255)); then
    unknown 5 "cannot reach $PI over SSH to read the journal of ghost-wispr-update.service: $(error_of "$tmp/journal.err")"
    return 0
  elif ((status != 0)); then
    unknown 5 "cannot read the journal of ghost-wispr-update.service on the Pi ($JOURNAL_READ, exit $status): $(error_of "$tmp/journal.err")"
    return 0
  fi
  journal=$(grep -vx -- '-- No entries --' <<<"$journal" | grep . || true)
  if [[ -z $journal ]]; then
    unknown 5 "the journal of ghost-wispr-update.service on the Pi is empty ($JOURNAL_READ)"
    return 0
  fi
  lines=$(grep -c . <<<"$journal")
  install=$(awk -v prefix="ghost-wispr update: installed commit $commit (invocation " \
    'index($0, prefix) == 1 && substr($0, length($0)) == ")" { last = $0 } END { if (last != "") print last }' <<<"$journal")
  if [[ -z $install ]]; then
    unknown 5 "the journal of ghost-wispr-update.service ($lines lines) has no line \"ghost-wispr update: installed commit $commit (invocation <id>)\""
    return 0
  fi
  id=${install#"ghost-wispr update: installed commit $commit (invocation "}
  id=${id%")"}
  # systemd's invocation id: 32 hex digits.
  if [[ ! $id =~ ^[0-9a-f]{32}$ ]]; then
    unknown 5 "the line \"$install\" names no invocation id, so the run that printed it cannot be found"
    return 0
  fi
  launch=$(awk -v prefix="ghost-wispr launcher: invocation $id trigger " 'index($0, prefix) == 1 { last = $0 } END { if (last != "") print last }' <<<"$journal")
  if [[ -z $launch ]]; then
    unknown 5 "the run $id that installed commit $commit has no launcher line (\"ghost-wispr launcher: invocation $id trigger ...\") in the journal of ghost-wispr-update.service"
    return 0
  fi
  rest=${launch#"ghost-wispr launcher: invocation $id trigger "}
  trigger=${rest%% at *}
  at=${rest##* at }
  at=$(jq -rn --arg at "$at" '$at | tonumber / 1000000 | floor | todate' 2>/dev/null) || at=${rest##* at }
  case $trigger in
    ghost-wispr-update.timer) pass 5 "ghost-wispr-update.timer started the run $id (at $at) that installed commit $commit" ;;
    none) fails 5 "the run $id that installed commit $commit was started by hand: its launcher line names trigger none, not ghost-wispr-update.timer" ;;
    *) fails 5 "the run $id that installed commit $commit was started by $trigger, not ghost-wispr-update.timer" ;;
  esac
}

check_protected() {
  local named out status=0 confirmed
  named=$(grep -c . <<<"${PI_PROTECTED_RECORDINGS:-}" || true)
  if ((named == 0)); then
    unknown 6 "PI_PROTECTED_RECORDINGS in $pi_env names no recording, so there is nothing to compare"
    return 0
  fi
  out=$(pi_in 'sha256sum --check --strict' <<<"$PI_PROTECTED_RECORDINGS" 2>&1) || status=$?
  confirmed=$(grep -c ': OK$' <<<"$out" || true)
  if ((status == 255)); then
    unknown 6 "cannot reach $PI over SSH to hash the protected recordings: $(awk 'NF { print; exit }' <<<"$out")"
  elif ((status == 1)); then
    fails 6 "a protected recording is missing or changed: $(grep -v ': OK$' <<<"$out" || true)"
  elif ((status == 0 && confirmed == named)); then
    pass 6 "the $named protected recordings (PI_PROTECTED_RECORDINGS) have their sha256"
  else
    unknown 6 "sha256sum --check on the Pi exited $status and confirmed $confirmed of the $named protected recordings: $out"
  fi
}

# serve_lines prints Serve's setting from `tailscale serve status --json` on stdin (tailscale's
# ipn.ServeConfig) as serve.conf writes it: "<port> <proxy target>", one line for each Web entry
# whose handler "/" names a Proxy, sorted. It fails on an answer it cannot read that way.
serve_lines() {
  jq -r 'if type == "object" and ((.Web // {}) | type == "object") then
      (.Web // {}) | to_entries[] | (.key | capture(":(?<port>[0-9]+)$").port) as $port
      | (.value.Handlers["/"].Proxy // empty) | "\($port) \(.)"
    else error("not a Serve config") end' | LC_ALL=C sort
}
readonly SERVE_READ='timeout 10s sudo -n tailscale serve status --json'
check_bootstrap() {
  local differences=() unread=() out status=0 sum path name want serve have
  if [[ -z $set_dir ]]; then
    unknown 7 "no release manifest and serve.conf to compare the Pi's bootstrap files and Serve with: $set_error"
    return 0
  fi
  for name in "${BOOTSTRAP_FILES[@]}" serve.conf; do
    if [[ -z $(manifest_sum "$name") ]]; then
      unknown 7 "the manifest of $release_name's deploy tarball names no $name"
      return 0
    fi
  done

  # The installed bootstrap files, by sha256; a file the Pi does not have is a difference, one it
  # cannot read is unknown.
  out=$(pi "for f in $(printf %q "$DEPLOY_DIR/ghost-wispr-update-launcher.sh") \"\$HOME/.config/systemd/user/ghost-wispr-update.service\" \"\$HOME/.config/systemd/user/ghost-wispr-update.timer\"; do
  if [ ! -e \"\$f\" ]; then echo \"missing \$f\"
  elif s=\$(sha256sum 2>/dev/null <\"\$f\"); then echo \"\${s%% *} \$f\"
  else echo \"unreadable \$f\"; fi
done" 2>"$tmp/bootstrap.err") || status=$?
  if ((status == 255)); then
    unread+=("cannot reach $PI over SSH to hash the bootstrap files: $(error_of "$tmp/bootstrap.err")")
  elif ((status != 0)) || [[ $(grep -c . <<<"$out") != "${#BOOTSTRAP_FILES[@]}" ]]; then
    unread+=("cannot hash the bootstrap files on the Pi over SSH (exit $status): $(error_of "$tmp/bootstrap.err")")
  else
    while read -r sum path; do
      name=${path##*/}
      want=$(manifest_sum "$name")
      case $sum in
        missing) differences+=("provisioning file $name is missing on the Pi ($path); run the release's provision.sh") ;;
        unreadable) unread+=("cannot read $name on the Pi ($path)") ;;
        "$want") ;;
        *) differences+=("provisioning file $name ($path) differs from the release: sha256 $sum, the manifest's $want; run the release's provision.sh") ;;
      esac
    done <<<"$out"
  fi

  # Serve's setting against the release's serve.conf, read as the nightly drift rule reads it.
  want=$(awk 'NF && $1 !~ /^#/ { print $1, $2 }' "$set_dir/serve.conf" | LC_ALL=C sort)
  status=0
  serve=$(pi "$SERVE_READ" 2>"$tmp/serve.err") || status=$?
  if [[ -z $want ]]; then
    unread+=("$release_name's serve.conf names no port")
  elif ((status == 255)); then
    unread+=("cannot reach $PI over SSH to read Serve's setting: $(error_of "$tmp/serve.err")")
  elif ((status != 0)); then
    unread+=("cannot read Serve's setting on the Pi ($SERVE_READ, exit $status): $(error_of "$tmp/serve.err")")
  elif [[ -z $serve ]] || ! have=$(serve_lines <<<"$serve" 2>/dev/null); then
    unread+=("cannot read Serve's setting from $SERVE_READ on the Pi: it answered no Serve config")
  elif [[ $have != "$want" ]]; then
    differences+=("Serve differs from the release's serve.conf: want $(paste -sd ',' - <<<"$want"), have $(paste -sd ',' - <<<"${have:-none}")")
  fi

  if ((${#differences[@]} > 0)); then
    local also=''
    ((${#unread[@]} == 0)) || also="; and $(joined "${unread[@]}")"
    fails 7 "$(joined "${differences[@]}")$also"
  elif ((${#unread[@]} > 0)); then
    unknown 7 "$(joined "${unread[@]}")"
  else
    pass 7 "the bootstrap files ($(joined "${BOOTSTRAP_FILES[@]}")) have the sha256 of $release_name's manifest, and Serve matches its serve.conf ($(paste -sd ',' - <<<"$want"))"
  fi
}

check_commit
read_local_settings
check_settings
check_providers
check_status
check_timer_install
check_protected
check_bootstrap

printf 'check-deploy: %s of 7 checks PASS against %s\n' "$((7 - not_passed))" "$release_name" >&2
((not_passed == 0)) || exit 1
