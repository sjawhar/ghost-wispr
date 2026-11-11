#!/usr/bin/env bash
set -euo pipefail

test_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly test_root
readonly provisioner="$test_root/deploy/pi/provision.sh"
template="$test_root/deploy/pi/files/ghost-wispr.yaml"
# The backslash, ampersand and pipe are sed replacement metacharacters the renderer must escape.
relay_url='https://relay.example.test/a&b|c\d/webhook/ghostwispr'
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

# The committed template names the approved models and Google settings, and keeps sync and GC off.
for line in \
  'deepgram_model: nova-3' \
  '  model: nova-3' \
  '  model: gemini/gemini-4-carbon' \
  'embedding_model: gemini/gemini-embedding-2' \
  'tts_provider: google' \
  'tts_voice: en-US-Chirp3-HD-Charon' \
  'genai_backend: vertex' \
  'gcp_project: theorem-agent-infra' \
  'gcp_location: global' \
  'google_credentials_file: /opt/ghost-wispr/google-credentials.json' \
  'gdrive_sync_enabled: false' \
  'gc_enabled: false' \
  'gc_max_age_days: 0' \
  'gc_max_audio_size_mb: 20000' \
  'envoy_webhook_url: __ENVOY_WEBHOOK_URL__'; do
  assert_line "$line" "$template"
done
assert_count '^gdrive_folder_id:' 0 "$template"

# The full provisioner's configuration: the URL substituted literally, sync and GC off.
base_config="$work_dir/base.yaml"
provisioner_function render_base_config "$template" "$relay_url" >"$base_config" \
  || fail 'the base renderer rejected a valid webhook URL'
assert_line "envoy_webhook_url: $relay_url" "$base_config"
assert_count '__ENVOY_WEBHOOK_URL__' 0 "$base_config"
assert_line 'gdrive_sync_enabled: false' "$base_config"
assert_line 'gc_enabled: false' "$base_config"
assert_count '^gdrive_folder_id:' 0 "$base_config"

# The models configuration: every template setting, the quoted drive ID, sync and GC on, each key once.
models_config="$work_dir/models.yaml"
provisioner_function render_models_config "$template" "$relay_url" "$drive_id" >"$models_config" \
  || fail 'the models renderer rejected a valid shared-drive ID'
for line in \
  'deepgram_model: nova-3' \
  '  model: nova-3' \
  '  model: gemini/gemini-4-carbon' \
  'embedding_model: gemini/gemini-embedding-2' \
  'tts_provider: google' \
  'tts_voice: en-US-Chirp3-HD-Charon' \
  'genai_backend: vertex' \
  'gcp_project: theorem-agent-infra' \
  'gcp_location: global' \
  'gc_max_age_days: 0' \
  'gc_max_audio_size_mb: 20000' \
  "envoy_webhook_url: $relay_url" \
  "gdrive_folder_id: \"$drive_id\"" \
  'gdrive_sync_enabled: true' \
  'gc_enabled: true'; do
  assert_line "$line" "$models_config"
done
for key in gdrive_folder_id gdrive_sync_enabled gc_enabled envoy_webhook_url; do
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
envoy_webhook_url: __ENVOY_WEBHOOK_URL__
EOF
provisioner_function render_models_config "$stale_template" "$relay_url" "$drive_id" >"$stale_config" \
  || fail 'the models renderer rejected a template with stale Drive settings'
assert_line 'existing_setting: retained' "$stale_config"
assert_count 'stale-id' 0 "$stale_config"
assert_count 'false' 0 "$stale_config"
for key in gdrive_folder_id gdrive_sync_enabled gc_enabled; do
  assert_count "^$key:" 1 "$stale_config"
done

# Renderers fail, rather than print a partial configuration, on bad input.
for bad_id in '' 'drive id' 'drive"id'; do
  if provisioner_function render_models_config "$template" "$relay_url" "$bad_id" >/dev/null; then
    fail "the models renderer accepted the invalid shared-drive ID $(printf '%q' "$bad_id")"
  fi
done
if provisioner_function render_models_config "$template" 'http://relay.example.test/webhook/ghostwispr' "$drive_id" >/dev/null; then
  fail 'the models renderer accepted a non-HTTPS webhook URL'
fi
if provisioner_function render_models_config "$work_dir/missing.yaml" "$relay_url" "$drive_id" >/dev/null 2>&1; then
  fail 'the models renderer succeeded without a template'
fi

# The deployed values --models and a rerun of the full provisioner carry over.
[[ $(provisioner_function deployed_relay_url "$base_config") == "$relay_url" ]] \
  || fail 'the deployed webhook URL was not read back'
[[ $(provisioner_function deployed_drive_id "$models_config") == "$drive_id" ]] \
  || fail 'the deployed shared-drive ID was not read back'
drive_on_base=$(provisioner_function deployed_drive_id "$base_config") \
  || fail 'a configuration without a shared drive was treated as invalid'
[[ -z "$drive_on_base" ]] || fail "a configuration without a shared drive returned '$drive_on_base'"
no_url="$work_dir/no-url.yaml"
two_urls="$work_dir/two-urls.yaml"
unquoted_drive="$work_dir/unquoted-drive.yaml"
two_drives="$work_dir/two-drives.yaml"
printf '%s\n' 'envoy_enabled: true' >"$no_url"
printf 'envoy_webhook_url: %s\n' "$relay_url" "$relay_url" >"$two_urls"
printf '%s\n' "gdrive_folder_id: $drive_id" >"$unquoted_drive"
printf 'gdrive_folder_id: "%s"\n' "$drive_id" "$drive_id" >"$two_drives"
for config in "$no_url" "$two_urls"; do
  if provisioner_function deployed_relay_url "$config" >/dev/null; then
    fail "a webhook URL was read from $config"
  fi
done
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

# --models keeps every .env line and sets the Vertex embedding location exactly once.
deployed_env="$work_dir/env"
models_env="$work_dir/models.env"
cat >"$deployed_env" <<'EOF'
GHOST_WISPR_ADDR=127.0.0.1:8080
GHOST_WISPR_EMBEDDING_LOCATION=europe-west4
GHOST_WISPR_DEEPGRAM_API_KEY=test-key
EOF
provisioner_function render_models_env "$deployed_env" >"$models_env" \
  || fail 'the .env renderer rejected a deployed .env'
assert_line 'GHOST_WISPR_ADDR=127.0.0.1:8080' "$models_env"
assert_line 'GHOST_WISPR_DEEPGRAM_API_KEY=test-key' "$models_env"
assert_line 'GHOST_WISPR_EMBEDDING_LOCATION=us-central1' "$models_env"
assert_count '^GHOST_WISPR_EMBEDDING_LOCATION=' 1 "$models_env"
if provisioner_function render_models_env "$work_dir/missing.env" >/dev/null 2>&1; then
  fail 'the .env renderer succeeded without a deployed .env'
fi
# The full provisioner writes the same setting into a fresh .env. render_models_env has an identical
# printf line, so read only the full provisioner's .env block.
env_block=$(awk '/GHOST_WISPR_ADDR=127\.0\.0\.1:8080/,/\/dev\/stdin "\$DEPLOY_DIR\/\.env"/' "$provisioner")
[[ -n "$env_block" ]] || fail 'could not find the full provisioner .env block'
grep -Fqx "  printf '%s\n' \"\$EMBEDDING_LOCATION_SETTING\"" <<<"$env_block" \
  || fail 'the full provisioner does not write the Vertex embedding location into .env'

printf 'provision models test passed\n'
