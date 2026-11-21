#!/usr/bin/env bash
# Runs deploy/pi/rollout.sh's steps against a fake Pi. ssh runs each remote command here, under sh,
# in a sandbox whose deploy directory, home, protected recordings and recorder answers stand for the
# Pi's, with fakes of systemctl, the recorder's HTTP answers, pgrep and sleep. The rollout runs from
# a copy of deploy/pi that is its own git checkout, the merge commit, whose update.sh and
# provision.sh are fakes that install the fake release and config as the real ones do.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT

fail() {
  printf 'rollout test: %s\n' "$*" >&2
  exit 1
}

readonly tree=$work/tree pi=$work/pi bin=$work/bin
readonly deploy=$pi/opt state=$pi/state
readonly old=1111111111111111111111111111111111111111
readonly drive='0AAbCdEf_Gh-Ij'
mkdir -p "$tree/deploy/pi/files" "$deploy" "$pi/home" "$state" "$bin"

# The checkout: rollout.sh and the template as they are, a fake update.sh and provision.sh.
cp -- "$repo_root/deploy/pi/rollout.sh" "$tree/deploy/pi/"
cp -- "$repo_root/deploy/pi/files/ghost-wispr.yaml" "$tree/deploy/pi/files/"
cat >"$tree/deploy/pi/update.sh" <<'EOF'
#!/usr/bin/env bash
# The fake update.sh: installs the fake Pi's latest release, as update.sh does. The rollout runs it
# paused-only, so a resume during the download stops it before the swap (update_test.sh).
set -euo pipefail
printf 'update.sh\n' >>"$FAKE_PI/state/events"
[[ ${GHOST_WISPR_UPDATE_PAUSED_ONLY:-} == 1 ]] || { echo 'fake update.sh: run without GHOST_WISPR_UPDATE_PAUSED_ONLY=1' >&2; exit 9; }
deploy=${GHOST_WISPR_DEPLOY_DIR:?}
release=$(<"$FAKE_PI/state/release")
if [[ $(sed -n 's/^commit //p' "$deploy/ghost-wispr") == "$release" ]]; then
  printf 'ghost-wispr update: already on commit %s\n' "$release"
  exit 0
fi
cp -p "$deploy/ghost-wispr" "$deploy/ghost-wispr.prev"
printf 'commit %s\n' "$release" >"$deploy/ghost-wispr"
systemctl --user restart ghost-wispr.service
printf 'ghost-wispr update: installed commit %s\n' "$release"
EOF
cat >"$tree/deploy/pi/provision.sh" <<'EOF'
#!/usr/bin/env bash
# The fake provision.sh --models: installs the template with the shared drive, drops a removed
# variable from .env and restarts the recorder, as provision.sh --models does.
set -euo pipefail
[[ $# -eq 2 && $1 == --models ]] || { echo "fake provision.sh: $*" >&2; exit 2; }
printf 'provision --models %s\n' "$2" >>"$FAKE_PI/state/events"
here=$(cd -- "$(dirname -- "$0")" && pwd)
deploy=${GHOST_WISPR_DEPLOY_DIR:?}
{ cat "$here/files/ghost-wispr.yaml"; printf 'gdrive_folder_id: "%s"\n' "$2"; } >"$deploy/ghost-wispr.yaml"
grep -v '^GHOST_WISPR_EMBEDDING_LOCATION=' "$deploy/.env" >"$deploy/.env.new" || true
mv -f "$deploy/.env.new" "$deploy/.env"
XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user restart ghost-wispr.service
EOF
chmod +x "$tree/deploy/pi/rollout.sh" "$tree/deploy/pi/update.sh" "$tree/deploy/pi/provision.sh"
git -C "$tree" init -q
git -C "$tree" add -A
git -C "$tree" -c user.name=test -c user.email=test@example.com commit -q -m merge
merge=$(git -C "$tree" rev-parse HEAD)
readonly merge

# The fakes. ssh runs the command under sh, in the fake Pi's home, with no input after -n.
real_curl=$(command -v curl)
cat >"$bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
no_input=false
while [[ $# -gt 0 ]]; do
  case $1 in
    -n) no_input=true; shift ;;
    -o) shift 2 ;;
    -*) echo "fake ssh: unknown option $1" >&2; exit 255 ;;
    *) break ;;
  esac
