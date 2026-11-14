#!/usr/bin/env bash
# Exercise the slice 3 acceptance checks from an operator machine on the tailnet: record one
# scripted conversation, choose "Delete text" on one of its segments (A11 text only), then
# "Delete text and audio" on another (A11), and check what each deletion leaves behind.
set -uo pipefail

host=${GHOST_WISPR_HOST:-ghost-wispr.tail12ec1c.ts.net}
self_test=false
given_session=
readonly deploy_dir=/opt/ghost-wispr
readonly drive_api=https://www.googleapis.com/drive/v3
readonly drive_scope=https://www.googleapis.com/auth/drive.readonly
readonly deepgram_url='https://api.deepgram.com/v1/listen?model=nova-3&smart_format=true'
readonly google_doc_type=application/vnd.google-apps.document
# The mode guard probes a session id that matches the API's pattern and does not exist: the
# handler checks the mode before it reads the session, and a build without modes answers 404
# without deleting anything.
readonly probe_session_id=slice3-mode-probe
readonly probe_segment_id=999999999
readonly search_limit=50
readonly poll_seconds=15
readonly settle_attempts=120
readonly after_attempts=60
readonly refinement_attempts=600
# Live transcription may write a number as digits, and a slug joins words with hyphens.
readonly separator='[^[:alnum:]]{1,3}'
readonly text_only_phrase='orange tractor ninety'
readonly text_only_pair="orange${separator}tractor"
readonly text_only_trace='(^|[^[:alnum:]])(orange|tractor|ninety)([^[:alnum:]]|$)'
readonly text_only_query='orange tractor'
readonly audio_phrase='purple elephant seventeen'
readonly audio_pair="purple${separator}elephant"
readonly audio_trace='(^|[^[:alnum:]])(purple|elephant|seventeen)([^[:alnum:]]|$)'
readonly audio_query='purple elephant'
# The control phrase is never deleted: every read after a delete must still hold it.
readonly control_phrase='blue harbor lantern'
readonly control_pair="blue${separator}harbou?r${separator}lantern"
readonly control_query='blue lantern'

usage() {
  cat <<EOF
Usage: deploy/pi/accept-slice3.sh [--host HOST] [--session ID] [--self-test]

Run from a tailnet member that can ssh to ghost-wispr@HOST, once the slice 3 build is on the
Pi. One run records one scripted conversation, chooses "Delete text" on its
"$text_only_phrase" segment (A11 text only), then "Delete text and audio" on its
"$audio_phrase" segment (A11), and checks each deletion. It restarts
ghost-wispr.service once, while no session is recording, so that the pre-migration backup
holds the conversation before the delete.

--session ID  use a conversation already recorded from the script and ended, instead of
              recording one. The database precheck is skipped: it cannot tell an earlier
              copy of a phrase from this conversation's.
--self-test   run every check against planted fixtures, with no network and no Pi.
EOF
}

fail() {
  printf 'slice 3 acceptance: %s\n' "$*" >&2
  exit 1
}

# Each check returns 0 (PASS), 1 (FAILED) or 2 (UNKNOWN: the read could not see what it looks
# for) and sets reason.
reason=
round_lines=()
round_failed=0
round_unknown=0

round_reset() {
  round_lines=()
  round_failed=0
  round_unknown=0
}

# check NAME CHECK ARGS...: run one check and record its line in the current round.
check() {
  local name=$1 result
  shift
  reason=
  "$@"
  result=$?
  case "$result" in
    0) round_lines+=("PASS: $name: $reason") ;;
    1)
      round_lines+=("FAILED: $name: $reason")
      round_failed=$((round_failed + 1))
      ;;
    *)
      round_lines+=("UNKNOWN: $name: $reason")
      round_unknown=$((round_unknown + 1))
      ;;
  esac
  return 0
}

round_result() {
  if (( round_failed > 0 )); then
    return 1
  fi
  if (( round_unknown > 0 )); then
    return 2
  fi
  return 0
}

print_round() {
  local row=$1 line
  for line in "${round_lines[@]}"; do
    printf '%s %s\n' "$row" "$line"
  done
}

# worse A B: the worse of two results, FAILED over UNKNOWN over PASS.
worse() {
  if (( $1 == 1 || $2 == 1 )); then
    printf 1
  elif (( $1 != 0 || $2 != 0 )); then
    printf 2
  else
    printf 0
  fi
}

matches() {
  LC_ALL=C grep -Eiq -- "$2" <<<"$1"
}

blank() {
  [[ -z "${1//[[:space:]]/}" ]]
}

line_count() {
  if [[ -z "$1" ]]; then
    printf 0
  else
    wc -l <<<"$1" | tr -d '[:space:]'
  fi
}

one_line() {
  local text=${1//$'\t'/=}
  printf '%s' "${text//$'\n'/, }"
}

tokens() {
  LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"$1" | LC_ALL=C tr -cs '[:alnum:]' ' '
}

session_field() {
  jq -r --arg field "$2" '.session[$field] // "" | tostring' <<<"$1" 2>/dev/null
}

segment_texts() {
  jq -r '.segments[]?.text // empty' <<<"$1" 2>/dev/null
}

recording_paths() {
  session_field "$1" audio_path | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//; /^$/d'
}

recording_drive_name() {
  printf 'audio-%s' "${1##*/}"
}

deepgram_transcript() {
  jq -r '.results.channels[0].alternatives[0].transcript // ""' <<<"$1" 2>/dev/null
}

drive_name() {
  jq -r '.name // ""' <<<"$1" 2>/dev/null
}

# drive_file_ids LISTING: sorted "name<TAB>id" lines for a session folder's documents and recordings.
drive_file_ids() {
  jq -r '.files[]? | select(.name == "transcript.md" or .name == "summary.md" or (.name | startswith("audio-")))
    | [.name, .id] | @tsv' <<<"$1" 2>/dev/null | LC_ALL=C sort
}

drive_id_named() {
  jq -r --arg name "$2" '[.files[]? | select(.name == $name) | .id][0] // empty' <<<"$1" 2>/dev/null
}

drive_md5_named() {
  jq -r --arg name "$2" '[.files[]? | select(.name == $name) | .md5Checksum // empty][0] // empty' <<<"$1" 2>/dev/null
}

# result DIR NAME: one file of the evidence the Pi returned; nothing when it is missing.
result() {
  if [[ -f "$1/$2" ]]; then
    printf '%s' "$(<"$1/$2")"
  fi
}

# response_code DIR KEY FIELD: the HTTP code a request recorded, or the error that stopped it.
response_code() {
  local code error
  code=$(result "$1" "$2.$3")
  if [[ -n "$code" ]]; then
    printf '%s' "$code"
    return
  fi
  error=$(result "$1" "$2.error")
  if [[ -n "$error" ]]; then
    printf '(error: %s)' "$(one_line "$error")"
  fi
}

# check_present TEXT PATTERN: before a delete, the read finds the phrase. This is the control
# that shows the same read can see the phrase when it is there.
check_present() {
  local text=$1 pattern=$2
  if blank "$text"; then
    reason='nothing was read'
    return 2
  fi
  if matches "$text" "$pattern"; then
    reason='the phrase is there'
    return 0
  fi
  reason='the phrase is not there'
  return 1
}

# check_absent TEXT TRACE [CONTROL]: after a delete, no trace of the phrase, and the same read
# holds the control phrase, so an empty, failed or wrong read cannot pass as absence.
check_absent() {
  local text=$1 trace=$2 control=${3:-}
  if blank "$text"; then
    reason='nothing was read, so absence shows nothing'
    return 2
  fi
  if matches "$text" "$trace"; then
    reason="a trace of the phrase is still there: $(LC_ALL=C grep -Eio -- "$trace" <<<"$text" | head -n 3 | tr '\n' ' ')"
    return 1
  fi
  if [[ -n "$control" ]] && ! matches "$text" "$control"; then
    reason="the same read lacks the control phrase \"$control_phrase\", so it may be blind"
    return 2
  fi
  reason='no trace of the phrase'
  return 0
}

# answered CODE CHECK ARGS...: run CHECK on a response body only when the request answered 200.
answered() {
  local code=$1
  shift
  if [[ "$code" != 200 ]]; then
    reason="the request answered ${code:-nothing}"
    return 2
  fi
  "$@"
}

check_answers_200() {
  case "$1" in
    200)
      reason='answers 200'
      return 0
      ;;
    404)
      reason='answers 404'
      return 1
      ;;
    *)
      reason="answers ${1:-nothing}, which shows neither that the file is there nor that it is gone"
      return 2
      ;;
  esac
}

check_answers_404() {
  case "$1" in
    404)
      reason='answers 404'
      return 0
      ;;
    2??)
      reason="answers $1: the file is still there"
      return 1
      ;;
    *)
      reason="answers ${1:-nothing}, which shows neither that the file is there nor that it is gone"
      return 2
      ;;
  esac
}

# check_revisions_listed CODE BODY: before the audio delete, revisions.list succeeds.
check_revisions_listed() {
  local code=$1 body=$2 count
  case "$code" in
    200) ;;
    404)
      reason='revisions.list answers 404'
      return 1
      ;;
    *)
      reason="revisions.list answers ${code:-nothing}"
      return 2
      ;;
  esac
  count=$(jq -er '.revisions | arrays | length' <<<"$body" 2>/dev/null) || {
    reason='revisions.list returned no revision list'
    return 2
  }
  if (( count < 1 )); then
    reason='revisions.list enumerated no revision'
    return 2
  fi
  reason="revisions.list succeeds with $count revisions"
  return 0
}

# check_one_revision CODE BODY: a file the audio delete uploaded holds exactly one revision.
check_one_revision() {
  local code=$1 body=$2 count
  if [[ "$code" != 200 ]]; then
    reason="revisions.list answers ${code:-nothing}"
    return 2
  fi
  count=$(jq -er '.revisions | arrays | length' <<<"$body" 2>/dev/null) || {
    reason='revisions.list returned no revision list'
    return 2
  }
  if (( count == 0 )); then
    reason='revisions.list enumerated no revision'
    return 2
  fi
  if (( count != 1 )); then
    reason="the file holds $count revisions"
    return 1
  fi
  reason='the file holds one revision'
  return 0
}

# check_listing CODE LISTING: the session folder lists a transcript, a summary and a recording.
check_listing() {
  local code=$1 listing=$2 ids names name
  if [[ "$code" != 200 ]]; then
    reason="the folder list answered ${code:-nothing}"
    return 2
  fi
  if ! jq -e '.files | type == "array"' >/dev/null 2>&1 <<<"$listing"; then
    reason='the folder list returned no file list'
    return 2
  fi
  if jq -e 'has("nextPageToken")' >/dev/null 2>&1 <<<"$listing"; then
    reason='the folder list has more pages, so it is not the whole folder'
    return 2
  fi
  ids=$(drive_file_ids "$listing")
  if [[ -z "$ids" ]]; then
    reason='the folder list enumerated no transcript, summary or recording'
    return 2
  fi
  names=$(cut -f1 <<<"$ids")
  for name in transcript.md summary.md; do
    if ! grep -Fxq -- "$name" <<<"$names"; then
      reason="the folder holds no $name"
      return 1
    fi
  done
  if ! grep -q '^audio-' <<<"$names"; then
    reason='the folder holds no recording'
    return 1
  fi
  reason="the folder holds $(one_line "$ids")"
  return 0
}

# check_ids_unchanged BEFORE AFTER: "Delete text" keeps every Drive file id.
check_ids_unchanged() {
  local before=$1 after=$2
  if [[ -z "$before" || -z "$after" ]]; then
    reason='a Drive file id list is empty'
    return 2
  fi
  if [[ "$before" != "$after" ]]; then
    reason="the Drive file ids changed from [$(one_line "$before")] to [$(one_line "$after")]"
    return 1
  fi
  reason="the Drive file ids are unchanged: $(one_line "$after")"
  return 0
}

# check_ids_replaced BEFORE AFTER: "Delete text and audio" gives every file a new id.
check_ids_replaced() {
  local before=$1 after=$2 old_ids name id
  if [[ -z "$before" || -z "$after" ]]; then
    reason='a Drive file id list is empty'
    return 2
  fi
  if [[ "$(cut -f1 <<<"$before")" != "$(cut -f1 <<<"$after")" ]]; then
    reason="the folder held [$(one_line "$before")] and now holds [$(one_line "$after")]"
    return 1
  fi
  old_ids=$(cut -f2 <<<"$before")
  while IFS=$'\t' read -r name id; do
    if grep -Fxq -- "$id" <<<"$old_ids"; then
      reason="$name kept its old id $id"
      return 1
    fi
  done <<<"$after"
  reason="every file has a new id: $(one_line "$after")"
  return 0
}

# check_hashes_unchanged BEFORE AFTER: "<sha256> <path>" lines for the unit's recordings.
check_hashes_unchanged() {
  local before=$1 after=$2 line
  if [[ -z "$before" || -z "$after" ]]; then
    reason='no recording was hashed'
    return 2
  fi
  while IFS= read -r line; do
    if [[ ! "$line" =~ ^[0-9a-f]{64}\ .+$ ]]; then
      reason="not a recording hash: $line"
      return 2
    fi
  done <<<"$before"$'\n'"$after"
  if [[ "$before" != "$after" ]]; then
    reason="the recording changed from [$(one_line "$before")] to [$(one_line "$after")]"
    return 1
  fi
  reason="sha256 unchanged: $(one_line "$after")"
  return 0
}

check_same_md5() {
  local unit=$1 drive=$2
  if [[ ! "$unit" =~ ^[0-9a-f]{32}$ || ! "$drive" =~ ^[0-9a-f]{32}$ ]]; then
    reason="a checksum is missing (unit ${unit:-none}, Drive ${drive:-none})"
    return 2
  fi
  if [[ "$unit" != "$drive" ]]; then
    reason="the unit's recording (md5 $unit) differs from Drive's (md5 $drive)"
    return 1
  fi
  reason="md5 $unit on the unit and on Drive"
  return 0
}

check_same_text() {
  if blank "$1" || blank "$2"; then
    reason='a value is empty'
    return 2
  fi
  if [[ "$1" != "$2" ]]; then
    reason="changed from $1 to $2"
    return 1
  fi
  reason="unchanged: $2"
  return 0
}

check_has_recording() {
  if blank "$1"; then
    reason='the session names no recording, so the recording checks have nothing to read'
    return 2
  fi
  reason="the session names $(one_line "$1")"
  return 0
}

check_remote_returned() {
  if [[ "$1" != 'done' ]]; then
    reason='the Pi did not return its evidence (ssh or tar failed)'
    return 2
  fi
  reason='the Pi returned its evidence'
  return 0
}

