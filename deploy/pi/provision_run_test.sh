#!/usr/bin/env bash
# Runs deploy/pi/provision.sh from a release's deploy set, the directory that
# deploy/pi/build-deploy-tarball.sh --stage writes as release.yml builds it, on a fake Pi: its deploy
# directory, its user unit directory and its key, under one temporary directory
# (dispatch://INFRA-5935/spec R14 and R16, check c-provision). Fakes stand in for sudo, systemctl
# --user, id, ip, curl, journalctl (through sudo) and tailscale with its Serve setting, and for the
# release binary that the nightly update installs. The release's pi.env is deploy/pi/test-pi.env,
# with the SHA-256 the manifest gives it, so no case reads the office Pi's settings.
#
# It checks that provision.sh reads no bundle; refuses a directory that is not a release's; writes
# the Google credential files from pi.env where they are missing and only compares them where they
# exist; installs the bootstrap files and the release's update.sh, pi-lib.sh and pi.env before it
# starts the nightly update, once and through systemd; writes no .env and installs no system file
# itself; applies serve.conf and turns off each other Serve port; and joins the tailnet with the
# client id and the subject of pi.env, in a join command that holds each Serve call as text and
# reads no file of the release's directory. The detached join accepts the tailnet's subnet routes
# only when the route check that provision.sh installs beside the token finds that no peer's route
# overlaps a network of the Pi; the check runs alone on the Pi's networks and on tailnet statuses of
# each kind. It also runs provision.sh --key, --certificate and --models. The fakes are no Pi, so a
# pass does not show one.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "$script_dir/../.." && pwd)"
readonly repo_root
work_dir=$(mktemp -d)
readonly work_dir
trap 'rm -rf -- "$work_dir"' EXIT

# The cases of this test, each of which begin names.
readonly case_names=(bundle-refused not-a-release first-run one-update-start release-files-first
  credentials-written bootstrap-files no-env-no-system-files groups-and-linger join-command idempotent
  credentials-same credentials-differ client-id newer-release newer-release-certificate bootstrap-repair
  serve update-fails recorder-unit join route-check manager-env key certificate models)
current_case=setup
ran_cases=' '

fail() {
  printf 'provision run test FAILED (%s): %s\n' "$current_case" "$*" >&2
  exit 1
}

# begin NAME starts the case NAME, which each failure then names.
begin() {
  [[ " ${case_names[*]} " == *" $1 "* ]] || fail "begin: $1 is not a case of this test"
  current_case=$1
  if [[ $ran_cases != *" $1 "* ]]; then
    ran_cases+="$1 "
    printf 'provision run test: %s\n' "$1"
  fi
}

uid=$(id -u)
readonly uid runtime_dir="/run/user/$uid"
[[ -d $runtime_dir ]] || fail "this test needs $runtime_dir, where provision.sh renders the credential files"

readonly bin=$work_dir/bin join=$work_dir/join pi=$work_dir/pi stage=$work_dir/stage first_join=$work_dir/first-join
readonly deploy=$pi/opt units=$pi/units key_dir=$pi/identity sysroot=$pi/root state=$pi/state
readonly key=$key_dir/key.pem
readonly release_commit=0123456789abcdef0123456789abcdef01234567
# The commits of the releases that the real nightly update installs here, the commit a binary
# answers when it never reports its own, and a commit that no release here has.
readonly commit_a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa commit_b=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  commit_c=cccccccccccccccccccccccccccccccccccccccc wrong_commit=0000000000000000000000000000000000000000 \
  other_commit=89abcdef0123456789abcdef0123456789abcdef
# What test-pi.env sets as PI_TAILNET_CLIENT_ID and PI_SERVICE_ACCOUNT_UNIQUE_ID, written out here.
readonly client_id=k-test-client-id subject=123456789012345678901
real_date=$(command -v date)
real_sleep=$(command -v sleep)
readonly real_date real_sleep
# The release API and the recorder's two answers, as the real nightly update reads them here.
readonly release_api=https://api.test.invalid/releases/latest status_url=http://recorder.test.invalid/api/status \
  version_url=http://recorder.test.invalid/api/version
export TEST_LOG="$work_dir/log" TEST_UID="$uid" TEST_RUNTIME_DIR="$runtime_dir" TEST_DEPLOY="$deploy" \
  TEST_UNIT_DIR="$units" TEST_BINARY="$work_dir/recorder-binary" \
  TEST_RECORDER_UNIT="$repo_root/deploy/pi/files/ghost-wispr.service" TEST_SERVE_STATE="$work_dir/serve.json" \
  TEST_SERVE_HOST=ghost-wispr.example-tailnet.ts.net TEST_ROUTE_ALL="$work_dir/route-all" TEST_SUBJECT="$subject" \
  TEST_REAL_DATE="$real_date" TEST_REAL_SLEEP="$real_sleep" TEST_STATE="$state" TEST_RELEASES="$work_dir/releases" \
  TEST_CLOCK="$work_dir/clock"
mkdir -p -- "$bin" "$join" "$first_join" "$TEST_RELEASES"

cat >"$bin/id" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  -un) printf 'ghost-wispr\n' ;;
  '-u ghost-wispr') printf '%s\n' "$TEST_UID" ;;
  '-nG ghost-wispr') printf 'ghost-wispr sudo plugdev input\n' ;;
  *) printf 'fake id: %s\n' "$*" >&2; exit 1 ;;
esac
EOF

# The fake sudo logs each command. It runs install -d, less -o and -g, which need root; keeps what is
# installed from standard input as $TEST_LOG.installed-NAME; runs tailscale through the fake below;
# prints $TEST_JOURNAL for journalctl; keeps systemd-run's command, and its whole line, to run the
# join below; and runs nothing else. provision.sh installs its own files as ghost-wispr, and the
# nightly update the system files: any other install with sudo, any udevadm call and a logind
# restart are errors that the test reports. The nightly update's own calls each have -n: it
# installs the system files under GHOST_WISPR_SYSTEM_ROOT, reloads udev and restarts logind, which
# the log records, and reads Serve's setting.
cat >"$bin/sudo" <<'EOF'
#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >>"$TEST_LOG"
error() { printf 'fake sudo: %s: sudo %s\n' "$1" "${*:2}" >>"$TEST_LOG.errors"; }
if [[ ${1:-} == -n ]]; then
  shift
  case $1 in
    install)
      if [[ ${!#} != "$GHOST_WISPR_SYSTEM_ROOT"/* ]]; then
        error 'a system file installed outside the system root' -n "$@"
        exit 1
      fi
      shift
      exec install "$@"
      ;;
    udevadm | systemctl) ;;
    tailscale)
      shift
      exec tailscale "$@"
      ;;
    *)
      error 'an unexpected call' -n "$@"
      exit 1
      ;;
  esac
  exit 0
fi
case $1 in
  install)
    if [[ " $* " == *' /dev/stdin '* ]]; then
      cat >"$TEST_LOG.installed-$(basename -- "${!#}")"
    elif [[ ${2:-} == -d ]]; then
      arguments=()
      while (($# > 1)); do
        shift
        case $1 in
          -o | -g) shift ;;
          *) arguments+=("$1") ;;
        esac
      done
      install "${arguments[@]}"
    else
      error 'a file installed as root' "$@"
    fi
    ;;
  tailscale)
    shift
    exec tailscale "$@"
    ;;
  journalctl)
    if [[ -f ${TEST_JOURNAL:-} ]]; then
      cat -- "$TEST_JOURNAL"
    fi
    ;;
  systemd-run)
    printf '%s' "${!#}" >"$TEST_LOG.join"
    printf '%s\n' "$*" >"$TEST_LOG.systemd-run"
    ;;
  udevadm) error 'a udev call' "$@" ;;
  systemctl)
    if [[ $* == *systemd-logind* || $* == *' --user '* ]]; then
      error 'a system file reload or a user call through sudo' "$@"
    fi
    ;;
esac
EOF

# The fake systemctl is ghost-wispr's user manager. It logs each call. A call without --user, or
# whose XDG_RUNTIME_DIR is not the user's runtime directory, is an error the test reports.
# - daemon-reload moves the file TEST_MANAGER_ENV_PENDING names, when it exists, over
#   TEST_MANAGER_ENV: an environment.d line written since the last reload counts from the next. It
#   loads ghost-wispr-update.service as its file is then, $TEST_LOG.loaded-update-unit.
# - show-environment prints the manager's environment: the file TEST_MANAGER_ENV names when it
#   exists, else the few variables a manager has; it exits TEST_MANAGER_ENV_EXIT (0 by default).
# - show -p LoadState -p DropInPaths UNIT prints the unit's load state from $TEST_UNITS/load-UNIT
#   (loaded when absent) and its drop-ins from $TEST_UNITS/dropins-UNIT; show -p DropInPaths
#   --value prints none, and show -p MainPID --value prints 0, a recorder with no image to read.
# - enable fails, as systemctl does, for a unit with no file in TEST_UNIT_DIR.
# - start ghost-wispr-update.service is the nightly update. It writes to $TEST_LOG.start the
#   SHA-256 of each file the update runs on, or "missing". With TEST_UPDATE_EXIT it fails as a
#   failed oneshot unit does; otherwise it installs what the update installs and provision.sh then
#   uses: the binary and ghost-wispr.service. With TEST_REAL_UPDATE=1 it runs the ExecStart of the
#   update unit it loaded instead, the real launcher and update.sh in the deploy directory, into
#   the journal $TEST_JOURNAL, and fails as systemd does when that program fails or is no program.
# - restart ghost-wispr.service starts the recorder, with TEST_REAL_UPDATE=1: it answers the commit
#   that the installed binary's "# answers commit" line names, and is-active says it runs.
cat >"$bin/systemctl" <<'EOF'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"$TEST_LOG"
error() { printf 'fake systemctl: %s: systemctl %s\n' "$1" "${*:2}" >>"$TEST_LOG.errors"; }
if [[ ${1:-} != --user ]]; then
  error 'a call without --user' "$@"
  exit 1
