#!/usr/bin/env bash
# Runs deploy/pi/update.sh, which installs a release's whole deploy set on the office Pi and rolls
# it back as one unit, and deploy/pi/files/ghost-wispr-update-launcher.sh, the fixed launcher that
# runs it each night, on a fake Pi: its deploy directory, its user unit directory and a system root,
# all under one temporary directory. Fakes stand in for curl (the release API, the release's
# assets, and the recorder's /api/status and /api/version), systemctl --user, sudo, tailscale,
# timeout, sleep and date. The release binary is a script that answers its commit once it runs and
# answers pi-secrets from fixture files, recording how it was run. Each release is built here in the
# layout of the plan's contract C2: the binary beside ghost-wispr-pi-deploy.tar.gz, whose
# manifest.json names the SHA-256 of the binary and of every other file.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "$script_dir/../.." && pwd)"
readonly repo_root
work_dir=$(mktemp -d)
readonly work_dir
readonly pi="$work_dir/pi"
readonly opt="$pi/opt" units="$pi/units" root="$pi/root" state="$pi/state" device_key="$pi/key.pem"
readonly releases="$work_dir/releases" bin="$work_dir/bin" timeout_bin="$work_dir/timeout-bin" fakes="$work_dir/fakes"
real_timeout=$(command -v timeout)
readonly real_timeout

# The cases of the spec's check c-update (dispatch://INFRA-5935/spec v5), each of which begin names.
readonly case_names=(whole-set system-files-unchanged env-lines local-settings-untouched override-refused
  drop-in-refused rollback-whole-set partial-install same-commit-different-set transition no-binary-yet
  prev-copy-by-rename busy-session busy-call busy-during-download held-link-versions bad-manifest
  secrets-only-change secrets-read-fails pi-secrets-hangs certificate release-and-certificate secrets-mode
  drift launcher unit-limits)
current_case=setup
ran_cases=' '

fail() {
  printf 'update test FAILED (%s): %s\n' "$current_case" "$*" >&2
  exit 1
}

# begin NAME starts the case NAME of c-update, which each failure then names.
begin() {
  [[ " ${case_names[*]} " == *" $1 "* ]] || fail "begin: $1 is not a case of the check c-update"
  current_case=$1
  if [[ $ran_cases != *" $1 "* ]]; then
    ran_cases+="$1 "
    printf 'update test: %s\n' "$1"
  fi
}

# A release binary that a fake restart left running (busy-exec below).
stop_busy_binary() {
  if [[ -f $work_dir/busy.pid ]]; then
    kill "$(<"$work_dir/busy.pid")" 2>/dev/null || true
    rm -f -- "$work_dir/busy.pid"
  fi
}
cleanup() {
  stop_busy_binary
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

# Every file of C2's tarball has its source in the repository.
for file in deploy/pi/update.sh deploy/pi/pi-lib.sh deploy/pi/provision.sh wait-for-network.sh \
  deploy/pi/files/ghost-wispr.yaml deploy/pi/files/wait-for-time-sync.sh deploy/pi/files/ghost-wispr.service \
  deploy/pi/files/99-ghost-wispr.rules deploy/pi/files/logind-power-key.conf \
  deploy/pi/files/ghost-wispr-update-launcher.sh deploy/pi/files/ghost-wispr-update.service \
  deploy/pi/files/ghost-wispr-update.timer deploy/pi/files/serve.conf; do
  [[ -f $repo_root/$file ]] || fail "$file is missing: the release's deploy set and the launcher need it"
done

commit_a=$(printf 'a%.0s' {1..40})
commit_b=$(printf 'b%.0s' {1..40})
commit_c=$(printf 'c%.0s' {1..40})
# What a release binary answers when it never reports its own commit.
wrong_commit=$(printf '0%.0s' {1..40})
# The commit of the release that the real build-deploy-tarball.sh builds.
commit_d=$(printf 'd%.0s' {1..40})
readonly commit_a commit_b commit_c commit_d wrong_commit
readonly drive_id='0AAbCdEf_Gh-Ij'
readonly template="$repo_root/deploy/pi/files/ghost-wispr.yaml"

# The deploy set (C2): the files update.sh installs, with the binary, and rolls back as one unit.
readonly set_files=(ghost-wispr.yaml pi.env update.sh pi-lib.sh wait-for-network.sh wait-for-time-sync.sh
  ghost-wispr.service 99-ghost-wispr.rules logind-power-key.conf)
# The bootstrap files (D4), which only provision.sh installs, and which update.sh only compares.
readonly bootstrap_files=(ghost-wispr-update-launcher.sh ghost-wispr-update.service ghost-wispr-update.timer)

# installed_path NAME prints where the Pi keeps the file NAME of a release (C2's table).
installed_path() {
  case $1 in
    ghost-wispr.service | ghost-wispr-update.service | ghost-wispr-update.timer) printf '%s\n' "$units/$1" ;;
    99-ghost-wispr.rules) printf '%s\n' "$root/etc/udev/rules.d/99-ghost-wispr.rules" ;;
    logind-power-key.conf) printf '%s\n' "$root/etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf" ;;
    *) printf '%s\n' "$opt/$1" ;;
  esac
}

# file_mode NAME prints the mode the Pi keeps the file NAME in.
file_mode() {
  case $1 in
    update.sh | pi-lib.sh | wait-for-network.sh | wait-for-time-sync.sh | ghost-wispr-update-launcher.sh | ghost-wispr) echo 755 ;;
    ghost-wispr.yaml) echo 640 ;;
    .env) echo 600 ;;
    *) echo 644 ;;
  esac
}

sha() {
  local sum
  sum=$(sha256sum -- "$1") || return 1
  printf '%s\n' "${sum%% *}"
}

mkdir -p -- "$bin" "$timeout_bin" "$fakes" "$releases"

cat >"$fakes/after" <<'EOF'
#!/usr/bin/env bash
# after EVENT puts each $TEST_STATE/NAME.after-EVENT in place of $TEST_STATE/NAME, once: the fake
# Pi's state changes at that event (download, secrets, system-reload or restart). A new status
# program starts again at its first line.
set -euo pipefail
state=${TEST_STATE:?}
for file in "$state"/*.after-"$1"; do
  [[ -e $file ]] || continue
  name=${file%.after-"$1"}
  mv -f -- "$file" "$name"
  if [[ ${name##*/} == status ]]; then
    rm -f -- "$state/status.reads"
  fi
done
EOF

cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
# The fake curl. The release API answers $TEST_RELEASES/$TEST_RELEASE/release.json, and each asset
# URL a file under $TEST_RELEASES; the download of the binary is the event "download". The
# recorder's /api/status answers the status program $TEST_STATE/status, one line a read and its
# last line for every read after: a keyword below or a JSON object. /api/version answers the commit
# that the running image names in its "# answers commit" line. Neither answers while the
# recorder's unit is not active. Each call's --max-time and URL go to $TEST_STATE/curl.log, and each
# status read the .env and certificate it found to $TEST_STATE/status-reads.log. With TEST_SLOW=1
# each call takes all of its --max-time on the test's clock, as a call at its worst; each call to a
# URL that the glob TEST_CURL_FAIL matches fails as curl does on a network fault (exit 6, the host
# could not be resolved).
set -euo pipefail
state=${TEST_STATE:?}
output='' url='' max_time=''
while (($#)); do
  case $1 in
    --output | -o) output=$2; shift 2 ;;
    --max-time | -m) max_time=$2; shift 2 ;;
    --connect-timeout) shift 2 ;;
    --fail | --silent | --show-error | --location) shift ;;
    -*)
      if [[ ! $1 =~ ^-[fsSL]+$ ]]; then
        printf 'fake curl: unexpected option %s\n' "$1" >>"$state/errors"
        exit 2
      fi
      shift
      ;;
    *) url=$1; shift ;;
  esac
done
printf '%s %s\n' "${max_time:-none}" "$url" >>"$state/curl.log"
if [[ -n ${TEST_CURL_FAIL:-} && $url == $TEST_CURL_FAIL ]]; then
  host=${url#*://}
  printf 'curl: (6) Could not resolve host: %s\n' "${host%%/*}" >&2
  exit 6
fi
if [[ ${TEST_SLOW:-} == 1 && $max_time =~ ^[0-9]+$ ]]; then
  echo $(($(<"$state/clock") + max_time)) >"$state/clock"
fi

status_answer() {
  local reads line
  reads=$(($(cat -- "$state/status.reads" 2>/dev/null || echo 0) + 1))
  echo "$reads" >"$state/status.reads"
  # What the recorder's .env and certificate were at this read: a read just before a restart sees the
  # files the restart would start on.
  printf 'status-read env=%s crt=%s\n' "$(sum "${GHOST_WISPR_DEPLOY_DIR:?}/.env")" \
    "$(sum "$GHOST_WISPR_DEPLOY_DIR/device.crt")" >>"$state/status-reads.log"
  line=$(sed -n "${reads}p" "$state/status" 2>/dev/null || true)
  if [[ -z $line ]]; then
    line=$(sed -n '$p' "$state/status" 2>/dev/null || true)
  fi
  if [[ $line == STOPPED ]]; then
    printf 'update.sh read the status of a recorder whose unit had stopped\n' >>"$state/errors"
    exit 7
  fi
  [[ -e $state/active ]] || exit 7
  case $line in
    IDLE) printf '{"paused":false,"capture_state":"capturing","active_session_id":""}\n' ;;
    PAUSED) printf '{"paused":true,"capture_state":"paused","active_session_id":""}\n' ;;
    SESSION) printf '{"paused":false,"capture_state":"recording","active_session_id":"session-1"}\n' ;;
    UNREADABLE) exit 7 ;;
    GARBAGE) printf '<html>502 Bad Gateway</html>\n' ;;
    '{'*) printf '%s\n' "$line" ;;
    *)
      printf 'fake curl: unknown status line %q\n' "$line" >>"$state/errors"
      exit 2
      ;;
  esac
}

sum() {
  local s
  if [[ -f $1 ]] && s=$(sha256sum -- "$1"); then printf '%s\n' "${s%% *}"; else echo none; fi
}

version_answer() {
  local commit
  [[ -e $state/active && ! -e $state/version-mute ]] || exit 7
  commit=$(awk '/^# answers commit / { print $4; exit }' "$state/running-image" 2>/dev/null || true)
  [[ -n $commit ]] || exit 7
  printf '{"commit":"%s"}\n' "$commit"
}

case $url in
  */releases/latest) cat -- "${TEST_RELEASES:?}/${TEST_RELEASE:?}/release.json" ;;
  https://test.invalid/assets/*)
    asset=${TEST_RELEASES:?}/${url#https://test.invalid/assets/}
    [[ -f $asset ]] || exit 22
    if [[ -n $output ]]; then
      cp -- "$asset" "$output"
    else
      cat -- "$asset"
    fi
    if [[ $url == */ghost-wispr-linux-arm64 ]]; then
      "${TEST_FAKES:?}/after" download
    fi
    ;;
  */api/status) status_answer ;;
  */api/version) version_answer ;;
  *)
    printf 'fake curl: unexpected URL %s\n' "$url" >>"$state/errors"
    exit 2
    ;;
esac
EOF

