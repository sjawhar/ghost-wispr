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
cp -- "$repo_root/deploy/pi/files/ghost-wispr.yaml" "$repo_root/deploy/pi/files/ghost-wispr-update.service" "$repo_root/deploy/pi/files/ghost-wispr.service" "$tree/deploy/pi/files/"
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
    -o) printf '%s\n' "$2" >>"$FAKE_PI/state/ssh-options"; shift 2 ;;
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
  # A timer's state across a reboot is whether it is enabled (reboot, below): stop and start
  # alone are refused, so a step that only stops the timer fails here.
  'disable --now ghost-wispr-update.timer') echo inactive >"$state/timer"; echo disabled >"$state/timer-enabled" ;;
  'enable --now ghost-wispr-update.timer') echo active >"$state/timer"; echo enabled >"$state/timer-enabled" ;;
  'is-active ghost-wispr-update.timer') cat "$state/timer"; [[ $(<"$state/timer") == active ]] ;;
  'is-enabled ghost-wispr-update.timer') cat "$state/timer-enabled"; [[ $(<"$state/timer-enabled") == enabled ]] ;;
  'is-active --quiet ghost-wispr.service') [[ $(<"$state/service") == active ]] ;;
  'show -p ActiveState --value ghost-wispr-update.service')
    # A poll takes state/poll-seconds of the fake Pi's clock, as a slow systemctl does; with
    # state/poll-hang it never returns, and only a timeout (the fake timeout's) ends it.
    [[ ! -s $state/poll-seconds ]] || fake-clock-advance "$(<"$state/poll-seconds")"
    if [[ -e $state/poll-hang ]]; then
      if [[ -z ${FAKE_TIMEOUT_SECONDS:-} ]]; then
        echo 'a poll ran with no timeout' >>"$state/events"
        exit 1
      fi
      fake-clock-advance "$FAKE_TIMEOUT_SECONDS"
      exit 124
    fi
    if [[ -e $state/update-forever ]]; then
      echo activating
    elif [[ -s $state/update-states ]]; then
      sed -n 1p "$state/update-states"
      sed -i 1d "$state/update-states"
    else
      echo inactive
    fi
    ;;
  'show -p MainPID --value ghost-wispr-update.service') echo 4321 ;;
  'show -p InactiveExitTimestamp --value ghost-wispr-update.service') echo 'Sun 2026-11-22 03:30:00 UTC' ;;
  'daemon-reload') ;;
  # LoadState from state/load-UNIT (loaded by default); DropInPaths from state/dropins-UNIT, and
  # from the second query on also state/dropins-later-UNIT (a drop-in that appears mid-install).
  'show -p LoadState -p DropInPaths ghost-wispr.service' | 'show -p LoadState -p DropInPaths ghost-wispr-update.service')
    unit=${*: -1}
    queries=$(($(cat "$state/queries-$unit" 2>/dev/null || echo 0) + 1))
    echo "$queries" >"$state/queries-$unit"
    paths=$(cat "$state/dropins-$unit" 2>/dev/null || true)
    ((queries < 2)) || paths="$paths$(cat "$state/dropins-later-$unit" 2>/dev/null || true)"
    printf 'LoadState=%s\nDropInPaths=%s\n' "$(cat "$state/load-$unit" 2>/dev/null || echo loaded)" "$paths"
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
# The fake Pi's clocks. state/uptime is its monotonic clock, as /proc/uptime reads ("SECONDS.00
# IDLE"): sleep, slow and timed-out polls move it on (fake-clock-advance). date +%s is its wall
# clock: the uptime plus 1700000000, stepped by state/wall-step (STEP seconds once the uptime
# reaches AT: "AT STEP"), as an NTP correction steps it; else the real date.
cat >"$bin/fake-clock-advance" <<'EOF'
#!/usr/bin/env bash
[[ -s $FAKE_PI/state/uptime ]] || exit 0
read -r up _ <"$FAKE_PI/state/uptime"
printf '%s.00 0.00\n' $((${up%%.*} + $1)) >"$FAKE_PI/state/uptime"
EOF
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nprintf "sleep %%s\\n" "$*" >>"$FAKE_PI/state/events"\nfake-clock-advance "$1"\n' >"$bin/sleep"
real_date=$(command -v date)
cat >"$bin/date" <<EOF
#!/usr/bin/env bash
if [[ \$* == +%s && -s \$FAKE_PI/state/uptime ]]; then
  read -r up _ <"\$FAKE_PI/state/uptime"
  wall=\$((1700000000 + \${up%%.*}))
  if [[ -s \$FAKE_PI/state/wall-step ]]; then
    read -r at step <"\$FAKE_PI/state/wall-step"
    ((\${up%%.*} < at)) || wall=\$((wall + step))
  fi
  echo "\$wall"
