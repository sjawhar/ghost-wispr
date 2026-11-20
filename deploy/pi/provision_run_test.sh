#!/usr/bin/env bash
# Runs the full deploy/pi/provision.sh on a credential bundle with fakes for sudo, id and
# tailscale, and checks what the Pi is told to do: the service user joins the audio group, the
# update runs with the user's runtime directory, and the detached join sets Tailscale Serve again
# once `tailscale up` has re-authenticated the node, and only then.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'provision run test FAILED: %s\n' "$*" >&2
  exit 1
}

# The sha256 of the office Pi's recording_rules block in deploy/pi/files/ghost-wispr.yaml, which
# its web UI shows under Recording rules, each as written and in this order (docs/pi-setup.md,
# "Accept slice 1", A2). The test holds the hash, not the text, because the repository is public.
# After a rule change, set it to the awk below run over the template, piped to sha256sum.
readonly recording_rules_sha256=89f65b9df94d27abf6657ba13e183921b7c27e19d8ba5e90c189e4046c5c8040

# assert_recording_rules fails unless the configuration provision.sh installed holds exactly the
# office Pi's recording rules, once.
assert_recording_rules() {
  local config=$TEST_LOG.installed-ghost-wispr.yaml rules
  rules=$(awk '/^recording_rules:/ { print; inside = 1; next } inside && /^  - / { print; next } { inside = 0 }' "$config" | sha256sum)
  [[ $rules == "$recording_rules_sha256  -" ]] \
    || fail "the $1 configuration does not hold the office Pi's five recording rules: $(<"$config")"
}

# assert_office_pi_devices fails unless the configuration provision.sh installed turns the Jabra's
# Mute button on once, with no .env line to override it, and names the Jabra Speak2 75 by its ALSA
# card id as the microphone's and the speaker's sound card once (mic_device and speaker_device: J75),
# so another capture card, such as an ALSA loopback or a second Jabra device, never counts as the
# microphone. The full provisioner rewrites .env, so the template's settings are the Pi's.
assert_office_pi_devices() {
  local config=$TEST_LOG.installed-ghost-wispr.yaml env=$TEST_LOG.installed-.env settings
  settings=$(grep -E '^jabra_mute_button:' "$config") || true
  [[ $settings == 'jabra_mute_button: true' ]] \
    || fail "the $1 configuration does not turn the Jabra's Mute button on once: ${settings:-no jabra_mute_button line}"
  if grep -Eq '^GHOST_WISPR_JABRA_MUTE_BUTTON=' "$env"; then
    fail "the $1 .env sets the Jabra's Mute button: $(grep -E '^GHOST_WISPR_JABRA_MUTE_BUTTON=' "$env")"
  fi
  settings=$(grep -E '^mic_device:' "$config") || true
  [[ $settings == 'mic_device: J75' ]] \
    || fail "the $1 configuration does not name the Jabra as the microphone once: ${settings:-no mic_device line}"
  settings=$(grep -E '^speaker_device:' "$config") || true
  [[ $settings == 'speaker_device: J75' ]] \
    || fail "the $1 configuration does not name the Jabra as the speaker once: ${settings:-no speaker_device line}"
}

uid=$(id -u)
readonly uid runtime_dir="/run/user/$uid"
[[ -d $runtime_dir ]] || fail "this test needs $runtime_dir, where provision.sh stages the bundle"

readonly CLIENT_ID=k-test-client-id SUBJECT=123456789012345678901
export CLIENT_ID SUBJECT TEST_LOG="$work_dir/log" TEST_UID="$uid"
mkdir -p "$work_dir/bin" "$work_dir/deploy" "$work_dir/bundle"
touch "$work_dir/key.pem"

cat >"$work_dir/bin/id" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  -un) printf 'ghost-wispr\n' ;;
  '-u ghost-wispr') printf '%s\n' "$TEST_UID" ;;
  '-nG ghost-wispr') printf 'ghost-wispr sudo plugdev input\n' ;;
  *) printf 'fake id: %s\n' "$*" >&2; exit 1 ;;
esac
EOF

