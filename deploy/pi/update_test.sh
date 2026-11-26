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
readonly opt="$pi/opt" units="$pi/units" root="$pi/root" state="$pi/state" key="$pi/key.pem"
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
readonly commit_a commit_b commit_c wrong_commit
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
# Pi's state changes at that event (download, secrets or restart). A new status program starts again
# at its first line.
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
# recorder's unit is not active. Each call's --max-time and URL go to $TEST_STATE/curl.log. With
# TEST_SLOW=1 each call takes all of its --max-time on the test's clock, as a call at its worst;
# TEST_CURL_EXIT makes every call exit with that code.
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
if [[ -n ${TEST_CURL_EXIT:-} ]]; then
  exit "$TEST_CURL_EXIT"
fi
if [[ ${TEST_SLOW:-} == 1 && $max_time =~ ^[0-9]+$ ]]; then
  echo $(($(<"$state/clock") + max_time)) >"$state/clock"
fi

status_answer() {
  local reads line
  reads=$(($(cat -- "$state/status.reads" 2>/dev/null || echo 0) + 1))
  echo "$reads" >"$state/status.reads"
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
#   LoadState (loaded while the unit file is in the unit directory, else not-found) and DropInPaths
#   (each *.conf in the unit's .d directory there), as NAME=VALUE lines or, with --value, values.
# - show-environment: $TEST_STATE/manager-env, else a manager's few variables; it exits
#   $TEST_STATE/manager-env-exit (0 by default).
# - daemon-reload logs the recorder's unit file as it reads it.
# - restart ghost-wispr.service logs what the recorder starts on (the binary, .env, config and
#   certificate), takes TEST_RESTART_SECONDS of the test's clock, starts the installed binary
#   (its running image) and makes the unit active, then is the event "restart". The Nth restart
#   does what $TEST_STATE/restart-N says: ok (the default); fail, a start job that fails while
#   the unit stays active; fail-stop, one whose unit stops; busy-exec, a start job that fails while
#   the installed binary runs on as a real process.
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
    props=() value=false unit=''
    while (($#)); do
      case $1 in
        -p | --property) IFS=, read -ra more <<<"$2"; props+=("${more[@]}"); shift 2 ;;
        --property=*) IFS=, read -ra more <<<"${1#--property=}"; props+=("${more[@]}"); shift ;;
        --value) value=true; shift ;;
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
        *) unexpected show "$prop" ;;
      esac
      if $value; then printf '%s\n' "$v"; else printf '%s=%s\n' "$prop" "$v"; fi
    done
    ;;
  daemon-reload)
    printf 'daemon-reload unit=%s\n' "$(sum "$units/ghost-wispr.service")" >>"$state/systemctl.log"
    ;;
  restart)
    [[ $* == 'restart ghost-wispr.service' ]] || unexpected "$@"
    n=$(($(cat -- "$state/restarts" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$state/restarts"
    printf 'restart binary=%s env=%s config=%s crt=%s\n' "$(sum "$deploy/ghost-wispr")" "$(sum "$deploy/.env")" \
      "$(sum "$deploy/ghost-wispr.yaml")" "$(sum "$deploy/device.crt")" >>"$state/systemctl.log"
    echo $(($(<"$state/clock") + ${TEST_RESTART_SECONDS:-0})) >"$state/clock"
    if [[ -f $deploy/ghost-wispr ]]; then
      cp -- "$deploy/ghost-wispr" "$state/running-image"
    fi
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
# the logind restart with the logind setting, in $TEST_STATE/sudo.log.
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
    printf 'timeout=%s install %s\n' "$under" "${destination#"$system_root"}" >>"$state/sudo.log"
    exec install "${@:2}"
    ;;
  udevadm)
    printf 'timeout=%s udevadm %s rules=%s\n' "$under" "${*:2}" "$(sum "$system_root/etc/udev/rules.d/99-ghost-wispr.rules")" >>"$state/sudo.log"
    ;;
  systemctl)
    [[ ${*:2} == 'restart systemd-logind' ]] || unexpected "$@"
    printf 'timeout=%s systemctl restart systemd-logind logind=%s\n' "$under" \
      "$(sum "$system_root/etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf")" >>"$state/sudo.log"
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
# to $TEST_STATE/timeout.log.
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
# rest the binary's arguments. Each call goes to $TEST_STATE/pi-secrets.log: the binary, its
# SHA-256, the timeout it runs under and each argument. As C4's pi-secrets does, it writes
# --out/secrets.env, one --env-secret VARIABLE=SECRET a line in flag order (each VARIABLE's value
# from $TEST_STATE/secrets), stages $TEST_STATE/certificate as --out/device.crt when that file
# exists, and prints C4's lines, never a value. It is the event "secrets". $TEST_STATE/pi-secrets-mode
# "fail" makes it fail as a read failure does (exit 1, no file); "hang" makes it hang until a
# timeout ends it.
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
} >>"$state/pi-secrets.log"
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
    printf 'pi-secrets: certificate version 1 kept: not newer than the installed certificate\n'
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
# /api/version reads its "# answers commit" line) and runs the fake pi-secrets for pi-secrets.
fake_binary() {
  printf '#!/usr/bin/env bash\n# answers commit %s\n# release %s\n' "$1" "$2"
  printf '%s\n' 'if [[ ${1:-} == pi-secrets ]]; then exec "${TEST_PI_SECRETS:?}" "$0" "$@"; fi' 'exit 0'
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
# the jq filter $manifest_filter applied last; then its tarball, the two checksum assets, and the
# release API's answer, whose asset URLs the fake curl serves.
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
  printf '%s  %s\n' "$(sha "$dir/ghost-wispr-linux-arm64")" ghost-wispr-linux-arm64 >"$dir/ghost-wispr-linux-arm64.sha256"
  jq -n --arg commit "$commit" --arg base "https://test.invalid/assets/$name" '{target_commitish: $commit, assets: [
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
  printf 'DEVICE KEY\n' >"$key"
  printf '{"type":"external_account"}\n' >"$opt/google-credentials.json"
  printf 'INSTALLED CERTIFICATE\n' >"$opt/device.crt"
  printf 'IDLE\n' >"$state/status"
  touch -- "$state/active" "$state/systemctl.log" "$state/sudo.log" "$state/sleep.log" "$state/curl.log" \
    "$state/timeout.log" "$state/pi-secrets.log"
  serve_json_for "$repo_root/deploy/pi/files/serve.conf" >"$state/serve.json"
  printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-test-1 GHOST_WISPR_DISPATCH_TOKEN=tok-test-1 >"$state/secrets"
  echo 1000 >"$state/clock"
}

