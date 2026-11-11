#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT
readonly HOME="$work_dir/home"
readonly TEST_REMOTE_DIR="$work_dir/remote"
readonly TEST_BUNDLE_LIST="$work_dir/bundle.list"
export HOME TEST_REMOTE_DIR TEST_BUNDLE_LIST
mkdir -p "$HOME/src/agent-infra/bootstrap" "$HOME/src/agent-infra/root" "$HOME/src/agent-infra/envoy-cluster" "$work_dir/bin" "$TEST_REMOTE_DIR"

cat >"$work_dir/bin/pulumi" <<'EOF'
#!/usr/bin/env bash
case "$PWD" in
  */bootstrap) printf '{"ghost-wispr":"CERTIFICATE"}\n' ;;
  */root) printf '{"ghost-wispr-x509":"tailscale-client-id"}\n' ;;
  */envoy-cluster) printf 'https://relay.example.test\n' ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$work_dir/bin/pulumi"

cat >"$work_dir/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
certificate_config=
credentials=
for argument in "$@"; do
  case "$argument" in
    --credential-cert-configuration-output-file=*) certificate_config=${argument#*=} ;;
    --output-file=*) credentials=${argument#*=} ;;
  esac
done
if [[ -n "$certificate_config" && -n "$credentials" ]]; then
  printf '{"certificate":"config"}\n' >"$certificate_config"
  printf '{"credential_source":{"certificate":{"certificate_config_location":"relative.json"}}}\n' >"$credentials"
  exit 0
fi
case " $* " in
  *' secrets versions access '*) printf 'envoy-secret' ;;
  *' iam service-accounts describe '*) printf '1234567890\n' ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$work_dir/bin/gcloud"

cat >"$work_dir/bin/secrets" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ $1 == "${TEST_DEEPGRAM_KEY:?}" && $2 == -- ]]
shift 2
env "${TEST_DEEPGRAM_KEY}=deepgram-secret" "$@"
EOF
chmod +x "$work_dir/bin/secrets"

cat >"$work_dir/bin/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
shift
command=$1
case "$command" in
  *'mktemp -d'*) printf '%s\n' "$TEST_REMOTE_DIR" ;;
  *'tar -xf - -C'*) tar -xf - -C "$TEST_REMOTE_DIR" ;;
  *'/deploy/pi/provision.sh'*) tar -tf - | LC_ALL=C sort >"$TEST_BUNDLE_LIST" ;;
  *'rm -rf'*) : ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$work_dir/bin/ssh"

export PATH="$work_dir/bin:$PATH"
deepgram_key_name="DEEPGRAM""_API_KEY"
export TEST_DEEPGRAM_KEY="$deepgram_key_name"

d=$(mktemp -d)
gcloud iam workload-identity-pools create-cred-config \
  projects/805240912354/locations/global/workloadIdentityPools/ghost-wispr/providers/ghost-wispr \
  --service-account=ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com \
  --credential-cert-path=/opt/ghost-wispr/device.crt \
  --credential-cert-private-key-path=/home/ghost-wispr/.config/ghost-wispr-identity/key.pem \
  --credential-cert-configuration-output-file="$d/certificate_config.json" \
  --output-file="$d/google-credentials.json"
jq --arg path /opt/ghost-wispr/certificate_config.json \
  '.credential_source.certificate.certificate_config_location = $path' \
  "$d/google-credentials.json" >"$d/google-credentials.json.tmp"
mv "$d/google-credentials.json.tmp" "$d/google-credentials.json"

bundle_root="$HOME/.local/share/ghost-wispr-provision"
mkdir -p "$bundle_root"
chmod 700 "$bundle_root"
bundle_dir=$(mktemp -d "$bundle_root/bundle.XXXXXX")
(cd ~/src/agent-infra/bootstrap && pulumi stack output --stack prod ghostWisprCertificates --json | jq -r '."ghost-wispr"') >"$bundle_dir/device.crt"
cp "$d/certificate_config.json" "$d/google-credentials.json" "$bundle_dir/"
(cd ~/src/agent-infra/root && pulumi stack output tailscaleIds --stack prod --json | jq -r '."ghost-wispr-x509"') >"$bundle_dir/tailscale-client-id"
(cd ~/src/agent-infra/envoy-cluster && printf '%s/webhook/ghostwispr' "$(pulumi stack output relayUrl --stack prod)") >"$bundle_dir/envoy-webhook-url"
gcloud secrets versions access latest --secret=envoy-ghostwispr-signing-secret --project=theorem-agent-infra >"$bundle_dir/envoy-webhook-secret"
gcloud iam service-accounts describe ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com --format='value(uniqueId)' >"$bundle_dir/tailscale-subject"
rm -rf -- "$d"
# shellcheck disable=SC2016 # The child shell reads its dynamic environment variable.
secrets "$deepgram_key_name" -- sh -c 'printenv "$1" > "$2/deepgram-key"' sh "$deepgram_key_name" "$bundle_dir"

cd "$repo_root"
remote_dir=$(ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  'umask 077; mkdir -p ~/.local/share/ghost-wispr-provision; chmod 700 ~/.local/share/ghost-wispr-provision; mktemp -d ~/.local/share/ghost-wispr-provision/sources.XXXXXX')
# shellcheck disable=SC2029 # The stub intentionally receives the locally expanded path.
tar -C . -cf - deploy/pi wait-for-network.sh \
  | ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "tar -xf - -C '$remote_dir'"
# shellcheck disable=SC2029 # The stub intentionally receives the locally expanded path.
tar -C "$bundle_dir" -cf - \
  device.crt google-credentials.json certificate_config.json \
  tailscale-client-id tailscale-subject envoy-webhook-url envoy-webhook-secret deepgram-key \
  | ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "$remote_dir/deploy/pi/provision.sh"
# shellcheck disable=SC2029 # The stub intentionally receives the locally expanded path.
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "rm -rf -- '$remote_dir'"

printf '%s\n' device.crt google-credentials.json certificate_config.json tailscale-client-id tailscale-subject envoy-webhook-url envoy-webhook-secret deepgram-key \
  | LC_ALL=C sort >"$work_dir/expected.list"
cmp "$work_dir/expected.list" "$TEST_BUNDLE_LIST"
printf 'provision bundle test passed\n'
