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

fail() {
  printf 'update test FAILED: %s\n' "$*" >&2
  exit 1
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
# pi-lib.sh: update.sh and provision.sh both source it.

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

printf 'update tests passed\n'
