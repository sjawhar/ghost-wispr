#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
driver="$repo_root/deploy/pi/accept-slice2.sh"
# The driver, and each mutated copy of it, reads neutral deployment settings, not the office Pi's.
export GHOST_WISPR_PI_ENV="$repo_root/deploy/pi/test-pi.env"

fail() {
  printf 'slice 2 acceptance test: %s\n' "$*" >&2
  exit 1
}

if ! output=$(bash "$driver" --self-test 2>&1); then
  printf '%s\n' "$output" >&2
  fail 'self-test did not complete successfully'
fi

for row in A9 A6 A8 A10 Cert-warning; do
  [[ "$output" == *"$row self-test RAN: planted failure rejected"* ]] \
    || fail "self-test did not prove the $row check can fail"
done
# The spec's A8 asks that the unit run the plan's models, and the plan reads each model "from the
# provider's response or the app's log": a provider that reports none passes on the model the
# running process reports for the role.
for role in summary search speech refinement; do
  [[ "$output" == *"A8 RAN: the provider did not report the $role model; the running process runs "* ]] \
    || fail "self-test did not pass an unreported $role model on the running one"
done
[[ "$output" == *'A6 self-test RAN: canonical transcript supplied the search word'* ]] \
  || fail 'self-test did not prove A6 searches canonical_transcript'
[[ "$output" == *'A8 self-test RAN: every reported model returns PASS'* ]] \
  || fail 'self-test did not prove run_a8 returns PASS when every role reports its model'
[[ "$output" == *'A8 self-test RAN: every role passes on a model the provider did not report, and later roles still run'* ]] \
  || fail 'self-test did not prove run_a8 passes, and runs every role, when no provider reports its model'
[[ "$output" == *'A8 self-test RAN: every role is compared with the model the running process reports'* ]] \
  || fail 'self-test did not prove A8 compares every role with the running process'
# The Pi names each provider once (README "Model providers"): A8 compares how the running process
# signs in to each with the plan, and refuses a deployed YAML or service environment that still sets
# a setting the providers replaced, at any depth.
[[ "$output" == *'A8 self-test RAN: every provider is compared with how the running process signs in to it'* ]] \
  || fail 'self-test did not prove A8 compares every provider with the running process'
[[ "$output" == *'A8 self-test RAN: every removed setting is rejected wherever it is nested'* ]] \
  || fail 'self-test did not prove A8 rejects a removed setting in the deployed YAML'
[[ "$output" == *"A8 self-test RAN: the deployed YAML's providers are compared with the plan's"* ]] \
  || fail "self-test did not prove A8 compares the deployed YAML's providers with the plan"
[[ "$output" == *'A8 self-test RAN: every removed variable in the service environment is rejected'* ]] \
  || fail 'self-test did not prove A8 rejects a removed variable in the service environment'

# The Cert-warning row checks that the test instance cannot open the Jabra before it trusts the
# instance's status: an instance that holds the audio or input group, runs with the Jabra button
# on, or holds a sound or input device is refused.
[[ "$output" == *'Cert-warning self-test RAN: a test instance that could open the Jabra is rejected'* ]] \
  || fail 'self-test did not prove the Cert-warning row rejects a test instance that could open the Jabra'


assert_source_contains() {
  local needle=$1
  grep -Fq -- "$needle" "$driver" || fail "driver does not contain: $needle"
}

assert_source_contains "nats sub 'notifications.ghostwispr.>' --server \"\$PI_NATS_SERVER\""
assert_source_contains "/api/sessions/start"
assert_source_contains "/api/sessions/\${acceptance_session_id}/resummarize"
assert_source_contains '/api/search/semantic'
assert_source_contains '/api/tts/speak'
assert_source_contains "/api/sessions/\${acceptance_session_id}/retry-refinement"
assert_source_contains 'A6 PRECHECK:'
assert_source_contains 'A8 PRECHECK:'
assert_source_contains 'nested/dev'
assert_source_contains "find \"\$root\" -mindepth 1 -maxdepth 1 -print0"
assert_source_contains 'run_row A9 run_a9'
assert_source_contains 'run_row A6 run_a6'
assert_source_contains 'run_row A8 run_a8'
assert_source_contains 'run_row A10 run_a10'
assert_source_contains 'run_row Cert-warning run_certificate_warning'
if grep -Fq -- '--exclude-dir=dev' "$driver"; then
  fail 'A9 sweep excludes nested dev directories'