done
[[ $1 == "$FAKE_PI_HOST" ]] || { echo "fake ssh: unexpected host $1" >&2; exit 255; }
shift
cd "$FAKE_PI/home"
if $no_input; then
  exec env HOME="$FAKE_PI/home" sh -c "$*" </dev/null
fi
exec env HOME="$FAKE_PI/home" sh -c "$*"
EOF
cat >"$bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state=$FAKE_PI/state
[[ $1 == --user && -n ${XDG_RUNTIME_DIR:-} ]] || { echo "fake systemctl: not the user manager: $*" >&2; exit 1; }
shift
printf 'systemctl %s\n' "$*" >>"$state/events"
case "$*" in
  'stop ghost-wispr-update.timer') echo inactive >"$state/timer" ;;
  'start ghost-wispr-update.timer') echo active >"$state/timer" ;;
  'is-active ghost-wispr-update.timer') cat "$state/timer"; [[ $(<"$state/timer") == active ]] ;;
  'is-active --quiet ghost-wispr.service') [[ $(<"$state/service") == active ]] ;;
  'show -p ActiveState --value ghost-wispr-update.service')
    if [[ -s $state/update-states ]]; then
      sed -n 1p "$state/update-states"
      sed -i 1d "$state/update-states"
    else
      echo inactive
    fi
    ;;
  'restart ghost-wispr.service')
    echo active >"$state/service"
    printf '{"commit":"%s"}\n' "$(sed -n 's/^commit //p' "$FAKE_DEPLOY/ghost-wispr")" >"$state/version.json"
    ;;
  *) echo "fake systemctl: unexpected: $*" >&2; exit 1 ;;
esac
EOF
cat >"$bin/curl" <<EOF
#!/usr/bin/env bash
# The recorder's answers on 127.0.0.1:8080 come from the fake Pi's state; any other URL goes to curl.
url=\${*: -1}
case \$url in
  http://127.0.0.1:8080/api/status) f=status.json ;;
  http://127.0.0.1:8080/api/version) f=version.json ;;
  http://127.0.0.1:8080/api/config) f=config.json ;;
  'http://127.0.0.1:8080/api/providers?refresh=1') f=providers.json ;;
  *) exec $(printf %q "$real_curl") "\$@" ;;
