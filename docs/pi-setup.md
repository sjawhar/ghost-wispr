# Office recorder Pi runbook

This runbook installs the Ghost Wispr office recorder on the Raspberry Pi 5 named
`ghost-wispr`. The finished card has Raspberry Pi OS Lite, Tailscale, the Ghost Wispr
release binary, and its runtime packages. It does not keep a source checkout, Go, or Node.js.

The recorder is reachable only through Tailscale Serve at
`https://ghost-wispr.tail12ec1c.ts.net`. Its application process listens on
`127.0.0.1:8080`.

## Prerequisites and release order

First flash and boot the Pi as described below, then create its key and certificate-signing request.
The `bootstrap` identity apply signs that request, so it cannot run before those two steps.

After the certificate-signing request exists, do these before provisioning:

1. Apply the approved `agent-infra` changes for the Google X.509 identity, the
   `tag:ghost-wispr` Tailscale identity, and the Envoy webhook. Record:
   - the `ghostWisprCertificates` output from `bootstrap`;
   - the service account unique ID from
     `gcloud iam service-accounts describe ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com --format='value(uniqueId)'`;
   - the Ghost Wispr Tailscale client ID from `root`'s `tailscaleIds` output;
   - the `relayUrl` output from `envoy-cluster`.
2. Ensure Secret Manager has `envoy-ghostwispr-signing-secret` and the operator can read it.
3. Wait for the Ghost Wispr release workflow to publish `ghost-wispr-linux-arm64` and
   `ghost-wispr-linux-arm64.sha256`.

Do not provision before these are ready. Provisioning installs the release binary and joins
Tailscale with the Pi's Google identity; it cannot make either dependency itself.

## Flash the card

Flash Raspberry Pi OS Lite (64-bit, Trixie) to the card. The image used for this recorder was
`2026-10-06-raspios-trixie-arm64-lite.img.xz`, SHA-256
`483db18a48da399b5b7022ffae9a07bc6d99daab30e593aedac09b69ff422843`.

Before the first boot, place the cloud-init files on the boot partition as
`/boot/firmware/user-data`, `/boot/firmware/meta-data`, and
`/boot/firmware/network-config`. These are the first-boot files on the recorder's card. Give
cloud-init the hostname `ghost-wispr`, a passwordless `ghost-wispr` user with passwordless
sudo, the approved operator SSH public keys, and the `Theorem` WPA2 network. Keep the Wi-Fi
password and private key material out of version control.

```yaml
# meta-data
instance-id: ghost-wispr
local-hostname: ghost-wispr
```

```yaml
# network-config
version: 2
wifis:
  wlan0:
    dhcp4: true
    access-points:
      Theorem:
        password: "<Theorem WPA2 password>"
```

The `user-data` file must create `ghost-wispr`, add it to `sudo`, set `lock_passwd: true`,
and contain the operator's public SSH keys. Boot the Pi, confirm it joins `Theorem`, and
connect as `ghost-wispr` over SSH.

Install Tailscale through its normal package route if it is not already installed. The
recorder's tested baseline has Tailscale 1.104.6. Join the tailnet temporarily as the
operator's own device with Tailscale SSH off (`sudo tailscale up`) so the Pi remains reachable
at `ghost-wispr.tail12ec1c.ts.net` off the office network. Provisioning replaces this
temporary join with `tag:ghost-wispr`.

## Create the Pi key and Google credential files

The key is generated on the Pi and never leaves it. Run this once before the first
`bootstrap` identity apply:

```bash
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  'umask 077; mkdir -p ~/.config/ghost-wispr-identity && openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout ~/.config/ghost-wispr-identity/key.pem -subj /CN=ghost-wispr'
```

Give the resulting certificate-signing request to the `bootstrap` apply. After the apply,
create the Google external-account credential files on a development machine, never on the Pi:

```bash
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
```

`device.crt`, `google-credentials.json`, and `certificate_config.json` are not secrets. The
private key stays at `/home/ghost-wispr/.config/ghost-wispr-identity/key.pem`. Do not set
`GOOGLE_API_CERTIFICATE_CONFIG`: the certificate is for the Security Token Service exchange,
not Google API mTLS.
Ghost Wispr reads this credential through
`google_credentials_file: /opt/ghost-wispr/google-credentials.json` in its YAML. The tailnet
join passes the same path with `ghost-wispr google-id-token --credentials`; it does not set
`GOOGLE_APPLICATION_CREDENTIALS`.


## Renew the X.509 certificate