fi
[[ ${XDG_RUNTIME_DIR:-} == "$TEST_RUNTIME_DIR" ]] || error "XDG_RUNTIME_DIR is ${XDG_RUNTIME_DIR:-unset}" "$@"
shift
sum_or_missing() {
  if [[ -f $1 ]]; then
    sha256sum <"$1" | cut -d' ' -f1
  else
    printf 'missing\n'
  fi
}
case "$*" in
  daemon-reload)
    # What the manager loads at this reload: the timer's SHA-256, in the order of the run's events.
    printf 'reload timer=%s\n' "$(sum_or_missing "$TEST_UNIT_DIR/ghost-wispr-update.timer")" >>"$TEST_LOG.events"
    if [[ -f $TEST_UNIT_DIR/ghost-wispr-update.service ]]; then
      cp -- "$TEST_UNIT_DIR/ghost-wispr-update.service" "$TEST_LOG.loaded-update-unit"
    else
      rm -f -- "$TEST_LOG.loaded-update-unit"
    fi
    if [[ -f ${TEST_MANAGER_ENV_PENDING:-} ]]; then
      mv -f -- "$TEST_MANAGER_ENV_PENDING" "$TEST_MANAGER_ENV"
    fi
    ;;
  'is-active --quiet ghost-wispr.service') [[ -f $TEST_STATE/active ]] ;;
  'show -p MainPID --value ghost-wispr.service') printf '0\n' ;;
  'show -p DropInPaths --value '*) printf '\n' ;;
  show-environment)
    if [[ -f ${TEST_MANAGER_ENV:-} ]]; then
      cat -- "$TEST_MANAGER_ENV"
    else
      printf '%s\n' HOME=/home/ghost-wispr LANG=C.UTF-8 PATH=/usr/local/bin:/usr/bin:/bin
    fi
    exit "${TEST_MANAGER_ENV_EXIT:-0}"
    ;;
  'show -p LoadState -p DropInPaths '*)
    unit=${!#} units=${TEST_UNITS:-/nonexistent}
    printf 'LoadState=%s\nDropInPaths=%s\n' "$(cat -- "$units/load-$unit" 2>/dev/null || echo loaded)" \
      "$(cat -- "$units/dropins-$unit" 2>/dev/null || true)"
    ;;
  'enable --now ghost-wispr-update.timer' | 'enable ghost-wispr.service')
    if [[ ! -f $TEST_UNIT_DIR/${!#} ]]; then
      printf 'Failed to enable unit: Unit file %s does not exist.\n' "${!#}" >&2
      exit 1
    fi
    ;;
  'start ghost-wispr-update.service')
    if [[ ${TEST_REAL_UPDATE:-} == 1 ]]; then
      [[ -f $TEST_LOG.loaded-update-unit ]] || { printf 'Unit ghost-wispr-update.service not found.\n' >&2; exit 5; }
      program=$(sed -n 's/^ExecStart=//p' "$TEST_LOG.loaded-update-unit")
      program=${program/#\/opt\/ghost-wispr\//$TEST_DEPLOY/}
      if [[ -z $program || ! -x $program ]]; then
        printf 'ghost-wispr-update.service: Failed at step EXEC spawning %s: No such file or directory (status=203/EXEC)\n' "$program" >>"$TEST_JOURNAL"
        printf 'Job for ghost-wispr-update.service failed because the control process exited with error code.\n' >&2
        exit 1
      fi
      status=0
      INVOCATION_ID=test-invocation-real "$program" >>"$TEST_JOURNAL" 2>&1 || status=$?
      if ((status != 0)); then
        printf 'Job for ghost-wispr-update.service failed because the control process exited with error code.\n' >&2
        exit 1
      fi
      exit 0
    fi
    {
      for name in update.sh pi-lib.sh pi.env ghost-wispr-update-launcher.sh google-credentials.json certificate_config.json; do
        printf '%s %s\n' "$(sum_or_missing "$TEST_DEPLOY/$name")" "$name"
      done
      for name in ghost-wispr-update.service ghost-wispr-update.timer; do
        printf '%s %s\n' "$(sum_or_missing "$TEST_UNIT_DIR/$name")" "$name"
      done
    } >"$TEST_LOG.start"
    if [[ ${TEST_UPDATE_EXIT:-0} != 0 ]]; then
      printf 'Job for ghost-wispr-update.service failed because the control process exited with error code.\n' >&2
      exit 1
    fi
    install -m 0755 -- "$TEST_BINARY" "$TEST_DEPLOY/ghost-wispr"
    install -m 0644 -- "$TEST_RECORDER_UNIT" "$TEST_UNIT_DIR/ghost-wispr.service"
    ;;
  'restart ghost-wispr.service')
    if [[ ${TEST_REAL_UPDATE:-} == 1 ]]; then
      rm -f -- "$TEST_STATE/active"
      commit=$(sed -n 's/^# answers commit //p' "$TEST_DEPLOY/ghost-wispr" 2>/dev/null) || commit=''
      [[ -n $commit ]] || { printf 'ghost-wispr.service: status=203/EXEC\n' >&2; exit 1; }
      printf '%s\n' "$commit" >"$TEST_STATE/running-commit"
      : >"$TEST_STATE/active"
    fi
    ;;
  *)
    error 'an unexpected call' --user "$@"
    exit 1
    ;;
esac
EOF

# tailscale logs each call and keeps the accept-routes setting, RouteAll, in $TEST_ROUTE_ALL (false
# when that file is absent), and the Serve setting, tailscale's ipn.ServeConfig, in
# $TEST_SERVE_STATE:
# - `up` exits TEST_UP_EXIT (0 by default), and on 0 sets RouteAll as its --accept-routes flag says.
#   Without the flag, while RouteAll is true, it refuses as up.go's check of settings left out does
#   (Tailscale v1.104.1, cmd/tailscale/cli/up.go:1013-1105) and exits 1. It keeps its exit code in
#   $TEST_LOG.up-exit;
# - `set --accept-routes` exits TEST_SET_EXIT (0 by default), and on 0 sets RouteAll true;
#   `set --accept-routes=false` sets it false;
# - `status --self --json` prints the node's own reading, TEST_SELF_STATUS when it is set;
# - `status --json` prints the status fixture $TEST_STATUS, or fails with TEST_STATUS_EXIT;
# - `serve --bg --https PORT TARGET` exits TEST_SERVE_EXIT (0 by default), and on 0 serves TARGET at
#   the mount / of the HTTPS port PORT, and keeps each other mount of that port, as tailscale's
#   SetWebHandler does (ipn/serve.go, v1.104); `serve --https=PORT off` serves nothing there;
#   `serve --https=PORT --set-path=MOUNT off` removes that mount, and the port with its last one;
#   `serve status --json` prints the setting, or fails with TEST_SERVE_STATUS_EXIT;
# - `debug prefs` prints RouteAll.
cat >"$bin/tailscale" <<'EOF'
#!/usr/bin/env bash
printf 'tailscale %s\n' "$*" >>"$TEST_LOG"
unexpected() {
  printf 'fake tailscale: unexpected call: tailscale %s\n' "$*" >&2
  exit 2
}
route_all=$(cat -- "$TEST_ROUTE_ALL" 2>/dev/null) || route_all=false
case $1 in
  up)
    code=${TEST_UP_EXIT:-0} flag=
    for argument in "${@:2}"; do
      case $argument in
        --accept-routes | --accept-routes=true) flag=true ;;
        --accept-routes=false) flag=false ;;
      esac
    done
    if [[ $code == 0 && -z $flag && $route_all == true ]]; then
      printf "Error: changing settings via 'tailscale up' requires mentioning all\n" >&2
      code=1
    elif [[ $code == 0 && -n $flag ]]; then
      printf '%s\n' "$flag" >"$TEST_ROUTE_ALL"
    fi
    printf '%s\n' "$code" >"$TEST_LOG.up-exit"
    exit "$code"
    ;;
  set)
    case ${*:2} in
      --accept-routes | --accept-routes=true)
        [[ ${TEST_SET_EXIT:-0} != 0 ]] || printf 'true\n' >"$TEST_ROUTE_ALL"
        exit "${TEST_SET_EXIT:-0}"
        ;;
      --accept-routes=false) printf 'false\n' >"$TEST_ROUTE_ALL" ;;
      *) unexpected "$@" ;;
    esac
    ;;
  status)
    case ${*:2} in
      '--self --json')
        if [[ -n ${TEST_SELF_STATUS:-} ]]; then
          printf '%s\n' "$TEST_SELF_STATUS"
        else
          printf '{"Self":{"Tags":null,"DNSName":"ghost-wispr.example-tailnet.ts.net."}}\n'
        fi
        ;;
      --json)
        if [[ ${TEST_STATUS_EXIT:-0} != 0 ]]; then
          printf 'failed to connect to the local Tailscale daemon\n' >&2
          exit "$TEST_STATUS_EXIT"
        fi
        cat -- "$TEST_STATUS"
        ;;
      *) unexpected "$@" ;;
    esac
    ;;
  serve)
    state=$TEST_SERVE_STATE
    [[ -f $state ]] || printf '{}\n' >"$state"
    case ${*:2} in
      'status --json')
        if [[ ${TEST_SERVE_STATUS_EXIT:-0} != 0 ]]; then
          printf 'failed to connect to the local Tailscale daemon\n' >&2
          exit "$TEST_SERVE_STATUS_EXIT"
        fi
        cat -- "$state"
        ;;
      '--bg --https '*)
        (($# == 5)) || unexpected "$@"
        code=${TEST_SERVE_EXIT:-0}
        if [[ $code == 0 ]]; then
          jq --arg port "$4" --arg target "$5" --arg host "$TEST_SERVE_HOST" \
            '.TCP[$port] = {HTTPS: true} | .Web["\($host):\($port)"].Handlers["/"] = {Proxy: $target}' \
            "$state" >"$state.new" && mv -f -- "$state.new" "$state"
        fi
        exit "$code"
        ;;
      --https=[0-9]*' --set-path='*' off')
        (($# == 4)) || unexpected "$@"
        port=${2#--https=} mount=${3#--set-path=}
        jq --arg port "$port" --arg mount "$mount" '
          reduce ((.Web // {}) | keys[] | select(endswith(":" + $port))) as $key (.;
            del(.Web[$key].Handlers[$mount])
            | if (.Web[$key].Handlers | length) == 0 then del(.Web[$key]) | del(.TCP[$port]) else . end)' \
          "$state" >"$state.new" && mv -f -- "$state.new" "$state"
        ;;
      --https=[0-9]*' off')
        (($# == 3)) || unexpected "$@"
        port=${2#--https=}
        jq --arg port "$port" \
          'del(.TCP[$port]) | .Web = ((.Web // {}) | with_entries(select(.key | endswith(":" + $port) | not)))' \
          "$state" >"$state.new" && mv -f -- "$state.new" "$state"
        ;;
      *) unexpected "$@" ;;
    esac
    ;;
  debug)
    [[ $* == 'debug prefs' ]] || unexpected "$@"
    printf '{"RouteAll":%s}\n' "$route_all"
    ;;
  *) unexpected "$@" ;;
esac
EOF

# ip prints the case's networks, the fixture $TEST_NETWORKS, as `ip -4 -o addr` does, or fails with
# TEST_IP_EXIT.
cat >"$bin/ip" <<'EOF'
#!/usr/bin/env bash
[[ $* == '-4 -o addr' ]] || { printf 'fake ip: unexpected call: ip %s\n' "$*" >&2; exit 2; }
if [[ ${TEST_IP_EXIT:-0} != 0 ]]; then
  printf 'Cannot open netlink socket: Permission denied\n' >&2
  exit "$TEST_IP_EXIT"
fi
cat -- "$TEST_NETWORKS"
EOF

# provision.sh reads nothing over HTTP. update.sh and the launcher do, so a curl call means
# provision.sh ran one of them itself, not through systemd: an error the test reports. With
# TEST_REAL_UPDATE=1, the nightly update runs through the fake systemctl, and curl answers it: the
# release API with $TEST_RELEASES/latest.json, each asset URL with its file under $TEST_RELEASES,
# and the recorder's /api/status (free) and /api/version (the commit it runs) while it runs.
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$TEST_LOG"
if [[ ${TEST_REAL_UPDATE:-} != 1 ]]; then
  printf 'fake curl: a call, so provision.sh ran update.sh or the launcher itself: curl %s\n' "$*" >>"$TEST_LOG.errors"
  exit 7
fi
output='' url=''
while (($#)); do
  case $1 in
    --output) output=$2 && shift ;;
    --connect-timeout | --max-time) shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
case $url in
  "$GHOST_WISPR_STATUS_URL")
    [[ -f $TEST_STATE/active ]] || exit 7
    printf '{"active_session_id":"","paused":false,"capture_state":"idle"}\n'
    ;;
  "$GHOST_WISPR_VERSION_URL")
    [[ -f $TEST_STATE/active ]] || exit 7
    printf '{"commit":"%s"}\n' "$(<"$TEST_STATE/running-commit")"
    ;;
  "$GHOST_WISPR_RELEASE_API") cat -- "$TEST_RELEASES/latest.json" ;;
  https://assets.test.invalid/*)
    file=$TEST_RELEASES/${url#https://assets.test.invalid/}
    [[ -f $file && -n $output ]] || exit 22
    cp -- "$file" "$output"
    ;;
  *) exit 6 ;;
esac
EOF

# The release binary that the nightly update installs. Its google-id-token logs how it was run and
# prints a JWT for the subject TEST_SUBJECT and the audience it is asked for.
cat >"$TEST_BINARY" <<'EOF'
#!/usr/bin/env bash
printf 'ghost-wispr %s\n' "$*" >>"$TEST_LOG"
[[ ${1:-} == google-id-token ]] || exit 3
audience=
while (($#)); do
  [[ $1 != --audience ]] || audience=$2
  shift
done
payload=$(printf '{"sub":"%s","aud":"%s"}' "$TEST_SUBJECT" "$audience" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJSUzI1NiJ9.%s.c2lnbmF0dXJl\n' "$payload"
EOF

# The binary that build-deploy-tarball.sh builds the stage for: it answers link-versions as a
# binary with no such subcommand does, so the manifest has no link_versions.
cat >"$work_dir/release-binary" <<'EOF'
#!/usr/bin/env bash
[[ $* == link-versions ]] || exit 3
printf 'unknown subcommand "link-versions"\n' >&2
exit 2
EOF

# date logs each call for the time now in seconds to $TEST_LOG.events, in order with the user
# manager's reloads, and answers as the real date; while $TEST_CLOCK exists, from that clock. sleep
# moves that clock instead of waiting, so the update's 120 s version wait takes no time.
cat >"$bin/date" <<'EOF'
#!/usr/bin/env bash
if [[ $* == +%s ]]; then
  printf 'date +%%s\n' >>"$TEST_LOG.events"
  if [[ -f ${TEST_CLOCK:-} ]]; then
    cat -- "$TEST_CLOCK"
    exit 0
  fi
fi
exec "$TEST_REAL_DATE" "$@"
EOF
cat >"$bin/sleep" <<'EOF'
#!/usr/bin/env bash
if [[ -f ${TEST_CLOCK:-} && $# == 1 && $1 =~ ^[0-9]+$ ]]; then
  printf '%s\n' "$(($(<"$TEST_CLOCK") + $1))" >"$TEST_CLOCK"
  exit 0
fi
exec "$TEST_REAL_SLEEP" "$@"
EOF
chmod +x "$bin/"* "$TEST_BINARY" "$work_dir/release-binary"

sha() {
  local sum
  sum=$(sha256sum -- "$1") || return 1
  printf '%s\n' "${sum%% *}"
}

# restage DIR NAME SOURCE puts SOURCE in the release directory DIR as NAME, and gives manifest.json
# its SHA-256, as a release built with that file would hold it.
restage() {
  local dir=$1 name=$2 sum
  cp -- "$3" "$dir/$name"
  sum=$(sha "$dir/$name")
  jq --arg name "$name" --arg sum "$sum" '.files[$name] = $sum' "$dir/manifest.json" >"$dir/manifest.json.new"
  mv -f -- "$dir/manifest.json.new" "$dir/manifest.json"
}

# make_stage DIR writes a release's deploy set into DIR with build-deploy-tarball.sh --stage, the
# directory the release's tarball holds, with test-pi.env as its pi.env.
make_stage() {
  rm -rf -- "$1"
  bash "$repo_root/deploy/pi/build-deploy-tarball.sh" --stage "$release_commit" "$work_dir/release-binary" "$1" \
    >"$work_dir/build.out" 2>&1 || fail "build-deploy-tarball.sh --stage failed: $(<"$work_dir/build.out")"
  [[ -f $1/manifest.json && -f $1/provision.sh ]] || fail "build-deploy-tarball.sh --stage wrote no release directory in $1"
  restage "$1" pi.env "$script_dir/test-pi.env"
}

# new_pi makes the fake Pi as provision.sh --key and --certificate leave it: the key and the first
# certificate, and no other file of Ghost Wispr. Serve serves nothing, the node has not joined yet,
# no recorder runs, and each log starts empty.
new_pi() {
  rm -rf -- "$pi"
  mkdir -p -- "$deploy" "$units" "$key_dir" "$sysroot" "$state"
  printf 'the key\n' >"$key"
  printf 'the certificate\n' >"$deploy/device.crt"
  rm -f -- "$TEST_LOG" "$TEST_LOG".*
  : >"$TEST_LOG"
  printf '{}\n' >"$TEST_SERVE_STATE"
  printf 'false\n' >"$TEST_ROUTE_ALL"
}

# run_provision DIR [ARGUMENT...] runs DIR/provision.sh as ghost-wispr on the fake Pi, with no input
# and no XDG_RUNTIME_DIR of its own; RUN_INPUT names a file to give it as input instead. Its output
# goes to $work_dir/out, and its exit code to rc.
run_provision() {
  local dir=$1
  shift
  rc=0
  env -u XDG_RUNTIME_DIR PATH="$bin:$PATH" GHOST_WISPR_DEPLOY_DIR="$deploy" GHOST_WISPR_UNIT_DIR="$units" \
    GHOST_WISPR_IDENTITY_KEY="$key" GHOST_WISPR_JOIN_DIR="$join" GHOST_WISPR_SYSTEM_ROOT="$sysroot" \
    GHOST_WISPR_RELEASE_API="$release_api" GHOST_WISPR_STATUS_URL="$status_url" GHOST_WISPR_VERSION_URL="$version_url" \
    bash "$dir/provision.sh" "$@" <"${RUN_INPUT:-/dev/null}" >"$work_dir/out" 2>&1 || rc=$?
}

# make_release NAME COMMIT ANSWER [CERTIFICATE] publishes the fake release NAME of COMMIT, whose
# binary answers /api/version with ANSWER once it runs, and answers pi-secrets with two secret lines
# and, given CERTIFICATE, a version number, a newer certificate of that version, staged as the real
# pi-secrets stages one ("certificate version N" in device.crt). Each file of its deploy set but
# provision.sh and serve.conf ends with the line "# release NAME", so the files of two releases
# differ. provision.sh runs from $TEST_RELEASES/NAME/ghost-wispr-pi-deploy, which
# build-deploy-tarball.sh --stage writes; the tarball of that directory, the binary and their
# .sha256 files are the assets of $TEST_RELEASES/NAME/release.json.
make_release() {
  local name=$1 commit=$2 answer=$3 certificate=${4:-} dir=$TEST_RELEASES/$1 file
  rm -rf -- "$dir"
  mkdir -p -- "$dir"
  {
    printf '#!/usr/bin/env bash\n# answers commit %s\ncertificate=%s\n' "$answer" "$certificate"
    cat <<'EOF'
case ${1:-} in
  link-versions)
    printf 'unknown subcommand "link-versions"\n' >&2
    exit 2
    ;;
  pi-secrets)
    out=''
    while (($#)); do
      [[ $1 != --out ]] || out=$2
      shift
    done
    [[ -d $out ]] || exit 1
    printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=test-key GHOST_WISPR_DISPATCH_TOKEN=test-token >"$out/secrets.env"
    chmod 0600 "$out/secrets.env"
    printf 'pi-secrets: read the two secrets\n'
    if [[ -n $certificate ]]; then
      printf 'certificate version %s\n' "$certificate" >"$out/device.crt"
      printf 'pi-secrets: certificate version %s, not after 2027-12-01T00:00:00Z: newer and signs in\n' "$certificate"
    fi
    ;;
  *) exit 3 ;;
esac
EOF
  } >"$dir/ghost-wispr-linux-arm64"
  chmod 0755 "$dir/ghost-wispr-linux-arm64"
  bash "$repo_root/deploy/pi/build-deploy-tarball.sh" --stage "$commit" "$dir/ghost-wispr-linux-arm64" "$dir/ghost-wispr-pi-deploy" \
    >"$work_dir/build.out" 2>&1 || fail "build-deploy-tarball.sh --stage failed for release $name: $(<"$work_dir/build.out")"
  restage "$dir/ghost-wispr-pi-deploy" pi.env "$script_dir/test-pi.env"
  for file in ghost-wispr.yaml pi.env update.sh pi-lib.sh wait-for-network.sh wait-for-time-sync.sh ghost-wispr.service \
    99-ghost-wispr.rules logind-power-key.conf ghost-wispr-update-launcher.sh ghost-wispr-update.service ghost-wispr-update.timer; do
    { cat -- "$dir/ghost-wispr-pi-deploy/$file" && printf '# release %s\n' "$name"; } >"$work_dir/marked"
    restage "$dir/ghost-wispr-pi-deploy" "$file" "$work_dir/marked"
  done
  tar -C "$dir" -czf "$dir/ghost-wispr-pi-deploy.tar.gz" ghost-wispr-pi-deploy
  (cd -- "$dir" && sha256sum ghost-wispr-pi-deploy.tar.gz >ghost-wispr-pi-deploy.tar.gz.sha256 \
    && sha256sum ghost-wispr-linux-arm64 >ghost-wispr-linux-arm64.sha256) || fail "could not sum release $name's assets"
  jq -n --arg commit "$commit" --arg base "https://assets.test.invalid/$name" '{target_commitish: $commit,
    assets: [("ghost-wispr-linux-arm64", "ghost-wispr-linux-arm64.sha256", "ghost-wispr-pi-deploy.tar.gz",
      "ghost-wispr-pi-deploy.tar.gz.sha256") | {name: ., browser_download_url: "\($base)/\(.)"}]}' >"$dir/release.json"
}

# real_provision NAME runs the provision.sh of release NAME, now the latest release, on the fake Pi,
# joined to the tailnet, with the real nightly update behind the update unit's start: the real
# launcher and update.sh run against the fake release API, recorder, systemd, sudo and Tailscale,
# on a clock that sleep moves. The update's lines go to the journal that provision.sh prints.
real_provision() {
  cp -- "$TEST_RELEASES/$1/release.json" "$TEST_RELEASES/latest.json"
  : >"$TEST_LOG"
  rm -f -- "$TEST_LOG.errors"
  : >"$work_dir/journal"
  printf '1000\n' >"$TEST_CLOCK"
  TEST_REAL_UPDATE=1 TEST_JOURNAL=$work_dir/journal TEST_SELF_STATUS=$joined_status \
    run_provision "$TEST_RELEASES/$1/ghost-wispr-pi-deploy"
  rm -f -- "$TEST_CLOCK"
}

# installed_set prints the SHA-256 of each installed file of the deploy set, the binary, .env and
# deploy.verified, or "missing", and the commit the recorder answers.
installed_set() {
  local path
  for path in "$deploy"/{ghost-wispr.yaml,pi.env,update.sh,pi-lib.sh,wait-for-network.sh,wait-for-time-sync.sh} \
    "$units/ghost-wispr.service" "$sysroot/etc/udev/rules.d/99-ghost-wispr.rules" \
    "$sysroot/etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf" "$deploy"/{ghost-wispr,.env,deploy.verified}; do
    if [[ -f $path ]]; then
      printf '%s %s\n' "$(sha "$path")" "${path#"$pi"/}"
    else
      printf 'missing %s\n' "${path#"$pi"/}"
    fi
  done
  printf 'the recorder answers %s\n' "$(cat -- "$state/running-commit" 2>/dev/null || echo nothing)"
}

# expect_ran WHAT fails unless the run exited 0, printed no bash error, and broke no rule of a fake.
expect_ran() {
  ((rc == 0)) || fail "$1: provision.sh exited $rc: $(<"$work_dir/out")"
  if grep -E 'provision\.sh: line [0-9]+: ' "$work_dir/out" >&2; then
    fail "$1: provision.sh printed a bash error (above)"
  fi
  [[ ! -s $TEST_LOG.errors ]] || fail "$1: $(<"$TEST_LOG.errors")"
}

# expect_refused WHAT TEXT fails unless the run exited non-zero and said TEXT.
expect_refused() {
  ((rc != 0)) || fail "$1: provision.sh exited 0: $(<"$work_dir/out")"
  grep -Fq -- "$2" "$work_dir/out" || fail "$1: provision.sh did not say '$2': $(<"$work_dir/out")"
}

said() { grep -Fq -- "$1" "$work_dir/out"; }
logged() { grep -Fxq -- "$1" "${2:-$TEST_LOG}"; }

# line_of LINE [LOG] prints the number of the first line of LOG that is LINE, or nothing.
line_of() {
  grep -n -F -x -- "$1" "${2:-$TEST_LOG}" | head -n 1 | cut -d: -f1 || true
}

# pi_files prints the mode, the SHA-256 and the path of each file on the fake Pi.
pi_files() {
  local file
  while IFS= read -r file; do
    printf '%s %s %s\n' "$(stat -c %a -- "$pi/$file")" "$(sha "$pi/$file")" "$file"
  done < <(cd -- "$pi" && find . -type f | LC_ALL=C sort)
}

# serve_now prints the fake Pi's Serve setting as serve.conf lines, "<port> <proxy target>", sorted:
# each HTTPS port, with the proxy target of its handler / or "none".
serve_now() {
  jq -r '(.TCP // {} | to_entries[] | select(.value.HTTPS == true) | .key) as $port
    | ([.Web // {} | to_entries[] | select(.key | endswith(":" + $port)) | .value.Handlers["/"].Proxy] | first // "none") as $target
    | "\($port) \($target)"' "$TEST_SERVE_STATE" | LC_ALL=C sort
}

make_stage "$stage"
readonly joined_status='{"Self":{"Tags":["tag:ghost-wispr"],"DNSName":"ghost-wispr.example-tailnet.ts.net."}}'

# ---------------------------------------------------------------------------------------------------
# bundle-refused: provision.sh reads no bundle. Input on standard input, such as the credential
# bundle an older provisioner read there, is refused with a usage line before any command runs; so
# is an argument it does not know.
begin bundle-refused
new_pi
before=$(pi_files)
tar -C "$script_dir" -cf "$work_dir/bundle.tar" test-pi.env
RUN_INPUT=$work_dir/bundle.tar run_provision "$stage"
expect_refused 'a bundle on standard input' 'usage: provision.sh'
said 'standard input' || fail "provision.sh did not say why it refused its input: $(<"$work_dir/out")"
[[ ! -s $TEST_LOG ]] || fail "provision.sh ran commands with a bundle on its input: $(<"$TEST_LOG")"
[[ $(pi_files) == "$before" ]] || fail 'provision.sh changed a file with a bundle on its input'
run_provision "$stage" --bundle
expect_refused 'an argument it does not know' 'usage: provision.sh'
[[ ! -s $TEST_LOG ]] || fail "provision.sh ran commands for an argument it does not know: $(<"$TEST_LOG")"

# ---------------------------------------------------------------------------------------------------
# not-a-release: provision.sh installs only a release's files. A checkout's deploy/pi/, which has
# no manifest.json, a file changed after the release was built, and a file the manifest names that
# is missing are each refused before any file is installed: the only commands run are the reads of
# the user manager and the packages.
begin not-a-release
# expect_nothing_installed WHAT
expect_nothing_installed() {
  [[ $(pi_files) == "$before" ]] || fail "$1: provision.sh changed a file on the Pi"
  if grep -vE '^(systemctl --user (daemon-reload|show-environment)|sudo apt-get .*)$' "$TEST_LOG" >&2; then
    fail "$1: provision.sh ran more than the reads of the user manager and the packages (above)"
  fi
}
new_pi
before=$(pi_files)
run_provision "$repo_root/deploy/pi"
expect_refused "a checkout's deploy/pi/" 'manifest.json'
expect_nothing_installed "a checkout's deploy/pi/"
rm -rf -- "$work_dir/changed-stage"
cp -a -- "$stage" "$work_dir/changed-stage"
printf '# changed after the release was built\n' >>"$work_dir/changed-stage/update.sh"
: >"$TEST_LOG"
run_provision "$work_dir/changed-stage"
expect_refused 'update.sh changed after the build' 'update.sh'
expect_nothing_installed 'update.sh changed after the build'
rm -rf -- "$work_dir/short-stage"
cp -a -- "$stage" "$work_dir/short-stage"
rm -- "$work_dir/short-stage/pi-lib.sh"
: >"$TEST_LOG"
run_provision "$work_dir/short-stage"
expect_refused 'pi-lib.sh missing' 'pi-lib.sh'
expect_nothing_installed 'pi-lib.sh missing'

# ---------------------------------------------------------------------------------------------------
# first-run: provision.sh on the office Pi before its first run of this provisioner, from P2's
# temporary copy of the release's directory. An older provisioner left an older update.sh, which
# installed the binary alone, and a timer that starts it at 03:30 alone.
begin first-run
new_pi
rm -rf -- "$work_dir/p2-stage"
cp -a -- "$stage" "$work_dir/p2-stage"
printf '#!/bin/sh\n# an older update.sh, which installed the binary alone\n' >"$deploy/update.sh"
printf '%s\n' '[Timer]' 'OnCalendar=*-*-* 03:30:00' 'Persistent=true' '[Install]' 'WantedBy=timers.target' \
  >"$units/ghost-wispr-update.timer"
printf '%s\n' 'ghost-wispr launcher: invocation test-invocation trigger none at none' \
  "ghost-wispr update: installed commit $release_commit (invocation test-invocation)" >"$work_dir/journal"
TEST_JOURNAL=$work_dir/journal run_provision "$work_dir/p2-stage"
cp -- "$TEST_LOG" "$work_dir/first.log"
cp -- "$work_dir/out" "$work_dir/first.out"
cp -- "$TEST_LOG.events" "$work_dir/first.events"
for file in join systemd-run installed-route-overlap-check; do
  if [[ -f $TEST_LOG.$file ]]; then
    cp -- "$TEST_LOG.$file" "$first_join/${file#installed-}"
  fi
done

# one-update-start: the update runs as one systemctl --user start of ghost-wispr-update.service, so
# systemd joins it with a start of the timer at the same moment, and never as a run of the launcher
# or update.sh by provision.sh itself. The timer is on before the start, and the recorder's unit,
# which the update installs, is enabled after it.
begin one-update-start
starts=$(grep -c '^systemctl --user start ' "$work_dir/first.log" || true)
if [[ $starts != 1 ]] || ! logged 'systemctl --user start ghost-wispr-update.service' "$work_dir/first.log"; then
  fail "provision.sh started the update $starts times, not once with systemctl --user start ghost-wispr-update.service: $(<"$work_dir/first.log")"
fi
if grep -q '^curl ' "$work_dir/first.log" || grep -q 'ghost-wispr launcher:' <(grep -v 'invocation test-invocation' "$work_dir/first.out"); then
  fail "provision.sh ran the launcher or update.sh itself: $(<"$work_dir/first.log")"
fi
timer_at=$(line_of 'systemctl --user enable --now ghost-wispr-update.timer' "$work_dir/first.log")
start_at=$(line_of 'systemctl --user start ghost-wispr-update.service' "$work_dir/first.log")
enable_at=$(line_of 'systemctl --user enable ghost-wispr.service' "$work_dir/first.log")
[[ -n $timer_at && -n $enable_at && $timer_at -lt $start_at && $start_at -lt $enable_at ]] \
  || fail "provision.sh did not turn the timer on before the start and enable ghost-wispr.service after it: $(<"$work_dir/first.log")"
# The journal it prints starts before the user manager loads the release's timer: its schedule has
# times the old one lacks, such as 03:45, and a reload can start the update at once
# (systemd#16732), before a time read after it. The first time read comes before that reload.
release_timer=$(sha "$work_dir/stage/ghost-wispr-update.timer")
mapfile -t events <"$work_dir/first.events"
since_at='' loaded_at=''
for i in "${!events[@]}"; do
  if [[ -z $since_at && ${events[i]} == 'date +%s' ]]; then
    since_at=$i
  fi
  if [[ -z $loaded_at && ${events[i]} == "reload timer=$release_timer" ]]; then
    loaded_at=$i
  fi
done
[[ -n $since_at && -n $loaded_at && $since_at -lt $loaded_at ]] \
  || fail "provision.sh read the time for the update's journal after the user manager loaded the release's timer: $(paste -sd ';' "$work_dir/first.events")"

# release-files-first: when the update starts, the release's update.sh, pi-lib.sh and pi.env, the
# bootstrap files and the credential files are in place, so the update that runs is the release's,
# on its own library and settings.
begin release-files-first
[[ -f $TEST_LOG.start ]] || fail 'the update never started'
want_start=$(
  for name in update.sh pi-lib.sh pi.env ghost-wispr-update-launcher.sh; do
    printf '%s %s\n' "$(sha "$stage/$name")" "$name"
  done
  for name in google-credentials.json certificate_config.json; do
    printf '%s %s\n' "$(sha "$deploy/$name")" "$name"
  done
  for name in ghost-wispr-update.service ghost-wispr-update.timer; do
    printf '%s %s\n' "$(sha "$stage/$name")" "$name"
  done
)
[[ $(<"$TEST_LOG.start") == "$want_start" ]] \
  || fail "the update started on other files than the release's: got $(<"$TEST_LOG.start"), want $want_start"

begin first-run
expect_ran 'the first run'
said "ghost-wispr update: installed commit $release_commit (invocation test-invocation)" \
  || fail "provision.sh did not print the update's lines from its journal: $(<"$work_dir/out")"
grep -Eq '^sudo journalctl _SYSTEMD_USER_UNIT=ghost-wispr-update\.service --since=@[0-9]+ --no-pager --output=cat$' \
  "$work_dir/first.log" || fail "provision.sh did not read this run's lines of the update from the journal: $(<"$work_dir/first.log")"

# credentials-written: where the Pi has no credential files, provision.sh writes them from pi.env
# with each field that gcloud's create-cred-config writes for an X.509 credential, naming the Pi's
# own paths, mode 0644. gcloud (as the office Pi's files hold it) impersonates through the mTLS
# endpoint, iamcredentials.mtls.googleapis.com.
begin credentials-written
readonly want_google_credentials='{"audience":"//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ghost-wispr/providers/ghost-wispr","credential_source":{"certificate":{"certificate_config_location":"/opt/ghost-wispr/certificate_config.json"}},"service_account_impersonation_url":"https://iamcredentials.mtls.googleapis.com/v1/projects/-/serviceAccounts/ghost-wispr@example-project.iam.gserviceaccount.com:generateAccessToken","subject_token_type":"urn:ietf:params:oauth:token-type:mtls","token_url":"https://sts.mtls.googleapis.com/v1/token","type":"external_account","universe_domain":"googleapis.com"}'
readonly want_certificate_config='{"cert_configs":{"workload":{"cert_path":"/opt/ghost-wispr/device.crt","key_path":"/home/ghost-wispr/.config/ghost-wispr-identity/key.pem"}}}'
for entry in "google-credentials.json $want_google_credentials" "certificate_config.json $want_certificate_config"; do
  name=${entry%% *} want=${entry#* }
  [[ -f $deploy/$name ]] || fail "provision.sh wrote no $name"
  [[ $(jq -cS . "$deploy/$name") == "$want" ]] || fail "$name holds $(jq -cS . "$deploy/$name"), want $want"
  [[ $(stat -c %a -- "$deploy/$name") == 644 ]] || fail "$name has mode $(stat -c %a -- "$deploy/$name"), not 644"
  said "credentials: wrote $name from pi.env" || fail "provision.sh did not say it wrote $name: $(<"$work_dir/out")"
done
if said 'credentials: same' || said 'credentials: differ'; then
  fail "provision.sh compared credential files that it wrote: $(<"$work_dir/out")"
fi
cp -- "$deploy/google-credentials.json" "$work_dir/google-credentials.json"
cp -- "$deploy/certificate_config.json" "$work_dir/certificate_config.json"

# bootstrap-files: the launcher, the update unit and the timer come from the release's directory,
# with update.sh, pi-lib.sh and pi.env, each byte for byte, with its mode.
begin bootstrap-files
for entry in "ghost-wispr-update-launcher.sh $deploy 755" "ghost-wispr-update.service $units 644" \
  "ghost-wispr-update.timer $units 644" "update.sh $deploy 755" "pi-lib.sh $deploy 755" "pi.env $deploy 644"; do
  read -r name dir mode <<<"$entry"
  cmp -s -- "$stage/$name" "$dir/$name" || fail "$dir/$name is not the release's $name"
  [[ $(stat -c %a -- "$dir/$name") == "$mode" ]] || fail "$dir/$name has mode $(stat -c %a -- "$dir/$name"), not $mode"
done

# no-env-no-system-files: the nightly update owns .env, the config, the udev rule and the logind
# setting. provision.sh writes none of them, and reloads neither udev nor logind.
begin no-env-no-system-files
for name in .env ghost-wispr.yaml; do
  [[ ! -e $deploy/$name ]] || fail "provision.sh wrote $deploy/$name"
done
if grep -E 'udev|logind|99-ghost-wispr\.rules|logind-power-key' "$work_dir/first.log" >&2; then
  fail 'provision.sh installed or reloaded a system file itself (above)'
fi

# groups-and-linger: a lingering user service has no seat, so only the audio group opens the ALSA
# devices; the packages install, and the user manager lingers.
begin groups-and-linger
for line in 'sudo apt-get install -y --no-install-recommends libportaudio2 ffmpeg lame jq curl' \
  'sudo usermod -aG audio ghost-wispr' 'sudo loginctl enable-linger ghost-wispr'; do
  logged "$line" "$work_dir/first.log" || fail "provision.sh did not run $line: $(<"$work_dir/first.log")"
done
if grep -Eq '^sudo usermod -aG (plugdev|input) ' "$work_dir/first.log"; then
  fail 'provision.sh re-added a group ghost-wispr already has'
fi

# join-command: the node had not joined, so provision.sh starts the detached join. It joins with
# pi.env's client id and a token for pi.env's subject, from the binary the update installed, and the
# join's command holds Serve's call as text: the join runs after P2's command removed the release's
# directory, so it reads no file of it.
begin join-command
[[ -s $first_join/join ]] || fail 'provision.sh started no detached join'
join_command=$(<"$first_join/join")
[[ $join_command == *"--client-id='$client_id?ephemeral=false&preauthorized=true'"* ]] \
  || fail "the join does not use pi.env's client id: $join_command"
[[ $join_command == *'tailscale serve --bg --https 443 http://127.0.0.1:8080 || rc=$?;'* ]] \
  || fail "the join does not hold serve.conf's line as a tailscale serve call: $join_command"
[[ $join_command != *"$work_dir/p2-stage"* && $join_command != *serve.conf* ]] \
  || fail "the join reads the release's directory: $join_command"
logged "ghost-wispr google-id-token --credentials $deploy/google-credentials.json --audience api.tailscale.com/$client_id" \
  "$work_dir/first.log" || fail "provision.sh did not ask the installed binary for a token for pi.env's client id: $(<"$work_dir/first.log")"
grep -Fq 'starting the tagged tailnet join as ghost-wispr-tailnet-join-' "$work_dir/first.out" \
  || fail "provision.sh did not name the join it started: $(<"$work_dir/first.out")"

# ---------------------------------------------------------------------------------------------------
# idempotent: a second run of the same release, on the Pi the first run left, now joined, changes
# no file, starts the update once more, and stops after "already joined", with no join and no
# route check.
begin idempotent
before=$(pi_files)
: >"$TEST_LOG"
TEST_SELF_STATUS=$joined_status run_provision "$stage"
expect_ran 'a second run'
[[ $(pi_files) == "$before" ]] || fail "a second run changed a file: $(diff <(printf '%s\n' "$before") <(pi_files) || true)"
[[ $(grep -c '^systemctl --user start ' "$TEST_LOG" || true) == 1 ]] || fail "a second run did not start the update once: $(<"$TEST_LOG")"
said 'already joined' || fail "a second run did not say the Pi had joined: $(<"$work_dir/out")"
if grep -Eq 'systemd-run|route-overlap-check|tailscale set' "$TEST_LOG"; then
  fail "a second run on a Pi already joined started a join or a route check: $(<"$TEST_LOG")"
fi

# credentials-same: where the credential files exist, provision.sh compares them with what pi.env
# renders, field by field, and changes nothing. gcloud wrote the office Pi's files in its own layout:
# its key order, and certificate_config.json with no final line feed, as below with test-pi.env's
# values. Those, and the same fields in another order and indentation, are the same.
begin credentials-same
said 'credentials: same as pi.env renders' || fail "a second run did not say the credentials are the same: $(<"$work_dir/out")"
if said 'credentials: wrote' || said 'credentials: differ'; then
  fail "a second run wrote or found different credential files: $(<"$work_dir/out")"
fi
cat >"$work_dir/gcloud-google-credentials.json" <<'EOF'
{
  "universe_domain": "googleapis.com",
  "type": "external_account",
  "audience": "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ghost-wispr/providers/ghost-wispr",
  "subject_token_type": "urn:ietf:params:oauth:token-type:mtls",
  "token_url": "https://sts.mtls.googleapis.com/v1/token",
  "credential_source": {
    "certificate": {
      "certificate_config_location": "/opt/ghost-wispr/certificate_config.json"
    }
  },
  "service_account_impersonation_url": "https://iamcredentials.mtls.googleapis.com/v1/projects/-/serviceAccounts/ghost-wispr@example-project.iam.gserviceaccount.com:generateAccessToken"
}
EOF
printf '%s' '{
  "cert_configs": {
    "workload": {
      "cert_path": "/opt/ghost-wispr/device.crt",
      "key_path": "/home/ghost-wispr/.config/ghost-wispr-identity/key.pem"
    }
  }
}' >"$work_dir/gcloud-certificate_config.json"
for layout in gcloud reordered; do
  for name in google-credentials.json certificate_config.json; do
    if [[ $layout == gcloud ]]; then
      cp -- "$work_dir/gcloud-$name" "$deploy/$name"
    else
      jq -S --indent 4 . "$work_dir/$name" >"$deploy/$name"
    fi
  done
  before=$(pi_files)
  : >"$TEST_LOG"
  TEST_SELF_STATUS=$joined_status run_provision "$stage"
  expect_ran "credential files in the $layout layout"
  said 'credentials: same as pi.env renders' || fail "the same fields in the $layout layout were not the same: $(<"$work_dir/out")"
  [[ $(pi_files) == "$before" ]] || fail "provision.sh rewrote credential files in the $layout layout that it found the same"
done

# credentials-differ: a field that differs, one that the render does not write, and one that it
# writes but the file lacks, are each named, by file and field, and the files are kept byte for
# byte. P2's run reads this line.
begin credentials-differ
# expect_differ WANT: the run named the fields WANT ("FILE:FIELD ...") and kept every file.
expect_differ() {
  local line
  : >"$TEST_LOG"
  before=$(pi_files)
  TEST_SELF_STATUS=$joined_status run_provision "$stage"
  expect_ran "credentials that differ ($1)"
  line=$(sed -n 's/^ghost-wispr provision: credentials: differ: \(.*\); the installed files were kept$/\1/p' "$work_dir/out")
  [[ $line == "$1" ]] || fail "provision.sh named the fields [$line], want [$1]: $(<"$work_dir/out")"
  ! said 'credentials: same' || fail "provision.sh also said the credentials are the same: $(<"$work_dir/out")"
  [[ $(pi_files) == "$before" ]] || fail "provision.sh changed a credential file that differs ($1)"
}
jq '.audience = "//iam.googleapis.com/projects/999/locations/global/workloadIdentityPools/other/providers/other"' \
  "$work_dir/google-credentials.json" >"$deploy/google-credentials.json"
expect_differ 'google-credentials.json:audience'
cp -- "$work_dir/google-credentials.json" "$deploy/google-credentials.json"
jq 'del(.cert_configs.workload.key_path) | .extra = "a field gcloud does not write"' \
  "$work_dir/certificate_config.json" >"$deploy/certificate_config.json"
expect_differ 'certificate_config.json:cert_configs.workload.key_path certificate_config.json:extra'
printf 'not JSON\n' >"$deploy/google-credentials.json"
expect_differ 'google-credentials.json:(not JSON) certificate_config.json:cert_configs.workload.key_path certificate_config.json:extra'
cp -- "$work_dir/google-credentials.json" "$work_dir/certificate_config.json" "$deploy/"

# client-id: where the Pi keeps the client id of an earlier join, provision.sh says whether it is
# pi.env's PI_TAILNET_CLIENT_ID, and keeps the file. Where it keeps none, it says nothing of it.
begin client-id
for entry in "$client_id matches" "another-client-id differs from"; do
  printf '%s\n' "${entry%% *}" >"$deploy/tailscale-client-id"
  : >"$TEST_LOG"
  TEST_SELF_STATUS=$joined_status run_provision "$stage"
  expect_ran "the client id ${entry%% *}"
  said "tailnet client id: $deploy/tailscale-client-id ${entry#* } PI_TAILNET_CLIENT_ID" \
    || fail "provision.sh did not say that $deploy/tailscale-client-id ${entry#* } PI_TAILNET_CLIENT_ID: $(<"$work_dir/out")"
  [[ $(<"$deploy/tailscale-client-id") == "${entry%% *}" ]] || fail 'provision.sh changed tailscale-client-id'
done
rm -- "$deploy/tailscale-client-id"
: >"$TEST_LOG"
TEST_SELF_STATUS=$joined_status run_provision "$stage"
expect_ran 'no client id kept'
! said 'tailnet client id' || fail "provision.sh compared a client id the Pi does not keep: $(<"$work_dir/out")"

# ---------------------------------------------------------------------------------------------------
# newer-release: provision.sh from a newer release than the one the Pi runs, as a person runs it to
# apply a changed bootstrap file. It installs its update.sh, pi-lib.sh and pi.env before the update
# starts, so the update finds the installed set not the one deploy.verified lists, and keeps no
# .prev copy of it. So, where the installed set is the one deploy.verified lists and provision.sh
# would replace one of those three files, it first copies that set to its .prev copies, as
# update.sh does, and a failed install of the newer release rolls the Pi back to the release it
# runs. The update here is the real launcher and update.sh, against a fake release API, recorder,
# systemd and Tailscale. The Pi gets release a on a new Pi, release a again, which keeps the .prev
# copies as they are, then release b, whose binary never answers its commit.
begin newer-release
make_release a "$commit_a" "$commit_a"
make_release b "$commit_b" "$wrong_commit"
make_release c "$commit_c" "$commit_c"
new_pi
real_provision a
expect_ran 'release a on a new Pi'
said "ghost-wispr update: installed commit $commit_a" || fail "the update did not install release a: $(<"$work_dir/out")"
[[ $(head -n 1 "$deploy/deploy.verified") == "commit $commit_a" ]] || fail "deploy.verified does not name release a: $(<"$deploy/deploy.verified")"
release_a=$(installed_set)
[[ $release_a != *missing* && $release_a == *"answers $commit_a" ]] || fail "release a is not installed whole: $release_a"
real_provision a
expect_ran 'release a again'
said "ghost-wispr update: already current at commit $commit_a" || fail "the update did not find release a current: $(<"$work_dir/out")"
[[ -z $(find "$deploy" -maxdepth 1 -name '*.prev') ]] \
  || fail "a run of the release the Pi runs made .prev copies: $(find "$deploy" -maxdepth 1 -name '*.prev')"
[[ $(installed_set) == "$release_a" ]] || fail 'a run of the release the Pi runs changed its set'
real_provision b
expect_refused 'release b, whose binary never answers its commit' 'the nightly update failed'
[[ ! -s $TEST_LOG.errors ]] || fail "release b: $(<"$TEST_LOG.errors")"
said "kept the set of commit $commit_a, which the Pi runs, as its .prev copies" \
  || fail "provision.sh did not keep release a's set before it replaced update.sh: $(<"$work_dir/out")"
if ! said "did not answer /api/version with commit $commit_b" || ! said 'restored the previous release'; then
  fail "the update did not install release b and roll it back: $(<"$work_dir/out")"
fi
[[ $(installed_set) == "$release_a" ]] \
  || fail "after the failed install of release b, the Pi is not on release a's set: $(diff <(printf '%s\n' "$release_a") <(installed_set) || true)"
for file in ghost-wispr-update.service ghost-wispr-update.timer; do
  cmp -s -- "$TEST_RELEASES/b/ghost-wispr-pi-deploy/$file" "$units/$file" || fail "$file is not release b's, which provision.sh installed"
done
# Where the installed set is not the one deploy.verified lists, as after a swap that a kill cut
# short, the .prev copies hold the set verified last, and provision.sh leaves them as they are.
printf '# changed after the install\n' >>"$deploy/wait-for-network.sh"
prev_copies=$(cd -- "$deploy" && sha256sum -- *.prev)
: >"$TEST_LOG"
TEST_SELF_STATUS=$joined_status run_provision "$TEST_RELEASES/c/ghost-wispr-pi-deploy"
expect_ran 'release c on a set that deploy.verified does not list'
said 'so its .prev copies stay as they are' || fail "provision.sh did not say it kept the .prev copies as they are: $(<"$work_dir/out")"
[[ $(cd -- "$deploy" && sha256sum -- *.prev) == "$prev_copies" ]] || fail 'provision.sh changed the .prev copies of the set verified last'

# newer-release-certificate: the certificate the Pi runs goes back with its set. The nightly update
# installs each newer certificate with device.crt.version, its version, and a failed install puts
# both back from device.crt.prev and device.crt.version.prev. So provision.sh from a newer release
# keeps the Pi's certificate and its version file with the set: otherwise those copies still hold
# the certificate before it, and a failed install would put that one back. Where the Pi has no
# device.crt.version, as before its first nightly certificate, a stale device.crt.version.prev goes,
# as update.sh's keep_previous removes the .prev copy of a file the Pi does not have. Release d's
# binary stages certificate version 8 and never answers its commit.
begin newer-release-certificate
readonly commit_d=dddddddddddddddddddddddddddddddddddddddd
make_release d "$commit_d" "$wrong_commit" 8
# certificate_file NAME prints the content of DEPLOY_DIR/NAME, or "missing".
certificate_file() {
  if [[ -f $deploy/$1 ]]; then
    printf '%s\n' "$(<"$deploy/$1")"
  else
    printf 'missing\n'
  fi
}
for history in 'with a version file' 'with no version file'; do
  new_pi
  real_provision a
  expect_ran "release a on a new Pi, $history"
  # Certificate version 7 runs; an earlier nightly install kept version 6 as the .prev copies.
  printf 'certificate version 7\n' >"$deploy/device.crt"
  printf 'certificate version 6\n' >"$deploy/device.crt.prev"
  printf '6\n' >"$deploy/device.crt.version.prev"
  want_version_prev=missing
  if [[ $history == 'with a version file' ]]; then
    printf '7\n' >"$deploy/device.crt.version"
    want_version_prev=7
  fi
  release_a=$(installed_set)
  real_provision d
  expect_refused "release d with certificate version 8, $history" 'the nightly update failed'
  [[ ! -s $TEST_LOG.errors ]] || fail "release d, $history: $(<"$TEST_LOG.errors")"
  said 'pi-secrets: certificate version 8, not after 2027-12-01T00:00:00Z: newer and signs in' \
    || fail "control, $history: pi-secrets staged no certificate: $(<"$work_dir/out")"
  if ! said "did not answer /api/version with commit $commit_d" || ! said 'restored'; then
    fail "the update did not install release d and roll it back, $history: $(<"$work_dir/out")"
  fi
  [[ $(installed_set) == "$release_a" ]] || fail "after the failed install of release d, $history, the Pi is not on release a's set"
  [[ $(certificate_file device.crt) == 'certificate version 7' ]] \
    || fail "after the failed install of release d, $history, device.crt is [$(certificate_file device.crt)], not certificate version 7, which the Pi ran"
  [[ $(certificate_file device.crt.prev) == 'certificate version 7' ]] \
    || fail "provision.sh did not keep the Pi's certificate as device.crt.prev, $history: [$(certificate_file device.crt.prev)]"
  [[ $(certificate_file device.crt.version.prev) == "$want_version_prev" ]] \
    || fail "device.crt.version.prev is [$(certificate_file device.crt.version.prev)], want [$want_version_prev], $history"
  if [[ $history == 'with a version file' && $(certificate_file device.crt.version) != 7 ]]; then
    fail "after the failed install of release d, device.crt.version is [$(certificate_file device.crt.version)], not 7"
  fi
done

# bootstrap-repair: a bootstrap file that is broken on a Pi that runs release a, here an update unit
# whose ExecStart names no program, so no nightly run starts and no release can install a fix. The
# release that fixes it is a later one, c: provision.sh from c installs c's update unit, and its
# start runs c's launcher, whose update installs c.
begin bootstrap-repair
new_pi
real_provision a
expect_ran 'release a on a new Pi'
printf '%s\n' '[Service]' 'Type=oneshot' 'TimeoutStartSec=infinity' 'ExecStart=/opt/ghost-wispr/no-such-launcher.sh' \
  >"$units/ghost-wispr-update.service"
# The control: with that unit loaded, the update does not start.
status=0
XDG_RUNTIME_DIR=$runtime_dir TEST_REAL_UPDATE=1 TEST_JOURNAL=$work_dir/journal PATH="$bin:$PATH" \
  systemctl --user daemon-reload || status=$?
XDG_RUNTIME_DIR=$runtime_dir TEST_REAL_UPDATE=1 TEST_JOURNAL=$work_dir/journal PATH="$bin:$PATH" \
  systemctl --user start ghost-wispr-update.service 2>/dev/null || status=$?
if ((status == 0)) || ! grep -q 'status=203/EXEC' "$work_dir/journal"; then
  fail "control: the broken update unit started the update: $(<"$work_dir/journal")"
fi
real_provision c
expect_ran 'release c on a Pi whose update unit is broken'
cmp -s -- "$TEST_RELEASES/c/ghost-wispr-pi-deploy/ghost-wispr-update.service" "$units/ghost-wispr-update.service" \
  || fail "provision.sh did not install release c's update unit: $(<"$units/ghost-wispr-update.service")"
said "ghost-wispr update: installed commit $commit_c" || fail "the update did not install release c: $(<"$work_dir/out")"
[[ $(head -n 1 "$deploy/deploy.verified") == "commit $commit_c" ]] || fail "deploy.verified does not name release c: $(<"$deploy/deploy.verified")"
said 'already joined' || fail "provision.sh stopped before the join: $(<"$work_dir/out")"

# ---------------------------------------------------------------------------------------------------
# serve: serve.conf is the one source of Serve's setting. provision.sh applies each of its lines,
# turns off each live Serve HTTPS port it does not list, and on each port it lists, each mount
# other than /: tailscale serve --bg sets the mount / and keeps the others, so a mount left there
# would show as drift each night. Each line is also a literal call in the join's command. When it
# cannot read Serve's setting, or the setting is not serve.conf's after it applied it, it stops
# before the join.
begin serve
new_pi
jq -n --arg host "$TEST_SERVE_HOST" '{TCP: {"443": {HTTPS: true}, "8443": {HTTPS: true}},
  Web: {"\($host):443": {Handlers: {"/": {Proxy: "http://127.0.0.1:9999"}, "/debug": {Proxy: "http://127.0.0.1:8082"}}},
    "\($host):8443": {Handlers: {"/": {Proxy: "http://127.0.0.1:8081"}}}}}' >"$TEST_SERVE_STATE"
run_provision "$stage"
expect_ran 'a live port and a mount that serve.conf does not list'
readonly want_serve='{"TCP":{"443":{"HTTPS":true}},"Web":{"ghost-wispr.example-tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}'
[[ $(jq -cS . "$TEST_SERVE_STATE") == "$want_serve" ]] || fail "Serve is $(jq -cS . "$TEST_SERVE_STATE"), want $want_serve"
logged 'sudo tailscale serve --bg --https 443 http://127.0.0.1:8080' || fail "provision.sh did not apply serve.conf's line: $(<"$TEST_LOG")"
logged 'sudo tailscale serve --https=8443 off' || fail "provision.sh did not turn off port 8443: $(<"$TEST_LOG")"
said 'turned off Serve port 8443' || fail "provision.sh did not say it turned off port 8443: $(<"$work_dir/out")"
logged 'sudo tailscale serve --https=443 --set-path=/debug off' || fail "provision.sh did not turn off the mount /debug of port 443: $(<"$TEST_LOG")"
said 'turned off the Serve mount /debug of port 443' || fail "provision.sh did not say it turned off /debug: $(<"$work_dir/out")"
! grep -q 'serve --https=443 off' "$TEST_LOG" || fail "provision.sh turned off the port serve.conf lists: $(<"$TEST_LOG")"
# A Serve that provision.sh does not manage, a TCP forward, stays and is named: the run stops before
# the join, as the nightly drift line would say Serve: UNKNOWN.
new_pi
printf '%s\n' '{"TCP":{"2222":{"TCPForward":"127.0.0.1:22"}}}' >"$TEST_SERVE_STATE"
run_provision "$stage"
expect_refused 'a TCP forward' "is not serve.conf's"
[[ ! -e $TEST_LOG.join ]] || fail "provision.sh started the join although Serve is not serve.conf's"
rm -rf -- "$work_dir/two-ports-stage"
cp -a -- "$stage" "$work_dir/two-ports-stage"
printf '%s\n' '# The page, and a second port.' '443 http://127.0.0.1:8080' '8443 http://127.0.0.1:8081' >"$work_dir/serve.conf"
restage "$work_dir/two-ports-stage" serve.conf "$work_dir/serve.conf"
new_pi
run_provision "$work_dir/two-ports-stage"
expect_ran 'serve.conf with two lines'
[[ $(serve_now) == $'443 http://127.0.0.1:8080\n8443 http://127.0.0.1:8081' ]] \
  || fail "Serve is [$(serve_now | paste -sd ';' -)], want both lines of serve.conf"
join_command=$(<"$TEST_LOG.join")
for line in '443 http://127.0.0.1:8080' '8443 http://127.0.0.1:8081'; do
  [[ $join_command == *"tailscale serve --bg --https $line || rc=\$?;"* ]] \
    || fail "the join does not hold the Serve call for '$line': $join_command"
done
new_pi
TEST_SERVE_STATUS_EXIT=1 run_provision "$stage"
expect_refused 'an unreadable Serve setting' 'tailscale serve status'
[[ ! -e $TEST_LOG.join ]] || fail 'provision.sh started the join although it could not read Serve'

# ---------------------------------------------------------------------------------------------------
# update-fails: when the update fails, provision.sh prints that run's lines from the journal and
# fails, before it enables the recorder, touches Serve or joins.
begin update-fails
new_pi
printf '%s\n' 'ghost-wispr launcher: invocation test-invocation trigger none at none' \
  'ghost-wispr update: could not read the latest release from https://api.github.com/repos/sjawhar/ghost-wispr/releases/latest' \
  >"$work_dir/journal"
TEST_UPDATE_EXIT=1 TEST_JOURNAL=$work_dir/journal run_provision "$stage"
expect_refused 'a failed update' 'could not read the latest release'
said 'ghost-wispr-update.service' || fail "provision.sh did not name the update that failed: $(<"$work_dir/out")"
if grep -Eq 'enable ghost-wispr\.service|tailscale serve|systemd-run' "$TEST_LOG"; then
  fail "provision.sh went on after the update failed: $(<"$TEST_LOG")"
fi

# ---------------------------------------------------------------------------------------------------
# recorder-unit: the recorder's unit, which the nightly update installs. The service starts only
# once the Pi's clock is synchronized, so no session is named or stamped with the time a Pi with no
# battery-backed clock restores at boot: a user unit cannot be ordered after the system's
# time-sync.target, so the unit waits in a start step, after the network wait. Then it reads the
# secret lines of .env from Secret Manager, and goes on when that read fails (the -). Its start
# limit is longer than both waits, so a Pi whose clock never synchronizes still starts once the wait
# gives up instead of being killed and restarted for good. The paused listener's ring of room audio
# must not reach disk through a core dump or swap.
begin recorder-unit
unit="$repo_root/deploy/pi/files/ghost-wispr.service"
mapfile -t start_steps < <(grep -E '^Exec(StartPre|Start)=' "$unit")
want_steps=(ExecStartPre=/opt/ghost-wispr/wait-for-network.sh ExecStartPre=/opt/ghost-wispr/wait-for-time-sync.sh
  'ExecStartPre=-/opt/ghost-wispr/update.sh secrets' ExecStart=/opt/ghost-wispr/ghost-wispr)
[[ "${start_steps[*]}" == "${want_steps[*]}" ]] \
  || fail "ghost-wispr.service starts with [${start_steps[*]}], want [${want_steps[*]}]"
for limit in LimitCORE=0 MemorySwapMax=0; do
  grep -qx -- "$limit" "$unit" || fail "ghost-wispr.service does not set $limit"
done
network_wait=$(sed -n 's/.*GHOST_WISPR_NETWAIT_TIMEOUT:-\([0-9][0-9]*\).*/\1/p' "$repo_root/wait-for-network.sh")
sync_wait=$(sed -n 's/.*GHOST_WISPR_TIMESYNC_TIMEOUT:-\([0-9][0-9]*\).*/\1/p' "$repo_root/deploy/pi/files/wait-for-time-sync.sh")
[[ -n $network_wait && -n $sync_wait ]] || fail "cannot read the network wait ($network_wait) or the clock wait ($sync_wait)"
start_limit=$(sed -n 's/^TimeoutStartSec=//p' "$unit")
case $start_limit in
  *min) start_limit=$((${start_limit%min} * 60)) ;;
  *s) start_limit=${start_limit%s} ;;
  '') fail "ghost-wispr.service sets no TimeoutStartSec: the user manager's 90 s default is shorter than its waits" ;;
esac
# The network wait's last try can run 5 s past its limit (a 3 s connect, then a 2 s sleep).
((start_limit >= network_wait + 5 + sync_wait + 30)) \
  || fail "ghost-wispr.service's TimeoutStartSec (${start_limit}s) does not cover the network wait (${network_wait}s) and the clock wait (${sync_wait}s)"

# ---------------------------------------------------------------------------------------------------
# join: the detached join that the first run started re-authenticates the node with `tailscale up`
# and the tailnet's subnet routes off, sets Serve again, and runs the route check that provision.sh
# installed beside the token. It accepts the routes (`tailscale set --accept-routes`) only when the
# check passed. The check compares each IPv4 network of the Pi, tailscale0's excepted, with each
# peer's PrimaryRoutes: an accepted route that overlaps a network of the Pi would take the Pi's own
# traffic for that network. P2's command removes the release's directory before the join runs, and
# so does this case.
begin join
rm -rf -- "$work_dir/p2-stage"
readonly fixtures="$work_dir/fixtures"
mkdir -p "$fixtures"

# The Pi's networks, as `ip -4 -o addr` prints them: field 2 is the interface, field 4 the address
# and its prefix.
# F-ip-pi-posted: the office Pi's `ip -4 -o addr`, captured on the Pi on 2026-11-20 at 06:2xZ and
# posted on dispatch://INFRA-4587/comment/214b89f6-725d-4230-9e03-f94c8eae1187, with a neutral
# tailscale0 address.
cat >"$fixtures/F-ip-pi-posted" <<'EOF'
1: lo    inet 127.0.0.1/8 scope host lo
3: wlan0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0
4: tailscale0    inet 100.64.0.2/32 scope global tailscale0
EOF
# F-ip-pi: the same lines in the form iproute2 prints with -o for a real kernel, which always sends
# the address's lifetimes: the posted reading was cut at the `\` that comes before them (iproute2
# 6.15.0, ip/ipaddress.c:1674-1684 and ip/ip.c:302).
cat >"$fixtures/F-ip-pi" <<'EOF'
1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
3: wlan0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0\       valid_lft 85520sec preferred_lft 85520sec
4: tailscale0    inet 100.64.0.2/32 scope global tailscale0\       valid_lft forever preferred_lft forever
EOF
# F-ip-pod: iproute2 6.15.0's `ip -4 -o addr` under gVisor, captured verbatim in INFRA-4599's pod on
# 2026-11-20: no label and no lifetimes, and each line ends in one space.
printf '%s \n' '1: lo    inet 127.0.0.1/8 scope global dynamic' '2: eth0    inet 10.1.50.246/32 scope global dynamic' \
  >"$fixtures/F-ip-pod"
# F-ip-secondary: F-ip-pi with a secondary address on wlan0, in the shape ip/ipaddress.c prints it:
# the secondary flag, then the address's label.
{
  sed -n '1,2p' "$fixtures/F-ip-pi"
  printf '%s\n' '3: wlan0    inet 10.20.0.5/24 brd 10.20.0.255 scope global secondary wlan0:1\       valid_lft forever preferred_lft forever'
  sed -n '3p' "$fixtures/F-ip-pi"
} >"$fixtures/F-ip-secondary"
# F-ip-ptp: F-ip-pi with a point-to-point address, in the form ip/ipaddress.c prints when the local
# address and the peer address differ: field 4 is the local address, with no prefix.
{
  cat -- "$fixtures/F-ip-pi"
  printf '%s\n' '5: wg0    inet 10.0.0.1 peer 10.0.0.2/32 scope global wg0\       valid_lft forever preferred_lft forever'
} >"$fixtures/F-ip-ptp"
# F-ip-lo-only: only the lo and tailscale0 lines of F-ip-pi. F-ip-empty: no line.
sed -n '1p;3p' "$fixtures/F-ip-pi" >"$fixtures/F-ip-lo-only"
: >"$fixtures/F-ip-empty"
# F-ip-ptp-control: F-ip-ptp with an ESC in the interface name of its wg0 line.
{
  cat -- "$fixtures/F-ip-pi"
  printf '%s\033%s\n' '5: wg' '0    inet 10.0.0.1 peer 10.0.0.2/32 scope global wg0\       valid_lft forever preferred_lft forever'
} >"$fixtures/F-ip-ptp-control"
# F-ip-control-name: F-ip-pi with an ESC in the interface name of its wlan0 line.
{
  sed -n '1p' "$fixtures/F-ip-pi"
  printf '%s\033%s\n' '3: wl' 'an0    inet 192.168.1.136/24 brd 192.168.1.255 scope global dynamic noprefixroute wlan0\       valid_lft 85520sec preferred_lft 85520sec'
  sed -n '3p' "$fixtures/F-ip-pi"
} >"$fixtures/F-ip-control-name"

# The tailnet's statuses, as `tailscale status --json` prints them.
# F-status-router: a status in the shape of Tailscale v1.104.1's ipn/ipnstate/ipnstate.go, with
# neutral names and addresses: BackendState and Self, then Peer keyed by node key (line 81), each
# peer with its HostName and DNSName (lines 242 and 246), and PrimaryRoutes only when it routes
# (line 266, omitempty). router-a routes 10.1.0.0/16, INFRA-3709's Pod range, the route that
# INFRA-4587 adds. svc-frontdoor routes a service /32 and an IPv6 /128, the shape of the
# office Pi's reading on dispatch://INFRA-4587/comment/214b89f6-725d-4230-9e03-f94c8eae1187. devbox
# routes nothing.
cat >"$fixtures/F-status-router" <<'EOF'
{
  "Version": "1.104.1",
  "BackendState": "Running",
  "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"],
  "Self": {
    "ID": "n100",
    "PublicKey": "nodekey:0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f",
    "HostName": "ghost-wispr",
    "DNSName": "ghost-wispr.example-tailnet.ts.net.",
    "OS": "linux",
    "TailscaleIPs": ["100.64.0.2", "fd7a:115c:a1e0::2"],
    "Tags": ["tag:ghost-wispr"],
    "Online": true
  },
  "MagicDNSSuffix": "example-tailnet.ts.net",
  "Peer": {
    "nodekey:1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a": {
      "ID": "n101",
      "PublicKey": "nodekey:1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a",
      "HostName": "router-a",
      "DNSName": "router-a.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.3", "fd7a:115c:a1e0::3"],
      "PrimaryRoutes": ["10.1.0.0/16"],
      "Online": true
    },
    "nodekey:2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b": {
      "ID": "n102",
      "PublicKey": "nodekey:2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b",
      "HostName": "svc-frontdoor",
      "DNSName": "svc-frontdoor.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.4", "fd7a:115c:a1e0::4"],
      "PrimaryRoutes": ["100.64.10.1/32", "fd7a:115c:a1e0::a/128"],
      "Online": true
    },
    "nodekey:3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c": {
      "ID": "n103",
      "PublicKey": "nodekey:3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c",
      "HostName": "devbox",
      "DNSName": "devbox.example-tailnet.ts.net.",
      "OS": "linux",
      "TailscaleIPs": ["100.64.0.5", "fd7a:115c:a1e0::5"],
      "Online": true
    }
  }
}
EOF
# F-status-overlap: F-status-router with one more peer, office-router, which routes 192.168.0.0/16.
jq '("nodekey:" + "4d" * 32) as $key | .Peer[$key] = {ID: "n104", PublicKey: $key, HostName: "office-router",
    DNSName: "office-router.example-tailnet.ts.net.", OS: "linux", TailscaleIPs: ["100.64.0.6", "fd7a:115c:a1e0::6"],
    PrimaryRoutes: ["192.168.0.0/16"], Online: true}' "$fixtures/F-status-router" >"$fixtures/F-status-overlap"