# sudo records each command and runs none, but systemd-run's command is kept to run below, and the
# configuration provision.sh installs is kept to read.
cat >"$work_dir/bin/sudo" <<'EOF'
#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >>"$TEST_LOG"
if [[ " $* " == *' /dev/stdin '* ]]; then
  cat >"$TEST_LOG.installed-$(basename -- "${!#}")"
elif [[ $1 == install && ${!#} == */ghost-wispr.yaml ]]; then
  cp -- "${@:$#-1:1}" "$TEST_LOG.installed-ghost-wispr.yaml"
fi
if [[ $1 == systemd-run ]]; then
  printf '%s' "${!#}" >"$TEST_LOG.join"
fi
EOF

cat >"$work_dir/bin/tailscale" <<'EOF'
#!/usr/bin/env bash
printf 'tailscale %s\n' "$*" >>"$TEST_LOG"
case $1 in
  status) printf '{"Self":{"Tags":null,"DNSName":"ghost-wispr.example-tailnet.ts.net."}}\n' ;;
  up) exit "${TEST_UP_EXIT:-0}" ;;
esac
EOF

# The release's update.sh and binary, as provision.sh runs them from the deploy directory. The real
# update.sh reads /api/status once, and fails when the recorder runs but does not answer yet, as
# it does right after a restart: so does this one when the recorder was restarted before it.
cat >"$work_dir/deploy/update.sh" <<'EOF'
#!/usr/bin/env bash
printf 'update.sh XDG_RUNTIME_DIR=%s\n' "${XDG_RUNTIME_DIR:-}" >>"$TEST_LOG"
if grep -q 'systemctl --user try-restart ghost-wispr.service$' "$TEST_LOG"; then
  printf 'ghost-wispr update: ghost-wispr is active but /api/status did not answer\n' >&2
  exit 1
fi
EOF
cat >"$work_dir/deploy/ghost-wispr" <<'EOF'
#!/usr/bin/env bash
payload=$(printf '{"sub":"%s","aud":"api.tailscale.com/%s"}' "$SUBJECT" "$CLIENT_ID" | base64 -w0 | tr '+/' '-_' | tr -d '=')
printf 'eyJhbGciOiJSUzI1NiJ9.%s.c2lnbmF0dXJl\n' "$payload"
EOF
chmod +x "$work_dir/bin/"* "$work_dir/deploy/update.sh" "$work_dir/deploy/ghost-wispr"

printf 'CERTIFICATE\n' >"$work_dir/bundle/device.crt"
printf '{"type":"external_account","credential_source":{"certificate":{"certificate_config_location":"/opt/ghost-wispr/certificate_config.json"}}}\n' \
  >"$work_dir/bundle/google-credentials.json"
printf '{}\n' >"$work_dir/bundle/certificate_config.json"
printf '%s\n' "$CLIENT_ID" >"$work_dir/bundle/tailscale-client-id"
printf '%s\n' "$SUBJECT" >"$work_dir/bundle/tailscale-subject"
printf 'https://relay.example.test/webhook/ghostwispr\n' >"$work_dir/bundle/envoy-webhook-url"
printf 'deepgram-key' >"$work_dir/bundle/deepgram-key"

# run_provision runs provision.sh on a bundle of the named files, with neutral deployment settings.
run_provision() {
  tar -C "$work_dir/bundle" -cf - "$@" \
    | env -u XDG_RUNTIME_DIR PATH="$work_dir/bin:$PATH" GHOST_WISPR_DEPLOY_DIR="$work_dir/deploy" \
      GHOST_WISPR_IDENTITY_KEY="$work_dir/key.pem" GHOST_WISPR_PI_ENV="$repo_root/deploy/pi/test-pi.env" \
      bash "$repo_root/deploy/pi/provision.sh" >"$work_dir/out" 2>&1
}
readonly bundle_files=(device.crt google-credentials.json certificate_config.json tailscale-client-id
  tailscale-subject envoy-webhook-url deepgram-key)

# The Envoy signing key is no longer a bundle file: the recorder reads it from Secret Manager with
# its Google identity. A bundle that still carries one is refused before anything is installed.
printf 'webhook-secret' >"$work_dir/bundle/envoy-webhook-secret"
if run_provision "${bundle_files[@]}" envoy-webhook-secret; then
  fail "provision.sh accepted a bundle with envoy-webhook-secret"
fi
grep -Fq 'credential bundle contains unexpected path: envoy-webhook-secret' "$work_dir/out" \
  || fail "provision.sh did not name envoy-webhook-secret when it refused the bundle: $(<"$work_dir/out")"
[[ ! -e $TEST_LOG ]] || fail "provision.sh ran commands for a refused bundle: $(<"$TEST_LOG")"

run_provision "${bundle_files[@]}" || fail "provision.sh failed: $(<"$work_dir/out")"
# A run that works says no bash error. On the office Pi (2026-11-16) every full run printed
# "provision.sh: line 42: local: relay_url: readonly variable": render_base_config declares a local
# relay_url while the main flow's relay_url is readonly, so the function renders the global value
# and ignores the URL it is given.
if grep -E 'provision\.sh: line [0-9]+: ' "$work_dir/out" >&2; then
  fail "provision.sh printed a bash error (above)"
fi
assert_recording_rules 'installed'
assert_office_pi_devices 'installed'

# The .env the recorder runs with holds the Deepgram key and no Envoy signing key.
env_file="$TEST_LOG.installed-.env"
deepgram_key_name="DEEPGRAM""_API_KEY"
grep -Fxq "GHOST_WISPR_$deepgram_key_name=deepgram-key" "$env_file" || fail "the installed .env has no Deepgram key"
if grep -q 'ENVOY_WEBHOOK_SECRET' "$env_file"; then
  fail "the installed .env still holds an Envoy webhook secret"
fi
# Without a dispatch-token in the bundle, the .env gives the meeting voice no Dispatch token.
dispatch_token_name="DISPATCH""_TOKEN"
if grep -q "^GHOST_WISPR_$dispatch_token_name=" "$env_file"; then
  fail "the installed .env has a Dispatch token the bundle did not carry"
fi

# The unit's own Dispatch token, when the bundle carries one, reaches the .env for the Concierge,
# and a token that is not one line is refused. Each run starts a fresh log, as a first run does.
: >"$TEST_LOG"
printf 'dispatch-token-value' >"$work_dir/bundle/dispatch-token"
run_provision "${bundle_files[@]}" dispatch-token || fail "provision.sh failed with a dispatch-token: $(<"$work_dir/out")"
grep -Fxq "GHOST_WISPR_$dispatch_token_name=dispatch-token-value" "$TEST_LOG.installed-.env" \
  || fail "the installed .env has no Dispatch token from the bundle's dispatch-token"
printf 'two\nlines' >"$work_dir/bundle/dispatch-token"
if run_provision "${bundle_files[@]}" dispatch-token; then
  fail "provision.sh accepted a dispatch-token of two lines"
fi
rm "$work_dir/bundle/dispatch-token"
# A dispatch-token that is a link to nothing is refused, not skipped as absent.
ln -s "$work_dir/no-such-token" "$work_dir/bundle/dispatch-token"
if run_provision "${bundle_files[@]}" dispatch-token; then
  fail "provision.sh accepted a dispatch-token that links to nothing"
fi
grep -q 'dispatch-token' "$work_dir/out" || fail "provision.sh refused the linked dispatch-token for another reason: $(<"$work_dir/out")"
rm "$work_dir/bundle/dispatch-token"

# A lingering user service has no seat, so only the audio group opens the ALSA devices.
grep -Fxq 'sudo usermod -aG audio ghost-wispr' "$TEST_LOG" || fail "provision.sh does not add ghost-wispr to the audio group"
if grep -Eq '^sudo usermod -aG (plugdev|input) ' "$TEST_LOG"; then
  fail "provision.sh re-added a group ghost-wispr already has"
fi
# update.sh runs systemctl --user, which needs the user's runtime directory.
grep -Fxq "update.sh XDG_RUNTIME_DIR=$runtime_dir" "$TEST_LOG" || fail "update.sh ran without XDG_RUNTIME_DIR=$runtime_dir"
[[ -s $TEST_LOG.join ]] || fail "provision.sh started no detached join"

# The service starts only once the Pi's clock is synchronized, so no session is named or stamped
# with the time a Pi with no battery-backed clock restores at boot. A user unit cannot be ordered
# after the system's time-sync.target, so the unit waits in a start step: provision.sh installs it
# beside the binary, the unit runs it after the network wait and before Ghost Wispr, and the unit's
# start time limit is longer than both waits together, so a Pi whose clock never synchronizes still
# starts once the wait gives up instead of being killed and restarted for good.
grep -Fxq "sudo install -o ghost-wispr -g ghost-wispr -m 0755 $repo_root/deploy/pi/files/wait-for-time-sync.sh $work_dir/deploy/wait-for-time-sync.sh" "$TEST_LOG" \
  || fail "provision.sh does not install wait-for-time-sync.sh beside the binary"
unit="$repo_root/deploy/pi/files/ghost-wispr.service"
mapfile -t start_steps < <(grep -E '^Exec(StartPre|Start)=' "$unit")
want_steps=(ExecStartPre=/opt/ghost-wispr/wait-for-network.sh ExecStartPre=/opt/ghost-wispr/wait-for-time-sync.sh
  ExecStart=/opt/ghost-wispr/ghost-wispr)
[[ "${start_steps[*]}" == "${want_steps[*]}" ]] \
  || fail "ghost-wispr.service starts with [${start_steps[*]}], want [${want_steps[*]}]"
network_wait=$(sed -n 's/.*GHOST_WISPR_NETWAIT_TIMEOUT:-\([0-9][0-9]*\).*/\1/p' "$repo_root/wait-for-network.sh")
sync_wait=$(sed -n 's/.*GHOST_WISPR_TIMESYNC_TIMEOUT:-\([0-9][0-9]*\).*/\1/p' "$repo_root/deploy/pi/files/wait-for-time-sync.sh")
[[ -n $network_wait && -n $sync_wait ]] || fail "cannot read the network wait ($network_wait) or the clock wait ($sync_wait)"
start_limit=$(sed -n 's/^TimeoutStartSec=//p' "$unit")
case $start_limit in
  *min) start_limit=$((${start_limit%min} * 60)) ;;
  *s) start_limit=${start_limit%s} ;;
  '') fail "ghost-wispr.service sets no TimeoutStartSec: the user manager's 90 s default is shorter than its waits" ;;