# install_set NAME [DRIVE] puts release NAME's deploy set in place on the fake Pi as update.sh
# leaves a set it installed and verified: each file where C2 puts it, the config rendered with the
# shared drive DRIVE when given, an .env of the four owned lines and one operator line, the
# bootstrap files as provision.sh installs them, deploy.verified naming the set, and the recorder
# running the installed binary.
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
}

# save_installed DIR copies what the fake Pi has installed of the deploy set, its rendered config,
# .env and binary included, into DIR, one file a name.
save_installed() {
  local dir=$1 file
  rm -rf -- "${dir:?}"
  mkdir -p -- "$dir"
  for file in "${set_files[@]}" .env ghost-wispr; do
    cp -p -- "$(installed_path "$file")" "$dir/$file"
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

# expect_installed_is WHAT DIR: each file of the deploy set, .env and the binary included, is
# installed again as save_installed copied it into DIR, byte for byte.
expect_installed_is() {
  local file path
  for file in "${set_files[@]}" .env ghost-wispr; do
    path=$(installed_path "$file")
    cmp -s -- "$2/$file" "$path" || fail "$1: $path is not, byte for byte, the $file installed before"
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
    GHOST_WISPR_PROC_DIR="$pi/proc" GHOST_WISPR_IDENTITY_KEY="$key" \
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
# about (provision.sh's REMOVED_ENV_VARIABLES).
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
# sourced it (the reason rollout.sh could not source provision.sh).
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

# The config renderer, moved from provision.sh's render_models_config and deployed_drive_id. With a
# shared drive it keeps every template setting but the drive's three, adds the quoted drive ID, and
# turns sync and GC on: GC removes only local audio that Drive holds a copy of.
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
# render_config keeps the drive the deployed config names, as render_models_config or yaml.v3 wrote
# it, so a release does not turn Drive sync and GC off.
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
# env_kept_lines, moved from provision.sh's render_models_env, keeps every line of a deployed .env
# in order, comments included, but the four lines update.sh owns (C3) and the variables Ghost Wispr
# no longer reads, each of which would warn at every start. It keeps the role overrides that share
# words with removed ones, and the operator's Jabra button line (docs/pi-setup.md).
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
# each under its 30 s bound, after its file (provision.sh's sequence).
rules_sum=$(sha "$(installed_path 99-ghost-wispr.rules)")
logind_sum=$(sha "$(installed_path logind-power-key.conf)")
for line in "timeout=30s udevadm control --reload rules=$rules_sum" "timeout=30s udevadm trigger rules=$rules_sum" \
  "timeout=30s systemctl restart systemd-logind logind=$logind_sum" 'timeout=10s tailscale serve status --json'; do
  grep -Fqx -- "$line" "$state/sudo.log" || fail "whole-set: sudo ran no $line: $(<"$state/sudo.log")"
done
grep -Eq '^timeout=[^ ]+ install /etc/udev/rules.d/99-ghost-wispr.rules$' "$state/sudo.log" \
  && grep -Eq '^timeout=[^ ]+ install /etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf$' "$state/sudo.log" \
  || fail "whole-set: the system files were not installed through sudo -n install: $(<"$state/sudo.log")"
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
expect_secrets_flag whole-set --key "$key"
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
  GHOST_WISPR_DISPATCH_TOKEN=tok-old GHOST_WISPR_MIC_DEVICE=hw:Loopback
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
expect_installed_is rollback-whole-set "$work_dir/before-a"
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
# rolled_back WHAT SLEEPS
rolled_back() {
  expect_run "$1" 10
  expect_line_like "$1" 'ghost-wispr update: .*; restored the previous release'
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

begin partial-install
# partial-install: an update cut short mid-swap (a power cut, or the launcher's time limit) leaves
# part of the new set installed while deploy.verified names the set before. The next run keeps the
# .prev copies of that verified set as they are, not the half-swapped files, and installs the whole
# set; a rollback then restores the verified set, not a mix.
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
partial_pi broken
release=broken run_update
rolled_back 'partial-install, then a release that fails' "$(printf 'sleep 2\n%.0s' {1..60} | head -c -1)"

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
expect_no_output 'same-commit-different-set control' 'installed'
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
# prev-copy-by-rename: the launcher runs update.sh.prev when update.sh is broken, and that run
# replaces update.sh.prev with the set it keeps. bash reads a script while it runs it, so a copy
# over update.sh.prev in place would have the running copy go on in the middle of other text; a
# rename leaves it reading its own file to its last line.
new_pi
make_release long "$commit_a"
{
  head -n1 "$repo_root/deploy/pi/update.sh"
  for ((i = 0; i < 400; i++)); do printf '# a longer update.sh than the copy that runs\n'; done
  tail -n +2 "$repo_root/deploy/pi/update.sh"
  printf '# release long\n'
} >"$releases/long/ghost-wispr-pi-deploy/update.sh"
pack_release long
install_set long
cp -- "$repo_root/deploy/pi/update.sh" "$opt/update.sh.prev"
printf '%s\n' "printf 'update.sh.prev: ran to its own last line\\n'" >>"$opt/update.sh.prev"
chmod 0755 "$opt/update.sh.prev"
cp -p -- "$opt/pi-lib.sh" "$opt/pi-lib.sh.prev"
updater=$opt/update.sh.prev release=b run_update
expect_run prev-copy-by-rename 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)"
[[ $(tail -n1 <<<"$out") == 'update.sh.prev: ran to its own last line' && $(grep -c 'installed commit' <<<"$out") == 1 ]] \
  || fail "prev-copy-by-rename: the running update.sh.prev did not read its own file to its end: $out"
expect_no_output prev-copy-by-rename 'already current'
cmp -s -- "$releases/long/ghost-wispr-pi-deploy/update.sh" "$opt/update.sh.prev" \
  || fail 'prev-copy-by-rename: update.sh.prev is not the update.sh installed before'
expect_set_of prev-copy-by-rename b

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
# The release current: nothing changes, and the run fails.
new_pi
install_set b
echo fail >"$state/pi-secrets-mode"
before=$(snapshot)
release=b run_update
expect_run 'secrets-read-fails, current' 10
expect_unchanged 'secrets-read-fails, current' "$before"
[[ $(restarts) == 0 ]] || fail 'secrets-read-fails, current: restarted the recorder'

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
# would cut the recorder off from every Google service.
new_pi
install_set b
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=b run_update
expect_run certificate 0 "ghost-wispr update: already current at commit $commit_b" \
  'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' && $(<"$opt/device.crt.prev") == 'INSTALLED CERTIFICATE' ]] \
  || fail 'certificate: did not install the staged certificate with the old one as device.crt.prev'
