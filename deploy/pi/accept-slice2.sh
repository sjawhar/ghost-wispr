#!/usr/bin/env bash
# Exercise the slice 2 acceptance checks from an operator machine on the tailnet.
set -euo pipefail

host=${GHOST_WISPR_HOST:-ghost-wispr.tail12ec1c.ts.net}
self_test=false
drive_members_file=
readers_group=
cert_test=false
readonly service_account_key_pattern='"type"[[:space:]]*:[[:space:]]*"service_account"|"private_key_id"'
readonly service_account_key_body_pattern='"private_key"[[:space:]]*:[[:space:]]*"-----BEGIN'
readonly credential_path=/opt/ghost-wispr/google-credentials.json
readonly expected_certificate_config=/opt/ghost-wispr/certificate_config.json
readonly deployed_config=/opt/ghost-wispr/ghost-wispr.yaml
readonly service_account=ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com
readonly drive_owner=sami@theorem.dev
readonly cert_test_port=8081
readonly summarization_model=gemini/gemini-4-carbon
expected_config_lines=(
  'deepgram_model: nova-3'
  'batch_transcription.provider: deepgram'
  'batch_transcription.model: nova-3'
  "summarization.model: $summarization_model"
  'embedding_model: gemini/gemini-embedding-2'
  'genai_backend: vertex'
  'gcp_project: theorem-agent-infra'
  'gcp_location: global'
  'tts_provider: google'
  'tts_voice: en-US-Chirp3-HD-Charon'
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
matches=$(printf '%s\n' "$key_pattern" \
  | as_root grep -rlE --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev --exclude-dir=run -f /dev/stdin -- "$root") \
  || status=$?
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

assert_running_summarization_model() {
  jq -e --arg model "$summarization_model" '.summarization.model == $model' >/dev/null <<<"$1"
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
  [[ $1 == GHOST_WISPR_EMBEDDING_LOCATION=us-central1 ]]
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

run_key_sweep_locally() {
  bash -s <<<"$(key_sweep_script "$1" "$1" '')"
}

# run_self_test runs in a subshell so that its EXIT trap removes the controls
# even when fail exits.
run_self_test() (
  local controls_dir key_root report good_config marshaled_config swapped_config
  controls_dir=$(mktemp -d)
  trap 'rm -rf -- "$controls_dir"' EXIT

  key_root="$controls_dir/key-root"
  mkdir "$key_root"
  printf 'benign\n' >"$key_root/notes.txt"
  printf 'ELF\000json:"private_key_id"\n' >"$key_root/binary-with-field-name"
  report=$(run_key_sweep_locally "$key_root") || fail 'A9 self-test could not run the key sweep'
  grep -Fqx "binary-field-name"$'\t'"$key_root/binary-with-field-name" <<<"$report" \
    || fail 'A9 self-test sweep did not report the binary file that names a key field'
  key_sweep_passes "$report" >/dev/null || fail 'A9 self-test rejected a tree whose only match is a binary field name'
  printf '{"type": "service_account", "private_key_id": "planted"}\n' >"$key_root/planted-key.json"
  report=$(run_key_sweep_locally "$key_root") || fail 'A9 self-test could not run the key sweep'
  if key_sweep_passes "$report" >/dev/null 2>&1; then
    fail 'A9 self-test accepted a planted service-account key file'
  fi
  rm -- "$key_root/planted-key.json"
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
  good_config=$(cat <<'EOF'
deepgram_model: nova-3
batch_transcription:
  provider: deepgram
  model: nova-3
summarization:
  model: gemini/gemini-4-carbon
embedding_model: gemini/gemini-embedding-2
genai_backend: vertex
gcp_project: theorem-agent-infra
gcp_location: global
tts_provider: google
tts_voice: en-US-Chirp3-HD-Charon
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
  if assert_running_summarization_model '{"summarization":{"model":"gemini/gemini-2.5-flash"}}'; then
    fail 'A8 self-test accepted a running summarization model that differs from the YAML'
  fi
  assert_running_summarization_model '{"summarization":{"model":"gemini/gemini-4-carbon"}}' \
    || fail 'A8 self-test rejected the running summarization model'
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
  if assert_embedding_location 'GHOST_WISPR_EMBEDDING_LOCATION=global' 2>/dev/null; then
    fail 'A8 self-test accepted an embedding location outside us-central1'
  fi
  assert_embedding_location 'GHOST_WISPR_EMBEDDING_LOCATION=us-central1' \
    || fail 'A8 self-test rejected the required embedding location'
  printf 'A8 self-test RAN: planted failure rejected\n'

  if assert_drive_members $'sami@theorem.dev|organizer\nreaders@theorem.dev|reader' readers@theorem.dev 2>/dev/null; then
    fail 'A10 self-test accepted a member list without the service account as organizer'
  fi
  if assert_drive_members $'ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com|organizer\nsami@theorem.dev|organizer\nreaders@theorem.dev|reader\nextra@theorem.dev|writer' readers@theorem.dev 2>/dev/null; then
    fail 'A10 self-test accepted an unapproved writer'
  fi
  if assert_drive_members $'ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com|organizer\nsami@theorem.dev|organizer\noutside@example.com|reader' readers@theorem.dev 2>/dev/null; then
    fail 'A10 self-test accepted a reader other than the approved group'
  fi
  if assert_drive_members $'ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com|organizer\nsami@theorem.dev|organizer' readers@theorem.dev 2>/dev/null; then
    fail 'A10 self-test accepted a member list without a reader'
  fi
  assert_drive_members $'# Task 2.4 members\nghost-wispr@theorem-agent-infra.iam.gserviceaccount.com|organizer\nSami@theorem.dev | organizer\nreaders@theorem.dev|reader' readers@theorem.dev \
    || fail 'A10 self-test rejected the expected member roles'
  printf 'A10 self-test RAN: planted failure rejected\n'

  if assert_certificate_warning '{"warnings":["Vertex unavailable"]}'; then
    fail 'Cert-warning self-test accepted a status with no certificate warning'
  fi
  assert_certificate_warning '{"warnings":["Google credential certificate expires on 2026-11-21; renew it with the runbook in docs/pi-setup.md."]}' \
    || fail 'Cert-warning self-test rejected a certificate-expiry warning'
  printf 'Cert-warning self-test RAN: planted failure rejected\n'
)

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
  for command in awk bash grep jq mktemp sort tr; do
    command -v "$command" >/dev/null || fail "self-test requires $command"
  done
  run_self_test
  exit 0
fi

for command in awk curl jq ssh sort tr; do
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

remote_run() {
  ssh -o BatchMode=yes "$remote" "$@"
}

status() {
  curl --fail --silent --show-error "$unit_url/api/status"
}

running_config() {
  curl --fail --silent --show-error "$unit_url/api/config"
}

service_environment_names() {
  # shellcheck disable=SC2016 # The remote shell expands the service runtime path.
  remote_run 'pid=$(XDG_RUNTIME_DIR="/run/user/$(id -u)" systemctl --user show --property=MainPID --value ghost-wispr.service) && [ "$pid" -gt 0 ] && tr "\000" "\n" <"/proc/$pid/environ" | sed "s/=.*//"'
}

service_embedding_location() {
  # shellcheck disable=SC2016 # The remote shell expands the service runtime path.
  remote_run 'pid=$(XDG_RUNTIME_DIR="/run/user/$(id -u)" systemctl --user show --property=MainPID --value ghost-wispr.service) && [ "$pid" -gt 0 ] && tr "\000" "\n" <"/proc/$pid/environ" | grep "^GHOST_WISPR_EMBEDDING_LOCATION="'
}

printf 'A9 RAN: plant a service-account key in /tmp and require the full key sweep to find it and nothing else.\n'
key_report=$(remote_run 'bash -s' <<<"$(key_sweep_script / /tmp sudo)") || fail 'A9 FAILED: the key sweep did not run on the Pi'
key_sweep_passes "$key_report" || fail 'A9 FAILED: the key sweep missed its planted control or found key material'
credential=$(remote_run "cat -- $credential_path") || fail "A9 FAILED: could not read $credential_path"
assert_external_account_credential "$credential" \
  || fail 'A9 FAILED: google-credentials.json is not an external-account certificate credential'
printf 'A9 RAN: no service-account key and the configured credential is external-account X.509.\n'

printf 'A6 RAN: require the running configuration to sync to a Drive folder with credentials and audio GC.\n'
current_config=$(running_config) || fail 'A6 FAILED: could not read /api/config'
assert_drive_config "$current_config" \
  || fail 'A6 FAILED: Drive sync, its folder, its credentials or GC are not as provision.sh --models sets them'
printf 'A6 NEEDS-PERSON: before recording or playing a two-minute conversation, start nats sub '\''notifications.ghostwispr.>'\'' --server nats://envoy-hub.tail12ec1c.ts.net:4222; after the silence timeout, verify a dated Drive folder has transcript, summary, and audio-*.mp3, the summary names the topics spoken, web search finds a spoken phrase, and the subscriber observed one summary.ready for that session.\n'

printf 'A8 RAN: require a warning-free status and the configured current models.\n'
current_status=$(status) || fail 'A8 FAILED: could not read /api/status'
assert_no_warnings "$current_status" || fail 'A8 FAILED: /api/status reports a warning'
deployed_yaml=$(remote_run "cat -- $deployed_config") || fail "A8 FAILED: could not read $deployed_config"
assert_expected_config "$deployed_yaml" || fail 'A8 FAILED: the Pi configuration does not name the accepted models'
assert_running_summarization_model "$current_config" \
  || fail 'A8 FAILED: the running summarization model is not the accepted one'
environment_names=$(service_environment_names) || fail 'A8 FAILED: could not read the ghost-wispr.service environment'
assert_no_config_overrides "$environment_names" \
  || fail 'A8 FAILED: the service environment overrides a configured model or Google setting'
embedding_location=$(service_embedding_location) || fail 'A8 FAILED: could not read the embedding location from the service environment'
assert_embedding_location "$embedding_location" || fail 'A8 FAILED: the embedding location is not us-central1'
printf 'A8 NEEDS-PERSON: make one summary, search, speak, and batch-refinement request through Ghost Wispr; verify provider responses or logs name nova-3, gemini-4-carbon, gemini-embedding-2, and en-US-Chirp3-HD-Charon, hear the voice, and see refinement completed.\n'

if [[ -n "$drive_members_file" ]]; then
  printf 'A10 RAN: check the Task 2.4 member list.\n'
  assert_drive_members "$(<"$drive_members_file")" "$readers_group" || fail 'A10 FAILED: the shared drive members are not as approved'
  printf 'A10 RAN: only the service account and Sami are organizers, and %s is the only reader.\n' "$readers_group"
else
  printf 'A10 NEEDS-PERSON: rerun with --drive-members FILE and --readers-group EMAIL from Task 2.4.\n'
fi
printf 'A10 NEEDS-PERSON: as a readers-group member who is not a Manager, open a folder in the shared drive; the member must read but fail to upload.\n'

if [[ "$cert_test" == true ]]; then
  printf 'Cert-warning RAN: read /api/status from the test instance on 127.0.0.1:%s.\n' "$cert_test_port"
  cert_status=$(remote_run "curl --fail --silent --show-error http://127.0.0.1:$cert_test_port/api/status") \
    || fail "Cert-warning FAILED: no test instance answered on 127.0.0.1:$cert_test_port"
  assert_certificate_warning "$cert_status" || fail 'Cert-warning FAILED: the test instance does not warn about certificate expiry'
  printf 'Cert-warning RAN: the test instance warns that its certificate expires.\n'
else
  printf 'Cert-warning NEEDS-PERSON: use a copied config with separate db_path and audio_dir; disable Drive sync, GC, Envoy, speaker, light, and power button; point its copied credential config at a certificate expiring in 10 days; start it with GHOST_WISPR_CONFIG=<copy> and GHOST_WISPR_ADDR=127.0.0.1:%s; then rerun with --cert-test.\n' "$cert_test_port"
fi
printf 'Slice 2 driver RAN automated rows; record every NEEDS-PERSON observation before acceptance.\n'