# with_routes writes, into the fixture named first, the status fixture named third (F-status-router
# by default) with router-a's PrimaryRoutes replaced by the JSON list given second.
with_routes() {
  jq --argjson routes "$2" '(.Peer[] | select(.HostName == "router-a") | .PrimaryRoutes) = $routes' \
    "$fixtures/${3:-F-status-router}" >"$fixtures/$1"
}
# The other statuses change only the routes of F-status-router, but the last five:
# F-status-two-overlaps gives router-a of F-status-overlap a route that overlaps lo;
# F-status-peer-null has no peer yet (ipnstate.go:467-469 adds the map with the first peer);
# F-status-needs-login is a node logged out; F-status-empty and F-status-not-json are no status.
with_routes F-status-host-route '["192.168.1.10/32"]'
with_routes F-status-adjacent '["192.168.2.0/24"]'
with_routes F-status-tailscale-range '["100.64.0.0/10"]'
with_routes F-status-loopback '["127.0.0.0/8"]'
with_routes F-status-default-route '["0.0.0.0/0"]'
with_routes F-status-secondary '["10.20.0.0/16"]'
jq 'del(.Peer[].PrimaryRoutes)' "$fixtures/F-status-router" >"$fixtures/F-status-no-routes"
with_routes F-status-two-overlaps '["127.0.0.0/8"]' F-status-overlap
jq '.Peer = null' "$fixtures/F-status-router" >"$fixtures/F-status-peer-null"
jq '.BackendState = "NeedsLogin"' "$fixtures/F-status-router" >"$fixtures/F-status-needs-login"
: >"$fixtures/F-status-empty"
printf 'not json\n' >"$fixtures/F-status-not-json"
# F-status-large: F-status-router with its peers replaced by 1,000 peers, each with a long HostName
# and one /32 route in Tailscale's 100.64.0.0/10. It is more than Linux's 128 KiB limit for one
# argument, so the check reads it whole only when it does not pass the status as an argument.
# F-status-large-overlap adds a peer, office-router, which routes 192.168.0.0/16.
jq '.Peer = ([range(1000) | (10000 + . | tostring | .[1:]) as $n | {
    key: ("nodekey:" + "e" * 60 + $n),
    value: {ID: ("n2" + $n), PublicKey: ("nodekey:" + "e" * 60 + $n),
      HostName: ("peer-with-a-long-host-name-in-a-large-tailnet-" + $n),
      DNSName: ("peer-with-a-long-host-name-in-a-large-tailnet-" + $n + ".example-tailnet.ts.net."),
      OS: "linux", PrimaryRoutes: ["100.\(64 + (. / 256 | floor)).\(. % 256).1/32"], Online: true}
  }] | from_entries)' "$fixtures/F-status-router" >"$fixtures/F-status-large"