[[ $(restarts) == 1 ]] && grep -Fq "crt=$(sha "$opt/device.crt")" "$state/systemctl.log" \
  || fail "certificate: did not restart the recorder once on the new certificate: $(<"$state/systemctl.log")"
expect_no_output certificate 'installed secrets'
new_pi
install_set b
printf 'NEW CERTIFICATE\n' >"$state/certificate"
touch -- "$state/version-mute.after-restart"
release=b run_update
expect_run 'certificate, the recorder does not answer' 10
expect_line_like 'certificate, the recorder does not answer' 'ghost-wispr update: .*; restored the previous release'
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' ]] || fail 'certificate, the recorder does not answer: did not put the old certificate back'
[[ $(restarts) == 2 ]] || fail "certificate, the recorder does not answer: want the rollback's restart: $(<"$state/systemctl.log")"
# release-and-certificate: a certificate staged while a release installs goes in with the set, and
# the release's one restart serves both (R11); when that release's version check fails, the old
# certificate comes back with the old set.
begin release-and-certificate
new_pi
install_set a
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=b run_update
expect_run release-and-certificate 0 "ghost-wispr update: installed commit $commit_b (invocation test-invocation-7)" \
  'ghost-wispr update: installed certificate version 2, not after 2027-11-26T00:00:00Z'