esac
[[ -f \$FAKE_PI/state/\$f ]] || { echo 'curl: (7) Failed to connect to 127.0.0.1 port 8080' >&2; exit 7; }
printf 'curl %s\n' "\$f" >>"\$FAKE_PI/state/events"
cat "\$FAKE_PI/state/\$f"
EOF
real_sha256sum=$(command -v sha256sum)
cat >"$bin/sha256sum" <<EOF
#!/usr/bin/env bash
# sha256sum, which notes each check of the protected recordings.
[[ " \$* " != *' --check '* ]] || printf 'sha256sum --check\n' >>"\$FAKE_PI/state/events"
exec $(printf %q "$real_sha256sum") "\$@"
EOF
# shellcheck disable=SC2016 # $FAKE_PI and $* expand when the fakes run.
printf '#!/usr/bin/env bash\n[[ -e $FAKE_PI/state/probe ]]\n' >"$bin/pgrep"
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nprintf "sleep %%s\\n" "$*" >>"$FAKE_PI/state/events"\n' >"$bin/sleep"
chmod +x "$bin"/*

# The protected recordings, and deployment settings that name them.
printf 'first meeting\n' >"$pi/home/meeting-a.wav"
printf 'second meeting\n' >"$pi/home/meeting-b.wav"
protected=$(cd "$pi/home" && sha256sum "$PWD/meeting-a.wav" "$PWD/meeting-b.wav")
env_file=$work/pi.env
{
  cat "$repo_root/deploy/pi/test-pi.env"
  printf "PI_PROTECTED_RECORDINGS='%s'\n" "$protected"
} >"$env_file"
# shellcheck source=test-pi.env
source "$repo_root/deploy/pi/test-pi.env"

# The recorder's good answers after the deploy: the template's models and providers, all signed in.
eval "$(awk '/^flatten_yaml\(\) \{$/ { found = 1 } found { print } found && /^\}$/ { exit }' "$repo_root/deploy/pi/rollout.sh")"
plan=$(flatten_yaml <"$tree/deploy/pi/files/ghost-wispr.yaml")
setting() { awk -v prefix="$1: " 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1); exit }' <<<"$plan"; }
good_config=$(jq -cn --arg lt "$(setting transcription.model)" --arg bt "$(setting batch_transcription.model)" \
  --arg s "$(setting summarization.model)" --arg e "$(setting embedding.model)" --arg c "$(setting concierge.model)" \
  --arg t "$(setting tts.model)" --arg tv "$(setting tts.voice)" --arg g "$(setting voice.gptlive.model)" \
  --arg d "$(setting voice.dgagent.provider)" \
  '{models: {live_transcription: $lt, batch_transcription: $bt, summarization: $s, embedding: $e, concierge: $c,
    tts: $t, tts_voice: $tv, gptlive: $g, dgagent_provider: $d}}')
good_providers=$(sed -nE 's/^providers\.([^.:]+):$/\1/p' <<<"$plan" \
  | jq -Rcn '{providers: [inputs | {name: ., ok: true, message: "works"}]}')
[[ $(jq '.providers | length' <<<"$good_providers") -ge 4 ]] || fail "read too few providers from the template: $good_providers"
readonly idle_status='{"paused":true,"capture_state":"paused","active_session_id":"","warnings":[]}'

# reset puts the fake Pi before the deploy: the old binary, config, .env and update.sh, the nightly
# update on, the recorder paused and idle, and the latest release the merge commit's.
reset() {
  rm -rf -- "${deploy:?}"/* "${deploy:?}"/.env "$pi/home/.local" "${state:?}"/*
  printf 'commit %s\n' "$old" >"$deploy/ghost-wispr"
  printf 'old: config\ngdrive_folder_id: "%s"\n' "$drive" >"$deploy/ghost-wispr.yaml"
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 GHOST_WISPR_EMBEDDING_LOCATION=global >"$deploy/.env"
  printf '#!/bin/sh\necho old update.sh\n' >"$deploy/update.sh"
  chmod 0600 "$deploy/.env"
  chmod 0755 "$deploy/update.sh"
  echo active >"$state/timer"
  echo active >"$state/service"
  printf '%s\n' "$idle_status" >"$state/status.json"
  printf '{"commit":"%s"}\n' "$old" >"$state/version.json"
  printf '%s\n' "$merge" >"$state/release"
  printf '%s\n' "$good_config" >"$state/config.json"
  printf '%s\n' "$good_providers" >"$state/providers.json"
  : >"$state/events"
}

# rollout STEP: runs the rollout's STEP from the checkout, against the fake Pi; its output is in
# $out and its exit status in $rc.
rollout() {
  rc=0
  out=$(PATH="$bin:$PATH" FAKE_PI="$pi" FAKE_DEPLOY="$deploy" FAKE_PI_HOST="ghost-wispr@$PI_HOST" \
    GHOST_WISPR_PI_ENV="$env_file" GHOST_WISPR_DEPLOY_DIR="$deploy" \
    GHOST_WISPR_RELEASE_API="file://$work/release.json" bash "$tree/deploy/pi/rollout.sh" "$1" 2>&1) || rc=$?
}
release() { printf '{"target_commitish":"%s"}\n' "$1" >"$work/release.json"; }
expect_ok() { ((rc == 0)) || fail "$1 failed ($rc): $out"; }
expect_stop() {
  ((rc != 0)) || fail "$1 did not stop: $out"
  [[ $out == *"$2"* ]] || fail "$1 stopped, but not because $2: $out"
}
timer() { cat "$state/timer"; }
# line PATTERN: the number of the first events line PATTERN matches, else 0.
line() { grep -nxF -- "$1" "$state/events" | sed -n '1s/:.*//p' | grep . || echo 0; }

release "$merge"

# hold: refused unless the Pi is paused and idle, and before any change.
reset
printf '%s\n' "${idle_status/\"active_session_id\":\"\"/\"active_session_id\":\"s1\"}" >"$state/status.json"
rollout hold
expect_stop 'hold during a session' 'not paused and idle'
[[ $(timer) == active ]] || fail 'hold during a session stopped the nightly update'
reset
: >"$state/probe"
rollout hold
expect_stop 'hold while a GPT-Live probe runs' 'not paused and idle'
rm -f -- "$state/probe"