else
  exec $(printf %q "$real_date") "\$@"
fi
EOF
# timeout [-k KILL] DURATION COMMAND: runs COMMAND, telling a fake that hangs how long it may take
# (FAKE_TIMEOUT_SECONDS) before it is killed.
cat >"$bin/timeout" <<'EOF'
#!/usr/bin/env bash
[[ $1 != -k ]] || shift 2
FAKE_TIMEOUT_SECONDS=${1%s} exec "${@:2}"
EOF
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
  echo enabled >"$state/timer-enabled"
  echo active >"$state/service"
  printf '%s\n' "$idle_status" >"$state/status.json"
  printf '{"commit":"%s"}\n' "$old" >"$state/version.json"
  printf '%s\n' "$merge" >"$state/release"
  printf '%s\n' "$good_config" >"$state/config.json"
  printf '%s\n' "$good_providers" >"$state/providers.json"
  echo '1000.00 0.00' >"$state/uptime"
  : >"$state/events"
}

# rollout STEP: runs the rollout's STEP from the checkout, against the fake Pi; its output is in
# $out and its exit status in $rc.
rollout() {
  rc=0
  out=$(PATH="$bin:$PATH" FAKE_PI="$pi" FAKE_DEPLOY="$deploy" FAKE_PI_HOST="ghost-wispr@$PI_HOST" \
    GHOST_WISPR_PI_ENV="$env_file" GHOST_WISPR_DEPLOY_DIR="$deploy" GHOST_WISPR_PI_UPTIME="$state/uptime" \
    GHOST_WISPR_RELEASE_API="file://$work/release.json" bash "$tree/deploy/pi/rollout.sh" "$1" 2>&1) || rc=$?
}
release() { printf '{"target_commitish":"%s"}\n' "$1" >"$work/release.json"; }
expect_ok() { ((rc == 0)) || fail "$1 failed ($rc): $out"; }
expect_stop() {
  ((rc != 0)) || fail "$1 did not stop: $out"
  [[ $out == *"$2"* ]] || fail "$1 stopped, but not because $2: $out"
}
timer() { cat "$state/timer"; }
enabled() { cat "$state/timer-enabled"; }
# reboot models the Pi's reboot: ghost-wispr's user manager starts each enabled timer, and
# Persistent=true runs a missed update at once; a disabled timer stays off.
reboot() {
  if [[ $(enabled) == enabled ]]; then echo active >"$state/timer"; else echo inactive >"$state/timer"; fi
}
# off_after_reboot WHEN: the nightly update is disabled, so a reboot leaves it off.
off_after_reboot() {
  [[ $(enabled) == disabled ]] || fail "$1 left ghost-wispr-update.timer $(enabled): a reboot starts it again"
  reboot
  [[ $(timer) == inactive ]] || fail "after $1, a reboot started the nightly update"
}
# line PATTERN: the number of the first events line PATTERN matches, else 0.
line() { grep -nxF -- "$1" "$state/events" | sed -n '1s/:.*//p' | grep . || echo 0; }

release "$merge"

# hold: refused unless the Pi is paused and idle, and before any change.
reset
printf '%s\n' "${idle_status/\"active_session_id\":\"\"/\"active_session_id\":\"s1\"}" >"$state/status.json"
rollout hold
expect_stop 'hold during a session' 'not paused and idle'
[[ $(timer) == active && $(enabled) == enabled ]] || fail 'hold during a session stopped the nightly update'
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
off_after_reboot hold
[[ $(grep -c '^systemctl show -p ActiveState --value ghost-wispr-update.service$' "$state/events") == 3 ]] \
  || fail "hold did not wait for the running update to end: $(<"$state/events")"
[[ $out == *'the 2 protected recordings have their sha256 (before)'* && $out == *'(after)'* ]] \
  || fail "hold did not check the protected recordings before and after: $out"

