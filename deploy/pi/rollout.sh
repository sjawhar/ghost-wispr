#!/usr/bin/env bash
# Roll out a release that replaces config keys on the office Pi, from the operator's machine: the
# merge commit's binary and config together, with the nightly update held off (docs/pi-setup.md,
# "Roll out a release that replaces config keys").
#
#   deploy/pi/rollout.sh hold       before the merge: turn the nightly update off, also across a
#                                   reboot
#   deploy/pi/rollout.sh install    from a clean checkout of the merge commit, once its release is
#                                   the latest: keep the Pi's binary, config, .env and update.sh,
#                                   then install the merge commit's update.sh, run it, and install
#                                   the merge commit's config (provision.sh --models)
#   deploy/pi/rollout.sh check      check the result; turn the nightly update on again only if it
#                                   holds
#   deploy/pi/rollout.sh rollback   put back the kept binary, config, .env and update.sh; the nightly
#                                   update stays off, and it prints the command that turns it on
#
# Every step works on the Pi as ghost-wispr over ssh, with ghost-wispr's user systemd. It refuses
# to start unless the Pi reads paused, with no session and no GPT-Live probe, and checks that again
# before each restart. It checks the sha256 of each protected recording (PI_PROTECTED_RECORDINGS in
# deploy/pi/files/pi.env) before it changes anything and again at its end. It stops at the first
# check that fails. install, check and rollback take the merge commit from their checkout's HEAD.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "$script_dir/../.." && pwd)"
readonly repo_root
# The office Pi's deployment settings (PI_HOST, PI_PROTECTED_RECORDINGS); GHOST_WISPR_PI_ENV names
# another file, as the tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || { printf 'rollout: cannot read the Pi deployment settings in %s\n' "$pi_env" >&2; exit 1; }
readonly PI="ghost-wispr@$PI_HOST"
# GHOST_WISPR_DEPLOY_DIR and GHOST_WISPR_RELEASE_API exist for rollout_test.sh, as update.sh's do
# for update_test.sh.
readonly DEPLOY_DIR="${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}"
readonly RELEASE_API="${GHOST_WISPR_RELEASE_API:-https://api.github.com/repos/sjawhar/ghost-wispr/releases/latest}"
# The copy install keeps for rollback, under ghost-wispr's home, one directory a merge commit.
readonly KEPT_ROOT='.local/share/ghost-wispr-rollback'
readonly KEPT_FILES='ghost-wispr ghost-wispr.yaml .env update.sh commit'
# Each remote command that runs systemctl --user starts with this: ghost-wispr's user manager.
# shellcheck disable=SC2016 # $(id -u) expands on the Pi.
readonly USER_SYSTEMD='export XDG_RUNTIME_DIR=/run/user/$(id -u); '
readonly STATUS_URL=http://127.0.0.1:8080/api/status
readonly VERSION_URL=http://127.0.0.1:8080/api/version

log() { printf 'rollout: %s\n' "$*" >&2; }
fail() {
  log "STOPPED: $*"
  exit 1
}
usage() { sed -n '6,15p' "$0" | sed 's/^# \{0,1\}//'; }

# pi runs a command on the Pi with no input; pi_in gives it this script's input. Each gives up on a
# Pi it cannot reach in 10 s, or that stops answering for a minute (ssh exits 255).
readonly SSH_OPTIONS=(-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=4)
pi() { ssh -n "${SSH_OPTIONS[@]}" "$PI" "$@"; }
pi_in() { ssh "${SSH_OPTIONS[@]}" "$PI" "$@"; }
# The Pi's monotonic clock, which no NTP correction steps (stop_updates); a test points it at a fake.
readonly PI_UPTIME="${GHOST_WISPR_PI_UPTIME:-/proc/uptime}"

# The Pi is idle: paused, with no session, and no GPT-Live probe running (docs/pi-setup.md, "Probe
# voice B (GPT-Live)").
readonly IDLE_TEST="curl -fsS --max-time 10 $STATUS_URL | jq -e '.paused == true and .capture_state == \"paused\" and (.active_session_id // \"\") == \"\"' >/dev/null && ! pgrep -x gptlive-probe >/dev/null"

