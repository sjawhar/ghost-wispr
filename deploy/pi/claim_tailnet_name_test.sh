#!/usr/bin/env bash
# Runs deploy/pi/claim-tailnet-name.sh against a fake Tailscale API (a fake curl over a device
# list) and checks that it removes only the temporary operator-owned device that holds the name,
# renames the tagged device and reads the name back, deletes nothing when a holder is anything
# else, stops on an API failure, calls only the Tailscale API, and never puts the API key in an
# argument or in its output.
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script="$script_dir/claim-tailnet-name.sh"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'claim tailnet name test FAILED: %s\n' "$*" >&2
  exit 1
}

readonly KEY=fake-tailscale-api-key-for-this-test
export TEST_KEY=$KEY
# An API override left in the environment must not send the key elsewhere: the script has none,
# and the fake curl refuses every URL that is not the Tailscale API.
export GHOST_WISPR_TAILSCALE_API=http://127.0.0.1:9/not-the-tailscale-api
mkdir -p "$work_dir/bin"

# The fake curl takes its Authorization header only from a curl config on stdin, records every
# argument, and answers from TEST_STATE, a device list it changes as the real API would.
# TEST_FAIL_METHOD makes it answer HTTP 403 to every call with that method, and TEST_RENAME_NOOP
# makes a rename answer 200 and keep the old name.
cat >"$work_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$TEST_ARGV"
method=GET format= url= config=false
while [[ $# -gt 0 ]]; do
  case $1 in
    -K) [[ $2 == - ]] || exit 90; config=true; shift 2 ;;
    -X) method=$2; shift 2 ;;
    -w) format=$2; shift 2 ;;
    -H | --data) shift 2 ;;
    -sS) shift ;;
    *) url=$1; shift ;;
  esac
