#!/usr/bin/env bash
# Exercise the slice 2 acceptance checks from an operator machine on the tailnet.
set -uo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
# The office Pi's deployment settings (PI_HOST, PI_GCP_PROJECT, PI_SERVICE_ACCOUNT, PI_DRIVE_OWNER,
# PI_NATS_SERVER); GHOST_WISPR_PI_ENV names another file, as the tests do.
pi_env=${GHOST_WISPR_PI_ENV:-$script_dir/files/pi.env}
# shellcheck source=files/pi.env
source "$pi_env" || { printf 'cannot read the Pi deployment settings in %s\n' "$pi_env" >&2; exit 1; }
host=${GHOST_WISPR_HOST:-$PI_HOST}
self_test=false
drive_members_file=
readers_group=
cert_test=false
readonly service_account_key_pattern='"type"[[:space:]]*:[[:space:]]*"service_account"|"private_key_id"'
readonly service_account_key_body_pattern='"private_key"[[:space:]]*:[[:space:]]*"-----BEGIN'
readonly credential_path=/opt/ghost-wispr/google-credentials.json
readonly expected_certificate_config=/opt/ghost-wispr/certificate_config.json
readonly deployed_config=/opt/ghost-wispr/ghost-wispr.yaml
readonly service_account=$PI_SERVICE_ACCOUNT
readonly drive_owner=$PI_DRIVE_OWNER
readonly cert_test_port=8081
readonly summarization_model=gemini/gemini-4-carbon
readonly summary_provider_model=gemini-4-carbon
readonly embedding_model=gemini-embedding-2
readonly batch_refinement_model=nova-3
readonly tts_model=gemini-3.8-flash-tts
readonly tts_voice=Kore
readonly model_not_reported='not reported by provider'
expected_config_lines=(
  "deepgram_model: $batch_refinement_model"
  'batch_transcription.provider: deepgram'
  "batch_transcription.model: $batch_refinement_model"
  "summarization.model: $summarization_model"
  "embedding_model: gemini/$embedding_model"
  'genai_backend: vertex'
  "gcp_project: $PI_GCP_PROJECT"
  'gcp_location: global'
  'tts_provider: gemini'
  "tts_model: $tts_model"
  "tts_voice: $tts_voice"
)
# Environment variables that override the YAML settings above (internal/config/config.go applyEnvOverrides).
config_override_variables=(
  GHOST_WISPR_DEEPGRAM_MODEL
  GHOST_WISPR_BATCH_TRANSCRIPTION_PROVIDER
  GHOST_WISPR_BATCH_TRANSCRIPTION_MODEL
  GHOST_WISPR_SUMMARIZATION_MODEL
  GHOST_WISPR_EMBEDDING_MODEL
  GHOST_WISPR_EMBEDDING_BACKEND
  GHOST_WISPR_GENAI_BACKEND
  GHOST_WISPR_GCP_PROJECT
  GHOST_WISPR_GCP_LOCATION
  GHOST_WISPR_GDRIVE_FOLDER_ID
  GHOST_WISPR_GOOGLE_CREDENTIALS_FILE
  GHOST_WISPR_TTS_PROVIDER
  GHOST_WISPR_TTS_MODEL
  GHOST_WISPR_TTS_VOICE
)

usage() {
  cat <<EOF
Usage: deploy/pi/accept-slice2.sh [--host HOST] [--drive-members FILE --readers-group EMAIL] [--cert-test] [--self-test]

Run from a tailnet member after provision.sh --models <shared-drive-id>.

--drive-members FILE  A10: the shared drive's members from Task 2.4, one
                      "email|role" line each, with Drive API role names
                      (organizer, fileOrganizer, writer, commenter, reader).
--readers-group EMAIL A10: the approved readers group from Task 2.4.
--cert-test           Cert warning: a test instance is running on the Pi at
                      127.0.0.1:$cert_test_port with a certificate that expires in 10 days.
EOF
}

fail() {
  printf 'slice 2 acceptance: %s\n' "$*" >&2
  exit 1
}

# key_sweep_script prints a bash script that plants a service-account control
# file under CONTROL_PARENT, runs the A9 sweep over ROOT, and prints one
# "kind<TAB>path" line per match. The pattern travels in a pipe so that sudo
# never logs it: a logged pattern would put "private_key_id" in the journal.
key_sweep_script() {
  local root=$1 control_parent=$2 elevate=$3
  printf 'root=%q\ncontrol_parent=%q\nelevate=%q\nkey_pattern=%q\nkey_body_pattern=%q\n' \
    "$root" "$control_parent" "$elevate" "$service_account_key_pattern" "$service_account_key_body_pattern"
  cat <<'EOF'
set -u
as_root() {
  if [ "$elevate" = sudo ]; then
    sudo env LC_ALL=C "$@"
  else
    env LC_ALL=C "$@"
  fi
}
control_dir=$(mktemp -d "$control_parent/ghost-wispr-key-sweep.XXXXXX") || exit 2
trap 'rm -rf -- "$control_dir"' EXIT
control_file=$control_dir/planted-service-account.json
printf '%s\n' '{"type":"service_account"}' >"$control_file" || exit 2
status=0
matches=
while IFS= read -r -d '' entry; do
  if [ "$root" = / ]; then
    case "$entry" in /dev|/proc|/run|/sys) continue ;; esac
  else
    case "$entry" in "$root"/dev|"$root"/proc|"$root"/run|"$root"/sys) continue ;; esac
  fi
  entry_matches=$(printf '%s\n' "$key_pattern" | as_root grep -rlE -f /dev/stdin -- "$entry") || entry_status=$?
  if [ "${entry_status:-0}" -gt 1 ]; then
    status=$entry_status
  fi
  matches+="$entry_matches"$'\n'
  unset entry_status
done < <(find "$root" -mindepth 1 -maxdepth 1 -print0)
if [ "$status" -gt 1 ]; then
  printf 'grep-error\t%s\n' "$status"
fi
control_found=false
while IFS= read -r file; do
  [ -n "$file" ] || continue
  if [ "$file" = "$control_file" ]; then
    control_found=true
    continue
  fi
  if printf '%s\n' "$key_pattern" | as_root grep -qIE -f /dev/stdin -- "$file"; then
    kind=text
  else
    body_status=0
    printf '%s\n' "$key_body_pattern" | as_root grep -qE -f /dev/stdin -- "$file" || body_status=$?
    case "$body_status" in
      0) kind=key-body ;;
      1) kind=binary-field-name ;;
      *) kind=unreadable ;;
    esac
  fi
  printf '%s\t%s\n' "$kind" "$file"
done <<<"$matches"
if [ "$control_found" = true ]; then
  printf 'control-found\t%s\n' "$control_file"
else
  printf 'control-missing\t%s\n' "$control_file"
fi
EOF
}

# key_sweep_passes accepts a sweep report only when the planted control was
# found and every other match is binary data that names a key field without
# holding a key body, such as Go struct tags in the ghost-wispr binary.
key_sweep_passes() {
  local report=$1 kind path control_found=false passed=true
  while IFS=$'\t' read -r kind path; do
    case "$kind" in
      control-found) control_found=true ;;
      binary-field-name) printf 'A9 note: %s names a key field in binary data and holds no key body\n' "$path" ;;
      '') ;;
      *)
        printf 'A9 finding: %s %s\n' "$kind" "$path" >&2
        passed=false
        ;;
    esac
  done <<<"$report"
  [[ "$control_found" == true && "$passed" == true ]]
}

assert_external_account_credential() {
  jq -e --arg certificate_config "$expected_certificate_config" '
    .type == "external_account" and
    .credential_source.certificate.certificate_config_location == $certificate_config
  ' >/dev/null <<<"$1"
}

