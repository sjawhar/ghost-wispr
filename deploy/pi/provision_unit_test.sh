#!/usr/bin/env bash
# Checks, on a real systemd, what the transient system unit that a provisioning run is does: the
# unit that pi-lib.sh's provision_unit_command starts, as provision.sh starts its run, with
# provision.sh's stop traps (provision_stop_traps). It needs systemd as PID 1 and passwordless sudo,
# as a GitHub runner and a dev box have, and fails, saying so, without them. For each command that
# GPT-6.1 Sol's review of #203 (round 5) found keeping a run's lock after the run died, a nested
# $(...), a pipeline, jq, install and openssl, and for timeout, which runs each tailscale call and
# moves itself to a process group of its own, each made to hang, it checks that:
#   - while the run lives, pi-lib.sh's provision_units_busy names the unit, when it became active
#     and its main PID, and a second start of the unit fails;
#   - systemctl stop, systemctl kill --signal=HUP and a SIGKILL of the main process each end every
#     process of the run within 0.5 s, the hung command among them, and leave the unit inactive;
#   - the run's own command, as systemd-run --wait gives it back, ends 143 after a stop and 129
#     after SIGHUP, never as a success, and not 0 after the kill.
# The units are named for this test and its PID, and it stops each it started when it ends.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
# shellcheck source=deploy/pi/pi-lib.sh
source "$script_dir/pi-lib.sh"

fail() {
  printf 'provision unit test FAILED: %s\n' "$*" >&2
  exit 1
}

[[ $(ps -o comm= -p 1) == systemd ]] || fail "systemd is not PID 1 here ($(ps -o comm= -p 1)): this test needs a real systemd"
sudo -n true 2>/dev/null || fail 'this test needs passwordless sudo, to start transient system units'

work_dir=$(mktemp -d)
readonly work_dir
chmod 0755 "$work_dir"
started=()
cleanup() {
  local unit
  for unit in "${started[@]}"; do
    sudo -n systemctl stop "$unit.service" 2>/dev/null || true
  done
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

user=$(id -un)
readonly user prefix="ghost-wispr-provision-test-$$"

# The run: provision.sh's stop traps, then one command of the shape given that never ends.
cat >"$work_dir/run.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=/dev/null
source "$1/pi-lib.sh"
provision_stop_traps
case $2 in
  nested) names=$(inner=$(sleep 600); printf '%s' "$inner"); printf '%s\n' "$names" ;;
  pipeline) id -nG | (sleep 600; cat) | grep -c . ;;
  jq) value=$( (sleep 600; printf '{}') | jq -r .a); printf '%s\n' "$value" ;;
  install) (sleep 600; printf 'data\n') | install -m 0600 /dev/stdin "$3/installed" ;;
  openssl) (sleep 600; printf 'data\n') | openssl dgst -sha256 ;;
  timeout) value=$(timeout 600 sleep 600); printf '%s\n' "$value" ;;
esac
EOF

# unit_processes UNIT prints the PIDs in UNIT's control group, one a line.
unit_processes() {
  local group
  group=$(systemctl show -p ControlGroup --value "$1.service")
  if [[ -n $group && -r /sys/fs/cgroup$group/cgroup.procs ]]; then
    cat -- "/sys/fs/cgroup$group/cgroup.procs"
  fi
}