done
[[ $config == true ]] || { printf 'fake curl: no config on stdin\n' >&2; exit 91; }
[[ $(cat) == "header = \"Authorization: Bearer $TEST_KEY\"" ]] || { printf 'fake curl: wrong header on stdin\n' >&2; exit 92; }
[[ $url == https://api.tailscale.com/api/v2/* ]] || { printf 'fake curl: %s is not the Tailscale API\n' "$url" >&2; exit 93; }
path=${url#https://api.tailscale.com/api/v2}
status=200 body=
if [[ -n ${TEST_FAIL_METHOD:-} && $method == "$TEST_FAIL_METHOD" ]]; then
  status=403 body='{"message":"forbidden"}'
  printf '%s\n%s' "$body" "$status"
  exit 0
fi
case "$method $path" in
  'GET /tailnet/-/devices?fields=all') body=$(<"$TEST_STATE") ;;
  GET\ /device/*)
    id=${path#/device/}
    body=$(jq -c --arg id "$id" '.devices[] | select(.id == $id)' "$TEST_STATE")
    [[ -n $body ]] || status=404
    ;;
  DELETE\ /device/*)
    id=${path#/device/}
    jq --arg id "$id" '.devices |= map(select(.id != $id))' "$TEST_STATE" >"$TEST_STATE.new"
    mv "$TEST_STATE.new" "$TEST_STATE"
    ;;
  POST\ /device/*/name)
    id=${path#/device/}
    id=${id%/name}
    # TEST_RENAME_NOOP keeps the old name. Otherwise Tailscale gives the next free name when the one
    # asked for is taken.
    if [[ -n ${TEST_RENAME_NOOP:-} ]]; then
      name=$(jq -r --arg id "$id" '.devices[] | select(.id == $id) | .name' "$TEST_STATE")
    elif jq -e '.devices[] | select(.name == "ghost-wispr.tail12ec1c.ts.net")' "$TEST_STATE" >/dev/null; then
      name=ghost-wispr-2.tail12ec1c.ts.net
    else
      name=ghost-wispr.tail12ec1c.ts.net
    fi
    jq --arg id "$id" --arg name "$name" '.devices |= map(if .id == $id then .name = $name else . end)' \
      "$TEST_STATE" >"$TEST_STATE.new"
    mv "$TEST_STATE.new" "$TEST_STATE"
    ;;
  *) status=400 ;;
esac
printf '%s' "$body"
[[ -z $format ]] || printf '\n%s' "$status"
EOF
chmod +x "$work_dir/bin/curl"

readonly tagged='{"id":"222","name":"ghost-wispr-1.tail12ec1c.ts.net","hostname":"ghost-wispr","user":"","tags":["tag:ghost-wispr"],"connectedToControl":true}'
readonly temporary='{"id":"111","name":"ghost-wispr.tail12ec1c.ts.net","hostname":"ghost-wispr","user":"operator@example.test","tags":null,"connectedToControl":false}'
# Passes every holder clause but does not hold the name, so it must stay.
readonly namesake='{"id":"555","name":"ghost-wispr-2.tail12ec1c.ts.net","hostname":"ghost-wispr","user":"operator@example.test","tags":null,"connectedToControl":false}'
readonly bystander='{"id":"333","name":"laptop.tail12ec1c.ts.net","hostname":"laptop","user":"operator@example.test","tags":null,"connectedToControl":false}'

# run_case NAME DEVICES... runs the script on a tailnet holding DEVICES, fails the test if the key
# is in its stdout or stderr, and returns the script's exit status.
run_case() {
  local name=$1 rc=0
  shift
  case_dir="$work_dir/$name"
  mkdir -p "$case_dir"
  jq -cn '{devices: $ARGS.positional | map(fromjson)}' --args "$@" >"$case_dir/state.json"
  cp "$case_dir/state.json" "$case_dir/before.json"
  : >"$case_dir/argv"
  TEST_STATE="$case_dir/state.json" TEST_ARGV="$case_dir/argv" TAILSCALE_API_KEY=$KEY PATH="$work_dir/bin:$PATH" \
    bash "$script" >"$case_dir/out" 2>&1 || rc=$?
  no_key_in_output "$name"
  return "$rc"
}
names() { jq -c '[.devices[] | {id, name}] | sort_by(.id)' "$case_dir/state.json"; }
unchanged() { cmp -s "$case_dir/before.json" "$case_dir/state.json"; }
no_key_in_argv() {
  if grep -qF "$KEY" "$case_dir/argv"; then
    fail "$1: the API key was an argument to curl"
  fi
}
# The message names the case only: printing the output would print the key.
no_key_in_output() {
  if grep -qF "$KEY" "$case_dir/out"; then
    fail "$1: the API key is in the script's output"
  fi
}

# The temporary device holds the name: it goes, and the tagged one takes the name.
run_case temporary-holder "$tagged" "$temporary" "$bystander" "$namesake" || fail "temporary-holder: $(<"$case_dir/out")"
[[ $(names) == '[{"id":"222","name":"ghost-wispr.tail12ec1c.ts.net"},{"id":"333","name":"laptop.tail12ec1c.ts.net"},{"id":"555","name":"ghost-wispr-2.tail12ec1c.ts.net"}]' ]] \
  || fail "temporary-holder: the tailnet is $(names)"
no_key_in_argv temporary-holder
grep -Fxq 'claim-tailnet-name: device 222 is ghost-wispr.tail12ec1c.ts.net' "$case_dir/out" \
  || fail "temporary-holder: no closing line: $(<"$case_dir/out")"

# The tagged device has the name: no call changes anything.
run_case already-named "$(jq -c '.name = "ghost-wispr.tail12ec1c.ts.net"' <<<"$tagged")" "$bystander" \
  || fail "already-named: $(<"$case_dir/out")"
unchanged || fail "already-named: the tailnet changed to $(names)"
if grep -Eq -- '-X (DELETE|POST)' "$case_dir/argv"; then
  fail "already-named: the script deleted or renamed something"
fi

# Nobody holds the name: rename only.
run_case free-name "$tagged" "$bystander" || fail "free-name: $(<"$case_dir/out")"
[[ $(names) == '[{"id":"222","name":"ghost-wispr.tail12ec1c.ts.net"},{"id":"333","name":"laptop.tail12ec1c.ts.net"}]' ]] \
  || fail "free-name: the tailnet is $(names)"
grep -q -- '-X DELETE' "$case_dir/argv" && fail "free-name: the script deleted a device"

# A holder that is not the temporary device, an id that is not a string of digits, or more than one
# holder is refused before any DELETE or POST, and nothing changes. Each case lists its holders as
# a JSON array. The output must be the guard's refusal alone, so a refusal that comes from jq
# failing on an odd value does not pass for the guard's decision.
for refused in \
  "connected:[$(jq -c '.connectedToControl = true' <<<"$temporary")]" \
  "tagged:[$(jq -c '.tags = ["tag:ghost-wispr"] | .user = "" | .hostname = "other"' <<<"$temporary")]" \
  "tagged-only:[$(jq -c '.tags = ["tag:other"]' <<<"$temporary")]" \
  "other-host:[$(jq -c '.hostname = "something-else"' <<<"$temporary")]" \
  "no-owner:[$(jq -c '.user = ""' <<<"$temporary")]" \
  "spaced-id:[$(jq -c '.id = "111 222"' <<<"$temporary")]" \
  "numeric-id:[$(jq -c '.id = 222' <<<"$temporary")]" \
  "newline-id:[$(jq -c '.id = "222\n"' <<<"$temporary")]" \
  "second-holder-x:[$temporary,$(jq -c '.id = "x"' <<<"$temporary")]" \
  "two-holders:[$temporary,$(jq -c '.id = "444"' <<<"$temporary")]" \
  "same-holder-twice:[$temporary,$temporary]"; do
  label=${refused%%:*}
  mapfile -t holders < <(jq -c '.[]' <<<"${refused#*:}")
  if run_case "refuse-$label" "$tagged" "${holders[@]}"; then
    fail "refuse-$label: the script accepted a holder that is not the temporary device"
  fi
  unchanged || fail "refuse-$label: the tailnet changed to $(names)"
  if grep -Eq -- '-X (DELETE|POST)' "$case_dir/argv"; then
    fail "refuse-$label: the script deleted or renamed before it refused"
  fi
  [[ $(<"$case_dir/out") == 'claim-tailnet-name: the name ghost-wispr.tail12ec1c.ts.net is held by '*'; nothing was changed' ]] \
    || fail "refuse-$label: the output is not the guard's refusal alone: $(<"$case_dir/out")"
done

# No tagged device, or two: refused before any change.
if run_case no-tagged "$temporary"; then
  fail "no-tagged: the script ran with no tagged device"
fi
unchanged || fail "no-tagged: the tailnet changed"
if run_case two-tagged "$tagged" "$(jq -c '.id = "444" | .name = "ghost-wispr-2.tail12ec1c.ts.net"' <<<"$tagged")" "$temporary"; then
  fail "two-tagged: the script ran with two tagged devices"
fi
unchanged || fail "two-tagged: the tailnet changed"

# The tagged device's id is not a string of digits: refused before any DELETE or POST. With the
# id "333\n" read as 333, the script would delete the temporary device and rename the bystander,
# whose id is 333.
for refused in 'newline:"333\n"' 'numeric:333'; do
  label=tagged-id-${refused%%:*}
  if run_case "$label" "$(jq -c --argjson id "${refused#*:}" '.id = $id' <<<"$tagged")" "$temporary" "$bystander"; then
    fail "$label: the script ran with a tagged id that is not a string of digits"
  fi
  unchanged || fail "$label: the tailnet changed to $(names)"
  if grep -Eq -- '-X (DELETE|POST)' "$case_dir/argv"; then
    fail "$label: the script deleted or renamed before it refused"
  fi
  [[ $(<"$case_dir/out") == "claim-tailnet-name: the tagged device has an id that is not a string of digits: ${refused#*:}; nothing was changed" ]] \
    || fail "$label: the output is not the refusal alone: $(<"$case_dir/out")"
done

# The rename answers 200 but the name does not change: the read-back refuses.
if TEST_RENAME_NOOP=1 run_case rename-not-taken "$tagged" "$temporary"; then
  fail "rename-not-taken: the script passed a rename that did not take"
fi
grep -Fxq 'claim-tailnet-name: device 222 is named ghost-wispr-1.tail12ec1c.ts.net after the rename, not ghost-wispr.tail12ec1c.ts.net' \
  "$case_dir/out" || fail "rename-not-taken: no read-back failure: $(<"$case_dir/out")"

# The API refuses a call: the script names the call and stops. After a refused list nothing
# changed, and after a refused delete no rename was sent.
for refusal in \
  'GET:GET /tailnet/-/devices?fields=all answered HTTP 403' \
  'DELETE:DELETE /device/111 answered HTTP 403' \
  'POST:POST /device/222/name answered HTTP 403'; do
  method=${refusal%%:*}
  if TEST_FAIL_METHOD=$method run_case "http-$method" "$tagged" "$temporary"; then
    fail "http-$method: the script passed a call the API refused"
  fi
  grep -Fxq "claim-tailnet-name: ${refusal#*:}" "$case_dir/out" \
    || fail "http-$method: the failure does not name the call: $(<"$case_dir/out")"
  if [[ $method == GET ]] && ! unchanged; then
    fail "http-GET: the tailnet changed to $(names)"
  fi
  if [[ $method == DELETE ]] && grep -q -- '-X POST' "$case_dir/argv"; then
    fail "http-DELETE: the script renamed after a refused delete"
  fi
done

# Control: the argv check fires on a curl call that carries the key.
case_dir="$work_dir/argv-control"
mkdir -p "$case_dir"
printf -- '-sS -H Authorization: Bearer %s https://api.tailscale.com/api/v2/x\n' "$KEY" >"$case_dir/argv"
if (no_key_in_argv argv-control) 2>/dev/null; then
  fail "argv-control: the argv check missed a key in an argument"
fi

# Control: the output check fires on output that carries the key.
case_dir="$work_dir/output-control"
mkdir -p "$case_dir"
printf 'claim-tailnet-name: using %s\n' "$KEY" >"$case_dir/out"
if (no_key_in_output output-control) 2>/dev/null; then
  fail "output-control: the output check missed a key in the output"
fi

# Control: without the key the script refuses before it calls the API.
case_dir="$work_dir/no-key"
mkdir -p "$case_dir"
: >"$case_dir/argv"
if TEST_ARGV="$case_dir/argv" PATH="$work_dir/bin:$PATH" env -u TAILSCALE_API_KEY bash "$script" >/dev/null 2>&1; then
  fail "no-key: the script ran without TAILSCALE_API_KEY"
fi
[[ ! -s $case_dir/argv ]] || fail "no-key: the script called curl without a key"

printf 'claim tailnet name test passed\n'