fi

assert_source_contains 'assert_provider_model'
assert_source_contains 'provider model mismatch'
# shellcheck disable=SC2016 # The needles are the driver's source text.
assert_source_contains 'assert_running_models "$current_config"'
# shellcheck disable=SC2016
assert_source_contains 'assert_running_providers "$current_config"'
# shellcheck disable=SC2016
assert_source_contains 'assert_expected_config "$deployed_yaml"'

# POST /api/tts/speak speaks with the tts role and takes no provider, so A8's request names only its
# text.
speak_source=$(awk '/^speak\(\) \{$/ { found = 1 } found { print } found && /^\}$/ { exit }' "$driver")
[[ $speak_source == *'/api/tts/speak'* ]] || fail "could not read speak from the driver: $speak_source"
[[ $speak_source == *"--data '{\"text\":"*'}'"'"* && $speak_source != *provider* ]] \
  || fail "A8's text-to-speech request names more than its text: $speak_source"

mutation_dir=$(mktemp -d)
trap 'rm -rf -- "$mutation_dir"' EXIT
mutated_driver="$mutation_dir/accept-slice2.sh"
# The self-test reads the Pi's template beside the driver, so each copy has one. The control: an
# unchanged copy passes there, so a mutated copy fails for its mutation, not for where it runs.
mkdir "$mutation_dir/files"
cp -- "$repo_root/deploy/pi/files/ghost-wispr.yaml" "$mutation_dir/files/"
cp -- "$driver" "$mutated_driver"
bash "$mutated_driver" --self-test >/dev/null 2>&1 || fail 'an unchanged copy of the driver fails its self-test where the mutations run'
source_text=$(<"$driver")
needle=$'printf \'%s provider model mismatch: got %q, want %q\\n\' "$role" "$actual" "$expected" >&2\n    return 1'
replacement=$'printf \'%s provider model mismatch: got %q, want %q\\n\' "$role" "$actual" "$expected" >&2\n    return 0'
[[ "$source_text" == *"$needle"* ]] || fail 'could not plant the provider-model checker mutation'
source_text=${source_text/"$needle"/"$replacement"}
printf '%s' "$source_text" >"$mutated_driver"
if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
  fail 'self-test accepted a provider-model checker that accepts planted mismatches'
fi

# run_row must report a row that returns 2 as NEEDS-PERSON and not count it as a failure; a row
# that returns 1 is a FAIL. On the office Pi (2026-11-16) the live driver would call A10 without
# --drive-members and Cert-warning without --cert-test FAIL: after `if "$@"; then ...; fi` with no
# else branch, $? is the if statement's own 0, not the row's status.
run_row_source=$(awk '/^run_row\(\) \{$/ { found = 1 } found { print } found && /^\}$/ { exit }' "$driver")
[[ -n "$run_row_source" ]] || fail 'could not read run_row from the driver'
run_row_output=$(
  row_failures=0
  eval "$run_row_source"
  # shellcheck disable=SC2329 # run_row, eval'd above, calls the row function it is given.
  needs_person() { return 2; }
  # shellcheck disable=SC2329 # run_row, eval'd above, calls the row function it is given.
  fails() { return 1; }
  run_row Person-row needs_person 2>&1
  run_row Failing-row fails 2>&1
  printf 'row_failures=%s\n' "$row_failures"
)
[[ "$run_row_output" == *'Person-row verdict: NEEDS-PERSON'* ]] \
  || fail "run_row did not report a row that returned 2 as NEEDS-PERSON: $run_row_output"