# hold: a protected recording that changed, or a list that names none, stops it before any change.
reset
printf 'edited\n' >>"$pi/home/meeting-b.wav"
rollout hold
expect_stop 'hold with a changed protected recording' 'a protected recording is missing or changed (before)'
[[ $out == *'meeting-b.wav: FAILED'* ]] || fail "the changed recording is not named: $out"
[[ $(timer) == active ]] || fail 'hold stopped the nightly update after a protected recording changed'
printf 'second meeting\n' >"$pi/home/meeting-b.wav"
saved_env=$(<"$env_file")
printf '%s\n' "$(<"$repo_root/deploy/pi/test-pi.env")" "PI_PROTECTED_RECORDINGS=''" >"$env_file"
rollout hold
expect_stop 'hold with no protected recording' 'names no recording'
printf '%s\n' "$saved_env" >"$env_file"

# hold: stops the timer and waits until an update the timer started has ended.
reset
printf '%s\n' activating activating >"$state/update-states"
rollout hold
expect_ok hold
[[ $(timer) == inactive ]] || fail 'hold left the nightly update timer on'
[[ $(grep -c '^systemctl show -p ActiveState --value ghost-wispr-update.service$' "$state/events") == 3 ]] \
  || fail "hold did not wait for the running update to end: $(<"$state/events")"
[[ $out == *'the 2 protected recordings have their sha256 (before)'* && $out == *'(after)'* ]] \
  || fail "hold did not check the protected recordings before and after: $out"

# install: refused while the latest release is another commit's, before any change.
reset
release "$old"
rollout install
expect_stop 'install before the release' "not the merge commit $merge"
[[ ! -s $state/events && $(timer) == active ]] || fail "install changed the Pi before its release was published: $(<"$state/events")"
release "$merge"

# install: refused from a checkout whose deploy/pi differs from its commit.
reset
printf '# edited\n' >>"$tree/deploy/pi/update.sh"
rollout install
expect_stop 'install from an edited checkout' 'deploy/pi differs from commit'
git -C "$tree" checkout -q -- deploy/pi/update.sh

# install: keeps the old files, stops the update first, installs this update.sh, the release and
# the config, in that order, and leaves the nightly update stopped.
reset
rollout install
expect_ok install
kept=$pi/home/.local/share/ghost-wispr-rollback/$merge
for f in ghost-wispr ghost-wispr.yaml .env update.sh; do
  [[ -f $kept/$f ]] || fail "install kept no $f"
done
grep -qx "commit $old" "$kept/ghost-wispr" || fail 'the kept binary is not the old one'
grep -qx 'old: config' "$kept/ghost-wispr.yaml" || fail 'the kept config is not the old one'
grep -qx 'GHOST_WISPR_EMBEDDING_LOCATION=global' "$kept/.env" || fail 'the kept .env is not the old one'
grep -qx 'echo old update.sh' "$kept/update.sh" || fail 'the kept update.sh is not the old one'
[[ $(<"$kept/commit") == "$old" ]] || fail 'the kept commit is not the one the Pi ran'
[[ $(stat -c %a "$kept") == 700 && $(stat -c %a "$kept/.env") == 600 ]] || fail 'the kept copy is readable by others'
cmp -s "$deploy/update.sh" "$tree/deploy/pi/update.sh" || fail "install did not install the merge commit's update.sh"
grep -qx "commit $merge" "$deploy/ghost-wispr" || fail 'install did not install the release'
grep -qx "gdrive_folder_id: \"$drive\"" "$deploy/ghost-wispr.yaml" || fail 'install did not keep the shared drive'
grep -q 'embedding:' "$deploy/ghost-wispr.yaml" || fail 'install did not install the template'
stop=$(line 'systemctl stop ghost-wispr-update.timer') update=$(line update.sh) models=$(line "provision --models $drive")
((stop > 0 && update > stop && models > update)) \
  || fail "install did not stop the update, then run update.sh, then the models step: $(<"$state/events")"
[[ $(timer) == inactive ]] || fail 'install started the nightly update again'
[[ -z $(find "$pi/home/.local/share/ghost-wispr-provision" -mindepth 1) ]] || fail 'install left its sources on the Pi'

