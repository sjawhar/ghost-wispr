#!/usr/bin/env bash
# Runs deploy/pi/check-deploy.sh, the acceptance driver a person runs from a tailnet member device,
# against a fake GitHub release and a fake office Pi.
#
# Each release is a releases API answer whose assets are file:// URLs of a deploy tarball built here
# in the layout every release carries: one top directory, ghost-wispr-pi-deploy/, with manifest.json,
# the config template, serve.conf and the three bootstrap files. The Pi answers Serve
# (https://PI_HOST/api/...) from fixture JSON, and SSH (ghost-wispr@PI_HOST) by running each remote
# command here, under sh, in a sandbox that stands for its deploy directory and home, with fakes of
# sudo, journalctl, tailscale and timeout. Each case says which break of the driver it catches.
#
# The driver prints one line for each of its seven checks, "check N PASS: ...", "check N FAIL: ..."
# or "check N UNKNOWN: ...", and exits 0 when all seven PASS, else 1.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
readonly driver="$script_dir/check-deploy.sh"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

name=setup
fail() {
  printf 'check-deploy test (%s): %s\n' "$name" "$*" >&2
  exit 1
}

# shellcheck source=test-pi.env
source "$script_dir/test-pi.env"
readonly bin=$work/bin releases=$work/releases
readonly releases_api=https://api.test.invalid/repos/example/ghost-wispr/releases
readonly old=1111111111111111111111111111111111111111 new=2222222222222222222222222222222222222222
# Invocation ids of the update unit's runs, as systemd gives them: the night before, tonight's
# install at 03:30, a later run of the timer, and a run started by hand.
readonly run_old=0000000000000000000000000000000a run_new=0000000000000000000000000000000b
readonly run_current=0000000000000000000000000000000c run_by_hand=0000000000000000000000000000000d
mkdir -p "$bin" "$releases"
real_curl=$(command -v curl) real_cat=$(command -v cat) real_timeout=$(command -v timeout)
readonly real_curl real_cat real_timeout

# --- The fakes.

# curl on the operator's device. The Pi's Serve answers come from the case's state, the releases
# API's from its github directory; a missing answer is curl's connection failure (exit 7). The list
# of releases is paged as GitHub pages it: per_page releases (30 unless the URL says, at most 100)
# of github/releases.json, newest first, from page (1 unless the URL says), each page it is asked
# for noted in log/release-pages; github/pages-ignored stands for an API that answers every page
# with the first. A file:// asset goes to the real curl. Any other URL is refused, the recorder's
# own 127.0.0.1:8080 among them: the driver reads the Pi over Serve.
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
args=("$@")
output=
url=
while [[ $# -gt 0 ]]; do
  case $1 in
    -o | --output) output=$2; shift 2 ;;
    -m | --max-time | --connect-timeout | -H | --header | -w | --write-out | --retry | --retry-max-time) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >>"$FAKE_LOG/curl"
answer() {
  [[ -f $1 ]] || { printf 'curl: (7) Failed to connect to %s\n' "${url#*://}" >&2; exit 7; }
  if [[ -n $output ]]; then cat -- "$1" >"$output"; else cat -- "$1"; fi
}
case $url in
  file://*) exec "$FAKE_REAL_CURL" "${args[@]}" ;;
  "https://$FAKE_PI_HOST/api/version") answer "$FAKE_STATE/version.json" ;;
  "https://$FAKE_PI_HOST/api/status") answer "$FAKE_STATE/status.json" ;;
  "https://$FAKE_PI_HOST/api/config") answer "$FAKE_STATE/config.json" ;;
  "https://$FAKE_PI_HOST/api/providers?refresh=1") answer "$FAKE_STATE/providers.json" ;;
  "$FAKE_RELEASES_API/latest") answer "$FAKE_GITHUB/latest.json" ;;
  "$FAKE_RELEASES_API" | "$FAKE_RELEASES_API?"*)
    per_page=30 page=1
    IFS='&' read -ra params <<<"${url#"$FAKE_RELEASES_API"}"
    for param in "${params[@]}"; do
      case ${param#\?} in
        per_page=*) per_page=${param#*=} ;;
        page=*) page=${param#*=} ;;
      esac
    done
    printf '%s\n' "$page" >>"$FAKE_LOG/release-pages"
    ((per_page <= 100)) || per_page=100
    [[ ! -e $FAKE_GITHUB/pages-ignored ]] || page=1
    [[ -f $FAKE_GITHUB/releases.json ]] || answer "$FAKE_GITHUB/releases.json"
    jq -c --argjson per "$per_page" --argjson page "$page" '.[($page - 1) * $per:$page * $per]' "$FAKE_GITHUB/releases.json"
    ;;
  *) printf 'fake curl: unexpected URL %s\n' "$url" >&2; exit 1 ;;
esac
EOF

# ssh to the fake Pi: runs the remote command under sh in ghost-wispr's home, with the fakes on
# PATH. state/ssh-down stands for a Pi that does not answer on port 22 (ssh exits 255), and
# state/ssh-lost-on for a connection lost during the one remote command that holds its text (ssh
# exits 255, and notes it in state/ssh-lost-fired).
cat >"$bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
no_input=false
while [[ $# -gt 0 ]]; do
  case $1 in
    -n) no_input=true; shift ;;
    -T | -q) shift ;;
    -o) printf '%s\n' "$2" >>"$FAKE_LOG/ssh-options"; shift 2 ;;
    -*) echo "fake ssh: unknown option $1" >&2; exit 255 ;;
    *) break ;;
  esac
done
[[ $1 == "ghost-wispr@$FAKE_PI_HOST" ]] || { echo "fake ssh: unexpected host $1" >&2; exit 255; }
shift
if [[ -e $FAKE_STATE/ssh-down ]]; then
  echo "ssh: connect to host $FAKE_PI_HOST port 22: Connection timed out" >&2
  exit 255
fi
if [[ -s $FAKE_STATE/ssh-lost-on && $* == *"$(<"$FAKE_STATE/ssh-lost-on")"* ]]; then
  : >"$FAKE_STATE/ssh-lost-fired"
  echo "ssh: connection to $FAKE_PI_HOST lost: Broken pipe" >&2
  exit 255
fi
printf '%s\n' "$*" >>"$FAKE_LOG/ssh"
cd "$FAKE_HOME"
if $no_input; then
  exec env HOME="$FAKE_HOME" FAKE_ON_PI=1 sh -c "$*" </dev/null
fi
exec env HOME="$FAKE_HOME" FAKE_ON_PI=1 sh -c "$*"
EOF

# cat. On the fake Pi (under the fake ssh) it refuses each file that state/unreadable names, one
# absolute path a line, as the Pi refuses ghost-wispr a file it may not read, whatever user runs
# this test; every other call is the real cat.
cat >"$bin/cat" <<'EOF'
#!/usr/bin/env bash
if [[ ${FAKE_ON_PI:-} == 1 && -f ${FAKE_STATE:-}/unreadable ]]; then
  for arg in "$@"; do
    if grep -qxF -- "$arg" "$FAKE_STATE/unreadable"; then
      printf 'cat: %s: Permission denied\n' "$arg" >&2
      exit 1
    fi
  done