jq '("nodekey:" + "f" * 64) as $key | .Peer[$key] = {ID: "n3000", PublicKey: $key, HostName: "office-router",
    PrimaryRoutes: ["192.168.0.0/16"]}' "$fixtures/F-status-large" >"$fixtures/F-status-large-overlap"
# F-status-control-host: F-status-router with router-a's HostName holding control characters, the
# terminal escape sequence ESC ]0; that sets a window's title, ended by a BEL.
jq '(.Peer[] | select(.HostName == "router-a") | .HostName) = "router-a\u001b]0;pwned\u0007"' \
  "$fixtures/F-status-router" >"$fixtures/F-status-control-host"
# F-status-control-overlap: F-status-overlap with ESC, BEL and U+202E in office-router's HostName.
jq '(.Peer[] | select(.HostName == "office-router") | .HostName) = "office-router\u001b]0;pwned\u0007\u202e"' \
  "$fixtures/F-status-overlap" >"$fixtures/F-status-control-overlap"
# F-status-control-state: F-status-router with an ESC in its BackendState.
jq '.BackendState = "Needs\u001bLogin"' "$fixtures/F-status-router" >"$fixtures/F-status-control-state"
# F-status-control-route: F-status-router with an ESC at the end of router-a's route.
with_routes F-status-control-route '["10.1.0.0/16\u001b"]'