# hold: an update that still runs 35 minutes on, by the Pi's monotonic clock (its uptime), is left
# running, named with its PID and since when it runs; nothing kills or stops it. The 35 minutes are
# of the uptime, not a count of polls nor the wall clock. GPT-6.1 Sol's cases: a slow systemctl
# (each poll 25 s, at 93a3b1e1) must not stretch them; a wall clock stepped forward 2 hours or back
# 1 hour (at a9e966db) must not cut them short or stretch them; and a poll that never returns (at
# a9e966db) is cut off by its own timeout, so it neither blocks the report nor stretches them.
# CASE: POLL_SECONDS WALL_STEP HANG
for case in '0 - -' '25 - -' '0 7200 -' '0 -3600 -' '0 - hang'; do
  read -r poll step hang <<<"$case"
  reset
  : >"$state/update-forever"
  echo '1000.00 0.00' >"$state/uptime"
  [[ $poll == 0 ]] || echo "$poll" >"$state/poll-seconds"
  [[ $step == - ]] || echo "1005 $step" >"$state/wall-step"
  [[ $hang == - ]] || : >"$state/poll-hang"
  rollout hold
  expect_stop "hold with an update that does not end ($case)" 'ghost-wispr-update.service (pid 4321, since Sun 2026-11-22 03:30:00 UTC) still runs after 35 minutes'
  [[ $out == *'left running'* ]] || fail "hold did not say the update is left running: $out"
  ! grep -Eq 'systemctl (kill|stop|restart) ghost-wispr-update|^kill' "$state/events" \
    || fail "hold killed or stopped the running update: $(<"$state/events")"
  ! grep -q 'a poll ran with no timeout' "$state/events" || fail "hold ($case) polled the update with no timeout"
  read -r up _ <"$state/uptime"
  waited=$((${up%%.*} - 1000))
  slowest=$poll
  [[ $hang == - ]] || slowest=30
  ((waited >= 2100 && waited < 2100 + 5 + slowest + 5)) \
    || fail "hold ($case) gave up after ${waited} s of the Pi's uptime, want 35 minutes"
done
grep -qx 'ConnectTimeout=10' "$state/ssh-options" && grep -qx 'ServerAliveInterval=15' "$state/ssh-options" \
  || fail "hold reached the Pi without a connect timeout and keepalives: $(<"$state/ssh-options")"

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
# The merge commit's units reach the Pi with its update.sh: the update unit gives update.sh no time
# limit, so nothing kills it mid-swap.
for unit in ghost-wispr-update.service ghost-wispr.service; do
  cmp -s "$pi/home/.config/systemd/user/$unit" "$tree/deploy/pi/files/$unit" \
    || fail "install did not install the merge commit's $unit"
done
reload=$(line 'systemctl daemon-reload')
((reload > 0 && reload < $(line update.sh))) || fail "install did not reload the user units before update.sh: $(<"$state/events")"
grep -qx "commit $merge" "$deploy/ghost-wispr" || fail 'install did not install the release'
grep -qx "gdrive_folder_id: \"$drive\"" "$deploy/ghost-wispr.yaml" || fail 'install did not keep the shared drive'
grep -q 'embedding:' "$deploy/ghost-wispr.yaml" || fail 'install did not install the template'
stop=$(line 'systemctl disable --now ghost-wispr-update.timer') update=$(line update.sh) models=$(line "provision --models $drive")
((stop > 0 && update > stop && models > update)) \
  || fail "install did not stop the update, then run update.sh, then the models step: $(<"$state/events")"
[[ $(timer) == inactive ]] || fail 'install started the nightly update again'
off_after_reboot install
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

# install refuses while either unit is not loaded on the Pi, or has a drop-in there, which can change
# any timeout update.sh's bounds rely on (deploy/pi/update_test.sh's contract): it names them, and
# changes nothing. It checks again once it has installed the units, before update.sh runs.
refused_before_change() {
  (($(line 'systemctl disable --now ghost-wispr-update.timer') == 0 && $(line update.sh) == 0)) && [[ $(timer) == active ]] \
    || fail "install changed the Pi before it refused $1: $(<"$state/events")"
}
reset
printf '%s\n' "$pi/home/.config/systemd/user/ghost-wispr.service.d/trace.conf" >"$state/dropins-ghost-wispr.service"
rollout install
expect_stop 'install with a drop-in on the recorder unit' 'ghost-wispr.service.d/trace.conf'
refused_before_change 'a drop-in'
reset
echo not-found >"$state/load-ghost-wispr-update.service"
rollout install
expect_stop 'install with the update unit not loaded' 'ghost-wispr-update.service is not-found'
refused_before_change 'a unit not loaded'
reset
printf '%s\n' /run/user/1000/systemd/user/ghost-wispr-update.service.d/late.conf >"$state/dropins-later-ghost-wispr-update.service"
rollout install
expect_stop 'install with a drop-in found once it installed the units' 'late.conf'
(($(line update.sh) == 0)) || fail "install ran update.sh past a drop-in it found once it installed the units: $(<"$state/events")"