fi
exec "${FAKE_REAL_CAT:?}" "$@"
EOF

# sudo on the fake Pi: ghost-wispr's passwordless sudo, which the driver must call with -n, as no
# terminal can answer a password prompt. state/sudo-refused stands for a Pi where sudo asks for a
# password, which -n refuses.
cat >"$bin/sudo" <<'EOF'
#!/usr/bin/env bash
[[ ${1:-} == -n ]] || { echo 'sudo: a terminal is required to read the password' >&2; exit 1; }
shift
if [[ -e $FAKE_STATE/sudo-refused ]]; then
  echo 'sudo: a password is required' >&2
  exit 1
fi
printf '%s\n' "$*" >>"$FAKE_LOG/sudo"
exec env FAKE_SUDO=1 "$@"
EOF

# journalctl on the fake Pi. The system journal holds state/journal-<unit>, the messages of
# ghost-wispr's user unit <unit>, one a line; only root reads it (sudo), and the Pi keeps no user
# journal (--user finds nothing). It answers the match _SYSTEMD_USER_UNIT=<unit>. -o cat prints each
# message alone; any other output form puts a time, the host and an identifier before it, as the
# short form does. With no entry it prints "-- No entries --", unless -q.
cat >"$bin/journalctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
unit=
form=short
quiet=false
user=false
while [[ $# -gt 0 ]]; do
  case $1 in
    _SYSTEMD_USER_UNIT=*) unit=${1#*=} ;;
    -o | --output) form=$2; shift ;;
    --output=*) form=${1#*=} ;;
    -o*) form=${1#-o} ;;
    -q | --quiet) quiet=true ;;
    --user) user=true ;;
  esac
  shift
done
file=$FAKE_STATE/journal-$unit
if [[ ${FAKE_SUDO:-} != 1 || $user == true || -z $unit || ! -s $file ]]; then
  $quiet || echo '-- No entries --'
  exit 0
fi
if [[ $form == cat ]]; then
  cat -- "$file"
else
  awk '{ printf "Nov 26 03:30:%02d ghost-wispr ghost-wispr-update[%d]: %s\n", NR % 60, 2000 + NR, $0 }' "$file"
fi
EOF

# tailscale on the fake Pi: serve status --json prints state/serve.json, the Serve config as
# tailscaled gives it, to root alone (sudo). With no state/serve.json, tailscaled does not answer.
cat >"$bin/tailscale" <<'EOF'
#!/usr/bin/env bash
[[ $* == 'serve status --json' ]] || { printf 'fake tailscale: unexpected arguments: %s\n' "$*" >&2; exit 1; }
[[ ${FAKE_SUDO:-} == 1 ]] || { echo 'Access denied: serve config denied' >&2; exit 1; }
[[ -f $FAKE_STATE/serve.json ]] || { echo 'failed to connect to local tailscaled' >&2; exit 1; }
cat -- "$FAKE_STATE/serve.json"
EOF

# timeout [OPTIONS] DURATION COMMAND: runs COMMAND; no fake here hangs.
cat >"$bin/timeout" <<'EOF'
#!/usr/bin/env bash
while [[ ${1:-} == -* ]]; do
  case $1 in
    -k | -s | --kill-after | --signal) shift 2 ;;
    *) shift ;;
  esac
done
shift
exec "$@"
EOF

# systemctl, on the device or the Pi: the driver changes nothing, so any call is noted and refused.
cat >"$bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_LOG/systemctl"
echo 'fake systemctl: the acceptance driver must change nothing on the Pi' >&2
exit 1
EOF
chmod +x "$bin"/*

# --- The templates, and what the recorder answers on each.

# The template the test reads its expectations from is the repository's; the new release's is the
# same with its first recording rule changed, as acceptance scenario S1 changes one.
readonly old_template=$script_dir/files/ghost-wispr.yaml new_template=$work/new-template.yaml
awk '/^recording_rules:/ { on = 1 } on && !done && /^  - / { $0 = $0 " The new release says so."; done = 1 } { print }' \
  "$old_template" >"$new_template"
! cmp -s "$old_template" "$new_template" || fail 'the new template changed no recording rule'

# template_setting FILE PATH: the value FILE gives the dotted setting PATH. The test reads the
# template's two-space indents with its own reader, apart from the driver's, so a broken reader in
# the driver cannot agree with itself.
template_setting() {
  awk -v want="$2" '
    /^[[:space:]]*(#|-|$)/ { next }
    {
      indent = match($0, /[^ ]/) - 1
      level = int(indent / 2)
      line = substr($0, indent + 1)
      key = line
      sub(/:.*/, "", key)
      path[level] = key
      p = path[0]
      for (i = 1; i <= level; i++) p = p "." path[i]
      if (p == want) {
        value = line
        sub(/^[^:]*:[[:space:]]*/, "", value)
        sub(/[[:space:]]+#.*$/, "", value)
        print value
        exit
      }
    }' "$1"
}
# template_rules FILE: FILE's recording_rules as a JSON list.
template_rules() {
  awk '/^recording_rules:/ { on = 1; next } on && /^  - / { print substr($0, 5); next } on { exit }' "$1" \
    | jq -Rcn '[inputs]'
}
# template_providers FILE: the providers FILE names, one a line.
template_providers() {
  awk '/^providers:/ { on = 1; next } on && /^[^ #]/ { exit } on && /^  [A-Za-z0-9_-]+:$/ { sub(/^  /, ""); sub(/:$/, ""); print }' "$1"
}
# Each role GET /api/config reports under "models", and the template setting that names it: the
# driver's MODEL_SETTINGS, listed here apart from the driver, so a driver that drops or misnames one
# fails that role's case below.
readonly model_settings=(
  live_transcription=transcription.model batch_transcription=batch_transcription.model
  summarization=summarization.model embedding=embedding.model concierge=concierge.model
  tts=tts.model tts_voice=tts.voice gptlive=voice.gptlive.model dgagent_provider=voice.dgagent.provider
)
# running_config FILE: what GET /api/config answers on a recorder that runs FILE: the model of each
# role of model_settings, the recording rules, and local_settings null, the report of a recorder
# that applied no local settings file.
running_config() {
  local entry value args=()
  for entry in "${model_settings[@]}"; do
    value=$(template_setting "$1" "${entry#*=}")
    [[ -n $value ]] || fail "the template $1 sets no ${entry#*=}"
    args+=(--arg "${entry%%=*}" "$value")
  done
  jq -cn "${args[@]}" --argjson rules "$(template_rules "$1")" \
    '{models: ($ARGS.named | del(.rules)), recording_rules: $rules, local_settings: null}'
}
old_config=$(running_config "$old_template")
new_config=$(running_config "$new_template")
readonly old_config new_config
[[ $(jq '.recording_rules | length' <<<"$old_config") -ge 1 ]] || fail "read no recording rule from the template: $old_config"
[[ $old_config != "$new_config" ]] || fail 'the old and new templates give the same running config'
good_providers=$(template_providers "$new_template" | jq -Rcn '{providers: [inputs | {name: ., ok: true, message: "works"}]}')
readonly good_providers
[[ $(jq '.providers | length' <<<"$good_providers") -ge 4 ]] || fail "read too few providers from the template: $good_providers"
first_provider=$(jq -er '.providers[0].name' <<<"$good_providers") || fail "read no provider name from $good_providers"
readonly first_provider
readonly idle_status='{"paused":false,"capture_state":"capturing","active_session_id":"","warnings":[]}'
good_serve=$(jq -cn --arg host "$PI_HOST" \
  '{TCP: {"443": {HTTPS: true}}, Web: {($host + ":443"): {Handlers: {"/": {Proxy: "http://127.0.0.1:8080"}}}}}')
