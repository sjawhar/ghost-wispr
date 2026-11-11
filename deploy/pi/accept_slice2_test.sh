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

work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
mutant="$work_dir/accept-slice2.sh"

# Each stub replaces one live check with one that cannot fail. The self-test
# must then fail, which shows that it exercises that check.
stubs=(
  'key_sweep_passes() { return 0; }'
  'run_key_sweep_locally() { printf "control-found\t/missing-control\n"; }'
  'assert_external_account_credential() { return 0; }'
  'assert_drive_config() { return 0; }'
  'assert_no_warnings() { return 0; }'
  'assert_expected_config() { return 0; }'
  'assert_running_summarization_model() { return 0; }'
  'assert_no_config_overrides() { return 0; }'
  'assert_drive_members() { return 0; }'
  'assert_certificate_warning() { return 0; }'
)
for stub in "${stubs[@]}"; do
  STUB=$stub awk '$0 == "if [[ \"$self_test\" == true ]]; then" { print ENVIRON["STUB"] } { print }' "$driver" >"$mutant"
  grep -Fqx -- "$stub" "$mutant" || fail "could not plant the stub: $stub"
  if bash "$mutant" --self-test >/dev/null 2>&1; then
    fail "self-test passed with a check that cannot fail: $stub"
  fi
done

printf 'slice 2 acceptance test passed\n'
