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
  status) printf '{"Self":{"Tags":null,"DNSName":"ghost-wispr.tail12ec1c.ts.net."}}\n' ;;
  up) exit "${TEST_UP_EXIT:-0}" ;;
esac
EOF

# The release's update.sh and binary, as provision.sh runs them from the deploy directory.
cat >"$work_dir/deploy/update.sh" <<'EOF'
#!/usr/bin/env bash
printf 'update.sh XDG_RUNTIME_DIR=%s\n' "${XDG_RUNTIME_DIR:-}" >>"$TEST_LOG"
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

# run_provision runs provision.sh on a bundle of the named files.
run_provision() {
  tar -C "$work_dir/bundle" -cf - "$@" \
    | env -u XDG_RUNTIME_DIR PATH="$work_dir/bin:$PATH" GHOST_WISPR_DEPLOY_DIR="$work_dir/deploy" \
      GHOST_WISPR_IDENTITY_KEY="$work_dir/key.pem" bash "$repo_root/deploy/pi/provision.sh" >"$work_dir/out" 2>&1
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

# The .env the recorder runs with holds the Deepgram key and no Envoy signing key.
env_file="$TEST_LOG.installed-.env"
deepgram_key_name="DEEPGRAM""_API_KEY"
grep -Fxq "GHOST_WISPR_$deepgram_key_name=deepgram-key" "$env_file" || fail "the installed .env has no Deepgram key"
if grep -q 'ENVOY_WEBHOOK_SECRET' "$env_file"; then
  fail "the installed .env still holds an Envoy webhook secret"
fi

# A lingering user service has no seat, so only the audio group opens the ALSA devices.
grep -Fxq 'sudo usermod -aG audio ghost-wispr' "$TEST_LOG" || fail "provision.sh does not add ghost-wispr to the audio group"
if grep -Eq '^sudo usermod -aG (plugdev|input) ' "$TEST_LOG"; then
  fail "provision.sh re-added a group ghost-wispr already has"
fi
# update.sh runs systemctl --user, which needs the user's runtime directory.
grep -Fxq "update.sh XDG_RUNTIME_DIR=$runtime_dir" "$TEST_LOG" || fail "update.sh ran without XDG_RUNTIME_DIR=$runtime_dir"
[[ -s $TEST_LOG.join ]] || fail "provision.sh started no detached join"

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
run_provision "${bundle_files[@]}" || fail "provision.sh failed on a rerun: $(<"$work_dir/out")"
if grep -E 'provision\.sh: line [0-9]+: ' "$work_dir/out" >&2; then
  fail "provision.sh printed a bash error on a rerun with a shared drive (above)"
fi
for line in 'gdrive_folder_id: "drive-0123"' 'gdrive_sync_enabled: true' \
  'envoy_webhook_url: https://relay.example.test/webhook/ghostwispr'; do
  grep -Fxq "$line" "$TEST_LOG.installed-ghost-wispr.yaml" \
    || fail "the rerun's configuration lacks '$line': $(<"$TEST_LOG.installed-ghost-wispr.yaml")"
done
printf 'provision run test passed\n'