readonly good_serve

# The update unit's journal on a Pi that the timer moved from the old release to the new one at
# 03:30, with a later run of the timer and one started by hand, which find it current.
launcher() { printf 'ghost-wispr launcher: invocation %s trigger %s at %s' "$1" "$2" "$3"; }
installed() { printf 'ghost-wispr update: installed commit %s (invocation %s)' "$1" "$2"; }
old_journal=$(printf '%s\n' \
  "$(launcher "$run_old" ghost-wispr-update.timer 1795577400000000)" \
  "$(installed "$old" "$run_old")")
good_journal=$(printf '%s\n' "$old_journal" \
  "$(launcher "$run_new" ghost-wispr-update.timer 1795663800000000)" \
  'pi-secrets: GHOST_WISPR_DEEPGRAM_API_KEY from example-deepgram-key version 3' \
  'pi-secrets: GHOST_WISPR_DISPATCH_TOKEN from example-dispatch-token version 1' \
  "$(installed "$new" "$run_new")" \
  "$(launcher "$run_current" ghost-wispr-update.timer 1795664700000000)" \
  "ghost-wispr update: already current at commit $new" \
  "$(launcher "$run_by_hand" none none)" \
  "ghost-wispr update: already current at commit $new")
readonly old_journal good_journal

# --- The releases.

# make_release COMMIT TEMPLATE: builds COMMIT's deploy tarball and its .sha256 in releases/COMMIT,
# from TEMPLATE, the repository's update unit and timer, and a launcher and serve.conf of their own.
# The flat directory stays in releases/COMMIT/stage/ghost-wispr-pi-deploy.
make_release() {
  local dir=$releases/$1 stage
  stage=$dir/stage/ghost-wispr-pi-deploy
  mkdir -p "$stage"
  cp -- "$2" "$stage/ghost-wispr.yaml"
  cp -- "$script_dir/test-pi.env" "$stage/pi.env"
  cp -- "$script_dir/files/ghost-wispr-update.service" "$script_dir/files/ghost-wispr-update.timer" "$stage/"
  printf '443 http://127.0.0.1:8080\n' >"$stage/serve.conf"
  printf '#!/bin/sh\n# The launcher: runs update.sh under its time limit.\nexec timeout 45m /opt/ghost-wispr/update.sh\n' \
    >"$stage/ghost-wispr-update-launcher.sh"
  pack_release "$1" "$dir"
}
# pack_release COMMIT DIR: writes DIR/stage/ghost-wispr-pi-deploy/manifest.json for its files, then
# the tarball and its .sha256 in DIR.
pack_release() {
  local stage=$2/stage/ghost-wispr-pi-deploy
  rm -f -- "$stage/manifest.json"
  (cd -- "$stage" && sha256sum -- *) \
    | jq -Rn --arg commit "$1" --arg binary "$(printf 'binary of %s' "$1" | sha256sum | cut -d' ' -f1)" \
      '{format: 1, commit: $commit, binary_sha256: $binary,
        files: ([inputs | capture("^(?<sum>[0-9a-f]{64})  (?<name>.+)$") | {(.name): .sum}] | add)}' \
      >"$stage/manifest.json"
  tar -C "$2/stage" -czf "$2/ghost-wispr-pi-deploy.tar.gz" ghost-wispr-pi-deploy
  (cd -- "$2" && sha256sum ghost-wispr-pi-deploy.tar.gz >ghost-wispr-pi-deploy.tar.gz.sha256)
}
make_release "$old" "$old_template"
make_release "$new" "$new_template"

# release_json COMMIT [DIR]: the releases API's answer for COMMIT's release, its assets in DIR
# (releases/COMMIT by default). The binary's asset is listed and absent: the driver never needs it.
release_json() {
  jq -cn --arg commit "$1" --arg dir "file://${2:-$releases/$1}" \
    '{tag_name: ("v2026.11.26-" + $commit[0:7]), target_commitish: $commit, assets: [
      {name: "ghost-wispr-linux-arm64", browser_download_url: ($dir + "/ghost-wispr-linux-arm64")},
      {name: "ghost-wispr-pi-deploy.tar.gz", browser_download_url: ($dir + "/ghost-wispr-pi-deploy.tar.gz")},
      {name: "ghost-wispr-pi-deploy.tar.gz.sha256", browser_download_url: ($dir + "/ghost-wispr-pi-deploy.tar.gz.sha256")}]}'
}

# filler_releases COUNT: COUNT releases of commits no case runs, newest first, as the releases API
# lists them, each with a commit of its own.
filler_releases() {
  jq -cn --argjson count "$1" --argjson release "$(release_json "$new")" \
    '[range($count) as $i | ($i | tostring) as $n
      | $release | .target_commitish = ("f" * 32 + "0" * (8 - ($n | length)) + $n)
      | .tag_name = "v2026.01.01-filler-\($n)"]'
}
# release_pages: the pages of the releases list the run asked for, in order, one line.
release_pages() { paste -sd ' ' - <"$log/release-pages" 2>/dev/null || true; }

# recorder_applies JSON: GET /api/config's local_settings becomes JSON, the recorder's report of the
# local settings file: the settings it applied from it, null for no file, or {"applied": false,
# "error": ...} for a file it did not apply. The case's other answers stay as they are.
recorder_applies() {
  jq -c --argjson local "$1" '.local_settings = $local' "$state/config.json" >"$state/config.json.new"
  mv -f -- "$state/config.json.new" "$state/config.json"
}

# --- Cases.

# new_case NAME: a fresh fake Pi on the new release, installed by the timer at 03:30, every check's
# input as it should be, and the latest release the new one.
case_count=0
new_case() {
  name=$1
  case_count=$((case_count + 1))
  case_dir=$work/case-$case_count
  deploy=$case_dir/pi/opt home=$case_dir/pi/home state=$case_dir/state github=$case_dir/github log=$case_dir/log
  units=$home/.config/systemd/user
  mkdir -p "$deploy" "$units" "$state" "$github" "$log"
  local stage=$releases/$new/stage/ghost-wispr-pi-deploy
  cp -- "$stage/ghost-wispr-update-launcher.sh" "$deploy/"
  cp -- "$stage/ghost-wispr-update.service" "$stage/ghost-wispr-update.timer" "$units/"
  printf 'first meeting\n' >"$home/meeting-a.wav"
  printf 'second meeting\n' >"$home/meeting-b.wav"
  env_file=$case_dir/pi.env
  {
    cat -- "$script_dir/test-pi.env"
    printf "PI_PROTECTED_RECORDINGS='%s'\n" "$(sha256sum -- "$home/meeting-a.wav" "$home/meeting-b.wav")"
  } >"$env_file"
  printf '{"version":"v2026.11.26-2222222","commit":"%s"}\n' "$new" >"$state/version.json"
  printf '%s\n' "$idle_status" >"$state/status.json"
  printf '%s\n' "$new_config" >"$state/config.json"
  printf '%s\n' "$good_providers" >"$state/providers.json"
  printf '%s\n' "$good_journal" >"$state/journal-ghost-wispr-update.service"
  printf '%s\n' "$good_serve" >"$state/serve.json"
  release_json "$new" >"$github/latest.json"
  jq -cn --argjson a "$(release_json "$new")" --argjson b "$(release_json "$old")" '[$a, $b]' >"$github/releases.json"
}
# on_old: the fake Pi runs the old release instead, with the old release's config, installed by the
# timer the night before.
on_old() {
  printf '{"version":"v2026.11.25-1111111","commit":"%s"}\n' "$old" >"$state/version.json"
  printf '%s\n' "$old_config" >"$state/config.json"
  printf '%s\n' "$old_journal" >"$state/journal-ghost-wispr-update.service"
}

