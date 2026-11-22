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

# The office Pi's deployment settings, which the template's Google project, Envoy hub, Dispatch
# server and public_url must agree with.
# shellcheck source=files/pi.env
source "$test_root/deploy/pi/files/pi.env"
# The hub's NATS server and its listener are one host: the listener answers on 9020.
hub_host=${PI_NATS_SERVER#nats://}
hub_host=${hub_host%:4222}
[[ $hub_host =~ ^[A-Za-z0-9.-]+$ ]] || fail "PI_NATS_SERVER names no hub host: $PI_NATS_SERVER"
readonly hub_url="http://$hub_host:9020"

# config_paths FILE prints each setting of FILE, a YAML file written one setting a line, as its
# dotted path and value ("providers.vertex.location: global"), at any depth and any indentation, so
# a setting is checked under its own parents. A setting with settings under it prints as "path:".
# Comments, blank lines and list items are left out.
config_paths() {
  awk '
    /^[[:space:]]*(#|-|$)/ { next }
    {
      indent = match($0, /[^[:space:]]/) - 1
      line = substr($0, indent + 1)
      if (line !~ /^[^:]+:([[:space:]]|$)/) next
      while (depth > 0 && indents[depth] >= indent) depth--
      key = line; sub(/:.*/, "", key)
      value = line; sub(/^[^:]*:[[:space:]]*/, "", value); sub(/[[:space:]]+#.*$/, "", value)
      path = depth > 0 ? paths[depth] "." key : key
      print (value == "" ? path ":" : path ": " value)
      depth++; indents[depth] = indent; paths[depth] = path
    }
  ' "$1"
}

# The settings that providers replaced (internal/config, removedSettings): a start that finds one
# warns, and the role it set is off.
readonly removed_settings=(genai_backend gcp_project gcp_location embedding_model deepgram_model
  tts_provider tts_model tts_voice tts_max_length summarization.base_url batch_transcription.provider
  voice.tts_engine voice.tts_model voice.tts_voice voice.tts_location
  voice.gptlive.openai_identity_provider_id voice.gptlive.openai_service_account_id)

# assert_models FILE: FILE names the office Pi's providers, each in the Pi's Google project where it
# is Vertex AI, and each role's provider/model; it sets no removed setting, and names no gateway
# provider, which refuses the Pi until it admits it.
assert_models() {
  local file=$1 paths line path
  paths=$(config_paths "$file")
  for line in \
    'providers.vertex.api: vertex' \
    "providers.vertex.project: $PI_GCP_PROJECT" \
    'providers.vertex.location: global' \
    'providers.vertex.auth.kind: google' \
    'providers.vertex-us.api: vertex' \
    "providers.vertex-us.project: $PI_GCP_PROJECT" \
    'providers.vertex-us.location: us' \
    'providers.vertex-us.auth.kind: google' \
    'providers.openai.api: openai' \
    'providers.openai.auth.kind: openai_wif' \
    'providers.deepgram.api: deepgram' \
    'providers.deepgram.auth.kind: api_key' \
    'providers.deepgram.auth.env: GHOST_WISPR_DEEPGRAM_API_KEY' \
    'transcription.model: deepgram/nova-3' \
    'batch_transcription.model: deepgram/nova-3' \
    'summarization.model: vertex/gemini-4-carbon' \
    'embedding.model: vertex/gemini-embedding-2' \
    'concierge.model: vertex/gemini-3.5-flash-lite' \
    'tts.model: vertex-us/gemini-3.8-flash-tts' \
    'tts.voice: Kore' \
    'voice.enabled: true' \
    'voice.engine: tts' \
    'voice.gptlive.model: openai/gpt-live-1' \
    'voice.dgagent.provider: deepgram'; do
    grep -Fqx -- "$line" <<<"$paths" || fail "$file does not set $line"
  done
  if grep -q '^providers[.]gateway[.:]' <<<"$paths"; then
    fail "$file names the gateway provider, which refuses the Pi until it admits it"
  fi
  # OpenAI's workload identity ids are the office's own.
  for path in providers.openai.auth.identity_provider_id providers.openai.auth.service_account_id; do
    grep -Eq "^${path//./[.]}: [^[:space:]]+$" <<<"$paths" || fail "$file sets no $path"
  done
  for path in "${removed_settings[@]}"; do
    if grep -q "^${path//./[.]}:" <<<"$paths"; then
      fail "$file still sets $path, which providers replaced"
    fi
  done
}

# The committed template names the approved providers and models, and keeps sync and GC off.
assert_models "$template"
for line in \
  'google_credentials_file: /opt/ghost-wispr/google-credentials.json' \
  'gdrive_sync_enabled: false' \
  'gc_enabled: false' \
  'gc_max_age_days: 0' \
  'gc_max_audio_size_mb: 20000' \
  'envoy_enabled: false' \
  "envoy_hub_url: $hub_url" \
  '  envoy_url: ""' \
  "  dispatch_url: $PI_DISPATCH_URL" \
  'jabra_mute_button: true' \
  'mic_device: J75' \
  'speaker_device: J75'; do
  assert_line "$line" "$template"
done
assert_count '^gdrive_folder_id:' 0 "$template"
# The template's public_url is the Pi's tailnet name, which the tailnet join checks it has.
assert_line "public_url: https://$PI_HOST" "$template"
assert_count '^public_url:' 1 "$template"
# The Deepgram provider reads its key from the variable the full provisioner writes the credential
# bundle's deepgram-key into.
grep -Fq "printf 'GHOST_WISPR_DEEPGRAM_API_KEY=%s\n' \"\$deepgram_key\"" "$provisioner" \
  || fail 'the full provisioner does not write the Deepgram key into the variable providers.deepgram.auth.env names'

# The check above sees a removed setting at any depth: each one planted in the template is refused,
# for that setting.
planted="$work_dir/planted.yaml"
for path in "${removed_settings[@]}"; do
  cp -- "$template" "$planted"
  IFS=. read -r -a parts <<<"$path"
  indent=''
  for part in "${parts[@]:0:${#parts[@]}-1}"; do
    printf '%s%s:\n' "$indent" "$part" >>"$planted"
    indent+='  '
  done
  printf '%s%s: planted\n' "$indent" "${parts[-1]}" >>"$planted"
  if refusal=$(assert_models "$planted" 2>&1); then
    fail "the template check did not find $path planted in it"
  fi
  [[ $refusal == *"still sets $path,"* ]] || fail "the template check refused $path planted in it for another reason: $refusal"
done

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
assert_models "$models_config"
for line in \
  'gc_max_age_days: 0' \
  'gc_max_audio_size_mb: 20000' \
  "envoy_hub_url: $hub_url" \
  'envoy_enabled: false' \
  '  envoy_url: ""' \
  "gdrive_folder_id: \"$drive_id\"" \
  'gdrive_sync_enabled: true' \
  'gc_enabled: true' \
  'jabra_mute_button: true' \
  'mic_device: J75' \
  'speaker_device: J75'; do
  assert_line "$line" "$models_config"
done
for key in gdrive_folder_id gdrive_sync_enabled gc_enabled envoy_enabled envoy_hub_url jabra_mute_button mic_device speaker_device providers; do
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

# --models keeps every .env line but those of settings Ghost Wispr no longer reads, each of which a
# start warns about: the Envoy webhook signing key an earlier provisioner wrote, and each variable
# that providers replaced (internal/config, removedEnvironment), such as the Vertex embedding
# location an earlier provisioner wrote. It keeps the role overrides that providers kept, whose
# names share words with removed ones, and writes no line of its own. The office Pi turns the
# Jabra's Mute button on in such a line too (docs/pi-setup.md, "Pause and resume at the unit"):
# --models keeps it on, as it is.
readonly removed_variables=(GCP_PROJECT GCP_LOCATION GENAI_BACKEND EMBEDDING_BACKEND EMBEDDING_LOCATION
  DEEPGRAM_MODEL BATCH_TRANSCRIPTION_PROVIDER TTS_PROVIDER TTS_API_KEY VOICE_TTS_ENGINE VOICE_TTS_MODEL
  VOICE_TTS_VOICE VOICE_TTS_LOCATION ENVOY_WEBHOOK_SECRET)
readonly kept_lines=(GHOST_WISPR_ADDR=127.0.0.1:8080 GHOST_WISPR_DEEPGRAM_API_KEY=test-key
  GHOST_WISPR_JABRA_MUTE_BUTTON=true GHOST_WISPR_TTS_VOICE=Kore GHOST_WISPR_TTS_MODEL=vertex/tts-1
  GHOST_WISPR_EMBEDDING_MODEL=vertex/embed-1 GHOST_WISPR_BATCH_TRANSCRIPTION_MODEL=deepgram/nova-3
  GHOST_WISPR_VOICE_ENGINE=tts)
deployed_env="$work_dir/env"
models_env="$work_dir/models.env"
for ((i = 0; i < ${#removed_variables[@]} || i < ${#kept_lines[@]}; i++)); do
  ((i >= ${#kept_lines[@]})) || printf '%s\n' "${kept_lines[i]}"
  ((i >= ${#removed_variables[@]})) || printf 'GHOST_WISPR_%s=removed-value\n' "${removed_variables[i]}"
done >"$deployed_env"
provisioner_function render_models_env "$deployed_env" >"$models_env" \
  || fail 'the .env renderer rejected a deployed .env'
if ! diff <(printf '%s\n' "${kept_lines[@]}") "$models_env" >&2; then
  fail "--models did not keep exactly the .env lines of settings Ghost Wispr reads, in order (diff above)"
fi
assert_count 'removed-value' 0 "$models_env"
if provisioner_function render_models_env "$work_dir/missing.env" >/dev/null 2>&1; then
  fail 'the .env renderer succeeded without a deployed .env'
fi

# While the hub moves (dispatch://INFRA-3862), --models also drops each .env line that would turn
# Envoy on over the template, which turns it off: an override of envoy_enabled or of
# concierge.envoy_url, also with spaces around its name. It names each on stderr. It keeps the
# hub's URL, which sends nothing with Envoy off, and every other line, in order.
envoy_env="$work_dir/envoy.env"
envoy_models_env="$work_dir/envoy-models.env"
envoy_messages="$work_dir/envoy-messages"
readonly envoy_kept_lines=(GHOST_WISPR_ADDR=127.0.0.1:8080 "GHOST_WISPR_ENVOY_HUB_URL=$hub_url"
  GHOST_WISPR_VOICE_ENGINE=tts)
printf '%s\n' "${envoy_kept_lines[0]}" GHOST_WISPR_ENVOY_ENABLED=true "${envoy_kept_lines[1]}" \
  "GHOST_WISPR_CONCIERGE_ENVOY_URL=$hub_url" ' GHOST_WISPR_ENVOY_ENABLED = 1' "${envoy_kept_lines[2]}" >"$envoy_env"
provisioner_function render_models_env "$envoy_env" >"$envoy_models_env" 2>"$envoy_messages" \
  || fail 'the .env renderer rejected a deployed .env with Envoy overrides'
if ! diff <(printf '%s\n' "${envoy_kept_lines[@]}") "$envoy_models_env" >&2; then
  fail "--models did not drop exactly the .env lines that turn Envoy on (diff above)"
fi
for name in GHOST_WISPR_ENVOY_ENABLED GHOST_WISPR_CONCIERGE_ENVOY_URL GHOST_WISPR_ENVOY_ENABLED; do
  printf 'ghost-wispr provision: removed %s from .env: the template keeps Envoy off while the hub moves (dispatch://INFRA-3862)\n' "$name"
done | diff - "$envoy_messages" >&2 || fail "--models did not name each .env line it dropped to keep Envoy off (diff above)"

for file in "$provisioner" "$test_root/docs/pi-setup.md"; do
  grep -Fq -- 'replaces settings saved in the UI' "$file" \
    || fail "$file does not say --models replaces settings saved in the UI"
done

printf 'provision models test passed\n'