Ghost Wispr warns on its status page when the certificate has fewer than 30 days left. The
certificate is renewed by the `bootstrap` apply, which issues a new value through
`ghostWisprCertificates`; the Pi private key stays where it is. Apply `bootstrap` through its
normal reviewed CI route, then install the refreshed public certificate and restart Ghost Wispr:

```bash
cd ~/src/agent-infra/bootstrap
pulumi stack output --stack prod ghostWisprCertificates --json | jq -r '."ghost-wispr"' \
  | ssh ghost-wispr@ghost-wispr 'sudo install -o ghost-wispr -g ghost-wispr -m 0644 /dev/stdin /opt/ghost-wispr/device.crt && XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user restart ghost-wispr.service'
```

Confirm that `https://ghost-wispr.tail12ec1c.ts.net/api/status` no longer reports the
certificate-expiry warning. Do not regenerate the Pi key or the Google credential configuration
for a normal certificate renewal.

## Build the private credential bundle

Create a mode-0700 directory on the operator machine. It exists only long enough to make the
tar stream.

```bash
bundle_root="$HOME/.local/share/ghost-wispr-provision"
mkdir -p -m 700 "$bundle_root"
bundle_dir=$(mktemp -d "$bundle_root/bundle.XXXXXX")
trap 'rm -rf -- "$bundle_dir"' EXIT
```

Put these files in it:

| File | Source |
| --- | --- |
| `device.crt` | `bootstrap` stack output `ghostWisprCertificates` |
| `google-credentials.json` and `certificate_config.json` | the rewritten files in `$d` from the `gcloud ... create-cred-config` command above |
| `tailscale-client-id` | Ghost Wispr's Tailscale client ID from `root` output `tailscaleIds` |
| `tailscale-subject` | `gcloud iam service-accounts describe ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com --format='value(uniqueId)'` |
| `envoy-webhook-url` | `$(pulumi stack output relayUrl --stack prod)` from `envoy-cluster`, followed by `/webhook/ghostwispr` |
| `envoy-webhook-secret` | `gcloud secrets versions access latest --secret=envoy-ghostwispr-signing-secret --project=theorem-agent-infra` |
| `deepgram-key` | `DEEPGRAM_API_KEY` from the operator's secrets broker |

```bash
(cd ~/src/agent-infra/bootstrap && pulumi stack output --stack prod ghostWisprCertificates --json | jq -r '."ghost-wispr"') >"$bundle_dir/device.crt"
cp "$d/certificate_config.json" "$d/google-credentials.json" "$bundle_dir/"
(cd ~/src/agent-infra/root && pulumi config get --stack prod --path 'tailscaleIds["ghost-wispr-x509"]') >"$bundle_dir/tailscale-client-id"
(cd ~/src/agent-infra/envoy-cluster && printf '%s/webhook/ghostwispr' "$(pulumi stack output relayUrl --stack prod)") >"$bundle_dir/envoy-webhook-url"
gcloud secrets versions access latest --secret=envoy-ghostwispr-signing-secret --project=theorem-agent-infra >"$bundle_dir/envoy-webhook-secret"
gcloud iam service-accounts describe ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com --format='value(uniqueId)' >"$bundle_dir/tailscale-subject"
rm -rf -- "$d"
```

The two secret files stay only in this private directory. Do not put either secret in an
argument, commit, or persistent file. For Deepgram, inject the broker secret into a process
that writes the private bundle file; do not print it:

```bash
secrets DEEPGRAM_API_KEY -- sh -c 'printf %s "$DEEPGRAM_API_KEY" > "$1/deepgram-key"' sh "$bundle_dir"
```

## Provision

Provisioning runs as `ghost-wispr` with passwordless sudo. It installs `libportaudio2`,
`ffmpeg`, `lame`, `jq`, and `curl`; copies the release runtime to `/opt/ghost-wispr`; installs
the user services; verifies and installs the newest release; enables Tailscale Serve; and then
starts the detached tagged-device join.

Copy only the provisioning sources to a temporary directory on the Pi. The directory is
removed after provisioning, so the card keeps no source checkout.

```bash
remote_dir=$(ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  'umask 077; mkdir -p ~/.local/share/ghost-wispr-provision; chmod 700 ~/.local/share/ghost-wispr-provision; mktemp -d ~/.local/share/ghost-wispr-provision/sources.XXXXXX')
tar -C . -cf - deploy/pi wait-for-network.sh \
  | ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "tar -xf - -C '$remote_dir'"
tar -C "$bundle_dir" -cf - \
  device.crt google-credentials.json certificate_config.json \
  tailscale-client-id tailscale-subject envoy-webhook-url envoy-webhook-secret deepgram-key \
  | ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "$remote_dir/deploy/pi/provision.sh"
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "rm -rf -- '$remote_dir'"
```

