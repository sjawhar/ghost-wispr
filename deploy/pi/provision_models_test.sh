#!/usr/bin/env bash
set -euo pipefail

test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly test_root
readonly provisioner="$test_root/deploy/pi/provision.sh"
template="$test_root/deploy/pi/files/ghost-wispr.yaml"
drive_id='0AAbCdEf_Gh-Ij'
work_dir=$(mktemp -d)
readonly work_dir
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'provision models test: %s\n' "$*" >&2
  exit 1
}

assert_line() {
  local expected=$1 file=$2
  grep -Fqx -- "$expected" "$file" || fail "$file is missing line: $expected"
}

assert_count() {
  local pattern=$1 expected=$2 file=$3 actual
  actual=$(grep -c -- "$pattern" "$file") || true
  [[ "$actual" == "$expected" ]] || fail "$file has '$actual' lines matching $pattern, want $expected"
}

# provisioner_function runs one provision.sh function in a subshell, so the sourced
# definitions (its fail among them) never replace this test's.
provisioner_function() {
  (
    # shellcheck disable=SC1090 # The provisioner exposes its pure renderers when sourced.
    source "$provisioner"
    "$@"
  )
}

# The command line rejects a missing or extra argument and every invalid ID before it checks the user
# or touches the unit.
for args in '--models' "--models $drive_id extra"; do
  # shellcheck disable=SC2086 # Each case is a whitespace-separated argument list.
  if output=$(bash "$provisioner" $args 2>&1); then
    fail "provision.sh $args unexpectedly succeeded"
  fi
  [[ "$output" == *'usage: deploy/pi/provision.sh --models <shared-drive-id>'* ]] \
    || fail "provision.sh $args did not explain its required input: $output"
done
for bad_id in '' 'drive id' 'drive/id' 'drive"id' "drive'id" 'drive:id' $'drive\nid'; do
  if output=$(bash "$provisioner" --models "$bad_id" 2>&1); then
    fail "--models accepted the invalid shared-drive ID $(printf '%q' "$bad_id")"
  fi
  [[ "$output" == *'shared-drive ID must be non-empty'* ]] \
    || fail "--models did not reject the invalid shared-drive ID $(printf '%q' "$bad_id"): $output"
done
provisioner_function valid_drive_id "$drive_id" || fail 'the shared-drive ID check rejected a valid ID'