# run_driver [ARGS]: runs the driver from deploy/pi, or from driver_copy when a case sets it,
# against this case's fake Pi and release; its output is in $out, its exit status in $rc. A run
# that has not ended in 120 s is killed (exit 124), so a driver that never stops fails the test
# rather than hangs it.
run_driver() {
  rc=0
  out=$(PATH="$bin:$PATH" FAKE_HOME="$home" FAKE_STATE="$state" FAKE_GITHUB="$github" FAKE_LOG="$log" \
    FAKE_PI_HOST="$PI_HOST" FAKE_RELEASES_API="$releases_api" FAKE_REAL_CURL="$real_curl" FAKE_REAL_CAT="$real_cat" \
    GHOST_WISPR_PI_ENV="$env_file" GHOST_WISPR_DEPLOY_DIR="$deploy" GHOST_WISPR_RELEASES_API="$releases_api" \
    "$real_timeout" 120 bash "${driver_copy:-$driver}" "$@" 2>&1) || rc=$?
}

# verdict_line N: the one line the run printed for check N.
verdict_line() {
  local lines count
  lines=$(grep -E "^check $1 (PASS|FAIL|UNKNOWN): " <<<"$out" || true)
  count=$(grep -c . <<<"$lines" || true)
  [[ $count == 1 ]] || fail "the run printed $count lines for check $1, want 1 (exit $rc): $out"
  printf '%s\n' "$lines"
}
# expect N VERDICT [WORDS...]: check N printed VERDICT, and its line holds each of WORDS.
expect() {
  local n=$1 want=$2 got words
  got=$(verdict_line "$n") || exit 1
  [[ $got == "check $n $want: "* ]] || fail "check $n: want $want, got: $got"
  shift 2
  for words in "$@"; do
    [[ $got == *"$words"* ]] || fail "check $n's line does not say \"$words\": $got"
  done
}
# expect_verdicts V1 ... V7: the seven checks printed these verdicts, in order, seven check lines in
# all, and the run exited 0 if all seven are PASS, else 1.
expect_verdicts() {
  local n=0 verdict total
  for verdict in "$@"; do
    n=$((n + 1))
    expect "$n" "$verdict"
  done
  total=$(grep -cE '^check [0-9]+ (PASS|FAIL|UNKNOWN): ' <<<"$out" || true)
  [[ $total == 7 ]] || fail "the run printed $total check lines, want 7: $out"
  if [[ " $* " == *' FAIL '* || " $* " == *' UNKNOWN '* ]]; then
    ((rc == 1)) || fail "exit $rc with a check not PASS, want 1: $out"
  else
    ((rc == 0)) || fail "exit $rc with every check PASS, want 0: $out"
  fi
}
# pi_tree: every path under the fake Pi, with its type, mode, size and modification time.
pi_tree() { (cd -- "$case_dir/pi" && find . -printf '%y %m %s %T@ %p\n' | LC_ALL=C sort); }
# unreadable PATH...: on the fake Pi, ghost-wispr may not read each PATH (the fake cat refuses it).
unreadable() { printf '%s\n' "$@" >>"$state/unreadable"; }

# The good run: a Pi on the latest release, installed at 03:30 by the timer's run, with a later run
# of the timer and one started by hand that find it current. Every check passes, and the run
# changes nothing on the Pi. Breaks it catches: a driver that pairs the install with the last
# launcher line (the run by hand) instead of the one with its invocation id; one that reads the
# recorder on 127.0.0.1 over SSH instead of over Serve (the fake curl refuses it); one that reads the
# journal without sudo -n, or the user journal the Pi does not keep (both find nothing); one that
# changes the Pi, such as turning the update timer on; one whose ssh can wait on a password prompt
# or hang on a Pi that does not answer.
new_case 'the good run'
before=$(pi_tree)
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
expect 1 PASS "$new"
expect 5 PASS ghost-wispr-update.timer "$new" "$run_new"
[[ $(pi_tree) == "$before" ]] || fail "the driver changed the Pi: before
$before
after
$(pi_tree)"
[[ ! -s $log/systemctl ]] || fail "the driver ran systemctl: $(<"$log/systemctl")"
if ! grep -qx 'BatchMode=yes' "$log/ssh-options" || ! grep -q '^ConnectTimeout=' "$log/ssh-options"; then
  fail "the driver's ssh has no BatchMode=yes and ConnectTimeout: $(cat -- "$log/ssh-options" 2>/dev/null)"
fi

# S1's control: the Pi has the old commit and the old rule text, and the latest release changes a
# recording rule. Check 1 (the commit) and check 2 (the rule text) both print FAIL in the one run;
# check 5 finds no install of the new commit, and the rest pass. Breaks it catches: a driver that
# stops at its first failing check, so check 2 never prints; one that compares the running rules
# with the checkout's template, or with the config installed on the Pi, which both hold the old
# text, instead of the release's tarball.
new_case "S1's control: the old commit and the old rule text"
on_old
run_driver
expect_verdicts FAIL FAIL PASS PASS UNKNOWN PASS PASS
expect 1 FAIL "$old" "$new"
expect 2 FAIL recording_rules
expect 5 UNKNOWN "$new"

# --release names the release to check instead of the latest. Break it catches: a driver that
# ignores --release and checks the latest release, which a Pi that is meant to be on an older one
# fails.
new_case '--release an older release'
on_old
run_driver --release "$old"
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
expect 1 PASS "$old"
# A --release that names no commit is refused before any check runs. Break it catches: a driver that
# compares the Pi with a branch name or an empty commit.
new_case '--release with no commit'
run_driver --release main
((rc != 0 && rc != 1)) || fail "--release main exited $rc, want a usage error: $out"
! grep -qE '^check [0-9]+ PASS: ' <<<"$out" || fail "--release main printed a PASS: $out"

