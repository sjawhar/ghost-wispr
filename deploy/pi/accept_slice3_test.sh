#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
driver="$repo_root/deploy/pi/accept-slice3.sh"

fail() {
  printf 'slice 3 acceptance test: %s\n' "$*" >&2
  exit 1
}

# A self-test run takes under a minute; a run that hangs fails the test instead of passing it.
readonly self_test_seconds=300

if ! output=$(timeout "$self_test_seconds" bash "$driver" --self-test 2>&1); then
  printf '%s\n' "$output" >&2
  fail 'self-test did not complete successfully'
fi

for line in \
  'A11 text only self-test RAN: planted failure rejected' \
  'A11 text only self-test RAN: the database scan sees a planted phrase in binary data in the database, its log and its backup' \
  'A11 text only self-test RAN: a failed control stops the row before "Delete text"' \
  'A11 text only self-test RAN: "Delete text" sends mode=text and its revision on its segment, then retries refinement' \
  'A11 self-test RAN: planted failure rejected' \
  'A11 self-test RAN: the Pi-side token, Drive and Deepgram requests, run against a fake curl, give the evidence the checks read, and no token or key reaches an argument or the evidence' \
  'A11 self-test RAN: a failed control stops the row before "Delete text and audio"' \
  'A11 self-test RAN: "Delete text and audio" sends mode=text_and_audio and its revision on its segment' \
  'Mode guard self-test RAN: planted failure rejected' \
  'Mode guard self-test RAN: a build without the modes stops the run before recording'; do
  grep -Fxq -- "$line" <<<"$output" || fail "self-test did not print: $line"
done

help_output=$(bash "$driver" --help) || fail '--help did not exit 0'
[[ "$help_output" == 'Usage: deploy/pi/accept-slice3.sh '* ]] || fail '--help did not print the usage'

# line_of NEEDLE: the line number of the one source line that holds NEEDLE.
line_of() {
  local needle=$1 count
  count=$(grep -cF -- "$needle" "$driver" || true)
  [[ "$count" == 1 ]] || fail "the driver holds $count lines with: $needle"
  grep -nF -m 1 -- "$needle" "$driver" | cut -d: -f1
}

# A11 text only runs ahead of A11's audio deletion, in the same conversation.
text_delete=$(line_of '"mode=text&revision=$text_only_revision"')
audio_delete=$(line_of '"mode=text_and_audio&revision=$audio_revision"')
(( text_delete < audio_delete )) || fail 'the driver sends mode=text_and_audio before mode=text'
text_row=$(line_of "run_row 'A11 text only' run_a11_text_only")
audio_row=$(line_of 'run_row A11 run_a11')
(( text_row < audio_row )) || fail 'the driver runs A11 before A11 text only'

# The operator prompts name the two actions exactly as the UI does, and by no other name.
line_of 'choose "Delete text" on segment %s' >/dev/null
line_of 'choose "Delete text and audio" on segment %s' >/dev/null
# shellcheck disable=SC2016 # The needle is the driver's source text.
line_of '"Delete text" on the "$text_only_phrase" segment and "Delete text and audio" on the' >/dev/null
names=$(grep -oE '"Delete [^"]*"' "$driver" | LC_ALL=C sort -u)
[[ "$names" == $'"Delete text and audio"\n"Delete text"' ]] \
  || fail "the driver names a deletion other than the UI's two: $(tr '\n' ' ' <<<"$names")"
line_of 'select((.transcript_revision // 0) > 0)' >/dev/null

mutation_dir=$(mktemp -d)
trap 'rm -rf -- "$mutation_dir"' EXIT
source_text=$(<"$driver")

# assert_mutation_caught NAME NEEDLE REPLACEMENT: the self-test fails on a driver with the change.
assert_mutation_caught() {
  local name=$1 needle=$2 replacement=$3 mutated status=0
  [[ "$source_text" == *"$needle"* ]] || fail "could not plant the $name mutation"
  mutated=${source_text/"$needle"/"$replacement"}
  printf '%s\n' "$mutated" >"$mutation_dir/accept-slice3.sh"
  timeout "$self_test_seconds" bash "$mutation_dir/accept-slice3.sh" --self-test >/dev/null 2>&1 || status=$?
  (( status != 124 )) || fail "self-test hung with $name"
  (( status != 0 )) || fail "self-test passed with $name"
}

# A checker that always passes must fail the self-test: each one is load-bearing.
for checker in check_absent check_ids_unchanged check_ids_replaced check_answers_200 check_answers_404 \
  check_one_revision check_hashes_unchanged check_same_md5 check_search_absent check_db_scan \
  check_mode_required check_doc_holds_summary check_session_settled; do
  needle=$'\n'"$checker() {"$'\n'
  assert_mutation_caught "a $checker that always passes" "$needle" "$needle  return 0"$'\n'
done

# A read that saw nothing must not pass as absence.
assert_mutation_caught 'an empty read that passes as absence' \
  $'reason=\'nothing was read, so absence shows nothing\'\n    return 2' \
  $'reason=\'nothing was read, so absence shows nothing\'\n    return 0'
assert_mutation_caught 'a failed search that passes as absence' \
  $'reason=\'the search returned no result list, so absence shows nothing\'\n    return 2' \
  $'reason=\'the search returned no result list, so absence shows nothing\'\n    return 0'
assert_mutation_caught 'a database scan without one of its files that passes' \
  $'reason="the scan found no $role file"\n      return 2' \
  $'reason="the scan found no $role file"\n      return 0'

# A control that could not see the phrase before the delete must stop the row.
assert_mutation_caught 'A11 text only deleting after a failed control' \
  $'print_round \'A11 text only\'\n  if ! round_result; then' \
  $'print_round \'A11 text only\'\n  if false; then'
assert_mutation_caught 'A11 deleting after a failed control' \
  $'print_round A11\n  if ! round_result; then' \
  $'print_round A11\n  if false; then'

# The Drive token and the Deepgram key stay out of curl's arguments, and the Drive token is read-only.
# shellcheck disable=SC2016 # The needles are the driver's source text.
assert_mutation_caught 'a Drive token passed as an argument' \
  '--config <(auth_header Bearer "$token")' '--header "Authorization: Bearer $token"'
# shellcheck disable=SC2016 # The needles are the driver's source text.
assert_mutation_caught 'a Deepgram key passed as an argument' \
  '--config <(auth_header Token "$deepgram_key")' '--header "Authorization: Token $deepgram_key"'
assert_mutation_caught 'a Drive token that can write' \
  'readonly drive_scope=https://www.googleapis.com/auth/drive.readonly' \
  'readonly drive_scope=https://www.googleapis.com/auth/drive'

printf 'slice 3 acceptance test passed\n'