cat >"$bin/systemctl" <<'EOF'
#!/usr/bin/env bash
# The fake systemctl --user of ghost-wispr's user manager.
# - is-active: the recorder's unit is active while $TEST_STATE/active exists.
# - show: MainPID ($TEST_STATE/main-pid, 4242 by default, 0 while the unit is not active),
#   LoadState (loaded while the unit file is in the unit directory, else not-found), DropInPaths
#   (each *.conf in the unit's .d directory there) and ExecMainStartTimestamp ($TEST_STATE/main-start,
#   the real time the recorder's main process started, as --timestamp=us+utc prints it; empty while
#   the unit is not active), as NAME=VALUE lines or, with --value, values.
# - show-environment: $TEST_STATE/manager-env, else a manager's few variables; it exits
#   $TEST_STATE/manager-env-exit (0 by default).
# - daemon-reload logs the recorder's unit file as it reads it. The Nth one fails, once, when
#   $TEST_STATE/fail-daemon-reload-at says N.
# - restart ghost-wispr.service logs the .env its start job begins on, then runs the installed
#   unit's ExecStartPre lines (run_start_pre), and logs what the recorder starts on after them (the
#   binary, .env, config and certificate). It takes TEST_RESTART_SECONDS of the test's clock,
#   starts the installed binary (its running image) and makes the unit active, then is the event
#   "restart". The recorder's main process starts after the ExecStartPre lines, at the real time it
#   writes to $TEST_STATE/main-start. The Nth restart does what $TEST_STATE/restart-N says: ok (the
#   default); fail, a start job that fails while the unit stays active; fail-stop, one whose unit
#   stops; busy-exec, a start job that fails while the installed binary runs on as a real process.
# Every other call is logged as it is.
set -euo pipefail
state=${TEST_STATE:?}
deploy=${GHOST_WISPR_DEPLOY_DIR:?}
units=${GHOST_WISPR_UNIT_DIR:?}
sum() {
  local s
  if [[ -f $1 ]] && s=$(sha256sum -- "$1"); then printf '%s\n' "${s%% *}"; else echo none; fi
}
unexpected() {
  printf 'fake systemctl: unexpected systemctl --user %s\n' "$*" >>"$state/errors"
  exit 1
}
# run_start_pre runs each ExecStartPre line of the installed ghost-wispr.service in order, as its
# start job does: from the deploy directory, which stands for /opt/ghost-wispr, on the fake Pi's
# network, which is up (the fake timeout answers wait-for-network.sh's probe), and with its clock
# synchronized ($TEST_STATE/clock-synchronized is systemd-timesyncd's mark). The steps' output goes
# to $TEST_STATE/journal.log, and the pi-secrets a step runs logs to
# $TEST_STATE/start-pi-secrets.log, apart from update.sh's own reads. A step that fails fails the
# start, unless its line starts with -.
run_start_pre() {
  local unit=$units/ghost-wispr.service line ignore command
  [[ -f $unit ]] || return 0
  while IFS= read -r line; do
    ignore=false
    if [[ $line == -* ]]; then
      ignore=true line=${line#-}
    fi
    if [[ $line != /opt/ghost-wispr/* ]]; then
      printf 'fake systemctl: ExecStartPre=%s runs no command in /opt/ghost-wispr\n' "$line" >>"$state/errors"
      return 1
    fi
    read -ra command <<<"$deploy/${line#/opt/ghost-wispr/}"
    printf 'start-pre %s\n' "$line" >>"$state/journal.log"
    if ! (cd -- "$deploy" && TEST_PI_SECRETS_LOG="$state/start-pi-secrets.log" \
      GHOST_WISPR_TIMESYNC_MARK="$state/clock-synchronized" GHOST_WISPR_TIMESYNC_WAITED="$state/time-sync-gave-up" \
      "${command[@]}") >>"$state/journal.log" 2>&1; then
      $ignore || return 1
    fi
  done < <(sed -n 's/^ExecStartPre=//p' "$unit")
}
if [[ ${1:-} != --user ]]; then
  printf 'systemctl without --user, which reaches the system manager: systemctl %s\n' "$*" >>"$state/errors"
  exit 1
fi
shift
case ${1:-} in
  is-active)
    [[ $* == 'is-active --quiet ghost-wispr.service' ]] || unexpected "$@"
    if [[ -e $state/active ]]; then exit 0; fi
    exit 3
    ;;
  show-environment)
    if [[ -f $state/manager-env ]]; then
      cat -- "$state/manager-env"
    else
      printf '%s\n' HOME=/home/ghost-wispr LANG=C.UTF-8 PATH=/usr/local/bin:/usr/bin:/bin
    fi
    exit "$(cat -- "$state/manager-env-exit" 2>/dev/null || echo 0)"
    ;;
  show)
    shift
    props=() value=false unit='' timestamp=pretty
    while (($#)); do
      case $1 in
        -p | --property) IFS=, read -ra more <<<"$2"; props+=("${more[@]}"); shift 2 ;;
        --property=*) IFS=, read -ra more <<<"${1#--property=}"; props+=("${more[@]}"); shift ;;
        --value) value=true; shift ;;
        --timestamp=*) timestamp=${1#--timestamp=}; shift ;;
        -*) unexpected show "$@" ;;
        *) unit=$1; shift ;;
      esac
    done
    for prop in "${props[@]}"; do
      case $prop in
        MainPID)
          v=0
          if [[ -e $state/active ]]; then v=$(cat -- "$state/main-pid" 2>/dev/null || echo 4242); fi
          ;;
        LoadState)
          v=not-found
          if [[ -f $units/$unit ]]; then v=loaded; fi
          ;;
        DropInPaths)
          v=''
          for dropin in "$units/$unit.d"/*.conf; do
            if [[ -e $dropin ]]; then v+="${v:+ }$dropin"; fi
          done
          ;;
        ExecMainStartTimestamp)
          # update.sh compares it with file times to the microsecond, in UTC.
          [[ $timestamp == us+utc ]] || unexpected show "$prop with --timestamp=$timestamp"
          v=''
          if [[ -e $state/active ]]; then v=$(cat -- "$state/main-start" 2>/dev/null || true); fi
          ;;
        *) unexpected show "$prop" ;;
      esac
      if $value; then printf '%s\n' "$v"; else printf '%s=%s\n' "$prop" "$v"; fi
    done
    ;;
  daemon-reload)
    n=$(($(cat -- "$state/daemon-reloads" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$state/daemon-reloads"
    if [[ $(cat -- "$state/fail-daemon-reload-at" 2>/dev/null || true) == "$n" ]]; then
      rm -f -- "$state/fail-daemon-reload-at"
      printf 'daemon-reload failed (made to fail)\n' >>"$state/systemctl.log"
      exit 1
    fi
    printf 'daemon-reload unit=%s\n' "$(sum "$units/ghost-wispr.service")" >>"$state/systemctl.log"
    ;;
  restart)
    [[ $* == 'restart ghost-wispr.service' ]] || unexpected "$@"
    n=$(($(cat -- "$state/restarts" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$state/restarts"
    printf 'start-pre env=%s\n' "$(sum "$deploy/.env")" >>"$state/systemctl.log"
    if ! run_start_pre; then
      rm -f -- "$state/active"
      exit 1
    fi
    printf 'restart binary=%s env=%s config=%s crt=%s\n' "$(sum "$deploy/ghost-wispr")" "$(sum "$deploy/.env")" \
      "$(sum "$deploy/ghost-wispr.yaml")" "$(sum "$deploy/device.crt")" >>"$state/systemctl.log"
    echo $(($(<"$state/clock") + ${TEST_RESTART_SECONDS:-0})) >"$state/clock"
    if [[ -f $deploy/ghost-wispr ]]; then
      cp -- "$deploy/ghost-wispr" "$state/running-image"
    fi
    # The start's time from bash's clock, formatted here: uutils date (Ubuntu 26.04's date) prints
    # %6N wrong for a fraction under 0.1 s.
    now=$EPOCHREALTIME
    printf '%s.%s UTC\n' "$(/bin/date -u -d "@${now%[.,]*}" '+%a %Y-%m-%d %H:%M:%S')" "${now#*[.,]}" >"$state/main-start"
    printf '%s\n' "${now//[!0-9]/}" >"$state/main-start.us"
    touch -- "$state/active"
    "${TEST_FAKES:?}/after" restart
    case $(cat -- "$state/restart-$n" 2>/dev/null || echo ok) in
      ok) ;;
      fail) exit 1 ;;
      fail-stop)
        rm -f -- "$state/active"
        exit 1
        ;;
      busy-exec)
        "$deploy/ghost-wispr" 60 >/dev/null 2>&1 &
        printf '%s\n' "$!" >"${TEST_BUSY_PID:?}"
        /bin/sleep 0.2
        exit 1
        ;;
      *) unexpected "restart behaviour $(<"$state/restart-$n")" ;;
    esac
    ;;
  *) printf '%s\n' "$*" >>"$state/systemctl.log" ;;
esac
EOF

cat >"$bin/sudo" <<'EOF'
#!/usr/bin/env bash
# The fake sudo. It refuses a call without -n, which would wait for a password where nobody can
# type one. It runs install for real, into GHOST_WISPR_SYSTEM_ROOT alone, and tailscale (the fake),
# and logs every call with the timeout it runs under, udevadm with the udev rule it would read and
# the logind restart with the logind setting, in $TEST_STATE/sudo.log. Each udevadm call and each
# logind restart is the event "system-reload". The call that $TEST_STATE/fail-sudo names, as its
# log line names it after the timeout (such as "udevadm control --reload"), fails, once.
set -euo pipefail
state=${TEST_STATE:?}
system_root=${GHOST_WISPR_SYSTEM_ROOT:?}
sum() {
  local s
  if [[ -f $1 ]] && s=$(sha256sum -- "$1"); then printf '%s\n' "${s%% *}"; else echo none; fi
}
unexpected() {
  printf 'fake sudo: unexpected sudo -n %s\n' "$*" >>"$state/errors"
  exit 1
}
# made_to_fail LINE ends the call with exit 1, once, when $TEST_STATE/fail-sudo names LINE.
made_to_fail() {
  if [[ -f $state/fail-sudo && $(<"$state/fail-sudo") == "$1" ]]; then
    rm -f -- "$state/fail-sudo"
    printf 'timeout=%s %s (made to fail)\n' "$under" "$1" >>"$state/sudo.log"
    exit 1
  fi
}
if [[ ${1:-} != -n ]]; then
  printf 'sudo without -n: sudo %s\n' "$*" >>"$state/errors"
  exit 1
fi
shift
under=${TEST_TIMEOUT_DURATION:-none}
case ${1:-} in
  install)
    destination=${!#}
    [[ $destination == "$system_root"/* ]] || unexpected "$@"
    made_to_fail "install ${destination#"$system_root"}"
    printf 'timeout=%s install %s\n' "$under" "${destination#"$system_root"}" >>"$state/sudo.log"
    exec install "${@:2}"
    ;;
  udevadm)
    made_to_fail "udevadm ${*:2}"
    printf 'timeout=%s udevadm %s rules=%s\n' "$under" "${*:2}" "$(sum "$system_root/etc/udev/rules.d/99-ghost-wispr.rules")" >>"$state/sudo.log"
    "${TEST_FAKES:?}/after" system-reload
    ;;
  systemctl)
    [[ ${*:2} == 'restart systemd-logind' ]] || unexpected "$@"
    made_to_fail 'systemctl restart systemd-logind'
    printf 'timeout=%s systemctl restart systemd-logind logind=%s\n' "$under" \
      "$(sum "$system_root/etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf")" >>"$state/sudo.log"
    "${TEST_FAKES:?}/after" system-reload
    ;;
  tailscale)
    printf 'timeout=%s tailscale %s\n' "$under" "${*:2}" >>"$state/sudo.log"
    TEST_VIA_SUDO=1 exec tailscale "${@:2}"
    ;;
  *) unexpected "$@" ;;
esac
EOF

cat >"$bin/tailscale" <<'EOF'
#!/usr/bin/env bash
# The fake tailscale: `serve status --json`, through sudo (C3), answers $TEST_STATE/serve.json, or
# fails with exit $TEST_STATE/serve-exit, or hangs while $TEST_STATE/serve-hang exists.
set -euo pipefail
state=${TEST_STATE:?}
if [[ ${TEST_VIA_SUDO:-} != 1 ]]; then
  printf 'tailscale ran without sudo: tailscale %s\n' "$*" >>"$state/errors"
  exit 1
fi
if [[ $* != 'serve status --json' ]]; then
  printf 'fake tailscale: unexpected tailscale %s\n' "$*" >>"$state/errors"
  exit 1
fi
if [[ -e $state/serve-hang ]]; then
  /bin/sleep 30
  printf 'tailscale serve status hung for 30 s and nothing stopped it\n' >>"$state/errors"
  exit 1
fi
if [[ -f $state/serve-exit ]]; then
  printf 'fake tailscale: failed\n' >&2
  exit "$(<"$state/serve-exit")"
fi
cat -- "$state/serve.json"
EOF

cat >"$bin/udevadm" <<'EOF'
#!/usr/bin/env bash
printf 'udevadm ran without sudo: udevadm %s\n' "$*" >>"${TEST_STATE:?}/errors"
exit 1
EOF

cat >"$bin/sleep" <<'EOF'
#!/usr/bin/env bash
# sleep N notes the wait and moves the test's clock on by it, at once. A fractional wait, a fake's
# own, really sleeps.
case $1 in *.*) exec /bin/sleep "$@" ;; esac
printf 'sleep %s\n' "$*" >>"${TEST_STATE:?}/sleep.log"
echo $(($(<"$TEST_STATE/clock") + $1)) >"$TEST_STATE/clock"
EOF

cat >"$bin/date" <<'EOF'
#!/usr/bin/env bash
# date +%s reads the test's clock, which only sleep, restarts, slow calls and timeouts move.
[[ $* == +%s ]] || exec /bin/date "$@"
cat -- "${TEST_STATE:?}/clock"
EOF

cat >"$timeout_bin/timeout" <<'EOF'
#!/usr/bin/env bash
# The fake timeout. It runs the command under the real timeout ($TEST_REAL_TIMEOUT), for at most
# TEST_TIMEOUT_REAL real seconds (3 by default), and tells the command through
# TEST_TIMEOUT_DURATION and TEST_TIMEOUT_KILL_AFTER which bound it runs under. When the real timeout
# ends a command that hung (124, or 137 after its kill), the test's clock moves on by the bound
# asked for, and by the kill's grace for a 137, as the real bound would have taken. Each call goes
# to $TEST_STATE/timeout.log. The fake Pi's network is up: wait-for-network.sh's probe of a TCP
# port connects at once, and the test opens no connection.
set -uo pipefail
state=${TEST_STATE:?}
kill_after=''
while (($#)); do
  case $1 in
    --kill-after=*) kill_after=${1#--kill-after=}; shift ;;
    -k) kill_after=$2; shift 2 ;;
    --foreground | --preserve-status) shift ;;
    -*)
      printf 'fake timeout: unexpected option %s\n' "$1" >>"$state/errors"
      exit 125
      ;;
    *) break ;;
  esac
done
duration=$1
shift
seconds() {
  case $1 in
    *s) printf '%s\n' "${1%s}" ;;
    *m) printf '%s\n' "$((${1%m} * 60))" ;;
    *h) printf '%s\n' "$((${1%h} * 3600))" ;;
    *) printf '%s\n' "$1" ;;
  esac
}
want=$(seconds "$duration")
grace=$(seconds "${kill_after:-0}")
cap=${TEST_TIMEOUT_REAL:-3}
real=$((want < cap ? want : cap))
printf 'kill-after=%s %s %s\n' "${kill_after:-none}" "$duration" "$*" >>"$state/timeout.log"
if [[ ${1:-} == bash && ${2:-} == -c && ${3:-} == 'exec 3<>/dev/tcp/'* ]]; then
  exit 0
fi
TEST_TIMEOUT_DURATION=$duration TEST_TIMEOUT_KILL_AFTER=${kill_after:-none} \
  "${TEST_REAL_TIMEOUT:?}" --kill-after=1 "$real" "$@"
status=$?
case $status in
  124) echo $(($(<"$state/clock") + want)) >"$state/clock" ;;
  137) echo $(($(<"$state/clock") + want + grace)) >"$state/clock" ;;
esac
exit "$status"
EOF

cat >"$fakes/pi-secrets" <<'EOF'
#!/usr/bin/env bash
# The fake pi-secrets, which the fake release binary runs: $1 is the binary as update.sh ran it, the
# rest the binary's arguments. Each call goes to $TEST_PI_SECRETS_LOG ($TEST_STATE/pi-secrets.log
# by default): the binary, its SHA-256, the timeout it runs under and each argument. As C4's
# pi-secrets does, it writes --out/secrets.env, one --env-secret VARIABLE=SECRET a line in flag
# order (each VARIABLE's value from $TEST_STATE/secrets), stages $TEST_STATE/certificate as
# --out/device.crt when that file exists, and prints C4's lines, never a value. It is the event
# "secrets". $TEST_STATE/pi-secrets-mode "fail" makes it fail as a key read failure does (R10): no
# secrets.env and exit 1, after the certificate step has still staged $TEST_STATE/certificate, when
# that exists; "hang" makes it hang until a timeout ends it. Its certificate lines are the real
# pi-secrets' (cmd/ghost-wispr/pi_secrets.go).
set -euo pipefail
state=${TEST_STATE:?}
binary=$1
shift
if [[ ${1:-} != pi-secrets ]]; then
  printf 'fake pi-secrets: run as %s\n' "$*" >>"$state/errors"
  exit 2
fi
shift
binary_sum=$(sha256sum -- "$binary")
{
  printf 'call\n'
  printf 'binary %s\n' "$binary"
  printf 'binary-sha %s\n' "${binary_sum%% *}"
  printf 'timeout %s kill-after %s\n' "${TEST_TIMEOUT_DURATION:-none}" "${TEST_TIMEOUT_KILL_AFTER:-none}"
  printf 'arg %s\n' "$@"
} >>"${TEST_PI_SECRETS_LOG:-$state/pi-secrets.log}"
out='' certificate=false env_secrets=()
while (($#)); do
  case $1 in
    --out) out=$2 ;;
    --env-secret) env_secrets+=("$2") ;;
    --certificate-secret) certificate=true ;;
  esac
  shift 2
done
"${TEST_FAKES:?}/after" secrets
case $(cat -- "$state/pi-secrets-mode" 2>/dev/null || echo ok) in
  hang)
    /bin/sleep 30
    printf 'pi-secrets hung for 30 s and nothing stopped it\n' >>"$state/errors"
    exit 0
    ;;
  fail)
    printf 'pi-secrets: could not read %s from %s\n' "${env_secrets[0]%%=*}" "${env_secrets[0]#*=}"
    if $certificate && [[ -f $state/certificate ]]; then
      cp -- "$state/certificate" "$out/device.crt"
      printf 'pi-secrets: certificate version 2, not after 2027-11-26T00:00:00Z: newer and signs in\n'
    fi
    exit 1
    ;;
esac
if [[ -z $out || ! -d $out ]]; then
  printf 'pi-secrets: --out %s is not a directory\n' "$out"
  exit 1
fi
lines=''
for pair in "${env_secrets[@]}"; do
  value=$(sed -n "s/^${pair%%=*}=//p" "$state/secrets")
  if [[ -z $value ]]; then
    printf 'pi-secrets: could not read %s from %s\n' "${pair%%=*}" "${pair#*=}"
    exit 1
  fi
  lines+="${pair%%=*}=$value"$'\n'
done
if ((${#env_secrets[@]})); then
  printf '%s' "$lines" >"$out/secrets.env"
  chmod 0600 "$out/secrets.env"
  for pair in "${env_secrets[@]}"; do
    printf 'pi-secrets: %s from %s version 3\n' "${pair%%=*}" "${pair#*=}"
  done
fi
if $certificate; then
  if [[ -f $state/certificate ]]; then
    cp -- "$state/certificate" "$out/device.crt"
    printf 'pi-secrets: certificate version 2, not after 2027-11-26T00:00:00Z: newer and signs in\n'
  else
    printf 'pi-secrets: certificate version 1 kept: not newer\n'
  fi
fi
EOF
chmod 0755 "$bin"/* "$timeout_bin/timeout" "$fakes"/*

# test_pi_env prints a release's pi.env with the settings update.sh reads from it (C1), in neutral
# values: the repository's own pi.env names the company's resources.
test_pi_env() {
  cat <<'EOF'
# shellcheck shell=bash disable=SC2034 # sourced by update.sh
PI_GCP_PROJECT=example-project
PI_CERTIFICATE_SECRET=example-device-certificate
PI_DEEPGRAM_SECRET=example-deepgram-secret
PI_DISPATCH_TOKEN_SECRET=example-dispatch-token-secret
EOF
}

# fake_binary COMMIT NAME prints a release binary that answers COMMIT once it runs (the fake
# /api/version reads its "# answers commit" line), runs the fake pi-secrets for pi-secrets, and
# answers link-versions as today's binary does, which has no such subcommand (exit 2).
fake_binary() {
  printf '#!/usr/bin/env bash\n# answers commit %s\n# release %s\n' "$1" "$2"
  cat <<'EOF'
if [[ ${1:-} == pi-secrets ]]; then exec "${TEST_PI_SECRETS:?}" "$0" "$@"; fi
if [[ ${1:-} == link-versions ]]; then
  printf '%s\n' 'unknown subcommand "link-versions"' 'usage: ghost-wispr <command>' >&2
  exit 2
fi
exit 0
EOF
}

# make_release NAME COMMIT stages release NAME of commit COMMIT in $releases/NAME: its binary, and
# ghost-wispr-pi-deploy/ with every file of C2 from the repository. Each file of the deploy set ends
# with a line naming NAME, so the sets of two releases differ in every file; with
# same_system_files=1 the udev rule and the logind setting are the repository's, the same in every
# release. The provisioning files are the repository's in every release. Edit the files, then
# pack_release.
make_release() {
  local name=$1 commit=$2 dir=$releases/$1 deploy file
  deploy=$dir/ghost-wispr-pi-deploy
  rm -rf -- "${dir:?}"
  mkdir -p -- "$deploy"
  cp -- "$repo_root/deploy/pi/files/ghost-wispr.yaml" "$repo_root/deploy/pi/files/wait-for-time-sync.sh" \
    "$repo_root/deploy/pi/files/ghost-wispr.service" "$repo_root/deploy/pi/files/99-ghost-wispr.rules" \
    "$repo_root/deploy/pi/files/logind-power-key.conf" "$repo_root/deploy/pi/files/ghost-wispr-update-launcher.sh" \
    "$repo_root/deploy/pi/files/ghost-wispr-update.service" "$repo_root/deploy/pi/files/ghost-wispr-update.timer" \
    "$repo_root/deploy/pi/files/serve.conf" "$repo_root/deploy/pi/update.sh" "$repo_root/deploy/pi/pi-lib.sh" \
    "$repo_root/deploy/pi/provision.sh" "$repo_root/wait-for-network.sh" "$deploy/"
  test_pi_env >"$deploy/pi.env"
  for file in "${set_files[@]}"; do
    if [[ ${same_system_files:-} == 1 && ($file == 99-ghost-wispr.rules || $file == logind-power-key.conf) ]]; then
      continue
    fi
    printf '# release %s\n' "$name" >>"$deploy/$file"
  done
  fake_binary "$commit" "$name" >"$dir/ghost-wispr-linux-arm64"
  chmod 0755 "$dir/ghost-wispr-linux-arm64"
  printf '%s\n' "$commit" >"$dir/commit"
}

# pack_release NAME writes release NAME's manifest.json (C2: format 1, its commit, binary_sha256,
# the SHA-256 of every other file, and link_versions when $link_versions holds a JSON array), with
# the jq filter $manifest_filter applied last; then its tarball, its checksum asset, and what
# publish_release writes.
pack_release() {
  local name=$1 dir=$releases/$1 deploy file files='{}' commit
  deploy=$dir/ghost-wispr-pi-deploy
  commit=$(<"$dir/commit")
  rm -f -- "$deploy/manifest.json"
  for file in "$deploy"/*; do
    files=$(jq -c --arg name "${file##*/}" --arg sum "$(sha "$file")" '. + {($name): $sum}' <<<"$files")
  done
  jq -n --arg commit "$commit" --arg binary "$(sha "$dir/ghost-wispr-linux-arm64")" --argjson files "$files" \
    --argjson link "${link_versions:-null}" \
    "{format: 1, commit: \$commit, binary_sha256: \$binary, files: \$files}
      + (if \$link == null then {} else {link_versions: \$link} end) | ${manifest_filter:-.}" >"$deploy/manifest.json"
  tar -C "$dir" -czf "$dir/ghost-wispr-pi-deploy.tar.gz" ghost-wispr-pi-deploy
  printf '%s  %s\n' "$(sha "$dir/ghost-wispr-pi-deploy.tar.gz")" ghost-wispr-pi-deploy.tar.gz >"$dir/ghost-wispr-pi-deploy.tar.gz.sha256"
  publish_release "$name"
}

# publish_release NAME writes, beside release NAME's binary and tarball, the binary's checksum asset
# and the release API's answer, whose asset URLs the fake curl serves.
publish_release() {
  local name=$1 dir=$releases/$1
  printf '%s  %s\n' "$(sha "$dir/ghost-wispr-linux-arm64")" ghost-wispr-linux-arm64 >"$dir/ghost-wispr-linux-arm64.sha256"
  jq -n --arg commit "$(<"$dir/commit")" --arg base "https://test.invalid/assets/$name" '{target_commitish: $commit, assets: [
      "ghost-wispr-linux-arm64", "ghost-wispr-linux-arm64.sha256", "ghost-wispr-pi-deploy.tar.gz",
      "ghost-wispr-pi-deploy.tar.gz.sha256", "THIRD_PARTY_NOTICES.md"
      | {name: ., browser_download_url: ($base + "/" + .)}]}' >"$dir/release.json"
}