`provision.sh` can end the SSH session while Tailscale re-authenticates. If the final cleanup
command fails, reconnect with Tailscale SSH and run it again. Then check the detached join:

```bash
ssh ghost-wispr@ghost-wispr 'tailscale status --self --json'
ssh ghost-wispr@ghost-wispr 'journalctl -u "ghost-wispr-tailnet-join-*"'
```

The status must show exactly `Tags: ["tag:ghost-wispr"]` and
`DNSName: "ghost-wispr.tail12ec1c.ts.net."`. If Tailscale names it `ghost-wispr-1`, remove
the stale Tailscale device and provision again.

## Restrict OpenSSH to loopback

Only after Tailscale SSH works, restrict OpenSSH. Raspberry Pi OS Trixie can activate OpenSSH
through `ssh.socket`; disabling it and enabling `ssh.service` makes the `sshd_config` listen
address effective.

```bash
ssh ghost-wispr@ghost-wispr '
  printf "ListenAddress 127.0.0.1\n" | sudo install -m 0644 /dev/stdin /etc/ssh/sshd_config.d/50-ghost-wispr.conf &&
  sudo sshd -t &&
  sudo systemctl disable --now ssh.socket &&
  sudo systemctl enable --now ssh.service &&
  sudo systemctl restart ssh.service &&
  listeners=$(sudo ss -ltnpH | grep "sshd") &&
  test -n "$listeners" &&
  ! printf "%s\n" "$listeners" | grep -Fv "127.0.0.1:22"
'
```

That command fails if `sshd` has no listener or any listener other than `127.0.0.1:22`.
Reconnect over Tailscale SSH once more after it succeeds.

## Update and inspect

The user timer runs `/opt/ghost-wispr/update.sh` every day at 03:30 local time. It does
nothing while `/api/status` reports an active session. It verifies the GitHub release checksum,
keeps `/opt/ghost-wispr/ghost-wispr.prev`, and restores that binary if the new process or
`/api/version` check fails.

Run an update manually:

```bash
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) /opt/ghost-wispr/update.sh'
```

Inspect the runtime:

```bash
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user status ghost-wispr.service'
ssh ghost-wispr@ghost-wispr 'curl -fsS http://127.0.0.1:8080/api/version'
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user list-timers ghost-wispr-update.timer'
```

## Recover a pending segment deletion

If `/api/status` names a recording with a pending segment deletion, do not issue another
delete for that conversation. First restore Drive connectivity and confirm the session
folder still contains its recordings. Then restart Ghost Wispr:

```bash
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user restart ghost-wispr.service'
```

The startup recovery verifies the recording hash, completes the splice, and reruns
refinement before it clears the warning for that session and recording. If the warning
names failed refinement, audio recovery is complete: repair batch transcription and rerun
refinement. Otherwise, preserve the named recording and contact the recorder operator; do
not remove any database row or audio file by hand.

## Accept slice 1

Run the acceptance driver from a tailnet member. Give it the relay URL. To run the signed
Envoy control, inject the signing secret only for this process.

```bash
GHOST_WISPR_RELAY_URL="$(cd ~/src/agent-infra/envoy-cluster && pulumi stack output relayUrl --stack prod)/webhook/ghostwispr" \
GHOST_WISPR_ENVOY_WEBHOOK_SECRET="$(gcloud secrets versions access latest --secret=envoy-ghostwispr-signing-secret --project=theorem-agent-infra)" \
deploy/pi/accept-slice1.sh
```

The driver pauses and resumes the recorder, asks the operator to power-cycle it, checks the
light state, verifies the tagged identity and loopback listeners, and tests the signed and
unsigned webhook paths. Before the pause it requires the Ghost Wispr Deepgram socket to grow by
at least 32 kB/s; during the 60-second pause it permits at most 4096 bytes. It compares package
and running-service inventories against `deploy/pi/baseline.txt`, allowing only
`libportaudio2`, `ffmpeg`, `lame`, `jq`, their package dependencies, and Ghost Wispr's user
services. An empty inventory is UNKNOWN, not a pass. While it runs, a member device must also
subscribe with `nats sub 'notifications.ghostwispr.>' --server nats://envoy-hub.tail12ec1c.ts.net:4222`
and confirm the signed probe plus a real
`session.started` and `session.ended` notification.