assert_drive_config() {
  jq -e '
    .gdrive.sync_enabled == true and
    (.gdrive.folder_id | type == "string" and test("^[A-Za-z0-9_-]+$")) and
    .gdrive.has_credentials == true and
    .gc.enabled == true and .gc.max_age_days == 0 and .gc.max_audio_size_mb == 20000
  ' >/dev/null <<<"$1"
}

assert_no_warnings() {
  jq -e 'has("warnings") and (.warnings | type == "array" and length == 0)' >/dev/null <<<"$1"
}

# flatten_yaml prints top-level lines as they are and indented lines as
# "parent.key: value", so a nested model is checked under its own parent.
flatten_yaml() {
  awk '
    /^[^[:space:]#][^:]*:/ { parent = $0; sub(/:.*/, "", parent); print; next }
    /^[[:space:]]+[^[:space:]#][^:]*:/ { line = $0; sub(/^[[:space:]]+/, "", line); print parent "." line }
  '
}

assert_expected_config() {
  local flattened line
  flattened=$(flatten_yaml <<<"$1")
  for line in "${expected_config_lines[@]}"; do
    if ! grep -Fqx -- "$line" <<<"$flattened"; then
      printf 'configuration lacks: %s\n' "$line" >&2
      return 1
    fi
  done
}

# The model, provider or Google setting of each role that /api/config reports under "models", as
# the running process resolved its configuration, and the value the plan names.
expected_running_models=(
  "live_transcription=$batch_refinement_model"
  'batch_transcription_provider=deepgram'
  "batch_transcription=$batch_refinement_model"
  "summarization=$summarization_model"
  "embedding=gemini/$embedding_model"
  'genai_backend=vertex'
  "gcp_project=$PI_GCP_PROJECT"
  'gcp_location=global'
  'tts_provider=gemini'
  "tts=$tts_model"
  "tts_voice=$tts_voice"
)

# assert_running_models CONFIG: the running process (its /api/config response, CONFIG) runs the
# plan's model for every role. The deployed YAML says what the next start runs; a YAML edited
# after the service started does not change what runs now.
assert_running_models() {
  local config=$1 entry role want got
  for entry in "${expected_running_models[@]}"; do
    role=${entry%%=*}
    want=${entry#*=}
    got=$(jq -r --arg role "$role" '.models[$role] // ""' <<<"$config" 2>/dev/null)
    if [[ "$got" != "$want" ]]; then
      printf 'the running process reports %s %q, want %q\n' "$role" "$got" "$want" >&2
      return 1
    fi
  done
}

assert_no_config_overrides() {
  local names=$1 variable
  if ! grep -Fqx GHOST_WISPR_ADDR <<<"$names"; then
    printf 'the service environment was not read: GHOST_WISPR_ADDR is missing\n' >&2
    return 1
  fi
  for variable in "${config_override_variables[@]}"; do
    if grep -Fqx -- "$variable" <<<"$names"; then
      printf '%s overrides the YAML setting\n' "$variable" >&2
      return 1
    fi
  done
}

assert_embedding_location() {
  [[ $1 == GHOST_WISPR_EMBEDDING_LOCATION=global ]]
}

# assert_provider_model ROLE ACTUAL EXPECTED: the model the provider reported for ROLE is the
# plan's (EXPECTED). The spec's A8 asks that the unit run the plan's models, and the plan reads
# each one "from the provider's response or the app's log". A provider that reports none (Vertex
# AI's embeddings never do) answers "not reported by provider"; that passes on the model the
# running process reports for the role, which run_a8 checks before any provider call
# (assert_running_models).
assert_provider_model() {
  local role=$1 actual=$2 expected=$3
  if [[ "$actual" == "$model_not_reported" ]]; then
    printf 'A8 RAN: the provider did not report the %s model; the running process runs %s.\n' "$role" "$expected"
    return 0
  fi
  if [[ -z "$actual" || "$actual" != "$expected" ]]; then
    printf '%s provider model mismatch: got %q, want %q\n' "$role" "$actual" "$expected" >&2
    return 1
  fi
}

lowercase_trimmed() {
  printf '%s' "$1" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]'
}

assert_drive_members() {
  local members=$1 readers_group=$2 principal role organizers='' readers=0
  readers_group=$(lowercase_trimmed "$readers_group")
  [[ -n "$readers_group" ]] || return 1
  while IFS='|' read -r principal role; do
    principal=$(lowercase_trimmed "$principal")
    role=$(printf '%s' "$role" | tr -d '[:space:]')
    if [[ -z "$principal" && -z "$role" ]] || [[ "$principal" == \#* ]]; then
      continue
    fi
    case "$role" in
      organizer)
        if [[ "$principal" != "$service_account" && "$principal" != "$drive_owner" ]]; then
          printf 'unapproved organizer: %s\n' "$principal" >&2
          return 1
        fi
        organizers+="$principal"$'\n'
        ;;
      reader)
        if [[ "$principal" != "$readers_group" ]]; then
          printf 'unapproved reader: %s\n' "$principal" >&2
          return 1
        fi
        readers=$((readers + 1))
        ;;
      *)
        printf '%s has role %s; only %s and %s may write, both as organizer, and %s must be the only reader\n' \
          "$principal" "${role:-<none>}" "$service_account" "$drive_owner" "$readers_group" >&2
        return 1
        ;;
    esac
  done <<<"$members"
  if [[ $(printf '%s' "$organizers" | LC_ALL=C sort -u) != $(printf '%s\n' "$drive_owner" "$service_account" | LC_ALL=C sort -u) ]]; then
    printf 'the organizers must be exactly %s and %s\n' "$service_account" "$drive_owner" >&2
    return 1
  fi
  if (( readers != 1 )); then
    printf '%s must be the only reader\n' "$readers_group" >&2
    return 1
  fi
}

assert_certificate_warning() {
  jq -e '[.warnings[]? | select(type == "string" and startswith("Google credential certificate expires on "))] | length > 0' \
    >/dev/null <<<"$1"
}

# assert_cert_test_instance STATE passes only for a test instance that cannot open the Jabra: all
# four of its user ids and group ids are ghost-wispr's, so it is not root; no capabilities in its
# permitted, effective or ambient set; no supplementary groups (the Pi's sound devices are
# root:audio 0660 and its input nodes root:input 0660); no sound or input device that ghost-wispr
# with no groups can open, which an ACL from a console login would allow;
# GHOST_WISPR_JABRA_MUTE_BUTTON=false; and no sound or input device held. STATE is
# cert_test_instance_state's "user:", "uid:", "gid:", "caps:", "groups:", "reach:", "jabra:" and
# "devices:" lines; a STATE without all of them is refused, so an unread instance never passes.
assert_cert_test_instance() {
  local state=$1 line user_uid user_gid groups reach jabra devices id
  local -a uids gids caps
  for line in user uid gid caps groups reach jabra devices; do
    grep -q "^$line:" <<<"$state" || {
      printf 'the test instance could not be read (no %s): %s\n' "$line" "$state" >&2
      return 1
    }
  done
  read -r user_uid user_gid <<<"$(sed -n 's/^user://p' <<<"$state")"
  read -r -a uids <<<"$(sed -n 's/^uid://p' <<<"$state")"
  read -r -a gids <<<"$(sed -n 's/^gid://p' <<<"$state")"
  read -r -a caps <<<"$(sed -n 's/^caps://p' <<<"$state")"
  groups=$(sed -n 's/^groups:[[:space:]]*//p' <<<"$state")
  reach=$(sed -n 's/^reach:[[:space:]]*//p' <<<"$state")
  jabra=$(sed -n 's/^jabra://p' <<<"$state")
  devices=$(sed -n 's/^devices:[[:space:]]*//p' <<<"$state")
  [[ $user_uid =~ ^[0-9]+$ && $user_gid =~ ^[0-9]+$ && ${#uids[@]} -eq 4 && ${#gids[@]} -eq 4 ]] || {
    printf 'the test instance ids could not be read: user %s %s, uid %s, gid %s\n' "$user_uid" "$user_gid" "${uids[*]}" "${gids[*]}" >&2
    return 1
  }
  for id in "${uids[@]}"; do
    [[ $id == "$user_uid" ]] || {
      printf 'the test instance runs with user ids %s, want all %s (ghost-wispr)\n' "${uids[*]}" "$user_uid" >&2
      return 1
    }
  done
  for id in "${gids[@]}"; do
    [[ $id == "$user_gid" ]] || {
      printf 'the test instance runs with group ids %s, want all %s (ghost-wispr)\n' "${gids[*]}" "$user_gid" >&2
      return 1
    }
  done
  [[ ${#caps[@]} -eq 3 && ${caps[0]} =~ ^0+$ && ${caps[1]} =~ ^0+$ && ${caps[2]} =~ ^0+$ ]] || {
    printf 'the test instance holds capabilities (permitted, effective, ambient) %s, want none\n' "${caps[*]}" >&2
    return 1
  }
  [[ -z "$groups" ]] || {
    printf 'the test instance holds supplementary groups %s, so it can open the Jabra\n' "$groups" >&2
    return 1
  }
  [[ -z "$reach" ]] || {
    printf 'ghost-wispr with no groups can open %s, so the test instance can open the Jabra\n' "$reach" >&2
    return 1
  }
  [[ "$jabra" == false ]] || {
    printf 'the test instance runs with GHOST_WISPR_JABRA_MUTE_BUTTON=%s, want false\n' "${jabra:-<unset>}" >&2
    return 1
  }
  [[ -z "$devices" ]] || {
    printf 'the test instance holds %s\n' "$devices" >&2
    return 1
  }
}

run_key_sweep_locally() {
  bash -s <<<"$(key_sweep_script "$1" "$1" '')"
}


while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      [[ $# -ge 2 ]] || fail '--host needs a value'
      host=$2
      shift 2
      ;;
    --drive-members)
      [[ $# -ge 2 ]] || fail '--drive-members needs a file'
      drive_members_file=$2
      shift 2
      ;;
    --readers-group)
      [[ $# -ge 2 ]] || fail '--readers-group needs an email address'
      readers_group=$2
      shift 2
      ;;
    --cert-test)
      cert_test=true
      shift
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
  for command in awk bash find grep jq mktemp sort tr; do
    command -v "$command" >/dev/null || fail "self-test requires $command"
  done
else
  for command in awk curl find grep jq mktemp nats ssh sort tr; do
    command -v "$command" >/dev/null || fail "missing required command: $command"
  done
  if [[ -n "$drive_members_file" ]]; then
    [[ -r "$drive_members_file" ]] || fail "cannot read --drive-members file: $drive_members_file"
  fi
  if [[ -n "$drive_members_file" || -n "$readers_group" ]]; then
    [[ -n "$drive_members_file" && -n "$readers_group" ]] || fail '--drive-members and --readers-group must be used together'
  fi
  readonly remote="ghost-wispr@$host"
  readonly unit_url="https://$host"
  scratch_dir=$(mktemp -d)
  nats_pid=
  nats_log="$scratch_dir/nats.log"
  acceptance_session_id=
  row_failures=0

  cleanup() {
    local result=$?
    trap - EXIT
    if [[ -n "$nats_pid" ]]; then
      kill "$nats_pid" >/dev/null 2>&1 || true
      wait "$nats_pid" 2>/dev/null || true
    fi
    rm -rf -- "$scratch_dir"
    exit "$result"
  }
  trap cleanup EXIT
fi

remote_run() {
  ssh -o BatchMode=yes "$remote" "$@"
}

status() {
  curl --fail --silent --show-error "$unit_url/api/status"
}

running_config() {
  curl --fail --silent --show-error "$unit_url/api/config"
}

session_detail() {
  curl --fail --silent --show-error "$unit_url/api/sessions/$1"
}

post_json() {
  local path=$1 body=$2
  curl --fail --silent --show-error --request POST \
    --header 'Content-Type: application/json' --data "$body" "$unit_url$path"
}

semantic_search() {
  curl --fail --silent --show-error --get \
    --data-urlencode "q=Slice 2 acceptance ${acceptance_session_id}" --data-urlencode 'limit=1' \
    "$unit_url/api/search/semantic"
}

speak() {
  local headers=$1
  curl --fail --silent --show-error --dump-header "$headers" \
    --request POST --header 'Content-Type: application/json' \
    --data '{"text":"Slice two model acceptance speech test.","provider":"gemini"}' \
    "$unit_url/api/tts/speak"
}

response_header() {
  local headers=$1 name=$2
  name=$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')
  awk -v name="$name:" 'tolower($1) == name { sub(/^[^:]*:[[:space:]]*/, ""); sub(/\r$/, ""); print; exit }' "$headers"
}

service_environment_names() {
  # shellcheck disable=SC2016 # The remote shell expands the service runtime path.
  remote_run 'pid=$(XDG_RUNTIME_DIR="/run/user/$(id -u)" systemctl --user show --property=MainPID --value ghost-wispr.service) && [ "$pid" -gt 0 ] && tr "\000" "\n" <"/proc/$pid/environ" | sed "s/=.*//"'
}

service_embedding_location() {
  # shellcheck disable=SC2016 # The remote shell expands the service runtime path.
  remote_run 'pid=$(XDG_RUNTIME_DIR="/run/user/$(id -u)" systemctl --user show --property=MainPID --value ghost-wispr.service) && [ "$pid" -gt 0 ] && tr "\000" "\n" <"/proc/$pid/environ" | grep "^GHOST_WISPR_EMBEDDING_LOCATION="'
}

start_nats_subscriber() {
  [[ -z "$nats_pid" ]] || return 0
  nats sub 'notifications.ghostwispr.>' --server "$PI_NATS_SERVER" >"$nats_log" 2>&1 &
  nats_pid=$!
  sleep 1
  if ! kill -0 "$nats_pid" 2>/dev/null; then
    cat "$nats_log" >&2
    return 1
  fi
}

wait_for_summary_ready() {
  local session_id=$1
  for _ in {1..120}; do
    if grep -Fq -- "notifications.ghostwispr.${session_id}.summary.ready" "$nats_log"; then
      return 0
    fi
    sleep 5
  done
  printf 'A6: no summary.ready notification for real session %s\n' "$session_id" >&2
  return 1
}

wait_for_summary() {
  local session_id=$1 before_version=$2 detail model version
  for _ in {1..120}; do
    if detail=$(session_detail "$session_id"); then
      if jq -e '.session.summary_status == "completed" and (.session.summary | type == "string" and length > 0)' \
        >/dev/null <<<"$detail"; then
        version=$(jq -r '.session.summary_version // 0' <<<"$detail")
        if (( version > before_version )); then
          model=$(jq -r '.session.summary_model // ""' <<<"$detail")
          assert_provider_model summary "$model" "$summary_provider_model"
          return
        fi
      fi
      if jq -e '.session.summary_status == "failed"' >/dev/null <<<"$detail"; then
        printf 'A8: summary failed: %s\n' "$(jq -r '.session.summary_error // .session.error_message // "no error detail"' <<<"$detail")" >&2
        return 1
      fi
    fi
    sleep 5
  done
  printf 'A8: summary did not complete for session %s\n' "$session_id" >&2
  return 1
}

wait_for_drive_sync() {
  local session_id=$1 detail
  for _ in {1..120}; do
    if detail=$(session_detail "$session_id"); then
      if jq -e '(.session.sync_state | ascii_downcase) == "synced" and (.session.gdrive_folder_id | type == "string" and length > 0)' \
        >/dev/null <<<"$detail"; then
        return 0
      fi
    fi
    sleep 5
  done
  printf 'A6: session %s did not sync to a Drive folder\n' "$session_id" >&2
  return 1
}

session_search_word() {
  jq -r '.session.canonical_transcript // ""' <<<"$1" \
    | tr '[:upper:]' '[:lower:]' \
    | tr -cs '[:alpha:]' '\n' \
    | awk 'length >= 6 && $0 !~ /^(acceptance|about|after|before|conversation|could|drive|ghost|google|recording|search|session|should|summary|through|their|there|these|topic|which|would)$/ { print; exit }'
}

wait_for_tts() {
  local request_id=$1 speech_status model voice
  for _ in {1..60}; do
    if speech_status=$(curl --fail --silent --show-error "$unit_url/api/tts/status/$request_id"); then
      if jq -e '.status == "completed" and (.bytes_written | type == "number" and . > 0)' >/dev/null <<<"$speech_status"; then
        model=$(jq -r '.model // ""' <<<"$speech_status")
        voice=$(jq -r '.voice // ""' <<<"$speech_status")
        assert_provider_model speech "$model" "$tts_model" || return 1
        [[ "$voice" == "$tts_voice" ]] || {
          printf 'speech provider voice mismatch: got %q, want %q\n' "$voice" "$tts_voice" >&2
          return 1
        }
        return 0
      fi
      if jq -e '.status == "failed"' >/dev/null <<<"$speech_status"; then
        printf 'A8: speech failed: %s\n' "$(jq -r '.error // "no error detail"' <<<"$speech_status")" >&2
        return 1
      fi
    fi
    sleep 1
  done
  printf 'A8: speech did not complete for request %s\n' "$request_id" >&2
  return 1
}

wait_for_refinement() {
  local session_id=$1 detail saw_running=false model
  for _ in {1..120}; do
    if detail=$(session_detail "$session_id"); then
      case "$(jq -r '.session.refinement_status' <<<"$detail")" in
        running) saw_running=true ;;
        completed)
          if [[ "$saw_running" == true ]]; then
            model=$(jq -r '.session.refinement_model // ""' <<<"$detail")
            assert_provider_model refinement "$model" "$batch_refinement_model"
            return
          fi
          ;;
        failed|no_audio)
          printf 'A8: refinement is %s for session %s\n' \
            "$(jq -r '.session.refinement_status' <<<"$detail")" "$session_id" >&2
          return 1
          ;;
      esac
    fi
    sleep 5
  done
  printf 'A8: refinement did not complete for session %s\n' "$session_id" >&2
  return 1
}

run_a9() {
  local key_report credential
  printf 'A9 RAN: plant a service-account key in /tmp and require the full key sweep to find it and nothing else.\n'
  key_report=$(remote_run 'bash -s' <<<"$(key_sweep_script / /tmp sudo)") || {
    printf 'A9 FAILED: the key sweep did not run on the Pi\n' >&2
    return 1
  }
  key_sweep_passes "$key_report" || {
    printf 'A9 FAILED: the key sweep missed its planted control or found key material\n' >&2
    return 1
  }
  credential=$(remote_run "cat -- $credential_path") || {
    printf 'A9 FAILED: could not read %s\n' "$credential_path" >&2
    return 1
  }
  assert_external_account_credential "$credential" || {
    printf 'A9 FAILED: google-credentials.json is not an external-account certificate credential\n' >&2
    return 1
  }
  printf 'A9 RAN: no service-account key and the configured credential is external-account X.509.\n'
}

run_a6() {
  local current_config start_response stop_response detail search_word search_response
  printf 'A6 PRECHECK: read the running Drive and GC configuration.\n'
  current_config=$(running_config) || {
    printf 'A6 FAILED: could not read /api/config\n' >&2
    return 1
  }
  assert_drive_config "$current_config" || {
    printf 'A6 FAILED: Drive sync, its folder, its credentials or GC are not as provision.sh --models sets them\n' >&2
    return 1
  }
  start_nats_subscriber || {
    printf 'A6 FAILED: could not start the NATS subscriber\n' >&2
    return 1
  }
  start_response=$(post_json '/api/sessions/start' '{"title_hint":"Slice 2 acceptance"}') || {
    printf 'A6 FAILED: could not start a real acceptance session\n' >&2
    return 1
  }
  acceptance_session_id=$(jq -er '.session_id | select(type == "string" and test("^[A-Za-z0-9_-]+$"))' <<<"$start_response") || {
    printf 'A6 FAILED: /api/sessions/start did not return a real session ID\n' >&2
    return 1
  }
  printf 'A6 NEEDS-PERSON: speak or play a two-minute conversation, including a distinctive topic, then press Enter to end real session %s. The NATS subscriber is running.\n' "$acceptance_session_id"
  read -r _
  stop_response=$(post_json "/api/sessions/${acceptance_session_id}/stop" '{}') || {
    printf 'A6 FAILED: could not stop real session %s\n' "$acceptance_session_id" >&2
    return 1
  }
  jq -e '.status == "stopped"' >/dev/null <<<"$stop_response" || {
    printf 'A6 FAILED: /api/sessions/%s/stop did not confirm the stop\n' "$acceptance_session_id" >&2
    return 1
  }
  wait_for_summary_ready "$acceptance_session_id" || return 1
  wait_for_drive_sync "$acceptance_session_id" || return 1
  detail=$(session_detail "$acceptance_session_id") || {
    printf 'A6: could not read session %s for transcript search\n' "$acceptance_session_id" >&2
    return 1
  }
  search_word=$(session_search_word "$detail")
  [[ -n "$search_word" ]] || {
    printf 'A6: session %s has no distinctive transcript word to search\n' "$acceptance_session_id" >&2
    return 1
  }
  search_response=$(curl --fail --silent --show-error --get --data-urlencode "q=$search_word" "$unit_url/api/search") || {
    printf 'A6: keyword search did not answer\n' >&2
    return 1
  }
  jq -e --arg id "$acceptance_session_id" 'any(.[]; .session_id == $id)' >/dev/null <<<"$search_response" || {
    printf 'A6: keyword search for %q did not return session %s\n' "$search_word" "$acceptance_session_id" >&2
    return 1
  }
  printf 'A6 RAN: real session %s produced summary.ready, synced to Drive, and was found by keyword search. Verify its dated Drive folder contains transcript, summary, and audio-*.mp3.\n' "$acceptance_session_id"
}

run_a8() {
  local current_status current_config deployed_yaml environment_names embedding_location before_summary_version
  local summary_response semantic_response semantic_model speech_response speech_headers speech_model speech_voice speech_request_id refinement_response
  printf 'A8 PRECHECK: read status, running configuration, deployed YAML, and service environment before provider calls.\n'
  current_status=$(status) || {
    printf 'A8 FAILED: could not read /api/status\n' >&2
    return 1
  }
  assert_no_warnings "$current_status" || {
    printf 'A8 FAILED: /api/status reports a warning\n' >&2
    return 1
  }
  current_config=$(running_config) || {
    printf 'A8 FAILED: could not read /api/config\n' >&2
    return 1
  }
  deployed_yaml=$(remote_run "cat -- $deployed_config") || {
    printf 'A8 FAILED: could not read %s\n' "$deployed_config" >&2
    return 1
  }
  assert_expected_config "$deployed_yaml" || {
    printf 'A8 FAILED: the Pi configuration does not name the accepted models\n' >&2
    return 1
  }
  assert_running_models "$current_config" || {
    printf 'A8 FAILED: the running process does not run the accepted model for every role\n' >&2
    return 1
  }
  environment_names=$(service_environment_names) || {
    printf 'A8 FAILED: could not read the ghost-wispr.service environment\n' >&2
    return 1
  }
  assert_no_config_overrides "$environment_names" || {
    printf 'A8 FAILED: the service environment overrides a configured model or Google setting\n' >&2
    return 1
  }
  embedding_location=$(service_embedding_location) || {
    printf 'A8 FAILED: could not read the embedding location from the service environment\n' >&2
    return 1
  }
  assert_embedding_location "$embedding_location" || {
    printf 'A8 FAILED: the embedding location is not global\n' >&2
    return 1
  }
  [[ -n "$acceptance_session_id" ]] || {
    printf 'A8 FAILED: A6 did not create a real session for route acceptance\n' >&2
    return 1
  }

  before_summary_version=$(session_detail "$acceptance_session_id" | jq -er '.session.summary_version | numbers') || {
    printf 'A8 FAILED: could not read the summary version before resummarization\n' >&2
    return 1
  }

  summary_response=$(post_json "/api/sessions/${acceptance_session_id}/resummarize" '{}') || {
    printf 'A8 FAILED: summary route did not accept real session %s\n' "$acceptance_session_id" >&2
    return 1
  }
  [[ -z "$summary_response" ]] || {
    printf 'A8 FAILED: summary route returned an unexpected body\n' >&2
    return 1
  }
  wait_for_summary "$acceptance_session_id" "$before_summary_version" || return 1

  semantic_response=$(semantic_search) || {
    printf 'A8 FAILED: semantic-search route did not answer\n' >&2
    return 1
  }
  jq -e '.results | type == "array" and length > 0' >/dev/null <<<"$semantic_response" || {
    printf 'A8 FAILED: semantic-search response contained no embedding result\n' >&2
    return 1
  }
  semantic_model=$(jq -r '.model // ""' <<<"$semantic_response")
  assert_provider_model search "$semantic_model" "$embedding_model" || return 1

  speech_headers="$scratch_dir/a8-tts.headers"
  speech_response=$(speak "$speech_headers") || {
    printf 'A8 FAILED: speech route did not accept the Gemini TTS request\n' >&2
    return 1
  }
  speech_model=$(response_header "$speech_headers" X-Ghost-Wispr-TTS-Model)
  speech_voice=$(response_header "$speech_headers" X-Ghost-Wispr-TTS-Voice)
  assert_provider_model speech "$speech_model" "$tts_model" || return 1
  [[ "$speech_voice" == "$tts_voice" ]] || {
    printf 'speech provider voice mismatch: got %q, want %q\n' "$speech_voice" "$tts_voice" >&2
    return 1
  }
  speech_request_id=$(jq -er '.id | select(type == "string" and length > 0)' <<<"$speech_response") || {
    printf 'A8 FAILED: speech route did not return a request ID\n' >&2
    return 1
  }
  wait_for_tts "$speech_request_id" || return 1

  refinement_response=$(post_json "/api/sessions/${acceptance_session_id}/retry-refinement" '{}') || {
    printf 'A8 FAILED: batch-refinement route did not accept real session %s\n' "$acceptance_session_id" >&2
    return 1
  }
  jq -e '.triggered == true' >/dev/null <<<"$refinement_response" || {
    printf 'A8 FAILED: batch-refinement route did not confirm its request\n' >&2
    return 1
  }
  wait_for_refinement "$acceptance_session_id" || return 1
  printf 'A8 RAN: summary, semantic-search, speech, and batch-refinement routes completed for real session %s with the planned models: %s, %s, %s, and %s/%s.\n' \
    "$acceptance_session_id" "$batch_refinement_model" "$summarization_model" "$embedding_model" "$tts_model" "$tts_voice"
}

run_a10() {
  if [[ -n "$drive_members_file" ]]; then
    printf 'A10 RAN: check the Task 2.4 member list.\n'
    assert_drive_members "$(<"$drive_members_file")" "$readers_group" || {
      printf 'A10 FAILED: the shared drive members are not as approved\n' >&2
      return 1
    }
    printf 'A10 RAN: only %s and %s are organizers, and %s is the only reader.\n' "$service_account" "$drive_owner" "$readers_group"
    return 0
  fi
  printf 'A10 NEEDS-PERSON: rerun with --drive-members FILE and --readers-group EMAIL from Task 2.4; as a readers-group member who is not a Manager, read a folder and confirm upload fails.\n'
  return 2
}

# cert_test_instance_state prints ghost-wispr's ids, and the user and group ids, capabilities,
# supplementary groups, Jabra button setting and sound and input devices of the process that
# listens on 127.0.0.1:$cert_test_port, for assert_cert_test_instance, and the sound and input
# devices that ghost-wispr with no groups can open ("reach:"; access(2) counts ACLs). The process
# changed its user, so only root reads its /proc entries. Any read that fails, or no listener,
# fails the whole call instead of printing an empty line.
cert_test_instance_state() {
  remote_run bash -s -- "$cert_test_port" <<'EOF'
set -euo pipefail
pid=$(sudo -n ss -ltnpH "sport = :$1" | sed -n 's/.*pid=\([0-9][0-9]*\).*/\1/p' | sed -n 1p)
[[ -n $pid ]] || exit 3
user_ids="$(id -u ghost-wispr) $(id -g ghost-wispr)"
status=$(sudo -n cat "/proc/$pid/status")
field() {
  local line
  line=$(grep "^$1:" <<<"$status")
  printf '%s' "${line#"$1":}"
}
uids=$(field Uid)
gids=$(field Gid)
groups=$(field Groups)
caps="$(field CapPrm) $(field CapEff) $(field CapAmb)"
jabra=$(sudo -n cat "/proc/$pid/environ" | tr '\000' '\n' | sed -n 's/^GHOST_WISPR_JABRA_MUTE_BUTTON=//p')
devices=$(sudo -n find "/proc/$pid/fd" \( -lname '/dev/snd/*' -o -lname '/dev/input/*' \) -printf '%l ')
# shellcheck disable=SC2016 # The inner shell expands these.
reach=$(sudo -n setpriv --reuid=ghost-wispr --regid=ghost-wispr --clear-groups sh -c '
  for f in /dev/snd/* /dev/input/*; do
    [ -c "$f" ] || continue
    if [ -r "$f" ] || [ -w "$f" ]; then printf "%s " "$f"; fi
  done')
printf 'user:%s\n' "$user_ids"
printf 'uid:%s\n' "$uids"
printf 'gid:%s\n' "$gids"
printf 'caps:%s\n' "$caps"
printf 'groups:%s\n' "$groups"
printf 'reach:%s\n' "$reach"
printf 'jabra:%s\n' "$jabra"
printf 'devices:%s\n' "$devices"
EOF
}

run_certificate_warning() {
  local cert_status instance_state
  if [[ "$cert_test" != true ]]; then
    printf 'Cert-warning NEEDS-PERSON: start a test instance on 127.0.0.1:%s as docs/pi-setup.md, "Accept slice 2", says: a copied config with separate db_path and audio_dir; Drive sync, GC, Envoy, speaker, light, power button, and Jabra mute button off; its copied credential config pointing at a certificate expiring in 10 days; seeded paused; started with sudo setpriv --reuid=ghost-wispr --regid=ghost-wispr --clear-groups and GHOST_WISPR_JABRA_MUTE_BUTTON=false, so it cannot open the Jabra; then rerun with --cert-test.\n' "$cert_test_port"
    return 2
  fi
  instance_state=$(cert_test_instance_state) || {
    printf 'Cert-warning FAILED: no test instance listens on 127.0.0.1:%s\n' "$cert_test_port" >&2
    return 1
  }
  assert_cert_test_instance "$instance_state" || {
    printf 'Cert-warning FAILED: the test instance could open the Jabra; stop it and start it as docs/pi-setup.md, "Accept slice 2", says\n' >&2
    return 1
  }
  printf 'Cert-warning RAN: the test instance runs as ghost-wispr with no capabilities and no supplementary groups, with the Jabra button off, and holds no sound or input device.\n'
  printf 'Cert-warning RAN: read /api/status from the test instance on 127.0.0.1:%s.\n' "$cert_test_port"
  cert_status=$(remote_run "curl --fail --silent --show-error http://127.0.0.1:$cert_test_port/api/status") || {
    printf 'Cert-warning FAILED: no test instance answered on 127.0.0.1:%s\n' "$cert_test_port" >&2
    return 1
  }
  assert_certificate_warning "$cert_status" || {
    printf 'Cert-warning FAILED: the test instance does not warn about certificate expiry\n' >&2
    return 1
  }
  printf 'Cert-warning RAN: the test instance warns that its certificate expires.\n'
}

# run_row keeps the row's own status: 0 PASS, 2 NEEDS-PERSON, anything else FAIL.
run_row() {
  local row=$1 result=0
  shift
  "$@" || result=$?
  case $result in
    0) printf '%s verdict: PASS\n' "$row" ;;
    2) printf '%s verdict: NEEDS-PERSON\n' "$row" ;;
    *)
      printf '%s verdict: FAIL\n' "$row" >&2
      row_failures=$((row_failures + 1))
      ;;
  esac
}

run_self_test() (
  local controls_dir key_root nested_dev_key report good_config marshaled_config swapped_config
  controls_dir=$(mktemp -d)
  trap 'rm -rf -- "$controls_dir"' EXIT

  key_root="$controls_dir/key-root"
  mkdir -p "$key_root/nested/dev"
  printf 'benign\n' >"$key_root/notes.txt"
  printf 'ELF\000json:"private_key_id"\n' >"$key_root/binary-with-field-name"
  report=$(run_key_sweep_locally "$key_root") || fail 'A9 self-test could not run the key sweep'
  grep -Fqx "binary-field-name"$'\t'"$key_root/binary-with-field-name" <<<"$report" \
    || fail 'A9 self-test sweep did not report the binary file that names a key field'
  key_sweep_passes "$report" >/dev/null || fail 'A9 self-test rejected a tree whose only match is a binary field name'
  nested_dev_key="$key_root/nested/dev/planted-key.json"
  printf '{"type": "service_account", "private_key_id": "planted"}\n' >"$nested_dev_key"
  report=$(run_key_sweep_locally "$key_root") || fail 'A9 self-test could not run the key sweep'
  if key_sweep_passes "$report" >/dev/null 2>&1; then
    fail 'A9 self-test accepted a service-account key below a nested dev directory'
  fi
  rm -- "$nested_dev_key"
  {
    printf 'PK\000{"type": "service_account", "private_key_id": "planted", "private_key": "--'
    printf '%s\n' '---BEGIN PRIVATE KEY-----"}'
  } >"$key_root/archive.bin"
  report=$(run_key_sweep_locally "$key_root") || fail 'A9 self-test could not run the key sweep'
  if key_sweep_passes "$report" >/dev/null 2>&1; then
    fail 'A9 self-test accepted a key body inside binary data'
  fi
  if key_sweep_passes '' >/dev/null 2>&1; then
    fail 'A9 self-test accepted a sweep that did not find its planted control'
  fi
  if key_sweep_passes $'grep-error\t2\ncontrol-found\t/tmp/control' >/dev/null 2>&1; then
    fail 'A9 self-test accepted a sweep that grep could not finish'
  fi
  printf '%s\n' '{"type":"service_account","private_key_id":"planted"}' >"$controls_dir/service-account.json"
  if assert_external_account_credential "$(<"$controls_dir/service-account.json")"; then
    fail 'A9 self-test accepted a planted service-account credential'
  fi
  assert_external_account_credential '{"type":"external_account","credential_source":{"certificate":{"certificate_config_location":"/opt/ghost-wispr/certificate_config.json"}}}' \
    || fail 'A9 self-test rejected a valid external-account credential'
  printf 'A9 self-test RAN: planted failure rejected\n'

  if assert_drive_config '{"gdrive":{"folder_id":"0AExample","has_credentials":true,"sync_enabled":false},"gc":{"enabled":true,"max_age_days":0,"max_audio_size_mb":20000}}'; then
    fail 'A6 self-test accepted a configuration with Drive sync disabled'
  fi
  if assert_drive_config '{"gdrive":{"folder_id":"","has_credentials":true,"sync_enabled":true},"gc":{"enabled":true,"max_age_days":0,"max_audio_size_mb":20000}}'; then
    fail 'A6 self-test accepted a configuration without a Drive folder'
  fi
  assert_drive_config '{"gdrive":{"folder_id":"0AExample","has_credentials":true,"sync_enabled":true},"gc":{"enabled":true,"max_age_days":0,"max_audio_size_mb":20000}}' \
    || fail 'A6 self-test rejected the provisioned Drive configuration'
  printf 'A6 self-test RAN: planted failure rejected\n'

  if assert_no_warnings '{"warnings":["Vertex unavailable"]}'; then
    fail 'A8 self-test accepted an application warning'
  fi
  if assert_no_warnings '{"capture_state":"capturing"}'; then
    fail 'A8 self-test accepted a status without a warnings field'
  fi
  if assert_no_warnings '{"warnings":null}'; then
    fail 'A8 self-test accepted warnings that are not an array'
  fi
  assert_no_warnings '{"warnings":[]}' || fail 'A8 self-test rejected a warning-free status'
  good_config=$(cat <<EOF
deepgram_model: nova-3
batch_transcription:
  provider: deepgram
  model: nova-3
summarization:
  model: gemini/gemini-4-carbon
embedding_model: gemini/gemini-embedding-2
genai_backend: vertex
gcp_project: $PI_GCP_PROJECT
gcp_location: global
tts_provider: gemini
tts_model: gemini-3.8-flash-tts
tts_voice: Kore
EOF
)
  assert_expected_config "$good_config" || fail 'A8 self-test rejected the accepted model configuration'
  marshaled_config=${good_config//$'\n  '/$'\n    '}
  assert_expected_config "$marshaled_config" || fail 'A8 self-test rejected yaml.v3 indentation'
  if assert_expected_config "${good_config/deepgram_model: nova-3/deepgram_model: nova-2}" 2>/dev/null; then
    fail 'A8 self-test accepted a superseded transcription model'
  fi
  swapped_config=${good_config/  model: nova-3/  model: swapped}
  swapped_config=${swapped_config/  model: gemini\/gemini-4-carbon/  model: nova-3}
  swapped_config=${swapped_config/  model: swapped/  model: gemini/gemini-4-carbon}
  if assert_expected_config "$swapped_config" 2>/dev/null; then
    fail 'A8 self-test accepted swapped batch and summarization models'
  fi
  local good_running_config
  good_running_config=$(jq -cn --arg project "$PI_GCP_PROJECT" '{"summarization":{"model":"gemini/gemini-4-carbon"},"models":{"live_transcription":"nova-3","batch_transcription_provider":"deepgram","batch_transcription":"nova-3","summarization":"gemini/gemini-4-carbon","embedding":"gemini/gemini-embedding-2","genai_backend":"vertex","gcp_project":$project,"gcp_location":"global","tts_provider":"gemini","tts":"gemini-3.8-flash-tts","tts_voice":"Kore"}}')
  assert_running_models "$good_running_config" || fail 'A8 self-test rejected the running models of the plan'
  for drift in '.models.summarization = "gemini/gemini-2.5-flash"' '.models.embedding = "gemini/gemini-embedding-001"' \
    '.models.live_transcription = "nova-2"' '.models.batch_transcription = "whisper-1"' '.models.tts_voice = "Charon"' \
    '.models.gcp_location = "us-central1"' 'del(.models.tts)' 'del(.models)'; do
    if assert_running_models "$(jq -c "$drift" <<<"$good_running_config")" 2>/dev/null; then
      fail "A8 self-test accepted a running process with $drift"
    fi
  done
  if assert_no_config_overrides $'GHOST_WISPR_ADDR\nGHOST_WISPR_TTS_VOICE' 2>/dev/null; then
    fail 'A8 self-test accepted an environment override of the TTS voice'
  fi
  if assert_no_config_overrides $'GHOST_WISPR_ADDR\nGHOST_WISPR_GOOGLE_CREDENTIALS_FILE' 2>/dev/null; then
    fail 'A8 self-test accepted a Google credential override'
  fi
  if assert_no_config_overrides '' 2>/dev/null; then
    fail 'A8 self-test accepted an unread service environment'
  fi
  assert_no_config_overrides $'GHOST_WISPR_ADDR\nGHOST_WISPR_EMBEDDING_LOCATION' \
    || fail 'A8 self-test rejected an environment without overrides'
  if assert_embedding_location 'GHOST_WISPR_EMBEDDING_LOCATION=us' 2>/dev/null; then
    fail 'A8 self-test accepted an embedding location outside global'
  fi
  assert_embedding_location 'GHOST_WISPR_EMBEDDING_LOCATION=global' \
    || fail 'A8 self-test rejected the required embedding location'
  if assert_provider_model summary configured-summary-model "$summary_provider_model" 2>/dev/null; then
    fail 'A8 self-test accepted a provider model mismatch for summary'
  fi
  if assert_provider_model search configured-embedding-model "$embedding_model" 2>/dev/null; then
    fail 'A8 self-test accepted a provider model mismatch for search'
  fi
  if assert_provider_model speech configured-tts-model "$tts_model" 2>/dev/null; then
    fail 'A8 self-test accepted a provider model mismatch for speech'
  fi
  if assert_provider_model refinement configured-refinement-model "$batch_refinement_model" 2>/dev/null; then
    fail 'A8 self-test accepted a provider model mismatch for refinement'
  fi
  assert_provider_model summary "$summary_provider_model" "$summary_provider_model" \
    || fail 'A8 self-test rejected the provider summary model'
  assert_provider_model search "$embedding_model" "$embedding_model" \
    || fail 'A8 self-test rejected the provider embedding model'
  assert_provider_model speech "$tts_model" "$tts_model" \
    || fail 'A8 self-test rejected the provider speech model'
  assert_provider_model refinement "$batch_refinement_model" "$batch_refinement_model" \
    || fail 'A8 self-test rejected the provider refinement model'
  assert_provider_model summary "$model_not_reported" "$summary_provider_model" \
    || fail 'A8 self-test rejected a summary model the provider did not report'
  assert_provider_model search "$model_not_reported" "$embedding_model" \
    || fail 'A8 self-test rejected a search model the provider did not report'
  assert_provider_model speech "$model_not_reported" "$tts_model" \
    || fail 'A8 self-test rejected a speech model the provider did not report'
  assert_provider_model refinement "$model_not_reported" "$batch_refinement_model" \
    || fail 'A8 self-test rejected a refinement model the provider did not report'
  printf 'A8 self-test RAN: planted failure rejected\n'

  local search_word scratch_dir acceptance_session_id unit_url=http://self-test
  search_word=$(session_search_word '{"session":{"canonical_transcript":"canonicaltoken"},"segments":[{"text":"segmenttoken"}]}')
  [[ "$search_word" == canonicaltoken ]] || fail 'A6 self-test did not use canonical_transcript for the search word'
  printf 'A6 self-test RAN: canonical transcript supplied the search word\n'

  session_detail() {
    printf '%s\n' '{"session":{"summary_status":"completed","summary":"summary","summary_version":1,"summary_model":"not reported by provider"}}'
  }
  wait_for_summary self-test 0 || fail 'A8 self-test rejected a summary whose model the provider did not report'

  curl() {
    printf '%s\n' '{"status":"completed","bytes_written":1,"model":"not reported by provider","voice":"Kore"}'
  }
  wait_for_tts self-test || fail 'A8 self-test rejected speech whose model the provider did not report'

  local refinement_marker="$controls_dir/refinement-running"
  session_detail() {
    if [[ ! -e "$refinement_marker" ]]; then
      : >"$refinement_marker"
      printf '%s\n' '{"session":{"refinement_status":"running"}}'
      return
    fi
    printf '%s\n' '{"session":{"refinement_status":"completed","refinement_model":"not reported by provider"}}'
  }
  wait_for_refinement self-test || fail 'A8 self-test rejected a refinement whose model the provider did not report'

  scratch_dir="$controls_dir/a8"
  mkdir "$scratch_dir"
  acceptance_session_id=self-test
  local summary_called="$controls_dir/summary-called"
  local speech_called="$controls_dir/speech-called"
  local refinement_called="$controls_dir/refinement-called"
  local summary_requested="$controls_dir/summary-requested"
  local refinement_requested="$controls_dir/refinement-requested"
  local refinement_seen_running="$controls_dir/refinement-seen-running"
  # The model each fake provider reports.
  local summary_reported=$summary_provider_model refinement_reported=$batch_refinement_model
  local search_reported=$embedding_model speech_reported=$tts_model
  status() { printf '%s\n' '{"warnings":[]}'; }
  running_config() { printf '%s\n' "$good_running_config"; }
  remote_run() { printf '%s\n' "$good_config"; }
  service_environment_names() { printf '%s\n' $'GHOST_WISPR_ADDR\nGHOST_WISPR_EMBEDDING_LOCATION'; }
  service_embedding_location() { printf '%s\n' 'GHOST_WISPR_EMBEDDING_LOCATION=global'; }
  session_detail() {
    if [[ -e "$refinement_requested" ]]; then
      if [[ ! -e "$refinement_seen_running" ]]; then
        : >"$refinement_seen_running"
        printf '%s\n' '{"session":{"refinement_status":"running"}}'
        return
      fi
      : >"$refinement_called"
      printf '%s\n' "{\"session\":{\"refinement_status\":\"completed\",\"refinement_model\":\"$refinement_reported\"}}"
      return
    fi
    if [[ -e "$summary_requested" ]]; then
      : >"$summary_called"
      printf '%s\n' "{\"session\":{\"summary_status\":\"completed\",\"summary\":\"summary\",\"summary_version\":2,\"summary_model\":\"$summary_reported\"}}"
      return
    fi
    printf '%s\n' '{"session":{"summary_version":1}}'
  }
  post_json() {
    case "$1" in
      */resummarize) : >"$summary_requested" ;;
      */retry-refinement)
        : >"$refinement_requested"
        printf '%s\n' '{"triggered":true}'
        ;;
      *) return 1 ;;
    esac
  }
  semantic_search() { printf '%s\n' "{\"results\":[{}],\"model\":\"$search_reported\"}"; }
  speak() {
    printf '%s\n' "X-Ghost-Wispr-TTS-Model: $speech_reported" "X-Ghost-Wispr-TTS-Voice: $tts_voice" >"$1"
    printf '%s\n' '{"id":"self-test"}'
  }
  curl() {
    : >"$speech_called"
    printf '%s\n' "{\"status\":\"completed\",\"bytes_written\":1,\"model\":\"$speech_reported\",\"voice\":\"$tts_voice\"}"
  }
  if ! run_a8; then
    fail 'A8 self-test did not pass when every role reported its model'
  fi
  [[ -e "$summary_called" && -e "$speech_called" && -e "$refinement_called" ]] \
    || fail 'A8 self-test did not run every provider role'
  printf 'A8 self-test RAN: every reported model returns PASS\n'

  # A provider that reports no model: A8 passes that role on the model the unit's configuration
  # names, which its precheck read, and runs every later role.
  rm -f -- "$summary_called" "$speech_called" "$refinement_called" "$summary_requested" "$refinement_requested" "$refinement_seen_running"
  summary_reported=$model_not_reported refinement_reported=$model_not_reported
  search_reported=$model_not_reported speech_reported=$model_not_reported
  run_a8 || fail 'A8 self-test rejected models no provider reported'
  [[ -e "$summary_called" && -e "$speech_called" && -e "$refinement_called" ]] \
    || fail 'A8 self-test stopped before every provider role ran when no provider reported its model'
  rm -f -- "$summary_called" "$speech_called" "$refinement_called" "$summary_requested" "$refinement_requested" "$refinement_seen_running"
  # Each negative check below passes only when run_a8 fails on the drift it plants: a run_a8 that
  # fails for any other reason fails the self-test and prints why.
  local a8_output
  # shellcheck disable=SC2329 # run_a8, called next, reads the deployed YAML through remote_run.
  remote_run() { printf '%s\n' "${good_config/embedding_model: gemini\/gemini-embedding-2/embedding_model: gemini/gemini-embedding-001}"; }
  if a8_output=$(run_a8 2>&1); then
    fail 'A8 self-test accepted an unreported search model when the configuration names another'
  fi
  [[ "$a8_output" == *"configuration lacks: embedding_model: gemini/$embedding_model"* ]] \
    || fail "A8 self-test rejected a configuration that names another search model, but not for that model: $a8_output"
  # shellcheck disable=SC2329 # The run_a8 of the next check reads the deployed YAML through remote_run.
  remote_run() { printf '%s\n' "$good_config"; }
  rm -f -- "$summary_called" "$speech_called" "$refinement_called" "$summary_requested" "$refinement_requested" "$refinement_seen_running"
  # shellcheck disable=SC2329 # run_a8, called next, reads the running models through running_config.
  running_config() { jq -c '.models.embedding = "gemini/gemini-embedding-001"' <<<"$good_running_config"; }
  if a8_output=$(run_a8 2>&1); then
    fail 'A8 self-test accepted a running process whose embedding model is not the plan'"'"'s while the YAML names it'
  fi
  [[ "$a8_output" == *"the running process reports embedding gemini/gemini-embedding-001, want gemini/$embedding_model"* ]] \
    || fail "A8 self-test rejected a running process with another embedding model, but not for that model: $a8_output"
  printf 'A8 self-test RAN: every role is compared with the model the running process reports\n'
  printf 'A8 self-test RAN: every role passes on a model the provider did not report, and later roles still run\n'

  # The member lists name the configured organizers (service_account and drive_owner), so the
  # self-test runs on any deployment's settings.
  if assert_drive_members "$(printf '%s\n' "$drive_owner|organizer" 'readers@example.com|reader')" readers@example.com 2>/dev/null; then
    fail 'A10 self-test accepted a member list without the service account as organizer'
  fi
  if assert_drive_members "$(printf '%s\n' "$service_account|organizer" "$drive_owner|organizer" 'readers@example.com|reader' 'extra@example.com|writer')" readers@example.com 2>/dev/null; then
    fail 'A10 self-test accepted an unapproved writer'
  fi
  if assert_drive_members "$(printf '%s\n' "$service_account|organizer" "$drive_owner|organizer" 'outside@example.com|reader')" readers@example.com 2>/dev/null; then
    fail 'A10 self-test accepted a reader other than the approved group'
  fi
  if assert_drive_members "$(printf '%s\n' "$service_account|organizer" "$drive_owner|organizer")" readers@example.com 2>/dev/null; then
    fail 'A10 self-test accepted a member list without a reader'
  fi
  assert_drive_members "$(printf '%s\n' '# Task 2.4 members' "$service_account|organizer" "${drive_owner^} | organizer" 'readers@example.com|reader')" readers@example.com \
    || fail 'A10 self-test rejected the expected member roles'
  printf 'A10 self-test RAN: planted failure rejected\n'

  if assert_certificate_warning '{"warnings":["Vertex unavailable"]}'; then
    fail 'Cert-warning self-test accepted a status with no certificate warning'
  fi
  assert_certificate_warning '{"warnings":["Google credential certificate expires on 2026-11-21; renew it with the runbook in docs/pi-setup.md, \"Renew the X.509 certificate\"."]}' \
    || fail 'Cert-warning self-test rejected a certificate-expiry warning'
  printf 'Cert-warning self-test RAN: planted failure rejected\n'

  # The office Pi's ghost-wispr user is uid and gid 1000.
  local ids=$'user:1000 1000\nuid:\t1000\t1000\t1000\t1000\ngid:\t1000\t1000\t1000\t1000\ncaps:0000000000000000 0000000000000000 0000000000000000'
  local good_instance="$ids"$'\ngroups:\nreach:\njabra:false\ndevices:'
  assert_cert_test_instance "$good_instance" 2>/dev/null \
    || fail 'Cert-warning self-test rejected a test instance as ghost-wispr with no groups, no capabilities, no device it can open, the Jabra button off and no device held'
  for planted in \
    "$ids"$'\ngroups:4 20 27 29 44 46 100 986 996\nreach:\njabra:false\ndevices:' \
    "$ids"$'\ngroups:\nreach:\njabra:\ndevices:' \
    "$ids"$'\ngroups:\nreach:\njabra:true\ndevices:' \
    "$ids"$'\ngroups:\nreach:\njabra:false\ndevices:/dev/snd/pcmC0D0c ' \
    "$ids"$'\ngroups:\nreach:\njabra:false\ndevices:/dev/input/event1 ' \
    "$ids"$'\ngroups:\nreach:/dev/snd/controlC0 /dev/snd/pcmC0D0c \njabra:false\ndevices:' \
    "$ids"$'\ngroups:\njabra:false\ndevices:' \
    $'user:1000 1000\nuid:\t0\t0\t0\t0\ngid:\t0\t0\t0\t0\ncaps:000001ffffffffff 000001ffffffffff 0000000000000000\ngroups:\nreach:\njabra:false\ndevices:' \
    $'user:1000 1000\nuid:\t1000\t0\t1000\t1000\ngid:\t1000\t1000\t1000\t1000\ncaps:0000000000000000 0000000000000000 0000000000000000\ngroups:\nreach:\njabra:false\ndevices:' \
    $'user:1000 1000\nuid:\t1000\t1000\t1000\t1000\ngid:\t1000\t29\t1000\t1000\ncaps:0000000000000000 0000000000000000 0000000000000000\ngroups:\nreach:\njabra:false\ndevices:' \
    $'user:1000 1000\nuid:\t1000\t1000\t1000\t1000\ngid:\t1000\t1000\t1000\t1000\ncaps:0000000000000000 0000000000000000 0000000000200000\ngroups:\nreach:\njabra:false\ndevices:' \
    $'user:1000 1000\nuid:\t1000\t1000\t1000\t1000\ngid:\t1000\t1000\t1000\t1000\ncaps:0000000000000000 0000000000000000\ngroups:\nreach:\njabra:false\ndevices:' \
    $'groups:\nreach:\njabra:false\ndevices:' \
    ''; do
    if assert_cert_test_instance "$planted" 2>/dev/null; then
      fail "Cert-warning self-test accepted a test instance that could open the Jabra: ${planted//$'\n'/; }"
    fi
  done
  printf 'Cert-warning self-test RAN: a test instance that could open the Jabra is rejected\n'
)
if [[ "$self_test" == true ]]; then
  run_self_test
  exit
fi

run_row A9 run_a9
run_row A6 run_a6
run_row A8 run_a8
run_row A10 run_a10
run_row Cert-warning run_certificate_warning

if (( row_failures > 0 )); then
  printf 'Slice 2 driver verdict: FAIL (%d row failures)\n' "$row_failures" >&2
  exit 1
fi
printf 'Slice 2 driver verdict: PASS; record every NEEDS-PERSON observation before acceptance.\n'