check_drive_token() {
  local state=$1 error=$2
  case "$state" in
    ready)
      reason="the service account's credential on the Pi minted a read-only Drive token"
      return 0
      ;;
    failed)
      reason="no Drive token: $(one_line "$error")"
      return 2
      ;;
    *)
      reason='no Drive request ran'
      return 2
      ;;
  esac
}

# check_mode_required CODE BODY: a deletion without a mode, or with an unknown one, is refused
# with 400 and an error that names both modes.
check_mode_required() {
  local code=$1 body=$2 message
  case "$code" in
    400) ;;
    ''|000)
      reason='no answer'
      return 2
      ;;
    *)
      reason="answered $code, not 400"
      return 1
      ;;
  esac
  message=$(jq -er '.error | strings' <<<"$body" 2>/dev/null) || {
    reason='the 400 carries no JSON error'
    return 1
  }
  # The API quotes each mode, so the word "text" in "Delete text and audio"
  # does not count as naming the text mode.
  if ! matches "$message" '"text_and_audio"' || ! matches "$message" '"text"'; then
    reason="the error does not name both modes: $message"
    return 1
  fi
  reason="refused with 400: $message"
  return 0
}

# search_tally BODY SESSION: "<result count> <whether SESSION is among them>", or nothing when
# BODY is not a search result list.
search_tally() {
  jq -er --arg id "$2" 'if type == "array" then "\(length) \(any(.[]; .session_id == $id))" else error("not a list") end' \
    <<<"$1" 2>/dev/null
}

# check_search_finds BODY SESSION: the UI's search returns the conversation.
check_search_finds() {
  local count found
  read -r count found <<<"$(search_tally "$1" "$2")"
  if [[ -z "$count" ]]; then
    reason='the search returned no result list'
    return 2
  fi
  if [[ "$found" == true ]]; then
    reason='search finds the conversation'
    return 0
  fi
  reason="search does not find the conversation among $count results"
  return 1
}

# check_search_absent BODY SESSION: the UI's search no longer returns the conversation.
check_search_absent() {
  local count found
  read -r count found <<<"$(search_tally "$1" "$2")"
  if [[ -z "$count" ]]; then
    reason='the search returned no result list, so absence shows nothing'
    return 2
  fi
  if [[ "$found" == true ]]; then
    reason='search still finds the conversation'
    return 1
  fi
  if (( count >= search_limit )); then
    reason="the search returned its $search_limit-result limit, so the list may be cut"
    return 2
  fi
  reason="search does not find the conversation among $count results"
  return 0
}

# check_search_empty BODY: before the conversation, no session holds the phrase.
check_search_empty() {
  local ids
  ids=$(jq -er 'if type == "array" then [.[].session_id] | join(", ") else error("not a list") end' <<<"$1" 2>/dev/null) || {
    reason='the search returned no result list'
    return 2
  }
  if [[ -n "$ids" ]]; then
    reason="already in conversations $ids"
    return 1
  fi
  reason='no conversation holds it'
  return 0
}

# check_db_scan REPORT MODE NAME [CONTROL]: read a database scan from the Pi (db_scan in
# remote_script). MODE present (a control before a delete) needs the NAME pattern in the
# database or its write-ahead log, and in the pre-migration backup. MODE absent needs NAME in no
# file, and CONTROL in the database or its log and in the backup, which shows that the same
# reads see conversation text.
check_db_scan() {
  local report=$1 mode=$2 name=$3 control=${4:-} kind role pattern count path
  local roles=' ' files=0 name_counts=0 live=0 backup=0 live_control=0 backup_control=0 holders=''
  if blank "$report"; then
    reason='the database scan returned nothing'
    return 2
  fi
  while IFS=$'\t' read -r kind role pattern count path; do
    case "$kind" in
      error)
        reason="the database scan failed: $role"
        return 2
        ;;
      file)
        roles+="$role "
        files=$((files + 1))
        ;;
      count)
        if [[ ! "$count" =~ ^[0-9]+$ ]]; then
          reason="grep could not read $path"
          return 2
        fi
        if [[ "$pattern" == "$name" ]]; then
          name_counts=$((name_counts + 1))
          if (( count > 0 )); then
            holders+="$role $path ($count lines); "
          fi
          case "$role" in
            db|wal) live=$((live + count)) ;;
            backup) backup=$((backup + count)) ;;
          esac
        elif [[ -n "$control" && "$pattern" == "$control" ]]; then
          case "$role" in
            db|wal) live_control=$((live_control + count)) ;;
            backup) backup_control=$((backup_control + count)) ;;
          esac
        fi
        ;;
    esac
  done <<<"$report"
  for role in db wal backup; do
    if [[ "$roles" != *" $role "* ]]; then
      reason="the scan found no $role file"
      return 2
    fi
  done
  if (( name_counts != files )); then
    reason="the scan read $name_counts of $files files"
    return 2
  fi
  if [[ "$mode" == present ]]; then
    if (( live > 0 && backup > 0 )); then
      reason="the phrase is in $holders"
      return 0
    fi
    if (( live == 0 )); then
      reason='the phrase is in neither the database nor its write-ahead log'
    else
      reason='the phrase is not in the pre-migration backup'
    fi
    return 1
  fi
  if [[ -n "$holders" ]]; then
    reason="the phrase is still in $holders"
    return 1
  fi
  if [[ -n "$control" ]] && (( live_control == 0 || backup_control == 0 )); then
    reason="the control phrase is not in both the database (or its log) and the backup, so the scan may be blind"
    return 2
  fi
  reason="no file of the $files in the database directory holds the phrase"
  return 0
}

backup_mtime() {
  awk -F '\t' '$1 == "file" && $2 == "backup" { print $4; exit }' <<<"$1"
}

# check_backup_retaken BEFORE AFTER: the restart wrote a new pre-migration backup.
check_backup_retaken() {
  local before after
  before=$(backup_mtime "$1")
  after=$(backup_mtime "$2")
  if [[ ! "$after" =~ ^[0-9]+$ ]]; then
    reason='there is no pre-migration backup after the restart'
    return 2
  fi
  if [[ "$before" =~ ^[0-9]+$ ]] && (( after <= before )); then
    reason='the restart did not write a new pre-migration backup'
    return 2
  fi
  reason="the restart wrote a new pre-migration backup (modified at $after, before ${before:-absent})"
  return 0
}