# --release finds its release past the first page of the releases API, which gives at most 100 a
# page: here 100 newer releases come before it, so it is the first of page 2. Break it catches: a
# driver that reads the first page alone.
new_case '--release a release on the second page'
on_old
jq -c --argjson old "$(release_json "$old")" '. + [$old]' <<<"$(filler_releases 100)" >"$github/releases.json"
run_driver --release "$old"
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
expect 1 PASS "$old"
[[ $(release_pages) == '1 2' ]] || fail "the driver read the pages $(release_pages) of the releases, want 1 2"
# A commit no release has: the driver reads every page, up to the first with fewer than 100
# releases, and says how many it read. Breaks it catches: a driver that stops before the last page,
# or reads past it for ever.
new_case '--release a commit no release has'
filler_releases 150 >"$github/releases.json"
unreleased=3333333333333333333333333333333333333333
run_driver --release "$unreleased"
expect_verdicts FAIL UNKNOWN UNKNOWN PASS UNKNOWN PASS UNKNOWN
expect 2 UNKNOWN "no release of commit $unreleased among the 150 releases"
[[ $(release_pages) == '1 2' ]] || fail "the driver read the pages $(release_pages) of the releases, want 1 2"
# An API that answers page 2 with page 1's releases would be read for ever: the driver stops at the
# page that repeats and says so. Break it catches: a driver that reads pages until one is short,
# and so never ends on such an answer (run_driver kills it after 120 s).
new_case 'a releases API that answers every page with the first'
on_old
jq -c --argjson old "$(release_json "$old")" '. + [$old]' <<<"$(filler_releases 100)" >"$github/releases.json"
: >"$github/pages-ignored"
run_driver --release "$old"
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS UNKNOWN
expect 2 UNKNOWN 'page 2'
[[ $(release_pages) == '1 2' ]] || fail "the driver read the pages $(release_pages) of the releases, want 1 2"

# Check 1: the Pi answers no version over Serve, or a version with no commit. Break it catches: a
# driver that takes an unreadable or empty commit for a pass or a plain mismatch.
new_case 'Serve does not answer'
rm -f -- "$state/version.json" "$state/status.json" "$state/config.json" "$state/providers.json"
run_driver
expect_verdicts UNKNOWN UNKNOWN UNKNOWN UNKNOWN PASS PASS PASS
expect 1 UNKNOWN /api/version
new_case 'a version with no commit'
printf '{"version":"dev"}\n' >"$state/version.json"
run_driver
expect_verdicts UNKNOWN PASS PASS PASS PASS PASS PASS