require_idle() {
  pi "$IDLE_TEST" \
    || fail "the Pi is not paused and idle (paused, no session, no GPT-Live probe); pause it outside a meeting and run this step again"
}

# require_idle_or_down is require_idle, or the recorder not running at all, which records nothing:
# a rollback must also run when the new binary does not start. A recorder that runs but whose
# /api/status cannot be read may be recording, and is refused.
require_idle_or_down() {
  pi "${USER_SYSTEMD}if systemctl --user is-active --quiet ghost-wispr.service; then $IDLE_TEST; else ! pgrep -x gptlive-probe >/dev/null; fi" \
    || fail "the recorder runs and is not paused and idle (paused, no session, no GPT-Live probe), or its /api/status cannot be read, so it may be recording: nothing was changed; pause it outside a meeting, or wait until it answers, and run this step again"
}

# The .env variables that would override the template's Envoy settings, envoy_enabled, envoy_hub_url
# and concierge.envoy_url: each variable internal/config's applyEnvOverrides reads for an Envoy
# setting. A .env line overrides the YAML, and the models step keeps .env, so such a line would
# change what the template says. deploy/pi/provision.sh --models holds the same check.
readonly ENVOY_OVERRIDE_VARIABLES='GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_HUB_URL|GHOST_WISPR_CONCIERGE_ENVOY_URL'

# require_no_envoy_override fails, naming each variable, while a line of the Pi's .env could set one
# of ENVOY_OVERRIDE_VARIABLES: a line that, past leading space and an export, starts with its name
# and =. A line ends at a carriage return as at a line feed, as systemd's EnvironmentFile= parser
# reads one (NEWLINE "\n\r", src/basic/env-file.c), so tr turns each CR into a line feed first. A
# match inside a multi-line value fails too, which only stops the install. grep -o reads back from
# the Pi each match up to its =, so no value leaves the Pi, and .env is not changed.
require_no_envoy_override() {
  local found names status=0
  found=$(pi "f=$(printf %q "$DEPLOY_DIR/.env"); [ -r \"\$f\" ] || exit 2; tr '\r' '\n' <\"\$f\" | grep -oE '^[[:space:]]*(export[[:space:]]+)?($ENVOY_OVERRIDE_VARIABLES)[[:space:]]*='") \
    || status=$?
  ((status <= 1)) || fail "cannot read $DEPLOY_DIR/.env on the Pi (exit $status): nothing was changed"
  [[ -n $found ]] || return 0
  names=$(grep -oE "$ENVOY_OVERRIDE_VARIABLES" <<<"$found" | sort -u | paste -sd ' ' -)
  fail "the Pi's .env can set $names, which would override the template's envoy_enabled, envoy_hub_url or concierge.envoy_url: nothing was changed; remove each such line from $DEPLOY_DIR/.env and run install again"
}

# require_unit_files WHAT: after a reload of the user units, ghost-wispr.service and
# ghost-wispr-update.service are each loaded on the Pi and have no drop-in, which could change how
# they run, such as a time limit that kills update.sh mid-swap. Otherwise it fails, naming the unit
# and its drop-ins, and WHAT (what the step has changed).
require_unit_files() {
  local unit out load paths
  pi "${USER_SYSTEMD}systemctl --user daemon-reload" || fail "could not reload the user units on the Pi; $1"
  for unit in ghost-wispr.service ghost-wispr-update.service; do
    out=$(pi "${USER_SYSTEMD}systemctl --user show -p LoadState -p DropInPaths $unit") \
      || fail "cannot read whether $unit is loaded on the Pi; $1"
    load=$(sed -n 's/^LoadState=//p' <<<"$out")
    paths=$(sed -n 's/^DropInPaths=//p' <<<"$out")
    [[ $load == loaded ]] || fail "$unit is ${load:-of no load state} on the Pi, not loaded; $1"
    [[ -z $paths ]] \
      || fail "$unit has a drop-in on the Pi ($paths), which can change how it runs, such as its time limits; remove it and run this step again; $1"
  done
}