# serve_json_for SERVE_CONF prints the `tailscale serve status --json` answer (ipn.ServeConfig) of a
# node whose Serve is exactly SERVE_CONF's lines: one Web entry a port, whose handler / proxies to
# the line's target.
serve_json_for() {
  local port target web='{}'
  while read -r port target; do
    if [[ -z $port || $port == \#* ]]; then
      continue
    fi
    web=$(jq -c --arg key "ghost-wispr.example-tailnet.ts.net:$port" --arg proxy "$target" \
      '. + {($key): {Handlers: {"/": {Proxy: $proxy}}}}' <<<"$web")
  done <"$1"
  jq -c --argjson web "$web" '{TCP: ([$web | keys[] | split(":")[1] | {(.): {HTTPS: true}}] | add), Web: $web}' <<<'{}'
}

# write_env FILE LINE... writes an .env of LINEs, mode 0600.
write_env() {
  local file=$1
  shift
  printf '%s\n' "$@" >"$file"
  chmod 0600 "$file"
}

# render_with_drive TEMPLATE DRIVE prints TEMPLATE as provision.sh --models installed it with the
# shared drive DRIVE: the fake Pi's installed config, not what the test checks update.sh against.
render_with_drive() {
  awk '!/^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):/' "$1"
  printf 'gdrive_folder_id: "%s"\n' "$2"
  printf '%s\n' 'gdrive_sync_enabled: true' 'gc_enabled: true'
}

# record_verified NAME [FILE] writes deploy.verified (or FILE) for release NAME as C2 defines it: a
# line "commit <sha>", then "<sha256>  <name>" for each file of the deploy set as the manifest gives
# it (the config's is its template's), and the binary's as "ghost-wispr".
record_verified() {
  local manifest=$releases/$1/ghost-wispr-pi-deploy/manifest.json file
  {
    printf 'commit %s\n' "$(jq -r .commit "$manifest")"
    for file in "${set_files[@]}"; do
      printf '%s  %s\n' "$(jq -r --arg f "$file" '.files[$f]' "$manifest")" "$file"
    done
    printf '%s  ghost-wispr\n' "$(jq -r .binary_sha256 "$manifest")"
  } >"${2:-$opt/deploy.verified}"
}

# new_pi makes an empty fake Pi: the recorder's unit active and idle, the device key, the credential
# file and the installed certificate, Serve as the repository's serve.conf sets it, a user manager
# that sets no override, and the fake pi-secrets answering the secret values of the default .env.
new_pi() {
  stop_busy_binary
  rm -rf -- "${pi:?}"
  mkdir -p -- "$opt" "$units" "$root/etc/udev/rules.d" "$root/etc/systemd/logind.conf.d" "$state" "$pi/proc/4242"
  ln -s -- "$state/running-image" "$pi/proc/4242/exe"
  printf 'DEVICE KEY\n' >"$device_key"
  printf '{"type":"external_account"}\n' >"$opt/google-credentials.json"
  printf 'INSTALLED CERTIFICATE\n' >"$opt/device.crt"
  mark_recorder_started
  printf 'IDLE\n' >"$state/status"
  touch -- "$state/active" "$state/systemctl.log" "$state/sudo.log" "$state/sleep.log" "$state/curl.log" \
    "$state/timeout.log" "$state/pi-secrets.log" "$state/start-pi-secrets.log" "$state/journal.log" \
    "$state/clock-synchronized" "$state/status-reads.log"
  serve_json_for "$repo_root/deploy/pi/files/serve.conf" >"$state/serve.json"
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 >"$state/secrets"
  echo 1000 >"$state/clock"
}

# set_recorder_start SECONDS MICROSECONDS notes that time, MICROSECONDS as six digits, as the start
# of the recorder's main process: $state/main-start as systemctl --timestamp=us+utc prints it, and
# $state/main-start.us in microseconds. The fraction is written here, not with date's %6N, which
# uutils date (Ubuntu 26.04's date) prints wrong for a fraction under 0.1 s.
set_recorder_start() {
  printf '%s.%s UTC\n' "$(date -u -d "@$1" '+%a %Y-%m-%d %H:%M:%S')" "$2" >"$state/main-start"
  printf '%s%s\n' "$1" "$2" >"$state/main-start.us"
}

# mark_recorder_started notes now as the start of the recorder's main process, as the fake restart
# does: each file written before is one it started on.
mark_recorder_started() {
  local now=$EPOCHREALTIME
  set_recorder_start "${now%[.,]*}" "${now#*[.,]}"
}

# written_after_start FILE... gives each FILE the time 1 ms after the recorder's start, as a file that
# an update installed and then never restarted the recorder for. Any later restart is later still.
written_after_start() {
  local start
  start=$(($(<"$state/main-start.us") + 1000))
  touch -d "@${start:0:-6}.${start: -6}" -- "$@"
}

# install_set NAME [DRIVE] puts release NAME's deploy set in place on the fake Pi as update.sh
# leaves a set it installed and verified: each file where C2 puts it, the config rendered with the
# shared drive DRIVE when given, an .env of the four owned lines and one operator line, the
# bootstrap files as provision.sh installs them, deploy.verified naming the set, and the recorder
# running the installed binary, started after each of these files was written.
install_set() {
  local name=$1 drive=${2:-} deploy=$releases/$1/ghost-wispr-pi-deploy file
  for file in "${set_files[@]}" "${bootstrap_files[@]}"; do
    install -D -m "$(file_mode "$file")" -- "$deploy/$file" "$(installed_path "$file")"
  done
  if [[ -n $drive ]]; then
    render_with_drive "$deploy/ghost-wispr.yaml" "$drive" >"$opt/ghost-wispr.yaml"
  fi
  install -m 0755 -- "$releases/$name/ghost-wispr-linux-arm64" "$opt/ghost-wispr"
  write_env "$opt/.env" GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
    GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 GHOST_WISPR_JABRA_MUTE_BUTTON=false
  record_verified "$name"
  cp -- "$opt/ghost-wispr" "$state/running-image"
  mark_recorder_started
}

# save_installed DIR copies what the fake Pi has installed of the deploy set, its rendered config,
# .env and binary included, and the certificate and its version file when it has them, into DIR,
# one file a name.
save_installed() {
  local dir=$1 file
  rm -rf -- "${dir:?}"
  mkdir -p -- "$dir"
  for file in "${set_files[@]}" .env ghost-wispr; do
    cp -p -- "$(installed_path "$file")" "$dir/$file"
  done
  for file in device.crt device.crt.version; do
    if [[ -e $opt/$file ]]; then
      cp -p -- "$opt/$file" "$dir/$file"
    fi
  done
}

# expect_prev_is WHAT DIR: each .prev copy update.sh keeps in the deploy directory is, byte for
# byte, the file save_installed copied into DIR.
expect_prev_is() {
  local file
  for file in "${set_files[@]}" .env ghost-wispr; do
    cmp -s -- "$2/$file" "$opt/$file.prev" || fail "$1: $opt/$file.prev is not the $file installed before"
  done
}

# expect_installed_is WHAT DIR [NAME...]: each file NAME, by default each file of the deploy set,
# .env and the binary, is installed again as save_installed copied it into DIR, byte for byte.
expect_installed_is() {
  local what=$1 dir=$2 file path
  shift 2
  (($#)) || set -- "${set_files[@]}" .env ghost-wispr
  for file in "$@"; do
    path=$(installed_path "$file")
    cmp -s -- "$dir/$file" "$path" || fail "$what: $path is not, byte for byte, the $file installed before"
  done
}

# expect_set_of WHAT NAME: every file of release NAME's deploy set but the rendered config, and its
# binary, is installed where C2 puts it, byte for byte and in its mode.
expect_set_of() {
  local what=$1 name=$2 deploy=$releases/$2/ghost-wispr-pi-deploy file path
  for file in "${set_files[@]}"; do
    [[ $file != ghost-wispr.yaml ]] || continue
    path=$(installed_path "$file")
    cmp -s -- "$deploy/$file" "$path" || fail "$what: $path is not release $name's $file"
    [[ $(stat -c %a -- "$path") == "$(file_mode "$file")" ]] \
      || fail "$what: $path has mode $(stat -c %a -- "$path"), want $(file_mode "$file")"
  done
  cmp -s -- "$releases/$name/ghost-wispr-linux-arm64" "$opt/ghost-wispr" || fail "$what: the installed binary is not release $name's"
  [[ $(stat -c %a -- "$opt/ghost-wispr") == 755 ]] || fail "$what: the installed binary is not mode 755"
}

# expect_verified WHAT NAME: deploy.verified names release NAME's set, its lines after the first in
# any order.
expect_verified() {
  local want have
  record_verified "$2" "$work_dir/verified.want"
  [[ -f $opt/deploy.verified ]] || fail "$1: no deploy.verified"
  want=$(head -n1 "$work_dir/verified.want" && tail -n +2 "$work_dir/verified.want" | LC_ALL=C sort)
  have=$(head -n1 "$opt/deploy.verified" && tail -n +2 "$opt/deploy.verified" | LC_ALL=C sort)
  [[ $have == "$want" ]] || fail "$1: deploy.verified is:
$(<"$opt/deploy.verified")
want:
$(<"$work_dir/verified.want")"
}

# The office Pi's recording rules in deploy/pi/files/ghost-wispr.yaml, which its web UI shows under
# Recording rules, each as written and in this order: the sha256 of the recording_rules block. The
# test holds the hash, not the text, because the repository is public. After a rule change, set it
# to the awk below run over the template, piped to sha256sum.
readonly recording_rules_sha256=89f65b9df94d27abf6657ba13e183921b7c27e19d8ba5e90c189e4046c5c8040
# expect_recording_rules WHAT CONFIG: CONFIG holds exactly the office Pi's recording rules, once.
expect_recording_rules() {
  local rules
  rules=$(awk '/^recording_rules:/ { print; inside = 1; next } inside && /^  - / { print; next } { inside = 0 }' "$2" | sha256sum)
  [[ $rules == "$recording_rules_sha256  -" ]] || fail "$1: $2 does not hold the office Pi's recording rules exactly once"
}

# expect_config_with_drive WHAT TEMPLATE: the installed config is TEMPLATE with the deployed shared
# drive: every setting of TEMPLATE but the drive's three, and those once each, sync and GC on.
expect_config_with_drive() {
  local config=$opt/ghost-wispr.yaml line key
  for line in "gdrive_folder_id: \"$drive_id\"" 'gdrive_sync_enabled: true' 'gc_enabled: true'; do
    [[ $(grep -cFx -- "$line" "$config") == 1 ]] || fail "$1: the installed config does not hold $line once"
  done
  for key in gdrive_folder_id gdrive_sync_enabled gc_enabled; do
    [[ $(grep -c "^$key:" "$config") == 1 ]] || fail "$1: the installed config sets $key more than once"
  done
  diff <(grep -Ev '^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):' "$2") \
    <(grep -Ev '^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):' "$config") >/dev/null \
    || fail "$1: the installed config is not the release's template with the deployed drive"
  [[ $(stat -c %a -- "$config") == 640 ]] || fail "$1: the installed config is not mode 640"
}

# snapshot prints every path of the fake Pi's deploy directory, unit directory and system root, with
# its mode, and every file's SHA-256.
snapshot() {
  (
    cd -- "$pi"
    find opt units root -printf '%M %p\n' | LC_ALL=C sort
    find opt units root -type f -exec sha256sum -- {} + | LC_ALL=C sort -k2
  )
}

# expect_unchanged WHAT BEFORE: the fake Pi's files are as snapshot printed them in BEFORE.
expect_unchanged() {
  local now
  now=$(snapshot)
  [[ $now == "$2" ]] || fail "$1 changed the Pi's files:
$(diff <(printf '%s\n' "$2") <(printf '%s\n' "$now") || true)"
}

# run_update [ARGUMENT] runs the fake Pi's installed update.sh (or $updater), as the launcher does,
# from its working directory, with release $release the latest; its output is in $out and its exit
# status in $status. The test fails on anything the fakes found that update.sh must never do.
run_update() {
  echo 1000 >"$state/clock"
  status=0
  out=$(cd -- "$opt" && env PATH="$bin:$timeout_bin:$PATH" \
    TEST_STATE="$state" TEST_RELEASES="$releases" TEST_RELEASE="${release:-none}" TEST_FAKES="$fakes" \
    TEST_PI_SECRETS="$fakes/pi-secrets" TEST_REAL_TIMEOUT="$real_timeout" TEST_BUSY_PID="$work_dir/busy.pid" \
    TEST_SLOW="${slow:-}" TEST_RESTART_SECONDS="${restart_seconds:-0}" \
    INVOCATION_ID=test-invocation-7 \
    GHOST_WISPR_DEPLOY_DIR="$opt" GHOST_WISPR_UNIT_DIR="$units" GHOST_WISPR_SYSTEM_ROOT="$root" \
    GHOST_WISPR_PROC_DIR="$pi/proc" GHOST_WISPR_IDENTITY_KEY="$device_key" \
    GHOST_WISPR_RELEASE_API=https://test.invalid/releases/latest \
    GHOST_WISPR_STATUS_URL=http://127.0.0.1:8080/api/status \
    GHOST_WISPR_VERSION_URL=http://127.0.0.1:8080/api/version \
    "${updater:-$opt/update.sh}" "$@" 2>&1) || status=$?
  if [[ -s $state/errors ]]; then
    fail "the fakes saw what update.sh must never do: $(<"$state/errors"); its output: $out"
  fi
}

# expect_run WHAT STATUS LINE...: update.sh exited STATUS and printed each LINE, whole.
expect_run() {
  local what=$1 want=$2 line
  shift 2
  [[ $status == "$want" ]] || fail "$what: exit $status, want $want; output: $out"
  for line in "$@"; do
    grep -Fqx -- "$line" <<<"$out" || fail "$what: printed no line \"$line\"; output: $out"
  done
}

# expect_line_like WHAT REGEX: update.sh printed a whole line matching the extended regex REGEX.
expect_line_like() {
  grep -Eqx -- "$2" <<<"$out" || fail "$1: printed no line like $2; output: $out"
}

# expect_no_output WHAT TEXT: update.sh printed TEXT nowhere.
expect_no_output() {
  if grep -Fq -- "$2" <<<"$out"; then
    fail "$1: printed \"$2\"; output: $out"
  fi
}

# restarts prints how many times update.sh restarted the recorder.
restarts() {
  grep -c '^restart ' "$state/systemctl.log" || true
}

# expect_env WHAT LINE...: the installed .env is exactly LINEs, mode 0600.
expect_env() {
  local what=$1
  shift
  diff <(printf '%s\n' "$@") "$opt/.env" >&2 || fail "$what: the installed .env is not as C3 writes it (diff above)"
  [[ $(stat -c %a -- "$opt/.env") == 600 ]] || fail "$what: .env is mode $(stat -c %a -- "$opt/.env"), want 600"
}

# secrets_args prints the arguments of the last pi-secrets call, one a line.
secrets_args() {
  awk '/^call$/ { n = 0; delete args; next } /^arg / { args[++n] = substr($0, 5) } END { for (i = 1; i <= n; i++) print args[i] }' \
    "$state/pi-secrets.log"
}

# expect_secrets_flag WHAT FLAG VALUE: the last pi-secrets call had FLAG VALUE as two arguments.
expect_secrets_flag() {
  local args i
  mapfile -t args < <(secrets_args)
  for ((i = 0; i + 1 < ${#args[@]}; i++)); do
    if [[ ${args[i]} == "$2" && ${args[i + 1]} == "$3" ]]; then
      return 0
    fi
  done
  fail "$1: pi-secrets ran without $2 $3: ${args[*]}"
}

# The removed .env variables of internal/config's removedEnvironment, each of which a start warns
# about (pi-lib.sh's PI_LIB_REMOVED_ENV_VARIABLES).
readonly removed_variables=(GCP_PROJECT GCP_LOCATION GENAI_BACKEND EMBEDDING_BACKEND EMBEDDING_LOCATION
  DEEPGRAM_MODEL BATCH_TRANSCRIPTION_PROVIDER TTS_PROVIDER TTS_API_KEY VOICE_TTS_ENGINE VOICE_TTS_MODEL
  VOICE_TTS_VOICE VOICE_TTS_LOCATION ENVOY_WEBHOOK_SECRET)

# lib FUNCTION ARGUMENT... runs one function of pi-lib.sh in a subshell.
lib() {
  (
    # shellcheck source=pi-lib.sh
    source "$repo_root/deploy/pi/pi-lib.sh"
    "$@"
  )
}

# ---------------------------------------------------------------------------------------------------
# whole-set, first: pi-lib.sh, which update.sh and provision.sh both source, and the renderer by
# whose rule the set's config installs (R6).
begin whole-set

# It defines no fail and makes no variable readonly: update.sh and provision.sh each define their own
# fail and a readonly DEPLOY_DIR, so a library that defined either would break whichever script
# sourced it.
if ! sourced=$(
  exec 2>&1
  DEPLOY_DIR=/opt/example
  readonly DEPLOY_DIR
  fail() { printf "the caller's fail\n"; }
  before=$(readonly -p)
  # shellcheck source=pi-lib.sh
  source "$repo_root/deploy/pi/pi-lib.sh" || exit 1
  if [[ $(readonly -p) != "$before" ]]; then
    printf 'pi-lib.sh made a variable readonly\n'
    exit 1
  fi
  fail
); then
  fail "pi-lib.sh could not be sourced beside its caller's readonly DEPLOY_DIR and fail: $sourced"
fi
[[ $sourced == "the caller's fail" ]] || fail "pi-lib.sh changed its caller's fail or variables: $sourced"

# The config renderer: pi-lib.sh's render_config_with_drive, render_config and deployed_drive_id.
# With a shared drive it keeps every template setting but the drive's three, adds the quoted drive
# ID, and turns sync and GC on: GC removes only local audio that Drive holds a copy of.
with_drive=$work_dir/with-drive.yaml
lib render_config_with_drive "$template" "$drive_id" >"$with_drive" \
  || fail 'render_config_with_drive refused a valid shared-drive ID'
for line in "gdrive_folder_id: \"$drive_id\"" 'gdrive_sync_enabled: true' 'gc_enabled: true'; do
  [[ $(grep -cFx -- "$line" "$with_drive") == 1 ]] || fail "the config rendered with a drive does not hold $line once"
done
for key in gdrive_folder_id gdrive_sync_enabled gc_enabled; do
  [[ $(grep -c "^$key:" "$with_drive") == 1 ]] || fail "the config rendered with a drive sets $key more than once"
done
diff <(grep -Ev '^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):' "$template") \
  <(grep -Ev '^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):' "$with_drive") >&2 \
  || fail 'the config rendered with a drive changed a template setting other than the drive'"'"'s (diff above)'
expect_recording_rules 'the config rendered with a drive' "$with_drive"
# A template that already names a drive loses the stale ID and settings.
stale=$work_dir/stale-template.yaml
printf '%s\n' 'existing_setting: retained' 'gdrive_folder_id: "stale-id"' 'gdrive_sync_enabled: false' \
  'envoy_hub_url: http://hub.example.ts.net:9020' 'gc_enabled: false' >"$stale"
lib render_config_with_drive "$stale" "$drive_id" >"$work_dir/stale.yaml" \
  || fail 'render_config_with_drive refused a template with stale Drive settings'
grep -Fqx 'existing_setting: retained' "$work_dir/stale.yaml" && grep -Fqx 'envoy_hub_url: http://hub.example.ts.net:9020' "$work_dir/stale.yaml" \
  || fail "render_config_with_drive dropped a template setting: $(<"$work_dir/stale.yaml")"
if grep -Eq 'stale-id|false' "$work_dir/stale.yaml"; then
  fail "render_config_with_drive kept a stale Drive setting: $(<"$work_dir/stale.yaml")"
fi
# It fails, rather than print a partial config, on a bad drive ID or no template.
for bad_id in '' 'drive id' 'drive/id' 'drive"id' "drive'id" 'drive:id' $'drive\nid'; do
  if lib render_config_with_drive "$template" "$bad_id" >/dev/null 2>&1; then
    fail "render_config_with_drive accepted the drive ID $(printf %q "$bad_id")"
  fi
done
if lib render_config_with_drive "$work_dir/missing.yaml" "$drive_id" >/dev/null 2>&1; then
  fail 'render_config_with_drive succeeded without a template'
fi
# render_config keeps the drive the deployed config names, as render_config_with_drive or yaml.v3
# wrote it, so a release does not turn Drive sync and GC off.
deployed=$work_dir/deployed.yaml
for line in "gdrive_folder_id: \"$drive_id\"" "gdrive_folder_id: $drive_id"; do
  printf '%s\n' 'silence_timeout: 45s' "$line" 'gdrive_sync_enabled: true' >"$deployed"
  lib render_config "$template" "$deployed" >"$work_dir/rendered.yaml" || fail "render_config refused a deployed config with $line"
  cmp -s -- "$with_drive" "$work_dir/rendered.yaml" || fail "render_config did not keep the deployed drive of $line"
done
# With no drive (no line, the gdrive_folder_id: "" every config API save writes without a folder,
# or no deployed config at all, on a new Pi), it is the template byte for byte: sync and GC stay off.
printf '%s\n' 'silence_timeout: 45s' >"$work_dir/no-drive.yaml"
printf '%s\n' 'gdrive_folder_id: ""' >"$work_dir/empty-drive.yaml"
for deployed in "$work_dir/no-drive.yaml" "$work_dir/empty-drive.yaml" "$work_dir/missing.yaml"; do
  lib render_config "$template" "$deployed" >"$work_dir/rendered.yaml" || fail "render_config refused $deployed"
  cmp -s -- "$template" "$work_dir/rendered.yaml" || fail "render_config with $deployed is not the template as it is"
done
# Any other gdrive_folder_id line, or two, fails the render: a config that silently dropped or
# changed the drive would turn sync and GC off, or point them at another drive.
printf 'gdrive_folder_id: "%s"\n' "$drive_id" "$drive_id" >"$work_dir/two-drives.yaml"
printf '%s\n' 'gdrive_folder_id: "drive id"' >"$work_dir/bad-drive.yaml"
for deployed in "$work_dir/two-drives.yaml" "$work_dir/bad-drive.yaml"; do
  if lib render_config "$template" "$deployed" >/dev/null 2>&1; then
    fail "render_config rendered a config from $(<"$deployed")"
  fi
done

begin env-lines
# env_kept_lines, pi-lib.sh's .env filter, keeps every line of a deployed .env in order, comments
# included, but the four lines update.sh owns (C3) and the variables Ghost Wispr no longer reads,
# each of which would warn at every start. It keeps the role overrides that share words with removed
# ones, and the operator's Jabra button line (docs/pi-setup.md).
kept_lines=('# an operator note' GHOST_WISPR_JABRA_MUTE_BUTTON=true GHOST_WISPR_TTS_VOICE=Kore
  GHOST_WISPR_TTS_MODEL=vertex/tts-1 GHOST_WISPR_EMBEDDING_MODEL=vertex/embed-1
  GHOST_WISPR_BATCH_TRANSCRIPTION_MODEL=deepgram/nova-3 GHOST_WISPR_VOICE_ENGINE=tts)
owned_lines=(GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false
  GHOST_WISPR_DEEPGRAM_API_KEY=test-key GHOST_WISPR_DISPATCH_TOKEN=test-token)
for ((i = 0; i < ${#removed_variables[@]} || i < ${#kept_lines[@]}; i++)); do
  if ((i < ${#kept_lines[@]})); then printf '%s\n' "${kept_lines[i]}"; fi
  if ((i < ${#removed_variables[@]})); then printf 'GHOST_WISPR_%s=removed-value\n' "${removed_variables[i]}"; fi
  if ((i < ${#owned_lines[@]})); then printf '%s\n' "${owned_lines[i]}"; fi
done >"$work_dir/deployed.env"
lib env_kept_lines "$work_dir/deployed.env" >"$work_dir/kept.env" || fail 'env_kept_lines refused a deployed .env'
diff <(printf '%s\n' "${kept_lines[@]}") "$work_dir/kept.env" >&2 \
  || fail 'env_kept_lines did not keep exactly the operator lines of a deployed .env, in order (diff above)'
if lib env_kept_lines "$work_dir/missing.env" >/dev/null 2>&1; then
  fail 'env_kept_lines succeeded without a deployed .env'
fi
# A line counts as systemd's EnvironmentFile= parser reads it (src/basic/env-file.c), which the
# recorder's environment comes from: past leading spaces and tabs, with spaces or tabs before the =,
# and after a carriage return, which ends a line as a line feed does. systemd keeps a variable's
# last value, so an indented GHOST_WISPR_ADDR=0.0.0.0:8080 kept after update.sh's own line would
# have the recorder listen past loopback, and an indented old key would be the key it uses. A
# comment, and a line inside another variable's quoted value, set nothing and stay. Breaks they
# catch: a filter that reads a line from its first byte, so an indented or a CR-joined line of an
# owned or a removed variable stays; one that reads each line alone, so it drops a line of a quoted
# value, and the lines after it then join that value.
printf '%s\n' '# an operator note' '  GHOST_WISPR_ADDR=0.0.0.0:8080' $'\tGHOST_WISPR_DEEPGRAM_API_KEY=dg-old' \
  'GHOST_WISPR_DISPATCH_TOKEN =tok-old' $' GHOST_WISPR_GCP_PROJECT\t= old-project' \
  $'GHOST_WISPR_MIC_DEVICE=hw:1\rGHOST_WISPR_ADDR=0.0.0.0:9090' '  # GHOST_WISPR_ADDR=0.0.0.0:7070, a comment' \
  'GHOST_WISPR_NOTE="a note' 'GHOST_WISPR_ADDR=inside the note"' 'GHOST_WISPR_JABRA_MUTE_BUTTON=true' >"$work_dir/indented.env"
lib env_kept_lines "$work_dir/indented.env" >"$work_dir/kept.env" || fail 'env_kept_lines refused an .env with indented lines'
# The CR-joined line keeps the microphone setting and its CR, which ends it as before.
printf '%s' '# an operator note' $'\n' 'GHOST_WISPR_MIC_DEVICE=hw:1' $'\r' '  # GHOST_WISPR_ADDR=0.0.0.0:7070, a comment' \
  $'\n' 'GHOST_WISPR_NOTE="a note' $'\n' 'GHOST_WISPR_ADDR=inside the note"' $'\n' 'GHOST_WISPR_JABRA_MUTE_BUTTON=true' \
  $'\n' >"$work_dir/kept.want"
cmp -s -- "$work_dir/kept.want" "$work_dir/kept.env" \
  || fail "env_kept_lines did not drop exactly the owned and removed variables systemd reads: $(od -c "$work_dir/kept.env")"
# env_secret_lines prints the two secret lines that systemd reads from an .env, each the last one of
# its variable, Deepgram's first: the lines a failed pi-secrets read keeps (R9).
lib env_secret_lines "$work_dir/indented.env" >"$work_dir/secret-lines" || fail 'env_secret_lines refused an .env'
diff <(printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-old 'GHOST_WISPR_DISPATCH_TOKEN =tok-old') "$work_dir/secret-lines" >&2 \
  || fail 'env_secret_lines did not print the secret lines systemd reads (diff above)'
printf '%s\n' GHOST_WISPR_DISPATCH_TOKEN=tok-first GHOST_WISPR_DEEPGRAM_API_KEY=dg-first '  GHOST_WISPR_DEEPGRAM_API_KEY=dg-last' \
  'GHOST_WISPR_NOTE="x' 'GHOST_WISPR_DISPATCH_TOKEN=inside the note"' >"$work_dir/twice.env"
diff <(printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-last GHOST_WISPR_DISPATCH_TOKEN=tok-first) <(lib env_secret_lines "$work_dir/twice.env") >&2 \
  || fail 'env_secret_lines did not print the last secret line of each variable that systemd reads (diff above)'
if lib env_secret_lines "$work_dir/missing.env" >/dev/null 2>&1; then
  fail 'env_secret_lines succeeded without a deployed .env'
fi

# ---------------------------------------------------------------------------------------------------
begin whole-set
# whole-set: a new release installs every file of its deploy set as one unit. A set installed in
# part would run a binary on another release's config, units or udev rule.
new_pi
make_release a "$commit_a" && pack_release a
make_release b "$commit_b" && pack_release b
install_set a "$drive_id"
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
save_installed "$work_dir/before-a"
release=b run_update
expect_run whole-set 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)" \
  'pi-secrets: GHOST_WISPR_DEEPGRAM_API_KEY from example-deepgram-secret version 3' \
  'pi-secrets: GHOST_WISPR_DISPATCH_TOKEN from example-dispatch-token-secret version 3'
expect_set_of whole-set b
# The config is the release's, rendered with the drive the Pi had, so sync and GC stay on; the
# office Pi's recording rules are in it exactly once (the check provision_run_test.sh made of the
# provisioner's config).
expect_config_with_drive whole-set "$releases/b/ghost-wispr-pi-deploy/ghost-wispr.yaml"
expect_recording_rules whole-set "$opt/ghost-wispr.yaml"
expect_env whole-set GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 GHOST_WISPR_JABRA_MUTE_BUTTON=false
# The .prev copies are the set verified last, the rendered config, .env and both system files
# included, so a rollback can restore all of it.
expect_prev_is whole-set "$work_dir/before-a"
expect_verified whole-set b
[[ ! -e $opt/ghost-wispr.yaml.before-release-config ]] \
  || fail 'whole-set: kept the config as before-release-config, which only a first install with no deploy.verified does'
# The recorder restarts once, after every file is in place and the user manager has read the new
# unit.
[[ $(restarts) == 1 ]] || fail "whole-set: restarted the recorder $(restarts) times: $(<"$state/systemctl.log")"
grep -Fqx "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$opt/.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" \
  "$state/systemctl.log" || fail "whole-set: the recorder restarted before its files were in place: $(<"$state/systemctl.log")"
last_reload=$(awk '/^restart / { exit } /^daemon-reload / { line = $0 } END { print line }' "$state/systemctl.log")
[[ $last_reload == "daemon-reload unit=$(sha "$units/ghost-wispr.service")" ]] \
  || fail "whole-set: the user manager did not read the new ghost-wispr.service before the restart: $(<"$state/systemctl.log")"
# The udev rule and the logind setting install with sudo -n, and udev reloads and logind restarts,
# each under its 30 s bound, after its file: udevadm control --reload before udevadm trigger.
rules_sum=$(sha "$(installed_path 99-ghost-wispr.rules)")
logind_sum=$(sha "$(installed_path logind-power-key.conf)")
for line in "timeout=30s udevadm control --reload rules=$rules_sum" "timeout=30s udevadm trigger rules=$rules_sum" \
  "timeout=30s systemctl restart systemd-logind logind=$logind_sum" 'timeout=10s tailscale serve status --json'; do
  grep -Fqx -- "$line" "$state/sudo.log" || fail "whole-set: sudo ran no $line: $(<"$state/sudo.log")"
done
# Each sudo -n install runs under its 30 s bound, as each sudo -n call does (R8).
for path in /etc/udev/rules.d/99-ghost-wispr.rules /etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf; do
  [[ $(grep -cFx -- "timeout=30s install $path" "$state/sudo.log") == 1 ]] \
    || fail "whole-set: sudo -n install did not install $path once, under its 30 s bound: $(<"$state/sudo.log")"
done
awk '/udevadm control --reload/ { r = NR } /udevadm trigger/ { t = NR } END { exit !(r && t && r < t) }' "$state/sudo.log" \
  || fail "whole-set: udev was not reloaded before its trigger: $(<"$state/sudo.log")"
# pi-secrets ran once, from the staged binary (the release's, checked against binary_sha256, not
# the installed one), with the credential file and the Pi's secret names, under its 60 s bound.
[[ $(grep -c '^call$' "$state/pi-secrets.log") == 1 ]] || fail "whole-set: pi-secrets ran $(grep -c '^call$' "$state/pi-secrets.log") times"
grep -Fqx "binary-sha $(sha "$releases/b/ghost-wispr-linux-arm64")" "$state/pi-secrets.log" \
  || fail "whole-set: pi-secrets did not run from release b's binary: $(<"$state/pi-secrets.log")"
if grep -Fqx "binary $opt/ghost-wispr" "$state/pi-secrets.log"; then
  fail 'whole-set: pi-secrets ran from the installed binary, before the new one was verified'
fi
grep -Fqx 'timeout 60s kill-after 5s' "$state/pi-secrets.log" \
  || fail "whole-set: pi-secrets did not run under timeout --kill-after=5s 60s: $(<"$state/pi-secrets.log")"
expect_secrets_flag whole-set --credentials "$opt/google-credentials.json"
expect_secrets_flag whole-set --project example-project
expect_secrets_flag whole-set --env-secret "GHOST_WISPR_DEEPGRAM_API_KEY""=example-deepgram-secret"
expect_secrets_flag whole-set --env-secret "GHOST_WISPR_DISPATCH_TOKEN""=example-dispatch-token-secret"
expect_secrets_flag whole-set --certificate-secret example-device-certificate
expect_secrets_flag whole-set --key "$device_key"
expect_secrets_flag whole-set --installed-certificate "$opt/device.crt"
grep -Fqx 'arg --out' "$state/pi-secrets.log" || fail 'whole-set: pi-secrets ran with no --out'
# No secret value reaches update.sh's output or an argument.
for value in dg-test-1 dg-test-2 tok-test-1 tok-test-2; do
  expect_no_output whole-set "$value"
  if grep -Fq -- "$value" "$state/pi-secrets.log"; then fail "whole-set: a secret value was an argument: $value"; fi
done
# No certificate was staged, so the installed one stays.
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' ]] || fail 'whole-set: replaced the certificate with none staged'
# The bootstrap files and Serve are the release's: no drift line.
expect_no_output whole-set 'differs from the release'
expect_no_output whole-set 'Serve:'
# Each call has its own bound (C3), so a stalled download never runs into the launcher's limit: the
# status and version reads 10 s, the release API 30 s, the tarball 60 s and its checksum 30 s, the
# binary 300 s and its checksum 60 s.
for kind in status version release tarball tarball-checksum binary binary-checksum; do
  declare "seen_${kind//-/_}=0"
done
while read -r max_time url; do
  case $url in
    */api/status) kind=status bound=10 ;;
    */api/version) kind=version bound=10 ;;
    */releases/latest) kind=release bound=30 ;;
    */ghost-wispr-pi-deploy.tar.gz) kind=tarball bound=60 ;;
    */ghost-wispr-pi-deploy.tar.gz.sha256) kind=tarball-checksum bound=30 ;;
    */ghost-wispr-linux-arm64) kind=binary bound=300 ;;
    */ghost-wispr-linux-arm64.sha256) kind=binary-checksum bound=60 ;;
    *) fail "whole-set: curl fetched $url" ;;
  esac
  [[ $max_time =~ ^[0-9]+$ ]] && ((max_time > 0 && max_time <= bound)) \
    || fail "whole-set: curl fetched $url with --max-time $max_time; want at most $bound"
  declare "seen_${kind//-/_}=1"
done <"$state/curl.log"
for kind in status version release tarball tarball-checksum binary binary-checksum; do
  seen="seen_${kind//-/_}"
  [[ ${!seen} == 1 ]] || fail "whole-set: made no $kind call: $(<"$state/curl.log")"
done

# The release as release.yml builds it: the real build-deploy-tarball.sh, run from a copy of the
# files it reads, as release_publish_test.sh runs it, packs the set that update.sh installs.
# make_release lays out each other release by this test's own list, so only here would update.sh
# and the builder disagree on the name of a file of the set; the Pi would then refuse each release.
mkdir -p -- "$work_dir/repo-copy/deploy" "$releases/built"
cp -pR -- "$repo_root/deploy/pi" "$work_dir/repo-copy/deploy/pi"
cp -p -- "$repo_root/wait-for-network.sh" "$work_dir/repo-copy/"
fake_binary "$commit_d" built >"$releases/built/ghost-wispr-linux-arm64"
chmod 0755 "$releases/built/ghost-wispr-linux-arm64"
printf '%s\n' "$commit_d" >"$releases/built/commit"
bash "$work_dir/repo-copy/deploy/pi/build-deploy-tarball.sh" "$commit_d" "$releases/built/ghost-wispr-linux-arm64" \
  "$releases/built" >"$work_dir/build.log" 2>&1 || fail "whole-set: build-deploy-tarball.sh failed: $(<"$work_dir/build.log")"
publish_release built
# The test's own checks read the set as the tarball holds it; update.sh reads the tarball.
tar -xzf "$releases/built/ghost-wispr-pi-deploy.tar.gz" -C "$releases/built"
new_pi
install_set a "$drive_id"
release=built run_update
expect_run 'whole-set (the built release)' 0 "ghost-wispr update: installed commit $commit_d (invocation test-invocation-7)"
expect_set_of 'whole-set (the built release)' built
expect_config_with_drive 'whole-set (the built release)' "$releases/built/ghost-wispr-pi-deploy/ghost-wispr.yaml"
expect_verified 'whole-set (the built release)' built
expect_no_output 'whole-set (the built release)' 'differs from the release'

begin system-files-unchanged
# system-files-unchanged: a release whose udev rule and logind setting are the installed ones
# installs neither and reloads neither: a udev trigger replays the device events, and a logind
# restart is a restart of the Pi's login manager, each for no change.
new_pi
same_system_files=1 make_release a "$commit_a" && pack_release a
same_system_files=1 make_release b "$commit_b" && pack_release b
install_set a
release=b run_update
expect_run system-files-unchanged 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
expect_set_of system-files-unchanged b
if grep -Eq 'udevadm|systemd-logind| install ' "$state/sudo.log"; then
  fail "system-files-unchanged: installed or reloaded an unchanged system file: $(<"$state/sudo.log")"
fi

begin env-lines
# env-lines: update.sh owns four .env lines, first and in C3's order; it keeps every operator line
# (the Jabra button switch, a microphone override) and drops the variables Ghost Wispr no longer
# reads, so an operator's override survives each release and no removed setting warns.
new_pi
make_release a "$commit_a" && pack_release a
make_release b "$commit_b" && pack_release b
install_set a
write_env "$opt/.env" '# set by the operator' GHOST_WISPR_DEEPGRAM_API_KEY=dg-old GHOST_WISPR_JABRA_MUTE_BUTTON=false \
  GHOST_WISPR_EMBEDDING_LOCATION=global GHOST_WISPR_ADDR=0.0.0.0:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=true \
  GHOST_WISPR_DISPATCH_TOKEN=tok-old GHOST_WISPR_MIC_DEVICE=hw:Loopback '  GHOST_WISPR_ADDR=0.0.0.0:9090' \
  $'\tGHOST_WISPR_DEEPGRAM_API_KEY=dg-older'
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
release=b run_update
expect_run env-lines 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
expect_env env-lines GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 '# set by the operator' \
  GHOST_WISPR_JABRA_MUTE_BUTTON=false GHOST_WISPR_MIC_DEVICE=hw:Loopback

begin local-settings-untouched
# local-settings-untouched: the local settings file holds what people saved on the Pi's page (D13).
# The release never ships it, and neither an install nor a rollback changes it or keeps a copy: a
# rollback that restored an older copy would undo a save.
new_pi
make_release a "$commit_a" && pack_release a
make_release b "$commit_b" && pack_release b
make_release broken "$commit_c"
fake_binary "$wrong_commit" broken >"$releases/broken/ghost-wispr-linux-arm64"
pack_release broken
install_set a
printf '%s\n' 'silence_timeout: 45s' 'jabra_mute_button: false' >"$opt/ghost-wispr.local.yaml"
chmod 0640 "$opt/ghost-wispr.local.yaml"
cp -p -- "$opt/ghost-wispr.local.yaml" "$work_dir/local.yaml"
release=b run_update
expect_run 'local-settings-untouched install' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
cmp -s -- "$work_dir/local.yaml" "$opt/ghost-wispr.local.yaml" && [[ $(stat -c %a -- "$opt/ghost-wispr.local.yaml") == 640 ]] \
  || fail 'local-settings-untouched: the install changed the local settings file'
release=broken run_update
expect_run 'local-settings-untouched rollback' 10
expect_line_like 'local-settings-untouched rollback' 'ghost-wispr update: .*; restored the previous release'
cmp -s -- "$work_dir/local.yaml" "$opt/ghost-wispr.local.yaml" && [[ $(stat -c %a -- "$opt/ghost-wispr.local.yaml") == 640 ]] \
  || fail 'local-settings-untouched: the rollback changed the local settings file'
if [[ -n $(find "$pi" -name '*local*' ! -path "$opt/ghost-wispr.local.yaml") ]]; then
  fail "local-settings-untouched: copied the local settings file: $(find "$pi" -name '*local*')"
fi

begin override-refused
# override-refused: a .env line, or the user manager's environment, that sets GHOST_WISPR_CONFIG or
# an Envoy variable would override the release's config, so the Pi would not run what the release
# says; update.sh refuses to install, changing nothing. The .env check reads a line past leading
# space and an export, and ends a line at a carriage return, as systemd's EnvironmentFile= parser
# does.
override_refused() {
  local what=$1 variable=$2 before
  before=$(snapshot)
  : >"$state/systemctl.log"
  release=b run_update
  expect_run "$what" 10
  expect_line_like "$what" "ghost-wispr update: .* sets ${variable}( [A-Z_]+)*, which would override the release's config; nothing was installed"
  expect_unchanged "$what" "$before"
  [[ $(restarts) == 0 ]] || fail "$what: restarted the recorder"
}
for env_line in GHOST_WISPR_CONFIG=/x '  export GHOST_WISPR_CONFIG=/x'; do
  new_pi
  make_release a "$commit_a" && pack_release a
  make_release b "$commit_b" && pack_release b
  install_set a
  printf '%s\n' "$env_line" >>"$opt/.env"
  override_refused "override-refused (.env line $env_line)" GHOST_WISPR_CONFIG
done
new_pi
install_set a
printf 'GHOST_WISPR_JABRA_MUTE_BUTTON=false\rGHOST_WISPR_ENVOY_HUB_URL=http://elsewhere.example:9020\n' >>"$opt/.env"
override_refused 'override-refused (an Envoy variable after a carriage return)' GHOST_WISPR_ENVOY_HUB_URL
new_pi
install_set a
printf '%s\n' HOME=/home/ghost-wispr PATH=/usr/bin:/bin GHOST_WISPR_ENVOY_ENABLED=false >"$state/manager-env"
override_refused "override-refused (the user manager's environment)" GHOST_WISPR_ENVOY_ENABLED
# An environment it cannot read is no reading at all: it refuses as for an override.
new_pi
install_set a
echo 1 >"$state/manager-env-exit"
before=$(snapshot)
release=b run_update
expect_run "override-refused (the user manager's environment unreadable)" 10
expect_unchanged "override-refused (the user manager's environment unreadable)" "$before"
# Control: a variable whose name only contains GHOST_WISPR_CONFIG overrides nothing, and installs.
new_pi
install_set a
printf '%s\n' MY_GHOST_WISPR_CONFIG=/x >>"$opt/.env"
printf '%s\n' HOME=/home/ghost-wispr PATH=/usr/bin:/bin MY_GHOST_WISPR_CONFIG=/x >"$state/manager-env"
release=b run_update
expect_run 'override-refused control' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
# The release current, with new secrets and a newer certificate to install: that refresh installs
# files and restarts the recorder too, so the refusal holds for it as for a release (R7). Break it
# catches: a refusal on the install path alone, so a refresh restarts the recorder on another config.
new_pi
install_set b
printf '%s\n' GHOST_WISPR_CONFIG=/x >>"$opt/.env"
mark_recorder_started
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
override_refused 'override-refused (the release current, new secrets and a certificate to install)' GHOST_WISPR_CONFIG

begin drop-in-refused
# drop-in-refused: a drop-in of ghost-wispr.service or ghost-wispr-update.service can set the
# recorder's environment past every check, or give the update a time limit that kills it mid-swap;
# update.sh refuses to install while one exists, changing nothing.
for unit in ghost-wispr.service ghost-wispr-update.service; do
  new_pi
  install_set a
  mkdir -p -- "$units/$unit.d"
  printf '[Service]\nEnvironment=GHOST_WISPR_LOG_LEVEL=debug\n' >"$units/$unit.d/override.conf"
  before=$(snapshot)
  release=b run_update
  expect_run "drop-in-refused ($unit)" 10 \
    "ghost-wispr update: $unit has a drop-in ($units/$unit.d/override.conf); nothing was installed"
  expect_unchanged "drop-in-refused ($unit)" "$before"
  [[ $(restarts) == 0 ]] || fail "drop-in-refused ($unit): restarted the recorder"
done
# Control: a drop-in directory with no .conf file in it is no drop-in, and the release installs.
new_pi
install_set a
mkdir -p -- "$units/ghost-wispr.service.d"
release=b run_update
expect_run 'drop-in-refused control' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
# The release current, with new secrets and a newer certificate to install: refused as well (R7).
for unit in ghost-wispr.service ghost-wispr-update.service; do
  what="drop-in-refused ($unit, the release current, new secrets and a certificate to install)"
  new_pi
  install_set b
  mkdir -p -- "$units/$unit.d"
  printf '[Service]\nEnvironment=GHOST_WISPR_LOG_LEVEL=debug\n' >"$units/$unit.d/override.conf"
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
  printf 'NEW CERTIFICATE\n' >"$state/certificate"
  before=$(snapshot)
  release=b run_update
  expect_run "$what" 10 "ghost-wispr update: $unit has a drop-in ($units/$unit.d/override.conf); nothing was installed"
  expect_unchanged "$what" "$before"
  [[ $(restarts) == 0 ]] || fail "$what: restarted the recorder"
done

# ---------------------------------------------------------------------------------------------------
begin rollback-whole-set
# rollback-whole-set: a new binary that never answers its commit is rolled back with its whole set:
# every file, the rendered config, .env, the unit and both system files back byte for byte, the user
# manager and udev reloaded and logind restarted again, and deploy.verified still the old set. A
# rollback of the binary alone would run the old binary on the new release's config, units and
# udev rule.
new_pi
install_set a "$drive_id"
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
save_installed "$work_dir/before-a"
cp -- "$opt/deploy.verified" "$work_dir/verified-a"
release=broken run_update
expect_run rollback-whole-set 10
expect_line_like rollback-whole-set 'ghost-wispr update: .*; restored the previous release'
expect_no_output rollback-whole-set 'installed commit'
expect_installed_is rollback-whole-set "$work_dir/before-a" "${set_files[@]}" ghost-wispr
# .env goes back byte for byte too (R6), before the rollback's restart. That restart's
# ExecStartPre, update.sh secrets, then writes the secret lines pi-secrets gives, read with the
# restored binary (R9), and keeps every other line of the restored .env: the recorder starts on a
# key a person replaced in Secret Manager, and on no line of the release rolled back.
[[ $(grep '^start-pre ' "$state/systemctl.log" | tail -n1) == "start-pre env=$(sha "$work_dir/before-a/.env")" ]] \
  || fail "rollback-whole-set: .env was not, byte for byte, the .env installed before when the rollback's restart began: $(<"$state/systemctl.log")"
secret_pattern='^(GHOST_WISPR_DEEPGRAM_API_KEY|GHOST_WISPR_DISPATCH_TOKEN)='
diff <(grep -Ev "$secret_pattern" "$work_dir/before-a/.env") <(grep -Ev "$secret_pattern" "$opt/.env") >&2 \
  || fail 'rollback-whole-set: the restart changed a line of the restored .env other than its secret lines (diff above)'
diff "$state/secrets" <(grep -E "$secret_pattern" "$opt/.env") >&2 \
  || fail 'rollback-whole-set: the secret lines of .env after the restart are not the ones pi-secrets gives (diff above)'
[[ $(stat -c %a -- "$opt/.env") == 600 ]] || fail "rollback-whole-set: .env is mode $(stat -c %a -- "$opt/.env"), want 600"
[[ $(awk '/^binary-sha / { sum = $2 } END { print sum }' "$state/start-pi-secrets.log") == "$(sha "$work_dir/before-a/ghost-wispr")" ]] \
  || fail "rollback-whole-set: the rollback's restart did not read the secrets with the restored binary: $(<"$state/start-pi-secrets.log")"
cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail 'rollback-whole-set: changed deploy.verified'
[[ $(restarts) == 2 ]] || fail "rollback-whole-set: restarted the recorder $(restarts) times, want the install's and the rollback's"
# The rollback's restart runs the old set, after the user manager read the old unit again, and udev
# and logind read the old system files again.
[[ $(grep '^restart ' "$state/systemctl.log" | tail -n1) == "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$opt/.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" ]] \
  || fail "rollback-whole-set: the rollback's restart did not run the restored set: $(<"$state/systemctl.log")"
awk -v want="daemon-reload unit=$(sha "$units/ghost-wispr.service")" \
  '/^daemon-reload / { reload = $0 } /^restart / { last = reload } END { exit !(last == want) }' "$state/systemctl.log" \
  || fail "rollback-whole-set: the user manager did not read the restored unit before the rollback's restart: $(<"$state/systemctl.log")"
rules_sum=$(sha "$(installed_path 99-ghost-wispr.rules)")
logind_sum=$(sha "$(installed_path logind-power-key.conf)")
[[ $(grep -c 'udevadm control --reload' "$state/sudo.log") == 2 && $(grep -c 'systemctl restart systemd-logind' "$state/sudo.log") == 2 ]] \
  || fail "rollback-whole-set: did not reload udev and restart logind for the install and again for the rollback: $(<"$state/sudo.log")"
[[ $(grep 'udevadm control --reload' "$state/sudo.log" | tail -n1) == "timeout=30s udevadm control --reload rules=$rules_sum" \
  && $(grep 'udevadm trigger' "$state/sudo.log" | tail -n1) == "timeout=30s udevadm trigger rules=$rules_sum" \
  && $(grep 'systemd-logind' "$state/sudo.log" | tail -n1) == "timeout=30s systemctl restart systemd-logind logind=$logind_sum" ]] \
  || fail "rollback-whole-set: udev and logind did not read the restored files: $(<"$state/sudo.log")"
# Each sudo -n install, the release's and the rollback's, ran under its 30 s bound.
for path in /etc/udev/rules.d/99-ghost-wispr.rules /etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf; do
  [[ $(grep -cFx -- "timeout=30s install $path" "$state/sudo.log") == 2 ]] \
    || fail "rollback-whole-set: sudo -n install did not install $path for the release and again for the rollback, each under its 30 s bound: $(<"$state/sudo.log")"
done

# A step after the swap that fails rolls the set back as a failed version check does: the install
# of a later file (the logind setting, after the udev rule and the user unit), the user manager's
# reload, udev's reload and logind's restart. Each run exits 10 with every file back byte for byte,
# deploy.verified as it was, no installed line, and udev, logind and the user manager reading the
# restored files again before the recorder restarts on them. Breaks they catch: an update that goes
# on after such a step failed, so the recorder runs a set the system never read and deploy.verified
# names it; a rollback that leaves the files swapped before the failed one.
# step_fails STEP FILE CONTENT WHY: $state/FILE holding CONTENT makes STEP fail once; the run fails
# with WHY.
step_fails() {
  local what="rollback-whole-set (the $1 fails)" last_logind
  new_pi
  install_set a
  save_installed "$work_dir/before-a"
  cp -- "$opt/deploy.verified" "$work_dir/verified-a"
  printf '%s\n' "$3" >"$state/$2"
  release=b run_update
  [[ ! -e $state/$2 ]] || fail "$what: the update never ran the step made to fail"
  expect_run "$what" 10 "ghost-wispr update: $4; restored the previous release"
  expect_no_output "$what" 'installed commit'
  expect_installed_is "$what" "$work_dir/before-a"
  cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail "$what: changed deploy.verified"
  [[ $(restarts) == 1 && $(grep '^restart ' "$state/systemctl.log") == "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$opt/.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" ]] \
    || fail "$what: did not restart the recorder once, on the restored set: $(<"$state/systemctl.log")"
  # The logind setting's install that fails leaves its file as it was, and logind restarts for
  # neither the release nor the rollback.
  last_logind=$(grep -v 'made to fail' "$state/sudo.log" | grep 'systemd-logind' | tail -n1 || true)
  [[ $(grep '^daemon-reload unit=' "$state/systemctl.log" | tail -n1) == "daemon-reload unit=$(sha "$units/ghost-wispr.service")" \
    && $(grep 'udevadm trigger' "$state/sudo.log" | tail -n1) == "timeout=30s udevadm trigger rules=$(sha "$(installed_path 99-ghost-wispr.rules)")" \
    && (-z $last_logind || $last_logind == "timeout=30s systemctl restart systemd-logind logind=$(sha "$(installed_path logind-power-key.conf)")") ]] \
    || fail "$what: the user manager, udev and logind did not read the restored files: $(cat -- "$state/systemctl.log" "$state/sudo.log")"
}
step_fails 'install of the logind setting' fail-sudo 'install /etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf' \
  "could not install the release's files"
# The first daemon-reload is the refusal check's, before any change; the second follows the swap.
step_fails "user manager's reload" fail-daemon-reload-at 2 'could not reload the user manager'
step_fails "udev reload" fail-sudo 'udevadm control --reload' 'could not reload udev or restart logind'
step_fails "logind restart" fail-sudo 'systemctl restart systemd-logind' 'could not reload udev or restart logind'

# A rollback puts the old binary back while the new one still runs: a copy over a file that is
# executing fails with "Text file busy", which would leave the new binary in place. The new binary
# here is a real executable that its failed start leaves running.
new_pi
make_release busy "$commit_c"
cp -- /bin/sleep "$releases/busy/ghost-wispr-linux-arm64"
pack_release busy
install_set a
save_installed "$work_dir/before-a"
echo busy-exec >"$state/restart-1"
release=busy run_update
stop_busy_binary
expect_run 'rollback over a running binary' 10
expect_line_like 'rollback over a running binary' 'ghost-wispr update: .*; restored the previous release'
expect_installed_is 'rollback over a running binary' "$work_dir/before-a"

# A new release whose start job fails while its unit stays active: the rollback restores every file
# of the set (R6), and then restarts the recorder only when the rule restartable allows it. A
# recorder that answers that it records a session or holds a call is left running, on the files it
# read at its start: a restart would end that recording or call. One whose status cannot be read (no
# answer, or an answer that is not a status) may be recording too: the rollback reads it again every
# 2 minutes and after the third read leaves it running. One that answers idle on two reads 2 minutes
# apart restarts on the restored set, and one whose unit stopped restarts at once, with no read.
# restart_fails HOW STATUS... runs release b on a Pi that runs a, whose first restart does HOW, and
# whose status program after it is STATUS.
restart_fails() {
  new_pi
  install_set a
  save_installed "$work_dir/before-a"
  echo "$1" >"$state/restart-1"
  shift
  printf '%s\n' "$@" >"$state/status.after-restart"
  release=b run_update
}
# left_running WHAT REASON SLEEPS READS
left_running() {
  expect_run "$1" 10
  [[ $out == *"$2"* && $out == *'left running'* ]] || fail "$1: want a failure naming \"$2\" that leaves it running; output: $out"
  expect_installed_is "$1" "$work_dir/before-a"
  [[ $(restarts) == 1 ]] || fail "$1: restarted the recorder again: $(<"$state/systemctl.log")"
  [[ $(<"$state/sleep.log") == "$3" && $(cat -- "$state/status.reads" 2>/dev/null || echo 0) == "$4" ]] \
    || fail "$1: read the status $(cat -- "$state/status.reads" 2>/dev/null || echo 0) times after waits $(printf %q "$(<"$state/sleep.log")"); want $4 reads after $(printf %q "$3")"
}
# rolled_back WHAT SLEEPS [LINE]: the run failed with a line like LINE, by default one that names the
# previous release restored, restored the set, and restarted the recorder after waits SLEEPS.
rolled_back() {
  expect_run "$1" 10
  expect_line_like "$1" "${3:-ghost-wispr update: .*; restored the previous release}"
  expect_installed_is "$1" "$work_dir/before-a"
  [[ $(restarts) == 2 ]] || fail "$1: want the rollback's restart: $(<"$state/systemctl.log")"
  [[ $(<"$state/sleep.log") == "$2" ]] || fail "$1: waited $(printf %q "$(<"$state/sleep.log")"), want $(printf %q "$2")"
}
restart_fails fail SESSION
left_running 'a new release that records a session' 'active session' '' 1
restart_fails fail '{"paused":false,"capture_state":"capturing","active_session_id":"","call":{"state":"live"}}'
left_running 'a new release that holds a call' 'call live' '' 1
restart_fails fail UNREADABLE
left_running 'a new release that does not answer' 'could not be read' $'sleep 120\nsleep 120' 3
restart_fails fail GARBAGE
left_running 'a new release that answers no status' 'could not be read' $'sleep 120\nsleep 120' 3
restart_fails fail UNREADABLE IDLE
rolled_back 'a new release that answers idle again' $'sleep 120\nsleep 120'
restart_fails fail-stop STOPPED
rolled_back 'a new release whose unit stopped' ''
# The version check needs a status too: within the same 120 s, /api/status answers a status whose
# call is absent or an object with a state, whether or not a session is in progress. A new release
# that answers its commit, but whose status holds a call that is not an object, or an object with no
# state, with no session or during one, or whose /api/status fails, is rolled back: every file back,
# and deploy.verified as it was. Breaks they catch: a check of /api/version alone, so deploy.verified
# names a release whose state no later run can read, and each later run stops at its first status
# read (a call it cannot read is busy, a status that fails a failure) before it could install a fix;
# a check that reads the session first, as busy_reason does, and so never reads the call while a
# session is in progress.
for answer in '{"paused":false,"capture_state":"capturing","active_session_id":"","call":"live"}|a call that is not an object' \
  '{"paused":false,"capture_state":"recording","active_session_id":"session-1","call":"live"}|a session and a call that is not an object' \
  '{"paused":false,"capture_state":"recording","active_session_id":"session-1","call":{"link_versions":[1]}}|a session and a call with no state' \
  'UNREADABLE|a failing /api/status'; do
  what="rollback-whole-set (a new release that answers its commit, with ${answer#*|})"
  new_pi
  install_set a
  save_installed "$work_dir/before-a"
  cp -- "$opt/deploy.verified" "$work_dir/verified-a"
  printf '%s\n' "${answer%%|*}" >"$state/status.after-restart"
  release=b run_update
  expect_run "$what" 10
  expect_line_like "$what" "ghost-wispr update: the new release did not answer /api/version with commit $commit_b and /api/status with a status it could read within 120 s; restored the previous release's files, .*"
  expect_no_output "$what" 'installed commit'
  expect_installed_is "$what" "$work_dir/before-a"
  cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail "$what: changed deploy.verified"
done
# Controls: a recorder that answers a session once it runs the new release, with no call, or with a
# call that is an object with a state, gives a status the update reads, and the release verifies.
for answer in 'SESSION|a session and no call' \
  '{"paused":false,"capture_state":"recording","active_session_id":"session-1","call":{"state":"live","link_versions":[1]}}|a session and a call with a state'; do
  new_pi
  install_set a
  printf '%s\n' "${answer%%|*}" >"$state/status.after-restart"
  release=b run_update
  expect_run "rollback-whole-set control (${answer#*|} after the restart)" 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
done

begin partial-install
# partial-install: an update cut short mid-swap (a power cut, or the launcher's time limit) leaves
# part of the new set installed while deploy.verified names the set before. The next run keeps the
# .prev copies of that verified set as they are, not the half-swapped files, and installs the whole
# set; a rollback then restores the verified set, not a mix, and says so.
partial_pi() {
  local file
  new_pi
  install_set a
  save_installed "$work_dir/before-a"
  for file in "${set_files[@]}" .env ghost-wispr; do
    cp -p -- "$(installed_path "$file")" "$opt/$file.prev"
  done
  # The swap had replaced the binary alone; the recorder still runs the old one.
  cp -- "$releases/$1/ghost-wispr-linux-arm64" "$opt/ghost-wispr"
}
partial_pi b
release=b run_update
expect_run partial-install 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
expect_set_of partial-install b
expect_verified partial-install b
expect_prev_is partial-install "$work_dir/before-a"
readonly last_verified_line='ghost-wispr update: .*; restored the last verified set, the one deploy\.verified lists'
partial_pi broken
release=broken run_update
rolled_back 'partial-install, then a release that fails' "$(printf 'sleep 2\n%.0s' {1..60} | head -c -1)" "$last_verified_line"
# An install of b cut short after it swapped in the binary and a new certificate, with its version
# file: the retry stages no certificate (pi-secrets finds the installed one current) and swaps in
# neither. A retry that finds the set not the verified one restores every file of the set and .env
# that has a .prev copy, and a busy Pi gets the same; each run exits 10 and says the last verified
# set went back. The certificate and its version file stay: a run puts them back only to undo its
# own swap of them, and pi-secrets found this one newer and signing in. Break it catches: a rollback
# that puts back device.crt.prev for a run that never swapped device.crt.
cut_short_pi() {
  local file
  new_pi
  install_set a
  printf '1\n' >"$opt/device.crt.version"
  save_installed "$work_dir/before-a"
  cp -- "$opt/deploy.verified" "$work_dir/verified-a"
  for file in "${set_files[@]}" .env ghost-wispr device.crt device.crt.version; do
    cp -p -- "$(installed_path "$file")" "$opt/$file.prev"
  done
  cp -- "$releases/b/ghost-wispr-linux-arm64" "$opt/ghost-wispr"
  printf 'NEW CERTIFICATE\n' >"$opt/device.crt"
  printf '2\n' >"$opt/device.crt.version"
}
cut_short_pi
touch -- "$state/version-mute.after-restart"
release=b run_update
what='partial-install, cut short after the binary and the certificate, then a retry that fails'
rolled_back "$what" "$(printf 'sleep 2\n%.0s' {1..60} | head -c -1)" "$last_verified_line"
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' ]] && cmp -s -- <(printf '2\n') "$opt/device.crt.version" \
  || fail "$what: device.crt is [$(<"$opt/device.crt")] and device.crt.version [$(<"$opt/device.crt.version")]; want the cut-short run's NEW CERTIFICATE and 2, which this run never swapped"
cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail "$what: changed deploy.verified"
what="partial-install, cut short after the binary and the certificate, then a session during the retry's reloads"
cut_short_pi
printf 'SESSION\n' >"$state/status.after-system-reload"
release=b run_update
expect_run "$what" 10
expect_line_like "$what" 'ghost-wispr update: active session; restored the last verified set, the one deploy\.verified lists.*'
[[ $(restarts) == 0 ]] || fail "$what: restarted the recorder: $(<"$state/systemctl.log")"
expect_installed_is "$what" "$work_dir/before-a" "${set_files[@]}" .env ghost-wispr
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' ]] && cmp -s -- <(printf '2\n') "$opt/device.crt.version" \
  || fail "$what: device.crt is [$(<"$opt/device.crt")] and device.crt.version [$(<"$opt/device.crt.version")]; want the cut-short run's NEW CERTIFICATE and 2, which this run never swapped"
cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail "$what: changed deploy.verified"
# A run that the launcher's limit killed, with SIGKILL past its 60 s grace, leaves its staging
# directory in the deploy directory, which can hold the secrets that run read. Each run removes, at
# its start, such a directory older than 46 minutes, the launcher's limit and its grace; a younger
# one may belong to a run still going, such as update.sh secrets in a start of the recorder, and
# stays, as does an old directory of another name. Breaks they catch: no clean-up, so each killed
# run leaves its secrets on the Pi for good; a clean-up that removes a directory a live run uses.
# stale_stages_pi makes a Pi on release b with a staging directory 47 minutes old that holds a
# secrets.env, one 45 minutes old, and a directory of another name 400 days old.
stale_stages_pi() {
  local now=${EPOCHREALTIME%[.,]*}
  new_pi
  install_set b
  mkdir -p -- "$opt/.ghost-wispr-update.K1lled/secrets" "$opt/.ghost-wispr-update.K1lled/new" \
    "$opt/.ghost-wispr-update.Y0ung1/new" "$opt/notes-2025"
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-stale >"$opt/.ghost-wispr-update.K1lled/secrets/secrets.env"
  touch -d "@$((now - 47 * 60))" -- "$opt/.ghost-wispr-update.K1lled"
  touch -d "@$((now - 45 * 60))" -- "$opt/.ghost-wispr-update.Y0ung1"
  touch -d "@$((now - 400 * 24 * 3600))" -- "$opt/notes-2025"
}
stale_stages_pi
release=b run_update
expect_run 'partial-install, staging directories left behind' 0 "ghost-wispr update: already current at commit $commit_b"
[[ ! -e $opt/.ghost-wispr-update.K1lled ]] \
  || fail 'partial-install: kept a staging directory 47 minutes old, with the secrets of the run that left it'
[[ -d $opt/.ghost-wispr-update.Y0ung1 && -d $opt/notes-2025 ]] \
  || fail "partial-install: removed a staging directory 45 minutes old, which a live run may use, or a directory of another name: $(cd -- "$opt" && find . -maxdepth 1 -type d | LC_ALL=C sort | paste -sd ' ')"

begin same-commit-different-set
# same-commit-different-set: deploy.verified names the release's commit, but an installed file of
# the set is not the one it lists (here pi.env, edited on the Pi): the release is not current, and
# the whole set installs again, so the Pi runs what the release says.
new_pi
install_set b
printf '# edited on the Pi\n' >>"$opt/pi.env"
release=b run_update
expect_run same-commit-different-set 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
expect_set_of same-commit-different-set b
# Control: the installed config alone differs from its template, which it does on every Pi with a
# shared drive, since it is rendered: the release is current, and nothing changes.
new_pi
install_set b "$drive_id"
before=$(snapshot)
release=b run_update
expect_run 'same-commit-different-set control' 0 "ghost-wispr update: already current at commit $commit_b"
expect_no_output 'same-commit-different-set control' 'ghost-wispr update: installed'
expect_unchanged 'same-commit-different-set control' "$before"
[[ $(restarts) == 0 ]] || fail 'same-commit-different-set control: restarted a current recorder'

begin transition
# transition: the office Pi's first run of this update.sh, after P2's provision.sh, finds no
# deploy.verified. Its old update.sh installed each release's binary nightly, so the binary may
# already answer the release's commit: the whole set installs all the same. The files it had become
# the .prev copies only when the recorder's running image is the installed binary, the one that
# answered its commit; its old config is kept as ghost-wispr.yaml.before-release-config, where a
# person finds a setting saved on the page before slice 1 (D13); and the old record
# ghost-wispr.verified goes.
old_pi() {
  local file
  new_pi
  # P2's provision.sh installed release b's update.sh, pi-lib.sh, pi.env and bootstrap files.
  install_set b
  rm -f -- "$opt/deploy.verified"
  # The old provisioner's config, with a page save in it, and its .env and other files.
  printf '%s\n' 'recording_rules:' '  - An old rule.' 'silence_timeout: 45s' "gdrive_folder_id: \"$drive_id\"" \
    'gdrive_sync_enabled: true' 'gc_enabled: true' >"$opt/ghost-wispr.yaml"
  write_env "$opt/.env" GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
    GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_EMBEDDING_LOCATION=global
  for file in wait-for-network.sh wait-for-time-sync.sh ghost-wispr.service 99-ghost-wispr.rules logind-power-key.conf; do
    printf '# the old %s\n' "$file" >"$(installed_path "$file")"
  done
  printf 'the binary the old update.sh verified last\n' >"$opt/ghost-wispr.prev"
  sha "$opt/ghost-wispr" >"$opt/ghost-wispr.verified"
  cp -p -- "$opt/ghost-wispr.yaml" "$work_dir/old-config.yaml"
}
old_pi
save_installed "$work_dir/before-old"
release=b run_update
expect_run transition 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
expect_set_of transition b
expect_config_with_drive transition "$releases/b/ghost-wispr-pi-deploy/ghost-wispr.yaml"
expect_prev_is transition "$work_dir/before-old"
cmp -s -- "$work_dir/old-config.yaml" "$opt/ghost-wispr.yaml.before-release-config" \
  || fail 'transition: did not keep the old config as ghost-wispr.yaml.before-release-config'
[[ ! -e $opt/ghost-wispr.verified ]] || fail 'transition: left the old ghost-wispr.verified'
expect_verified transition b
[[ $(restarts) == 1 ]] || fail "transition: restarted the recorder $(restarts) times"
# When the installed binary is not the image the recorder runs (an old update cut short after its
# swap), or that image cannot be read (no main process), nothing shows the installed files worked:
# the .prev copies stay as they are, and update.sh says why. The release still installs.
for main_pid in 4242 0; do
  old_pi
  if [[ $main_pid == 4242 ]]; then
    cp -- "$releases/a/ghost-wispr-linux-arm64" "$state/running-image"
  fi
  echo "$main_pid" >"$state/main-pid"
  release=b run_update
  what="transition, the running image not the installed binary (main process $main_pid)"
  expect_run "$what" 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
  [[ $out == *"is not the image the recorder runs (main process $main_pid)"* ]] || fail "$what: did not say why it kept no .prev: $out"
  [[ $(<"$opt/ghost-wispr.prev") == 'the binary the old update.sh verified last' ]] || fail "$what: replaced ghost-wispr.prev"
  for file in "${set_files[@]}" .env; do
    [[ ! -e $opt/$file.prev ]] || fail "$what: kept $file.prev from files nothing verified"
  done
  cmp -s -- "$work_dir/old-config.yaml" "$opt/ghost-wispr.yaml.before-release-config" \
    || fail "$what: did not keep the old config as ghost-wispr.yaml.before-release-config"
done
# A before-release-config a first try kept stays: a second try, after that one failed, must not
# replace the person's old settings with the release's config.
old_pi
printf 'the config before the first try\n' >"$opt/ghost-wispr.yaml.before-release-config"
release=b run_update
expect_run 'transition, a second try' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
[[ $(<"$opt/ghost-wispr.yaml.before-release-config") == 'the config before the first try' ]] \
  || fail 'transition, a second try: replaced the before-release-config a first try kept'
# The first run with no deploy.verified and the recorder stopped (a Pi that rebooted into a failed
# start, or a person's stop) cannot read which image runs. When ghost-wispr.verified, the old
# update.sh's record, names the installed binary, that binary ran and answered its commit, and the
# old update.sh's ghost-wispr.prev is the binary before it: the installed files are kept as the
# .prev copies, as for a recorder that runs the installed binary, and a rollback puts them back.
# When it names another binary, nothing shows which binary ghost-wispr.prev is: it goes before the
# swap, and a rollback leaves the binary as installed. Break it catches: a rollback that restores
# the old update.sh's leftover ghost-wispr.prev, a binary older than the one the Pi ran.
old_pi
rm -f -- "$state/active"
save_installed "$work_dir/before-old"
release=broken run_update
what='transition, the recorder stopped and ghost-wispr.verified naming the installed binary, then a release that fails'
cmp -s -- "$work_dir/before-old/ghost-wispr" "$opt/ghost-wispr" \
  || fail "$what: the installed binary is not the one the Pi had: $(head -n 3 -- "$opt/ghost-wispr" | paste -sd ' ')"
expect_run "$what" 10
expect_line_like "$what" 'ghost-wispr update: .*; restored the previous release'
expect_installed_is "$what" "$work_dir/before-old"
old_pi
rm -f -- "$state/active"
sha "$releases/a/ghost-wispr-linux-arm64" >"$opt/ghost-wispr.verified"
release=broken run_update
what='transition, the recorder stopped and ghost-wispr.verified naming another binary, then a release that fails'
[[ ! -e $opt/ghost-wispr.prev ]] && cmp -s -- "$releases/broken/ghost-wispr-linux-arm64" "$opt/ghost-wispr" \
  || fail "$what: want ghost-wispr.prev gone and the release's binary left installed; the installed binary: $(head -n 3 -- "$opt/ghost-wispr" | paste -sd ' ')"
expect_run "$what" 10
expect_line_like "$what" 'ghost-wispr update: ghost-wispr\.service is stopped, and .*/ghost-wispr\.verified does not name the installed binary, .*ghost-wispr\.prev.*'
expect_line_like "$what" 'ghost-wispr update: .*no previous release to restore.*'

begin no-binary-yet
# no-binary-yet: a new Pi as provision.sh leaves it before its first update: update.sh, pi-lib.sh,
# pi.env and the bootstrap files, the credentials and the certificate, and no binary, config, .env
# or ghost-wispr.service, with no recorder running. pi-secrets runs from the staged binary with the
# credential file; the drop-in check passes the unit that is not found yet; the whole set installs,
# the unit included.
new_pi
rm -f -- "$state/active"
for file in update.sh pi-lib.sh pi.env "${bootstrap_files[@]}"; do
  install -D -m "$(file_mode "$file")" -- "$releases/b/ghost-wispr-pi-deploy/$file" "$(installed_path "$file")"
done
release=b run_update
expect_run no-binary-yet 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)" \
  'pi-secrets: GHOST_WISPR_DEEPGRAM_API_KEY from example-deepgram-secret version 3'
expect_set_of no-binary-yet b
cmp -s -- "$releases/b/ghost-wispr-pi-deploy/ghost-wispr.yaml" "$opt/ghost-wispr.yaml" \
  || fail 'no-binary-yet: the config is not the template as it is, with no shared drive yet'
expect_env no-binary-yet GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1
expect_verified no-binary-yet b
grep -Fqx "binary-sha $(sha "$releases/b/ghost-wispr-linux-arm64")" "$state/pi-secrets.log" \
  || fail "no-binary-yet: pi-secrets did not run from the staged binary: $(<"$state/pi-secrets.log")"
expect_secrets_flag no-binary-yet --credentials "$opt/google-credentials.json"
[[ $(restarts) == 1 ]] || fail "no-binary-yet: started the recorder $(restarts) times"
[[ -z $(find "$opt" -name '*.prev') && ! -e $opt/ghost-wispr.yaml.before-release-config ]] \
  || fail "no-binary-yet: kept copies of files the Pi never had: $(find "$opt" -name '*.prev' -o -name '*.before-release-config')"
# When that first install's binary never answers its commit, no file of the set has a .prev copy:
# each file stays as installed, no deploy.verified is written, and the run fails and says so (R6).
new_pi
rm -f -- "$state/active"
for file in update.sh pi-lib.sh pi.env "${bootstrap_files[@]}"; do
  install -D -m "$(file_mode "$file")" -- "$releases/b/ghost-wispr-pi-deploy/$file" "$(installed_path "$file")"
done
release=broken run_update
expect_run 'no-binary-yet, a binary that never answers' 10
expect_line_like 'no-binary-yet, a binary that never answers' 'ghost-wispr update: .*no previous release to restore.*'
[[ ! -e $opt/deploy.verified ]] || fail 'no-binary-yet, a binary that never answers: wrote deploy.verified'
expect_set_of 'no-binary-yet, a binary that never answers' broken
[[ $(restarts) == 1 ]] || fail "no-binary-yet, a binary that never answers: restarted the recorder again with nothing restored: $(<"$state/systemctl.log")"

begin prev-copy-by-rename
# prev-copy-by-rename: bash reads a script while it runs it, so a file that a run replaces while a
# copy of it runs goes in place by a rename, which leaves the running copy reading its own file to
# its last line. The fallback, update.sh.prev, which the launcher runs when update.sh breaks,
# replaces neither update.sh.prev nor pi-lib.sh.prev (see the launcher case), and reads its own file
# to its end as it installs a release. update.sh itself reads its own file to its last line while
# the install renames the release's update.sh over it, and keeps the update.sh that ran as
# update.sh.prev, by a copy to a temporary name and a rename.
new_pi
make_release long "$commit_a"
{
  head -n1 "$repo_root/deploy/pi/update.sh"
  for ((i = 0; i < 400; i++)); do printf '# a longer update.sh than the copy that runs\n'; done
  tail -n +2 "$repo_root/deploy/pi/update.sh"
  printf '%s\n' "printf 'update.sh: ran to its own last line\\n'" '# release long'
} >"$releases/long/ghost-wispr-pi-deploy/update.sh"
pack_release long
install_set long
cp -- "$repo_root/deploy/pi/update.sh" "$opt/update.sh.prev"
printf '%s\n' "printf 'update.sh.prev: ran to its own last line\\n'" >>"$opt/update.sh.prev"
chmod 0755 "$opt/update.sh.prev"
cp -p -- "$opt/pi-lib.sh" "$opt/pi-lib.sh.prev"
cp -p -- "$opt/update.sh.prev" "$work_dir/fallback-update.sh"
cp -p -- "$opt/pi-lib.sh.prev" "$work_dir/fallback-pi-lib.sh"
updater=$opt/update.sh.prev release=b run_update
expect_run prev-copy-by-rename 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
[[ $(tail -n1 <<<"$out") == 'update.sh.prev: ran to its own last line' && $(grep -c 'installed commit' <<<"$out") == 1 ]] \
  || fail "prev-copy-by-rename: the running update.sh.prev did not read its own file to its end: $out"
expect_no_output prev-copy-by-rename 'already current'
cmp -s -- "$work_dir/fallback-update.sh" "$opt/update.sh.prev" && cmp -s -- "$work_dir/fallback-pi-lib.sh" "$opt/pi-lib.sh.prev" \
  || fail 'prev-copy-by-rename: the fallback replaced update.sh.prev or pi-lib.sh.prev, the scripts that ran'
expect_set_of prev-copy-by-rename b
new_pi
install_set long
release=b run_update
expect_run 'prev-copy-by-rename (update.sh)' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
[[ $(tail -n1 <<<"$out") == 'update.sh: ran to its own last line' ]] \
  || fail "prev-copy-by-rename: the running update.sh did not read its own file to its end: $out"
cmp -s -- "$releases/long/ghost-wispr-pi-deploy/update.sh" "$opt/update.sh.prev" \
  || fail 'prev-copy-by-rename: update.sh.prev is not the update.sh installed before'
expect_set_of 'prev-copy-by-rename (update.sh)' b

# ---------------------------------------------------------------------------------------------------
# busy-session and busy-call: while the recorder records a session, or holds a call (INFRA-5904's
# call.state checking or live), a restart would end it. Nothing changes, and the run exits 0, so the
# timer tries again 15 minutes later.
busy_cases=(
  'SESSION|active session'
  '{"paused":false,"capture_state":"capturing","active_session_id":"","call":{"state":"live"}}|call live'
  '{"paused":false,"capture_state":"capturing","active_session_id":"","call":{"state":"checking"}}|call checking'
)
for busy in "${busy_cases[@]}"; do
  if [[ ${busy#*|} == 'active session' ]]; then begin busy-session; else begin busy-call; fi
  new_pi
  install_set a
  printf '%s\n' "${busy%%|*}" >"$state/status"
  before=$(snapshot)
  release=b run_update
  expect_run "${busy#*|}" 0 "ghost-wispr update: ${busy#*|}; leaving the current release in place"
  expect_unchanged "${busy#*|}" "$before"
  [[ $(restarts) == 0 ]] || fail "${busy#*|}: restarted the recorder"
done
# Control: a call whose state is none holds nothing, and the release installs.
begin busy-call
new_pi
install_set a
printf '%s\n' '{"paused":false,"capture_state":"capturing","active_session_id":"","call":{"state":"none"}}' >"$state/status"
release=b run_update
expect_run 'control, call none' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
# A recorder that runs but gives no status may be recording: the run fails and changes nothing.
begin busy-session
new_pi
install_set a
printf 'UNREADABLE\n' >"$state/status"
before=$(snapshot)
release=b run_update
expect_run 'no status' 10
expect_unchanged 'no status' "$before"

begin busy-during-download
# busy-during-download: a session or a call that starts while the release downloads stops the run
# before the swap: the second status read, just before it, finds it, and nothing is installed.
for busy in "${busy_cases[@]:0:2}"; do
  new_pi
  install_set a
  printf '%s\n' "${busy%%|*}" >"$state/status.after-download"
  before=$(snapshot)
  release=b run_update
  expect_run "busy-during-download (${busy#*|})" 0 "ghost-wispr update: ${busy#*|}; leaving the current release in place"
  expect_unchanged "busy-during-download (${busy#*|})" "$before"
  [[ $(restarts) == 0 ]] || fail "busy-during-download (${busy#*|}): restarted the recorder"
done
# One that starts during the swap or the reloads after it stops the run just before the restart:
# the status read there finds it (R7). Every file swapped in goes back as a rollback puts it, the
# user manager, udev and logind read the restored files again, and the recorder runs on,
# unrestarted: the Pi is as it was, the run exits 0, and the timer tries again. A recorder that runs
# but gives no status there may be recording too: the same, and the run fails. Breaks they catch: an
# update that reads the status before the swap alone, and restarts the recorder mid-session after
# the logind restart or the udev trigger; one that leaves the new files for the recorder's next
# start without deploy.verified.
for busy in "${busy_cases[@]:0:2}" 'UNREADABLE|ghost-wispr.service is active but /api/status gave no status'; do
  what="busy-during-download (${busy#*|}, during the reloads)"
  new_pi
  install_set a "$drive_id"
  save_installed "$work_dir/before-a"
  cp -- "$opt/deploy.verified" "$work_dir/verified-a"
  printf '%s\n' "${busy%%|*}" >"$state/status.after-system-reload"
  release=b run_update
  if [[ ${busy%%|*} == UNREADABLE ]]; then
    expect_run "$what" 10
    expect_line_like "$what" "ghost-wispr update: ${busy#*|}; restored the previous release's files.*"
  else
    expect_run "$what" 0 "ghost-wispr update: ${busy#*|}; leaving the current release in place"
  fi
  expect_no_output "$what" 'installed commit'
  [[ $(restarts) == 0 ]] || fail "$what: restarted the recorder: $(<"$state/systemctl.log")"
  expect_installed_is "$what" "$work_dir/before-a"
  cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail "$what: changed deploy.verified"
  [[ $(grep -c 'udevadm control --reload' "$state/sudo.log") == 2 && $(grep -c 'restart systemd-logind' "$state/sudo.log") == 2 \
    && $(grep 'udevadm trigger' "$state/sudo.log" | tail -n1) == "timeout=30s udevadm trigger rules=$(sha "$(installed_path 99-ghost-wispr.rules)")" \
    && $(grep 'systemd-logind' "$state/sudo.log" | tail -n1) == "timeout=30s systemctl restart systemd-logind logind=$(sha "$(installed_path logind-power-key.conf)")" ]] \
    || fail "$what: udev and logind did not read the restored files again: $(<"$state/sudo.log")"
  [[ $(grep '^daemon-reload ' "$state/systemctl.log" | tail -n1) == "daemon-reload unit=$(sha "$units/ghost-wispr.service")" ]] \
    || fail "$what: the user manager did not read the restored unit again: $(<"$state/systemctl.log")"
done

begin held-link-versions
# held-link-versions: a release whose link versions (manifest link_versions) share none with the
# versions the call bridge accepts (/api/status call.link_versions) could not link to the bridge:
# it is held, with nothing changed, and the run exits 0 (C6).
make_release linked "$commit_c"
link_status='{"paused":false,"capture_state":"capturing","active_session_id":"","call":{"state":"none","link_versions":[2]}}'
new_pi
install_set a
printf '%s\n' "$link_status" >"$state/status"
link_versions='[1]' pack_release linked
before=$(snapshot)
release=linked run_update
expect_run held-link-versions 0 'ghost-wispr update: held: the bridge accepts link versions [2], this release [1]'
expect_unchanged held-link-versions "$before"
[[ $(restarts) == 0 ]] || fail 'held-link-versions: restarted the recorder'
# Controls: a release that shares a version, one whose manifest has no link_versions, a status with
# no call, and a call with no link versions yet each hold nothing.
held_control() {
  new_pi
  install_set a
  printf '%s\n' "$2" >"$state/status"
  link_versions=$3 pack_release linked
  release=linked run_update
  expect_run "held-link-versions control ($1)" 0 "ghost-wispr update: installed commit $commit_c (invocation test-invocation-7)"
}
held_control 'a shared version' "$link_status" '[1,2]'
held_control 'no link_versions in the manifest' "$link_status" ''
held_control 'no call in the status' IDLE '[1]'
held_control 'no link versions in the call' \
  '{"paused":false,"capture_state":"capturing","active_session_id":"","call":{"state":"none","link_versions":[]}}' '[1]'
# A held release still lets the release the Pi runs take new secrets and a renewed certificate (R9,
# R11): the run refreshes them as for the release it runs, the one deploy.verified names, and
# changes no file of the deploy set. Break it catches: a hold that ends the run at once, so a Pi
# whose bridge waits for a newer release keeps a replaced key, or its certificate to its end.
new_pi
install_set a
printf '1\n' >"$opt/device.crt.version"
printf '%s\n' "$link_status" >"$state/status"
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
link_versions='[1]' pack_release linked
save_installed "$work_dir/before-a"
cp -- "$opt/deploy.verified" "$work_dir/verified-a"
release=linked run_update
what='held-link-versions, new secrets and a newer certificate'
expect_run "$what" 0 'ghost-wispr update: held: the bridge accepts link versions [2], this release [1]' \
  'ghost-wispr update: installed secrets' 'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
expect_no_output "$what" 'installed commit'
expect_installed_is "$what" "$work_dir/before-a" "${set_files[@]}" ghost-wispr
cmp -s -- "$work_dir/verified-a" "$opt/deploy.verified" || fail "$what: changed deploy.verified"
expect_env "$what" GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 GHOST_WISPR_JABRA_MUTE_BUTTON=false
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' ]] && cmp -s -- <(printf '2\n') "$opt/device.crt.version" \
  || fail "$what: did not install the newer certificate with its version"
[[ $(restarts) == 1 ]] && grep -Fqx "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$opt/.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" "$state/systemctl.log" \
  || fail "$what: did not restart the recorder once on the new .env and certificate: $(<"$state/systemctl.log")"
grep -Fqx "binary $opt/ghost-wispr" "$state/pi-secrets.log" \
  || fail "$what: pi-secrets did not run from the installed binary: $(<"$state/pi-secrets.log")"
# A drop-in refuses that refresh as it refuses an install (R7), and nothing changes.
new_pi
install_set a
mkdir -p -- "$units/ghost-wispr.service.d"
printf '[Service]\nEnvironment=GHOST_WISPR_CONFIG=/tmp/other.yaml\n' >"$units/ghost-wispr.service.d/override.conf"
printf '%s\n' "$link_status" >"$state/status"
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
link_versions='[1]' pack_release linked
before=$(snapshot)
release=linked run_update
what='held-link-versions, new secrets and a drop-in'
expect_run "$what" 10 "ghost-wispr update: ghost-wispr.service has a drop-in ($units/ghost-wispr.service.d/override.conf); nothing was installed"
expect_unchanged "$what" "$before"
[[ $(restarts) == 0 ]] || fail "$what: restarted the recorder"

begin bad-manifest
# bad-manifest: a release that does not verify installs nothing and fails: a file whose SHA-256 is
# not the manifest's, a file the manifest does not name, a manifest of another format or of another
# commit than the release's, a binary whose SHA-256 is not binary_sha256, a checksum asset that does
# not match its asset, or a release with no immutable commit. Any of them could install a set nobody
# built for this release.
zeros=$(printf '0%.0s' {1..64})
bad_manifest() {
  local what="bad-manifest ($1)" before
  before=$(snapshot)
  release=bad run_update
  expect_run "$what" 10
  expect_unchanged "$what" "$before"
  [[ $(restarts) == 0 ]] || fail "$what: restarted the recorder"
}
for filter in ".files[\"pi.env\"] = \"$zeros\"" 'del(.files["serve.conf"])' '.format = 2' ".commit = \"$commit_a\"" \
  ".binary_sha256 = \"$zeros\""; do
  new_pi
  install_set a
  make_release bad "$commit_c"
  manifest_filter=$filter pack_release bad
  bad_manifest "$filter"
done
for asset in ghost-wispr-pi-deploy.tar.gz ghost-wispr-linux-arm64; do
  new_pi
  install_set a
  make_release bad "$commit_c"
  pack_release bad
  printf '%s  %s\n' "$zeros" "$asset" >"$releases/bad/$asset.sha256"
  bad_manifest "a $asset.sha256 that does not match"
done
new_pi
install_set a
make_release bad "$commit_c"
pack_release bad
jq '.target_commitish = "main"' "$releases/bad/release.json" >"$releases/bad/release.json.new"
mv -f -- "$releases/bad/release.json.new" "$releases/bad/release.json"
bad_manifest 'a release whose target is a branch'

begin secrets-only-change
# secrets-only-change: the release is current, and pi-secrets reads new values (a key a person
# replaced in Secret Manager): .env gets them, its other lines kept and its old copy as .env.prev,
# and the recorder restarts to read them, once it is idle.
new_pi
install_set b
save_installed "$work_dir/before-b"
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
release=b run_update
expect_run secrets-only-change 0 "ghost-wispr update: already current at commit $commit_b" 'ghost-wispr update: installed secrets'
expect_env secrets-only-change GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 GHOST_WISPR_JABRA_MUTE_BUTTON=false
cmp -s -- "$work_dir/before-b/.env" "$opt/.env.prev" || fail 'secrets-only-change: .env.prev is not the .env before'
expect_set_of secrets-only-change b
[[ $(restarts) == 1 ]] && grep -Fq "env=$(sha "$opt/.env")" "$state/systemctl.log" \
  || fail "secrets-only-change: did not restart the recorder once on the new .env: $(<"$state/systemctl.log")"
# The release is current, so the installed binary reads the secrets.
grep -Fqx "binary $opt/ghost-wispr" "$state/pi-secrets.log" \
  || fail "secrets-only-change: pi-secrets did not run from the installed binary: $(<"$state/pi-secrets.log")"
expect_secrets_flag secrets-only-change --credentials "$opt/google-credentials.json"
# Busy when the run starts, or when the secrets are read: nothing changes, exit 0.
for hook in status status.after-secrets; do
  new_pi
  install_set b
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
  printf 'SESSION\n' >"$state/$hook"
  before=$(snapshot)
  release=b run_update
  expect_run "secrets-only-change, busy ($hook)" 0 'ghost-wispr update: active session; leaving the current release in place'
  expect_unchanged "secrets-only-change, busy ($hook)" "$before"
  [[ $(restarts) == 0 ]] || fail "secrets-only-change, busy ($hook): restarted the recorder"
done
# A session or a call that starts during the swap of the new .env and certificate, after the read
# just before it, stops the run just before the restart (R7): the status read there, which sees the
# new files in place, finds it. The files go back from their .prev copies, the recorder runs on,
# unrestarted, and the run exits 0. Break it catches: a refresh that reads the status before its
# swap alone, and restarts the recorder mid-session or mid-call.
write_env "$work_dir/refreshed.env" GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 GHOST_WISPR_JABRA_MUTE_BUTTON=false
printf 'NEW CERTIFICATE\n' >"$work_dir/new.crt"
for busy in "${busy_cases[@]:0:2}"; do
  what="secrets-only-change, ${busy#*|} during the swap"
  new_pi
  install_set b
  printf '1\n' >"$opt/device.crt.version"
  save_installed "$work_dir/before-b"
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
  cp -- "$work_dir/new.crt" "$state/certificate"
  # The run's first read and the read just before the swap find the Pi free; the next one, busy.
  printf '%s\n' IDLE IDLE "${busy%%|*}" >"$state/status"
  release=b run_update
  expect_run "$what" 0 "ghost-wispr update: ${busy#*|}; leaving the current release in place"
  [[ $(restarts) == 0 ]] || fail "$what: restarted the recorder: $(<"$state/systemctl.log")"
  [[ $(tail -n1 "$state/status-reads.log") == "status-read env=$(sha "$work_dir/refreshed.env") crt=$(sha "$work_dir/new.crt")" ]] \
    || fail "$what: the last status read was not just before the restart, with the new files in place: $(<"$state/status-reads.log")"
  expect_installed_is "$what" "$work_dir/before-b" .env device.crt device.crt.version
  expect_no_output "$what" 'installed secrets'
  expect_no_output "$what" 'installed certificate'
done
# A run killed after a refresh put in .env or the certificate, before the restart, leaves the
# recorder on the old ones, and the next run finds the files current. A run that finds .env or the
# certificate newer than the recorder's start (systemd's ExecMainStartTimestamp) restarts the
# recorder on them once it is free, and checks its version as a refresh does. Breaks they catch: a
# refresh whose cut-short swap leaves the recorder on an old key, or an old certificate, for good.
cut_short_refresh() {
  new_pi
  install_set b
  printf '1\n' >"$opt/device.crt.version"
  cp -p -- "$opt/.env" "$opt/.env.prev"
  cp -p -- "$opt/device.crt" "$opt/device.crt.prev"
  cp -p -- "$opt/device.crt.version" "$opt/device.crt.version.prev"
  save_installed "$work_dir/before-b"
  # pi-secrets reads what the cut-short refresh installed, so this run has nothing new to install.
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
  cp -p -- "$work_dir/refreshed.env" "$opt/.env"
  written_after_start "$opt/.env"
}
cut_short_refresh
release=b run_update
what='secrets-only-change, a refresh cut short before its restart'
expect_run "$what" 0 "ghost-wispr update: already current at commit $commit_b" \
  'ghost-wispr update: the recorder started before the installed .env; restarting it'
[[ $(restarts) == 1 ]] && grep -Fqx "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$work_dir/refreshed.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" "$state/systemctl.log" \
  || fail "$what: did not restart the recorder once on the installed .env: $(<"$state/systemctl.log")"
cmp -s -- "$work_dir/refreshed.env" "$opt/.env" || fail "$what: changed .env"
# Once it restarted, the next run has nothing to do.
mark_recorder_started
: >"$state/systemctl.log"
rm -f -- "$state/restarts"
release=b run_update
expect_run "$what, the next run" 0 "ghost-wispr update: already current at commit $commit_b"
[[ $(restarts) == 0 ]] || fail "$what, the next run: restarted the recorder again"
# Busy: nothing changes, and the run exits 0.
cut_short_refresh
printf '%s\n' IDLE SESSION >"$state/status"
before=$(snapshot)
release=b run_update
expect_run "$what, busy" 0 'ghost-wispr update: active session; leaving the current release in place'
expect_unchanged "$what, busy" "$before"
[[ $(restarts) == 0 ]] || fail "$what, busy: restarted the recorder"
# The certificate too; when the recorder does not answer after that restart, the certificate, its
# version file and .env go back from their .prev copies and the run fails.
cut_short_refresh
cp -p -- "$work_dir/new.crt" "$opt/device.crt"
printf '2\n' >"$opt/device.crt.version"
written_after_start "$opt/device.crt"
touch -- "$state/version-mute.after-restart"
release=b run_update
expect_run "$what, then no answer" 10 \
  'ghost-wispr update: the recorder started before the installed .env device.crt; restarting it'
expect_line_like "$what, then no answer" 'ghost-wispr update: .*; restored the previous release'
expect_installed_is "$what, then no answer" "$work_dir/before-b" device.crt device.crt.version
# .env is the .prev copy when the rollback's restart begins; that restart's own secrets step then
# writes the secret lines pi-secrets gives, as each start does (R9).
[[ $(grep '^start-pre ' "$state/systemctl.log" | tail -n1) == "start-pre env=$(sha "$work_dir/before-b/.env")" ]] \
  || fail "$what, then no answer: .env was not its .prev copy when the rollback's restart began: $(<"$state/systemctl.log")"
# The start and the file times 1 ms apart on either side of a tenth of a second: update.sh compares
# them to the microsecond, so a .env 1 ms after the start restarts the recorder, and one 1 ms before
# it does not. Break it catches: a reading of the times through date's %6N, which uutils date
# (Ubuntu 26.04's date, and the GitHub runner's) prints wrong for a fraction under 0.1 s: it drops
# the fraction's leading zeros, so 0.099500 s reads as .995000 and 0.100500 s as .100500, and each
# of the two comparisons comes out the wrong way. (Two times with the same number of leading zeros,
# such as 0.050 s and 0.051 s, still compare the right way through it.)
# in_one_second START ENV: the recorder started START microseconds into a second, and .env was
# written ENV microseconds into the same second; device.crt a second before.
in_one_second() {
  local second
  new_pi
  install_set b
  second=$((${EPOCHREALTIME%[.,]*} - 100))
  set_recorder_start "$second" "$1"
  touch -d "@$((second - 1)).000000" -- "$opt/device.crt"
  touch -d "@$second.$2" -- "$opt/.env"
  release=b run_update
}
in_one_second 099500 100500
expect_run 'secrets-only-change, .env 1 ms after the start, across a tenth of a second' 0 \
  "ghost-wispr update: already current at commit $commit_b" 'ghost-wispr update: the recorder started before the installed .env; restarting it'
[[ $(restarts) == 1 ]] || fail "secrets-only-change, .env 1 ms after the start: restarted the recorder $(restarts) times, want 1"
in_one_second 100500 099500
expect_run 'secrets-only-change, .env 1 ms before the start, across a tenth of a second' 0 "ghost-wispr update: already current at commit $commit_b"
expect_no_output 'secrets-only-change, .env 1 ms before the start' 'restarting it'
[[ $(restarts) == 0 ]] || fail "secrets-only-change, .env 1 ms before the start: restarted a recorder that started on it"

begin secrets-read-fails
# secrets-read-fails: pi-secrets fails (Secret Manager down, or a grant gone): .env keeps the secret
# lines it has, a new release still installs, and the run fails, so the failure shows.
new_pi
install_set a
echo fail >"$state/pi-secrets-mode"
release=b run_update
expect_run secrets-read-fails 10 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)" \
  'pi-secrets: could not read GHOST_WISPR_DEEPGRAM_API_KEY from example-deepgram-secret'
expect_set_of secrets-read-fails b
expect_env secrets-read-fails GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 GHOST_WISPR_JABRA_MUTE_BUTTON=false
# The secret lines it keeps are the ones systemd reads from .env: the last line of each variable,
# an indented one too. Break it catches: a fallback that keeps the first unindented line, so a key
# the recorder never used becomes the one it uses.
new_pi
install_set a
write_env "$opt/.env" GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 GHOST_WISPR_JABRA_MUTE_BUTTON=false \
  '  GHOST_WISPR_DEEPGRAM_API_KEY=dg-indented'
echo fail >"$state/pi-secrets-mode"
release=b run_update
expect_run 'secrets-read-fails, an indented key' 10 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
expect_env 'secrets-read-fails, an indented key' GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-indented GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 GHOST_WISPR_JABRA_MUTE_BUTTON=false
# The release current: nothing changes, and the run fails.
new_pi
install_set b
echo fail >"$state/pi-secrets-mode"
before=$(snapshot)
release=b run_update
expect_run 'secrets-read-fails, current' 10
expect_unchanged 'secrets-read-fails, current' "$before"
[[ $(restarts) == 0 ]] || fail 'secrets-read-fails, current: restarted the recorder'
# A failed key read still lets the certificate step run (R10): a certificate it staged installs,
# with the release or alone, while .env keeps its secret lines and the run still fails.
for pi_release in a b; do
  what="secrets-read-fails, a certificate staged, the Pi on release $pi_release"
  new_pi
  install_set "$pi_release"
  echo fail >"$state/pi-secrets-mode"
  printf 'NEW CERTIFICATE\n' >"$state/certificate"
  release=b run_update
  expect_run "$what" 10 'pi-secrets: could not read GHOST_WISPR_DEEPGRAM_API_KEY from example-deepgram-secret' \
    'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
  [[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' && $(<"$opt/device.crt.prev") == 'INSTALLED CERTIFICATE' ]] \
    || fail "$what: did not install the staged certificate with the old one as device.crt.prev"
  expect_env "$what" GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
    GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 GHOST_WISPR_JABRA_MUTE_BUTTON=false
  expect_set_of "$what" b
  [[ $(restarts) == 1 ]] && grep -Fq "crt=$(sha "$opt/device.crt")" "$state/systemctl.log" \
    || fail "$what: did not restart the recorder once on the new certificate: $(<"$state/systemctl.log")"
done

begin pi-secrets-hangs
# pi-secrets-hangs: a pi-secrets that hangs (a stalled connection) is killed by its bound, 60 s and
# a 5 s grace, and reported through fail; .env keeps its lines. With no bound it would hold the
# whole run until the launcher's limit.
new_pi
install_set b
echo hang >"$state/pi-secrets-mode"
before=$(snapshot)
release=b run_update
expect_run pi-secrets-hangs 10
expect_unchanged pi-secrets-hangs "$before"
[[ $(restarts) == 0 ]] || fail 'pi-secrets-hangs: restarted the recorder'
elapsed=$(($(<"$state/clock") - 1000))
((elapsed >= 60 && elapsed <= 65)) || fail "pi-secrets-hangs: the run took $elapsed s of the test's clock, want 60 to 65"
grep -Eq '^kill-after=5s 60s .* pi-secrets ' "$state/timeout.log" \
  || fail "pi-secrets-hangs: pi-secrets did not run under timeout --kill-after=5s 60s: $(<"$state/timeout.log")"

begin certificate
# certificate: pi-secrets stages a newer certificate that signs in: it is installed with the old
# one as device.crt.prev, and the recorder restarts on it. When the recorder does not answer after
# that restart, the old certificate is put back and the run fails: a certificate that cannot sign in
# would cut the recorder off from every Google service. Beside device.crt, device.crt.version holds
# the version installed, which goes to pi-secrets as --installed-certificate-version (T1.1's
# contract), and goes in and back with the certificate. Breaks they catch: a version file left at an
# old version, so pi-secrets stages the installed certificate again each night; one a rollback does
# not restore, so pi-secrets takes the certificate that failed as installed.
new_pi
install_set b
printf '1\n' >"$opt/device.crt.version"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=b run_update
expect_secrets_flag certificate --installed-certificate-version 1
expect_run certificate 0 "ghost-wispr update: already current at commit $commit_b" \
  'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' && $(<"$opt/device.crt.prev") == 'INSTALLED CERTIFICATE' ]] \
  || fail 'certificate: did not install the staged certificate with the old one as device.crt.prev'
[[ $(restarts) == 1 ]] && grep -Fq "crt=$(sha "$opt/device.crt")" "$state/systemctl.log" \
  || fail "certificate: did not restart the recorder once on the new certificate: $(<"$state/systemctl.log")"
expect_no_output certificate 'installed secrets'
cmp -s -- <(printf '2\n') "$opt/device.crt.version" && cmp -s -- <(printf '1\n') "$opt/device.crt.version.prev" \
  && [[ $(stat -c %a -- "$opt/device.crt.version") == 644 ]] \
  || fail "certificate: device.crt.version is not 2, mode 644, with 1 as its .prev: $(cat -A -- "$opt/device.crt.version"*)"
# A version file that does not hold one positive integer passes no version: pi-secrets then reads
# the installed certificate alone.
for version in missing '' 0 01 abc '1 2' $'1\n2' -3; do
  new_pi
  install_set b
  if [[ $version != missing ]]; then printf '%s\n' "$version" >"$opt/device.crt.version"; fi
  release=b run_update
  expect_run "certificate (version file $(printf %q "$version"))" 0 "ghost-wispr update: already current at commit $commit_b"
  if secrets_args | grep -Fqx -- --installed-certificate-version; then
    fail "certificate (version file $(printf %q "$version")): passed --installed-certificate-version: $(secrets_args | paste -sd ' ')"
  fi
done
new_pi
install_set b
printf '1\n' >"$opt/device.crt.version"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
touch -- "$state/version-mute.after-restart"
release=b run_update
expect_run 'certificate, the recorder does not answer' 10
expect_line_like 'certificate, the recorder does not answer' 'ghost-wispr update: .*; restored the previous release'
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' ]] || fail 'certificate, the recorder does not answer: did not put the old certificate back'
cmp -s -- <(printf '1\n') "$opt/device.crt.version" || fail 'certificate, the recorder does not answer: did not put device.crt.version back'
[[ $(restarts) == 2 ]] || fail "certificate, the recorder does not answer: want the rollback's restart: $(<"$state/systemctl.log")"
# The first renewal on a Pi with no device.crt.version, as provision.sh --certificate leaves it:
# device.crt and its version file are kept and swapped together, so device.crt.prev with no
# device.crt.version.prev means the Pi had no version file. Putting the Pi back removes the version
# file that run installed: a rollback after a recorder that does not answer, and a session that
# starts during the swap, which leaves the Pi as it was and exits 0 (R7). Breaks they catch: a
# rollback that leaves device.crt.version naming the certificate it took back, so pi-secrets takes
# the failed certificate as installed and never stages it again; a restore that counts the version
# file as a file it could not restore, so the busy run exits 10.
new_pi
install_set b
printf 'NEW CERTIFICATE\n' >"$state/certificate"
touch -- "$state/version-mute.after-restart"
release=b run_update
what='certificate, no version file before, the recorder does not answer'
expect_run "$what" 10
expect_line_like "$what" 'ghost-wispr update: .*; restored the previous release'
expect_no_output "$what" 'no previous release to restore'
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' && ! -e $opt/device.crt.version ]] \
  || fail "$what: device.crt is [$(<"$opt/device.crt")], device.crt.version $(cat -- "$opt/device.crt.version" 2>/dev/null || echo missing); want the old certificate and no version file"
[[ $(restarts) == 2 ]] || fail "$what: want the rollback's restart: $(<"$state/systemctl.log")"
new_pi
install_set b
printf 'NEW CERTIFICATE\n' >"$state/certificate"
# The run's first read and the read just before the swap find the Pi free; the next one, busy.
printf '%s\n' IDLE IDLE SESSION >"$state/status"
release=b run_update
what='certificate, no version file before, a session during the swap'
expect_run "$what" 0 'ghost-wispr update: active session; leaving the current release in place'
[[ $(restarts) == 0 ]] || fail "$what: restarted the recorder: $(<"$state/systemctl.log")"
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' && ! -e $opt/device.crt.version ]] \
  || fail "$what: device.crt is [$(<"$opt/device.crt")], device.crt.version $(cat -- "$opt/device.crt.version" 2>/dev/null || echo missing); want the old certificate and no version file"
# release-and-certificate: a certificate staged while a release installs goes in with the set, and
# the release's one restart serves both (R11); when that release's version check fails, the old
# certificate comes back with the old set.
begin release-and-certificate
new_pi
install_set a
printf '1\n' >"$opt/device.crt.version"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=b run_update
expect_run release-and-certificate 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)" \
  'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' && $(<"$opt/device.crt.prev") == 'INSTALLED CERTIFICATE' ]] \
  || fail 'did not install the staged certificate with the old one as device.crt.prev'
cmp -s -- <(printf '2\n') "$opt/device.crt.version" && cmp -s -- <(printf '1\n') "$opt/device.crt.version.prev" \
  || fail 'release-and-certificate: did not install device.crt.version 2 with the release, with 1 as its .prev'
[[ $(restarts) == 1 ]] && grep -Fqx "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$opt/.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" \
  "$state/systemctl.log" || fail "the release and the certificate did not share one restart: $(<"$state/systemctl.log")"
new_pi
install_set a
printf '1\n' >"$opt/device.crt.version"
save_installed "$work_dir/before-a"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=broken run_update
expect_run 'release-and-certificate, a release that fails' 10
expect_line_like 'release-and-certificate, a release that fails' 'ghost-wispr update: .*; restored the previous release'
expect_installed_is 'release-and-certificate, a release that fails' "$work_dir/before-a"
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' ]] || fail 'a release that fails: did not restore device.crt with the set'
expect_installed_is 'release-and-certificate, a release that fails' "$work_dir/before-a" device.crt.version
[[ $(restarts) == 2 ]] || fail "a release that fails: want the install's and the rollback's restarts: $(<"$state/systemctl.log")"
# The same with no device.crt.version before, as the office Pi's first renewal may come with a
# release: the rollback removes the version file the release's swap installed (see certificate).
new_pi
install_set a
save_installed "$work_dir/before-a"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=broken run_update
what='release-and-certificate, no version file before, a release that fails'
expect_run "$what" 10
expect_no_output "$what" 'no previous release to restore'
expect_installed_is "$what" "$work_dir/before-a"
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' && ! -e $opt/device.crt.version ]] \
  || fail "$what: device.crt is [$(<"$opt/device.crt")], device.crt.version $(cat -- "$opt/device.crt.version" 2>/dev/null || echo missing); want the old certificate and no version file"
# A certificate goes back from device.crt.prev only to undo the run's own swap of it. A refresh of
# the release the Pi runs installed C1, with C0, the certificate before it, as device.crt.prev; then
# a newer release fails its version check, from a set that deploy.verified lists, and from one it
# does not (pi.env edited on the Pi), whose rollback restores each file of the set that has a .prev
# copy: device.crt stays C1, and device.crt.version 2. When that release stages C2 too, its rollback
# puts back C1, the certificate installed before it, not C0. Breaks they catch: a rollback that puts
# back device.crt.prev for a run that never swapped device.crt, so the Pi goes back to C0, the older
# certificate, nearer its end; a staged certificate that leaves an older device.crt.prev as it is.
# refreshed_certificate_pi WHAT: a Pi on release a, with .prev copies of its set as an install keeps
# them, whose refresh installed C1 (version 2) over C0 (version 1).
refreshed_certificate_pi() {
  local file
  new_pi
  install_set a
  printf '1\n' >"$opt/device.crt.version"
  for file in "${set_files[@]}" .env ghost-wispr; do
    cp -p -- "$(installed_path "$file")" "$opt/$file.prev"
  done
  printf 'C1\n' >"$state/certificate"
  release=a run_update
  expect_run "$1 (the refresh)" 0 'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
  [[ $(<"$opt/device.crt") == C1 && $(<"$opt/device.crt.prev") == 'INSTALLED CERTIFICATE' ]] \
    || fail "$1: the refresh did not install C1 with C0 as device.crt.prev"
  rm -f -- "$state/certificate"
}
for history in 'a set that deploy.verified lists' 'a set that deploy.verified does not list'; do
  what="release-and-certificate, a refresh of C1, then a release that fails, from $history"
  refreshed_certificate_pi "$what"
  if [[ $history == *'does not list' ]]; then printf '# edited on the Pi\n' >>"$opt/pi.env"; fi
  release=broken run_update
  expect_run "$what" 10
  expect_line_like "$what" 'ghost-wispr update: .*; restored .*'
  [[ $(<"$opt/device.crt") == C1 ]] && cmp -s -- <(printf '2\n') "$opt/device.crt.version" \
    || fail "$what: device.crt is [$(<"$opt/device.crt")] and device.crt.version [$(<"$opt/device.crt.version")]; want C1 and 2, which the refresh installed"
done
what='release-and-certificate, a refresh of C1, then a release that stages C2 and fails, from a set that deploy.verified does not list'
refreshed_certificate_pi "$what"
printf '# edited on the Pi\n' >>"$opt/pi.env"
printf 'C2\n' >"$state/certificate"
release=broken run_update
expect_run "$what" 10
[[ $(<"$opt/device.crt") == C1 ]] && cmp -s -- <(printf '2\n') "$opt/device.crt.version" \
  || fail "$what: device.crt is [$(<"$opt/device.crt")] and device.crt.version [$(<"$opt/device.crt.version")]; want C1 and 2, the certificate installed before the release"

begin secrets-mode
# secrets-mode: `update.sh secrets`, the recorder unit's ExecStartPre, writes the secret lines
# into .env and installs nothing else: no restart and no reload (the recorder is starting), no
# status or release read, no certificate. On a read failure it keeps .env and exits 10, and the
# recorder starts on the lines it has.
new_pi
install_set b
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 >"$state/secrets"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
run_update secrets
expect_run secrets-mode 0 'ghost-wispr update: installed secrets'
expect_env secrets-mode GHOST_WISPR_ADDR=127.0.0.1:8080 GOOGLE_API_USE_CLIENT_CERTIFICATE=false \
  GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-2 GHOST_WISPR_DISPATCH_TOKEN=tok-test-2 GHOST_WISPR_JABRA_MUTE_BUTTON=false
[[ ! -s $state/systemctl.log && ! -s $state/curl.log && ! -s $state/sudo.log ]] \
  || fail "secrets-mode: did more than write .env: $(cat -- "$state/systemctl.log" "$state/curl.log" "$state/sudo.log")"
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' ]] || fail 'secrets-mode: installed a certificate'
grep -Fqx "binary $opt/ghost-wispr" "$state/pi-secrets.log" && grep -Fqx 'timeout 60s kill-after 5s' "$state/pi-secrets.log" \
  || fail "secrets-mode: pi-secrets did not run from the installed binary under its bound: $(<"$state/pi-secrets.log")"
expect_secrets_flag secrets-mode --credentials "$opt/google-credentials.json"
# It removes a staging directory older than 46 minutes at its start too, and no younger one (see
# partial-install).
stale_stages_pi
run_update secrets
expect_run 'secrets-mode, staging directories left behind' 0
[[ ! -e $opt/.ghost-wispr-update.K1lled && -d $opt/.ghost-wispr-update.Y0ung1 && -d $opt/notes-2025 ]] \
  || fail "secrets-mode: did not remove the staging directory 47 minutes old alone: $(cd -- "$opt" && find . -maxdepth 1 -type d | LC_ALL=C sort | paste -sd ' ')"
new_pi
install_set b
echo fail >"$state/pi-secrets-mode"
before=$(snapshot)
run_update secrets
expect_run 'secrets-mode, a read failure' 10
expect_unchanged 'secrets-mode, a read failure' "$before"
[[ ! -s $state/systemctl.log ]] || fail "secrets-mode, a read failure: called systemctl: $(<"$state/systemctl.log")"

begin drift
# drift (D4): the bootstrap files and Serve are provision.sh's alone. Each run compares them with the
# release and says which differ, so a person sees that the Pi needs the release's provision.sh; it
# never changes them, and a difference changes no exit code.
mapfile -t serve_lines < <(grep -Ev '^[[:space:]]*(#|$)' "$repo_root/deploy/pi/files/serve.conf" | LC_ALL=C sort)
printf -v want_serve '%s; ' "${serve_lines[@]}"
want_serve=${want_serve%; }
new_pi
install_set b
release=b run_update
expect_run 'drift (none)' 0 "ghost-wispr update: already current at commit $commit_b"
expect_no_output 'drift (none)' 'differs from the release'
expect_no_output 'drift (none)' 'Serve'
for file in "${bootstrap_files[@]}"; do
  new_pi
  install_set b
  printf '# edited on the Pi\n' >>"$(installed_path "$file")"
  cp -p -- "$(installed_path "$file")" "$work_dir/edited"
  release=b run_update
  expect_run "drift ($file)" 0 "ghost-wispr update: provisioning file $file differs from the release; run the release's provision.sh"
  [[ $(grep -c 'differs from the release; run' <<<"$out") == 1 ]] || fail "drift ($file): named another file: $out"
  cmp -s -- "$work_dir/edited" "$(installed_path "$file")" || fail "drift ($file): changed the bootstrap file"
done
new_pi
install_set b
rm -f -- "$opt/ghost-wispr-update-launcher.sh"
release=b run_update
expect_run 'drift (no launcher)' 0 \
  "ghost-wispr update: provisioning file ghost-wispr-update-launcher.sh differs from the release; run the release's provision.sh"
[[ ! -e $opt/ghost-wispr-update-launcher.sh ]] || fail 'drift (no launcher): installed the launcher'
# After an install too.
new_pi
install_set a
printf '# edited on the Pi\n' >>"$units/ghost-wispr-update.timer"
release=b run_update
expect_run 'drift (after an install)' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)" \
  "ghost-wispr update: provisioning file ghost-wispr-update.timer differs from the release; run the release's provision.sh"
# Serve that proxies to another target, or serves nothing, differs. So does every other handler of
# an HTTPS port, each a line of its own (R16): files at /, files on another port too, a proxy at
# another path too, and a proxy at / that forwards peer capabilities. Breaks they catch: a Serve
# reader that reads the proxy at / alone, so a port or a path someone served by hand matches.
# Serve is never changed: no sudo tailscale call but the read.
[[ ${#serve_lines[@]} == 1 && ${serve_lines[0]} == '443 '* ]] \
  || fail "drift: the Serve cases below want serve.conf to name port 443 alone, not ${serve_lines[*]}"
good_serve_json=$(serve_json_for "$repo_root/deploy/pi/files/serve.conf")
# serve_with FILTER prints the repository's Serve with the jq FILTER applied; $host is the node.
serve_with() {
  jq -c --arg host ghost-wispr.example-tailnet.ts.net "$1" <<<"$good_serve_json"
}
printf '443 http://127.0.0.1:9999\n' >"$work_dir/other-serve.conf"
serve_drift() {
  new_pi
  install_set b
  printf '%s\n' "$2" >"$state/serve.json"
  release=b run_update
  expect_run "drift (Serve $1)" 0 "ghost-wispr update: Serve differs from the release: want $want_serve, have $3"
  [[ $(grep -c tailscale "$state/sudo.log") == 1 ]] && grep -Fqx 'timeout=10s tailscale serve status --json' "$state/sudo.log" \
    || fail "drift (Serve $1): ran tailscale other than to read Serve under its 10 s bound: $(<"$state/sudo.log")"
}
serve_drift 'with another target' "$(serve_json_for "$work_dir/other-serve.conf")" '443 http://127.0.0.1:9999'
serve_drift 'serving nothing' '{}' none
serve_drift 'with files at /' "$(serve_with '.Web[$host + ":443"].Handlers["/"] = {Path: "/var/www"}')" '443 "/" path "/var/www"'
serve_drift 'with files on 8443 too' \
  "$(serve_with '.TCP["8443"] = {HTTPS: true} | .Web[$host + ":8443"] = {Handlers: {"/": {Path: "/srv/www"}}}')" \
  "$want_serve; 8443 \"/\" path \"/srv/www\""
serve_drift 'with a proxy at another path too' \
  "$(serve_with '.Web[$host + ":443"].Handlers["/admin"] = {Proxy: "http://127.0.0.1:9000"}')" \
  "443 \"/admin\" proxy \"http://127.0.0.1:9000\"; $want_serve"
serve_drift 'whose proxy forwards peer capabilities' \
  "$(serve_with '.Web[$host + ":443"].Handlers["/"].AcceptAppCaps = ["example.com/cap/admin"]')" \
  "443 \"/\" proxy \"${serve_lines[0]#* }\" accept-app-caps [\"example.com/cap/admin\"]"
# A release whose serve.conf has two lines matches a Serve of both, and differs from a Serve of one.
make_release two-ports "$commit_b"
printf '9443 http://127.0.0.1:9081\n' >>"$releases/two-ports/ghost-wispr-pi-deploy/serve.conf"
pack_release two-ports
new_pi
install_set two-ports
serve_json_for "$releases/two-ports/ghost-wispr-pi-deploy/serve.conf" >"$state/serve.json"
release=two-ports run_update
expect_run 'drift (two Serve lines)' 0 "ghost-wispr update: already current at commit $commit_b"
expect_no_output 'drift (two Serve lines)' 'Serve'
new_pi
install_set two-ports
release=two-ports run_update
mapfile -t two_lines < <(grep -Ev '^[[:space:]]*(#|$)' "$releases/two-ports/ghost-wispr-pi-deploy/serve.conf" | LC_ALL=C sort)
printf -v want_two '%s; ' "${two_lines[@]}"
expect_run 'drift (one of two Serve lines)' 0 "ghost-wispr update: Serve differs from the release: want ${want_two%; }, have $want_serve"
# A Serve status it cannot read (tailscale fails, answers what is not one ServeConfig, or hangs past
# its 10 s bound) is UNKNOWN, never a match. So is a Serve of a shape the reader does not know: a
# handler that is no object, one of two kinds, a Web entry on a port that serves no HTTPS, an HTTPS
# port with no handler, a TCP forward, Funnel, a foreground Serve and a service. Breaks they catch:
# a reader that skips a handler it cannot read, so a Serve with one more port matches; one that
# reads the Web handlers alone, so a port forwarded, or opened to the internet, matches.
serve_unknown() {
  expect_run "drift (Serve $1)" 0
  expect_line_like "drift (Serve $1)" 'ghost-wispr update: Serve: UNKNOWN \(.+\)'
  expect_no_output "drift (Serve $1)" 'Serve differs'
}
new_pi
install_set b
echo 1 >"$state/serve-exit"
release=b run_update
serve_unknown 'failing'
unknown_answers=('not json' '[]')
for filter in \
  '.TCP["8443"] = {HTTPS: true} | .Web[$host + ":8443"] = {Handlers: {"/": "http://127.0.0.1:8081"}}' \
  '.Web[$host + ":443"].Handlers["/"].Path = "/srv/www"' \
  'del(.TCP)' \
  '.TCP["8443"] = {HTTPS: true}' \
  '.TCP["2222"] = {TCPForward: "127.0.0.1:22"}' \
  '.AllowFunnel = {($host + ":443"): true}' \
  '.Foreground = {"session-1": .}' \
  '.Services = {"svc:ghost-wispr": {Tun: true}}'; do
  unknown_answers+=("$(serve_with "$filter")")
done
for answer in "${unknown_answers[@]}"; do
  new_pi
  install_set b
  printf '%s\n' "$answer" >"$state/serve.json"
  release=b run_update
  serve_unknown "answering $answer"
done
new_pi
install_set b
touch -- "$state/serve-hang"
release=b run_update
serve_unknown 'hanging'
(($(<"$state/clock") - 1000 == 10)) || fail "drift (Serve hanging): the Serve read took $(($(<"$state/clock") - 1000)) s of the test's clock, want its 10 s bound"

# ---------------------------------------------------------------------------------------------------
# launcher: the fixed start of each update (R8). It prints the run's invocation and trigger, runs
# update.sh under its limit, and counts failed runs in update.failed-runs. It runs update.sh.prev at
# once when update.sh broke (an exit other than 0 or 10: a shell error, or the limit's 124 or 137),
# and once when the count of failed runs reaches 11, one night of the timer's runs; never after one
# exit 10, so a single network fault runs no update.sh.prev.
begin launcher
launcher_file=$repo_root/deploy/pi/files/ghost-wispr-update-launcher.sh
[[ $(head -n1 "$launcher_file") == '#!/bin/sh' ]] || fail 'the launcher does not start with #!/bin/sh'
(($(wc -l <"$launcher_file") < 50)) \
  || fail "the launcher has $(wc -l <"$launcher_file") lines, want fewer than 50: it is the one part of the update no release can fix"
# The Pi's /bin/sh is dash: the launcher runs under it here.
launcher_shell=$(command -v dash || command -v sh)
# fake_update FILE NAME writes a fake update.sh at FILE that logs NAME in launcher.log and exits
# $TEST_STATE/NAME.exit (0 by default), or hangs when that file says hang, or hangs and ignores
# SIGTERM when it says deaf.
fake_update() {
  cat >"$1" <<EOF
#!/usr/bin/env bash
printf '%s\n' '$2' >>"\$TEST_STATE/launcher.log"
code=\$(cat -- "\$TEST_STATE/$2.exit" 2>/dev/null || echo 0)
if [[ \$code == hang ]]; then exec /bin/sleep 60; fi
if [[ \$code == deaf ]]; then trap '' TERM; exec /bin/sleep 120; fi
exit "\$code"
EOF
  chmod 0755 "$1"
}
# launcher_pi makes a fake Pi with the launcher and a fake update.sh and update.sh.prev.
launcher_pi() {
  new_pi
  install -m 0755 -- "$launcher_file" "$opt/ghost-wispr-update-launcher.sh"
  fake_update "$opt/update.sh" update.sh
  fake_update "$opt/update.sh.prev" update.sh.prev
  : >"$state/launcher.log"
}
# run_launcher runs the installed launcher as ghost-wispr-update.service does, under $launcher_cmd
# (the Pi's sh by default), with the timer's TRIGGER_UNIT and TRIGGER_TIMER_REALTIME_USEC when
# $trigger is set, GHOST_WISPR_UPDATE_LIMIT=$limit when $limit is set, and the real timeout, or the
# fake one when $fake_timeout is set; $out and $status as run_update's, and $elapsed, the real
# seconds it took.
run_launcher() {
  local started=$SECONDS
  echo 1000 >"$state/clock"
  status=0
  # shellcheck disable=SC2086 # Each trigger word is one assignment or none.
  out=$(cd -- "$opt" && env -u TRIGGER_UNIT -u TRIGGER_TIMER_REALTIME_USEC -u GHOST_WISPR_UPDATE_LIMIT \
    PATH="$bin:${fake_timeout:+$timeout_bin:}$PATH" TEST_REAL_TIMEOUT="$real_timeout" \
    TEST_STATE="$state" TEST_RELEASES="$releases" TEST_RELEASE="${release:-none}" TEST_FAKES="$fakes" \
    TEST_PI_SECRETS="$fakes/pi-secrets" TEST_BUSY_PID="$work_dir/busy.pid" TEST_CURL_FAIL="${curl_fail:-}" \
    INVOCATION_ID=test-invocation-9 ${trigger:+TRIGGER_UNIT=ghost-wispr-update.timer} \
    ${trigger:+TRIGGER_TIMER_REALTIME_USEC=$trigger} \
    GHOST_WISPR_DEPLOY_DIR="$opt" GHOST_WISPR_UNIT_DIR="$units" GHOST_WISPR_SYSTEM_ROOT="$root" \
    GHOST_WISPR_PROC_DIR="$pi/proc" GHOST_WISPR_IDENTITY_KEY="$device_key" \
    GHOST_WISPR_RELEASE_API=https://test.invalid/releases/latest \
    GHOST_WISPR_STATUS_URL=http://127.0.0.1:8080/api/status \
    GHOST_WISPR_VERSION_URL=http://127.0.0.1:8080/api/version \
    ${limit:+GHOST_WISPR_UPDATE_LIMIT=$limit} \
    "${launcher_cmd:-$launcher_shell}" "$opt/ghost-wispr-update-launcher.sh" 2>&1) || status=$?
  elapsed=$((SECONDS - started))
  if [[ -s $state/errors ]]; then
    fail "the fakes saw what the update must never do: $(<"$state/errors"); its output: $out"
  fi
}
failed_runs() {
  cat -- "$opt/update.failed-runs" 2>/dev/null || echo none
}
# Its invocation line names the timer's trigger, or none for a start by hand; it exits as update.sh
# did, runs no update.sh.prev after an exit 0, and sets the count to 0.
launcher_pi
trigger=1764127800000000 run_launcher
[[ $status == 0 ]] && grep -Fqx 'ghost-wispr launcher: invocation test-invocation-9 trigger ghost-wispr-update.timer at 1764127800000000' <<<"$out" \
  || fail "a timer's run: exit $status, output $out"
[[ $(<"$state/launcher.log") == update.sh && $(failed_runs) == 0 ]] \
  || fail "a timer's run: ran $(<"$state/launcher.log") and left the count $(failed_runs)"
launcher_pi
run_launcher
grep -Fqx 'ghost-wispr launcher: invocation test-invocation-9 trigger none at none' <<<"$out" \
  || fail "a run by hand did not print trigger none at none: $out"
# Run as the unit runs it, by its #! line.
launcher_pi
launcher_cmd=env run_launcher
[[ $status == 0 && $(<"$state/launcher.log") == update.sh ]] || fail "run by its #! line: exit $status, output $out"
# An exit 10 is a failure update.sh found and reported: the launcher exits 10, counts it, and runs no
# update.sh.prev.
launcher_pi
echo 10 >"$state/update.sh.exit"
run_launcher
[[ $status == 10 && $(<"$state/launcher.log") == update.sh && $(failed_runs) == 1 ]] \
  || fail "after an exit 10: exit $status, ran $(<"$state/launcher.log"), count $(failed_runs)"
# network_fault WHAT GLOB LINE: the real update.sh, on an idle recorder whose status it reads, finds
# each call to a URL that GLOB matches failing as on a network fault. It prints LINE and exits 10;
# the launcher counts the run and runs no update.sh.prev, and nothing restarts.
network_fault() {
  new_pi
  install_set b
  install -m 0755 -- "$launcher_file" "$opt/ghost-wispr-update-launcher.sh"
  fake_update "$opt/update.sh.prev" update.sh.prev
  : >"$state/launcher.log"
  curl_fail=$2 release=b run_launcher
  [[ $status == 10 && ! -s $state/launcher.log && $(failed_runs) == 1 && $(restarts) == 0 ]] \
    || fail "$1: exit $status, ran $(<"$state/launcher.log"), count $(failed_runs), $(restarts) restarts, output $out"
  grep -Fqx -- "$3" <<<"$out" || fail "$1: printed no line \"$3\"; output: $out"
}
network_fault 'the release API unreachable' 'https://test.invalid/*' \
  'ghost-wispr update: could not read the latest release from https://test.invalid/releases/latest'
network_fault 'the tarball download failing' 'https://test.invalid/assets/*/ghost-wispr-pi-deploy.tar.gz' \
  'ghost-wispr update: could not download https://test.invalid/assets/b/ghost-wispr-pi-deploy.tar.gz'
# update.sh broke (exit 2, a shell error): update.sh.prev runs at once, and sources pi-lib.sh.prev,
# not the new pi-lib.sh.
new_pi
install_set b
install -m 0755 -- "$launcher_file" "$opt/ghost-wispr-update-launcher.sh"
cp -p -- "$opt/update.sh" "$opt/update.sh.prev"
{
  cat -- "$opt/pi-lib.sh"
  printf '%s\n' "printf 'pi-lib.sh.prev was sourced\\n'"
} >"$opt/pi-lib.sh.prev"
printf '%s\n' "printf 'pi-lib.sh was sourced\\n'" >>"$opt/pi-lib.sh"
fake_update "$opt/update.sh" update.sh
echo 2 >"$state/update.sh.exit"
printf 'SESSION\n' >"$state/status"
: >"$state/launcher.log"
release=b run_launcher
[[ $status == 0 && $out == *'pi-lib.sh.prev was sourced'* && $out != *'pi-lib.sh was sourced'* \
  && $out == *'ghost-wispr update: active session; leaving the current release in place'* ]] \
  || fail "after an exit 2: exit $status, output $out; want update.sh.prev run on pi-lib.sh.prev"
[[ $(<"$state/launcher.log") == update.sh && $(failed_runs) == 0 ]] \
  || fail "after an exit 2: ran $(<"$state/launcher.log"), count $(failed_runs)"
# Two releases in a row whose update.sh breaks (exit 2 at once): the fallback, the update.sh.prev
# of the release before them, installs the first, then the second, and stays the update.sh.prev,
# with its pi-lib.sh.prev. Break it catches: a fallback that replaces update.sh.prev and
# pi-lib.sh.prev with the copies it keeps, so the first bad release's update.sh becomes the
# fallback, and once the second installs no update runs at all.
commit_bad1=$(printf 'e%.0s' {1..40})
commit_bad2=$(printf 'f%.0s' {1..40})
make_release bad1 "$commit_bad1"
printf '%s\n' '#!/usr/bin/env bash' '# release bad1, whose update.sh breaks at once' 'exit 2' >"$releases/bad1/ghost-wispr-pi-deploy/update.sh"
pack_release bad1
make_release bad2 "$commit_bad2"
printf '%s\n' '#!/usr/bin/env bash' '# release bad2, whose update.sh breaks at once' 'exit 2' >"$releases/bad2/ghost-wispr-pi-deploy/update.sh"
pack_release bad2
new_pi
install_set bad1
install -m 0755 -- "$launcher_file" "$opt/ghost-wispr-update-launcher.sh"
cp -p -- "$releases/a/ghost-wispr-pi-deploy/update.sh" "$opt/update.sh.prev"
cp -p -- "$releases/a/ghost-wispr-pi-deploy/pi-lib.sh" "$opt/pi-lib.sh.prev"
release=bad2 run_launcher
what='two bad releases in a row, the first night'
[[ $status == 0 && $out == *'ghost-wispr launcher: update.sh exited 2; running update.sh.prev'* ]] \
  && grep -Fqx "ghost-wispr update: installed commit $commit_bad2 (invocation test-invocation-9)" <<<"$out" \
  || fail "$what: exit $status, output $out; want update.sh.prev to install release bad2"
cmp -s -- "$releases/a/ghost-wispr-pi-deploy/update.sh" "$opt/update.sh.prev" \
  && cmp -s -- "$releases/a/ghost-wispr-pi-deploy/pi-lib.sh" "$opt/pi-lib.sh.prev" \
  || fail "$what: the fallback replaced update.sh.prev or pi-lib.sh.prev; update.sh.prev now ends: $(tail -n 2 -- "$opt/update.sh.prev" | paste -sd ' ')"
expect_set_of "$what" bad2
release=b run_launcher
what='two bad releases in a row, the second night'
[[ $status == 0 && $out == *'ghost-wispr launcher: update.sh exited 2; running update.sh.prev'* ]] \
  && grep -Fqx "ghost-wispr update: installed commit $commit_b (invocation test-invocation-9)" <<<"$out" \
  || fail "$what: exit $status, output $out; want update.sh.prev to install release b"
expect_set_of "$what" b
# An update.sh that exits 10 on every run: the run at which the count reaches 11 runs update.sh.prev
# once, and the count goes back to 0, whatever update.sh.prev returned.
launcher_pi
echo 10 >"$state/update.sh.exit"
echo 10 >"$state/update.sh.prev.exit"
for ((run = 1; run <= 10; run++)); do
  run_launcher
  [[ $status == 10 && $(failed_runs) == "$run" ]] && ! grep -qx update.sh.prev "$state/launcher.log" \
    || fail "failed run $run: exit $status, count $(failed_runs), ran $(paste -sd ' ' "$state/launcher.log")"
done
run_launcher
[[ $(grep -cx update.sh "$state/launcher.log") == 11 && $(grep -cx update.sh.prev "$state/launcher.log") == 1 \
  && $(tail -n1 "$state/launcher.log") == update.sh.prev && $(failed_runs) == 0 ]] \
  || fail "the 11th failed run: count $(failed_runs), ran $(paste -sd ' ' "$state/launcher.log"); want update.sh.prev once and the count 0"
run_launcher
[[ $(failed_runs) == 1 && $(grep -cx update.sh.prev "$state/launcher.log") == 1 ]] \
  || fail "the run after: count $(failed_runs), ran $(paste -sd ' ' "$state/launcher.log")"
# An exit 0 sets the count to 0.
launcher_pi
echo 5 >"$opt/update.failed-runs"
run_launcher
[[ $status == 0 && $(failed_runs) == 0 ]] || fail "an exit 0 after 5 failed runs: exit $status, count $(failed_runs)"
# An update.sh that hangs: the limit (2 s here) ends it with 124, and update.sh.prev runs at once.
launcher_pi
echo hang >"$state/update.sh.exit"
limit=2s run_launcher
[[ $status == 0 && $(paste -sd ' ' "$state/launcher.log") == 'update.sh update.sh.prev' ]] && ((elapsed < 30)) \
  || fail "a hung update.sh: exit $status after $elapsed s, ran $(paste -sd ' ' "$state/launcher.log")"
# An update.sh that ignores the limit's SIGTERM (a step that traps it, or one stuck where no signal
# but SIGKILL reaches it): the 60 s grace of --kill-after=60s ends it with SIGKILL, exit 137, and
# update.sh.prev runs at once. Break it catches: a launcher with no grace, whose run never ends, so
# each later start of the timer joins it and no update runs again.
launcher_pi
echo deaf >"$state/update.sh.exit"
limit=1s run_launcher
[[ $status == 0 && $(paste -sd ' ' "$state/launcher.log") == 'update.sh update.sh.prev' \
  && $out == *'ghost-wispr launcher: update.sh exited 137; running update.sh.prev'* ]] && ((elapsed >= 60 && elapsed < 90)) \
  || fail "an update.sh that ignores SIGTERM: exit $status after $elapsed s, ran $(paste -sd ' ' "$state/launcher.log"), output $out"
# The production limit: with no GHOST_WISPR_UPDATE_LIMIT set, as the update unit runs the launcher,
# update.sh runs under 45 minutes and the 60 s grace (R8), which the fake timeout logs.
launcher_pi
fake_timeout=1 run_launcher
[[ $status == 0 ]] && grep -Fqx "kill-after=60s 45m $opt/update.sh" "$state/timeout.log" \
  || fail "the launcher with no GHOST_WISPR_UPDATE_LIMIT: exit $status, ran $(<"$state/timeout.log"); want timeout --kill-after=60s 45m"
# With no update.sh.prev, the launcher exits with update.sh's code; at 11 failed runs the count still
# goes back to 0 (R8), so each night counts its own runs.
launcher_pi
rm -f -- "$opt/update.sh.prev"
echo 2 >"$state/update.sh.exit"
run_launcher
[[ $status == 2 && $(failed_runs) == 1 ]] || fail "no update.sh.prev, an exit 2: exit $status, count $(failed_runs)"
launcher_pi
rm -f -- "$opt/update.sh.prev"
echo 10 >"$state/update.sh.exit"
echo 10 >"$opt/update.failed-runs"
run_launcher
[[ $status == 10 && $(failed_runs) == 0 && $out != *'running update.sh.prev'* ]] \
  || fail "no update.sh.prev, the 11th failed run: exit $status, count $(failed_runs), output $out; want exit 10 and the count 0"

# ---------------------------------------------------------------------------------------------------
# unit-limits: the update unit's one time limit is TimeoutStartSec=infinity, and it runs the
# launcher, whose limit is the one bound on a whole run (R8); a unit limit would kill update.sh
# mid-swap. The recorder unit reads the secrets after its two waits, and goes on when that step
# fails (the -), within its own start limit. The timer runs 11 times a night, 03:30 to 06:00.
begin unit-limits
update_unit=$repo_root/deploy/pi/files/ghost-wispr-update.service
limits=$(grep -E '^[[:space:]]*[A-Za-z]*(Timeout|RuntimeMax)[A-Za-z]*[[:space:]]*=' "$update_unit" || true)
[[ $limits == 'TimeoutStartSec=infinity' ]] \
  || fail "the update unit's time limits are $(printf %q "$limits"); want TimeoutStartSec=infinity alone"
[[ $(sed -n 's/^ExecStart=//p' "$update_unit") == /opt/ghost-wispr/ghost-wispr-update-launcher.sh ]] \
  || fail "the update unit runs $(sed -n 's/^ExecStart=//p' "$update_unit"), not the launcher"
# The update unit is Type=oneshot as systemd itself reads the file, so `systemctl start` (which
# provision.sh runs) waits for the whole run, and a start during a run joins it (R7). systemd's
# test mode loads the unit from a directory of its own and prints each setting as it reads it.
# Break it catches: a unit that runs the launcher as Type=simple, or with no Type, whose start
# returns at once while the update runs on.
systemd_binary=''
for candidate in /usr/lib/systemd/systemd /lib/systemd/systemd; do
  if [[ -x $candidate ]]; then systemd_binary=$candidate && break; fi
done
[[ -n $systemd_binary ]] || fail 'systemd (/usr/lib/systemd/systemd) is needed to read the update unit as systemd does'
mkdir -p -- "$work_dir/unit-dump"
cp -- "$update_unit" "$work_dir/unit-dump/"
SYSTEMD_UNIT_PATH="$work_dir/unit-dump:" "$systemd_binary" --test --system --no-pager --unit=ghost-wispr-update.service \
  >"$work_dir/unit-dump.out" 2>"$work_dir/unit-dump.err" \
  || fail "systemd --test could not read the update unit: $(<"$work_dir/unit-dump.err")"
# The dump heads each unit's section with a tab, an arrow and "Unit NAME:", and starts each line of
# the section with two tabs. The arrow is "->" or "→" by systemd's version and locale: systemd 255
# (Ubuntu 24.04) printed "->" here, and systemd 259 (Ubuntu 26.04, the GitHub runner) prints "→".
unit_dump=$(awk '/^\t(->|→) Unit ghost-wispr-update\.service:$/ { on = 1; next } on && !/^\t\t/ { exit } on' "$work_dir/unit-dump.out")
grep -Fqx -- $'\t\tFragment Path: '"$work_dir/unit-dump/ghost-wispr-update.service" <<<"$unit_dump" \
  || fail "systemd --test dumped no section of the update unit read from $work_dir/unit-dump: lines that name the unit: $(grep -n 'Unit ghost-wispr-update\.service:' "$work_dir/unit-dump.out" || echo none); Fragment Path lines that name the copy: $(grep -nF "$work_dir/unit-dump/" "$work_dir/unit-dump.out" || echo none); systemd's last lines: $(tail -n 5 "$work_dir/unit-dump.err")"
grep -Fqx -- $'\t\tType: oneshot' <<<"$unit_dump" && grep -Fqx -- $'\t\tTimeoutStartSec: infinity' <<<"$unit_dump" \
  || fail "systemd reads the update unit as $(grep -E '^[[:space:]]+(Type|TimeoutStartSec):' <<<"$unit_dump" | paste -sd ' '); want Type: oneshot and TimeoutStartSec: infinity"
recorder_unit=$repo_root/deploy/pi/files/ghost-wispr.service
mapfile -t pre < <(sed -n 's/^ExecStartPre=//p' "$recorder_unit")
[[ ${#pre[@]} == 3 && ${pre[0]} == /opt/ghost-wispr/wait-for-network.sh && ${pre[1]} == /opt/ghost-wispr/wait-for-time-sync.sh \
  && ${pre[2]} == '-/opt/ghost-wispr/update.sh secrets' ]] \
  || fail "the recorder unit's ExecStartPre lines are $(printf '%q ' "${pre[@]}"); want the two waits, then -/opt/ghost-wispr/update.sh secrets"
start_limit=$(sed -n 's/^TimeoutStartSec=//p' "$recorder_unit")
case $start_limit in
  *min) start_seconds=$((${start_limit%min} * 60)) ;;
  *s) start_seconds=${start_limit%s} ;;
  *) start_seconds=$start_limit ;;
esac
network_wait=$(sed -n 's/.*GHOST_WISPR_NETWAIT_TIMEOUT:-\([0-9]*\).*/\1/p' "$repo_root/wait-for-network.sh")
sync_wait=$(sed -n 's/.*GHOST_WISPR_TIMESYNC_TIMEOUT:-\([0-9]*\).*/\1/p' "$repo_root/deploy/pi/files/wait-for-time-sync.sh")
[[ $start_seconds =~ ^[0-9]+$ && $network_wait =~ ^[0-9]+$ && $sync_wait =~ ^[0-9]+$ ]] \
  || fail "could not read the recorder's start limit ($start_limit) or its waits ($network_wait, $sync_wait)"
# The network wait can end 5 s past its deadline (a 3 s probe and a 2 s pause), and the secrets step
# takes 65 s at most (60 s and the kill's 5 s).
((start_seconds >= network_wait + 5 + sync_wait + 65)) \
  || fail "the recorder's start limit, $start_seconds s, is shorter than its waits and the secrets step"
timer=$repo_root/deploy/pi/files/ghost-wispr-update.timer
mapfile -t calendars < <(sed -n 's/^OnCalendar=//p' "$timer")
((${#calendars[@]})) || fail 'the update timer has no OnCalendar'
command -v systemd-analyze >/dev/null || fail 'systemd-analyze is needed to read the timer schedule'
if ! analyzed=$(for calendar in "${calendars[@]}"; do TZ=UTC systemd-analyze calendar --iterations=40 "$calendar" || exit 1; done); then
  fail "systemd-analyze could not read the timer's OnCalendar lines: $(printf '%q ' "${calendars[@]}")"
fi
fires=$(awk '/(Next elapse|Iter(\.|ation) #[0-9]+):/ { print $(NF - 1) }' <<<"$analyzed" | LC_ALL=C sort -u)
want_fires=$(printf '%s\n' 03:30:00 03:45:00 04:00:00 04:15:00 04:30:00 04:45:00 05:00:00 05:15:00 05:30:00 05:45:00 06:00:00)
[[ $fires == "$want_fires" ]] || fail "the timer fires at $(paste -sd ' ' <<<"$fires"); want $(paste -sd ' ' <<<"$want_fires")"
grep -qx 'Persistent=true' "$timer" || fail 'the update timer is not Persistent=true'
# A restart that takes 10 hours, with every other step at its worst too, is never cut off: update.sh
# has a bound on each step and none on a whole run. It installs and verifies, or, when the new
# binary never reports its commit, rolls back once the recorder answers again.
new_pi
install_set a
slow=1 restart_seconds=36000 release=b run_update
expect_run 'a 10-hour restart' 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
(($(<"$state/clock") - 1000 > 36000)) || fail "control: the 10-hour restart took $(($(<"$state/clock") - 1000)) s of the clock"
new_pi
install_set a
save_installed "$work_dir/before-a"
printf '%s\n' UNREADABLE IDLE >"$state/status.after-restart"
slow=1 restart_seconds=36000 release=broken run_update
# The version wait's ten 2 s pauses between its 10 s reads, then the rollback's two status waits.
rolled_back 'a 10-hour restart, then a release that never reports its commit' \
  "$(printf 'sleep 2\n%.0s' {1..10}; printf 'sleep 120\nsleep 120')"

for name in "${case_names[@]}"; do
  [[ $ran_cases == *" $name "* ]] || fail "the case $name of c-update never ran"
done
printf 'update tests passed: %d cases\n' "${#case_names[@]}"