[[ $(<"$opt/device.crt") == 'NEW CERTIFICATE' && $(<"$opt/device.crt.prev") == 'INSTALLED CERTIFICATE' ]] \
  || fail 'did not install the staged certificate with the old one as device.crt.prev'
[[ $(restarts) == 1 ]] && grep -Fqx "restart binary=$(sha "$opt/ghost-wispr") env=$(sha "$opt/.env") config=$(sha "$opt/ghost-wispr.yaml") crt=$(sha "$opt/device.crt")" \
  "$state/systemctl.log" || fail "the release and the certificate did not share one restart: $(<"$state/systemctl.log")"
new_pi
install_set a
save_installed "$work_dir/before-a"
printf 'NEW CERTIFICATE\n' >"$state/certificate"
release=broken run_update
expect_run 'release-and-certificate, a release that fails' 10
expect_line_like 'release-and-certificate, a release that fails' 'ghost-wispr update: .*; restored the previous release'
expect_installed_is 'release-and-certificate, a release that fails' "$work_dir/before-a"
[[ $(<"$opt/device.crt") == 'INSTALLED CERTIFICATE' ]] || fail 'a release that fails: did not restore device.crt with the set'
[[ $(restarts) == 2 ]] || fail "a release that fails: want the install's and the rollback's restarts: $(<"$state/systemctl.log")"

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
# Serve that proxies to another target, or serves nothing, differs; so does a handler that is no
# proxy. Serve is never changed: no sudo tailscale call but the read.
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
serve_drift 'with no proxy' '{"Web":{"ghost-wispr.example-tailnet.ts.net:443":{"Handlers":{"/":{"Path":"/var/www"}}}}}' none
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
# A Serve status it cannot read (tailscale fails, answers what is not a ServeConfig, or hangs past
# its 10 s bound) is UNKNOWN, never a match.
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
for answer in 'not json' '[]'; do
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
# $TEST_STATE/NAME.exit (0 by default), or hangs when that file says hang.
fake_update() {
  cat >"$1" <<EOF
#!/usr/bin/env bash
printf '%s\n' '$2' >>"\$TEST_STATE/launcher.log"
code=\$(cat -- "\$TEST_STATE/$2.exit" 2>/dev/null || echo 0)
if [[ \$code == hang ]]; then exec /bin/sleep 60; fi
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
# $trigger is set, and the real timeout; $out and $status as run_update's, and $elapsed, the real
# seconds it took.
run_launcher() {
  local started=$SECONDS
  echo 1000 >"$state/clock"
  status=0
  # shellcheck disable=SC2086 # Each trigger word is one assignment or none.
  out=$(cd -- "$opt" && env -u TRIGGER_UNIT -u TRIGGER_TIMER_REALTIME_USEC PATH="$bin:$PATH" \
    TEST_STATE="$state" TEST_RELEASES="$releases" TEST_RELEASE="${release:-none}" TEST_FAKES="$fakes" \
    TEST_PI_SECRETS="$fakes/pi-secrets" TEST_BUSY_PID="$work_dir/busy.pid" TEST_CURL_EXIT="${curl_exit:-}" \
    INVOCATION_ID=test-invocation-9 ${trigger:+TRIGGER_UNIT=ghost-wispr-update.timer} \
    ${trigger:+TRIGGER_TIMER_REALTIME_USEC=$trigger} \
    GHOST_WISPR_DEPLOY_DIR="$opt" GHOST_WISPR_UNIT_DIR="$units" GHOST_WISPR_SYSTEM_ROOT="$root" \
    GHOST_WISPR_PROC_DIR="$pi/proc" GHOST_WISPR_IDENTITY_KEY="$key" \
    GHOST_WISPR_RELEASE_API=https://test.invalid/releases/latest \
    GHOST_WISPR_STATUS_URL=http://127.0.0.1:8080/api/status \
    GHOST_WISPR_VERSION_URL=http://127.0.0.1:8080/api/version \
    GHOST_WISPR_UPDATE_LIMIT="${limit:-45m}" \
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
# The real update.sh, whose curl fails as on a network fault, exits 10: no update.sh.prev runs.
new_pi
install_set b
install -m 0755 -- "$launcher_file" "$opt/ghost-wispr-update-launcher.sh"
fake_update "$opt/update.sh.prev" update.sh.prev
: >"$state/launcher.log"
curl_exit=22 release=b run_launcher
[[ $status == 10 && ! -s $state/launcher.log && $(failed_runs) == 1 && $out == *'ghost-wispr update: '* ]] \
  || fail "a network fault: exit $status, ran $(<"$state/launcher.log"), count $(failed_runs), output $out"
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
# With no update.sh.prev, the launcher exits with update.sh's code, and the count goes on past 11.
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
[[ $status == 10 && $(failed_runs) == 11 ]] || fail "no update.sh.prev, the 11th failed run: exit $status, count $(failed_runs)"

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