# install again: the copy is the first run's, with the old binary, not the release's.
rollout install
expect_ok 'a second install'
[[ $out == *'from an earlier run, as it was'* ]] || fail "a second install did not keep the first copy: $out"
grep -qx "commit $old" "$kept/ghost-wispr" || fail 'a second install replaced the kept binary with the release'

# install: an update.sh that does not install the merge commit stops it before the config.
reset
printf '%s\n' "$old" >"$state/release"
rollout install
expect_stop 'install when update.sh installs another commit' "did not install the merge commit $merge"
(($(line "provision --models $drive") == 0)) || fail 'install ran the models step after update.sh installed another commit'

# check: each failure leaves the nightly update stopped.
check_fails() {
  local name=$1 because=$2
  rollout check
  expect_stop "check $name" "$because"
  [[ $(timer) == inactive ]] || fail "check $name started the nightly update"
}
deployed() {
  reset
  rollout install
  expect_ok install
}
deployed
printf '{"commit":"%s"}\n' "$old" >"$state/version.json"
check_fails 'on the old binary' "the Pi runs commit $old, not the merge commit"
deployed
jq -c '.warnings = ["Search cannot index sessions"]' <<<"$idle_status" >"$state/status.json"
check_fails 'with a warning' '/api/status warns'
deployed
jq -c '.models.embedding = "vertex/other-embedding"' <<<"$good_config" >"$state/config.json"
check_fails 'with another embedding model' 'the Pi runs embedding vertex/other-embedding'
deployed
jq -c '.providers[0].ok = false' <<<"$good_providers" >"$state/providers.json"
check_fails 'with a provider that did not sign in' 'a provider did not sign in'
deployed
jq -c '.providers += [{name: "gateway", ok: true, message: "works"}]' <<<"$good_providers" >"$state/providers.json"
check_fails 'with a provider the template does not name' 'but the template names'
deployed
printf 'edited\n' >>"$pi/home/meeting-a.wav"
check_fails 'with a changed protected recording' 'a protected recording is missing or changed (before)'
printf 'first meeting\n' >"$pi/home/meeting-a.wav"

# check: when every check holds, it starts the nightly update again, last.
deployed
: >"$state/events"
rollout check
expect_ok check
[[ $(timer) == active ]] || fail 'check did not start the nightly update again'
start=$(line 'systemctl start ghost-wispr-update.timer')
last_check=$(grep -nE '^(curl |sha256sum --check$)' "$state/events" | sed -n '$s/:.*//p')
(($(grep -c '^sha256sum --check$' "$state/events") == 2 && start > last_check)) \
  || fail "check did not start the nightly update after its last check: $(<"$state/events")"

# rollback: stops the nightly update, puts back the kept files by rename, and waits for the old
# commit; the nightly update stays stopped.
rollout rollback
expect_ok rollback
[[ $(timer) == inactive ]] || fail 'rollback left the nightly update on'
grep -qx "commit $old" "$deploy/ghost-wispr" || fail 'rollback did not put back the old binary'
grep -qx 'old: config' "$deploy/ghost-wispr.yaml" || fail 'rollback did not put back the old config'
grep -qx 'GHOST_WISPR_EMBEDDING_LOCATION=global' "$deploy/.env" || fail 'rollback did not put back the old .env'
grep -qx 'echo old update.sh' "$deploy/update.sh" || fail 'rollback did not put back the old update.sh'
[[ $(stat -c %a "$deploy/.env") == 600 ]] || fail 'rollback put back .env readable by others'
[[ $(jq -r .commit "$state/version.json") == "$old" ]] || fail 'rollback did not restart the recorder'
[[ $out == *"the Pi runs commit $old again"* ]] || fail "rollback did not wait for the old commit: $out"

# rollback: runs when the recorder does not run, and does not answer.
deployed
echo failed >"$state/service"
rm -f -- "$state/status.json"
rollout rollback
expect_ok 'rollback with the recorder down'
grep -qx "commit $old" "$deploy/ghost-wispr" || fail 'rollback with the recorder down did not put back the old binary'
# The control: a recorder that runs and does not read paused and idle refuses it.
deployed
printf '%s\n' "${idle_status/\"paused\":true/\"paused\":false}" >"$state/status.json"
rollout rollback
expect_stop 'rollback while the recorder records' 'not paused and idle'
grep -qx "commit $merge" "$deploy/ghost-wispr" || fail 'a refused rollback changed the binary'

printf 'rollout test passed\n'