# The office Pi's deployment settings, which the template's Google project, Envoy hub and
# public_url must agree with.
# shellcheck source=files/pi.env
source "$test_root/deploy/pi/files/pi.env"
# The hub's NATS server and its listener are one host: the listener answers on 9020.
hub_host=${PI_NATS_SERVER#nats://}
hub_host=${hub_host%:4222}
[[ $hub_host =~ ^[A-Za-z0-9.-]+$ ]] || fail "PI_NATS_SERVER names no hub host: $PI_NATS_SERVER"
readonly hub_url="http://$hub_host:9020"

# The committed template names the approved models and Google settings, and keeps sync and GC off.
for line in \
  'deepgram_model: nova-3' \
  '  model: nova-3' \
  '  model: gemini/gemini-4-carbon' \
  'embedding_model: gemini/gemini-embedding-2' \
  'tts_provider: gemini' \
  'tts_voice: Kore' \
  'genai_backend: vertex' \
  "gcp_project: $PI_GCP_PROJECT" \
  'gcp_location: global' \
  'google_credentials_file: /opt/ghost-wispr/google-credentials.json' \
  'gdrive_sync_enabled: false' \
  'gc_enabled: false' \
  'gc_max_age_days: 0' \
  'gc_max_audio_size_mb: 20000' \
  'envoy_enabled: true' \
  "envoy_hub_url: $hub_url" \
  "  envoy_url: $hub_url" \
  'jabra_mute_button: true' \
  'mic_device: J75' \
  'speaker_device: J75'; do
  assert_line "$line" "$template"
done
assert_count '^gdrive_folder_id:' 0 "$template"
# The template's public_url is the Pi's tailnet name, which the tailnet join checks it has.
assert_line "public_url: https://$PI_HOST" "$template"
assert_count '^public_url:' 1 "$template"

# The template holds every setting the full provisioner installs: nothing is substituted, and no
# Envoy webhook setting is left.
for key in envoy_webhook_url envoy_webhook_secret_name; do
  assert_count "^$key:" 0 "$template"
done
assert_count '__[A-Z_]*__' 0 "$template"

# The models configuration: every template setting, the quoted drive ID, sync and GC on, each key once.
models_config="$work_dir/models.yaml"
provisioner_function render_models_config "$template" "$drive_id" >"$models_config" \
  || fail 'the models renderer rejected a valid shared-drive ID'
for line in \
  'deepgram_model: nova-3' \
  '  model: nova-3' \
  '  model: gemini/gemini-4-carbon' \
  'embedding_model: gemini/gemini-embedding-2' \
  'tts_provider: gemini' \
  'tts_voice: Kore' \
  'genai_backend: vertex' \
  "gcp_project: $PI_GCP_PROJECT" \
  'gcp_location: global' \
  'gc_max_age_days: 0' \
  'gc_max_audio_size_mb: 20000' \
  "envoy_hub_url: $hub_url" \
  "gdrive_folder_id: \"$drive_id\"" \
  'gdrive_sync_enabled: true' \
  'gc_enabled: true' \
  'jabra_mute_button: true' \
  'mic_device: J75' \
  'speaker_device: J75'; do
  assert_line "$line" "$models_config"
done
for key in gdrive_folder_id gdrive_sync_enabled gc_enabled envoy_hub_url jabra_mute_button mic_device speaker_device; do
  assert_count "^$key:" 1 "$models_config"
done

# A template that already names a drive loses the stale ID and settings.
stale_template="$work_dir/stale-template.yaml"
stale_config="$work_dir/stale.yaml"
cat >"$stale_template" <<'EOF'
existing_setting: retained
gdrive_folder_id: "stale-id"
gdrive_sync_enabled: false
gc_enabled: false
envoy_hub_url: http://hub.example.ts.net:9020
EOF
provisioner_function render_models_config "$stale_template" "$drive_id" >"$stale_config" \
  || fail 'the models renderer rejected a template with stale Drive settings'
assert_line 'existing_setting: retained' "$stale_config"
assert_count 'stale-id' 0 "$stale_config"
assert_line 'envoy_hub_url: http://hub.example.ts.net:9020' "$stale_config"
assert_count 'false' 0 "$stale_config"
for key in gdrive_folder_id gdrive_sync_enabled gc_enabled; do
  assert_count "^$key:" 1 "$stale_config"
done

# Renderers fail, rather than print a partial configuration, on bad input.
for bad_id in '' 'drive id' 'drive"id'; do
  if provisioner_function render_models_config "$template" "$bad_id" >/dev/null; then
    fail "the models renderer accepted the invalid shared-drive ID $(printf '%q' "$bad_id")"
  fi
done
if provisioner_function render_models_config "$work_dir/missing.yaml" "$drive_id" >/dev/null 2>&1; then
  fail 'the models renderer succeeded without a template'
fi

# The deployed shared drive carries over to --models and a rerun of the full provisioner.
[[ $(provisioner_function deployed_drive_id "$models_config") == "$drive_id" ]] \
  || fail 'the deployed shared-drive ID was not read back'
drive_on_template=$(provisioner_function deployed_drive_id "$template") \
  || fail 'a configuration without a shared drive was treated as invalid'
[[ -z "$drive_on_template" ]] || fail "a configuration without a shared drive returned '$drive_on_template'"
unquoted_drive="$work_dir/unquoted-drive.yaml"
two_drives="$work_dir/two-drives.yaml"
printf '%s\n' "gdrive_folder_id: $drive_id" >"$unquoted_drive"
printf 'gdrive_folder_id: "%s"\n' "$drive_id" "$drive_id" >"$two_drives"
[[ $(provisioner_function deployed_drive_id "$unquoted_drive") == "$drive_id" ]] \
  || fail 'the deployed shared-drive ID was not read from yaml.v3 output'
if provisioner_function deployed_drive_id "$two_drives" >/dev/null; then
  fail "a shared-drive ID was read from $two_drives"
fi
# Every config API save writes yaml.v3 output, and yaml.v3 writes an empty folder as "".
empty_drive="$work_dir/empty-drive.yaml"
printf '%s\n' 'gdrive_folder_id: ""' >"$empty_drive"
drive_on_empty=$(provisioner_function deployed_drive_id "$empty_drive") \
  || fail 'a configuration the config API saved without a shared drive was treated as invalid'
[[ -z "$drive_on_empty" ]] || fail "a configuration with an empty shared drive returned '$drive_on_empty'"

# --models keeps every .env line but the Envoy webhook signing key an earlier provisioner wrote,
# which nothing reads now, and sets the Vertex embedding location exactly once. The office Pi
# turns the Jabra's Mute button on in such a line too (docs/pi-setup.md, "Pause and resume at the
# unit"): --models keeps it on, as it is, and writes no other.
deployed_env="$work_dir/env"
models_env="$work_dir/models.env"
cat >"$deployed_env" <<'EOF'
GHOST_WISPR_ADDR=127.0.0.1:8080
GHOST_WISPR_EMBEDDING_LOCATION=europe-west4
GHOST_WISPR_DEEPGRAM_API_KEY=test-key
GHOST_WISPR_ENVOY_WEBHOOK_SECRET=stale-signing-key
GHOST_WISPR_JABRA_MUTE_BUTTON=true
EOF
provisioner_function render_models_env "$deployed_env" >"$models_env" \
  || fail 'the .env renderer rejected a deployed .env'
assert_line 'GHOST_WISPR_ADDR=127.0.0.1:8080' "$models_env"
assert_line 'GHOST_WISPR_DEEPGRAM_API_KEY=test-key' "$models_env"
assert_line 'GHOST_WISPR_JABRA_MUTE_BUTTON=true' "$models_env"
assert_count '^GHOST_WISPR_JABRA_MUTE_BUTTON=' 1 "$models_env"
assert_line 'GHOST_WISPR_EMBEDDING_LOCATION=global' "$models_env"
assert_count '^GHOST_WISPR_EMBEDDING_LOCATION=' 1 "$models_env"
assert_count 'stale-signing-key' 0 "$models_env"
if provisioner_function render_models_env "$work_dir/missing.env" >/dev/null 2>&1; then
  fail 'the .env renderer succeeded without a deployed .env'
fi
# The full provisioner writes the same setting into a fresh .env. render_models_env has an identical
# printf line, so read only the full provisioner's .env block.
env_block=$(awk '/GHOST_WISPR_ADDR=127\.0\.0\.1:8080/,/\/dev\/stdin "\$DEPLOY_DIR\/\.env"/' "$provisioner")
[[ -n "$env_block" ]] || fail 'could not find the full provisioner .env block'
grep -Fqx "  printf '%s\n' \"\$EMBEDDING_LOCATION_SETTING\"" <<<"$env_block" \
  || fail 'the full provisioner does not write the Vertex embedding location into .env'

for file in "$provisioner" "$test_root/docs/pi-setup.md"; do
  grep -Fq -- 'replaces settings saved in the UI' "$file" \
    || fail "$file does not say --models replaces settings saved in the UI"
done

printf 'provision models test passed\n'