[[ "$run_row_output" == *'Failing-row verdict: FAIL'* ]] \
  || fail "run_row did not report a row that returned 1 as FAIL: $run_row_output"
[[ "$run_row_output" == *'row_failures=1'* ]] \
  || fail "run_row counted the wrong number of failures: $run_row_output"

# A check of the running models, of how the running process signs in to each provider, of the
# deployed YAML's providers or removed settings, or of the service environment's removed variables
# that always passes must fail the self-test.
for check in assert_running_models assert_running_providers assert_expected_providers assert_no_removed_settings assert_no_removed_variables; do
  source_text=$(<"$driver")
  needle=$'\n'"$check() {"$'\n'
  [[ "$source_text" == *"$needle"* ]] || fail "could not plant the $check mutation"
  printf '%s' "${source_text/"$needle"/"$needle  return 0"$'\n'}" >"$mutated_driver"
  if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
    fail "self-test accepted a $check that always passes"
  fi
done

# A8 compares OpenAI's workload identity ids with the template's own
# (deploy/pi/files/ghost-wispr.yaml). Each comparison loosened to the shape it had before, any
# string, must fail the self-test, whose running process signs in with well-formed values that are
# not the template's. The unchanged copy above passing is the control.
for check in auth.identity_provider_id:providers.openai.auth.identity_provider_id \
  auth.service_account_id:providers.openai.auth.service_account_id; do
  field=${check%%:*} path=${check#*:}
  source_text=$(<"$driver")
  needle=".$field == \$plan[\"$path\"]"
  [[ "$source_text" == *"$needle"* ]] || fail "could not plant the mutation of the $path comparison"
  printf '%s' "${source_text/"$needle"/"(.$field | type == \"string\")"}" >"$mutated_driver"
  if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
    fail "self-test accepted a running process whose $path is any string"
  fi
done
# A8 compares each Vertex AI provider's billing labels with the template's. Either comparison
# dropped must fail the self-test, whose drifts include a provider with no labels.
for provider in vertex vertex-us; do
  source_text=$(<"$driver")
  needle=".labels == {\"owner\": \$plan[\"providers.$provider.labels.owner\"], \"stream\": \$plan[\"providers.$provider.labels.stream\"]}"
  [[ "$source_text" == *"$needle"* ]] || fail "could not plant the mutation of the $provider labels comparison"
  printf '%s' "${source_text/"$needle"/true}" >"$mutated_driver"
  if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
    fail "self-test accepted a running process whatever its $provider labels"
  fi
done
# A8 refuses a running process that names a provider the plan does not, such as the gateway the
# template keeps commented out until it admits the Pi: that provider's row can read Fails.
source_text=$(<"$driver")
# shellcheck disable=SC2016 # The needle is the driver's source text.
needle='if [[ "$names" != "$want_names" ]]; then'
[[ "$source_text" == *"$needle"* ]] || fail 'could not plant the mutation of the provider-set comparison'
printf '%s' "${source_text/"$needle"/'if false; then'}" >"$mutated_driver"
if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
  fail 'self-test accepted a running process that names a provider the plan does not'
fi
# A plan that sets no value to compare with must fail the check, not pass on an empty value.
source_text=$(<"$driver")
# shellcheck disable=SC2016 # The needle is the driver's source text.
needle='if [[ -z "$value" ]]; then'
[[ "$source_text" == *"$needle"* ]] || fail 'could not plant the mutation of the plan reader'
printf '%s' "${source_text/"$needle"/'if false; then'}" >"$mutated_driver"
if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
  fail 'self-test accepted a plan reader that passes a setting the plan does not set'
fi

# The cert-warning test instance runs beside the recorder, on the same Pi. Its PortAudio scan, the
# first time it starts its microphone, opens the Jabra's microphone, and with its Jabra button on it
# would also write the Jabra's light.
# The recorder, running with its own Jabra button on, cannot see either: the Jabra then sends its
# own Phone Mute, and the recorder reads it as a tap (docs/pi-setup.md, "Pause and resume at the
# unit"). The runbook starts it with no supplementary groups, so it can open no sound device
# (root:audio 0660 on the office Pi) and no input node (root:input 0660), and with the Jabra button
# off. This runs the runbook's block with a fake sudo that resets the environment as sudo does, a
# fake setpriv, and a fake binary that records the environment and the options each run gets.
runbook_doc="$repo_root/docs/pi-setup.md"
cert_block=$(awk -v marker='<!-- accept_slice2_test.sh: cert-test instance -->' '
  $0 == marker { found = 1; next }
  found && !capture && /^```bash$/ { capture = 1; next }
  capture && /^```$/ { exit }
  capture { print }
' "$runbook_doc")
[[ -n "$cert_block" ]] || fail 'docs/pi-setup.md has no bash block after <!-- accept_slice2_test.sh: cert-test instance -->'
fake_dir="$mutation_dir/cert-instance"
mkdir -p "$fake_dir/bin" "$fake_dir/test-dir"
cat >"$fake_dir/bin/sudo" <<'EOF'
#!/usr/bin/env bash
# sudo's env_reset: the command gets none of the caller's environment.
exec env -i PATH="$PATH" FAKE_DIR="$FAKE_DIR" "$@"
EOF
cat >"$fake_dir/bin/setpriv" <<'EOF'
#!/usr/bin/env bash
options=()
while [[ $# -gt 0 && $1 == --* ]]; do options+=("$1"); shift; done
printf '%s\n' "${options[@]}" >"$FAKE_DIR/setpriv-options"
exec "$@"
EOF
cat >"$fake_dir/ghost-wispr" <<'EOF'
#!/usr/bin/env bash
env | LC_ALL=C sort >"$FAKE_DIR/env-${1:-instance}"
[[ -z ${1:-} && -e $FAKE_DIR/setpriv-options ]] && cp "$FAKE_DIR/setpriv-options" "$FAKE_DIR/instance-setpriv-options"
exit 0
EOF
chmod +x "$fake_dir/bin/sudo" "$fake_dir/bin/setpriv" "$fake_dir/ghost-wispr"
(
  export FAKE_DIR="$fake_dir" test_dir="$fake_dir/test-dir" PATH="$fake_dir/bin:$PATH"
  bash -euo pipefail -c "${cert_block//\/opt\/ghost-wispr\/ghost-wispr/$fake_dir/ghost-wispr}"
) || fail 'the runbook block that seeds and starts the cert-warning test instance failed against fakes'
[[ -s "$fake_dir/env-seed-paused-state" ]] || fail 'the runbook block does not seed the test instance as paused'
grep -Fqx "GHOST_WISPR_CONFIG=$fake_dir/test-dir/ghost-wispr.yaml" "$fake_dir/env-seed-paused-state" \
  || fail "the runbook block seeds a database other than the copy's"
[[ -s "$fake_dir/env-instance" ]] || fail 'the runbook block does not start the test instance'
[[ -s "$fake_dir/instance-setpriv-options" ]] \
  || fail 'the runbook block starts the test instance without sudo setpriv, so it keeps the audio and input groups'
for option in --reuid=ghost-wispr --regid=ghost-wispr --clear-groups; do
  grep -Fqx -- "$option" "$fake_dir/instance-setpriv-options" \
    || fail "the runbook block starts the test instance without setpriv $option: $(tr '\n' ' ' <"$fake_dir/instance-setpriv-options")"
done
for setting in GHOST_WISPR_JABRA_MUTE_BUTTON=false GHOST_WISPR_ADDR=127.0.0.1:8081 \
  "GHOST_WISPR_CONFIG=$fake_dir/test-dir/ghost-wispr.yaml"; do
  grep -Fqx -- "$setting" "$fake_dir/env-instance" \
    || fail "the test instance does not get $setting; its environment: $(tr '\n' ' ' <"$fake_dir/env-instance")"
done
printf 'slice 2 acceptance test passed\n'