# install refuses, before it changes anything, while a line of the Pi's .env could set a variable
# that would turn Envoy on over the template, which keeps it off while the hub moves
# (dispatch://INFRA-3862): a line that, past leading space and an export, starts with the name and
# =. It names the variable, prints no value, and leaves .env byte for byte as it was. A match inside
# a multi-line quoted value refuses too: a false refusal only stops the install. systemd's
# EnvironmentFile= parser ends a line at a carriage return as at a line feed (NEWLINE "\n\r",
# src/basic/env-file.c), so a bare CR starts a line too; a case whose text holds CR LF is a CRLF file.
for case in \
  'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_ENABLED=true' \
  'GHOST_WISPR_CONCIERGE_ENVOY_URL|  export GHOST_WISPR_CONCIERGE_ENVOY_URL = http://hub.example.test:9020' \
  $'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_DISPATCH_TOKEN="first line\nGHOST_WISPR_ENVOY_ENABLED=true\nlast line"' \
  $'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ADDR=127.0.0.1:8080\rGHOST_WISPR_ENVOY_ENABLED=true' \
  $'GHOST_WISPR_CONCIERGE_ENVOY_URL|GHOST_WISPR_ADDR=127.0.0.1:8080\r\nGHOST_WISPR_CONCIERGE_ENVOY_URL=http://hub.example.test:9020\r'; do
  name=${case%%|*} content=${case#*|} eol=''
  [[ $content != *$'\r\n'* ]] || eol=$'\r'
  reset
  printf "%s$eol\n%s\n" GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 "$content" >"$deploy/.env"
  before=$(sha256sum <"$deploy/.env")
  rollout install
  expect_stop "install with $name in .env" "the Pi's .env can set $name"
  refused_before_change "$name in .env"
  [[ $(sha256sum <"$deploy/.env") == "$before" ]] || fail "install changed .env when it refused $name"
  [[ $out != *'hub.example.test'* && $out != *'first line'* && $out != *dg-1* ]] \
    || fail "install printed a value from .env when it refused $name: $out"
done
# The control: a .env that names neither installs, with the hub's URL, a commented-out override and
# a longer name that starts with one.
reset
printf '%s\n' 'GHOST_WISPR_ENVOY_HUB_URL=http://hub.example.test:9020' '# GHOST_WISPR_ENVOY_ENABLED=true' \
  'GHOST_WISPR_ENVOY_ENABLED_BEFORE=true' >>"$deploy/.env"
rollout install
expect_ok 'install with a .env that sets no Envoy override'

# check: each failure leaves the nightly update stopped.
check_fails() {
  local name=$1 because=$2
  rollout check
  expect_stop "check $name" "$because"
  [[ $(timer) == inactive ]] || fail "check $name started the nightly update"
  off_after_reboot "check $name"
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
[[ $(timer) == active && $(enabled) == enabled ]] || fail 'check did not turn the nightly update on again'
start=$(line 'systemctl enable --now ghost-wispr-update.timer')
last_check=$(grep -nE '^(curl |sha256sum --check$)' "$state/events" | sed -n '$s/:.*//p')
(($(grep -c '^sha256sum --check$' "$state/events") == 2 && start > last_check)) \
  || fail "check did not start the nightly update after its last check: $(<"$state/events")"

# rollback: stops the nightly update, puts back the kept files by rename, and waits for the old
# commit; the nightly update stays stopped.
rollout rollback
expect_ok rollback
[[ $(timer) == inactive ]] || fail 'rollback left the nightly update on'
off_after_reboot rollback
[[ $out == *"systemctl --user enable --now ghost-wispr-update.timer"* ]] \
  || fail "rollback did not print the command that turns the nightly update on again: $out"
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
# A recorder that runs but whose /api/status cannot be read may be recording: rollback refuses it,
# restarting nothing.
deployed
rm -f -- "$state/status.json"
: >"$state/events"
rollout rollback
expect_stop 'rollback while the recorder runs and does not answer' 'cannot be read'
grep -qx "commit $merge" "$deploy/ghost-wispr" || fail 'a rollback of a recorder whose state is unknown changed the binary'
(($(line 'systemctl restart ghost-wispr.service') == 0)) || fail "a rollback restarted a recorder whose state is unknown: $(<"$state/events")"

printf 'rollout test passed\n'
