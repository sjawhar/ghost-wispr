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
   `tag:ghost-wispr` Tailscale identity, and the Envoy webhook. Each value the bundle needs is
   then on `agent-infra` `main`:
   - the device certificate: the `bootstrap` apply publishes it as the latest version of Secret
     Manager secret `ghost-wispr-device-certificate` (`bootstrap/README.md`);
   - the Ghost Wispr Tailscale client ID: `root` creates the Tailscale identity `ghost-wispr-x509`
     and prints its client ID as `ghostWisprTailnetClientId` in the log of the `seed` workflow's
     `root` job that creates it. `tailscaleIds` has no entry for it;
   - the service account's unique ID: `ghostWisprAccountUniqueId` in `root`'s prod
     configuration (`root/Pulumi.prod.yaml`);
   - the relay URL: the URL of `envoy-cluster`'s webhook relay, the Cloud Run service
     `envoy-webhook-relay-*` in `theorem-agent-infra`.
2. Ensure Secret Manager has `envoy-ghostwispr-signing-secret` and that `ghost-wispr@` has
   Secret Accessor on it (dispatch://INFRA-3599). Ghost Wispr reads the key itself at runtime,
   with the same Google identity it uses for Drive and Gemini, so no person or bundle carries
   it to the Pi. Box identities cannot read it: a process that holds the secret can sign events
   that Envoy accepts as the recorder's.
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

<!-- provision_bundle_test.sh: credentials -->
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

Ghost Wispr warns on its status page when the certificate has fewer than 30 days left, or when it
cannot read the certificate. The `bootstrap` apply renews the certificate and publishes it as the
latest version of Secret Manager secret `ghost-wispr-device-certificate`; the Pi private key stays
where it is. Apply `bootstrap` through its normal reviewed CI route. Then read the new certificate
on a development machine whose `gcloud` is the devbox identity (the reader `bootstrap/README.md`
names), check it, install it on the Pi and restart Ghost Wispr:

<!-- provision_bundle_test.sh: renew -->
```bash
crt=$(mktemp) &&
gcloud secrets versions access latest --secret=ghost-wispr-device-certificate --project=theorem-agent-infra >"$crt" &&
openssl x509 -in "$crt" -noout -subject -enddate &&
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  'sudo install -o ghost-wispr -g ghost-wispr -m 0644 /dev/stdin /opt/ghost-wispr/device.crt && XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user restart ghost-wispr.service' <"$crt"
rm -f -- "$crt"
```

`openssl` must print `subject=CN=ghost-wispr` and the new end date. The certificate is not a
secret: if this machine cannot reach the Pi over SSH, copy `$crt` to one that can and run the
`ssh` line there. Confirm that `https://ghost-wispr.tail12ec1c.ts.net/api/status` no longer
reports the certificate warning. Do not regenerate the Pi key or the Google credential
configuration for a normal certificate renewal.

## Prepare the bundle's public files

The credential bundle holds six public files and one secret:

| File | Source |
| --- | --- |
| `device.crt` | the latest version of Secret Manager secret `ghost-wispr-device-certificate` |
| `google-credentials.json` and `certificate_config.json` | the rewritten files in `$d` from the `gcloud ... create-cred-config` command above |
| `tailscale-client-id` | `ghostWisprTailnetClientId`, which `agent-infra`'s `seed` workflow printed in the log of the `root` job that created the Tailscale identity `ghost-wispr-x509` |
| `tailscale-subject` | `ghostWisprAccountUniqueId` in `root/Pulumi.prod.yaml` on `agent-infra` `main` |
| `envoy-webhook-url` | the URL of the Cloud Run service `envoy-webhook-relay-*` in `theorem-agent-infra` (`envoy-cluster`'s relay), followed by `/webhook/ghostwispr` |
| `deepgram-key` | `DEEPGRAM_API_KEY` from the operator's `secrets` store |

The Envoy webhook signing key is not in the bundle, and `provision.sh` refuses a bundle that
holds an `envoy-webhook-secret` file. Ghost Wispr reads the key from Secret Manager
(`envoy_webhook_secret_name`, by default
`projects/theorem-agent-infra/secrets/envoy-ghostwispr-signing-secret/versions/latest`) and keeps
it in memory. When Envoy answers 401 it reads the key again once, so a rotation needs no change on
the Pi. When it cannot read the key it sends no events and `/api/status` says so; recording goes on.

Make the six public files on the development machine where you ran `create-cred-config`, from a
Ghost Wispr checkout. Its `gcloud` must be the devbox identity, its `gh` must read
`theorem-labs/agent-infra`'s Actions logs, and it needs `agent-infra` checked out at
`~/src/agent-infra`. The block reads no Pulumi state: the devbox identity cannot list the `root`
and `envoy-cluster` state buckets (403). Each value comes from a source that identity reads, at
the time you run the block:

- `tailscale-subject` from `root/Pulumi.prod.yaml` on `agent-infra`'s `origin/main`.
- `tailscale-client-id` from a `seed` run's log, with `deploy/pi/tailnet-client-id.sh`. It reads
  the `seed` workflow's runs from the newest, and every attempt of a re-run run, and takes the
  first `root` job whose log says that `ghost-wispr-x509` was created or replaced, so the newest
  creation wins. It prints that job's link. It stops with an error, and writes no ID, when a
  `root` job that ran is still running or its log cannot be read, because that job can hold a
  newer ID; run it again. It reads the log with `gh api .../actions/jobs/<id>/logs`, because
  `gh run view --job <id> --log` refuses while another job of the same run still waits for its
  approval. It reads the runs of the last 90 days: `agent-infra` keeps run logs for 90 days
  (`root/retention.json`). It makes one API call for each run it reads, so it takes about 15
  minutes for each 1000 runs before the creation. When no run of the last 90 days made the
  identity, it says so; then read the ID from `/opt/ghost-wispr/tailscale-client-id` on a Pi
  provisioned before, or ask someone who can read `root`'s Pulumi state for
  `pulumi stack output ghostWisprTailnetClientId --stack prod`.
- `envoy-webhook-url` from the Cloud Run service list: `envoy-cluster` gives the relay's service
  name a suffix, so the block matches its prefix `envoy-webhook-relay-`.

<!-- provision_bundle_test.sh: prepare -->
```bash
prepared=$(mktemp -d)
gcloud secrets versions access latest --secret=ghost-wispr-device-certificate --project=theorem-agent-infra >"$prepared/device.crt"
openssl x509 -in "$prepared/device.crt" -noout -subject -enddate
cp "$d/certificate_config.json" "$d/google-credentials.json" "$prepared/"
git -C ~/src/agent-infra fetch --quiet origin main
git -C ~/src/agent-infra show origin/main:root/Pulumi.prod.yaml \
  | sed -n 's/^  agent-infra-root:ghostWisprAccountUniqueId: "\{0,1\}\([0-9][0-9]*\)"\{0,1\}$/\1/p' >"$prepared/tailscale-subject"
deploy/pi/tailnet-client-id.sh >"$prepared/tailscale-client-id"
relay=$(gcloud run services list --project=theorem-agent-infra --filter='metadata.name ~ ^envoy-webhook-relay-' --format='value(status.url)')
printf '%s/webhook/ghostwispr\n' "$relay" >"$prepared/envoy-webhook-url"
head "$prepared/tailscale-subject" "$prepared/tailscale-client-id" "$prepared/envoy-webhook-url"
rm -rf -- "$d"
```

`openssl` must print `subject=CN=ghost-wispr`, and `head` one line for each of the three files:
the subject is digits, the client ID one token, and the URL an `https://...run.app` address
ending in `/webhook/ghostwispr`. `provision-pi.sh` refuses an empty file, and `provision.sh` a
file of another form. None of these files is a secret: if another machine provisions the Pi, copy
`$prepared` there.

## Provision

`deploy/pi/provision-pi.sh` reads the Deepgram key, makes the bundle and runs
`deploy/pi/provision.sh` on the Pi. Run it from a checkout of the release's commit, on the
machine that reaches the Pi over SSH.

- It reads `DEEPGRAM_API_KEY` with `secrets` on the provisioning machine.
- The key reaches disk only in a mode-0700 directory under `$XDG_RUNTIME_DIR` while it makes
  the tar stream. It removes that directory as soon as `provision.sh` returns, also when
  `provision.sh` fails, and on any exit. No secret goes in an argument.
- It copies only the provisioning sources to a temporary directory on the Pi and removes it
  afterwards, so the card keeps no source checkout.

`provision.sh` runs as `ghost-wispr` with passwordless sudo. It installs `libportaudio2`,
`ffmpeg`, `lame`, `jq`, and `curl`; copies the release runtime to `/opt/ghost-wispr`; installs
the user services; verifies and installs the newest release; enables Tailscale Serve; and then
starts the detached tagged-device join.

Then run:

<!-- provision_bundle_test.sh: provision -->
```bash
deploy/pi/provision-pi.sh --prepared "$prepared"
```

`provision.sh` can end the SSH session while Tailscale re-authenticates; the script then says
so (exit 255).

The tagged join makes a new Tailscale device. The temporary device you joined as your own still
holds the name `ghost-wispr`, so Tailscale names the tagged one `ghost-wispr-1`, and
`ghost-wispr.tail12ec1c.ts.net` does not reach the recorder. Provisioning again does not fix
that: `provision-pi.sh` reaches the Pi by that name. Give the tagged device its name with
`deploy/pi/claim-tailnet-name.sh`, under a secrets-broker grant of `TAILSCALE_API_KEY` that Sami
approves on the broker page:

```bash
secret-run --register --wait 12h \
  --reason 'Ghost Wispr (docs/pi-setup.md): give the tagged recorder the tailnet name ghost-wispr' \
  TAILSCALE_API_KEY -- deploy/pi/claim-tailnet-name.sh
```

It removes the temporary device only when that device is the one device other than the tagged
one that holds the name, has an id that is a string of digits, has the hostname `ghost-wispr`,
has a user owner and no tag, and is offline. Then it renames the tagged device and reads the name
back. It refuses any other holder, or more than one, before it changes anything. When the tagged
device has the name already, it does nothing. The key reaches `curl` on stdin, never as an
argument, and goes only to `https://api.tailscale.com`.

If `provision-pi.sh` could not remove the sources directory on the Pi, run the `rm` command it
printed now. That command reaches the Pi as `ghost-wispr.tail12ec1c.ts.net`, and before this step
that name is the temporary device's, or no device's.

Then check the join and the name:

```bash
ssh ghost-wispr@ghost-wispr 'tailscale status --self --json'
ssh ghost-wispr@ghost-wispr 'journalctl -u "ghost-wispr-tailnet-join-*"'
curl -fsS https://ghost-wispr.tail12ec1c.ts.net/api/version
```

The status must show exactly `Tags: ["tag:ghost-wispr"]` and
`DNSName: "ghost-wispr.tail12ec1c.ts.net."`, and `curl` must print the release's commit over a
valid certificate. If the certificate check fails, set Serve again
(`ssh ghost-wispr@ghost-wispr 'sudo tailscale serve --bg --https 443 http://127.0.0.1:8080'`) and
run `curl` again.

Then reboot the Pi once and wait until `curl` above answers again:

```bash
ssh ghost-wispr@ghost-wispr 'sudo systemctl reboot'
```

The reboot is needed because tailscaled keeps a Serve listener after the re-authentication. It
keeps the listener it opened on the temporary device's tailnet address, `<old 100.x
address>%tailscale0:443`, although that address is no longer on the interface. On the office Pi
on 2026-11-16 this listener stayed until the reboot. The listener check of slice 1's A5 fails on
it, because it is a listener outside loopback and Tailscale.

## Enable models and Drive sync

Run the models step after the shared drive exists and its members are added. It takes the
shared-drive ID and rejects an absent or malformed one. It replaces settings saved in the UI
with `deploy/pi/files/ghost-wispr.yaml` rendered with that ID,
`gdrive_sync_enabled: true` and `gc_enabled: true`, keeps the deployed
`envoy_webhook_url`, installs the result over `/opt/ghost-wispr/ghost-wispr.yaml`, and
restarts Ghost Wispr. The credential files from the full provisioner stay as they are. A later
full-provisioner run keeps the drive ID and both enablements.

```bash
(
read -r -p 'Shared Drive ID: ' drive_id
[[ "$drive_id" =~ ^[A-Za-z0-9_-]+$ ]] || { printf 'Invalid shared-drive ID\n' >&2; exit 1; }
remote_dir=$(ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  'umask 077; mkdir -p ~/.local/share/ghost-wispr-provision; chmod 700 ~/.local/share/ghost-wispr-provision; mktemp -d ~/.local/share/ghost-wispr-provision/sources.XXXXXX')
tar -C . -cf - deploy/pi/provision.sh deploy/pi/files/ghost-wispr.yaml \
  | ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "tar -xf - -C '$remote_dir'"
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net \
  "$remote_dir/deploy/pi/provision.sh --models '$drive_id'"
ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net "rm -rf -- '$remote_dir'"
)
```

That YAML names Deepgram `nova-3` for live and batch transcription, Gemini 4 Carbon for
summaries, Gemini Embedding 2 for search, and Gemini 3.8 Flash TTS with voice `Kore` for
speech. Chirp 3: HD stays available as another setting (`tts_provider: google` with a Chirp 3: HD
voice); Ghost Wispr does not switch to it by itself. Gemini calls use Vertex
AI in `theorem-agent-infra`, location `global`; embeddings use the documented global endpoint
through `GHOST_WISPR_EMBEDDING_LOCATION` in `/opt/ghost-wispr/.env`. Nothing sets
`GOOGLE_APPLICATION_CREDENTIALS`. Garbage collection keeps every session
(`gc_max_age_days: 0`) and holds local audio to a 20000 MB budget
(`gc_max_audio_size_mb: 20000`), removing only audio that Drive holds a copy of; the template
ships GC off so it never runs before sync is on.

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

The startup recovery verifies the recording hash and completes the splice. It then runs the
refinement and the summary of every segment deletion a stop cut off after its splice, since
the Drive replacement waits for both. When it succeeds, no audio-recovery warning remains for
that recording. If `/api/status` instead says refinement after segment deletion failed, audio
recovery is complete: repair batch transcription, then rerun refinement with
`POST /api/sessions/{id}/retry-refinement`. The warning lasts until refinement completes or
the service restarts; after a restart, check the session's refinement badge. If an
audio-recovery warning remains, preserve the named recording and contact the recorder
operator; do not remove any database row or audio file by hand.

## Sessions kept on this unit only

`/api/status` warns "N sessions are kept on this unit only, because their Drive folder is gone
or unreadable; retry sync once the folder is back, or, if its Drive folder was deleted on
purpose, remove the session from this unit" while any session is marked deleted on Drive and
still has its audio on the Pi. Its card says "Sync: deleted on Drive". Garbage collection never
frees that audio, so clear each such session one of two ways:

- The folder should be on Drive: restore it from the Drive trash, or fix the app's access to the
  drive, then press the session card's retry button (`POST /api/sessions/{id}/retry-sync`).
  The session syncs into its folder.
- The folder was deleted on purpose: remove the session from the unit with the card's
  "Delete from this unit; this unit holds the only copy" control, or
  `DELETE /api/sessions/{id}`. That deletes the session and its audio from the Pi for good.

If a segment of the session was deleted, that delete can answer 409 "delete session ID: this
session still has a deleted segment on Google Drive; delete it after its Drive files are
replaced". The segment deletion still owes the session's Drive folder new files without the
deleted words, and a folder in the Drive trash still holds the old ones. The sync sweep retries
the replacement every 5 minutes while Drive sync is on, but it cannot reach a folder in the
trash. To clear it:

1. In Google Drive, open the trash, find the session's folder (named for the session's date and
   title) and choose "Delete forever". If the folder should be on Drive after all, restore it
   instead: the sweeps then replace its files and sync the session, with no retry needed.
2. Wait for the next sweep (up to 5 minutes). It finds the folder gone, and the segment deletion
   owes nothing more. With `id` set to the session ID the 409 names, this prints `false`:

   ```bash
   ssh ghost-wispr@ghost-wispr "curl -fsS http://127.0.0.1:8080/api/sessions/$id | jq .session.drive_replace_pending"
   ```

3. Delete the session from the unit again.

The warning goes once no such session is left. The count is read again at each status request.

## Accept slice 1

Run the acceptance driver from a tailnet member. Give it the relay URL. To run the signed
Envoy control, inject the signing secret only into the driver's process: `webhook-secret.sh`
reads it under a one-hour secrets-broker grant that Sami approves, as the admin login
`secret-run --register --ttl 1h GCLOUD_SAMI` mints through `gcloud-broker`. Set `broker_host`
to run the read on a machine whose `secret-run` has a broker identity when this one has none
(Sami's laptop: name the devbox), or `broker_key` to read a broker key that holds a copy.

<!-- provision_bundle_test.sh: accept -->
```bash
secret=$(deploy/pi/webhook-secret.sh ${broker_host:+--broker-host "$broker_host"} ${broker_key:+--broker-key "$broker_key"}) &&
relay=$(gcloud run services list --project=theorem-agent-infra --filter='metadata.name ~ ^envoy-webhook-relay-' --format='value(status.url)') &&
[[ $relay =~ ^https://[^[:space:]]+$ ]] &&
GHOST_WISPR_RELAY_URL="$relay/webhook/ghostwispr" \
GHOST_WISPR_ENVOY_WEBHOOK_SECRET="$secret" \
deploy/pi/accept-slice1.sh
unset secret
```

The driver pauses and resumes the recorder, asks the operator to power-cycle it, checks the
light state, verifies the tagged identity and loopback listeners, and tests the signed and
unsigned webhook paths. Before the pause it requires the Ghost Wispr Deepgram socket to grow by
at least 32 kB/s over 60 s, between two readings that one command on the Pi takes 60 s apart: the
stream runs less than 1 % above 32 kB/s in 8060-byte chunks, so a 5-second window can hold one
chunk fewer and fail. When a socket to `api.deepgram.com` opens or closes between the two readings
(the stream reconnects, or a batch upload starts), it measures once more; a stream that is too slow
keeps its socket and fails at once. During the 60-second pause it permits at most 4096 bytes on
that same socket.
It finds the socket among Ghost Wispr's sockets by its peer's certificate for `api.deepgram.com`,
checked from the Pi, and does not resolve the name: the name has several addresses, and a new
lookup often misses the one the recorder is connected to. It compares package
and running-service inventories against `deploy/pi/baseline.txt`, allowing only
`libportaudio2`, `ffmpeg`, `lame`, `jq`, their package dependencies, and Ghost Wispr's user
services. An empty inventory is UNKNOWN, not a pass. While it runs, a member device must also
subscribe with `nats sub 'notifications.ghostwispr.>' --server nats://envoy-hub.tail12ec1c.ts.net:4222`
and confirm the signed probe plus a real
`session.started` and `session.ended` notification.

## Accept slice 2

Run both from a tailnet member after the models step. The self-test plants a
service-account key, a key body inside binary data, a sweep that missed its control, Drive
sync disabled, a missing Drive folder, a status warning, a superseded model, swapped batch
and summarization models, a provider-model mismatch, a running model that differs from the
YAML, an environment override of a model setting, an unapproved Drive writer, and a status
with no certificate warning; each planted condition must be rejected before the live driver
runs.

```bash
deploy/pi/accept-slice2.sh --self-test
deploy/pi/accept-slice2.sh --drive-members "$members_file" --readers-group "$readers_group" --cert-test
```

`--drive-members FILE` is the shared drive's member list from the Drive owner, one
`email|role` line each with Drive API role names (`organizer`, `fileOrganizer`, `writer`,
`commenter`, `reader`). `--readers-group EMAIL` is the approved readers group from Task 2.4.
`--cert-test` reads `/api/status` from a test instance on the Pi
at `127.0.0.1:8081`. The binary has no `--config` flag. Put the test credential file and
its certificate config in a private temporary directory, point the copied certificate config
at the certificate that expires in 10 days, and use a copied YAML that sets its
`google_credentials_file`, separate `db_path` and `audio_dir`, and disables Drive sync, GC,
Envoy, speaker, indicator light, and power button. Before starting it, seed its
separate database as paused. A paused instance opens the microphone, and connects to Deepgram,
only when someone resumes it, so the test instance does not hold the recorder's microphone; do
not resume it. PortAudio still checks the audio devices once as the app starts. Seed and start
it like this:

```bash
GHOST_WISPR_CONFIG="$test_dir/ghost-wispr.yaml" \
/opt/ghost-wispr/ghost-wispr seed-paused-state
GHOST_WISPR_CONFIG="$test_dir/ghost-wispr.yaml" \
GHOST_WISPR_ADDR=127.0.0.1:8081 \
/opt/ghost-wispr/ghost-wispr
```

`seed-paused-state` resolves the config and its `db_path` exactly as the recorder does
(`GHOST_WISPR_CONFIG`, else `ghost-wispr.yaml` in the working directory; a relative `db_path`
against the working directory) and prints the absolute database path it wrote. Check that the
path is the copy's database, not the production one under `/opt/ghost-wispr`.

Then run the driver with `--cert-test`. Without that flag, the row prints `NEEDS-PERSON`.

The live driver reports `RAN`, `FAILED`, or `NEEDS-PERSON` for each slice 2 row.

- A9 plants a service-account control in `/tmp`, sweeps the whole root once, and requires
  the sweep to find that control and no other key material; a binary file that names a key
  field without a key body (the `ghost-wispr` binary's Go struct tags) is printed as a note.
  The configured credential must be an external-account X.509 credential.
- A6 requires `/api/config` to show Drive sync on, a folder ID, credentials present, and GC
  enabled with no age limit and the 20000 MB audio budget. After the real session ends, it
  requires `summary.ready`, a synced Drive folder, and a keyword-search result for that session.
- A8 requires `/api/status` to report no warnings, and the running process's `/api/config`
  `models` to name the plan's model for every role (live and batch transcription, summaries,
  embeddings, the voice and its Google settings): a YAML edited after the service started does
  not change what runs. It also requires the deployed YAML to name them and no environment
  variable to override them, which is what the next start runs. It then starts one real session
  and calls summary, semantic search, speech, and batch refinement, and compares the summary's
  `summary_model`, semantic-search `model`, speech response headers and status model/voice, and
  refinement `refinement_model` with the configured roles. A provider that does not report its
  model produces `not reported by provider` (Vertex AI's embeddings never report one). The spec
  asks that the unit run the plan's models, so A8 passes that role on the model the running
  process reports for it.
- A10 checks the member list: the only organizers are the service account and Sami, and the
  approved readers group is the only reader.

Record the Drive, search, Envoy, model-call, read-only-member, and (without `--cert-test`)
expiring-certificate observations requested by its `NEEDS-PERSON` rows before accepting
the slice.

## Accept slice 3

Run both from a tailnet member once the slice 3 build is on the Pi. The self-test plants, and
must reject, a transcript, Drive document, recording transcription, title or folder name that
still holds the phrase; a changed recording sha256 or path; a Drive file id that answers 404
after "Delete text", or an old one that answers 200 after "Delete text and audio"; a new Drive
file with two revisions; a database, write-ahead log or pre-migration backup that still holds
the phrase; and a build that does not refuse a deletion without a mode. An empty download, a
failed search, a database scan without one of its files, and a read without the control phrase
must each be UNKNOWN, not PASS. The self-test also runs the database scan over planted binary
files, and the Pi-side token, Drive and Deepgram requests against a fake `curl`; it fails if a
token or key reaches a `curl` argument or the evidence.

```bash
deploy/pi/accept-slice3.sh --self-test
deploy/pi/accept-slice3.sh
```

The driver uses `ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net`. It reaches Drive with the
Pi's own credential: on the Pi, it exchanges the X.509 certificate for a token and impersonates
`ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com` with the read-only Drive scope. It
transcribes recordings on the Pi with the Deepgram key in `/opt/ghost-wispr/.env`. Neither
credential leaves the Pi, and the driver prints neither.

One run records one conversation for both rows:

1. The mode guard sends `DELETE` for a segment of a session that does not exist, once without
   a mode and once with `mode=bogus`. Both must answer 400 with an error that names `text` and
   `text_and_audio`. A build without the modes would read `mode=text` as "Delete text and
   audio", so the driver stops here, before it records anything.
2. The precheck requires that no conversation and no file in the database's directory already
   holds "orange tractor ninety" or "purple elephant seventeen". If one does, delete that
   conversation from the unit and run again.
3. The driver starts a conversation and prints its script (`Setup NEEDS-PERSON`). Two people
   read it: "purple elephant seventeen", the topic, and "orange tractor ninety" are each said
   alone in their own turn, and the control phrase "blue harbor lantern" in another. Press Enter
   after the last line. The driver waits until refinement, summary and sync finish. It selects
   only batch-derived timed segments with a positive transcript revision.
4. It restarts `ghost-wispr.service` once no conversation is recording. Ghost Wispr writes the
   pre-migration backup at each start, so the backup then holds the conversation. That is the
   control for the backup check.
5. A11 text only: every check runs once and must find "orange tractor ninety". The driver then
   chooses "Delete text" on that timed segment with its revision
   (`?mode=text&revision=N`) and checks again until the phrase is gone or 15 minutes pass. It
   then chooses "retry refinement" and checks once more.
6. A11: every check runs once and must find "purple elephant seventeen", in any case and with
   its number as digits or words, except the title and the Drive folder name: the summarizer
   chooses whether to name the phrase there, so those two are noted, and after the delete they
   must hold no trace of it either way. The driver then chooses "Delete text and audio" on that
   timed segment with its revision (`?mode=text_and_audio&revision=N`) and checks again.

`--session ID` uses a conversation already recorded from the script, and skips the precheck.

The live driver reports `PASS`, `FAILED` or `UNKNOWN` for each check, and a verdict for each
row and for the run.

- Before each delete, each check must find the phrase (A11's title and Drive folder name only
  note it). That finding is the control for the same check after the delete. If a check cannot
  find the phrase, the row is UNKNOWN and nothing is deleted; record a new conversation and run
  again.
- After a delete, a read that is empty, that failed, or that does not hold "blue harbor
  lantern" is UNKNOWN, not PASS.
- A11 text only checks the segments, the transcript, the refined transcript, the summary, the
  UI search and, with `grep` while Ghost Wispr runs, every file in the database's directory: the
  database, its write-ahead log and the pre-migration backup. It exports the Drive
  `transcript.md` and `summary.md` as text, and requires the recording's sha256 on the unit and
  every Drive file id to stay the same, each id still answering 200.
- A11 checks the segments, the transcript, the refined transcript, the summary, the title, the
  UI search, a Deepgram transcription of the unit's recording and the Drive folder name. It
  exports the new `transcript.md` and `summary.md`, and downloads and transcribes the new
  recording. Each old file id must answer 404 to `files.get` and `revisions.list`, and each new
  file must hold one revision. It also checks that "orange tractor ninety" stays gone.
- The database check looks for each word of a phrase on its own ("orange" and "tractor",
  "purple" and "elephant"; the number is left out, because live transcription may write it as
  digits). SQLite's search index stores each word as its own term, in sorted order, so it never
  holds a phrase as adjacent words. On an index page the first term is whole and each later
  term is stored as the part that differs from the term before it, so a word that shares its
  first letters with its neighbour can still escape `grep`: a delete rebuilds the index, and the
  UI search check covers it.
