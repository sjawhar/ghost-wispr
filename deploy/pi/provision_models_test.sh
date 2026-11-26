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
  [[ "$output" == *'usage: provision.sh --models <shared-drive-id>'* ]] \
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

# The Pi's two secrets in Secret Manager, which update.sh reads each night through these settings:
# each name starts with operator-only-, the prefix that holds every box identity off a secret of the
# project, so no box can read the Pi's Deepgram key or Dispatch token or add a version of either
# (dispatch://INFRA-5935/spec R3). provision.sh joins the tailnet with the client id and the service
# account's unique ID, each one token.
for variable in PI_DEEPGRAM_SECRET PI_DISPATCH_TOKEN_SECRET; do
  [[ ${!variable:-} =~ ^operator-only-[a-z0-9-]+$ ]] \
    || fail "pi.env sets $variable to '${!variable:-}', not a name under operator-only-, the prefix that holds every box identity off it"
done
[[ ${PI_TAILNET_CLIENT_ID:-} =~ ^[A-Za-z0-9._:-]+$ ]] \
  || fail "pi.env sets PI_TAILNET_CLIENT_ID to '${PI_TAILNET_CLIENT_ID:-}', not one client id"
[[ ${PI_SERVICE_ACCOUNT_UNIQUE_ID:-} =~ ^[0-9]+$ ]] \
  || fail "pi.env sets PI_SERVICE_ACCOUNT_UNIQUE_ID to '${PI_SERVICE_ACCOUNT_UNIQUE_ID:-}', not the service account's unique ID"

# Each setting of pi.env has a reader: a deploy script or a runbook step, the tests left out. A
# setting that only a deleted script read would stay in every release to mislead.
mapfile -t settings < <(sed -n 's/^\(PI_[A-Z0-9_]*\)=.*/\1/p' "$test_root/deploy/pi/files/pi.env")
((${#settings[@]} >= 10)) || fail "this reading of pi.env finds ${#settings[@]} settings: the reader check cannot see the file"
for setting in "${settings[@]}"; do
  status=0
  git -C "$test_root" grep -q -w -F -e "$setting" -- deploy docs ':(exclude)deploy/pi/files/pi.env' \
    ':(exclude)deploy/pi/test-pi.env' ':(exclude)*_test.sh' || status=$?
  case $status in
    0) ;;
    1) fail "pi.env sets $setting, which no deploy script and no runbook step reads" ;;
    *) fail "git grep could not search the repository for $setting (exit $status)" ;;
  esac
done

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
# is Vertex AI, with the request labels Cloud Billing attributes the Pi's Gemini spend by, and each
# role's provider/model; it sets no removed setting, and names no gateway provider, which refuses
# the Pi until it admits it.
assert_models() {
  local file=$1 paths line path
  paths=$(config_paths "$file")
  for line in \
    'providers.vertex.api: vertex' \
    "providers.vertex.project: $PI_GCP_PROJECT" \
    'providers.vertex.location: global' \
    'providers.vertex.auth.kind: google' \
    'providers.vertex.labels.owner: sjawhar' \
    'providers.vertex.labels.stream: agents-no-stream' \
    'providers.vertex-us.api: vertex' \
    "providers.vertex-us.project: $PI_GCP_PROJECT" \
    'providers.vertex-us.location: us' \
    'providers.vertex-us.auth.kind: google' \
    'providers.vertex-us.labels.owner: sjawhar' \
    'providers.vertex-us.labels.stream: agents-no-stream' \
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

# The committed template names the approved providers and models, keeps sync and GC off, and turns
# Envoy on at the hub's listener, for events and for the Concierge, which reads one Dispatch server.
assert_models "$template"
for line in \
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
assert_count '^  dispatch_url: https://[A-Za-z0-9.-][A-Za-z0-9.-]*$' 1 "$template"
# The template's public_url is the Pi's tailnet name, which the tailnet join checks it has.
assert_line "public_url: https://$PI_HOST" "$template"
assert_count '^public_url:' 1 "$template"

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

# The template holds every setting the nightly update installs: nothing is substituted, and no Envoy
# webhook setting is left.
for key in envoy_webhook_url envoy_webhook_secret_name; do
  assert_count "^$key:" 0 "$template"
done
assert_count '__[A-Z_]*__' 0 "$template"

# The sentence can wrap in the runbook, so each line break reads as a space.
for file in "$provisioner" "$test_root/docs/pi-setup.md"; do
  text=$(tr -s '\n' ' ' <"$file") || fail "cannot read $file"
  [[ $text == *'Settings saved on the page stay in the local settings file'* ]] \
    || fail "$file does not say settings saved on the page stay in the local settings file"
done

printf 'provision models test passed\n'