readonly accepted_prefix='ghost-wispr routes: no route of a peer overlaps a network of the Pi: '
readonly not_accepting="ghost-wispr routes: not accepting the tailnet's subnet routes: an accepted route would take the Pi's own traffic for that network"
readonly wlan0_overlap='wlan0 192.168.1.136/24 overlaps 192.168.0.0/16 of office-router'

# manager_expansion prints the command given as the service manager runs a systemd-run command when
# --expand-environment=no did not turn its expansion off (systemd v257, replace_env_full with no
# flags): each ${NAME} becomes the value of NAME in the environment, empty when NAME is unset, and
# each $$ becomes $. A ${ with a : before its }, and a ${ with no }, stay as written.
manager_expansion() {
  local text=$1 out='' word='' state=word char i
  for ((i = 0; i < ${#text}; i++)); do
    char=${text:i:1}
    case $state in
      word)
        word+=$char
        [[ $char != '$' ]] || state=dollar
        ;;
      dollar)
        # shellcheck disable=SC2016 # '${' is the two characters, as the manager reads them
        case $char in
          '{') out+=${word%?} word='${' state=variable ;;
          '$') out+=$word word='' state=word ;;
          *) word+=$char state=word ;;
        esac
        ;;
      variable)
        case $char in
          '}') out+=$(printenv -- "${word:2}" || true) word='' state=word ;;
          :) word+=$char state=word ;;
          *) word+=$char ;;
        esac
        ;;
    esac
  done
  printf '%s' "$out$word"
}