# check_doc_holds_summary EXPORT SUMMARY: the Drive summary is the app's current summary, so a
# stale document cannot pass as one that lost the phrase.
check_doc_holds_summary() {
  local export=$1 summary=$2 words prefix
  if blank "$export"; then
    reason='nothing was read'
    return 2
  fi
  read -ra words <<<"$(tokens "$summary")"
  if (( ${#words[@]} < 3 )); then
    reason='the current summary is too short to compare'
    return 2
  fi
  prefix="${words[*]:0:8}"
  if [[ " $(tokens "$export") " == *" $prefix "* ]]; then
    reason='the Drive document holds the current summary'
    return 0
  fi
  reason='the Drive document does not hold the current summary'
  return 2
}

# check_session_settled DETAIL: the conversation ended, was refined and summarized, and is
# synced with no Drive replacement owed.
check_session_settled() {
  local state
  state=$(jq -r '.session | if type != "object" then "unreadable"
    elif .summary_status == "failed" or .refinement_status == "failed" or .refinement_status == "no_audio" then
      "failed\tsummary \(.summary_status) (\(.summary_error // "")), refinement \(.refinement_status)"
    elif .status == "ended" and .summary_status == "completed" and (.summary | type == "string" and length > 0)
      and .refinement_status == "completed" and .sync_state == "SYNCED"
      and (.gdrive_folder_id | type == "string" and length > 0) and (.drive_replace_pending | not) then "settled"
    else "waiting\tstatus \(.status), summary \(.summary_status), refinement \(.refinement_status), sync \(.sync_state), Drive replacement owed \(.drive_replace_pending)"
    end' <<<"$1" 2>/dev/null)
  case "${state%%$'\t'*}" in
    settled)
      reason='ended, refined, summarized and synced'
      return 0
      ;;
    failed)
      reason=${state#*$'\t'}
      return 1
      ;;
    waiting)
      reason=${state#*$'\t'}
      return 2
      ;;
    *)
      reason='the session could not be read'
      return 2
      ;;
  esac
}

# segment_ids_matching DETAIL PATTERN: the ids of timed, batch-derived
# segments whose text holds PATTERN. Live segments must never reach deletion.
segment_ids_matching() {
  local id text
  while IFS=$'\t' read -r id text; do
    if [[ "$id" =~ ^[0-9]+$ ]] && matches "$text" "$2"; then
      printf '%s\n' "$id"
    fi
  done < <(jq -r '.segments[]? | select((.transcript_revision // 0) > 0) | [(.id | tostring), (.text // "")] | @tsv' <<<"$1" 2>/dev/null)
}

segment_revision() {
  jq -r --argjson id "$2" '[.segments[]? | select(.id == $id) | .transcript_revision] | if length == 1 then .[0] else empty end' <<<"$1" 2>/dev/null
}

# choose_segments waits for completed refinement and then selects the unique
# timed segments and revisions that carry each acceptance phrase.
choose_segments() {
  local detail=$1 text_only_ids audio_ids control_ids
  text_only_ids=$(segment_ids_matching "$detail" "$text_only_pair")
  audio_ids=$(segment_ids_matching "$detail" "$audio_pair")
  if [[ $(line_count "$text_only_ids") != 1 ]]; then
    reason="\"$text_only_phrase\" is in $(line_count "$text_only_ids") refined timed segments, not one"
    return 2
  fi
  if [[ $(line_count "$audio_ids") != 1 ]]; then
    reason="\"$audio_phrase\" is in $(line_count "$audio_ids") refined timed segments, not one"
    return 2
  fi
  if [[ "$text_only_ids" == "$audio_ids" ]]; then
    reason="both phrases are in segment $audio_ids"
    return 2
  fi
  text_only_revision=$(segment_revision "$detail" "$text_only_ids")
  audio_revision=$(segment_revision "$detail" "$audio_ids")
  if [[ ! "$text_only_revision" =~ ^[1-9][0-9]*$ ]] || [[ ! "$audio_revision" =~ ^[1-9][0-9]*$ ]]; then
    reason='the selected segment has no positive transcript revision'
    return 2
  fi
  if [[ "$(segment_ids_matching "$detail" "$text_only_trace")" != "$text_only_ids" ]]; then
    reason="a word of \"$text_only_phrase\" is in another segment too"
    return 2
  fi
  if [[ "$(segment_ids_matching "$detail" "$audio_trace")" != "$audio_ids" ]]; then
    reason="a word of \"$audio_phrase\" is in another segment too"
    return 2
  fi
  control_ids=$(segment_ids_matching "$detail" "$control_pair" | grep -Fvx -e "$text_only_ids" -e "$audio_ids")
  if [[ -z "$control_ids" ]]; then
    reason="no other refined timed segment holds the control phrase \"$control_phrase\""
    return 2
  fi
  text_only_segment_id=$text_only_ids
  audio_segment_id=$audio_ids
  reason="segment $text_only_segment_id revision $text_only_revision holds \"$text_only_phrase\", segment $audio_segment_id revision $audio_revision holds \"$audio_phrase\", and segment $(one_line "$control_ids") holds \"$control_phrase\""
  return 0
}

# choose_audio_segment re-reads the replacement table after Delete text rebuilt
# every id and retry refinement advanced the transcript revision.
choose_audio_segment() {
  local detail=$1 audio_ids control_ids
  audio_ids=$(segment_ids_matching "$detail" "$audio_pair")
  if [[ $(line_count "$audio_ids") != 1 ]]; then
    reason="\"$audio_phrase\" is in $(line_count "$audio_ids") refined timed segments, not one"
    return 2
  fi
  if [[ "$(segment_ids_matching "$detail" "$audio_trace")" != "$audio_ids" ]]; then
    reason="a word of \"$audio_phrase\" is in another segment too"
    return 2
  fi
  audio_revision=$(segment_revision "$detail" "$audio_ids")
  if [[ ! "$audio_revision" =~ ^[1-9][0-9]*$ ]]; then
    reason='the audio segment has no positive transcript revision'
    return 2
  fi
  control_ids=$(segment_ids_matching "$detail" "$control_pair" | grep -Fvx -e "$audio_ids")
  if [[ -z "$control_ids" ]]; then
    reason="no other refined timed segment holds the control phrase \"$control_phrase\""
    return 2
  fi
  audio_segment_id=$audio_ids
  reason="segment $audio_segment_id revision $audio_revision holds \"$audio_phrase\""
  return 0
}

recording_hashes() {
  local dir=$1 detail=$2 index=0 recording hash
  while IFS= read -r recording; do
    [[ -n "$recording" ]] || continue
    hash=$(result "$dir" "unit.$index.sha256")
    printf '%s %s\n' "${hash:-missing}" "$recording"
    index=$((index + 1))
  done <<<"$(recording_paths "$detail")"
}

check_pi_evidence() {
  check 'evidence from the Pi' check_remote_returned "$(result "$1" remote.ok)"
  check 'Drive credential' check_drive_token "$(result "$1" token.state)" "$(result "$1" token.error)"
}

# check_drive_documents DIR LISTING MODE TRACE SUMMARY: the current transcript.md and summary.md.
# MODE present needs the phrase in both; MODE absent needs no trace, the control phrase in the
# transcript, and the app's current summary in summary.md.
check_drive_documents() {
  local dir=$1 listing=$2 mode=$3 trace=$4 summary=$5 transcript_id summary_id
  transcript_id=$(drive_id_named "$listing" transcript.md)
  summary_id=$(drive_id_named "$listing" summary.md)
  if [[ "$mode" == present ]]; then
    check "Drive transcript.md ($transcript_id) exported as text" answered "$(response_code "$dir" "file.$transcript_id.export" code)" \
      check_present "$(result "$dir" "file.$transcript_id.export.body")" "$trace"
    check "Drive summary.md ($summary_id) exported as text" answered "$(response_code "$dir" "file.$summary_id.export" code)" \
      check_present "$(result "$dir" "file.$summary_id.export.body")" "$trace"
    return
  fi
  check "Drive transcript.md ($transcript_id) exported as text" answered "$(response_code "$dir" "file.$transcript_id.export" code)" \
    check_absent "$(result "$dir" "file.$transcript_id.export.body")" "$trace" "$control_pair"
  check "Drive summary.md ($summary_id) exported as text" answered "$(response_code "$dir" "file.$summary_id.export" code)" \
    check_absent "$(result "$dir" "file.$summary_id.export.body")" "$trace"
  check "Drive summary.md ($summary_id) is the current summary" answered "$(response_code "$dir" "file.$summary_id.export" code)" \
    check_doc_holds_summary "$(result "$dir" "file.$summary_id.export.body")" "$summary"
}

# evaluate_text_only_before DIR DETAIL PHRASE_SEARCH CONTROL_SEARCH: before "Delete text", every
# check must find "orange tractor ninety"; that finding is the control for the same check after.
evaluate_text_only_before() {
  local dir=$1 detail=$2 phrase_search=$3 control_search=$4 listing name id index=0 recording
  check_pi_evidence "$dir"
  check 'session' check_session_settled "$detail"
  check 'segments' check_present "$(segment_texts "$detail")" "$text_only_pair"
  check 'transcript' check_present "$(session_field "$detail" canonical_transcript)" "$text_only_pair"
  check 'refined transcript' check_present "$(session_field "$detail" refined_transcript)" "$text_only_pair"
  check 'summary' check_present "$(session_field "$detail" summary)" "$text_only_pair"
  check "UI search for \"$text_only_query\"" check_search_finds "$phrase_search" "$acceptance_session_id"
  check "UI search for \"$control_query\"" check_search_finds "$control_search" "$acceptance_session_id"
  check 'database files and pre-migration backup' check_db_scan "$(result "$dir" db.report)" present text_only
  listing=$(result "$dir" folder.list.body)
  check 'Drive folder' check_listing "$(response_code "$dir" folder.list code)" "$listing"
  while IFS=$'\t' read -r name id; do
    [[ -n "$id" ]] || continue
    check "Drive $name ($id) files.get" check_answers_200 "$(response_code "$dir" "file.$id.get" code)"
  done <<<"$(drive_file_ids "$listing")"
  check_drive_documents "$dir" "$listing" present "$text_only_pair" ''
  check 'recordings' check_has_recording "$(recording_paths "$detail")"
  while IFS= read -r recording; do
    [[ -n "$recording" ]] || continue
    check "recording $recording on the unit is the one on Drive" check_same_md5 \
      "$(result "$dir" "unit.$index.md5")" "$(drive_md5_named "$listing" "$(recording_drive_name "$recording")")"
    index=$((index + 1))
  done <<<"$(recording_paths "$detail")"
}

# evaluate_text_only_after DIR DETAIL PHRASE_SEARCH CONTROL_SEARCH: after "Delete text" (and
# after a retry refinement), the A11 text only row's checks.
evaluate_text_only_after() {
  local dir=$1 detail=$2 phrase_search=$3 control_search=$4 listing name id summary
  summary=$(session_field "$detail" summary)
  check_pi_evidence "$dir"
  check 'session' check_session_settled "$detail"
  check 'segments' check_absent "$(segment_texts "$detail")" "$text_only_trace" "$control_pair"
  check 'transcript' check_absent "$(session_field "$detail" canonical_transcript)" "$text_only_trace" "$control_pair"
  check 'refined transcript' check_absent "$(session_field "$detail" refined_transcript)" "$text_only_trace" "$control_pair"
  check 'summary' check_absent "$summary" "$text_only_trace"
  check "UI search for \"$text_only_query\"" check_search_absent "$phrase_search" "$acceptance_session_id"
  check "UI search for \"$control_query\" (control)" check_search_finds "$control_search" "$acceptance_session_id"
  check 'database files and pre-migration backup' check_db_scan "$(result "$dir" db.report)" absent text_only control
  check 'recording path' check_same_text "$text_only_audio_path_before" "$(session_field "$detail" audio_path)"
  check 'recording sha256 on the unit' check_hashes_unchanged "$text_only_hashes_before" "$(recording_hashes "$dir" "$detail")"
  listing=$(result "$dir" folder.list.body)
  check 'Drive folder' check_listing "$(response_code "$dir" folder.list code)" "$listing"
  check 'Drive file ids' check_ids_unchanged "$text_only_ids_before" "$(drive_file_ids "$listing")"
  while IFS=$'\t' read -r name id; do
    [[ -n "$id" ]] || continue
    check "Drive $name ($id) files.get" check_answers_200 "$(response_code "$dir" "file.$id.get" code)"
  done <<<"$text_only_ids_before"
  check_drive_documents "$dir" "$listing" absent "$text_only_trace" "$summary"
}

# evaluate_audio_before DIR DETAIL PHRASE_SEARCH CONTROL_SEARCH: before "Delete text and audio",
# every check must find "purple elephant seventeen".
evaluate_audio_before() {
  local dir=$1 detail=$2 phrase_search=$3 control_search=$4 listing name id index=0 recording
  check_pi_evidence "$dir"
  check 'session' check_session_settled "$detail"
  check 'segments' check_present "$(segment_texts "$detail")" "$audio_pair"
  check 'transcript' check_present "$(session_field "$detail" canonical_transcript)" "$audio_pair"
  check 'refined transcript' check_present "$(session_field "$detail" refined_transcript)" "$audio_pair"
  check 'summary' check_present "$(session_field "$detail" summary)" "$audio_pair"
  check 'title' check_present "$(session_field "$detail" title)" "$audio_pair"
  check "UI search for \"$audio_query\"" check_search_finds "$phrase_search" "$acceptance_session_id"
  check "UI search for \"$control_query\"" check_search_finds "$control_search" "$acceptance_session_id"
  check 'Drive folder name' answered "$(response_code "$dir" folder.get code)" \
    check_present "$(drive_name "$(result "$dir" folder.get.body)")" "$audio_pair"
  listing=$(result "$dir" folder.list.body)
  check 'Drive folder' check_listing "$(response_code "$dir" folder.list code)" "$listing"
  while IFS=$'\t' read -r name id; do
    [[ -n "$id" ]] || continue
    check "Drive $name ($id) files.get" check_answers_200 "$(response_code "$dir" "file.$id.get" code)"
    check "Drive $name ($id) revisions.list" check_revisions_listed \
      "$(response_code "$dir" "file.$id.revisions" code)" "$(result "$dir" "file.$id.revisions.body")"
  done <<<"$(drive_file_ids "$listing")"
  check_drive_documents "$dir" "$listing" present "$audio_pair" ''
  check 'recordings' check_has_recording "$(recording_paths "$detail")"
  while IFS= read -r recording; do
    [[ -n "$recording" ]] || continue
    check "Deepgram transcription of $recording on the unit" answered "$(response_code "$dir" "unit.$index" dg_code)" \
      check_present "$(deepgram_transcript "$(result "$dir" "unit.$index.dg_body")")" "$audio_pair"
    index=$((index + 1))
  done <<<"$(recording_paths "$detail")"
}

# evaluate_audio_after DIR DETAIL PHRASE_SEARCH CONTROL_SEARCH: after "Delete text and audio",
# the A11 row's checks.
evaluate_audio_after() {
  local dir=$1 detail=$2 phrase_search=$3 control_search=$4 listing ids name id index=0 recording drive_id
  local segments transcript refined summary recordings
  segments=$(segment_texts "$detail")
  transcript=$(session_field "$detail" canonical_transcript)
  refined=$(session_field "$detail" refined_transcript)
  summary=$(session_field "$detail" summary)
  recordings=$(recording_paths "$detail")
  check_pi_evidence "$dir"
  check 'session' check_session_settled "$detail"
  check 'segments' check_absent "$segments" "$audio_trace" "$control_pair"
  check 'transcript' check_absent "$transcript" "$audio_trace" "$control_pair"
  check 'refined transcript' check_absent "$refined" "$audio_trace" "$control_pair"
  check 'summary' check_absent "$summary" "$audio_trace"
  check 'title' check_absent "$(session_field "$detail" title)" "$audio_trace"
  check "\"$text_only_phrase\" stays gone from the segments" check_absent "$segments" "$text_only_trace" "$control_pair"
  check "\"$text_only_phrase\" stays gone from the transcript" check_absent "$transcript" "$text_only_trace" "$control_pair"
  check "\"$text_only_phrase\" stays gone from the refined transcript" check_absent "$refined" "$text_only_trace" "$control_pair"
  check "\"$text_only_phrase\" stays gone from the summary" check_absent "$summary" "$text_only_trace"
  check "UI search for \"$audio_query\"" check_search_absent "$phrase_search" "$acceptance_session_id"
  check "UI search for \"$control_query\" (control)" check_search_finds "$control_search" "$acceptance_session_id"
  check 'Drive folder name' answered "$(response_code "$dir" folder.get code)" \
    check_absent "$(drive_name "$(result "$dir" folder.get.body)")" "$audio_trace"
  listing=$(result "$dir" folder.list.body)
  ids=$(drive_file_ids "$listing")
  check 'Drive folder' check_listing "$(response_code "$dir" folder.list code)" "$listing"
  check 'Drive file ids' check_ids_replaced "$audio_ids_before" "$ids"
  while IFS=$'\t' read -r name id; do
    [[ -n "$id" ]] || continue
    check "new Drive $name ($id) files.get" check_answers_200 "$(response_code "$dir" "file.$id.get" code)"
    check "new Drive $name ($id) revisions.list" check_one_revision \
      "$(response_code "$dir" "file.$id.revisions" code)" "$(result "$dir" "file.$id.revisions.body")"
  done <<<"$ids"
  while IFS=$'\t' read -r name id; do
    [[ -n "$id" ]] || continue
    check "old Drive $name ($id) files.get" check_answers_404 "$(response_code "$dir" "old.$id.get" code)"
    check "old Drive $name ($id) revisions.list" check_answers_404 "$(response_code "$dir" "old.$id.revisions" code)"
  done <<<"$audio_ids_before"
  check_drive_documents "$dir" "$listing" absent "$audio_trace" "$summary"
  check 'recordings' check_has_recording "$recordings"
  while IFS= read -r recording; do
    [[ -n "$recording" ]] || continue
    check "Deepgram transcription of $recording on the unit" answered "$(response_code "$dir" "unit.$index" dg_code)" \
      check_absent "$(deepgram_transcript "$(result "$dir" "unit.$index.dg_body")")" "$audio_trace" "$control_pair"
    drive_id=$(drive_id_named "$listing" "$(recording_drive_name "$recording")")
    check "new Drive $(recording_drive_name "$recording") ($drive_id) download" \
      check_answers_200 "$(response_code "$dir" "file.$drive_id.media" code)"
    check "new Drive $(recording_drive_name "$recording") ($drive_id) is the unit's recording" check_same_md5 \
      "$(result "$dir" "unit.$index.md5")" "$(result "$dir" "file.$drive_id.media.md5")"
    check "Deepgram transcription of the downloaded $(recording_drive_name "$recording")" \
      answered "$(response_code "$dir" "file.$drive_id.media" dg_code)" \
      check_absent "$(deepgram_transcript "$(result "$dir" "file.$drive_id.media.dg_body")")" "$audio_trace" "$control_pair"
    index=$((index + 1))
  done <<<"$recordings"
}

scan_patterns() {
  printf 'text_only=%s\naudio=%s\ncontrol=%s\n' "$text_only_pair" "$audio_pair" "$control_pair"
}

# remote_script REQUESTS [DEPLOY_DIR DATABASE]: a bash script for the Pi that runs each request
# ("op<TAB>key<TAB>arg<TAB>arg" lines) and writes its evidence as a tar stream on standard
# output. The Google and Deepgram credentials stay on the Pi: the script reads them there,
# hands each token to curl through a pipe, and never writes or prints one. DATABASE replaces the
# running service's open database, for the self-test.
remote_script() {
  local requests=$1 directory=${2:-$deploy_dir} database=${3:-}
  printf 'requests=%q\ndeploy_dir=%q\ndb_override=%q\ncredential_file=%q\ndrive_api=%q\ndrive_scope=%q\ndeepgram_url=%q\nscan_patterns=%q\n' \
    "$requests" "$directory" "$database" "$directory/google-credentials.json" "$drive_api" "$drive_scope" "$deepgram_url" "$(scan_patterns)"
  cat <<'EOF'
set -u
umask 077
token=
token_state=new

auth_header() {
  printf 'header = "Authorization: %s %s"\n' "$1" "$2"
}

record_error() {
  printf '%s\n' "$2" >>"$out/$1.error"
}

# The Pi's X.509 workload identity: exchange the certificate for a federated token at the
# security token service, then impersonate the service account for a read-only Drive token.
mint_token() {
  local config cert key audience token_url subject_type impersonation_url der subject response code federated
  case "$token_state" in
    ready) return 0 ;;
    failed) return 1 ;;
  esac
  token_state=failed
  printf 'failed' >"$out/token.state"
  config=$(jq -r '.credential_source.certificate.certificate_config_location // empty' "$credential_file" 2>/dev/null)
  audience=$(jq -r '.audience // empty' "$credential_file" 2>/dev/null)
  token_url=$(jq -r '.token_url // empty' "$credential_file" 2>/dev/null)
  subject_type=$(jq -r '.subject_token_type // empty' "$credential_file" 2>/dev/null)
  impersonation_url=$(jq -r '.service_account_impersonation_url // empty' "$credential_file" 2>/dev/null)
  cert=
  key=
  if [ -n "$config" ]; then
    cert=$(jq -r '.cert_configs.workload.cert_path // empty' "$config" 2>/dev/null)
    key=$(jq -r '.cert_configs.workload.key_path // empty' "$config" 2>/dev/null)
  fi
  if [ -z "$cert" ] || [ -z "$key" ] || [ -z "$audience" ] || [ -z "$token_url" ] \
    || [ -z "$subject_type" ] || [ -z "$impersonation_url" ]; then
    record_error token "$credential_file or its certificate config lacks a field the token exchange needs"
    return 1
  fi
  der=$(openssl x509 -in "$cert" -outform DER | base64 -w 0)
  if [ -z "$der" ]; then
    record_error token "could not read the certificate $cert"
    return 1
  fi
  subject=$(jq -cn --arg certificate "$der" '[$certificate]')
  response=$(curl --silent --show-error --max-time 60 --cert "$cert" --key "$key" \
    --data-urlencode "audience=$audience" \
    --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
    --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:access_token' \
    --data-urlencode 'scope=https://www.googleapis.com/auth/cloud-platform' \
    --data-urlencode "subject_token_type=$subject_type" \
    --data-urlencode "subject_token=$subject" \
    --write-out '\n%{http_code}' "$token_url" 2>>"$out/token.stderr")
  code=${response##*$'\n'}
  response=${response%$'\n'*}
  if [ "$code" != 200 ]; then
    record_error token "the security token exchange answered $code: $(printf '%s' "$response" | jq -r '.error_description // .error // "no detail"' 2>/dev/null)"
    return 1
  fi
  federated=$(printf '%s' "$response" | jq -r '.access_token // empty')
  response=$(curl --silent --show-error --max-time 60 --cert "$cert" --key "$key" \
    --config <(auth_header Bearer "$federated") \
    --header 'Content-Type: application/json' \
    --data "$(jq -cn --arg scope "$drive_scope" '{scope: [$scope], lifetime: "3600s"}')" \
    --write-out '\n%{http_code}' "$impersonation_url" 2>>"$out/token.stderr")
  code=${response##*$'\n'}
  response=${response%$'\n'*}
  if [ "$code" != 200 ]; then
    record_error token "the service account impersonation answered $code: $(printf '%s' "$response" | jq -r '.error.message // "no detail"' 2>/dev/null)"
    return 1
  fi
  token=$(printf '%s' "$response" | jq -r '.accessToken // empty')
  if [ -z "$token" ]; then
    record_error token 'the service account impersonation returned no token'
    return 1
  fi
  token_state=ready
  printf 'ready' >"$out/token.state"
}

# drive_call KEY DEST URL [CURL ARGS...]: one Drive API request; KEY.code holds its HTTP code.
drive_call() {
  local key=$1 dest=$2 url=$3
  shift 3
  if ! mint_token; then
    printf 'token' >"$out/$key.code"
    return
  fi
  curl --silent --show-error --globoff --max-time 900 \
    --config <(auth_header Bearer "$token") \
    --output "$dest" --write-out '%{http_code}' "$@" "$url" >"$out/$key.code" 2>>"$out/$key.stderr"
}

hash_file() {
  local key=$1 path=$2
  if [ ! -f "$path" ]; then
    record_error "$key" "no recording at $path"
    return
  fi
  sha256sum <"$path" | cut -d ' ' -f 1 >"$out/$key.sha256"
  md5sum <"$path" | cut -d ' ' -f 1 >"$out/$key.md5"
}

# transcribe KEY PATH: a Deepgram prerecorded transcription, with the key the service uses.
transcribe() {
  local key=$1 path=$2 type deepgram_key
  if [ ! -s "$path" ]; then
    record_error "$key" "no recording at $path"
    return
  fi
  case "$path" in
    *.wav) type=audio/wav ;;
    *) type=audio/mpeg ;;
  esac
  deepgram_key=$(sed -n 's/^GHOST_WISPR_DEEPGRAM_API_KEY=//p' "$deploy_dir/.env" 2>/dev/null | head -n 1)
  deepgram_key=${deepgram_key#\"}
  deepgram_key=${deepgram_key%\"}
  if [ -z "$deepgram_key" ]; then
    record_error "$key" "$deploy_dir/.env holds no Deepgram key"
    return
  fi
  curl --silent --show-error --max-time 900 --request POST \
    --config <(auth_header Token "$deepgram_key") \
    --header "Content-Type: $type" --data-binary "@$path" \
    --output "$out/$key.dg_body" --write-out '%{http_code}' "$deepgram_url" >"$out/$key.dg_code" 2>>"$out/$key.stderr"
}

# db_scan: grep every file in the directory of the database the service holds open (the
# database, its write-ahead log, the pre-migration backup and anything else there) for each
# pattern, as the app runs.
db_scan() {
  local report=$out/db.report pid fd target db=$db_override file role size mtime name pattern count
  : >"$report"
  if [ -z "$db" ]; then
    pid=$(XDG_RUNTIME_DIR="/run/user/$(id -u)" systemctl --user show --property=MainPID --value ghost-wispr.service 2>/dev/null)
    case "$pid" in
      ''|0|*[!0-9]*)
        printf 'error\tghost-wispr.service has no running process\n' >>"$report"
        return
        ;;
    esac
    for fd in /proc/"$pid"/fd/*; do
      target=$(readlink -- "$fd" 2>/dev/null) || continue
      case "$target" in
        *.db)
          if [ -n "$db" ] && [ "$db" != "$target" ]; then
            printf 'error\tthe service holds two databases open: %s and %s\n' "$db" "$target" >>"$report"
            return
          fi
          db=$target
          ;;
      esac
    done
    if [ -z "$db" ]; then
      printf 'error\tno database among the open files of service process %s\n' "$pid" >>"$report"
      return
    fi
  fi
  printf 'db\t%s\n' "$db" >>"$report"
  if ! find "${db%/*}" -maxdepth 1 -type f -print0 >"$work/db-files"; then
    printf 'error\tcould not list %s\n' "${db%/*}" >>"$report"
    return
  fi
  while IFS= read -r -d '' file; do
    case "$file" in
      "$db") role=db ;;
      "$db-wal") role=wal ;;
      "$db-shm") role=shm ;;
      "$db.pre-migrate.bak") role=backup ;;
      *) role=other ;;
    esac
    size=$(stat -c %s -- "$file" 2>/dev/null) || size=error
    mtime=$(stat -c %Y -- "$file" 2>/dev/null) || mtime=error
    printf 'file\t%s\t%s\t%s\t%s\n' "$role" "$size" "$mtime" "$file" >>"$report"
    while IFS='=' read -r name pattern; do
      [ -n "$name" ] || continue
      count=$(LC_ALL=C grep -a -c -i -E -e "$pattern" -- "$file" 2>/dev/null)
      if [ $? -gt 1 ]; then
        count=error
      fi
      printf 'count\t%s\t%s\t%s\t%s\n' "$role" "$name" "$count" "$file" >>"$report"
    done <<<"$scan_patterns"
  done <"$work/db-files"
}

valid_id() {
  case "$1" in
    ''|*[!A-Za-z0-9_-]*) return 1 ;;
  esac
}

# drive_folder FOLDER MEDIA: the folder's name and files; each file's files.get and
# revisions.list; each Google Doc exported as text; with MEDIA "yes", each recording downloaded,
# hashed and transcribed.
drive_folder() {
  local folder=$1 media=$2 id name mime extension
  if ! valid_id "$folder"; then
    record_error folder.list "not a Drive folder id: $folder"
    return
  fi
  drive_call folder.get "$out/folder.get.body" "$drive_api/files/$folder?supportsAllDrives=true&fields=id,name,trashed"
  drive_call folder.list "$out/folder.list.body" "$drive_api/files" --get \
    --data-urlencode "q='$folder' in parents and trashed = false" \
    --data-urlencode 'supportsAllDrives=true' --data-urlencode 'includeItemsFromAllDrives=true' \
    --data-urlencode 'pageSize=100' \
    --data-urlencode 'fields=nextPageToken,files(id,name,mimeType,md5Checksum)'
  [ "$(cat "$out/folder.list.code")" = 200 ] || return
  jq -r '.files[]? | [.id, .name, .mimeType] | @tsv' "$out/folder.list.body" >"$work/folder-files" 2>/dev/null || return
  while IFS="$(printf '\t')" read -r id name mime; do
    valid_id "$id" || continue
    drive_call "file.$id.get" "$out/file.$id.get.body" "$drive_api/files/$id?supportsAllDrives=true&fields=id,name,md5Checksum"
    drive_call "file.$id.revisions" "$out/file.$id.revisions.body" "$drive_api/files/$id/revisions?fields=revisions(id,modifiedTime)"
    if [ "$mime" = application/vnd.google-apps.document ]; then
      drive_call "file.$id.export" "$out/file.$id.export.body" "$drive_api/files/$id/export?mimeType=text/plain"
    elif [ "$media" = yes ]; then
      extension=${name##*.}
      [ "$extension" != "$name" ] || extension=bin
      drive_call "file.$id.media" "$work/media-$id.$extension" "$drive_api/files/$id?alt=media&supportsAllDrives=true"
      hash_file "file.$id.media" "$work/media-$id.$extension"
      transcribe "file.$id.media" "$work/media-$id.$extension"
    fi
  done <"$work/folder-files"
}

# drive_old IDS: files.get and revisions.list for each file id the folder held before.
drive_old() {
  local id
  for id in $1; do
    valid_id "$id" || continue
    drive_call "old.$id.get" "$work/old-$id.get" "$drive_api/files/$id?supportsAllDrives=true&fields=id,name,trashed"
    drive_call "old.$id.revisions" "$work/old-$id.revisions" "$drive_api/files/$id/revisions?fields=revisions(id)"
  done
}

main() {
  local op key first second
  out=$(mktemp -d) || exit 2
  work=$(mktemp -d) || exit 2
  trap 'rm -rf -- "$out" "$work"' EXIT
  if cd "$deploy_dir"; then
    while IFS="$(printf '\t')" read -r op key first second; do
      case "$op" in
        '') ;;
        db_scan) db_scan ;;
        hash) hash_file "$key" "$first" ;;
        transcribe) transcribe "$key" "$first" ;;
        drive_folder) drive_folder "$first" "$second" ;;
        drive_old) drive_old "$first" ;;
        *) record_error "$key" "unknown request $op" ;;
      esac
    done <<<"$requests"
    printf 'done' >"$out/remote.ok"
  else
    record_error remote "cannot enter $deploy_dir"
  fi
  tar -C "$out" -cf - .
}

main </dev/null
EOF
}

remote_run() {
  ssh -o BatchMode=yes "$remote" "$@"
}

# gather DIR REQUESTS: run the requests on the Pi and unpack its evidence into DIR.
gather() {
  remote_run 'bash -s' <<<"$(remote_script "$2")" | tar -x -C "$1" -f - 2>/dev/null
}

status() {
  curl --fail --silent --show-error --max-time 60 "$unit_url/api/status"
}

running_config() {
  curl --fail --silent --show-error --max-time 60 "$unit_url/api/config"
}

session_detail() {
  curl --fail --silent --show-error --max-time 60 "$unit_url/api/sessions/$1"
}

post_json() {
  curl --fail --silent --show-error --max-time 60 --request POST \
    --header 'Content-Type: application/json' --data "$2" "$unit_url$1"
}

# search_ui QUERY: the keyword search the UI's search box sends.
search_ui() {
  curl --fail --silent --show-error --max-time 60 --get --data-urlencode "q=$1" "$unit_url/api/search"
}

# delete_segment SESSION SEGMENT QUERY BODY_FILE: the deletion the UI's segment menu sends;
# prints the HTTP code. The request runs the deletion through, so it can take minutes.
delete_segment() {
  curl --silent --show-error --max-time 900 --request DELETE --output "$4" --write-out '%{http_code}' \
    "$unit_url/api/sessions/$1/segments/$2${3:+?$3}"
}

unit_requests() {
  local detail=$1 index=0 recording op
  shift
  while IFS= read -r recording; do
    [[ -n "$recording" ]] || continue
    for op in "$@"; do
      printf '%s\tunit.%d\t%s\t-\n' "$op" "$index" "$recording"
    done
    index=$((index + 1))
  done <<<"$(recording_paths "$detail")"
}

text_only_requests() {
  printf 'db_scan\tdb\t-\t-\n'
  unit_requests "$1" hash
  printf 'drive_folder\tfolder\t%s\tno\n' "$(session_field "$1" gdrive_folder_id)"
}

audio_requests() {
  local detail=$1 phase=$2
  if [[ "$phase" == before ]]; then
    unit_requests "$detail" transcribe
    printf 'drive_folder\tfolder\t%s\tno\n' "$(session_field "$detail" gdrive_folder_id)"
    return
  fi
  unit_requests "$detail" hash transcribe
  printf 'drive_folder\tfolder\t%s\tyes\n' "$(session_field "$detail" gdrive_folder_id)"
  printf 'drive_old\told\t%s\t-\n' "$(cut -f2 <<<"$audio_ids_before" | tr '\n' ' ')"
}

# text_only_round PHASE: one round of A11 text only evidence and checks, before or after.
text_only_round() {
  local phase=$1 dir detail phrase_search control_search
  round_reset
  dir=$(mktemp -d "$scratch_dir/text-only.XXXXXX")
  detail=$(session_detail "$acceptance_session_id") || detail=
  phrase_search=$(search_ui "$text_only_query") || phrase_search=
  control_search=$(search_ui "$control_query") || control_search=
  gather "$dir" "$(text_only_requests "$detail")"
  if [[ "$phase" == before ]]; then
    evaluate_text_only_before "$dir" "$detail" "$phrase_search" "$control_search"
    text_only_ids_before=$(drive_file_ids "$(result "$dir" folder.list.body)")
    text_only_hashes_before=$(recording_hashes "$dir" "$detail")
    text_only_audio_path_before=$(session_field "$detail" audio_path)
  else
    evaluate_text_only_after "$dir" "$detail" "$phrase_search" "$control_search"
  fi
}

# audio_round PHASE: one round of A11 evidence and checks, before or after.
audio_round() {
  local phase=$1 dir detail phrase_search control_search
  round_reset
  dir=$(mktemp -d "$scratch_dir/audio.XXXXXX")
  detail=$(session_detail "$acceptance_session_id") || detail=
  phrase_search=$(search_ui "$audio_query") || phrase_search=
  control_search=$(search_ui "$control_query") || control_search=
  gather "$dir" "$(audio_requests "$detail" "$phase")"
  if [[ "$phase" == before ]]; then
    evaluate_audio_before "$dir" "$detail" "$phrase_search" "$control_search"
    audio_ids_before=$(drive_file_ids "$(result "$dir" folder.list.body)")
  else
    evaluate_audio_after "$dir" "$detail" "$phrase_search" "$control_search"
  fi
}

# poll_after ROW ROUND: repeat the after-delete round until every check passes or time runs
# out (Drive changes land after the request returns), then print the last round.
poll_after() {
  local row=$1 round=$2 attempt result=2
  for (( attempt = 1; attempt <= after_attempts; attempt++ )); do
    "$round" after
    round_result
    result=$?
    (( result == 0 )) && break
    if (( attempt < after_attempts )); then
      printf '%s RAN: after-delete round %d: %d failed, %d unknown; checking again in %d s.\n' \
        "$row" "$attempt" "$round_failed" "$round_unknown" "$poll_seconds"
      sleep "$poll_seconds"
    fi
  done
  print_round "$row"
  return "$result"
}

# retry_refinement: the UI's "retry refinement", then poll until a newer
# refinement revision completes.
retry_refinement() {
  local response detail state revision before_revision attempt
  if ! detail=$(session_detail "$acceptance_session_id"); then
    printf 'A11 text only FAILED: could not read the refinement before retrying\n' >&2
    return 1
  fi
  before_revision=$(session_field "$detail" refined_revision)
  if ! [[ "$before_revision" =~ ^[0-9]+$ ]]; then
    printf 'A11 text only FAILED: the refinement before retrying has no revision\n' >&2
    return 1
  fi
  response=$(post_json "/api/sessions/$acceptance_session_id/retry-refinement" '{}') || {
    printf 'A11 text only FAILED: retry refinement was refused\n' >&2
    return 1
  }
  if ! jq -e '.triggered == true' >/dev/null 2>&1 <<<"$response"; then
    printf 'A11 text only FAILED: retry refinement did not confirm its request\n' >&2
    return 1
  fi
  for (( attempt = 1; attempt <= refinement_attempts; attempt++ )); do
    if detail=$(session_detail "$acceptance_session_id"); then
      state=$(session_field "$detail" refinement_status)
      revision=$(session_field "$detail" refined_revision)
      case "$state" in
        completed)
          if [[ "$revision" =~ ^[0-9]+$ ]] && (( 10#$revision > 10#$before_revision )); then
            printf 'A11 text only RAN: retry refinement completed at revision %s.\n' "$revision"
            return 0
          fi
          ;;
        failed|no_audio)
          printf 'A11 text only FAILED: retry refinement ended %s\n' "$state" >&2
          return 1
          ;;
      esac
    fi
    sleep 2
  done
  printf 'A11 text only FAILED: retry refinement did not complete a newer revision in %d minutes\n' "$((refinement_attempts * 2 / 60))" >&2
  return 1
}

run_a11_text_only() {
  local body_file="$scratch_dir/delete-text.json" code result
  if [[ "$conversation_ready" != true ]]; then
    printf 'A11 text only UNKNOWN: not run: %s\n' "$conversation_problem"
    return 2
  fi
  printf 'A11 text only RAN: before "Delete text", every check must find "%s"; that finding is the control for the same check after the delete.\n' "$text_only_phrase"
  text_only_round before
  print_round 'A11 text only'
  if ! round_result; then
    printf 'A11 text only UNKNOWN: a check could not see "%s" before the delete, so its absence after the delete would prove nothing. Nothing was deleted.\n' "$text_only_phrase"
    return 2
  fi
  printf 'A11 text only RAN: choose "Delete text" on segment %s revision %s: DELETE /api/sessions/%s/segments/%s?mode=text&revision=%s\n' \
    "$text_only_segment_id" "$text_only_revision" "$acceptance_session_id" "$text_only_segment_id" "$text_only_revision"
  code=$(delete_segment "$acceptance_session_id" "$text_only_segment_id" "mode=text&revision=$text_only_revision" "$body_file")
  if [[ "$code" != 204 ]]; then
    printf 'A11 text only FAILED: "Delete text" answered %s: %s\n' "${code:-nothing}" "$(result "$scratch_dir" delete-text.json)" >&2
    return 1
  fi
  printf 'A11 text only RAN: after "Delete text", every check must find no trace of "%s".\n' "$text_only_phrase"
  poll_after 'A11 text only' text_only_round
  result=$?
  printf 'A11 text only RAN: choose "retry refinement" (POST /api/sessions/%s/retry-refinement), then check again.\n' "$acceptance_session_id"
  retry_refinement
  result=$(worse "$result" "$?")
  poll_after 'A11 text only' text_only_round
  result=$(worse "$result" "$?")
  return "$result"
}

run_a11() {
  local body_file="$scratch_dir/delete-text-and-audio.json" code detail
  if [[ "$conversation_ready" != true ]]; then
    printf 'A11 UNKNOWN: not run: %s\n' "$conversation_problem"
    return 2
  fi
  detail=$(session_detail "$acceptance_session_id") || {
    printf 'A11 UNKNOWN: cannot read refined segments before audio delete\n'
    return 2
  }
  if ! choose_audio_segment "$detail"; then
    printf 'A11 UNKNOWN: %s\n' "$reason"
    return 2
  fi
  printf 'A11 RAN: before "Delete text and audio", every check must find "%s"; that finding is the control for the same check after the delete.\n' "$audio_phrase"
  audio_round before
  print_round A11
  if ! round_result; then
    printf 'A11 UNKNOWN: a check could not see "%s" before the delete, so its absence after the delete would prove nothing. Nothing was deleted.\n' "$audio_phrase"
    return 2
  fi
  printf 'A11 RAN: choose "Delete text and audio" on segment %s revision %s: DELETE /api/sessions/%s/segments/%s?mode=text_and_audio&revision=%s\n' \
    "$audio_segment_id" "$audio_revision" "$acceptance_session_id" "$audio_segment_id" "$audio_revision"
  code=$(delete_segment "$acceptance_session_id" "$audio_segment_id" "mode=text_and_audio&revision=$audio_revision" "$body_file")
  if [[ "$code" != 204 ]]; then
    printf 'A11 FAILED: "Delete text and audio" answered %s: %s\n' "${code:-nothing}" "$(result "$scratch_dir" delete-text-and-audio.json)" >&2
    return 1
  fi
  printf 'A11 RAN: after "Delete text and audio", every check must find no trace of "%s".\n' "$audio_phrase"
  poll_after A11 audio_round
}

# run_mode_guard: a deletion without a mode, or with an unknown one, must be refused before
# anything is read. A build without the modes would read "mode=text" as today's deletion and
# splice the audio, so no deletion runs unless this passes.
run_mode_guard() {
  local body_file="$scratch_dir/mode-guard.json" code query
  round_reset
  for query in '' 'mode=bogus'; do
    rm -f -- "$body_file"
    code=$(delete_segment "$probe_session_id" "$probe_segment_id" "$query" "$body_file")
    check "DELETE /api/sessions/$probe_session_id/segments/$probe_segment_id${query:+?$query}" \
      check_mode_required "$code" "$(result "$scratch_dir" mode-guard.json)"
  done
  print_round 'Mode guard'
  round_result
}

print_conversation_script() {
  cat <<EOF
Setup NEEDS-PERSON: record this conversation now; conversation $acceptance_session_id is recording.
  It takes about two minutes. Two people read it, A and B. Wait three seconds before and after
  each line marked >>, and say nothing else in that turn. Do not say "purple", "elephant",
  "seventeen", "orange", "tractor" or "ninety" anywhere else. After it, the driver chooses
  "Delete text" on the "$text_only_phrase" segment and "Delete text and audio" on the
  "$audio_phrase" segment; do not delete anything yourself. Press Enter after the last line.

     A: Good morning, and thanks for joining. Today we choose the name of the new warehouse robot project.
     B: Good morning. I spoke with the team yesterday about the robot and its work.
     A: What does the robot do, in short?
     B: It moves boxes from the loading dock to the shelves in the north building, day and night.
     A: How many boxes can it move in one hour?
     B: About two hundred boxes, if the floor is clear and the shelves are labelled.
     A: And when does the first test start?
     B: The first test starts in March, on a Monday morning, with Dana and Lee.
     A: Good. We need a project name before that test. Do you have one?
     B: Yes. The team agreed on one name.
     A: Please tell me the name.
  >> B: Purple elephant seventeen.
     A: Good. That is the project name from now on. Please put it on the plan.
     B: I will add it to the plan today.
     A: Next, the budget. We have twelve thousand dollars for the first stage.
     B: That pays for the robot, the charger and the safety fence.
     A: One more decision. Which machine do we order to clear the yard?
  >> B: Orange tractor ninety.
     A: Agreed. Order it this week. That is our action item.
     B: Last thing: the code word for the meeting room is blue harbor lantern.
     A: Noted. I will send the notes tomorrow. Thanks, everyone.
     B: Thank you. Goodbye.
EOF
}

# precheck: before the conversation, no copy of either phrase is on the unit, so whatever the
# checks find is this conversation's.
precheck() {
  local dir
  round_reset
  dir=$(mktemp -d "$scratch_dir/precheck.XXXXXX")
  gather "$dir" $'db_scan\tdb\t-\t-'
  check 'evidence from the Pi' check_remote_returned "$(result "$dir" remote.ok)"
  check "database files before the conversation hold no \"$text_only_phrase\"" \
    check_db_scan "$(result "$dir" db.report)" absent text_only
  check "database files before the conversation hold no \"$audio_phrase\"" \
    check_db_scan "$(result "$dir" db.report)" absent audio
  check "UI search for \"$text_only_query\" before the conversation" check_search_empty "$(search_ui "$text_only_query")"
  check "UI search for \"$audio_query\" before the conversation" check_search_empty "$(search_ui "$audio_query")"
  print_round Precheck
  if ! round_result; then
    conversation_problem='a copy of a phrase is already on the unit, so the checks could not tell it from this conversation; delete that conversation from the unit, then run again'
    return 2
  fi
}

record_conversation() {
  local start_response stop_response
  start_response=$(post_json /api/sessions/start '{}') || {
    conversation_problem='could not start a conversation (is one recording, or is capture paused?)'
    return 2
  }
  acceptance_session_id=$(jq -er '.session_id | select(type == "string" and test("^[A-Za-z0-9_-]+$"))' <<<"$start_response") || {
    conversation_problem='/api/sessions/start returned no session id'
    return 2
  }
  print_conversation_script
  read -r _
  stop_response=$(post_json "/api/sessions/$acceptance_session_id/stop" '{}') || {
    conversation_problem="could not stop conversation $acceptance_session_id"
    return 2
  }
  if ! jq -e '.status == "stopped"' >/dev/null 2>&1 <<<"$stop_response"; then
    conversation_problem="/api/sessions/$acceptance_session_id/stop did not confirm the stop"
    return 2
  fi
  printf 'Setup RAN: conversation %s ended.\n' "$acceptance_session_id"
}

wait_settled() {
  local detail attempt result
  for (( attempt = 1; attempt <= settle_attempts; attempt++ )); do
    detail=$(session_detail "$acceptance_session_id") || detail=
    check_session_settled "$detail"
    result=$?
    if (( result == 0 )); then
      printf 'Setup RAN: conversation %s is %s.\n' "$acceptance_session_id" "$reason"
      return 0
    fi
    if (( result == 1 )); then
      conversation_problem="conversation $acceptance_session_id: $reason"
      return 2
    fi
    sleep "$poll_seconds"
  done
  conversation_problem="conversation $acceptance_session_id did not settle in $((settle_attempts * poll_seconds / 60)) minutes: $reason"
  return 2
}

wait_for_status() {
  local attempt
  for (( attempt = 1; attempt <= 90; attempt++ )); do
    if status >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# retake_backup: Ghost Wispr writes the pre-migration backup at each start, so a restart after
# the conversation makes a backup that holds both phrases: the control for the backup check.
retake_backup() {
  local before_dir after_dir current_status active='' attempt asked=false
  for (( attempt = 1; attempt <= settle_attempts; attempt++ )); do
    current_status=$(status) || current_status=
    active=$(jq -r '.active_session_id // ""' <<<"$current_status" 2>/dev/null)
    if [[ -n "$current_status" && -z "$active" ]]; then
      break
    fi
    if [[ "$asked" == false && -n "$active" ]]; then
      printf 'Setup NEEDS-PERSON: conversation %s is recording; the driver restarts Ghost Wispr once it ends. End it in the UI to go on now.\n' "$active"
      asked=true
    fi
    sleep "$poll_seconds"
  done
  if [[ -z "$current_status" || -n "$active" ]]; then
    conversation_problem='Ghost Wispr could not be restarted for the backup control: a conversation kept recording or /api/status did not answer'
    return 2
  fi
  before_dir=$(mktemp -d "$scratch_dir/backup-before.XXXXXX")
  gather "$before_dir" $'db_scan\tdb\t-\t-'
  printf 'Setup RAN: restart ghost-wispr.service so its start-up backup (the pre-migration backup) holds this conversation; the backup check needs it as its control.\n'
  # shellcheck disable=SC2016 # The remote shell expands the service runtime path.
  if ! remote_run 'XDG_RUNTIME_DIR="/run/user/$(id -u)" systemctl --user restart ghost-wispr.service' </dev/null; then
    conversation_problem='could not restart ghost-wispr.service'
    return 2
  fi
  if ! wait_for_status; then
    conversation_problem='/api/status did not answer within three minutes of the restart'
    return 2
  fi
  after_dir=$(mktemp -d "$scratch_dir/backup-after.XXXXXX")
  gather "$after_dir" $'db_scan\tdb\t-\t-'
  if ! check_backup_retaken "$(result "$before_dir" db.report)" "$(result "$after_dir" db.report)"; then
    conversation_problem=$reason
    return 2
  fi
  printf 'Setup RAN: %s.\n' "$reason"
}

# prepare_conversation: the mode guard, the precheck, the recorded conversation, its two
# segments and the backup control that both rows share.
prepare_conversation() {
  local current_config detail result
  if ! status >/dev/null; then
    conversation_problem="/api/status did not answer at $unit_url"
    return 2
  fi
  current_config=$(running_config) || {
    conversation_problem='/api/config did not answer'
    return 2
  }
  if ! jq -e '.gdrive.sync_enabled == true and (.gdrive.folder_id | type == "string" and length > 0)' \
    >/dev/null 2>&1 <<<"$current_config"; then
    conversation_problem='Drive sync is off or has no folder, so the Drive checks cannot run'
    return 2
  fi
  run_mode_guard
  result=$?
  if (( result != 0 )); then
    conversation_problem='the deployed build does not refuse a deletion without a known mode, so "mode=text" could delete the audio too; nothing was deleted'
    return "$result"
  fi
  if [[ -n "$given_session" ]]; then
    acceptance_session_id=$given_session
    printf 'Setup RAN: use conversation %s; the database precheck is skipped.\n' "$acceptance_session_id"
  else
    precheck || return
    record_conversation || return
  fi
  wait_settled || return
  detail=$(session_detail "$acceptance_session_id") || detail=
  if ! choose_segments "$detail"; then
    conversation_problem=$reason
    return 2
  fi
  printf 'Setup RAN: %s.\n' "$reason"
  retake_backup || return
  wait_settled || return
  conversation_ready=true
}

run_row() {
  local row=$1 result
  shift
  "$@"
  result=$?
  case "$result" in
    0) printf '%s verdict: PASS\n' "$row" ;;
    1) printf '%s verdict: FAIL\n' "$row" >&2 ;;
    *) printf '%s verdict: UNKNOWN\n' "$row" ;;
  esac
  overall=$(worse "$overall" "$result")
}

# expect WANT DESCRIPTION CHECK ARGS...: one self-test assertion on a check's result.
expect() {
  local want=$1 description=$2 got
  shift 2
  reason=
  "$@" >/dev/null 2>&1
  got=$?
  (( got == want )) || fail "self-test: $description: got $got, want $want ($reason)"
}

# evaluated EVALUATOR ARGS...: one round's result, with its lines that did not pass as the reason.
evaluated() {
  local line summary=''
  round_reset
  "$@"
  for line in "${round_lines[@]}"; do
    [[ "$line" == PASS:* ]] || summary+="$line; "
  done
  reason=$summary
  round_result
}

plant() {
  printf '%s' "$3" >"$1/$2"
}

drive_listing() {
  local entries='' name id
  while (( $# > 0 )); do
    name=$1
    id=$2
    shift 2
    case "$name" in
      *.md) entries+="{\"id\":\"$id\",\"name\":\"$name\",\"mimeType\":\"$google_doc_type\"}," ;;
      *) entries+="{\"id\":\"$id\",\"name\":\"$name\",\"mimeType\":\"audio/mpeg\",\"md5Checksum\":\"$planted_md5\"}," ;;
    esac
  done
  printf '{"files":[%s]}' "${entries%,}"
}

deepgram_body() {
  jq -cn --arg transcript "$1" '{results: {channels: [{alternatives: [{transcript: $transcript}]}]}}'
}

# fake_curl: a curl for the self-test that answers as the token service, IAM, Drive and
# Deepgram would, refuses a request whose token or key is not in its --config, logs each
# request's data to FAKE_REQUESTS, and logs to FAKE_LOG any secret it sees in an argument.
fake_curl() {
  cat <<'EOF'
#!/usr/bin/env bash
set -u
output='' write_out='' url='' config='' sent=()
for argument in "$@"; do
  if [[ "$argument" == *secret* ]]; then
    printf 'a secret in an argument: %s\n' "$argument" >>"$FAKE_LOG"
  fi
done
while (( $# > 0 )); do
  case "$1" in
    --silent|--show-error|--globoff|--get) shift ;;
    --output) output=$2; shift 2 ;;
    --write-out) write_out=$2; shift 2 ;;
    --config) config=$(<"$2"); shift 2 ;;
    --data-urlencode|--data|--data-binary) sent+=("$2"); shift 2 ;;
    --max-time|--cert|--key|--header|--request) shift 2 ;;
    -*) printf 'an unexpected option: %s\n' "$1" >>"$FAKE_LOG"; shift ;;
    *) url=$1; shift ;;
  esac
done
{
  printf '%s' "$url"
  printf '\t%s' "${sent[@]}"
  printf '\n'
} >>"$FAKE_REQUESTS"
authorized() {
  [[ "$config" == *"header = \"Authorization: $1\""* ]]
}
code=200 body='' media=''
case "$url" in
  https://sts.test/*) body='{"access_token":"federated-not-a-secret-1","token_type":"Bearer"}' ;;
  https://iam.test/*)
    authorized 'Bearer federated-not-a-secret-1' || code=401
    body='{"accessToken":"drive-not-a-secret-2","expireTime":"2026-11-12T11:00:00Z"}'
    ;;
  https://api.deepgram.com/*)
    authorized 'Token deepgram-not-a-secret-3' || code=401
    body=$FAKE_TRANSCRIPT
    ;;
  https://www.googleapis.com/drive/v3/*)
    authorized 'Bearer drive-not-a-secret-2' || code=401
    case "${url#https://www.googleapis.com/drive/v3}" in
      /files) body=$FAKE_LISTING ;;
      /files/F1\?*) body='{"id":"F1","name":"2026-11-12-warehouse-robot-project","trashed":false}' ;;
      /files/[TSA]1\?*|/files/[TSA]1/*) code=404; body='{"error":{"code":404,"message":"File not found"}}' ;;
      /files/A2\?alt=media*) media=$FAKE_MEDIA ;;
      /files/*/revisions\?*) body='{"revisions":[{"id":"1","modifiedTime":"2026-11-12T10:30:00Z"}]}' ;;
      /files/T2/export\?*) body=$'A: Please tell me the name.\nB: the code word is blue harbor lantern.\n' ;;
      /files/S2/export\?*) body=$FAKE_SUMMARY ;;
      /files/*\?*) body='{"id":"planted"}' ;;
      *) code=404 ;;
    esac
    ;;
  *)
    printf 'an unexpected URL: %s\n' "$url" >>"$FAKE_LOG"
    code=000
    ;;
esac
if [[ -n "$media" ]]; then
  cat -- "$media" >"${output:-/dev/stdout}"
elif [[ -n "$output" ]]; then
  printf '%s' "$body" >"$output"
else
  printf '%s' "$body"
fi
printf '%b' "${write_out//'%{http_code}'/$code}"
EOF
}

# fake_openssl: "openssl x509 -in FILE -outform DER" prints FILE.
fake_openssl() {
  cat <<'EOF'
#!/usr/bin/env bash
while (( $# > 0 )); do
  if [[ "$1" == -in ]]; then
    exec cat -- "$2"
  fi
  shift
done
exit 1
EOF
}

run_self_test() (
  local controls_dir dir variant detail listing report db_root database scan_dir marker sha_before sha_after
  local fifty=() index
  controls_dir=$(mktemp -d)
  trap 'rm -rf -- "$controls_dir"' EXIT
  scratch_dir=$controls_dir
  acceptance_session_id=20261112100000
  planted_md5=0123456789abcdef0123456789abcdef
  sha_before=$(printf 'a%.0s' {1..64})
  sha_after=$(printf 'b%.0s' {1..64})
  for index in $(seq 1 "$search_limit"); do
    fifty+=("{\"session_id\":\"2026010110$(printf '%04d' "$index")\"}")
  done

  # A11 text only: each pure check against a planted failure and a planted pass.
  expect 0 'a transcript with the phrase written as digits holds it' check_present 'B: Orange tractor 90.' "$text_only_pair"
  expect 1 'a transcript without the phrase does not hold it' check_present 'A: Order it this week.' "$text_only_pair"
  expect 2 'an empty download is UNKNOWN before the delete' check_present '' "$text_only_pair"
  expect 1 'a transcript that still holds the phrase fails' check_absent \
    'B: Orange tractor ninety. A: the code word is blue harbor lantern.' "$text_only_trace" "$control_pair"
  expect 1 'a transcript that still holds one word of the phrase fails' check_absent \
    'B: the orange one. A: the code word is blue harbor lantern.' "$text_only_trace" "$control_pair"
  expect 0 'a transcript without the phrase and with the control phrase passes' check_absent \
    'A: Agreed. B: the code word is blue harbour lantern.' "$text_only_trace" "$control_pair"
  expect 2 'an empty download is UNKNOWN, not PASS' check_absent '' "$text_only_trace" "$control_pair"
  expect 2 'a read without the control phrase is UNKNOWN, not PASS' check_absent \
    'A: Agreed. Order it this week.' "$text_only_trace" "$control_pair"
  expect 0 'a summary without the phrase passes' check_absent 'The team named the project.' "$text_only_trace"
  expect 2 'a failed request is UNKNOWN, not PASS' answered 500 check_absent 'no trace here' "$text_only_trace"
  expect 1 'a sha256 that changed fails' check_hashes_unchanged \
    "$sha_before data/audio/rec.mp3" "$sha_after data/audio/rec.mp3"
  expect 0 'an unchanged sha256 passes' check_hashes_unchanged \
    "$sha_before data/audio/rec.mp3" "$sha_before data/audio/rec.mp3"
  expect 2 'no hash read is UNKNOWN' check_hashes_unchanged "$sha_before data/audio/rec.mp3" ''
  expect 2 'a missing hash is UNKNOWN' check_hashes_unchanged \
    'missing data/audio/rec.mp3' 'missing data/audio/rec.mp3'
  expect 1 'a file id answering 404 in text only fails' check_answers_200 404
  expect 0 'a file id answering 200 passes' check_answers_200 200
  expect 2 'a file id answering 403 is UNKNOWN' check_answers_200 403
  expect 2 'a file id with no answer is UNKNOWN' check_answers_200 ''
  expect 1 'changed Drive file ids fail' check_ids_unchanged \
    $'summary.md\tS1\ntranscript.md\tT1' $'summary.md\tS1\ntranscript.md\tT2'
  expect 0 'unchanged Drive file ids pass' check_ids_unchanged \
    $'summary.md\tS1\ntranscript.md\tT1' $'summary.md\tS1\ntranscript.md\tT1'
  expect 2 'an empty Drive file id list is UNKNOWN' check_ids_unchanged $'summary.md\tS1' ''
  expect 1 'a changed recording path fails' check_same_text data/audio/a.mp3 data/audio/b.mp3
  expect 0 'an unchanged recording path passes' check_same_text data/audio/a.mp3 data/audio/a.mp3
  expect 1 'a unit recording that differs from Drive fails' check_same_md5 "$planted_md5" fedcba9876543210fedcba9876543210
  expect 2 'a missing Drive checksum is UNKNOWN' check_same_md5 "$planted_md5" ''
  expect 1 'search that still finds the conversation fails' check_search_absent \
    "[{\"session_id\":\"$acceptance_session_id\"}]" "$acceptance_session_id"
  expect 0 'search that does not find the conversation passes' check_search_absent \
    '[{"session_id":"20260101100000"}]' "$acceptance_session_id"
  expect 2 'an empty body from a failed search is UNKNOWN, not absent' check_search_absent '' "$acceptance_session_id"
  expect 2 'an error object from search is UNKNOWN, not absent' check_search_absent '{"error":"search failed"}' "$acceptance_session_id"
  expect 2 'a full result page without the conversation is UNKNOWN' check_search_absent \
    "[$(IFS=,; printf '%s' "${fifty[*]}")]" "$acceptance_session_id"
  expect 0 'search that finds the conversation passes the control' check_search_finds \
    "[{\"session_id\":\"$acceptance_session_id\"}]" "$acceptance_session_id"
  expect 1 'search that does not find the conversation fails the control' check_search_finds '[]' "$acceptance_session_id"
  expect 1 'an earlier conversation with the phrase fails the precheck' check_search_empty '[{"session_id":"20260101100000"}]'
  expect 0 'no earlier conversation passes the precheck' check_search_empty '[]'
  expect 2 'a failed precheck search is UNKNOWN' check_search_empty ''

  report=$(printf '%s\n' \
    $'db\t/u/data/ghost-wispr.db' \
    $'file\tdb\t4096\t100\t/u/data/ghost-wispr.db' \
    $'count\tdb\ttext_only\t0\t/u/data/ghost-wispr.db' \
    $'count\tdb\tcontrol\t2\t/u/data/ghost-wispr.db' \
    $'file\twal\t4096\t100\t/u/data/ghost-wispr.db-wal' \
    $'count\twal\ttext_only\t0\t/u/data/ghost-wispr.db-wal' \
    $'count\twal\tcontrol\t0\t/u/data/ghost-wispr.db-wal' \
    $'file\tbackup\t4096\t90\t/u/data/ghost-wispr.db.pre-migrate.bak' \
    $'count\tbackup\ttext_only\t0\t/u/data/ghost-wispr.db.pre-migrate.bak' \
    $'count\tbackup\tcontrol\t1\t/u/data/ghost-wispr.db.pre-migrate.bak')
  expect 0 'a clean database scan with the control phrase passes' check_db_scan "$report" absent text_only control
  expect 1 'a log that still holds the phrase fails' check_db_scan \
    "${report/$'wal\ttext_only\t0'/$'wal\ttext_only\t3'}" absent text_only control
  expect 1 'a backup that still holds the phrase fails' check_db_scan \
    "${report/$'backup\ttext_only\t0'/$'backup\ttext_only\t1'}" absent text_only control
  expect 2 'a backup without the control phrase is UNKNOWN' check_db_scan \
    "${report/$'backup\tcontrol\t1'/$'backup\tcontrol\t0'}" absent text_only control
  expect 2 'a scan without the backup is UNKNOWN' check_db_scan \
    "$(grep -v 'pre-migrate' <<<"$report")" absent text_only control
  expect 2 'a scan grep could not read is UNKNOWN' check_db_scan \
    "${report/$'wal\ttext_only\t0'/$'wal\ttext_only\terror'}" absent text_only control
  expect 2 'a failed scan is UNKNOWN' check_db_scan $'error\tghost-wispr.service has no running process' absent text_only control
  expect 2 'an empty scan is UNKNOWN' check_db_scan '' absent text_only control
  expect 1 'a scan before the delete without the phrase fails the control' check_db_scan "$report" present text_only
  expect 0 'a scan before the delete with the phrase in the log and the backup passes the control' check_db_scan \
    "$(sed $'s/wal\ttext_only\t0/wal\ttext_only\t2/; s/backup\ttext_only\t0/backup\ttext_only\t1/' <<<"$report")" present text_only
  expect 0 'a newer backup after the restart passes' check_backup_retaken "$report" \
    "${report/$'backup\t4096\t90'/$'backup\t4096\t200'}"
  expect 2 'a backup the restart did not rewrite is UNKNOWN' check_backup_retaken "$report" "$report"
  expect 0 'a Drive summary that holds the current summary passes' check_doc_holds_summary \
    $'---\nid: "x"\n---\n\n# Robot naming\n\nThe team named the warehouse robot project and set its budget.' \
    'The team named the warehouse robot project and set its budget.'
  expect 2 'a stale Drive summary is UNKNOWN' check_doc_holds_summary \
    '# Robot naming The team chose Orange tractor ninety.' 'The team named the warehouse robot project and set its budget.'
  expect 2 'an empty Drive summary is UNKNOWN' check_doc_holds_summary '' 'The team named the warehouse robot project.'

  detail=$(jq -n --arg control 'B: the code word for the meeting room is blue harbor lantern.' '{session: {}, segments: [
    {id: 1, text: "A: Please tell me the name."},
    {id: 2, text: "B: Purple elephant 17."},
    {id: 3, text: "A: Which machine do we order to clear the yard?"},
    {id: 4, text: "B: Orange tractor ninety."},
    {id: 5, text: $control}]}' | jq '.segments |= map(. + {transcript_revision: 1})')
  expect 0 'one segment per phrase and the control phrase elsewhere is chosen' choose_segments "$detail"
  [[ "$text_only_segment_id" == 4 && "$audio_segment_id" == 2 && "$text_only_revision" == 1 && "$audio_revision" == 1 ]] \
    || fail "self-test: chose segments $text_only_segment_id/$text_only_revision and $audio_segment_id/$audio_revision, want 4/1 and 2/1"
  expect 2 'a live segment without a transcript revision stops the run' choose_segments \
    "$(jq '.segments[1].transcript_revision = 0' <<<"$detail")"
  expect 2 'a phrase word in a second segment stops the run' choose_segments \
    "$(jq '.segments[2].text = "A: Which orange machine do we order?"' <<<"$detail")"
  expect 2 'a phrase in two segments stops the run' choose_segments \
    "$(jq '.segments[0].text = "A: purple elephant seventeen, again"' <<<"$detail")"
  expect 2 'no control phrase stops the run' choose_segments "$(jq 'del(.segments[4])' <<<"$detail")"
  expect 2 'a conversation without the phrase stops the run' choose_segments "$(jq 'del(.segments[3])' <<<"$detail")"

  detail=$(jq -n --arg summary 'The team named the warehouse robot project and agreed to order a machine for the yard.' '{session: {
    id: "20261112100000", status: "ended", title: "Purple elephant seventeen naming", summary: $summary,
    summary_status: "completed", refinement_status: "completed", sync_state: "SYNCED", gdrive_folder_id: "F1",
    drive_replace_pending: false, audio_path: "data/audio/rec.mp3",
    canonical_transcript: "B: Purple elephant seventeen. A: Which machine do we order? B: the code word is blue harbor lantern.",
    refined_transcript: "B: Purple elephant seventeen. A: Which machine do we order? B: the code word is blue harbor lantern."},
    segments: [{id: 2, text: "B: Purple elephant seventeen."}, {id: 5, text: "B: the code word is blue harbor lantern."}]}')
  expect 0 'a settled session passes' check_session_settled "$detail"
  expect 2 'a session owing a Drive replacement is UNKNOWN' check_session_settled \
    "$(jq '.session.drive_replace_pending = true' <<<"$detail")"
  expect 2 'a session still syncing is UNKNOWN' check_session_settled "$(jq '.session.sync_state = "PENDING"' <<<"$detail")"
  expect 1 'a failed summary fails' check_session_settled "$(jq '.session.summary_status = "failed"' <<<"$detail")"
  expect 2 'a session that could not be read is UNKNOWN' check_session_settled ''

  listing=$(drive_listing transcript.md T1 summary.md S1 audio-rec.mp3 A1)
  expect 0 'a folder with its transcript, summary and recording passes' check_listing 200 "$listing"
  expect 1 'a folder without its summary fails' check_listing 200 "$(drive_listing transcript.md T1 audio-rec.mp3 A1)"
  expect 2 'an empty folder list is UNKNOWN' check_listing 200 '{"files":[]}'
  expect 2 'a folder list with more pages is UNKNOWN' check_listing 200 "$(jq '.nextPageToken = "next"' <<<"$listing")"
  expect 2 'a failed folder list is UNKNOWN' check_listing 500 ''

  # The whole text only evaluation after "Delete text", on planted evidence from the Pi.
  text_only_ids_before=$(drive_file_ids "$listing")
  text_only_hashes_before="$sha_before data/audio/rec.mp3"
  text_only_audio_path_before=data/audio/rec.mp3
  dir="$controls_dir/text-only-after"
  mkdir "$dir"
  plant "$dir" remote.ok 'done'
  plant "$dir" token.state ready
  plant "$dir" db.report "$report"
  plant "$dir" unit.0.sha256 "$sha_before"
  plant "$dir" folder.list.code 200
  plant "$dir" folder.list.body "$listing"
  for index in T1 S1 A1; do
    plant "$dir" "file.$index.get.code" 200
  done
  plant "$dir" file.T1.export.code 200
  plant "$dir" file.T1.export.body $'---\nid: "20261112100000"\n---\n\nB: Purple elephant seventeen.\nA: Which machine do we order?\nB: the code word is blue harbor lantern.\n'
  plant "$dir" file.S1.export.code 200
  plant "$dir" file.S1.export.body "# Purple elephant seventeen naming

$(session_field "$detail" summary)"
  local found="[{\"session_id\":\"$acceptance_session_id\"}]"
  expect 0 'planted evidence of a clean "Delete text" passes' evaluated evaluate_text_only_after "$dir" "$detail" '[]' "$found"
  variant="$controls_dir/text-only-variant"
  plant_variant() {
    rm -rf -- "$variant"
    cp -R -- "$dir" "$variant"
    plant "$variant" "$1" "$2"
  }
  plant_variant file.T1.export.body $'B: Orange tractor ninety.\nB: the code word is blue harbor lantern.\n'
  expect 1 'a Drive transcript that still holds the phrase fails' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant folder.list.body "$(drive_listing transcript.md T9 summary.md S1 audio-rec.mp3 A1)"
  expect 1 'a Drive transcript with a new id fails' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant unit.0.sha256 "$sha_after"
  expect 1 'a recording whose sha256 changed fails' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant file.S1.get.code 404
  expect 1 'a Drive file id answering 404 fails' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant db.report "${report/$'backup\ttext_only\t0'/$'backup\ttext_only\t1'}"
  expect 1 'a pre-migration backup that still holds the phrase fails' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant file.T1.export.body ''
  expect 2 'an empty Drive transcript download is UNKNOWN' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant file.S1.export.body '# Purple elephant seventeen naming. An older summary.'
  expect 2 'a stale Drive summary is UNKNOWN' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  plant_variant token.state failed
  expect 2 'a Drive token that could not be minted is UNKNOWN' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  rm -f -- "$variant/remote.ok"
  expect 2 'evidence the Pi did not return is UNKNOWN' evaluated evaluate_text_only_after "$variant" "$detail" '[]' "$found"
  expect 2 'a failed UI search is UNKNOWN' evaluated evaluate_text_only_after "$dir" "$detail" '' "$found"
  expect 1 'a transcript that still holds the phrase fails' evaluated evaluate_text_only_after "$dir" \
    "$(jq '.session.canonical_transcript += " B: Orange tractor 90."' <<<"$detail")" '[]' "$found"

  # The database scan itself, run as the Pi runs it, over planted binary files.
  db_root="$controls_dir/unit"
  mkdir -p "$db_root/data/audio"
  database="$db_root/data/ghost-wispr.db"
  printf 'SQLite format 3\000\001\002B: Orange tractor ninety.\000\377blue harbor lantern\000' >"$database"
  printf 'WAL\000\377B: Purple elephant seventeen.\000' >"$database-wal"
  printf 'SQLite format 3\000\003orange tractor 90\000blue harbor lantern\000' >"$database.pre-migrate.bak"
  printf 'ID3\000planted recording' >"$db_root/data/audio/rec.mp3"
  scan_dir=$(mktemp -d "$controls_dir/scan.XXXXXX")
  bash -s <<<"$(remote_script $'db_scan\tdb\t-\t-\nhash\tunit.0\tdata/audio/rec.mp3\t-' "$db_root" "$database")" \
    | tar -x -C "$scan_dir" -f - || fail 'self-test: the database scan did not run'
  expect 0 'the scan finds a planted phrase in a binary database and its backup' check_db_scan \
    "$(result "$scan_dir" db.report)" present text_only
  expect 1 'the scan reports a planted phrase as still there' check_db_scan \
    "$(result "$scan_dir" db.report)" absent text_only control
  expect 1 'the scan finds a planted phrase in the write-ahead log' check_db_scan \
    "$(result "$scan_dir" db.report)" absent audio control
  [[ "$(result "$scan_dir" unit.0.sha256)" == "$(sha256sum <"$db_root/data/audio/rec.mp3" | cut -d ' ' -f 1)" ]] \
    || fail 'self-test: the recording hash is not the sha256 of the planted recording'
  printf 'SQLite format 3\000\001\002B: the yard machine.\000\377blue harbor lantern\000' >"$database"
  printf 'SQLite format 3\000\003the yard machine\000blue harbor lantern\000' >"$database.pre-migrate.bak"
  scan_dir=$(mktemp -d "$controls_dir/scan.XXXXXX")
  bash -s <<<"$(remote_script $'db_scan\tdb\t-\t-' "$db_root" "$database")" | tar -x -C "$scan_dir" -f - \
    || fail 'self-test: the database scan did not run'
  expect 0 'the scan passes files the phrase is gone from' check_db_scan "$(result "$scan_dir" db.report)" absent text_only control
  rm -- "$database.pre-migrate.bak"
  scan_dir=$(mktemp -d "$controls_dir/scan.XXXXXX")
  bash -s <<<"$(remote_script $'db_scan\tdb\t-\t-' "$db_root" "$database")" | tar -x -C "$scan_dir" -f - \
    || fail 'self-test: the database scan did not run'
  expect 2 'the scan without a pre-migration backup is UNKNOWN' check_db_scan "$(result "$scan_dir" db.report)" absent text_only control
  printf 'A11 text only self-test RAN: planted failure rejected\n'
  printf 'A11 text only self-test RAN: the database scan sees a planted phrase in binary data in the database, its log and its backup\n'

  # A11: each pure check against a planted failure and a planted pass.
  expect 1 'an old file id answering 200 after the audio delete fails' check_answers_404 200
  expect 0 'an old file id answering 404 passes' check_answers_404 404
  expect 2 'an old file id answering 403 is UNKNOWN, not gone' check_answers_404 403
  expect 2 'an old file id with no answer is UNKNOWN, not gone' check_answers_404 000
  expect 1 'a revisions.list with two revisions fails' check_one_revision 200 '{"revisions":[{"id":"1"},{"id":"2"}]}'
  expect 0 'a revisions.list with one revision passes' check_one_revision 200 '{"revisions":[{"id":"1"}]}'
  expect 2 'a revisions.list with no revision is UNKNOWN' check_one_revision 200 '{"revisions":[]}'
  expect 2 'a failed revisions.list is UNKNOWN' check_one_revision 500 ''
  expect 2 'a revisions.list without a list is UNKNOWN' check_one_revision 200 '{}'
  expect 0 'a revisions.list before the delete succeeds' check_revisions_listed 200 '{"revisions":[{"id":"1"},{"id":"2"}]}'
  expect 1 'a revisions.list before the delete that answers 404 fails' check_revisions_listed 404 ''
  expect 1 'a file that kept its old id fails' check_ids_replaced \
    $'summary.md\tS1\ntranscript.md\tT1' $'summary.md\tS2\ntranscript.md\tT1'
  expect 1 'a folder that lost a file fails' check_ids_replaced $'summary.md\tS1\ntranscript.md\tT1' $'transcript.md\tT2'
  expect 0 'new ids for every file pass' check_ids_replaced \
    $'summary.md\tS1\ntranscript.md\tT1' $'summary.md\tS2\ntranscript.md\tT2'
  expect 2 'an empty new id list is UNKNOWN' check_ids_replaced $'summary.md\tS1' ''
  expect 1 'a recording transcription that still holds the phrase fails' answered 200 check_absent \
    "$(deepgram_transcript "$(deepgram_body 'Purple elephant 17. The code word is blue harbor lantern.')")" "$audio_trace" "$control_pair"
  expect 2 'a recording transcription Deepgram refused is UNKNOWN' answered 401 check_absent '' "$audio_trace" "$control_pair"
  expect 2 'an empty recording transcription is UNKNOWN' answered 200 check_absent \
    "$(deepgram_transcript "$(deepgram_body '')")" "$audio_trace" "$control_pair"
  expect 1 'a folder name that still holds the slug of the phrase fails' check_absent \
    2026-11-12-purple-elephant-seventeen-naming "$audio_trace"
  expect 0 'a folder name without the phrase passes' check_absent 2026-11-12-warehouse-robot-project-naming "$audio_trace"

  # The whole A11 evaluation after "Delete text and audio", on planted evidence from the Pi.
  audio_ids_before=$(drive_file_ids "$listing")
  detail=$(jq -n --arg summary 'The team discussed the warehouse robot project, its budget and a machine for the yard.' '{session: {
    id: "20261112100000", status: "ended", title: "Warehouse robot project", summary: $summary,
    summary_status: "completed", refinement_status: "completed", sync_state: "SYNCED", gdrive_folder_id: "F1",
    drive_replace_pending: false, audio_path: "data/audio/rec.mp3",
    canonical_transcript: "A: Please tell me the name. B: the code word is blue harbor lantern.",
    refined_transcript: "A: Please tell me the name. B: the code word is blue harbor lantern."},
    segments: [{id: 1, text: "A: Please tell me the name."}, {id: 5, text: "B: the code word is blue harbor lantern."}]}')
  listing=$(drive_listing transcript.md T2 summary.md S2 audio-rec.mp3 A2)
  dir="$controls_dir/audio-after"
  mkdir "$dir"
  plant "$dir" remote.ok 'done'
  plant "$dir" token.state ready
  plant "$dir" unit.0.md5 "$planted_md5"
  plant "$dir" unit.0.dg_code 200
  plant "$dir" unit.0.dg_body "$(deepgram_body 'Please tell me the name. Orange tractor 90. The code word is blue harbor lantern.')"
  plant "$dir" folder.get.code 200
  plant "$dir" folder.get.body '{"id":"F1","name":"2026-11-12-warehouse-robot-project"}'
  plant "$dir" folder.list.code 200
  plant "$dir" folder.list.body "$listing"
  for index in T2 S2 A2; do
    plant "$dir" "file.$index.get.code" 200
    plant "$dir" "file.$index.revisions.code" 200
    plant "$dir" "file.$index.revisions.body" '{"revisions":[{"id":"1"}]}'
  done
  for index in T1 S1 A1; do
    plant "$dir" "old.$index.get.code" 404
    plant "$dir" "old.$index.revisions.code" 404
  done
  plant "$dir" file.T2.export.code 200
  plant "$dir" file.T2.export.body $'A: Please tell me the name.\nB: the code word is blue harbor lantern.\n'
  plant "$dir" file.S2.export.code 200
  plant "$dir" file.S2.export.body "# Warehouse robot project

$(session_field "$detail" summary)"
  plant "$dir" file.A2.media.code 200
  plant "$dir" file.A2.media.md5 "$planted_md5"
  plant "$dir" file.A2.media.dg_code 200
  plant "$dir" file.A2.media.dg_body "$(deepgram_body 'Please tell me the name. The code word is blue harbor lantern.')"
  expect 0 'planted evidence of a clean "Delete text and audio" passes' evaluated evaluate_audio_after "$dir" "$detail" '[]' "$found"
  variant="$controls_dir/audio-variant"
  plant_variant old.A1.get.code 200
  expect 1 'an old recording id answering 200 fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant old.S1.revisions.code 403
  expect 2 'an old revisions.list answering 403 is UNKNOWN' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant file.T2.revisions.body '{"revisions":[{"id":"1"},{"id":"2"}]}'
  expect 1 'a new transcript with two revisions fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant folder.list.body "$(drive_listing transcript.md T1 summary.md S2 audio-rec.mp3 A2)"
  expect 1 'a transcript that kept its old id fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant file.A2.media.dg_body "$(deepgram_body 'Purple elephant 17. The code word is blue harbor lantern.')"
  expect 1 'a new Drive recording that still says the phrase fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant unit.0.dg_body "$(deepgram_body 'Purple elephant seventeen. The code word is blue harbor lantern.')"
  expect 1 'a unit recording that still says the phrase fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant file.A2.media.md5 fedcba9876543210fedcba9876543210
  expect 1 'a new Drive recording that is not the unit recording fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant folder.get.body '{"id":"F1","name":"2026-11-12-purple-elephant-seventeen-naming"}'
  expect 1 'a folder name that still holds the phrase fails' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  plant_variant file.A2.media.dg_body "$(deepgram_body 'Please tell me the name.')"
  expect 2 'a new Drive recording transcription without the control phrase is UNKNOWN' evaluated evaluate_audio_after "$variant" "$detail" '[]' "$found"
  expect 1 'a title that still holds the phrase fails' evaluated evaluate_audio_after "$dir" \
    "$(jq '.session.title = "Purple Elephant Seventeen naming"' <<<"$detail")" '[]' "$found"
  expect 1 'search that still finds the conversation fails' evaluated evaluate_audio_after "$dir" "$detail" "$found" "$found"
  expect 1 'a "Delete text" phrase that came back fails' evaluated evaluate_audio_after "$dir" \
    "$(jq '.session.refined_transcript += " B: Orange tractor ninety."' <<<"$detail")" '[]' "$found"
  printf 'A11 self-test RAN: planted failure rejected\n'

  # The Pi-side requests themselves, run as the Pi runs them, against a fake curl that answers
  # as the token service, IAM, Drive and Deepgram would.
  local fake_bin="$controls_dir/bin" fake_log="$controls_dir/fake-curl.log" sent_log="$controls_dir/fake-curl.requests"
  local evidence sts_request field
  mkdir -p "$fake_bin"
  fake_curl >"$fake_bin/curl"
  fake_openssl >"$fake_bin/openssl"
  chmod +x "$fake_bin/curl" "$fake_bin/openssl"
  printf 'planted device certificate' >"$db_root/device.crt"
  printf 'planted key' >"$db_root/key.pem"
  jq -n --arg root "$db_root" '{cert_configs: {workload: {cert_path: "\($root)/device.crt", key_path: "\($root)/key.pem"}}}' \
    >"$db_root/certificate_config.json"
  jq -n --arg config "$db_root/certificate_config.json" '{type: "external_account",
    audience: "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/ghost-wispr/providers/ghost-wispr",
    subject_token_type: "urn:ietf:params:oauth:token-type:mtls", token_url: "https://sts.test/v1/token",
    service_account_impersonation_url: "https://iam.test/v1/projects/-/serviceAccounts/ghost-wispr@test.iam.gserviceaccount.com:generateAccessToken",
    credential_source: {certificate: {certificate_config_location: $config}}}' >"$db_root/google-credentials.json"
  printf 'GHOST_WISPR_DEEPGRAM_API_KEY=deepgram-not-a-secret-3\n' >"$db_root/.env"
  : >"$fake_log"
  : >"$sent_log"
  evidence=$(mktemp -d "$controls_dir/evidence.XXXXXX")
  FAKE_LOG=$fake_log FAKE_REQUESTS=$sent_log FAKE_MEDIA="$db_root/data/audio/rec.mp3" FAKE_LISTING=$listing \
    FAKE_SUMMARY="$(result "$dir" file.S2.export.body)" \
    FAKE_TRANSCRIPT="$(deepgram_body 'Please tell me the name. The code word is blue harbor lantern.')" \
    PATH="$fake_bin:$PATH" bash -s <<<"$(remote_script "$(audio_requests "$detail" after)" "$db_root")" \
    | tar -x -C "$evidence" -f - || fail 'self-test: the Pi-side requests did not run against the fake curl'
  [[ ! -s "$fake_log" ]] || fail "self-test: the fake curl saw $(one_line "$(<"$fake_log")")"
  if grep -rlq -- secret "$evidence"; then
    fail "self-test: a token or key is in the evidence: $(one_line "$(grep -rl -- secret "$evidence")")"
  fi
  [[ "$(grep -c '^https://sts.test/' "$sent_log")" == 1 ]] || fail 'self-test: the Drive token was not minted exactly once'
  sts_request=$(grep '^https://sts.test/' "$sent_log")
  for field in 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
    'requested_token_type=urn:ietf:params:oauth:token-type:access_token' \
    'subject_token_type=urn:ietf:params:oauth:token-type:mtls' \
    'scope=https://www.googleapis.com/auth/cloud-platform' \
    "subject_token=[\"$(base64 -w 0 <"$db_root/device.crt")\"]"; do
    [[ "$sts_request" == *$'\t'"$field"* ]] || fail "self-test: the token exchange did not send $field"
  done
  grep -Fq '"scope":["https://www.googleapis.com/auth/drive.readonly"]' <<<"$(grep '^https://iam.test/' "$sent_log")" \
    || fail 'self-test: the impersonation did not ask for the read-only Drive scope'
  expect 0 'the evidence the Pi-side requests return passes A11' evaluated evaluate_audio_after "$evidence" "$detail" '[]' "$found"
  printf 'A11 self-test RAN: the Pi-side token, Drive and Deepgram requests, run against a fake curl, give the evidence the checks read, and no token or key reaches an argument or the evidence\n'

  expect 1 'a 400 whose unquoted text names only one mode fails' check_mode_required 400 \
    '{"error":"mode must be \"text_and_audio\" (Delete text and audio)"}'
  expect 0 "the API's 400 for a deletion without a mode passes" check_mode_required 400 \
    '{"error":"mode must be \"text\" (Delete text: the words only; the recording and the Drive files stay) or \"text_and_audio\" (Delete text and audio: the words, their audio, and the Drive files replaced)"}'
  expect 1 'a 400 that names one mode fails' check_mode_required 400 '{"error":"mode must be \"text_and_audio\""}'
  expect 1 'a 400 without a JSON error fails' check_mode_required 400 'bad request'
  expect 1 'a deletion without a mode that answers 204 fails' check_mode_required 204 ''
  expect 1 'a build that reads the session before the mode fails' check_mode_required 404 '{"error":"get session: not found"}'
  expect 2 'no answer is UNKNOWN' check_mode_required 000 ''
  printf 'Mode guard self-test RAN: planted failure rejected\n'

  marker="$controls_dir/deletes"
  (
    : >"$marker"
    status() { printf '{}'; }
    running_config() { printf '{"gdrive":{"sync_enabled":true,"folder_id":"F0"}}'; }
    delete_segment() {
      printf '%s\n' "$3" >>"$marker"
      printf '{"error":"get session: not found"}' >"$4"
      printf 404
    }
    precheck() { printf 'recorded\n' >>"$marker"; return 2; }
    record_conversation() { printf 'recorded\n' >>"$marker"; return 2; }
    prepare_conversation >/dev/null 2>&1
    [[ $? == 1 ]] || fail 'self-test: a build without the modes did not fail the setup'
    grep -Fxq recorded "$marker" && fail 'self-test: a build without the modes went on to record a conversation'
    [[ "$(grep -c '' "$marker")" == 2 ]] || fail 'self-test: the mode guard did not send both probes'
  ) || exit 1
  printf 'Mode guard self-test RAN: a build without the modes stops the run before recording\n'

  (
    : >"$marker"
    conversation_ready=true
    text_only_segment_id=4
    text_only_revision=1
    audio_segment_id=2
    audio_revision=1
    delete_segment() { printf '%s\n' "$3" >>"$marker"; printf 204; }
    retry_refinement() { :; }
    sleep() { :; }
    text_only_round() {
      round_reset
      round_lines=('FAILED: planted: the phrase is not there')
      round_failed=1
    }
    run_a11_text_only >/dev/null 2>&1
    [[ $? == 2 ]] || fail 'self-test: A11 text only did not stop as UNKNOWN after a failed control'
    [[ ! -s "$marker" ]] && exit 0
    fail 'self-test: A11 text only deleted after a failed control'
  ) || exit 1
  printf 'A11 text only self-test RAN: a failed control stops the row before "Delete text"\n'
  (
    : >"$marker"
    conversation_ready=true
    text_only_segment_id=4
    text_only_revision=1
    delete_segment() { printf '%s %s\n' "$2" "$3" >>"$marker"; printf 204; }
    text_only_round() { round_reset; round_lines=('PASS: planted'); }
    retry_refinement() { printf 'retry\n' >>"$marker"; }
    sleep() { :; }
    run_a11_text_only >/dev/null 2>&1 || fail 'self-test: A11 text only did not pass on planted passing rounds'
    [[ "$(<"$marker")" == $'4 mode=text&revision=1\nretry' ]] || fail "self-test: A11 text only sent $(one_line "$(<"$marker")")"
  ) || exit 1
  printf 'A11 text only self-test RAN: "Delete text" sends mode=text and its revision on its segment, then retries refinement\n'

  (
    unit_url=https://unit.test
    marker="$controls_dir/fast-refinement"
    : >"$marker"
    retry_http_status=202
    curl() {
      [[ "$retry_http_status" == 202 ]] || return 1
      [[ " $* " == *" --fail "* ]] || return 1
      printf '{"triggered":true}'
    }
    session_detail() {
      printf 'read\n' >>"$marker"
      if [[ "$(wc -l <"$marker")" == 1 ]]; then
        printf '{"session":{"refinement_status":"completed","refined_revision":1}}'
      else
        printf '{"session":{"refinement_status":"completed","refined_revision":2}}'
      fi
    }
    retry_refinement >/dev/null || fail 'self-test: a fast refinement did not accept the planted 202'
    [[ "$(wc -l <"$marker")" == 2 ]] || fail 'self-test: a fast refinement did not read the newer revision'
  ) || exit 1
  printf 'A11 text only self-test RAN: a fast refinement accepts a 202 response and completes\n'
  (
    unit_url=https://unit.test
    marker="$controls_dir/slow-refinement"
    : >"$marker"
    retry_http_status=202
    curl() {
      [[ "$retry_http_status" == 202 ]] || return 1
      [[ " $* " == *" --fail "* ]] || return 1
      printf '{"triggered":true}'
    }
    session_detail() {
      printf 'read\n' >>"$marker"
      case "$(wc -l <"$marker")" in
        1) printf '{"session":{"refinement_status":"completed","refined_revision":1}}' ;;
        2) printf '{"session":{"refinement_status":"running","refined_revision":1}}' ;;
        *) printf '{"session":{"refinement_status":"completed","refined_revision":2}}' ;;
      esac
    }
    sleep() { :; }
    retry_refinement >/dev/null || fail 'self-test: a slow refinement did not complete after the planted running state'
    [[ "$(wc -l <"$marker")" == 3 ]] || fail 'self-test: a slow refinement did not wait for completion'
  ) || exit 1
  printf 'A11 text only self-test RAN: a slow refinement accepts a 202 response, stays running, then completes\n'
  (
    unit_url=https://unit.test
    marker="$controls_dir/stale-refinement"
    : >"$marker"
    retry_http_status=202
    curl() {
      [[ "$retry_http_status" == 202 ]] || return 1
      [[ " $* " == *" --fail "* ]] || return 1
      printf '{"triggered":true}'
    }
    session_detail() {
      printf 'read\n' >>"$marker"
      case "$(wc -l <"$marker")" in
        1|2) printf '{"session":{"refinement_status":"completed","refined_revision":1}}' ;;
        *) printf '{"session":{"refinement_status":"failed","refined_revision":1}}' ;;
      esac
    }
    sleep() { :; }
    if retry_refinement >/dev/null 2>&1; then
      fail 'self-test: a stale completed revision passed as the retry'
    fi
    [[ "$(wc -l <"$marker")" == 3 ]] || fail 'self-test: a stale completed revision was not polled again'
  ) || exit 1
  printf 'A11 text only self-test RAN: retry refinement waits for a newer revision\n'
  (
    : >"$marker"
    conversation_ready=true
    audio_segment_id=2
    audio_revision=1
    delete_segment() { printf '%s\n' "$3" >>"$marker"; printf 204; }
    choose_audio_segment() { :; }
    session_detail() { printf '{}'; }
    sleep() { :; }
    audio_round() {
      round_reset
      round_lines=('UNKNOWN: planted: nothing was read')
      round_unknown=1
    }
    run_a11 >/dev/null 2>&1
    [[ $? == 2 ]] || fail 'self-test: A11 did not stop as UNKNOWN after a blind control'
    [[ ! -s "$marker" ]] && exit 0
    fail 'self-test: A11 deleted after a blind control'
  ) || exit 1
  printf 'A11 self-test RAN: a failed control stops the row before "Delete text and audio"\n'
  (
    : >"$marker"
    conversation_ready=true
    audio_segment_id=2
    audio_revision=1
    delete_segment() { printf '%s %s\n' "$2" "$3" >>"$marker"; printf 204; }
    choose_audio_segment() { :; }
    session_detail() { printf '{}'; }
    audio_round() { round_reset; round_lines=('PASS: planted'); }
    run_a11 >/dev/null 2>&1 || fail 'self-test: A11 did not pass on planted passing rounds'
    [[ "$(<"$marker")" == '2 mode=text_and_audio&revision=1' ]] || fail "self-test: A11 sent $(one_line "$(<"$marker")")"
  ) || exit 1
  printf 'A11 self-test RAN: "Delete text and audio" sends mode=text_and_audio and its revision on its segment\n'
)

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      [[ $# -ge 2 ]] || fail '--host needs a value'
      host=$2
      shift 2
      ;;
    --session)
      [[ $# -ge 2 ]] || fail '--session needs a conversation id'
      [[ "$2" =~ ^[A-Za-z0-9_-]+$ ]] || fail "not a conversation id: $2"
      given_session=$2
      shift 2
      ;;
    --self-test)
      self_test=true
      shift
      ;;
    --help)
      usage
      exit 0
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

if [[ "$self_test" == true ]]; then
  for command in base64 bash cat chmod cp cut find grep head jq md5sum mktemp sed seq sha256sum sort stat tar tr wc; do
    command -v "$command" >/dev/null || fail "self-test requires $command"
  done
  run_self_test
  exit
fi

for command in curl cut grep jq mktemp sed sort ssh tar tr wc; do
  command -v "$command" >/dev/null || fail "missing required command: $command"
done
readonly remote="ghost-wispr@$host"
readonly unit_url="https://$host"
scratch_dir=$(mktemp -d)
acceptance_session_id=
conversation_ready=false
conversation_problem='the setup did not finish'
text_only_segment_id=
text_only_revision=
audio_segment_id=
audio_revision=
text_only_ids_before=
text_only_hashes_before=
text_only_audio_path_before=
audio_ids_before=
overall=0

cleanup() {
  local result=$?
  trap - EXIT
  rm -rf -- "$scratch_dir"
  exit "$result"
}
trap cleanup EXIT

prepare_conversation
setup_result=$?
if (( setup_result != 0 )); then
  if (( setup_result == 1 )); then
    printf 'Setup FAILED: %s\n' "$conversation_problem" >&2
  else
    printf 'Setup UNKNOWN: %s\n' "$conversation_problem"
  fi
  overall=$(worse "$overall" "$setup_result")
fi

run_row 'A11 text only' run_a11_text_only
run_row A11 run_a11

case "$overall" in
  0) printf 'Slice 3 driver verdict: PASS\n' ;;
  1)
    printf 'Slice 3 driver verdict: FAIL\n' >&2
    exit 1
    ;;
  *)
    printf 'Slice 3 driver verdict: UNKNOWN; a check could not see what it looks for. Fix what its UNKNOWN line names and run again with a new conversation.\n'
    exit 2
    ;;
esac
