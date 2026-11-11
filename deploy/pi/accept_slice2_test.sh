#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
driver="$repo_root/deploy/pi/accept-slice2.sh"

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

assert_source_contains() {
  local needle=$1
  grep -Fq -- "$needle" "$driver" || fail "driver does not contain: $needle"
}

assert_source_contains "nats sub 'notifications.ghostwispr.>' --server nats://envoy-hub.tail12ec1c.ts.net:4222"
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

mutation_dir=$(mktemp -d)
trap 'rm -rf -- "$mutation_dir"' EXIT
mutated_driver="$mutation_dir/accept-slice2.sh"
source_text=$(<"$driver")
needle=$'printf \'%s provider model mismatch: got %q, want %q\\n\' "$role" "$actual" "$expected" >&2\n    return 1'
replacement=$'printf \'%s provider model mismatch: got %q, want %q\\n\' "$role" "$actual" "$expected" >&2\n    return 0'
[[ "$source_text" == *"$needle"* ]] || fail 'could not plant the provider-model checker mutation'
source_text=${source_text/"$needle"/"$replacement"}
printf '%s' "$source_text" >"$mutated_driver"
if bash "$mutated_driver" --self-test >/dev/null 2>&1; then
  fail 'self-test accepted a provider-model checker that accepts planted mismatches'
fi

printf 'slice 2 acceptance test passed\n'