# run_join runs the first run's join command as its unit does, on F-ip-pi and the status fixture
# given second, with RouteAll set to the first argument beforehand. It starts a fresh log, writes a
# token, and puts the route check provision.sh installed back, each at the path the command names,
# because the command removes both. When the systemd-run line lacks --expand-environment=no, the
# command first gets the service manager's expansion. The exit status is kept in join_exit, stdout
# and stderr in $work_dir/join.out.
run_join() {
  local command options
  printf '%s\n' "$1" >"$TEST_ROUTE_ALL"
  [[ -f $first_join/route-overlap-check ]] \
    || fail "provision.sh installed no route check: $first_join/route-overlap-check is missing"
  cp -- "$first_join/route-overlap-check" "$join/route-overlap-check"
  printf 'id-token' >"$join/token"
  rm -f -- "$TEST_LOG.up-exit"
  : >"$TEST_LOG"
  command=$(<"$first_join/join")
  options=$(<"$first_join/systemd-run")
  options=${options%"$command"}
  [[ " $options " == *' --expand-environment=no '* ]] || command=$(manager_expansion "$command")
  join_exit=0
  TEST_NETWORKS="$fixtures/F-ip-pi" TEST_STATUS=$2 PATH="$bin:$PATH" \
    /bin/sh -c "$command" >"$work_dir/join.out" 2>&1 || join_exit=$?
}