# Check 2: each model setting, run with a value the template does not give it, fails, and the line
# names that role, its setting and the value it runs, and no other difference. One case a role.
# Breaks it catches: a driver that compares the recording rules alone, or skips a role; one that
# compares a role with another role's setting.
for entry in "${model_settings[@]}"; do
  role=${entry%%=*} setting=${entry#*=}
  new_case "a running $role that the template does not name"
  jq -c --arg role "$role" '.models[$role] = "other/\($role)-value"' <<<"$new_config" >"$state/config.json"
  run_driver
  expect_verdicts PASS FAIL PASS PASS PASS PASS PASS
  expect 2 FAIL "$role runs \"other/$role-value\"" "($setting)"
  [[ $(grep -o 'but the template names' <<<"$(verdict_line 2)" | grep -c .) == 1 ]] \
    || fail "check 2 names a difference besides $role's: $(verdict_line 2)"
done
# A role the running config does not report at all was not read: the recorder reports all nine
# (internal/server's GET /api/config), so an answer without one cannot show what that role runs.
# Check 2 is UNKNOWN and names it, never a model named null, and never the template's model. The
# control: the same role reported as an empty model is a value the driver read, and differs: FAIL.
# Breaks it catches: a driver that reads a missing model as a difference, as the model null, or as
# the template's model; one that reads an empty model as missing.
new_case 'a running config that reports no concierge model'
jq -c 'del(.models.concierge)' <<<"$new_config" >"$state/config.json"
run_driver
expect_verdicts PASS UNKNOWN PASS PASS PASS PASS PASS
expect 2 UNKNOWN 'reports no concierge model' concierge.model
[[ $(verdict_line 2) != *null* ]] || fail "check 2 calls the missing model null: $(verdict_line 2)"
jq -c '.models.concierge = ""' <<<"$new_config" >"$state/config.json"
run_driver
expect_verdicts PASS FAIL PASS PASS PASS PASS PASS
expect 2 FAIL 'concierge runs ""' '(concierge.model)'
# The local settings come from the recorder's own report, GET /api/config's local_settings: the
# settings it applied from ghost-wispr.local.yaml, as JSON. The driver reads no YAML of that file.
# Each case below writes the file on the fake Pi as the recorder read it, and the report the
# recorder gives of it.
#
# A model saved on the page: check 2 compares the running model with the report's value, and names
# the setting. The control is the same Pi whose recorder reports no local settings, whose check 2
# fails. Breaks it catches: a driver that reads no local settings, so every page save fails it; one
# that reads them and does not say which settings it took from there.
new_case 'a model saved on the page'
printf 'summarization:\n    model: vertex/gemini-saved-on-the-page\n' >"$deploy/ghost-wispr.local.yaml"
jq -c '.models.summarization = "vertex/gemini-saved-on-the-page"' <<<"$new_config" >"$state/config.json"
recorder_applies '{"summarization":{"model":"vertex/gemini-saved-on-the-page"}}'
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
expect 2 PASS summarization.model
rm -f -- "$deploy/ghost-wispr.local.yaml"
recorder_applies null
run_driver
expect_verdicts PASS FAIL PASS PASS PASS PASS PASS
expect 2 FAIL summarization vertex/gemini-saved-on-the-page

# Sol's cases (GPT-6.1 Sol's review of PR #201 at 6c53026b, finding 1): a local settings file in
# YAML's flow style, which the recorder applies as it applies the block style. With the release's
# model still running, check 2 fails; with the saved model running, all seven pass, and the driver
# read no local settings file over SSH; with the block style and the release's model running,
# check 2 fails too, the control. Breaks they catch: a driver that reads the file with a reader that
# skips the flow style, so a stale model passes and the right one fails.
new_case 'a flow-style local settings file, and the release model running'
printf 'summarization: {model: vertex/gemini-saved-on-the-page}\n' >"$deploy/ghost-wispr.local.yaml"
recorder_applies '{"summarization":{"model":"vertex/gemini-saved-on-the-page"}}'
run_driver
expect_verdicts PASS FAIL PASS PASS PASS PASS PASS
expect 2 FAIL summarization vertex/gemini-saved-on-the-page
new_case 'a flow-style local settings file, and its model running'
printf 'summarization: {model: vertex/gemini-saved-on-the-page}\n' >"$deploy/ghost-wispr.local.yaml"
jq -c '.models.summarization = "vertex/gemini-saved-on-the-page"' <<<"$new_config" >"$state/config.json"
recorder_applies '{"summarization":{"model":"vertex/gemini-saved-on-the-page"}}'
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
expect 2 PASS summarization.model
[[ -s $log/ssh ]] || fail "the driver made no SSH call, so its SSH log shows nothing: $out"
! grep -q 'ghost-wispr\.local\.yaml' "$log/ssh" \
  || fail "the driver read the local settings file over SSH: $(grep 'ghost-wispr\.local\.yaml' "$log/ssh")"
new_case 'a block-style local settings file, and the release model running'
printf 'summarization:\n    model: vertex/gemini-saved-on-the-page\n' >"$deploy/ghost-wispr.local.yaml"
recorder_applies '{"summarization":{"model":"vertex/gemini-saved-on-the-page"}}'
run_driver
expect_verdicts PASS FAIL PASS PASS PASS PASS PASS
expect 2 FAIL summarization vertex/gemini-saved-on-the-page

# Recording rules the recorder took from the local settings file (an older build saved
# GHOST_WISPR_RECORDING_RULES there) are a list in its report. Break it catches: a driver that
# compares the running rules with the template's when the report gives its own.
new_case 'recording rules in the local settings'
printf 'recording_rules:\n    - A rule from the environment, saved.\n' >"$deploy/ghost-wispr.local.yaml"
jq -c '.recording_rules = ["A rule from the environment, saved."]' <<<"$new_config" >"$state/config.json"
recorder_applies '{"recording_rules":["A rule from the environment, saved."]}'
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
expect 2 PASS recording_rules

# Checks 2 and 3 cannot read their input: GET /api/config does not answer, or it answers with no
# local_settings field (a recorder from before the field), or with one that is neither null nor an
# object. A provider that did not sign in still fails check 3, and the line names what it could not
# read too. Breaks it catches: a driver that passes check 2 on a config it never read, or compares
# with the template alone when it cannot tell what the recorder applied; one whose check 3 passes
# with no word of the providers the local settings add or remove.
new_case 'GET /api/config does not answer'
rm -f -- "$state/config.json"
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS PASS
expect 2 UNKNOWN /api/config
expect 3 UNKNOWN /api/config
new_case 'a recorder that reports no local_settings'
jq -c 'del(.local_settings)' <<<"$new_config" >"$state/config.json"
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS PASS
expect 2 UNKNOWN local_settings
expect 3 UNKNOWN local_settings
jq -c '.providers[0].ok = false | .providers[0].message = "401 Unauthorized"' <<<"$good_providers" >"$state/providers.json"
run_driver
expect_verdicts PASS UNKNOWN FAIL PASS PASS PASS PASS
expect 3 FAIL "$first_provider" local_settings
printf '%s\n' "$good_providers" >"$state/providers.json"
recorder_applies '"summarization"'
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS PASS
expect 2 UNKNOWN local_settings

# The recorder did not apply the local settings file (it does not parse): it runs the release's
# config alone, and its report says so, with why. Checks 2 and 3 compare with the template alone,
# pass, and name the file the recorder did not apply, and why. The recorder also warns of that file
# in /api/status (each warning of its config's load goes there, cmd/ghost-wispr/main.go's
# warningRegistry), so check 4 fails, and the run with it. Breaks it catches: a driver that reads
# that report as settings (the keys applied and error), or that hides that the file was not
# applied; a run that passes on a Pi whose page saves are not applied.
new_case 'a local settings file the recorder did not apply'
printf 'summarization: [\n' >"$deploy/ghost-wispr.local.yaml"
recorder_applies '{"applied":false,"error":"yaml: line 2: did not find expected node content"}'
jq -c --arg warning "$deploy/ghost-wispr.local.yaml: yaml: line 2: did not find expected node content; settings saved on the page are not applied" \
  '.warnings = [$warning]' <<<"$idle_status" >"$state/status.json"
run_driver
expect_verdicts PASS PASS PASS FAIL PASS PASS PASS
expect 2 PASS 'did not apply' 'did not find expected node content'
expect 3 PASS 'did not apply'
expect 4 FAIL 'ghost-wispr.local.yaml' 'settings saved on the page are not applied'

# A model that differs is a FAIL even when check 2 cannot read another of its inputs, here
# recording rules that the report gives as no list of text; the line names both. The control is the
# same Pi with the model the template gives, whose check 2 is UNKNOWN. Breaks it catches: a driver
# that prints UNKNOWN and so hides a difference it found; one that passes rules it could not read.
new_case 'a model that differs, and recording rules that cannot be read'
jq -c '.models.embedding = "vertex/other-embedding"' <<<"$new_config" >"$state/config.json"
recorder_applies '{"recording_rules":"A rule, not a list."}'
run_driver
expect_verdicts PASS FAIL PASS PASS PASS PASS PASS
expect 2 FAIL 'embedding runs' vertex/other-embedding 'local_settings gives recording_rules'
printf '%s\n' "$new_config" >"$state/config.json"
recorder_applies '{"recording_rules":"A rule, not a list."}'
run_driver
expect_verdicts PASS UNKNOWN PASS PASS PASS PASS PASS
expect 2 UNKNOWN 'local_settings gives recording_rules'

# Check 3: a provider that did not sign in, one the template does not name, and one the template
# names that the Pi does not list each fail, and the line names it. Breaks it catches: a driver that
# lists the providers without reading ok; one that checks only that the template's providers are
# there; one that checks only that the Pi lists no provider the template does not name.
new_case 'a provider that did not sign in'
jq -c '.providers[0].ok = false | .providers[0].message = "401 Unauthorized"' <<<"$good_providers" >"$state/providers.json"
run_driver
expect_verdicts PASS PASS FAIL PASS PASS PASS PASS
expect 3 FAIL "$first_provider"
new_case 'a provider the template does not name'
jq -c '.providers += [{name: "gateway", ok: true, message: "works"}]' <<<"$good_providers" >"$state/providers.json"
run_driver
expect_verdicts PASS PASS FAIL PASS PASS PASS PASS
expect 3 FAIL gateway
new_case 'a provider the template names that the Pi does not list'
missing_provider=$(jq -er '.providers[-1].name' <<<"$good_providers") || fail "read no provider name from $good_providers"
jq -c 'del(.providers[-1])' <<<"$good_providers" >"$state/providers.json"
run_driver
expect_verdicts PASS PASS FAIL PASS PASS PASS PASS
expect 3 FAIL "the Pi lists no $missing_provider"
# A provider the local settings add runs, since the recorder applies them over the template, and
# passes. Break it catches: a driver whose check 3 fails for good on a Pi whose local settings file
# adds a provider.
new_case 'a provider added to the local settings'
printf 'providers:\n    gateway:\n        api: openai\n' >"$deploy/ghost-wispr.local.yaml"
recorder_applies '{"providers":{"gateway":{"api":"openai"}}}'
jq -c '.providers += [{name: "gateway", ok: true, message: "works"}]' <<<"$good_providers" >"$state/providers.json"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS PASS
# The local settings set to null each provider the template names, and the Pi lists none: no
# provider is left to check, which is no pass. Break it catches: a driver that applies the nulls and
# then passes an empty list against an empty list.
new_case 'local settings that remove every provider'
{
  echo 'providers:'
  template_providers "$new_template" | sed 's/^/    /; s/$/: null/'
} >"$deploy/ghost-wispr.local.yaml"
recorder_applies "$(template_providers "$new_template" | jq -Rcn '{providers: (reduce inputs as $name ({}; .[$name] = null))}')"
printf '{"providers":[]}\n' >"$state/providers.json"
run_driver
expect_verdicts PASS PASS UNKNOWN PASS PASS PASS PASS
expect 3 UNKNOWN 'no provider' ghost-wispr.local.yaml
new_case 'GET /api/providers does not answer'
rm -f -- "$state/providers.json"
run_driver
expect_verdicts PASS PASS UNKNOWN PASS PASS PASS PASS
expect 3 UNKNOWN /api/providers

# Check 4: a warning fails; a status with no warnings list is not a pass. Break it catches: a driver
# that reads a missing warnings list as an empty one.
new_case 'a warning'
jq -c '.warnings = ["Search cannot index sessions"]' <<<"$idle_status" >"$state/status.json"
run_driver
expect_verdicts PASS PASS PASS FAIL PASS PASS PASS
expect 4 FAIL 'Search cannot index sessions'
new_case 'a status with no warnings list'
printf '{"paused":false,"capture_state":"capturing"}\n' >"$state/status.json"
run_driver
expect_verdicts PASS PASS PASS UNKNOWN PASS PASS PASS

# Check 5: the run that installed the commit was started by hand, and the timer's runs after it find
# it current. Breaks it catches: a driver that reads the last launcher line, or the first, instead of
# the one whose invocation id the install line names; one that passes on an install line alone.
new_case 'an install started by hand'
printf '%s\n' "$old_journal" \
  "$(launcher "$run_by_hand" none none)" \
  "$(installed "$new" "$run_by_hand")" \
  "$(launcher "$run_current" ghost-wispr-update.timer 1795664700000000)" \
  "ghost-wispr update: already current at commit $new" >"$state/journal-ghost-wispr-update.service"
run_driver
expect_verdicts PASS PASS PASS PASS FAIL PASS PASS
expect 5 FAIL "$run_by_hand" none
# The journal has no install line for the commit (rotated away, or the Pi was installed another
# way), or the install line's run has no launcher line. Break it catches: a driver that passes
# check 5 on "already current" lines, or on the install line with no launcher line to show what
# started it.
new_case 'no install line for the commit'
printf '%s\n' "$old_journal" \
  "$(launcher "$run_current" ghost-wispr-update.timer 1795664700000000)" \
  "ghost-wispr update: already current at commit $new" >"$state/journal-ghost-wispr-update.service"
run_driver
expect_verdicts PASS PASS PASS PASS UNKNOWN PASS PASS
expect 5 UNKNOWN "installed commit $new"
new_case 'an install line whose run has no launcher line'
printf '%s\n' "$old_journal" "$(installed "$new" "$run_new")" >"$state/journal-ghost-wispr-update.service"
run_driver
expect_verdicts PASS PASS PASS PASS UNKNOWN PASS PASS
expect 5 UNKNOWN "$run_new"
# A launcher line with no invocation id (INVOCATION_ID unset), and so an install line with none.
# Break it catches: a driver that pairs the empty id of the install line with the empty id of a
# launcher line, and passes because that line names the timer.
new_case 'a launcher line with no invocation id'
printf '%s\n' "$old_journal" \
  "$(launcher '' ghost-wispr-update.timer 1795663800000000)" \
  "$(installed "$new" '')" >"$state/journal-ghost-wispr-update.service"
run_driver
expect_verdicts PASS PASS PASS PASS UNKNOWN PASS PASS
# An empty journal is no pass. Break it catches: a driver that reads journalctl's "-- No entries --"
# or an empty answer as a journal it searched.
new_case 'an empty journal'
: >"$state/journal-ghost-wispr-update.service"
run_driver
expect_verdicts PASS PASS PASS PASS UNKNOWN PASS PASS
expect 5 UNKNOWN ghost-wispr-update.service

# sudo -n refused: the journal and the Serve status cannot be read. Break it catches: a driver that
# takes sudo's refusal for an empty journal or an empty Serve config.
new_case 'sudo -n refused'
: >"$state/sudo-refused"
run_driver
expect_verdicts PASS PASS PASS PASS UNKNOWN PASS UNKNOWN
expect 5 UNKNOWN sudo
expect 7 UNKNOWN sudo

# SSH does not reach the Pi: every check that reads over SSH is UNKNOWN and says so; the checks over
# Serve still run, checks 2 and 3 among them, which read the recorder's report and no file of the
# Pi. Break it catches: a driver that stops at the first SSH failure, or that takes ssh's exit 255
# for a mismatch or for a clean answer.
new_case 'ssh unreachable'
: >"$state/ssh-down"
run_driver
expect_verdicts PASS PASS PASS PASS UNKNOWN UNKNOWN UNKNOWN
for n in 5 6 7; do expect "$n" UNKNOWN SSH; done

# Check 6: a protected recording changed, and a list that names none. Breaks it catches: a driver
# that does not hash the recordings; one that passes a list that names no recording.
new_case 'a changed protected recording'
printf 'edited\n' >>"$home/meeting-b.wav"
run_driver
expect_verdicts PASS PASS PASS PASS PASS FAIL PASS
expect 6 FAIL meeting-b.wav
new_case 'no protected recording named'
printf '%s\n' "$(<"$script_dir/test-pi.env")" "PI_PROTECTED_RECORDINGS=''" >"$env_file"
run_driver
expect_verdicts PASS PASS PASS PASS PASS UNKNOWN PASS
expect 6 UNKNOWN PI_PROTECTED_RECORDINGS

# Check 7: a bootstrap file that differs from the release's manifest, one that is missing, and a
# Serve target that differs from serve.conf each fail and name the difference. A missing bootstrap
# file is a FAIL, not an UNKNOWN: the driver read the Pi and found the file absent, which is a
# difference from the release (D4). Only a file the Pi has and ghost-wispr cannot read is UNKNOWN
# ('an unreadable bootstrap file' below). Breaks it catches: a driver that compares the installed
# files with the checkout instead of the manifest, or names the wrong file; one that reads a missing
# launcher (a Pi that never ran the release's provision.sh) as a match; one that compares Serve's
# ports alone.
new_case 'a changed timer file'
printf '# edited on the Pi\n' >>"$units/ghost-wispr-update.timer"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS FAIL
expect 7 FAIL ghost-wispr-update.timer
[[ $(verdict_line 7) != *ghost-wispr-update.service* && $(verdict_line 7) != *ghost-wispr-update-launcher.sh* ]] \
  || fail "check 7 names a bootstrap file that matches: $(verdict_line 7)"
new_case 'a missing launcher'
rm -f -- "$deploy/ghost-wispr-update-launcher.sh"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS FAIL
expect 7 FAIL ghost-wispr-update-launcher.sh
# Sol's case (GPT-6.1 Sol's review of PR #201 at 6c53026b, finding 4): the launcher is missing, and
# SSH is lost while the driver reads the next bootstrap file. The absence that the first read found
# is a FAIL whatever the later reads give, and the line names the missing launcher and the lost
# SSH. Break it catches: a driver that keeps a file it found absent only when every later read
# succeeds, so a lost connection turns a known difference into UNKNOWN.
new_case 'a missing launcher, then SSH lost'
rm -f -- "$deploy/ghost-wispr-update-launcher.sh"
printf '%s' '.config/systemd/user/ghost-wispr-update.service' >"$state/ssh-lost-on"
run_driver
[[ -e $state/ssh-lost-fired ]] || fail "SSH was never lost: the driver did not read ghost-wispr-update.service: $out"
expect_verdicts PASS PASS PASS PASS PASS PASS FAIL
expect 7 FAIL ghost-wispr-update-launcher.sh SSH
new_case 'a Serve target that differs'
jq -c '.Web[].Handlers["/"].Proxy = "http://127.0.0.1:9090"' <<<"$good_serve" >"$state/serve.json"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS FAIL
expect 7 FAIL Serve 'http://127.0.0.1:9090' 'http://127.0.0.1:8080'
# Check 7 cannot read its input: a Serve status that is not the JSON it reads, tailscaled not
# answering, or a bootstrap file it cannot read. Break it catches: a driver that reads an unreadable
# answer as no Serve config, or as a match.
new_case 'an unreadable Serve status'
printf 'tailscaled is starting\n' >"$state/serve.json"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS UNKNOWN
expect 7 UNKNOWN 'tailscale serve status'
new_case 'tailscaled does not answer'
rm -f -- "$state/serve.json"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS UNKNOWN
expect 7 UNKNOWN 'tailscale serve status'
new_case 'an unreadable bootstrap file'
unreadable "$units/ghost-wispr-update.service"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS UNKNOWN
expect 7 UNKNOWN ghost-wispr-update.service

# The driver reads the bootstrap files and Serve with the functions of the pi-lib.sh beside it,
# bootstrap_drift, serve_conf_lines and serve_lines_from_status, which update.sh's nightly drift
# rule runs, so the two cannot read them two ways. Here it runs from a copy of deploy/pi whose
# pi-lib.sh is the repository's, with bootstrap_drift and serve_lines_from_status replaced by ones
# that name a timer and a Serve line the fake Pi does not have: check 7 fails with both. The same
# copy with no pi-lib.sh stops with a usage error that names it, and prints no check. Breaks it
# catches: a driver that keeps its own drift rule or Serve parser; one that sources a pi-lib.sh
# other than its sibling; one that runs its checks without the library.
new_case "the driver reads with its sibling pi-lib.sh"
lib_tree=$case_dir/tree
mkdir -p "$lib_tree"
cp -- "$driver" "$lib_tree/"
{
  cat -- "$script_dir/pi-lib.sh"
  printf '%s\n' \
    "bootstrap_drift() { printf 'ghost-wispr-update.timer\\n'; }" \
    "serve_lines_from_status() { printf '443 http://127.0.0.1:7777\\n'; }"
} >"$lib_tree/pi-lib.sh"
driver_copy=$lib_tree/check-deploy.sh run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS FAIL
expect 7 FAIL 'provisioning file ghost-wispr-update.timer' 'have 443 http://127.0.0.1:7777'
rm -f -- "$lib_tree/pi-lib.sh"
driver_copy=$lib_tree/check-deploy.sh run_driver
((rc == 2)) || fail "with no pi-lib.sh the driver exited $rc, want 2: $out"
! grep -qE '^check [0-9]+ (PASS|FAIL|UNKNOWN): ' <<<"$out" || fail "with no pi-lib.sh the driver printed a check: $out"
[[ $out == *pi-lib.sh* ]] || fail "with no pi-lib.sh the driver did not name it: $out"

# The release cannot be read: the checks that compare with it are UNKNOWN; the status and the
# protected recordings still run. Break it catches: a driver that compares with the checkout's files
# when GitHub does not answer.
new_case 'the releases API does not answer'
rm -f -- "$github/latest.json"
run_driver
expect_verdicts UNKNOWN UNKNOWN UNKNOWN PASS UNKNOWN PASS UNKNOWN
# The tarball does not match its .sha256, its files do not match its manifest, its manifest names
# another commit, or the release has no tarball (a release from before the deploy set): the checks
# that read the template, serve.conf or the manifest are UNKNOWN; the commit check still runs.
# Breaks it catches: a driver that uses a tarball it did not verify, or the checkout's template when
# the release's cannot be used.
new_case 'a tarball that does not match its .sha256'
mkdir -p "$case_dir/release"
cp -- "$releases/$new/ghost-wispr-pi-deploy.tar.gz" "$case_dir/release/"
printf '%064d  ghost-wispr-pi-deploy.tar.gz\n' 0 >"$case_dir/release/ghost-wispr-pi-deploy.tar.gz.sha256"
release_json "$new" "$case_dir/release" >"$github/latest.json"
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS UNKNOWN
expect 2 UNKNOWN .sha256
new_case 'a template that does not match the manifest'
mkdir -p "$case_dir/release/stage"
cp -R -- "$releases/$new/stage/ghost-wispr-pi-deploy" "$case_dir/release/stage/"
pack_release "$new" "$case_dir/release"
printf '# changed after the manifest was written\n' >>"$case_dir/release/stage/ghost-wispr-pi-deploy/ghost-wispr.yaml"
tar -C "$case_dir/release/stage" -czf "$case_dir/release/ghost-wispr-pi-deploy.tar.gz" ghost-wispr-pi-deploy
(cd -- "$case_dir/release" && sha256sum ghost-wispr-pi-deploy.tar.gz >ghost-wispr-pi-deploy.tar.gz.sha256)
release_json "$new" "$case_dir/release" >"$github/latest.json"
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS UNKNOWN
expect 2 UNKNOWN manifest
new_case "a release that carries another commit's deploy set"
release_json "$new" "$releases/$old" >"$github/latest.json"
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS UNKNOWN
expect 2 UNKNOWN manifest
new_case 'a release with no deploy tarball'
jq -c '.assets |= map(select(.name | startswith("ghost-wispr-pi-deploy") | not))' <<<"$(release_json "$new")" >"$github/latest.json"
run_driver
expect_verdicts PASS UNKNOWN UNKNOWN PASS PASS PASS UNKNOWN
expect 2 UNKNOWN ghost-wispr-pi-deploy.tar.gz

# The driver runs from a tailnet member device and names no devbox: the box that once ran the Pi's
# procedures is no part of them. The pattern is built from parts, so this file names none of them.
# Break it catches: a driver that reaches the Pi or a secret through that box or its identity.
name='no devbox'
[[ -f $driver ]] || fail "no $driver"
! grep -niE 'sami''-devbox|dev''-sjawhar@|devbox identity' -- "$driver" || fail "$driver names the devbox"

# Serve's setting is every handler it serves, not its proxies alone. A Serve that also serves files
# on 8443, which serve.conf does not name, differs from the release; a handler that is not a
# handler at all cannot be read, which is UNKNOWN, never a match. Breaks they catch: a Serve reader
# that drops a handler that is no proxy, so a port someone served by hand passes; one that drops a
# handler it cannot read.
new_case 'Serve also serves files on 8443'
jq -c --arg host "$PI_HOST" \
  '.TCP["8443"] = {HTTPS: true} | .Web[$host + ":8443"] = {Handlers: {"/": {Path: "/home/ghost-wispr/public"}}}' \
  <<<"$good_serve" >"$state/serve.json"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS FAIL
expect 7 FAIL Serve 8443
new_case 'a Serve handler that cannot be read'
jq -c --arg host "$PI_HOST" \
  '.TCP["8443"] = {HTTPS: true} | .Web[$host + ":8443"] = {Handlers: {"/": "http://127.0.0.1:8081"}}' \
  <<<"$good_serve" >"$state/serve.json"
run_driver
expect_verdicts PASS PASS PASS PASS PASS PASS UNKNOWN
expect 7 UNKNOWN 'tailscale serve status'

printf 'check-deploy tests passed\n'