# check_protected WHEN: each protected recording on the Pi has the sha256 PI_PROTECTED_RECORDINGS
# gives it. A list that names none has checked nothing, and fails.
check_protected() {
  local when=$1 out named
  named=$(grep -c . <<<"${PI_PROTECTED_RECORDINGS:-}" || true)
  ((named > 0)) || fail "PI_PROTECTED_RECORDINGS in $pi_env names no recording, so there is nothing to compare"
  out=$(pi_in 'sha256sum --check --strict' <<<"$PI_PROTECTED_RECORDINGS" 2>&1) \
    || fail "a protected recording is missing or changed ($when): $out"
  [[ $(grep -c ': OK$' <<<"$out" || true) == "$named" ]] \
    || fail "sha256sum did not confirm all $named protected recordings ($when): $out"
  log "the $named protected recordings have their sha256 ($when)"
}

# The command that turns the nightly update on again, as the operator runs it.
# shellcheck disable=SC2016 # $(id -u) expands on the Pi.
readonly UPDATES_ON_COMMAND="ssh $PI 'XDG_RUNTIME_DIR=/run/user/\$(id -u) systemctl --user enable --now ghost-wispr-update.timer'"

# stop_updates disables the nightly update's timer and stops it, then waits until an update the
# timer already started has ended (ghost-wispr-update.service inactive or failed): stopping the
# timer does not stop it, and it can still swap or roll back the binary. Disabled, not only stopped:
# ghost-wispr's user manager starts an enabled timer at each boot, and Persistent=true then runs a
# missed update at once, which would install the release's binary on the old config after a reboot.
# The update has no time limit (each step of it has one), so this stops polling at a deadline 35
# minutes on by the Pi's monotonic clock (its uptime), more than an update takes at its worst,
# however slow each poll is or however the wall clock steps. Each poll and each of the report's
# two reads has 30 s (and 5 more before SIGKILL), so one that hangs neither blocks the report nor
# moves the deadline: the step ends at most the 5 s sleep after the last poll before it, a last
# poll, and those two reads past it (110 s). Then it stops, naming the update, its PID and since
# when it runs, or unknown for a read that gave nothing. It never kills the update: a kill can land
# mid-swap.
stop_updates() {
  local status=0 out
  out=$(pi "${USER_SYSTEMD}systemctl --user disable --now ghost-wispr-update.timer || exit 2
[ \"\$(systemctl --user is-enabled ghost-wispr-update.timer)\" = disabled ] || exit 2
[ \"\$(systemctl --user is-active ghost-wispr-update.timer)\" = inactive ] || exit 2
uptime_s() { cut -d. -f1 $(printf %q "$PI_UPTIME"); }
show() { timeout -k 5s 30s systemctl --user show -p \"\$1\" --value ghost-wispr-update.service; }
deadline=\$((\$(uptime_s) + 2100))
while :; do
  case \$(show ActiveState) in
    inactive | failed) exit 0 ;;
  esac
  [ \"\$(uptime_s)\" -lt \"\$deadline\" ] || break
  sleep 5
done
pid=\$(show MainPID) || pid=
since=\$(show InactiveExitTimestamp) || since=
echo \"pid \${pid:-unknown}, since \${since:-unknown}\"
exit 3") || status=$?
  case $status in
    0) log 'the nightly update is off: its timer is disabled and inactive, and no update runs' ;;
    2) fail 'ghost-wispr-update.timer did not stop and disable' ;;
    3) fail "ghost-wispr-update.service (${out:-pid unknown, since unknown}) still runs at its 35-minute deadline; it is left running, since a kill can land mid-swap: run this step again once it ends" ;;
    *) fail "lost the Pi while waiting for an update under way (ssh exit $status); whether it runs is unknown, and nothing stopped it: run this step again" ;;
  esac
}

# merge_commit prints the checkout's HEAD, the merge commit, once deploy/pi is as HEAD has it.
merge_commit() {
  local head changed
  head=$(git -C "$repo_root" rev-parse HEAD) || fail "cannot read the commit of $repo_root"
  [[ $head =~ ^[0-9a-f]{40}$ ]] || fail "$repo_root names no commit: $head"
  changed=$(git -C "$repo_root" status --porcelain -- deploy/pi) || fail "cannot read the state of $repo_root/deploy/pi"
  [[ -z $changed ]] || fail "deploy/pi differs from commit $head; run from a clean checkout of the merge commit:
$changed"
  printf '%s' "$head"
}