for shape in nested pipeline jq install openssl timeout; do
  for end in stop hup kill; do
    unit=$prefix-$shape-$end
    what="$shape, $end"
    started+=("$unit")
    provision_unit_command "$unit" "$user"
    # shellcheck disable=SC2024 # The test reads the run's output as its own user.
    sudo -n "${pl_unit_command[@]}" /bin/bash "$work_dir/run.sh" "$script_dir" "$shape" "$work_dir" \
      </dev/null >"$work_dir/$unit.out" 2>&1 &
    client=$!
    processes=''
    for _ in {1..100}; do
      processes=$(unit_processes "$unit")
      # The run's shell, the hung command and the commands of its shape around it.
      (($(grep -c . <<<"$processes") >= 3)) && break
      sleep 0.1
    done
    (($(grep -c . <<<"$processes") >= 3)) || fail "$what: the run never reached its hung command: $processes; $(cat -- "$work_dir/$unit.out")"
    main=$(systemctl show -p MainPID --value "$unit.service")
    since=$(systemctl show -p ActiveEnterTimestamp --value "$unit.service")
    busy=$(provision_units_busy "$unit") || fail "$what: provision_units_busy could not read the unit"
    [[ $busy == "$unit.service is active since $since, main PID $main" ]] \
      || fail "$what: provision_units_busy says '$busy' of the live run, main PID $main"
    # shellcheck disable=SC2024 # The test reads the second start's output as its own user.
    if sudo -n "${pl_unit_command[@]}" /bin/true </dev/null >"$work_dir/$unit.second" 2>&1; then
      fail "$what: a second start of the unit ran while the run lives"
    fi
    case $end in
      stop) sudo -n systemctl stop "$unit.service" ;;
      hup) sudo -n systemctl kill --signal=HUP "$unit.service" ;;
      kill) sudo -n kill -KILL "$main" ;;
    esac
    status=0
    wait "$client" || status=$?
    sleep 0.5
    busy=$(provision_units_busy "$unit") || fail "$what: provision_units_busy could not read the unit"
    [[ -z $busy ]] || fail "$what: the unit is still busy 0.5 s after the run ended: $busy"
    while IFS= read -r pid; do
      [[ -z $pid ]] || ! kill -0 "$pid" 2>/dev/null \
        || fail "$what: process $pid of the run is left 0.5 s after it ended: $(ps -o args= -p "$pid")"
    done <<<"$processes"
    case $end in
      stop) want=143 ;;
      hup) want=129 ;;
      kill) want=nonzero ;;
    esac
    if [[ $want == nonzero ]]; then
      ((status != 0)) || fail "$what: the killed run's command ended 0"
    else
      ((status == want)) || fail "$what: the run's command ended $status, want $want: $(cat -- "$work_dir/$unit.out")"
    fi
  done
done

# The unit's settings (pi-lib.sh's provision_unit_settings) carry PATH and the provisioning settings,
# never a credential of the caller's environment: a unit's Environment property is readable by any
# local user (GPT-6.1 Sol's review of #203, round 6). Break it catches: settings made of every
# exported GHOST_WISPR_ variable.
# The two credential names the recorder's deployment exports, each given a dummy value.
credential_names=(GHOST_WISPR_DISPATCH_TOKEN GHOST_WISPR_DEEPGRAM_API_KEY)
export "${credential_names[0]}=dummy-dispatch-canary" "${credential_names[1]}=dummy-deepgram-canary" \
  GHOST_WISPR_DEPLOY_DIR=/opt/ghost-wispr-unit-test
unit=$prefix-settings
started+=("$unit")
provision_unit_settings
provision_unit_command "$unit" "$user"
# shellcheck disable=SC2154 # pi-lib.sh's provision_unit_settings sets it.
sudo -n "${pl_unit_command[@]}" "${pl_unit_settings[@]}" /bin/sleep 60 </dev/null >/dev/null 2>&1 &
client=$!
for _ in {1..100}; do
  [[ $(systemctl show -p ActiveState --value "$unit.service") == active ]] && break
  sleep 0.1
done
environment=$(systemctl show -p Environment --value "$unit.service")
[[ $environment == *GHOST_WISPR_DEPLOY_DIR=/opt/ghost-wispr-unit-test* ]] \
  || fail "control: the unit's Environment lacks a provisioning setting: $environment"
[[ $environment != *canary* ]] || fail "the unit's Environment, which any local user reads, carries a credential: $environment"
if id nobody >/dev/null 2>&1; then
  other=$(sudo -n -u nobody systemctl show -p Environment --value "$unit.service") \
    || fail "control: another user could not read the unit's Environment"
  [[ $other != *canary* ]] || fail "another user read a credential in the unit's Environment: $other"
fi
sudo -n systemctl stop "$unit.service"
wait "$client" || true
unset GHOST_WISPR_DISPATCH_TOKEN GHOST_WISPR_DEEPGRAM_API_KEY GHOST_WISPR_DEPLOY_DIR
printf 'provision unit tests passed: 6 commands, each ended by a stop, SIGHUP and a kill; no credential in the settings\n'