# J1: no route overlaps a network of the Pi, so the join accepts the routes once the check passed.
# It removes the route check and the token.
run_join false "$fixtures/F-status-router"
j1="the join did not accept the tailnet's subnet routes although no route overlaps the Pi's networks: $(<"$work_dir/join.out")"
mapfile -t calls <"$TEST_LOG"
[[ $join_exit == 0 && ${#calls[@]} == 4 && ${calls[0]} == 'tailscale up '* \
  && ${calls[1]} == 'tailscale serve --bg --https 443 http://127.0.0.1:8080' \
  && ${calls[2]} == 'tailscale status --json' && ${calls[3]} == 'tailscale set --accept-routes' ]] || fail "$j1"
[[ $(<"$TEST_ROUTE_ALL") == true ]] || fail "$j1"
accepted=$(grep -e "^$accepted_prefix" "$work_dir/join.out") || fail "$j1"
[[ $accepted == *'wlan0 192.168.1.136/24'* && $accepted == *'10.1.0.0/16'* ]] || fail "$j1"
[[ ! -e $work_dir/join/route-overlap-check ]] || fail "$j1"
[[ ! -e $work_dir/join/token ]] || fail "the join left the token in the join directory (J1)"

# J2: office-router's route holds wlan0's network, so the join refuses: it runs no `tailscale set`,
# leaves RouteAll false, ends failed, and names the overlap.
run_join false "$fixtures/F-status-overlap"
j2="the join accepted the tailnet's subnet routes although 192.168.0.0/16 of office-router overlaps wlan0 192.168.1.136/24"
if grep -q '^tailscale set' "$TEST_LOG" || [[ $(<"$TEST_ROUTE_ALL") != false ]]; then
  fail "$j2"
fi
[[ $join_exit != 0 ]] || fail "$j2"
grep -Fq "$wlan0_overlap" "$work_dir/join.out" || fail "the join's refusal does not name the overlap: $(<"$work_dir/join.out")"
grep -Fq "$not_accepting" "$work_dir/join.out" || fail "the join's refusal does not name the overlap: $(<"$work_dir/join.out")"

# J3: a re-join of a Pi that accepts the routes, while a route now overlaps wlan0. `tailscale up`
# turns the routes off and so passes up.go's check of settings left out, and the check keeps them
# off.
run_join true "$fixtures/F-status-overlap"
up_exit='not run'
[[ ! -f $TEST_LOG.up-exit ]] || up_exit=$(<"$TEST_LOG.up-exit")
j3="a re-join with an overlapping route left RouteAll $(<"$TEST_ROUTE_ALL") (tailscale up: $up_exit)"
[[ $(<"$TEST_ROUTE_ALL") == false ]] || fail "$j3"
[[ $up_exit == 0 ]] || fail "$j3"
if grep -q '^tailscale set' "$TEST_LOG"; then
  fail "$j3"
fi
grep -Fq "$wlan0_overlap" "$work_dir/join.out" || fail "$j3"

# J4: a re-join of a Pi that accepts the routes, while no route overlaps: the check turns them on
# again.
run_join true "$fixtures/F-status-router"
[[ $(<"$TEST_ROUTE_ALL") == true ]] || fail "a re-join with no overlapping route left RouteAll false"
grep -Fxq 'tailscale set --accept-routes' "$TEST_LOG" || fail "a re-join with no overlapping route left RouteAll false"
[[ $join_exit == 0 ]] || fail "a re-join with no overlapping route left RouteAll false"

# J5: when `tailscale up` fails, the join stops there and ends failed. It still removes the token.
TEST_UP_EXIT=1 run_join false "$fixtures/F-status-router"
if [[ $join_exit == 0 ]] || grep -q -e '^tailscale serve' -e '^tailscale status --json$' -e '^tailscale set' "$TEST_LOG"; then
  fail "the join went on after tailscale up failed: $(<"$TEST_LOG")"
fi
[[ ! -e $work_dir/join/token ]] || fail "the join left the token in the join directory after tailscale up failed (J5)"

# J6: accepting the routes does not depend on Serve, so a failed `tailscale serve` still runs the
# check and accepts the routes, and the join ends failed.
TEST_SERVE_EXIT=1 run_join false "$fixtures/F-status-router"
if ! grep -Fxq 'tailscale set --accept-routes' "$TEST_LOG" || [[ $(<"$TEST_ROUTE_ALL") != true || $join_exit == 0 ]]; then
  fail "a failed tailscale serve stopped the route check, or the join reported success: $(<"$TEST_LOG")"
fi

# J7: when the check cannot read the tailnet's status, it refuses, and the join says which read failed.
TEST_STATUS_EXIT=1 run_join false "$fixtures/F-status-router"
if [[ $join_exit == 0 ]] || grep -q '^tailscale set' "$TEST_LOG" || [[ $(<"$TEST_ROUTE_ALL") != false ]] \
  || ! grep -Fq 'cannot compare' "$work_dir/join.out" || ! grep -Fq 'tailscale status' "$work_dir/join.out"; then
  fail "the join accepted routes it could not compare: $(<"$work_dir/join.out")"
fi

# J8: a failed `tailscale set` ends the join failed.
TEST_SET_EXIT=1 run_join false "$fixtures/F-status-router"
[[ $join_exit != 0 ]] || fail "the join reported success although tailscale set failed"

# route-check: route_row runs the route check provision.sh installed as the join unit runs it,
# /bin/sh on the file, on the network fixture and the status fixture given third and fourth. It
# fails unless the check exits with the code given second and its output names each text given
# after the fixtures. The first argument names the row.
begin route-check
route_row() {
  local row=$1 want=$2 got=0 text
  TEST_NETWORKS="$fixtures/$3" TEST_STATUS="$fixtures/$4" PATH="$bin:$PATH" \
    /bin/sh "$first_join/route-overlap-check" >"$work_dir/check.out" 2>&1 || got=$?
  [[ $got == "$want" ]] || fail "route check $row: exit $got, want $want: $(<"$work_dir/check.out")"
  shift 4
  for text in "$@"; do
    grep -Fq -- "$text" "$work_dir/check.out" || fail "route check $row does not name $text: $(<"$work_dir/check.out")"
  done
}

# clean_route_row runs route_row on its arguments, then fails when the check's output holds an ESC,
# a BEL or a U+202E: a terminal runs an escape sequence, and shows the text after a U+202E in
# another order.
clean_route_row() {
  route_row "$@"
  if grep -q -e $'\033' -e $'\007' -e $'\xe2\x80\xae' "$work_dir/check.out"; then
    fail "route check $1 prints a control or format character: $(cat -v -- "$work_dir/check.out")"
  fi
}

route_row 'G1 route holds the network' 1 F-ip-pi F-status-overlap 'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16'
route_row 'G2 network holds the route' 1 F-ip-pi F-status-host-route 'overlaps 192.168.1.10/32'
route_row 'G3 adjacent' 0 F-ip-pi F-status-adjacent "$accepted_prefix"
route_row "G4 the tree's route, a service /32, an IPv6 /128" 0 F-ip-pi F-status-router 'routes compared:' \
  '10.1.0.0/16' '100.64.10.1/32'
route_row "G5 Tailscale's range" 0 F-ip-pi F-status-tailscale-range "$accepted_prefix"
accepted=$(grep -F -- "$accepted_prefix" "$work_dir/check.out")
networks=${accepted#*"$accepted_prefix"}
networks=${networks%%'; routes compared:'*}
[[ $networks != *tailscale0* ]] \
  || fail "route check G5 Tailscale's range names tailscale0 among the Pi's networks: $(<"$work_dir/check.out")"
route_row 'G6 loopback' 1 F-ip-pi F-status-loopback 'lo 127.0.0.1/8 overlaps 127.0.0.0/8'
route_row 'G7 no peers' 0 F-ip-pi F-status-peer-null 'routes compared: none'
route_row 'G8 no route key' 0 F-ip-pi F-status-no-routes 'routes compared: none'
route_row 'G9 the posted form' 1 F-ip-pi-posted F-status-overlap 'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16'
route_row 'G10 another interface, gVisor form' 1 F-ip-pod F-status-router 'eth0 10.1.50.246/32 overlaps 10.1.0.0/16'
route_row 'G11 a secondary address' 1 F-ip-secondary F-status-secondary 'wlan0 10.20.0.5/24 overlaps 10.20.0.0/16'
route_row 'G12 two overlaps' 1 F-ip-pi F-status-two-overlaps "$wlan0_overlap" 'lo 127.0.0.1/8 overlaps 127.0.0.0/8 of router-a'
route_row 'G13 a default route' 1 F-ip-pi F-status-default-route 'overlaps 0.0.0.0/0'
large_size=$(wc -c <"$fixtures/F-status-large")
((large_size > 131072)) || fail "F-status-large is $large_size bytes, not more than 128 KiB"
route_row 'G14 a large status' 0 F-ip-pi F-status-large "$accepted_prefix"
route_row 'G14 a large status, with an overlap' 1 F-ip-pi F-status-large-overlap 'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16'
TEST_IP_EXIT=1 route_row 'G15 ip fails' 2 F-ip-pi F-status-router 'cannot compare'
route_row 'G16 ip prints nothing' 2 F-ip-empty F-status-router 'cannot compare'
route_row 'G17 only lo and tailscale0' 2 F-ip-lo-only F-status-router 'cannot compare'
route_row 'G18 point-to-point line' 2 F-ip-ptp F-status-router 'cannot compare' 'wg0' '10.0.0.1 peer 10.0.0.2/32'
TEST_STATUS_EXIT=1 route_row 'G19 status fails' 2 F-ip-pi F-status-router 'cannot compare'
route_row 'G20 status prints nothing' 2 F-ip-pi F-status-empty 'cannot compare'
route_row 'G21 status is not JSON' 2 F-ip-pi F-status-not-json 'cannot compare'
route_row 'G22 Tailscale not running' 2 F-ip-pi F-status-needs-login 'cannot compare' 'NeedsLogin'
clean_route_row 'G23 control characters in a host name' 0 F-ip-pi F-status-control-host 'router-a?]0;pwned?'
clean_route_row 'G24 control and format characters in a refusal' 1 F-ip-pi F-status-control-overlap \
  'wlan0 192.168.1.136/24 overlaps 192.168.0.0/16 of office-router?]0;pwned??'
clean_route_row 'G25 control characters in an ip line with no prefix' 2 F-ip-ptp-control F-status-router 'cannot compare' 'wg?0'
clean_route_row 'G26 control characters in BackendState' 2 F-ip-pi F-status-control-state 'Needs?Login'
clean_route_row 'G27 control characters in a route that is no prefix' 2 F-ip-pi F-status-control-route 'cannot compare' \
  '10.1.0.0/16?'
clean_route_row 'G28 control characters in an interface name' 1 F-ip-control-name F-status-overlap \
  'wl?an0 192.168.1.136/24 overlaps'

# ---------------------------------------------------------------------------------------------------
# manager-env: ghost-wispr's user manager, as the fake systemctl stands for it. Each of its units
# inherits its environment (systemctl --user set-environment or import-environment, an environment.d
# file, DefaultEnvironment=, the manager's own start-up environment), ghost-wispr.service with it.
# provision.sh reloads the manager and reads its environment before it changes anything, and
# refuses while the manager sets one of the variables that --models refuses in .env (below): the
# recorder would inherit it. The reload comes first, so an environment.d line written since the
# last reload counts, as it will at the recorder's next start. It names the variable and why, prints
# no value, and refuses as well when it cannot read the environment or reads an empty one.
begin manager-env
export TEST_MANAGER_ENV="$work_dir/manager-env" TEST_MANAGER_ENV_PENDING="$work_dir/manager-env-pending"
export TEST_UNITS="$work_dir/units"
mkdir -p -- "$TEST_UNITS"
readonly reload='systemctl --user daemon-reload' show_environment='systemctl --user show-environment'
# env_lines VARIABLE...: an environment as systemctl --user show-environment prints one, with these
# VARIABLEs among the few a manager has.
env_lines() { printf '%s\n' HOME=/home/ghost-wispr LANG=C.UTF-8 "$@" PATH=/usr/local/bin:/usr/bin:/bin; }
manager_env() { env_lines "$@" >"$TEST_MANAGER_ENV"; }
# The first run reloaded the manager and read its environment before its first change.
reload_at=$(line_of "$reload" "$work_dir/first.log")
read_at=$(line_of "$show_environment" "$work_dir/first.log")
change_at=$(line_of 'sudo apt-get update' "$work_dir/first.log")
[[ -n $reload_at && -n $read_at && -n $change_at && $reload_at -lt $read_at && $read_at -lt $change_at ]] \
  || fail "the first run did not reload the user manager and read its environment before it changed anything: $(<"$work_dir/first.log")"
# Each case is NAME|REASON|NOW|PENDING: the manager sets NOW now, and PENDING from its next reload.
# With no NAME the manager's environment reads empty.
for case in \
  'GHOST_WISPR_CONFIG|replace the installed config|GHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml|' \
  "GHOST_WISPR_ENVOY_HUB_URL|override the template's|GHOST_WISPR_ENVOY_HUB_URL=http://hub.example.test:9020|" \
  'GHOST_WISPR_CONFIG|replace the installed config||GHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml' \
  '|||'; do
  IFS='|' read -r name reason now pending <<<"$case"
  new_pi
  before=$(pi_files)
  manager_env ${now:+"$now"}
  [[ -n $name ]] || : >"$TEST_MANAGER_ENV"
  rm -f -- "$TEST_MANAGER_ENV_PENDING"
  [[ -z $pending ]] || env_lines "$pending" >"$TEST_MANAGER_ENV_PENDING"
  what=${name:-an empty environment}${pending:+ from an environment.d line since the last reload}
  run_provision "$stage"
  ((rc != 0)) || fail "provision.sh ran with $what in the user manager: $(<"$work_dir/out")"
  if [[ -n $name ]]; then
    said "ghost-wispr's user manager sets $name, which would $reason" \
      || fail "provision.sh did not name $name in the user manager's environment, and that it would $reason: $(<"$work_dir/out")"
  else
    said "could not read the environment of ghost-wispr's user manager" \
      || fail "provision.sh did not say it could not read an empty user manager environment: $(<"$work_dir/out")"
  fi
  ! grep -qE 'hub\.example\.test|elsewhere' "$work_dir/out" \
    || fail "provision.sh printed a value from the user manager's environment: $(<"$work_dir/out")"
  [[ $(<"$TEST_LOG") == "$reload"$'\n'"$show_environment" ]] \
    || fail "provision.sh did more than reload the user manager and read its environment before it refused $what: $(<"$TEST_LOG")"
  [[ $(pi_files) == "$before" ]] || fail "provision.sh changed a file before it refused $what"
done
rm -f -- "$TEST_MANAGER_ENV" "$TEST_MANAGER_ENV_PENDING"

# ---------------------------------------------------------------------------------------------------
# key: provision.sh --key makes the Pi's P-256 key, mode 0600 in a directory of mode 0700, and
# prints its certificate request for CN=ghost-wispr, for the infrastructure's bootstrap. The key
# never leaves the Pi, so a second --key refuses and keeps it.
begin key
new_pi
rm -rf -- "$key_dir"
run_provision "$stage" --key
expect_ran 'provision.sh --key'
[[ -f $key ]] || fail "provision.sh --key made no key at $key"
[[ $(stat -c %a -- "$key") == 600 && $(stat -c %a -- "$key_dir") == 700 ]] \
  || fail "the key has mode $(stat -c %a -- "$key") in a directory of mode $(stat -c %a -- "$key_dir"), not 600 in 700"
sed -n '/^-----BEGIN CERTIFICATE REQUEST-----$/,/^-----END CERTIFICATE REQUEST-----$/p' "$work_dir/out" >"$work_dir/request.pem"
[[ -s $work_dir/request.pem ]] || fail "provision.sh --key printed no certificate request: $(<"$work_dir/out")"
openssl req -in "$work_dir/request.pem" -noout -verify >/dev/null 2>&1 || fail 'the certificate request does not verify'
[[ $(openssl req -in "$work_dir/request.pem" -noout -subject -nameopt RFC2253) == 'subject=CN=ghost-wispr' ]] \
  || fail "the certificate request is for $(openssl req -in "$work_dir/request.pem" -noout -subject -nameopt RFC2253), not CN=ghost-wispr"
[[ $(openssl req -in "$work_dir/request.pem" -noout -pubkey) == "$(openssl pkey -in "$key" -pubout)" ]] \
  || fail 'the certificate request is not for the key provision.sh --key made'
key_text=$(openssl pkey -in "$key" -noout -text)
[[ $key_text == *'ASN1 OID: prime256v1'* ]] || fail "the key is no P-256 key: $key_text"
before=$(sha "$key")
run_provision "$stage" --key
expect_refused 'a second --key' "$key"
[[ $(sha "$key") == "$before" ]] || fail 'a second --key changed the key'

# ---------------------------------------------------------------------------------------------------
# certificate: provision.sh --certificate FILE installs the Pi's first certificate, byte for byte
# and mode 0644, once FILE is one PEM certificate whose public key is the key's; the deploy
# directory need not exist yet. A certificate for another key, one that does not parse, two in one
# file, and a second certificate are each refused, and install nothing. The nightly update installs
# each renewed certificate.
begin certificate
rm -rf -- "$deploy"
openssl req -x509 -new -key "$key" -subj /CN=ghost-wispr -days 2 -out "$work_dir/good.pem" >/dev/null 2>&1 \
  || fail 'openssl could not make a certificate for the key'
openssl req -x509 -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$work_dir/other.key" \
  -subj /CN=ghost-wispr -days 2 -out "$work_dir/other.pem" >/dev/null 2>&1 || fail 'openssl could not make another certificate'
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'bm90IGEgY2VydGlmaWNhdGU=' '-----END CERTIFICATE-----' >"$work_dir/bad.pem"
cat -- "$work_dir/good.pem" "$work_dir/other.pem" >"$work_dir/two.pem"
for entry in 'other.pem another key' 'bad.pem no PEM certificate' 'two.pem not one'; do
  run_provision "$stage" --certificate "$work_dir/${entry%% *}"
  expect_refused "the certificate ${entry%% *}" "${entry#* }"
  [[ ! -e $deploy/device.crt ]] || fail "provision.sh --certificate installed ${entry%% *}"
done
run_provision "$stage" --certificate "$work_dir/good.pem"
expect_ran 'provision.sh --certificate with the certificate for the key'
cmp -s -- "$work_dir/good.pem" "$deploy/device.crt" || fail "$deploy/device.crt is not the certificate given"
[[ $(stat -c %a -- "$deploy/device.crt") == 644 ]] || fail "device.crt has mode $(stat -c %a -- "$deploy/device.crt"), not 644"
said 'not after' || fail "provision.sh --certificate did not say when the certificate ends: $(<"$work_dir/out")"
run_provision "$stage" --certificate "$work_dir/good.pem"
expect_refused 'a second certificate' 'exists'
cmp -s -- "$work_dir/good.pem" "$deploy/device.crt" || fail 'a second --certificate changed device.crt'

# ---------------------------------------------------------------------------------------------------
# models: provision.sh --models DRIVE_ID, run by hand from the release's directory, installs the
# release's config with the shared drive and restarts the recorder. It writes no .env, which the
# nightly update owns. It refuses before it writes anything while a line of the deployed .env could
# set GHOST_WISPR_CONFIG, which would replace the installed config with another file, or a variable
# that would override the template's envoy_enabled, envoy_hub_url or concierge.envoy_url: a line
# that, past leading space and an export, starts with the name and =. It names the variable and
# why, prints no value, and leaves every file byte for byte as it was. GHOST_WISPR_CONFIG's reason
# comes first when .env sets both kinds. A match inside a multi-line quoted value refuses too: a
# false refusal only stops the step. systemd's EnvironmentFile= parser ends a line at a carriage
# return as at a line feed (NEWLINE "\n\r", src/basic/env-file.c), so a bare CR starts a line too; a
# case whose text holds CR LF is a CRLF file.
begin models
models_dir=$work_dir/models-deploy
mkdir -p -- "$models_dir"
# run_models [DIR] runs DIR/provision.sh --models, the release's directory by default, on the deploy
# directory models_dir.
run_models() {
  : >"$TEST_LOG"
  env -u XDG_RUNTIME_DIR PATH="$bin:$PATH" GHOST_WISPR_DEPLOY_DIR="$models_dir" \
    bash "${1:-$stage}/provision.sh" --models 0AAbCdEf_Gh-Ij >"$work_dir/out" 2>&1
}
models_files() { sha256sum "$models_dir/ghost-wispr.yaml" "$models_dir/.env"; }
for case in \
  'GHOST_WISPR_CONFIG|export GHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml' \
  $'GHOST_WISPR_CONFIG|GHOST_WISPR_ENVOY_ENABLED=false\nGHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml' \
  'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_ENABLED=false' \
  'GHOST_WISPR_ENVOY_HUB_URL|GHOST_WISPR_ENVOY_HUB_URL=http://hub.example.test:9020' \
  'GHOST_WISPR_CONCIERGE_ENVOY_URL|  export GHOST_WISPR_CONCIERGE_ENVOY_URL = http://hub.example.test:9020' \
  $'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_DISPATCH_TOKEN="first line\nGHOST_WISPR_ENVOY_ENABLED=true\nlast line"' \
  $'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ADDR=127.0.0.1:8080\rGHOST_WISPR_ENVOY_ENABLED=true' \
  $'GHOST_WISPR_CONCIERGE_ENVOY_URL|GHOST_WISPR_ADDR=127.0.0.1:8080\r\nGHOST_WISPR_CONCIERGE_ENVOY_URL=http://hub.example.test:9020\r'; do
  name=${case%%|*} content=${case#*|} eol='' reason="override the template's"
  [[ $content != *$'\r\n'* ]] || eol=$'\r'
  [[ $name != GHOST_WISPR_CONFIG ]] || reason='replace the installed config'
  printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
  printf "%s$eol\n%s\n" GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 "$content" >"$models_dir/.env"
  before=$(models_files)
  if run_models; then
    fail "--models installed with $name in .env: $(<"$work_dir/out")"
  fi
  grep -qF "can set $name, which would $reason" "$work_dir/out" \
    || fail "--models did not name $name, and that it would $reason: $(<"$work_dir/out")"
  ! grep -qE 'hub\.example\.test|elsewhere|first line|dg-1' "$work_dir/out" || fail "--models printed a value from .env: $(<"$work_dir/out")"
  [[ ! -s $TEST_LOG ]] || fail "--models changed the Pi before it refused $name: $(<"$TEST_LOG")"
  [[ $(models_files) == "$before" ]] || fail "--models changed a file when it refused $name"
done
# A checkout's deploy/pi/ is no release: --models refuses it before it writes anything, so the
# config it installs is always a release's, which the nightly update then keeps as current.
printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 >"$models_dir/.env"
before=$(models_files)
if run_models "$repo_root/deploy/pi"; then
  fail "--models installed a checkout's config: $(<"$work_dir/out")"
fi
grep -qF 'manifest.json' "$work_dir/out" || fail "--models did not say a checkout has no manifest.json: $(<"$work_dir/out")"
[[ ! -s $TEST_LOG ]] || fail "--models changed the Pi before it refused a checkout: $(<"$TEST_LOG")"
[[ $(models_files) == "$before" ]] || fail '--models changed a file when it refused a checkout'
# --models runs only from the release the Pi runs, as provision.sh does: another release's config
# on this release's binary is a set that no release holds. Where deploy.verified names another
# commit, it refuses before it runs any command. The control below runs it with deploy.verified
# naming the release's commit.
printf '%s\n' "commit $other_commit" >"$models_dir/deploy.verified"
before=$(models_files)
if run_models; then
  fail "--models installed the config of another release than the one the Pi runs: $(<"$work_dir/out")"
fi
if ! grep -qF "$other_commit" "$work_dir/out" || ! grep -qF 'The release the Pi runs' "$work_dir/out"; then
  fail "--models did not name the commit the Pi runs and where to get its release: $(<"$work_dir/out")"
fi
[[ ! -s $TEST_LOG ]] || fail "--models changed the Pi before it refused another release: $(<"$TEST_LOG")"
[[ $(models_files) == "$before" ]] || fail '--models changed a file when it refused another release'
printf '%s\n' "commit $release_commit" >"$models_dir/deploy.verified"
# An unreadable .env stops --models too, before it writes anything: the check cannot say it sets no
# override, and a read through a pipe must not pass for a clean one.
chmod 000 "$models_dir/.env"
status=0
run_models || status=$?
# The check itself fails, not only a later step, which would also stop the step.
! bash -c 'source "$1" && config_override_names "$2"' _ "$repo_root/deploy/pi/provision.sh" "$models_dir/.env" >/dev/null 2>&1 \
  || fail 'config_override_names read an unreadable .env as one that sets no override'
chmod 600 "$models_dir/.env"
((status != 0)) || fail "--models installed with an unreadable .env: $(<"$work_dir/out")"
grep -qF "could not read $models_dir/.env" "$work_dir/out" || fail "--models did not say it could not read .env: $(<"$work_dir/out")"
[[ ! -s $TEST_LOG ]] || fail "--models changed the Pi before it refused an unreadable .env: $(<"$TEST_LOG")"
[[ $(models_files) == "$before" ]] || fail "--models changed a file when it refused an unreadable .env"
# --models refuses too, before it changes anything, while ghost-wispr's user manager sets one of
# those variables in its own environment. Before it reads the environment it reloads the manager, so
# an environment.d line written since the last reload counts, as it will at the recorder's next
# start, and it refuses while either unit is not loaded or has a drop-in, which can set the
# recorder's environment (Environment=, EnvironmentFile=) past every file it checks. It reads the
# names from systemctl --user show-environment, names the variable and why, and prints no value. It
# refuses as well when it cannot read that environment, or reads an empty one: a user manager
# always holds some variables, so an empty reading is a blind one.
show_unit() { printf 'systemctl --user show -p LoadState -p DropInPaths %s' "$1"; }
models_reads="$reload
$(show_unit ghost-wispr.service)
$(show_unit ghost-wispr-update.service)
$show_environment"
readonly models_reads
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 >"$models_dir/.env"
# A line the manager reads only from its next reload: --models reloads it before it reads.
manager_env
env_lines GHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml >"$TEST_MANAGER_ENV_PENDING"
printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
before=$(models_files)
if run_models; then
  fail "--models installed with GHOST_WISPR_CONFIG from an environment.d line written since the user manager's last reload: $(<"$work_dir/out")"
fi
grep -qF "ghost-wispr's user manager sets GHOST_WISPR_CONFIG, which would replace the installed config" "$work_dir/out" \
  || fail "--models did not name GHOST_WISPR_CONFIG from an environment.d line written since the last reload: $(<"$work_dir/out")"
[[ $(<"$TEST_LOG") == "$models_reads" ]] \
  || fail "--models did more than reload the user manager and read it before it refused: $(<"$TEST_LOG")"
[[ $(models_files) == "$before" ]] || fail "--models changed a file when it refused a pending environment.d line"
rm -f -- "$TEST_MANAGER_ENV" "$TEST_MANAGER_ENV_PENDING"
# A unit not loaded, or with a drop-in: each case is UNIT|FILE|CONTENT|SAYS, FILE in $TEST_UNITS.
readonly dropin=/home/ghost-wispr/.config/systemd/user/ghost-wispr.service.d/trace.conf
for case in \
  "ghost-wispr.service|dropins|$dropin|ghost-wispr.service has a drop-in ($dropin), which can change how it runs" \
  'ghost-wispr-update.service|dropins|/etc/systemd/user/ghost-wispr-update.service.d/limit.conf|ghost-wispr-update.service has a drop-in (/etc/systemd/user/ghost-wispr-update.service.d/limit.conf)' \
  'ghost-wispr-update.service|load|not-found|ghost-wispr-update.service is not-found, not loaded'; do
  IFS='|' read -r unit file content says <<<"$case"
  printf '%s\n' "$content" >"$TEST_UNITS/$file-$unit"
  printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
  before=$(models_files)
  if run_models; then
    fail "--models installed with $unit's $file $content: $(<"$work_dir/out")"
  fi
  rm -f -- "$TEST_UNITS/$file-$unit"
  grep -qF "$says" "$work_dir/out" || fail "--models did not say: $says: $(<"$work_dir/out")"
  expected="$reload"$'\n'"$(show_unit ghost-wispr.service)"
  [[ $unit == ghost-wispr.service ]] || expected+=$'\n'"$(show_unit "$unit")"
  [[ $(<"$TEST_LOG") == "$expected" ]] \
    || fail "--models did more than reload the user manager and read the units before it refused $unit's $file: $(<"$TEST_LOG")"
  [[ $(models_files) == "$before" ]] || fail "--models changed a file when it refused $unit's $file"
done
for case in \
  'GHOST_WISPR_CONFIG|GHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml' \
  $'GHOST_WISPR_CONFIG|GHOST_WISPR_ENVOY_ENABLED=false\nGHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml' \
  'GHOST_WISPR_ENVOY_ENABLED|GHOST_WISPR_ENVOY_ENABLED=false' \
  'GHOST_WISPR_ENVOY_HUB_URL|GHOST_WISPR_ENVOY_HUB_URL=http://hub.example.test:9020' \
  'GHOST_WISPR_CONCIERGE_ENVOY_URL|GHOST_WISPR_CONCIERGE_ENVOY_URL=http://hub.example.test:9020'; do
  name=${case%%|*} reason="override the template's"
  [[ $name != GHOST_WISPR_CONFIG ]] || reason='replace the installed config'
  manager_env "${case#*|}"
  printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
  before=$(models_files)
  if run_models; then
    fail "--models installed with $name in the user manager's environment: $(<"$work_dir/out")"
  fi
  grep -qF "ghost-wispr's user manager sets $name, which would $reason" "$work_dir/out" \
    || fail "--models did not name $name in the user manager's environment, and that it would $reason: $(<"$work_dir/out")"
  ! grep -qE 'hub\.example\.test|elsewhere' "$work_dir/out" \
    || fail "--models printed a value from the user manager's environment: $(<"$work_dir/out")"
  [[ $(<"$TEST_LOG") == "$models_reads" ]] \
    || fail "--models did more than reload the user manager and read it before it refused $name: $(<"$TEST_LOG")"
  [[ $(models_files) == "$before" ]] || fail "--models changed a file when it refused $name in the user manager's environment"
done
for broken in 'systemctl fails' 'an empty environment'; do
  if [[ $broken == 'systemctl fails' ]]; then
    manager_env
    export TEST_MANAGER_ENV_EXIT=1
  else
    : >"$TEST_MANAGER_ENV"
  fi
  printf 'old: config\n' >"$models_dir/ghost-wispr.yaml"
  before=$(models_files)
  status=0
  run_models || status=$?
  unset TEST_MANAGER_ENV_EXIT
  ((status != 0)) || fail "--models installed when $broken: $(<"$work_dir/out")"
  grep -qF "could not read the environment of ghost-wispr's user manager" "$work_dir/out" \
    || fail "--models did not say it could not read the user manager's environment when $broken: $(<"$work_dir/out")"
  [[ $(<"$TEST_LOG") == "$models_reads" ]] \
    || fail "--models did more than reload the user manager and read it when $broken: $(<"$TEST_LOG")"
  [[ $(models_files) == "$before" ]] || fail "--models changed a file when $broken"
done
# The control: a .env and a user manager's environment that set none install. The .env has each
# override commented out and longer names that start with one; the environment has those longer
# names, a name inside another variable's value, and a value with a line break, quoted onto one line
# as systemctl prints it. --models still reloaded the manager and read the units and the environment,
# first. The installed config is the release's template with the shared drive, mode 0640, and .env
# is as it was.
printf '%s\n' GHOST_WISPR_DEEPGRAM_API_KEY=dg-1 '# GHOST_WISPR_CONFIG=/srv/elsewhere/ghost-wispr.yaml' \
  '# GHOST_WISPR_ENVOY_HUB_URL=http://hub.example.test:9020' '# GHOST_WISPR_ENVOY_ENABLED=true' \
  '# GHOST_WISPR_CONCIERGE_ENVOY_URL=http://hub.example.test:9020' 'GHOST_WISPR_ENVOY_ENABLED_BEFORE=true' \
  'GHOST_WISPR_CONFIG_DIR=/srv/elsewhere' >"$models_dir/.env"
cp -- "$models_dir/.env" "$work_dir/models.env"
manager_env 'GHOST_WISPR_CONFIG_DIR=/srv/elsewhere' 'GHOST_WISPR_ENVOY_ENABLED_BEFORE=true' \
  'LESSOPEN=GHOST_WISPR_CONFIG=/srv/elsewhere' "GHOST_WISPR_DISPATCH_TOKEN=\$'first\\nGHOST_WISPR_CONFIG=x'"
rm -f -- "$TEST_LOG.errors"
run_models || fail "--models refused a .env and a user manager's environment that set no override: $(<"$work_dir/out")"
[[ ! -s $TEST_LOG.errors ]] || fail "--models: $(<"$TEST_LOG.errors")"
[[ $(head -n 4 "$TEST_LOG") == "$models_reads" ]] \
  || fail "--models installed without first reloading the user manager and reading it: $(<"$TEST_LOG")"
for line in 'gdrive_folder_id: "0AAbCdEf_Gh-Ij"' 'gdrive_sync_enabled: true' 'gc_enabled: true'; do
  [[ $(grep -cFx -- "$line" "$models_dir/ghost-wispr.yaml" || true) == 1 ]] || fail "--models did not install the config with '$line' once"
done
diff <(awk '!/^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):/' "$stage/ghost-wispr.yaml") \
  <(awk '!/^(gdrive_folder_id|gdrive_sync_enabled|gc_enabled):/' "$models_dir/ghost-wispr.yaml") >&2 \
  || fail "--models did not install the release's template with the shared drive (diff above)"
[[ $(stat -c %a -- "$models_dir/ghost-wispr.yaml") == 640 ]] || fail "the config has mode $(stat -c %a -- "$models_dir/ghost-wispr.yaml"), not 640"
cmp -s -- "$work_dir/models.env" "$models_dir/.env" || fail '--models changed .env, which the nightly update owns'
grep -Fxq 'systemctl --user restart ghost-wispr.service' "$TEST_LOG" || fail "--models did not restart the recorder: $(<"$TEST_LOG")"
grep -qF 'Settings saved on the page stay in the local settings file' "$work_dir/out" \
  || fail "--models did not say that settings saved on the page stay: $(<"$work_dir/out")"

for name in "${case_names[@]}"; do
  [[ $ran_cases == *" $name "* ]] || fail "the case $name never ran"
done
printf 'provision run tests passed: %d cases\n' "${#case_names[@]}"