# kept_dir MERGE prints the copy install keeps for MERGE, as a path on the Pi.
# shellcheck disable=SC2016 # $HOME is ghost-wispr's, on the Pi.
kept_dir() { printf '$HOME/%s/%s' "$KEPT_ROOT" "$1"; }

# keep MERGE copies the Pi's binary, config, .env and update.sh, and the commit it runs, into
# kept_dir MERGE, mode 0700. A copy an earlier run made is kept as it is: by then the binary may be
# the release's.
keep() {
  local out
  out=$(pi "set -eu
deploy=$(printf %q "$DEPLOY_DIR") kept=$(kept_dir "$1")
if [ -d \"\$kept\" ]; then
  for f in $KEPT_FILES; do [ -f \"\$kept/\$f\" ] || { echo \"\$kept has no \$f\" >&2; exit 2; }; done
  echo \"kept \$kept from an earlier run, as it was: commit \$(cat \"\$kept/commit\")\"
  exit 0
fi
umask 077
mkdir -p \"\${kept%/*}\"
tmp=\$(mktemp -d \"\$kept.XXXXXX\")
cp -p \"\$deploy/ghost-wispr\" \"\$deploy/ghost-wispr.yaml\" \"\$deploy/.env\" \"\$deploy/update.sh\" \"\$tmp/\"
curl -fsS --max-time 10 $VERSION_URL | jq -er '.commit | select(test(\"^[0-9a-f]{40}\$\"))' >\"\$tmp/commit\"
mv \"\$tmp\" \"\$kept\"
echo \"kept \$kept: commit \$(cat \"\$kept/commit\")\"" 2>&1) || fail "could not keep the Pi's binary, config, .env and update.sh: $out"
  log "$out"
}

# drive_id prints the shared drive the Pi's config names, which the models step keeps.
drive_id() {
  local line id
  line=$(pi "grep '^gdrive_folder_id:' $(printf %q "$DEPLOY_DIR/ghost-wispr.yaml")") || true
  id=$(sed -nE 's/^gdrive_folder_id: *"?([A-Za-z0-9_-]+)"?$/\1/p' <<<"$line")
  [[ $id =~ ^[A-Za-z0-9_-]+$ ]] \
    || fail "the Pi's config names no shared drive (gdrive_folder_id); run the full provisioner (docs/pi-setup.md, \"Provision\") from the merge commit instead"
  printf '%s' "$id"
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

# Each role /api/config reports under "models", and the template setting that names it.
readonly MODEL_SETTINGS=(
  live_transcription=transcription.model batch_transcription=batch_transcription.model
  summarization=summarization.model embedding=embedding.model concierge=concierge.model
  tts=tts.model tts_voice=tts.voice gptlive=voice.gptlive.model dgagent_provider=voice.dgagent.provider
)

step_hold() {
  require_idle
  check_protected before
  stop_updates
  check_protected after
  log 'held: merge, then run install from a checkout of the merge commit once its release is the latest'
}

step_install() {
  local merge latest drive out remote status=0
  merge=$(merge_commit)
  latest=$(curl -fsS --max-time 30 "$RELEASE_API" | jq -r '.target_commitish // empty') \
    || fail "cannot read the latest release from $RELEASE_API"
  [[ $latest == "$merge" ]] \
    || fail "the latest release is of commit ${latest:-<none>}, not the merge commit $merge; wait until the release workflow publishes it"
  require_idle
  require_unit_files 'nothing was changed'
  require_no_envoy_override
  check_protected before
  stop_updates
  keep "$merge"
  drive=$(drive_id)

  # This release's update.sh, before it runs: only the full provisioner installs it otherwise.
  pi_in "set -e; u=$(printf %q "$DEPLOY_DIR/update.sh"); cat >\"\$u.new\"; chmod 0755 \"\$u.new\"; mv -f \"\$u.new\" \"\$u\"" \
    <"$repo_root/deploy/pi/update.sh" || fail 'could not install the merge commit'\''s update.sh'
  [[ $(pi "sha256sum <$(printf %q "$DEPLOY_DIR/update.sh")") == "$(sha256sum <"$repo_root/deploy/pi/update.sh")" ]] \
    || fail 'the update.sh on the Pi is not the merge commit'\''s'
  log "installed the merge commit's update.sh"
  # Its units too, which only the full provisioner installs otherwise: the update unit gives
  # update.sh no time limit, so nothing kills it mid-swap. A rollback keeps them: the old update.sh
  # runs as well with no limit.
  local unit
  for unit in ghost-wispr-update.service ghost-wispr.service; do
    pi_in "set -e; d=\$HOME/.config/systemd/user; mkdir -p \"\$d\"; cat >\"\$d/$unit.new\"; chmod 0644 \"\$d/$unit.new\"; mv -f \"\$d/$unit.new\" \"\$d/$unit\"" \
      <"$repo_root/deploy/pi/files/$unit" || fail "could not install the merge commit's $unit"
    [[ $(pi "sha256sum <\$HOME/.config/systemd/user/$unit") == "$(sha256sum <"$repo_root/deploy/pi/files/$unit")" ]] \
      || fail "the $unit on the Pi is not the merge commit's"
  done
  require_unit_files "the Pi has the merge commit's update.sh and units, with its old binary and config, and the nightly update off: run install again once the drop-in is gone, or roll back"
  log "installed the merge commit's ghost-wispr-update.service and ghost-wispr.service"

  require_idle
  # Paused-only: update.sh checks again just before the swap and before a rollback's restart, so a
  # resume during the release's download stops it with the binary in place (docs/pi-setup.md,
  # "Update and inspect").
  out=$(pi "${USER_SYSTEMD}GHOST_WISPR_UPDATE_PAUSED_ONLY=1 GHOST_WISPR_DEPLOY_DIR=$(printf %q "$DEPLOY_DIR") $(printf %q "$DEPLOY_DIR/update.sh")" 2>&1) || status=$?
  log "update.sh: $out"
  ((status == 0)) || fail "update.sh failed; the Pi runs the binary it reports above, on its old config"
  grep -Eqx "ghost-wispr update: (installed|already on) commit $merge" <<<"$out" \
    || fail "update.sh did not install the merge commit $merge; the config is unchanged"

  require_idle
  remote=$(pi 'umask 077; mkdir -p ~/.local/share/ghost-wispr-provision; chmod 700 ~/.local/share/ghost-wispr-provision; mktemp -d ~/.local/share/ghost-wispr-provision/sources.XXXXXX') \
    || fail 'cannot make the sources directory on the Pi'
  [[ $remote =~ ^/[A-Za-z0-9._/-]+/sources\.[A-Za-z0-9]+$ ]] || fail "unexpected sources directory on the Pi: $remote"
  tar -C "$repo_root" -cf - deploy/pi/provision.sh deploy/pi/files/ghost-wispr.yaml \
    | pi_in "tar -xf - -C $(printf %q "$remote")" || fail 'cannot copy provision.sh and the config template to the Pi'
  status=0
  pi "GHOST_WISPR_DEPLOY_DIR=$(printf %q "$DEPLOY_DIR") $(printf %q "$remote/deploy/pi/provision.sh") --models $(printf %q "$drive")" || status=$?
  pi "rm -rf -- $(printf %q "$remote")" || log "could not remove $remote on the Pi"
  ((status == 0)) || fail 'provision.sh --models failed; the Pi runs the merge commit'\''s binary on its old config: run install again, or rollback'
  check_protected after
  log "installed the merge commit $merge and its config; next: deploy/pi/rollout.sh check"
}

step_check() {
  local merge got status config providers want_providers names entry role setting want plan
  merge=$(merge_commit)
  require_idle
  check_protected before
  got=$(pi "curl -fsS --max-time 10 $VERSION_URL" | jq -r '.commit // empty') || got=
  [[ $got == "$merge" ]] || fail "the Pi runs commit ${got:-<none>}, not the merge commit $merge"
  status=$(pi "curl -fsS --max-time 10 $STATUS_URL") || fail 'cannot read /api/status'
  jq -e '.warnings == []' >/dev/null <<<"$status" || fail "/api/status warns: $(jq -c '.warnings' <<<"$status")"
  plan=$(flatten_yaml <"$repo_root/deploy/pi/files/ghost-wispr.yaml")
  config=$(pi "curl -fsS --max-time 10 http://127.0.0.1:8080/api/config") || fail 'cannot read /api/config'
  for entry in "${MODEL_SETTINGS[@]}"; do
    role=${entry%%=*} setting=${entry#*=}
    want=$(awk -v prefix="$setting: " 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1); exit }' <<<"$plan")
    [[ -n $want ]] || fail "the template sets no $setting to compare the running $role with"
    got=$(jq -r --arg role "$role" '.models[$role] // ""' <<<"$config")
    [[ $got == "$want" ]] || fail "the Pi runs $role ${got:-<none>}, but the template names $want"
  done
  providers=$(pi "curl -fsS --max-time 180 'http://127.0.0.1:8080/api/providers?refresh=1'") || fail 'cannot read /api/providers'
  log "providers: $(jq -c '[.providers[] | {name, ok, message}]' <<<"$providers")"
  names=$(jq -r '.providers[].name' <<<"$providers" | LC_ALL=C sort | tr '\n' ' ')
  want_providers=$(sed -nE 's/^providers\.([^.:]+):$/\1/p' <<<"$plan" | LC_ALL=C sort | tr '\n' ' ')
  [[ -n $want_providers && $names == "$want_providers" ]] \
    || fail "the Pi lists the providers ${names:-<none>}, but the template names ${want_providers:-<none>}"
  jq -e 'all(.providers[]; .ok == true)' >/dev/null <<<"$providers" \
    || fail "a provider did not sign in: $(jq -c '[.providers[] | select(.ok != true) | {name, message}]' <<<"$providers")"
  check_protected after
  pi "${USER_SYSTEMD}systemctl --user enable --now ghost-wispr-update.timer && [ \"\$(systemctl --user is-enabled ghost-wispr-update.timer)\" = enabled ] && [ \"\$(systemctl --user is-active ghost-wispr-update.timer)\" = active ]" \
    || fail 'the checks passed, but ghost-wispr-update.timer did not enable and start'
  log "the Pi runs the merge commit $merge on its config, every provider signed in, and the nightly update is on again; next: A8 of \"Accept slice 2\""
}

step_rollback() {
  local merge out
  merge=$(merge_commit)
  require_idle_or_down
  check_protected before
  stop_updates
  require_idle_or_down
  out=$(pi "${USER_SYSTEMD}set -eu
deploy=$(printf %q "$DEPLOY_DIR") kept=$(kept_dir "$merge")
for f in $KEPT_FILES; do [ -f \"\$kept/\$f\" ] || { echo \"\$kept has no \$f\" >&2; exit 2; }; done
# A rename, not a copy over the file: the running process executes the binary, and a copy over it
# fails with \"Text file busy\".
for f in ghost-wispr.yaml .env update.sh ghost-wispr; do
  cp -p \"\$kept/\$f\" \"\$deploy/\$f.rollback\"
  mv -f \"\$deploy/\$f.rollback\" \"\$deploy/\$f\"
done
systemctl --user restart ghost-wispr.service
want=\$(cat \"\$kept/commit\")
for _ in \$(seq 60); do
  got=\$(curl -fsS --max-time 5 $VERSION_URL 2>/dev/null | jq -r '.commit // empty' 2>/dev/null || true)
  if [ \"\$got\" = \"\$want\" ]; then echo \"the Pi runs commit \$want again\"; exit 0; fi
  sleep 2
done
echo \"the Pi does not answer with commit \$want after the restart\" >&2
exit 3" 2>&1) || fail "rollback: $out"
  log "$out"
  check_protected after
  log "rolled back the binary, config, .env and update.sh; the nightly update stays off, also across a reboot, or it would install the release again on the old config. Once the cause is fixed (the release reverted, or one that runs on this config published), turn it on again: $UPDATES_ON_COMMAND"
}

case ${1:-} in
  hold | install | check | rollback)
    [[ $# -eq 1 ]] || { usage >&2; fail "$1 takes no argument"; }
    "step_$1"
    ;;
  -h | --help) usage ;;
  *)
    usage >&2
    fail "unknown step: ${1:-<none>}"
    ;;
esac