esac
# The network wait's last try can run 5 s past its limit (a 3 s connect, then a 2 s sleep).
((start_limit >= network_wait + 5 + sync_wait + 30)) \
  || fail "ghost-wispr.service's TimeoutStartSec (${start_limit}s) does not cover the network wait (${network_wait}s) and the clock wait (${sync_wait}s)"

# The join re-authenticates the node; Serve is set again after it, in the same unit.
run_join() {
  : >"$TEST_LOG"
  PATH="$work_dir/bin:$PATH" /bin/sh -c "$(<"$TEST_LOG.join")"
}
run_join || fail "the join command failed with tailscale up succeeding"
mapfile -t calls <"$TEST_LOG"
[[ ${#calls[@]} == 2 && ${calls[0]} == 'tailscale up '* && ${calls[1]} == 'tailscale serve --bg --https 443 http://127.0.0.1:8080' ]] \
  || fail "the join did not set Serve after tailscale up: ${calls[*]}"
if TEST_UP_EXIT=1 run_join; then
  fail "the join succeeded although tailscale up failed"
fi
grep -q '^tailscale serve' "$TEST_LOG" && fail "the join set Serve although tailscale up failed"

# A rerun on a Pi that has a shared drive renders through render_models_config, which takes the
# webhook URL as an argument too: it must keep the drive and say no bash error either.
printf 'gdrive_folder_id: "drive-0123"\n' >"$work_dir/deploy/ghost-wispr.yaml"
: >"$TEST_LOG"
run_provision "${bundle_files[@]}" || fail "provision.sh failed on a rerun: $(<"$work_dir/out")"
if grep -E 'provision\.sh: line [0-9]+: ' "$work_dir/out" >&2; then
  fail "provision.sh printed a bash error on a rerun with a shared drive (above)"
fi
for line in 'gdrive_folder_id: "drive-0123"' 'gdrive_sync_enabled: true' \
  'envoy_webhook_url: https://relay.example.test/webhook/ghostwispr'; do
  grep -Fxq "$line" "$TEST_LOG.installed-ghost-wispr.yaml" \
    || fail "the rerun's configuration lacks '$line': $(<"$TEST_LOG.installed-ghost-wispr.yaml")"
done
assert_recording_rules "rerun's"
assert_office_pi_devices "rerun's"
# The recorder reads its configuration only when it starts, and update.sh restarts nothing when the
# Pi already runs the newest release: a rerun restarts a running recorder once the configuration is
# installed, and try-restart starts none that is stopped.
# grep finding no line is a failure this check reports, not one that stops the test silently.
config_at=$(grep -n -E '^sudo install .*/ghost-wispr\.yaml$' "$TEST_LOG" | cut -d: -f1 | head -n 1) || true
restart_at=$(grep -n -F -x -- "sudo -u ghost-wispr XDG_RUNTIME_DIR=$runtime_dir systemctl --user try-restart ghost-wispr.service" \
  "$TEST_LOG" | cut -d: -f1 | head -n 1) || true
[[ -n $config_at && -n $restart_at && $restart_at -gt $config_at ]] \
  || fail "the rerun did not restart a running recorder after it installed the configuration: $(<"$TEST_LOG")"
printf 'provision run test passed\n'
