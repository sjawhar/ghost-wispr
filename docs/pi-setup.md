# Office recorder Pi runbook

This runbook installs the Ghost Wispr office recorder on the Raspberry Pi 5 named
`ghost-wispr`, and says how each release reaches it. The finished card has Raspberry Pi OS Lite,
Tailscale, the deploy set of a Ghost Wispr release, and its runtime packages. It does not keep a
source checkout, Go, or Node.js.

The recorder is reachable only through Tailscale Serve at
`https://ghost-wispr.tail12ec1c.ts.net`. Its application process listens on
`127.0.0.1:8080`.

## The release and its deploy set

Each Ghost Wispr release has the binary `ghost-wispr-linux-arm64` and the Pi's deploy set,
`ghost-wispr-pi-deploy.tar.gz`, each with its `.sha256`. The tarball has one directory,
`ghost-wispr-pi-deploy/`, the release directory. It holds these files and `manifest.json`, which
names the release's commit and the SHA-256 of the binary and of each other file:

| Files | Installed by | On the Pi |
| --- | --- | --- |
| `ghost-wispr.yaml`, `pi.env`, `update.sh`, `pi-lib.sh`, `wait-for-network.sh`, `wait-for-time-sync.sh` | the nightly update, with the binary | `/opt/ghost-wispr/` |
| `ghost-wispr.service` | the nightly update | `~/.config/systemd/user/` |
| `99-ghost-wispr.rules` | the nightly update | `/etc/udev/rules.d/99-ghost-wispr.rules` |
| `logind-power-key.conf` | the nightly update | `/etc/systemd/logind.conf.d/50-ghost-wispr-power-key.conf` |
| `ghost-wispr-update-launcher.sh` | `provision.sh` only | `/opt/ghost-wispr/` |
| `ghost-wispr-update.service`, `ghost-wispr-update.timer` | `provision.sh` only | `~/.config/systemd/user/` |
| `serve.conf` | `provision.sh` only | Tailscale Serve's setting, one `<port> <proxy target>` line for each HTTPS port |
| `provision.sh` | a person runs it from the release directory | not installed |

The nightly update installs the first nine files and the binary as one unit ("Update and
inspect"); `provision.sh` also installs `update.sh`, `pi-lib.sh` and `pi.env`, so that its first
update can run. The launcher, the update unit and timer, and `serve.conf` reach the Pi only when a
person runs the release's `provision.sh`, so no release can break the path that installs the next
one.
`pi.env` holds the Pi's public deployment settings: its tailnet name, its Google project, service
account and workload identity provider, the names of its Secret Manager secrets, its Tailscale
client ID and the service account's unique ID, the Dispatch and Envoy hub addresses, and the
recordings that no deploy step may change. No file of the release holds a secret, and no release
holds the local settings file ("Settings saved on the page").

## Prerequisites

A new Pi needs these before `provision.sh` runs. Each is in place for the office Pi.

1. agent-infra's `bootstrap` program declares the Pi's Google identity: the service account
   `ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com`, the workload identity provider
   `ghost-wispr`, which trusts the certificate authority `ghost-wispr-ca`, and the Pi's three
   Secret Manager secrets in `theorem-agent-infra`, with the account's read of each
   (`bootstrap/ghostwispridentity.go`): `operator-only-ghost-wispr-deepgram-key`,
   `operator-only-ghost-wispr-dispatch-token` ("The Pi's secrets") and
   `ghost-wispr-device-certificate` ("Renew the X.509 certificate").
2. Each `operator-only-` secret has a version ("The Pi's secrets").
3. agent-infra's `root` program declares the Tailscale identity `ghost-wispr-x509`, with which the
   Pi joins the tailnet as `tag:ghost-wispr`. `pi.env` holds its client ID, `PI_TAILNET_CLIENT_ID`,
   and the service account's unique ID, `PI_SERVICE_ACCOUNT_UNIQUE_ID`, which the join uses. When
   `root` makes the identity again, the `root` job of agent-infra's `seed` workflow prints the new
   client ID as `ghostWisprTailnetClientId` in its log; a pull request puts it in `pi.env`.
4. The tailnet policy gives `tag:ghost-wispr` the Envoy hub's 4222 and 9020
   (theorem-labs/agent-infra#4761, `seed`'s `root` job). Ghost Wispr publishes its events to the
   hub's listener, `envoy_hub_url` in `ghost-wispr.yaml`, over the tailnet with no credential;
   until the policy applies, the events wait on the Pi, and the publisher's five-minute sweep sends
   them once the hub takes them (README, "Envoy events").
5. The latest Ghost Wispr release has `ghost-wispr-pi-deploy.tar.gz` and its `.sha256`.

The Pi's certificate is signed after the Pi makes its key ("Provision", steps 2 to 4).

## The Pi's secrets

The Pi reads two secrets from Secret Manager: its Deepgram key,
`operator-only-ghost-wispr-deepgram-key`, and its own Dispatch token,
`operator-only-ghost-wispr-dispatch-token`. `pi.env` names them as `PI_DEEPGRAM_SECRET` and
`PI_DISPATCH_TOKEN_SECRET`. No release and no file that a person keeps holds them. No box
identity, the Google identity that an agent's machine signs in with, can read them or add a
version: the box identities' Secret Manager access holds off each name that begins
`operator-only-`. The token is a no-scope agent token, never a person's; the meeting voice's
Concierge reads Dispatch with it ("Turn on the meeting voice").

`update.sh` writes the two values into `/opt/ghost-wispr/.env`, mode 0600, which
`ghost-wispr.service` reads. The first four lines of `.env` are the update's, in this order:
`GHOST_WISPR_ADDR=127.0.0.1:8080`, `GOOGLE_API_USE_CLIENT_CERTIFICATE=false`,
`GHOST_WISPR_DEEPGRAM_API_KEY` and `GHOST_WISPR_DISPATCH_TOKEN`. It keeps every other line of
`.env`, such as a person's `GHOST_WISPR_JABRA_MUTE_BUTTON` or `GHOST_WISPR_MIC_DEVICE` line, and
drops the variables Ghost Wispr no longer reads (README, "Removed settings"). It reads the values
with `ghost-wispr pi-secrets`, signed in as `PI_SERVICE_ACCOUNT` with the Pi's certificate, which
prints the name and version of each secret it read, never a value. It removes one final line feed
from a value, and then refuses a value that is empty, that holds a carriage return, a line feed or a
NUL, or a backslash, that starts with a quote, or that has white space at either end: systemd would
read such a value in `.env` otherwise than it was written. When a read fails or a value is refused,
`.env` keeps its secret lines, and the run exits 10.

The update reads the secrets at two times:

- before each start of the recorder: after its two waits, `ghost-wispr.service` runs
  `update.sh secrets`, which writes the four lines and changes nothing else. When that read fails,
  the recorder still starts, on the lines that `.env` has;
- in each nightly run: when a value changed, it installs the new `.env` while the recorder is free,
  restarts the recorder, and prints `ghost-wispr update: installed secrets`. It refuses while an
  override or a drop-in is set ("Update and inspect", step 5).

So a new version reaches the Pi the next night, or at the next restart of the recorder. A restart
ends a conversation that is being recorded:

```bash
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user restart ghost-wispr.service'
```

Add each version on your own device, in a Ghost Wispr checkout, with your own Google login
(`gcloud auth login`). The blocks read the names from `deploy/pi/files/pi.env`
(`GHOST_WISPR_PI_ENV` names another file, as the tests do). Each block gives the value to `gcloud`
in a mode-0600 file that it removes, never in a command argument, and adds no version for an empty
value. The Deepgram key, which the block reads without showing it:

<!-- runbook_blocks_test.sh: deepgram-key -->
```bash
(source "${GHOST_WISPR_PI_ENV:-deploy/pi/files/pi.env}" && umask 077 &&
  f=$(mktemp) && trap 'rm -f -- "$f"' EXIT &&
  IFS= read -r -s -p 'Deepgram key: ' key && echo &&
  printf '%s' "$key" >"$f" && [ -s "$f" ] &&
  gcloud secrets versions add "$PI_DEEPGRAM_SECRET" --project="$PI_GCP_PROJECT" --data-file="$f")
```

The Dispatch token: the block mints a new no-scope agent token with your own Dispatch token, which
must hold the `person` and `approve` scopes, in the file that `DISPATCH_TOKEN_FILE` names. Then it
adds the new token as the secret's latest version:

<!-- runbook_blocks_test.sh: dispatch-token -->
```bash
(source "${GHOST_WISPR_PI_ENV:-deploy/pi/files/pi.env}" && umask 077 &&
  [ -s "${DISPATCH_TOKEN_FILE:?}" ] &&
  m=$(mktemp) && f=$(mktemp) && trap 'rm -f -- "$m" "$f"' EXIT &&
  sed 's/^/Authorization: Bearer /' "$DISPATCH_TOKEN_FILE" |
  curl -fsS -H @- -H 'Content-Type: application/json' \
    --data '{"name":"ghost-wispr-office-pi-secret-manager","scopes":[]}' \
    "$PI_DISPATCH_URL/api/v1/me/agent-tokens" >"$m" &&
  jq -c '{id, name, scopes}' "$m" &&
  jq -jer .token "$m" >"$f" && [ -s "$f" ] &&
  gcloud secrets versions add "$PI_DISPATCH_TOKEN_SECRET" --project="$PI_GCP_PROJECT" --data-file="$f")
```

It prints the new token's id, its name and `"scopes":[]`, and the version that it added, never the
token: Dispatch shows a token only in the answer to its mint. Revoke the token that the Pi used
before only once the Pi runs with the new one. Run the block below on your own device, in a
checkout, with your own Google login; the revoke waits for all three of its readings:

1. The two hashes it prints are equal, and neither is
   `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` (the hash of nothing): the
   Pi's `.env` holds the secret's latest version.
2. The recorder started after the update wrote that `.env`. The block reads the recorder's last
   start line (`… web UI on …`) in its journal of this boot (`journalctl --boot=0`) and the time
   `.env` was written, and prints `the recorder started at <time>, after .env was written at
   <time>`. It stops when the start came first, or when the journal of this boot has no start. The
   update writes `.env` before it restarts the recorder, and prints its `installed commit` or
   `installed secrets` line after the restart, so the start is compared with `.env`, not with that
   line.
3. The `whoami` read prints `{"scopes":[]}`: Dispatch accepts the Pi's token. The read sends the
   token on standard input, on the Pi.

<!-- runbook_blocks_test.sh: token-check -->
```bash
(source "${GHOST_WISPR_PI_ENV:-deploy/pi/files/pi.env}" &&
  ssh "ghost-wispr@$PI_HOST" "sed -n 's/^GHOST_WISPR_DISPATCH_TOKEN[=]//p' /opt/ghost-wispr/.env" |
    tr -d '\n' | shasum -a 256 &&
  gcloud secrets versions access latest --secret="$PI_DISPATCH_TOKEN_SECRET" --project="$PI_GCP_PROJECT" |
    tr -d '\n' | shasum -a 256 &&
  ssh "ghost-wispr@$PI_HOST" 'w=$(stat -c %.6Y /opt/ghost-wispr/.env) &&
    s=$(sudo -n journalctl _SYSTEMD_USER_UNIT=ghost-wispr.service --boot=0 -o short-unix --no-pager |
      grep "web UI on" | tail -n 1 | cut -d " " -f 1) &&
    { [ -n "$s" ] || { echo "found no recorder start in the journal of this boot" >&2; false; }; } &&
    if awk -v s="$s" -v w="$w" "BEGIN { exit !(s > w) }"; then
      echo "the recorder started at $s, after .env was written at $w"
    else echo "the recorder started at $s, before .env was written at $w" >&2; false; fi' &&
  ssh "ghost-wispr@$PI_HOST" "sed -n 's/^GHOST_WISPR_DISPATCH_TOKEN[=]/Authorization: Bearer /p' /opt/ghost-wispr/.env |
    curl -fsS -H @- '$PI_DISPATCH_URL/api/v1/whoami' | jq -c '{scopes}'")
```

Then revoke the old token with your own Dispatch token (`DELETE /api/v1/me/agent-tokens/{id}`, with
the id that its mint printed), and run the `whoami` read again: it must still print
`{"scopes":[]}`.

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
recorder's tested baseline has Tailscale 1.104.6. When this card replaces the card of a Pi that is
on the tailnet, first read that Pi's tagged device's IPv4 address on the Machines page of
Tailscale's admin console, and keep it for "Provision", step 8. Then remove that device there:
`deploy/pi/claim-tailnet-name.sh` refuses while a second tagged device has the hostname
`ghost-wispr`. Join the tailnet temporarily as the operator's own device with Tailscale SSH off
(`sudo tailscale up`) so the Pi remains reachable at `ghost-wispr.tail12ec1c.ts.net` off the
office network. Provisioning replaces this temporary join with `tag:ghost-wispr`.

## Provision

A person provisions the Pi in these steps. Each step runs on the Pi as `ghost-wispr`, over SSH,
unless it says where it runs. Provision outside meetings: step 5 starts the nightly update, which
restarts the recorder, and a restart ends a conversation that is being recorded.

1. On the Pi, download the latest release's deploy set and check it:

   <!-- runbook_blocks_test.sh: download-latest -->
   ```bash
   d=$(mktemp -d) && cd "$d" &&
   u=https://github.com/sjawhar/ghost-wispr/releases/latest/download &&
   curl -fsSLO "$u/ghost-wispr-pi-deploy.tar.gz" &&
   curl -fsSLO "$u/ghost-wispr-pi-deploy.tar.gz.sha256" &&
   sha256sum -c ghost-wispr-pi-deploy.tar.gz.sha256 &&
   tar -xzf ghost-wispr-pi-deploy.tar.gz &&
   cd ghost-wispr-pi-deploy
   ```

   `sha256sum` must print `ghost-wispr-pi-deploy.tar.gz: OK`. The shell is then in the release
   directory, where each later step on the Pi runs. The directory is in `/tmp`, which a reboot
   empties: in a new SSH session, or after a reboot, run this block again.
2. On the Pi, make its key:

   <!-- runbook_blocks_test.sh: provision-key -->
   ```bash
   ./provision.sh --key
   ```

   It makes `~/.config/ghost-wispr-identity/key.pem`, mode 0600, and prints the key's certificate
   request, with the subject `CN=ghost-wispr`. It refuses when the key exists. The key never leaves
   the Pi.
3. Open a pull request in agent-infra that adds the request to `ghostWisprCSRs` in
   `bootstrap/ghostwispridentity.go`, as the value of its `ghost-wispr` entry: only that entry's
   certificate goes to `ghost-wispr-device-certificate`. When the pull request merges, the
   `bootstrap` apply signs the request with the certificate authority `ghost-wispr-ca` and adds the
   certificate as the latest version of that secret.
4. On your own device, in a checkout, with your own Google login, read the certificate and copy it
   to the Pi as `~/device.crt`. The certificate is not a secret.

   ```bash
   (source "${GHOST_WISPR_PI_ENV:-deploy/pi/files/pi.env}" && f=$(mktemp) && trap 'rm -f -- "$f"' EXIT &&
     gcloud secrets versions access latest --secret="$PI_CERTIFICATE_SECRET" --project="$PI_GCP_PROJECT" >"$f" &&
     openssl x509 -in "$f" -noout -subject -nameopt RFC2253 -enddate &&
     scp "$f" "ghost-wispr@$PI_HOST:device.crt")
   ```

   `openssl` must print `subject=CN=ghost-wispr` and an end date one year after the apply. Then,
   on the Pi, in the release directory, install it:

   <!-- runbook_blocks_test.sh: provision-certificate -->
   ```bash
   ./provision.sh --certificate ~/device.crt
   ```

   It installs `/opt/ghost-wispr/device.crt` only when the certificate's public key is the public
   key of `key.pem`. It refuses while `device.crt` exists: the nightly update installs each renewed
   certificate ("Renew the X.509 certificate").
5. On the Pi, in the release directory, provision:

   <!-- runbook_blocks_test.sh: provision -->
   ```bash
   ./provision.sh
   ```

   `provision.sh` runs as `ghost-wispr`, with passwordless sudo, and refuses data on standard
   input. It refuses a directory whose files do not match its `manifest.json`, such as a checkout's
   `deploy/pi/`. Before it changes anything, it refuses while the environment of `ghost-wispr`'s
   user manager sets `GHOST_WISPR_CONFIG` or an Envoy variable, which the recorder would inherit
   ("Turn on the meeting voice", step 1). Then it:

   - installs `libportaudio2`, `ffmpeg`, `lame`, `jq` and `curl`;
   - writes `/opt/ghost-wispr/google-credentials.json` and `/opt/ghost-wispr/certificate_config.json`
     from `pi.env`, where they are missing. Where they exist, it changes neither, and prints
     `credentials: same as pi.env renders` or the names of the fields that differ. Where
     `/opt/ghost-wispr/tailscale-client-id` exists, it prints whether that ID equals
     `PI_TAILNET_CLIENT_ID`;
   - adds `ghost-wispr` to the groups `plugdev`, `input` and `audio`, and installs the launcher,
     `ghost-wispr-update.service` and `ghost-wispr-update.timer`, and the release's `update.sh`,
     `pi-lib.sh` and `pi.env`. It turns on lingering for `ghost-wispr` and enables the timer;
   - starts the nightly update once, with `systemctl --user start ghost-wispr-update.service`. That
     run installs the release's whole deploy set and reads the Pi's secrets ("Update and inspect").
     `provision.sh` prints the run's journal lines, and stops when the run failed;
   - enables `ghost-wispr.service`;
   - applies each line of the release's `serve.conf` with `tailscale serve --bg --https`. It turns
     off each live Serve HTTPS port that `serve.conf` does not list, and each mount other than `/`
     on a port that it lists. Then it reads Serve again, and stops before the join when Serve still
     holds a part that `serve.conf` does not, such as a TCP forward or Funnel;
   - starts the tagged tailnet join as a detached unit, with `PI_TAILNET_CLIENT_ID` and
     `PI_SERVICE_ACCOUNT_UNIQUE_ID`, or stops after "already joined" on a Pi that is joined as
     `tag:ghost-wispr` already.

   It ends with `already joined as tag:ghost-wispr`, or with the line that names the detached join
   unit it started. The SSH session can end while Tailscale re-authenticates (exit 255). The join
   unit's command line holds each `tailscale serve` call of `serve.conf`, so the join reads no file
   of the release directory. The join runs `tailscale up` with `--accept-routes=false`, so the Pi
   joins with the tailnet's subnet routes off. Then it runs the route check of "Accept the tailnet's
   subnet routes", and accepts the routes (`tailscale set --accept-routes`) only when no peer's
   `PrimaryRoutes` overlaps a network of the Pi.
6. On your own device, in a Ghost Wispr checkout, give the tagged device its name. The tagged join
   makes a new Tailscale device. The temporary device you joined as your own still holds the name
   `ghost-wispr`, so Tailscale names the tagged one `ghost-wispr-1`, and
   `ghost-wispr.tail12ec1c.ts.net` does not reach the recorder. A new run of `provision.sh` does
   not change that. Give the tagged device its name with `deploy/pi/claim-tailnet-name.sh`, which
   needs `curl` and `jq`, and a Tailscale API access token. A tailnet admin makes the token on the
   [Keys](https://console.tailscale.com/admin/settings/keys) page of Tailscale's admin console,
   with an expiry of 1 day. The block reads the token without showing it and gives it to that one
   command alone, in its environment:

   <!-- runbook_blocks_test.sh: claim-name -->
   ```bash
   (IFS= read -r -s -p 'Tailscale API access token: ' key && echo &&
     TAILSCALE_API_KEY=$key deploy/pi/claim-tailnet-name.sh)
   ```

   It removes the temporary device only when that device is the one device other than the tagged
   one that holds the name, has an id that is a string of digits, has the hostname `ghost-wispr`,
   has a user owner and no tag, and is offline. Then it renames the tagged device and reads the
   name back. It refuses any other holder, or more than one, before it changes anything. When the
   tagged device has the name already, it does nothing. The token reaches `curl` on stdin, never as
   an argument, and goes only to `https://api.tailscale.com`. Revoke the token on the Keys page
   afterwards.
7. From a tailnet member device, check the join and the name:

   ```bash
   ssh ghost-wispr@ghost-wispr 'tailscale status --self --json'
   ssh ghost-wispr@ghost-wispr 'sudo journalctl -u "ghost-wispr-tailnet-join-*"'
   ssh ghost-wispr@ghost-wispr 'tailscale debug prefs' | jq .RouteAll
   curl -fsS https://ghost-wispr.tail12ec1c.ts.net/api/version
   ```

   The status must show exactly `Tags: ["tag:ghost-wispr"]` and
   `DNSName: "ghost-wispr.tail12ec1c.ts.net."`, and `curl` must print the release's commit over a
   valid certificate. If the certificate check fails, run `./provision.sh` again ("Update and
   inspect", "Apply a provisioning change"), which applies `serve.conf` and stops after "already
   joined", and run `curl` again.

   `jq` prints `RouteAll`, the Pi's Tailscale setting that `--accept-routes` controls. It must read
   `true` after the join's route check accepted the routes, and `false` after the check refused
   them. After a refusal, the journal says why. "Accept the tailnet's subnet routes" says how to fix
   it.
8. When this card replaces the card of a Pi that was on the tailnet, give the tagged device that
   Pi's IPv4 address, which you read before the temporary join ("Flash the card"), so a client that
   reaches the Pi by its address still reaches it. A tailnet admin sets it on the Machines page of
   Tailscale's admin console, in the device's IPv4 edit dialog
   ([IP pool](https://tailscale.com/docs/reference/ip-pool)). Then reboot the Pi once, and wait until
   `curl` of step 7 answers again:

   ```bash
   ssh ghost-wispr@ghost-wispr 'sudo systemctl reboot'
   ```

   The reboot is needed because tailscaled keeps a Serve listener after the re-authentication. It
   keeps the listener it opened on the temporary device's tailnet address, `<old 100.x
   address>%tailscale0:443`, although that address is no longer on the interface. The listener
   check of slice 1's A5 fails on it, because it is a listener outside loopback and Tailscale.

`device.crt`, `google-credentials.json` and `certificate_config.json` are not secrets. The private
key stays at `/home/ghost-wispr/.config/ghost-wispr-identity/key.pem`. `google-credentials.json` is
an `external_account` credential for the workload identity provider `PI_WORKLOAD_PROVIDER`: it
exchanges the certificate at `https://sts.mtls.googleapis.com/v1/token` and impersonates
`PI_SERVICE_ACCOUNT`. `certificate_config.json` names `/opt/ghost-wispr/device.crt` and the key. Do
not set `GOOGLE_API_CERTIFICATE_CONFIG`: the certificate is for the Security Token Service exchange,
not Google API mTLS. Ghost Wispr reads this credential through
`google_credentials_file: /opt/ghost-wispr/google-credentials.json` in its YAML. The tailnet join
passes the same path with `ghost-wispr google-id-token --credentials`; it does not set
`GOOGLE_APPLICATION_CREDENTIALS`.

The Ghost Wispr user service starts only once the Pi's clock is synchronized. The Pi has no
battery-backed clock, so at boot its clock is behind until a time server sets it, and a session
started before then is named and stamped with the wrong time. A user service cannot be ordered
after the system's `time-sync.target`, so the service's start runs
`/opt/ghost-wispr/wait-for-time-sync.sh` after the network wait: it waits for the mark
`systemd-timesyncd` leaves once it synchronizes the clock, and after 120 s starts the service
anyway, so a Pi that reaches no time server still records, its times possibly wrong. It waits
those 120 s once per boot: once it gives up it leaves a mark in `/run/user/<uid>`, so a restart of
the service in the same boot starts at once. While it waits nothing records and the light is off.
A Pi with no network at all does not start: the network wait before it fails and systemd tries
again. The nightly update installs `ghost-wispr.service` and both waits with each release.

## Renew the X.509 certificate

The Pi's certificate, `/opt/ghost-wispr/device.crt`, lasts one year. A `bootstrap` apply in its last
60 days signs the Pi's request again and adds the new certificate as the latest version of
`ghost-wispr-device-certificate`; the Pi's key does not change. agent-infra's workflow
`.github/workflows/ghost-wispr-certificate.yaml` asks for that apply each Monday at 10:00 UTC. Then
it reads the newest version whose public key is the Pi's, and fails when that version has fewer
than 45 days left. A failed run posts one alert to `#fleet-alerts` that names the step that failed,
the days left and the end date.

The Pi installs a renewed certificate itself. Each nightly update runs `pi-secrets`, which reads
the latest version of the secret and up to nine versions under it, newest first, and skips a
version that is disabled, destroyed or absent. It takes the newest version that is one PEM
certificate, has the public key of the Pi's `key.pem`, ends later than the installed `device.crt`,
and signs in to Google. `update.sh` installs that version while the recorder is free, with its
version number in `/opt/ghost-wispr/device.crt.version`. It keeps `device.crt.prev` and
`device.crt.version.prev`, restarts the recorder, and prints
`ghost-wispr update: installed certificate version <n>, not after <date>`. When the recorder does
not answer after the restart, it puts both `.prev` copies back and exits 10.

`update.sh` gives `pi-secrets` the installed version from `device.crt.version`, as
`--installed-certificate-version`. When `latest` cannot be read because it is disabled, destroyed
or absent, `pi-secrets` reads the versions above the installed one by number instead, oldest first
and at most ten: it skips a disabled or destroyed version, stops at the first version that does not
exist, and takes the newest version read that passes. So a renewal under a disabled `latest` still
reaches the Pi. Until the update installs its first certificate, the Pi has no
`device.crt.version`, and a `latest` that cannot be read keeps the installed certificate.

When no version passes, `pi-secrets` keeps the installed certificate, and a line that starts
`pi-secrets: certificate` says why. For one, the walk up that found no version prints
`pi-secrets: certificate kept: latest certificate cannot be read: <code>; no readable version above <n>`.
A certificate that is kept changes no exit code.

Ghost Wispr warns on its status page and in `/api/status` when the certificate has fewer than 30
days left, or when it cannot read the certificate. Then start the workflow by hand, and read the
`bootstrap` run that it started, whose id its log prints:

```bash
gh workflow run ghost-wispr-certificate.yaml -R theorem-labs/agent-infra --ref main
```

The next nightly update installs the new version. Read its line on the Pi:

```bash
ssh ghost-wispr@ghost-wispr 'sudo -n journalctl _SYSTEMD_USER_UNIT=ghost-wispr-update.service --since yesterday -o cat' | grep 'certificate'
```

Do not make a new Pi key or new Google credential files for a renewal.

## Accept the tailnet's subnet routes

A Linux device uses a subnet route only when Tailscale runs with `--accept-routes`
([Subnet routers](https://tailscale.com/docs/features/subnet-routers)). Legion pods reach the
recorder at their own addresses, through subnet routers. The Pi needs those subnet routes to send
its replies to the pods. But an accepted route takes the Pi's own traffic for that route's network.
So the Pi accepts the routes only when the route check finds no overlap.

The route check compares two lists:

- the Pi's IPv4 networks, from `ip -4 -o addr`, except the network of `tailscale0`
- the routes in each peer's `PrimaryRoutes`, from `tailscale status --json`

It does not compare IPv6 routes. A route overlaps a network when one of the two holds the other.
The check refuses when it cannot read one of its inputs. When it refuses, it prints one line for
each overlap, or one line that names the step that failed. Then it prints one more line, which
starts with `ghost-wispr routes: not accepting the tailnet's subnet routes:`.

The detached join of "Provision" uses the check:

1. It runs `tailscale up` with `--accept-routes=false`. This sets `RouteAll`, the Tailscale
   setting that `--accept-routes` controls, to false.
2. It runs the check.
3. It runs `tailscale set --accept-routes` only when the check passed. This sets `RouteAll` to
   true.

When the check refuses, the join unit ends failed. The Pi stays in the tailnet, with `RouteAll`
false. The check's lines are in the unit's journal. Read them on the Pi with
`sudo journalctl -u 'ghost-wispr-tailnet-join-*'`. To fix an overlap, remove the route or change
the office network's range. Then run the block below.

A new run of `provision.sh` on a Pi already joined as `tag:ghost-wispr` exits before the join. So
it does not run the check, and it does not change `RouteAll`.

Run the block below in these cases:

- on a Pi that joined the tailnet before the join had the route check
- after you fix a refusal
- when someone adds a subnet route to the tailnet
- when the Pi's network changes

Run it on the Pi as `ghost-wispr`, in the release directory ("Provision", step 1):

<!-- runbook_blocks_test.sh: accept-routes -->
```bash
check=$(bash -c 'source ./provision.sh && route_overlap_check_script') &&
[ -n "$check" ] &&
verdict=$(printf '%s\n' "$check" | sh -s) &&
printf '%s\n' "$verdict" &&
case $verdict in "ghost-wispr routes: no route of a peer overlaps"*) ;; *) false ;; esac &&
sudo tailscale set --accept-routes &&
tailscale debug prefs | jq -e .RouteAll
```

The block runs the route check of the release's `provision.sh`. On success, it prints the check's
acceptance line, and then `true`. The acceptance line starts with
`ghost-wispr routes: no route of a peer overlaps a network of the Pi:`, and it names the networks
and the routes that the check compared. `true` is the value of `RouteAll` after `tailscale set`.

The block stops before `tailscale set` in each of these cases:

- the release's `provision.sh` has no `route_overlap_check_script`
- `route_overlap_check_script` prints nothing
- the check refuses
- the check prints no acceptance line, as when the program reaches `sh` incomplete

It exits non-zero when `tailscale set` fails, or when `RouteAll` then reads `false`.

To read `RouteAll` on the Pi:

```bash
tailscale debug prefs | jq .RouteAll
```

If the check finds an overlap while `RouteAll` reads `true`, disable the routes on the Pi:

```bash
sudo tailscale set --accept-routes=false
```

Then fix the overlap, as above. Run the block again after the fix.

## The model gateway

The Pi's search embeddings move to Theorem's model gateway, the LiteLLM proxy at
`https://model-gateway.tail12ec1c.ts.net`, which speaks the OpenAI API under `/v1`, once it admits
the Pi. Its `gateway` provider, commented out in `deploy/pi/files/ghost-wispr.yaml`, signs in with
a Google ID token for the audience `model-gateway-gce`, which Ghost Wispr mints from
`google_credentials_file` by impersonating `ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com`
with its email in the token. The gateway admits a token whose email is on its account list, so the
Pi holds no gateway key.

Until theorem-labs/agent-infra#5187 is merged and applied, the gateway refuses the Pi, and a
provider that refuses reads Fails on the settings page. So the template names no gateway provider
and sends search to `vertex/gemini-embedding-2`: the model and location (`global`) of the vectors
the Pi already holds. Search labels a vector `gemini-embedding-2` whichever provider serves the
model, so the move to the gateway embeds no session again either.

The gateway serves the Pi `gemini-embedding-2` and no Gemini chat, text-to-speech or streaming
audio model. So the Concierge and summaries call Gemini on Vertex AI directly, text-to-speech
streams from Vertex AI's `us` multi-region, Deepgram takes its key, and GPT-Live signs in by OpenAI
workload identity. Each of them is a named provider: moving a role to the gateway, once the gateway
serves its model to the Pi, is a change of that role's provider/model in the template, which the
nightly update installs with its release.

The admission is one agent-infra change, theorem-labs/agent-infra#5187:

- `shared/boxaccounts` lists the Pi's account as a `Fleet` entry in team `legion-infra`, with no
  project or instance, since a token from `generateIdToken` carries no Compute Engine claim; the
  gateway's rendered account lists carry it.
- `model-gateway/main.go` gives team `legion-infra` `gemini-embedding-2`, under the one
  inputs-a-minute bound all its embedding teams share. `model-gateway/` applies on merge, by CI.
- `root/tailnetpolicy.go` lets `tag:ghost-wispr` reach `tag:model-gateway` on tcp:443. `root/`
  applies by the seed workflow's `root` job, behind an approval of a member of the theorem-labs
  team `infrastructure`.

Check it from the Pi once both apply. Before `root/` applies, the name does not resolve; before
`model-gateway/` applies, the embedding answers 401. After both, the first prints `200` and the
second the length of a vector, 3072:

```bash
ssh ghost-wispr@ghost-wispr "curl -sS -o /dev/null -w '%{http_code}\n' https://model-gateway.tail12ec1c.ts.net/health/liveliness"
ssh ghost-wispr@ghost-wispr 'GOOGLE_API_USE_CLIENT_CERTIFICATE=false /opt/ghost-wispr/ghost-wispr google-id-token \
    --credentials /opt/ghost-wispr/google-credentials.json --audience model-gateway-gce |
  { IFS= read -r t; printf "Authorization: Bearer %s\n" "$t"; } |
  curl -sS -H @- -H "content-type: application/json" \
    -d "{\"model\":\"gemini-embedding-2\",\"input\":\"office search probe\",\"encoding_format\":\"float\",\"task_type\":\"RETRIEVAL_QUERY\"}" \
    https://model-gateway.tail12ec1c.ts.net/v1/embeddings | jq ".data[0].embedding | length"'
```

Then move search to the gateway in one pull request. In `deploy/pi/files/ghost-wispr.yaml`, the
switch is the line `model: gateway/gemini-embedding-2` under `embedding`, with the commented-out
`gateway` entry under `providers` uncommented. The checks that pin the Pi's plan move with it:
`embedding_model` and a `gateway` row in `expected_running_providers` (its `url` and
`auth.audience` compared with the template's) in `deploy/pi/accept-slice2.sh`, the embedding line
and the gateway check in `deploy/pi/provision_models_test.sh`, and `internal/config`'s
`TestThePiConfigSearchesOnVertexAIUntilTheGatewayAdmitsThePi`. The nightly update installs it with
that release ("A release that changes config"); the Pi's credentials do not change.

## Enable models and Drive sync

Run the models step once on a Pi, after the shared drive exists and its members are added. It
takes the shared-drive ID and rejects an absent or malformed one. It renders the release's
`ghost-wispr.yaml` with that ID, `gdrive_sync_enabled: true` and `gc_enabled: true`, installs the
result over `/opt/ghost-wispr/ghost-wispr.yaml`, and restarts Ghost Wispr. It writes no `.env`
line: the restart refreshes the update's four lines ("The Pi's secrets"). A restart ends a
conversation that is being recorded, so run it outside meetings. Settings saved on the page stay in
the local settings file, `/opt/ghost-wispr/ghost-wispr.local.yaml`, and apply over the installed
config (README, "Configuration"). Each nightly install renders the release's config with the drive
ID of the installed config's `gdrive_folder_id:` line and both enablements, so the Pi needs the
step again only to change its shared drive. The models step refuses before it writes anything if a
line of `.env`, or the environment of `ghost-wispr`'s user manager, could set `GHOST_WISPR_CONFIG`,
which would have the recorder load another file than the config it installs, or
`GHOST_WISPR_ENVOY_ENABLED`, `GHOST_WISPR_ENVOY_HUB_URL` or `GHOST_WISPR_CONCIERGE_ENVOY_URL`, which
override the template's `envoy_enabled`, `envoy_hub_url` and `concierge.envoy_url` ("Turn on the
meeting voice", step 1), naming the variable. Set `GHOST_WISPR_CONFIG` only on a command line, as
for the test instance ("Accept slice 2"). Before it reads the manager's environment it reloads the
manager, and refuses while `ghost-wispr.service` or `ghost-wispr-update.service` has a drop-in,
naming it, since a drop-in can set the recorder's environment past both checks; the nightly update
makes the same checks ("Update and inspect"). Remove the drop-in and run the step again.

Run it only from the release the Pi runs, which can be older than the latest release: the models
step installs that release's `ghost-wispr.yaml`, and the Pi's binary must not run on a newer
release's config. `provision.sh --models` refuses the directory of another release than the one
`deploy.verified` names, and names the commit the Pi runs. Enter that release's directory on the Pi
as "Update and inspect", "The release the Pi runs" says. Then, in that release directory, run the
models step with the shared drive's ID:

<!-- runbook_blocks_test.sh: models -->
```bash
./provision.sh --models '<shared-drive-id>'
```

That YAML names the Pi's providers (README, "Model providers"): `vertex` and `vertex-us`, Vertex AI
in `theorem-agent-infra` at `global` and at the `us` multi-region, signed in with the Pi's Google
workload identity; `openai`, by OpenAI workload identity; and `deepgram`, by the key that the
nightly update reads from Secret Manager into `.env` as `GHOST_WISPR_DEEPGRAM_API_KEY` ("The Pi's
secrets"). Both Vertex AI providers carry
the labels `owner: sjawhar` and `stream: agents-no-stream`: every Gemini generate call (the
Concierge, summaries, speech) sends them, and Vertex AI copies them into Cloud Billing, so the Pi's
Gemini spend is attributed to its owner and stream (dispatch://INFRA-1027); search's embeddings
carry none. The model gateway's `gateway` entry is there commented out, until the gateway admits
the Pi ("The model gateway"). Its roles:
`deepgram/nova-3` for live and batch transcription,
`vertex/gemini-4-carbon` for summaries (each session online: `summarization.batch_jobs: false`,
because `theorem-agent-infra`'s IAM deny policy `vertex-jobs-fence`, dispatch://INFRA-3614, refuses
every Vertex AI batch job create, and genai sends Vertex AI no batch of inlined requests anyway),
`vertex/gemini-embedding-2` for search,
`vertex/gemini-3.5-flash-lite` for the Concierge, and `vertex-us/gemini-3.8-flash-tts` with voice
`Kore` for speech. Another text-to-speech provider is a config change (README, "Text-to-speech"):
Chirp 3: HD voices, for one, need a `google-tts` provider; Ghost Wispr does not switch by itself.
Nothing sets `GOOGLE_APPLICATION_CREDENTIALS`. Garbage collection keeps every session
(`gc_max_age_days: 0`) and holds local audio to a 20000 MB budget
(`gc_max_audio_size_mb: 20000`), removing only audio that Drive holds a copy of; the template
ships GC off so it never runs before sync is on. It also holds the office's five recording rules
(`recording_rules`), which the web UI shows under Recording rules; the nightly update and the
models step install them with the rest of the template.

## Pause and resume at the unit

With `power_button: true`, set in `deploy/pi/files/ghost-wispr.yaml`, a short press of the Pi's
power button pauses recording, or resumes it when it is paused; with `jabra_mute_button: true`,
which the template also sets, so does a tap of the Jabra Speak2 75's Mute button. A restart of a
paused unit stays paused: before the fix of defect D9 (#75) it could resume recording by itself,
because the Jabra sends a mute-key press as the old process closes its microphone. The buttons
change the same pause switch as the web page, `POST /api/pause` and `POST /api/resume`, so any of
them resumes a pause made by any other, and a resume from a button checks the recording light as
one from the web page does. A long press of the power button is left to the Pi, which powers off.

The nightly update installs the template with the rest of each release ("A release that changes
config"), so a key added to the template reaches the Pi with its release. Settings saved on the
page stay in the local settings file through each release. To turn the Jabra's button off or on
without a release, put `GHOST_WISPR_JABRA_MUTE_BUTTON=false` or `GHOST_WISPR_JABRA_MUTE_BUTTON=true`
in `/opt/ghost-wispr/.env`, which overrides the YAML, and restart `ghost-wispr.service`. The nightly
update keeps that line. A settings save on the page while that line is set writes its value to the
local settings file, where it stays after the line is removed. Turn it off while recording, or
unplug the Jabra and plug it in again afterwards: Ghost Wispr leaves the Jabra's light as it last
set it.

The Jabra's mute light is on while recording is paused and off otherwise, whichever control
paused it. Ghost Wispr never changes it while any of the Jabra's streams is open, its microphone
or the speaker's stream: a Jabra Speak2 75 whose light changed while one of its streams was open
sends Phone Mute by itself 12-15 ms after its microphone stream next closes, which reads as a tap.
The speaker's stream opens with the first sound and closes 0.3 s after the last one. So a pause
closes the microphone, then lights the Jabra, and a resume darkens the Jabra, then opens the
microphone. Ghost Wispr also sets the light when the Jabra connects or is plugged in again, after
each tap, and within a quarter of a second after any other pause or resume, each time only while
no stream is open; a change during speech waits for the speaker's stream to close. A resume
during speech waits for the speaker's stream to close, 0.3 s after the last sound, with capture
shown as resuming and the blink(1) off, then darkens the Jabra, turns the blink(1) red and opens
the microphone, so the ring never reads Muted while the unit records. A pause during that wait
cancels the resume. If the ring may still
show Muted while recording is on, as when the Jabra is plugged in while the microphone is open,
or when a pause and a resume come during one microphone open, no audio is recorded: Ghost Wispr
closes the microphone, darkens the Jabra and opens the microphone again, and the log says
`the Jabra's ring may show the pause while capture is on; reopening the microphone`. During
speech that takes until the speaker's stream closes, with capture shown as resuming and the
blink(1) off.

A Mute press that would resume recording is ignored when the kernel stamped it during Ghost
Wispr's own open, close or PortAudio device scan of the microphone, or the close of the speaker's
stream, or within 100 ms after one: that is the Jabra's own report, and recording stays
paused. Ghost Wispr has the kernel stamp the Jabra's events with the monotonic clock, so a change
of the Pi's time does not move the stamps. The log says
`Jabra mute report came with Ghost Wispr's own microphone operation; capture stays paused`.
A press that would pause always pauses. The blink(1) stays the recording light: the Jabra's light
only shows the pause switch.

The blink(1) reads back red before the microphone opens, and stays red until it is open: every
start, resume and reconnect of the microphone, and PortAudio's device scan, waits for that check.
Before it lights the blink(1) for an open, Ghost Wispr looks in `/proc/asound` for a capture PCM
(`cardN/pcmMc`) of the card `mic_device: J75` names, the Jabra Speak2 75's ALSA card id, without
opening one, and it opens only that card's input, through ALSA's plug layer (`plughw:0,0`): the
Jabra captures only at 32000 Hz, and the plug layer gives Ghost Wispr the 16000 Hz it records and
streams at, so the log says `microphone started at 16000 Hz`. With the Jabra unplugged there is
none, also while another capture card, such as the ALSA loopback or a second Jabra device, is
present; then
the blink(1) stays off, `/api/status` warns `No microphone found`, and the start-up retry (every 15
seconds) and the reconnect (backing off to every 30 seconds) look again without lighting it. A unit that
starts paused scans only at its first resume. The speaker does not wait for it: it plays through the
ALSA playback PCM of the card `speaker_device: J75` names (`plughw:CARD=J75,DEV=0`, the Jabra's
`pcmC0D0p` on the office Pi), opening no microphone and needing no red light, so a unit restarted
paused still speaks. It plays at the first of 48000, 44100, 32000, 24000 and 16000 Hz that the
Jabra takes without resampling, with 10 ms periods and an 80 ms buffer. When a cancel stops the
speech, as when someone talks over it, the speaker rewinds that buffer, so the stream plays on,
and writes the rest of the mix (a chime, say) again without the speech. Audio the Jabra has
already taken still plays. If that is more than a period, or the device cannot rewind, the speaker
drops the buffer instead. Each time the speaker opens, the log says
`speaker stream` with the rate, period and buffer the Jabra granted, and `"rewind":true` if it
can rewind. Each cancel logs `speaker cancel` with `"way":"rewind"` or `"way":"drop"`, so the log
shows what the Jabra did. Each underrun, when the speaker runs out of audio and clicks, is logged as
`speaker underrun` with the device, the count so far in that stream and the buffer the device
granted, and the count again at the close as `speaker stream closed after underruns`. A check that
counts underruns reads those lines: a reader of `/proc/asound` can miss a short one between its
polls. The speaker needs no package beyond `libportaudio2`,
whose dependency `libasound2t64` the Pi already has (`deploy/pi/baseline.txt`). A pause during an
open keeps the blink(1) red until that open has finished.

To find what keeps the speaker's mixer from the Jabra when it underruns, set the debug setting
`speaker_trace_dir` (or `GHOST_WISPR_SPEAKER_TRACE_DIR`) to a directory and restart the service; it
is off by default. The process then keeps its last 3 seconds of Go execution trace in memory, aiming
at 16 MiB, a target the Go runtime can pass during a burst of events, and on an underrun writes them
to `speaker-underrun-<time>.trace` there, at most once every 10 seconds. A capture larger than
32 MiB is not saved (the log says `speaker trace: the capture was not saved`), and the directory
keeps the newest 20 captures that fit 128 MiB together; the log says `speaker trace: saved the
capture of an underrun`.
`/opt/ghost-wispr/ghost-wispr speaker-trace FILE` prints each turn of the mixer longer than 40 ms
in the capture (`-slow` changes the bound), longest first, with its start and end on the wall
clock in UTC, as `journalctl --utc -o short-iso-precise` prints the journal's lines, and with what
it spent the time on: running, runnable (Go had no processor free), in a system call (ALSA's
write), or waiting on the pacer's sleep, the garbage collector, the speaker's lock (naming the
goroutine that held it) or something else, and how long the Go runtime stopped the world and the
collector marked during the turn. The wall-clock times come from the clock snapshots the trace
takes once each generation (about every second): each is projected from the latest snapshot before
it, assuming the clock did not step since. A time the snapshots cannot pin is printed qualified,
never as a plain time: `between A and B` when the next snapshot shows the clock stepped (an NTP
correction), at a moment the trace does not show; and `no clock snapshot before it` when it comes
before the capture's first snapshot. The time it also gives into the capture counts from the
capture's first event, not from the process's start. "Running" covers both computing and a thread
Linux took off its processor, which a Go trace cannot tell apart.

Every capture ends with the turn-off: remove `speaker_trace_dir`, and anything else added for the
capture (a systemd drop-in, a `GODEBUG=gctrace=1` in `/opt/ghost-wispr/.env`), then restart the
service. The trace costs CPU and memory while it runs. No deploy step is a turn-off: the nightly
update installs the release's `ghost-wispr.service` and keeps each `.env` line that it does not own
but those of variables Ghost Wispr no longer reads, so a forgotten `GODEBUG` line stays on through
it. While a drop-in is under `ghost-wispr.service.d`, the nightly update and the models step refuse
to install anything, so the Pi stays on its release until the drop-in goes.

The microphone is never open while the blink(1) is off: the blink(1) stays red while the microphone
is open, and goes off only once Ghost Wispr has closed it. A pause closes it at once, also while
its read waits on a device that delivers no frame or its audio waits on a stuck recording file;
then the blink(1) goes off, within 500 ms of the tap: that is the bound for A1. If a call into
PortAudio does not return, the microphone cannot be
closed: the blink(1) stays red, `/api/status` warns `Microphone still open after a pause` until it
closes, and nothing is recorded meanwhile. Once no audio has reached the recording for 2 seconds,
Ghost Wispr closes the microphone, and the blink(1) goes off, with `capture_state` `no_audio`; it
opens it again under a new check after a wait that doubles up to 30 seconds: with a microphone that
delivers nothing, the light shows red for about 2 seconds at each retry while nothing is recorded.
After a stream error
it closes the microphone before it waits to reconnect. It reads the blink(1) back for each buffer
of audio: once the blink(1) was not red while the microphone was open, as when it was unplugged or
its watchdog turned it off, Ghost Wispr drops what the microphone holds and opens it again under a
new check; the log says `the recording light was not red while the microphone was open; reopening
the microphone`. At shutdown the blink(1) goes off once the microphone is closed.

The office Pi's blink(1) (USB `27b8:01ed`, an mk3: its version reply is `01760033 33000000`) has
two LEDs. LED 1, the top one, is the recording light: every check above reads LED 1 alone, and
only exact red counts. LED 2, the bottom one, is red with LED 1 while the unit records, and flashes
green, half a second on and half a second off, while an agent's answer waits for "Wispr, go ahead".
It goes back to red once the answer is spoken, "Wispr, never mind" drops it, or its session ends,
and goes off with LED 1 at a pause, a light hold or shutdown; it never flashes while the unit does
not record, and flashes only while room audio reaches the recording: while the microphone closes
after a pause or reopens under a red check, LED 2 shows red. A check that finds LED 1 not red writes
both LEDs red and stops the flash until a check finds LED 1 red again. A read selects LED 1 ('l' 0)
and reads it ('r') in one hold of the lock below: the mk3 reads the LED selected last, whatever LED
the read names. The flash writes LED 2 alone with a fade of one 10 ms tick ('c', LED 2): an 'n'
write that names one LED writes the next one and starts the blink(1)'s pattern player, which fades
both LEDs to the stored black pattern. That pattern counts as black only when each line is black on
every LED (LED number 0, which 'l' 0 before the pattern's writes sets), since a black line on LED 1
alone would leave LED 2 lit once the watchdog plays it.

`/opt/ghost-wispr/ghost-wispr light-state` prints the colour of the blink(1)'s LED 1 (`red`, `off`,
or `other` and its values) and `light-off` turns both LEDs off; both may run while the recorder
runs. The blink(1)
has one report buffer, and each read gets the reply to the last command that any program sent:
on the office Pi a `light-state` run while the recorder recorded made the recorder read its red
light as 0,51,51, the version reply, and drop its audio. So every command that Ghost Wispr sends,
from the recorder or from a subcommand, holds an exclusive `flock` on the blink(1)'s hidraw node
from the command to its reply, and a read takes only a reply that names its own command. A recorder
command that does not get the lock within 50 ms, or a subcommand's within 2 seconds, sends nothing:
the recorder counts such a read as the light not red, and `light-state` prints `unknown` and
fails. While another program keeps the light from the recorder, `/api/status` warns `Recording
light busy: another program is using it`, until the recorder's next look at the light gets
through. A waiting recorder gets the lock before a subcommand does, so `light-state` runs that finish
do not make the recorder drop audio; one stopped while it holds the lock does, until it is
continued. The watchdog tickle and an off write, which wait for no reply, are then sent without the
lock, so the watchdog does not fire while the recorder runs, a pause turns the blink(1) off within
500 ms whoever holds the lock, and `light-off` turns it off. A red write sends nothing. The recorder
tickles only a blink(1) whose stored pattern it knows is black: a save of the black pattern reached
it, since a read shows the pattern the light plays, not the one it saves. After a device error it
knows the same unit by its serial number and tickles it at once, whoever holds the lock; another
unit, or one with no serial number, gets off writes only until a check finds its pattern black (the
one before a red confirmation, or one a refused tickle or red write starts), and a check that finds
it black saves it once more before it knows the unit. `/opt/ghost-wispr/data/blink1-saves/` holds
two records per serial number: `<hex serial>.attempt`, written before each save is sent, after
which no save is sent for a day (and none at all if the attempt cannot be written); and
`<hex serial>.saved`, written once a save is seen to reach the unit, after which for a day a check
that finds that unit's pattern black knows it without a save. A recorder that restarts every 10 s
so writes the blink(1)'s flash at most once a day. An attempt with no `.saved` after it, as after a
crash or a failed transfer, leaves the unit unknown, so the recorder checks it at each open and
tickles it only once checked. Deleting the directory only allows one more save at the next start. That check runs before a resume
takes the pause switch, from the web page, the power button or the Jabra alike, so a pause tapped
during it is not held up, and the resume or microphone open it overtakes does not light the
blink(1). A press of the power button or the Jabra that resumes checks the light in the background,
so a press right after it is taken at once and pauses; otherwise a press reads the pause switch, so a
refused resume is tried again by the next press. While a Jabra press resumes, the Jabra's ring goes
off at once, as the button counts the unit as resumed, though the blink(1) stays off until red is
confirmed; if the light refuses the resume, the ring shows Muted again. Other programs
that open the blink(1), such as `blink1-tool`, do not take the lock: do not run them while the
recorder runs, since their own colour read can still answer the recorder's. A process stopped while
it holds the lock (`kill -STOP`) keeps it. So the Light hang step of `deploy/pi/accept-slice1.sh`
("Accept slice 1"), which stops the recorder and requires the blink(1) dark within 3 seconds, stops
it with `flock` on the node, between two of its commands.

Do not open the Jabra's microphone or speaker with another program, such as `arecord`, `aplay` or
a second Ghost Wispr, while the unit runs with the Jabra button on. Ghost Wispr cannot see that
program's stream, so it may change the light while the stream is open, and the Jabra's own Phone
Mute then comes when the other program closes it, outside every operation the guard covers: a
paused unit would resume. Turn the button off first (`GHOST_WISPR_JABRA_MUTE_BUTTON=false` in
`/opt/ghost-wispr/.env` and a restart, as above), or stop `ghost-wispr.service`. The slice 2
certificate test starts its second instance so that it cannot open the Jabra ("Accept slice 2").

The kernel's HID driver gives the Jabra's input device the HID Telephony usage Phone Mute as the
key `KEY_MICMUTE`, and the HID LED usage Mute (page 0x08, usage 0x09) as the light `LED_MUTE`.
Ghost Wispr finds the input device whose name starts with `Jabra ` and which has both (on the
office Pi, `/dev/input/event1`), reads the key there and writes the light there. With two such
Jabras plugged in, it uses the first one the kernel lists. The service user is in the `input`
group, and `99-ghost-wispr.rules` gives the Jabra's input nodes mode 0660 and group `input`.
While the Jabra is missing or not accessible, or its input node refuses the monotonic clock, the
button is off, `/api/status` warns and names which, and Ghost Wispr looks for it again every 10
seconds; the other controls still work.

Answering each tap with the light is what a Microsoft Teams client does: in the
[Teams devices general specification](https://learn.microsoft.com/en-us/microsoftteams/devices/certification-specifications)
(V5, section 5.3 "Mute Button and LED"), the device mutes its own microphone on a tap, sends Phone
Mute, and takes LED Mute from the client. Jabra's
[call-control SDK](https://sdk.jabra.com/javascript/docs/jabra-js/4.4.1/interfaces/ICallControl.html)
says its `mute` "Mutes or unmutes the microphone of the Jabra device", and that it "Should only be
used during a call". Ghost Wispr uses the Jabra outside a call, so what the Jabra does with the
light is checked on the unit. After a deploy that changes these controls, open the web page, with
the Jabra's button on (the template's setting). Then:

1. While it records (the blink(1) red), tap the Jabra's Mute button. The blink(1) goes off, the
   Jabra's ring shows Muted (red), and the page shows the pause. Wait 70 seconds: the ring still
   shows Muted. Ghost Wispr closes the microphone while paused; the Jabra's manual shows the Mute
   button "only visible when the microphone is active", and its buttons fade after a minute.
2. Tap Mute again. The blink(1) turns red, the ring goes off, and the page shows recording. Say
   a sentence at once: it appears in the live transcript. If the buttons have faded, the first tap
   may only light them; if it did not resume, tap Mute once more. A resume on the second tap
   passes; none after two taps fails. Ghost Wispr sends the light again just before the microphone
   opens, so the ring may flash red for a moment as recording resumes.
3. Press the power button briefly, twice. The blink(1) and the ring change as in steps 1 and 2.
4. Pause on the web page: the ring shows Muted. Tap the Jabra's Mute button: recording resumes,
   the ring goes off, and a sentence you say appears in the transcript, so the Jabra did not keep
   its microphone muted.
5. While paused, unplug the Jabra and plug it in again. Within about 10 seconds the ring shows
   Muted again, and recording stays paused.
6. While paused, restart Ghost Wispr with `systemctl --user restart ghost-wispr` as the
   `ghost-wispr` user. Recording stays paused, the blink(1) stays off, and the ring shows Muted.

After each step the state stays as it is: nothing pauses or resumes by itself. If any step fails,
turn the Jabra's button off with `GHOST_WISPR_JABRA_MUTE_BUTTON=false` as above, and the power
button and the web page still pause and resume. The nightly update keeps that `.env` line. To ship
the button off, set `jabra_mute_button: false` in the template, in a pull request that also changes
the deploy tests that pin it on.

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

Each night the Pi installs the latest release's whole deploy set ("The release and its deploy
set"), as one unit, with no person. `ghost-wispr-update.timer` starts `ghost-wispr-update.service`
at 03:30, at 03:45, and then every 15 minutes to 06:00 local time: 11 runs a night. With
`Persistent=true`, a start missed while the Pi was off runs once it is on again. A start during a
run joins that run.

The unit runs the launcher, `/opt/ghost-wispr/ghost-wispr-update-launcher.sh`, which runs
`/opt/ghost-wispr/update.sh` under `timeout --kill-after=60s 45m`. That is the one time limit on a
whole run: the unit sets none (`TimeoutStartSec=infinity`), and each step of `update.sh` has a
limit of its own (`pi-secrets` 60 s, each `sudo -n` call 30 s, the read of Serve 10 s, the version
check 120 s). `update.sh` exits 0 when it installed, found the release current, held it or found
the recorder busy, and 10 for each failure that it found and reported, a GitHub outage included.
Any other exit code, the time limit's 124 and 137 among them, means that `update.sh` itself broke:
the launcher then runs `/opt/ghost-wispr/update.sh.prev`, the previous release's, at once, when that
file exists, under the same limit. The launcher counts the runs that failed in a row in
`/opt/ghost-wispr/update.failed-runs`. When the count reaches 11, one night of runs, it runs
`update.sh.prev` once and sets the count to 0; an exit 0 also sets it to 0. So one network fault
runs no `update.sh.prev`.

A run of `update.sh`:

1. Changes nothing while the recorder is busy: while `/api/status` has a non-empty
   `active_session_id`, or a `call` whose `state` is not `none` (a Meet call that the call bridge of
   dispatch://INFRA-5904 links to the Pi). It reads the status before any change and again just
   before it swaps a file or restarts the recorder, and prints
   `ghost-wispr update: active session; leaving the current release in place` (or the call's
   state in place of `active session`). A later run of the night tries again.
2. Downloads the release's tarball, checks it against its `.sha256`, and checks each file against
   `manifest.json` and the manifest against the release's commit. It installs nothing when one does
   not match.
3. When the release is current, prints `ghost-wispr update: already current at commit <sha>`. The
   release is current when `/opt/ghost-wispr/deploy.verified` names its commit and each installed
   file but `ghost-wispr.yaml` has the SHA-256 that `deploy.verified` lists. Then, unless step 5
   refuses, it reads the Pi's secrets and a renewed certificate, and installs a change while the
   recorder is free ("The Pi's secrets", "Renew the X.509 certificate"). When nothing changed, it
   restarts a recorder that started before the installed `.env` or `device.crt`, as after a run cut
   short between a refresh's swap and its restart, and prints
   `ghost-wispr update: the recorder started before the installed <files>; restarting it`.
4. Otherwise, holds a release whose link versions the call bridge does not accept. The bridge's
   list is `call.link_versions` in `/api/status`, and the release's is `link_versions` in its
   manifest. When both lists are there and share no number, it prints
   `ghost-wispr update: held: ...` with both lists, and exits 0.
5. Refuses to install a release, new secrets or a renewed certificate, and exits 10, while `.env`
   or the environment of `ghost-wispr`'s user manager sets `GHOST_WISPR_CONFIG` or an Envoy override
   variable ("Turn on the meeting voice", step 1), or while `ghost-wispr.service` or
   `ghost-wispr-update.service` has a drop-in. Its line names the variable or the drop-in. Remove
   it, and the next run installs. The refresh at each start of the recorder (`update.sh secrets`)
   still writes the four lines of `.env`.
6. Installs the whole set, also when the running binary already answers the release's commit: the
   binary and the nine files, `ghost-wispr.service`, the udev rule and the logind setting included.
   It renders `ghost-wispr.yaml` with the shared drive of the installed config ("Enable models and
   Drive sync") and writes `.env` ("The Pi's secrets"). Before the swap it keeps a `.prev` copy of
   each installed file of the set that `deploy.verified` lists. It installs the two system files
   with `sudo -n`, only when they change. Then it reloads the user units, reloads udev or restarts
   logind when their file changed, restarts the recorder, and waits up to 120 s for `/api/version`
   to answer the release's commit. Then it writes `deploy.verified` and prints
   `ghost-wispr update: installed commit <sha> (invocation <id>)`. When that answer does not come,
   it puts back each file from its `.prev` copy, byte for byte, and prints
   `ghost-wispr update: <why>; restored the previous release`. It restarts the recorder for that
   rollback only while the recorder reports no session and no call, and leaves it running
   otherwise. A file with no `.prev` copy stays as installed, and the line says
   `no previous release to restore`. When the installed files were not the set `deploy.verified`
   lists, as after an install cut short, the run keeps no new `.prev` copies, since those it has
   hold the set verified last. A rollback then puts back each file that has a `.prev` copy, prints
   `ghost-wispr update: <why>; restored the last verified set, the one deploy.verified lists`, and
   exits 10, also when a session starts during the swap. When those copies are not that set, the
   line says `restored each file that has a .prev copy, but they are not the set deploy.verified lists`.
7. After an install, or with the release current, compares the three provisioning files that the
   nightly update does not install (the launcher, `ghost-wispr-update.service` and
   `ghost-wispr-update.timer`) with the manifest, and the live Tailscale Serve setting with the
   release's `serve.conf`. It prints one drift line for each difference:
   - `ghost-wispr update: provisioning file <name> differs from the release; run the release's provision.sh`
   - `ghost-wispr update: Serve differs from the release: want <lines>, have <lines>`
   - `ghost-wispr update: Serve: UNKNOWN (<why>)`, when it cannot read or parse Serve's setting

   A difference changes no exit code. Apply it with one run of the release's `provision.sh`
   ("Apply a provisioning change", below).

The Pi keeps no user journal: the lines of its user units are in the system journal. Before each
run the launcher prints `ghost-wispr launcher: invocation <id> trigger <unit> at <time>`, where the
trigger is `ghost-wispr-update.timer` for a run the timer started, and `none` for a run started by
hand. Read the update's lines on the Pi:

```bash
ssh ghost-wispr@ghost-wispr 'sudo -n journalctl _SYSTEMD_USER_UNIT=ghost-wispr-update.service --since yesterday -o cat'
```

Check what the Pi runs from a tailnet member device, in a Ghost Wispr checkout:

```bash
deploy/pi/check-deploy.sh
```

It checks the latest release, or with `--release COMMIT` the release of that commit. It reads the
release from GitHub, the recorder over Serve and the Pi over SSH, writes nothing on the Pi, and
prints `PASS`, `FAIL` or `UNKNOWN` for each of seven checks. `UNKNOWN` means that it could not read
the check's input. It exits 1 when one check is not `PASS`.

1. `/api/version` answers the release's commit.
2. Each model setting and `recording_rules` runs as the release's `ghost-wispr.yaml` gives it. A
   setting that the local settings file sets is compared with that file's value, and the line names
   it.
3. Each provider of that config signs in.
4. `/api/status` has no warning.
5. The timer started the run that printed `installed commit` for that commit: `FAIL` for a run
   started by hand.
6. The protected recordings, which no deploy step may change, still have the SHA-256 that
   `PI_PROTECTED_RECORDINGS` in `pi.env` lists for them.
7. The three provisioning files on the Pi and Serve's setting match the release.

To run an update by hand, start its unit, so the launcher's time limit applies and a start of the
timer joins it. The command returns when the run ends. Check 5 then fails for that install, which
no timer started.

```bash
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user start ghost-wispr-update.service'
```

Inspect the runtime:

```bash
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user status ghost-wispr.service'
ssh ghost-wispr@ghost-wispr 'curl -fsS http://127.0.0.1:8080/api/version'
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user list-timers ghost-wispr-update.timer'
```

### The release the Pi runs

The models step runs `provision.sh --models` from the release the Pi runs ("Enable models and Drive
sync"), which can be older than the latest release: a busy night, a held release or a failed install
leaves the Pi on an older one. When check 1 of `check-deploy.sh` passes, the Pi runs the latest
release, and the directory of "Provision", step 1, is that release. Otherwise, on the Pi as
`ghost-wispr`, download the release whose commit `/api/version` names:

<!-- runbook_blocks_test.sh: download-commit -->
```bash
c=$(curl -fsS http://127.0.0.1:8080/api/version | jq -er .commit) &&
t='' p=1 prev='' &&
while [ -z "$t" ]; do
  r=$(curl -fsS "https://api.github.com/repos/sjawhar/ghost-wispr/releases?per_page=100&page=$p") &&
    t=$(jq -r --arg c "$c" 'first(.[] | select(.target_commitish == $c) | .tag_name) // ""' <<<"$r") &&
    n=$(jq length <<<"$r") && first=$(jq -c '.[0]' <<<"$r") || break
  [ -z "$t" ] || break
  if [ "$n" -lt 100 ] || [ "$first" = "$prev" ]; then
    echo "no release names commit $c" >&2
    break
  fi
  prev=$first p=$((p + 1))
done &&
[ -n "$t" ] &&
d=$(mktemp -d) && cd "$d" &&
u="https://github.com/sjawhar/ghost-wispr/releases/download/$t" &&
curl -fsSLO "$u/ghost-wispr-pi-deploy.tar.gz" &&
curl -fsSLO "$u/ghost-wispr-pi-deploy.tar.gz.sha256" &&
sha256sum -c ghost-wispr-pi-deploy.tar.gz.sha256 &&
tar -xzf ghost-wispr-pi-deploy.tar.gz &&
cd ghost-wispr-pi-deploy
```

`sha256sum` must print `ghost-wispr-pi-deploy.tar.gz: OK`, and the shell is then in that release's
directory, in `/tmp`. The block reads GitHub's release list, newest first, 100 releases a page,
until a page holds the release of the commit, or holds fewer than 100 releases (the last page), or
repeats the page before it. When no release names the commit, it prints
`no release names commit <commit>` and stops before it makes a directory.

### Apply a provisioning change

On the Pi, download the release that carries the change, normally the latest ("Provision", step 1),
and run `./provision.sh` in its directory. On a provisioned Pi it does what step 5 of "Provision"
says: it writes no credential file and prints `credentials: same as pi.env renders`, or the names of
the fields that differ. It installs the launcher, the update unit and timer, and the release's
`update.sh`, `pi-lib.sh` and `pi.env`, starts the nightly update once, applies `serve.conf`, and
stops after "already joined".

The release can be newer than the one the Pi runs, as when it fixes a launcher, unit or timer that
kept the nightly update from installing it. The nightly update keeps `.prev` copies only of the set
that `deploy.verified` lists, and `provision.sh` installs its own release's files before it starts
the update. So when it would replace `update.sh`, `pi-lib.sh` or `pi.env`, and the installed files
are the set `deploy.verified` lists, `provision.sh` first copies that set and `.env` to their
`.prev` copies, and prints
`ghost-wispr provision: kept the set of commit <sha>, which the Pi runs, as its .prev copies, so a failed install of this release rolls back to it`.
A failed install of the newer release then rolls back to the release the Pi runs. When the installed
files are not that set, as after an install cut short, it prints
`ghost-wispr provision: the installed files are not the set /opt/ghost-wispr/deploy.verified lists (<file> differs), so its .prev copies stay as they are`:
those copies already hold the set verified last.

The models step is the one exception: `provision.sh --models` runs only from the release the Pi runs
("Enable models and Drive sync").

## A release that changes config

A release carries its config. The nightly update installs the release's whole deploy set together
("Update and inspect"): the binary, `ghost-wispr.yaml`, `ghost-wispr.service`, the udev rule, the
logind setting and the rest of the set. So a release that adds, replaces or removes a config key,
such as `recording_rules`, `mic_device`, `envoy_hub_url` or a model provider, needs no step of its
own: merge it, and the Pi installs it in the first run of the night that finds the recorder free. A
new binary never starts on an old config, and an old binary never runs on a new config. Settings
saved on the page stay in `/opt/ghost-wispr/ghost-wispr.local.yaml`, which no release changes
("Settings saved on the page").

The update restarts the recorder only while it is free. While a session or a call goes on, it
prints `ghost-wispr update: active session; leaving the current release in place` (or the call's
state), and a later run of the night tries again. When the new release does not answer its commit
within 120 s, the update puts back the whole set that ran before and prints
`...; restored the previous release`. A later run tries the release again.

A setting in `/opt/ghost-wispr/.env` overrides the YAML. The office Pi can set the Jabra's Mute
button there, with `GHOST_WISPR_JABRA_MUTE_BUTTON` ("Pause and resume at the unit"). The nightly
update writes its own four lines of `.env` and keeps every other line of `.env` but those of
variables Ghost Wispr no longer reads, so that line stays through each release. The template's
`jabra_mute_button: true` applies while no such line and no saved setting sets the button.

A release that changes a provisioning file (the launcher, `ghost-wispr-update.service`,
`ghost-wispr-update.timer` or `serve.conf`) reaches the Pi only when a person runs that release's
`provision.sh` ("Update and inspect", "Apply a provisioning change"). Until then, each night's run
prints its drift line, and check 7 of `check-deploy.sh` fails.

The morning after the release publishes, check it from a tailnet member device, in a checkout:

```bash
deploy/pi/check-deploy.sh
```

Each of its seven checks must print `PASS` ("Update and inspect"). For a release that changes
`mic_device` or the Jabra's Mute button, also check the Pi: `/api/status` must hold no
`No microphone found` warning, and `capture_state` must be `capturing` or `paused`:

```bash
curl -fsS https://ghost-wispr.tail12ec1c.ts.net/api/status | jq '{capture_state, warnings}'
```

Then check the two values the recorder runs with. `/api/config` shows neither, so the block below
reads each as the recorder resolves it: `GHOST_WISPR_MIC_DEVICE` or `GHOST_WISPR_JABRA_MUTE_BUTTON`
in the running process's environment (from `.env` or its user manager), else the local settings
file `ghost-wispr.local.yaml`, else the installed `ghost-wispr.yaml`. It prints those two settings
alone, never the process's other variables, which hold the Pi's secrets. Its lines must read
`mic_device: J75` and `jabra_mute_button: true`, each with where its value comes from.

<!-- runbook_blocks_test.sh: running-settings -->
```bash
(source "${GHOST_WISPR_PI_ENV:-deploy/pi/files/pi.env}" &&
  ssh "ghost-wispr@$PI_HOST" 'bash -s' <<'EOF'
d=/opt/ghost-wispr
pid=$(XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user show -p MainPID --value ghost-wispr.service)
[ "${pid:-0}" != 0 ] || { echo 'ghost-wispr.service runs no process' >&2; exit 1; }
environment=$(tr '\0' '\n' <"/proc/$pid/environ") || exit 1
for setting in mic_device:GHOST_WISPR_MIC_DEVICE jabra_mute_button:GHOST_WISPR_JABRA_MUTE_BUTTON; do
  key=${setting%%:*} variable=${setting#*:}
  value=$(sed -n "s/^$variable=//p" <<<"$environment" | tail -n 1)
  from="$variable in the recorder's environment"
  if [ -n "$value" ] && [ "$key" = jabra_mute_button ]; then
    case ${value,,} in true | 1) value=true ;; *) value=false ;; esac
  fi
  if [ -z "$value" ]; then
    value=$(sed -n "s/^$key: *//p" "$d/ghost-wispr.local.yaml" 2>/dev/null) from=ghost-wispr.local.yaml
  fi
  if [ -z "$value" ]; then
    value=$(sed -n "s/^$key: *//p" "$d/ghost-wispr.yaml") from=ghost-wispr.yaml
  fi
  echo "$key: ${value:-unset} (from $from)"
done
EOF
)
```

When the new release answers its commit but check 2, 3 or 4 fails, revert the change on `main`. The
revert publishes a new release, which the nightly update installs that night, or at once when you
start the update by hand:

```bash
ssh ghost-wispr@ghost-wispr 'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user start ghost-wispr-update.service'
```

Do not restore the `.prev` copies by hand: the next run finds installed files that differ from the
set `deploy.verified` lists, and installs the latest release again.

## Settings saved on the page

A setting saved on the web UI's settings page goes to the local settings file,
`/opt/ghost-wispr/ghost-wispr.local.yaml`, never to `ghost-wispr.yaml`. Ghost Wispr reads that file
over the release's config (README, "Configuration"). No release holds the file, and the nightly
update never changes it, so a saved setting stays through each release. Check 2 of
`check-deploy.sh` compares a setting that the file sets with the file's value, and names it.
Recording rules cannot be changed on the page.

To drop every saved setting, remove the file and restart the recorder:

```bash
ssh ghost-wispr@ghost-wispr 'rm -- /opt/ghost-wispr/ghost-wispr.local.yaml && XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user restart ghost-wispr.service'
```

The first install of a deploy set on a Pi, with no `/opt/ghost-wispr/deploy.verified`, replaces the
config that the Pi had, and with it each setting that an older build saved into `ghost-wispr.yaml`
before the local settings file existed. The update keeps that config as
`/opt/ghost-wispr/ghost-wispr.yaml.before-release-config`. Read a lost setting there, and save it
again on the page.

## The settings page's providers

The web UI's settings page lists the Pi's four providers, each with its API, how it signs in, the
roles that use it and whether it signed in when last checked (README, "The settings page and the
API"): `vertex` and `vertex-us` with the Pi's Google workload identity, `openai` with OpenAI
workload identity, and `deepgram` with its key from `GHOST_WISPR_DEEPGRAM_API_KEY`. Every provider
must show that it signed in. A check is kept for 5 minutes; `GET /api/providers?refresh=1` checks
every provider at once. The `deepgram` key is read-only (`auth.read_only: true` in the template):
the page shows no key field for it, only "This unit reads its key from its deployment. It cannot be
changed here.", and `PATCH /api/config` with its key answers 400 and saves nothing. The key comes
from Secret Manager, and a new version of it reaches the Pi as "The Pi's secrets" says.

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

## Inject speech for acceptance

The rows that need speech (slice 2's A6 and A8, slice 3) can run on synthesized speech that
bypasses the microphone: speech played through the Jabra's own speaker does not reach its
recording, because its echo cancellation removes it. Ghost Wispr records only the sound card
`mic_device` names, so for the run point it at the ALSA loopback, and back afterwards. Run these
steps on the Pi as `ghost-wispr`, outside meetings, with no session active.

1. Load the loopback: `sudo modprobe snd-aloop`. `grep Loopback /proc/asound/cards` shows its card.
   While `mic_device: J75` applies, Ghost Wispr ignores that card.
2. Add `GHOST_WISPR_MIC_DEVICE=Loopback` to `/opt/ghost-wispr/.env`, which overrides the YAML, and
   restart `ghost-wispr.service`. Ghost Wispr then captures from the loopback's device 0, under the
   same light rule as on the Jabra.
3. Play each conversation into the loopback's device 1, whose sound comes out of device 0:
   `aplay -D plughw:CARD=Loopback,DEV=1 <conversation>.wav`.
4. Afterwards, remove the `GHOST_WISPR_MIC_DEVICE` line from `.env`, restart `ghost-wispr.service`,
   and unload the loopback: `sudo modprobe -r snd-aloop`. Then `/api/status` must show
   `capture_state` `capturing` (or `paused`) with no `No microphone found` warning: the Jabra is
   the microphone again.

## Turn on the meeting voice

The template turns the meeting voice on (`voice.enabled: true`; README, "Meeting voice"): "Wispr,
what is happening with …?" gets a chime, then voice A (text-to-speech) speaks the answer from Dispatch's record
with the template's text-to-speech, `tts.model: vertex-us/gemini-3.8-flash-tts` and `tts.voice:
Kore`: Gemini TTS's Kore voice, streamed from Vertex AI's `us` multi-region with the Pi's own
Google identity. The fifth recording rule tells the room that a question sent to an agent leaves
the unit.

1. Reach. The Concierge reads `concierge.dispatch_url` and registers with the Envoy listener at
   `concierge.envoy_url`. The Pi reaches neither until the tailnet grant of dispatch://INFRA-4617
   is applied; until then the voice answers "I cannot reach Dispatch yet", and the log warns every
   minute that the Envoy session did not open, with no restart needed once it can. The template
   sets `envoy_enabled: true`, and `envoy_hub_url` and `concierge.envoy_url` to the hub's listener.
   A `.env` line overrides the YAML, so the nightly update refuses to install a release, new secrets
   or a renewed certificate, and the models step (`provision.sh --models`) refuses and changes
   nothing, while a line of the Pi's `.env` could set `GHOST_WISPR_ENVOY_ENABLED`,
   `GHOST_WISPR_ENVOY_HUB_URL` or
   `GHOST_WISPR_CONCIERGE_ENVOY_URL`, or `GHOST_WISPR_CONFIG`, which would have the recorder load
   another file than the installed config and so replace every setting, Envoy's included: a line
   that, past leading space and an `export`, starts with the name and `=`, where a carriage return
   ends a line as a line feed does, as systemd reads `.env`. Each refuses the same way while
   `ghost-wispr`'s user manager sets one of them in its own environment
   (`systemctl --user set-environment` or `import-environment`, an `environment.d` file,
   `DefaultEnvironment=`, the manager's own start-up environment), which the recorder inherits: it
   reads the names from `systemctl --user show-environment`, and refuses too when it cannot read
   that environment or reads an empty one. A variable from `set-environment` or `import-environment`
   goes with `systemctl --user unset-environment NAME`; one from an `environment.d` or `user.conf`
   line goes once you remove the line and run `systemctl --user daemon-reload`; one from the
   manager's start-up environment (`/etc/environment`, a `user@.service` drop-in) goes once you
   remove it and restart `user@<uid>.service`. Each reloads the manager before it reads, so a newly
   written `environment.d` line counts. `provision.sh` refuses the same way while the manager's
   environment sets one of them, before it changes anything. The models step and `provision.sh`
   name `GHOST_WISPR_CONFIG` first, with why; the nightly update names each variable it found.
   The refresh at each start of the recorder (`update.sh secrets`) still writes the four lines of
   `.env`. While a config names a hub with Envoy off, the web page shows a notice
   under the warnings, and `/api/status` gives it in `notices`, never in `warnings`, which check 4
   of `check-deploy.sh` needs empty: no session event goes to the hub, each completed summary waits
   until Envoy is on again, and, with no `concierge.envoy_url`, the voice cannot ask an issue's
   agent.
2. The Pi's Dispatch token: a no-scope agent token, never a person's, which the nightly update
   reads from Secret Manager into `.env` as `GHOST_WISPR_DISPATCH_TOKEN`. "The Pi's secrets" says
   how to mint it into its secret, and how to replace it. Without it the voice answers "I cannot
   reach Dispatch yet".
3. After the deploy, `/api/status` must hold no `voice` warning, and the running process must
   have logged `voice: ready (wake phrases ["Wispr" "Ghost Wispr"]; ...)` and
   `concierge: the Envoy session is open`. The service writes JSON records, so the journal holds
   the first with its quotes escaped, `voice: ready (wake phrases [\"Wispr\" \"Ghost Wispr\"]; ...)`,
   and it keeps the lines of the processes before the deploy. This prints the `message` of each
   record the running process wrote, its quotes decoded:

   ```bash
   ssh ghost-wispr@ghost-wispr 'pid=$(XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user show -p MainPID --value ghost-wispr.service) &&
     sudo journalctl _SYSTEMD_USER_UNIT=ghost-wispr.service _PID="$pid" -o cat | jq -R -r "fromjson? | .message // empty"'
   ```

   The voice reads `voice.enabled` and `voice.engine` at each call, so the journal names no
   voice: `/api/config` shows which one speaks (`"voice":{"enabled":true,"engine":"tts"}` with
   `"tts":{"model":"vertex-us/gemini-3.8-flash-tts","voice":"Kore"}` is voice A, text-to-speech, on Gemini TTS).
4. Time to first audio of the first sentence: the Gemini TTS engine warms its connection when the voice
   opens, at the wake. Measure it once on the Pi, with and without the warm-up, using the voice
   package's live test (`GHOST_WISPR_LIVE_TTS=gemini ... TestLiveFirstSayTimeToFirstAudio`, built for
   arm64); keep the warm-up only if it lowers the median of the first sentence.

## Probe voice B (GPT-Live)

`gptlive-probe` sends only silence and its own question WAV over its
WebSocket. It opens no microphone or speaker. It still must never run while
the Pi records or a session is active.

From the laptop or devbox, check the unit **before you copy or run
anything**:

```bash
PI='ssh -o BatchMode=yes -o IdentityAgent=/run/user/1000/gcr/ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net'
$PI 'curl -fsS localhost:8080/api/status | jq -e ".paused == true and .capturing == false and .active_session_id == \"\"" >/dev/null' ||
  { echo 'the unit is not paused, not idle or still captures; do not copy or run the probe' >&2; exit 1; }
```

The program reads the same endpoint at its start, and again after
`session.started` before it feeds its first audio frame. Those checks are
the final guard; the pre-copy check prevents an accidental probe file from
reaching a meeting.

Build the arm64 binary on `sami-devbox`, copy the binary and a 16 kHz mono
question WAV into a private `mktemp -d` on the Pi, and run it as
`ghost-wispr`, with the workload identity ids of the `openai` provider in
`deploy/pi/files/ghost-wispr.yaml`:

```bash
sudo -u ghost-wispr ./gptlive-probe \
  --identity-provider-id idp_b2d6c7c4ac719ef0764eba89 \
  --service-account-id user-cd72c7d9a1bb41594ed3c6e7 \
  --ask question.wav
```

It first runs a wrong-audience control, then checks the token exchange,
session, three spoken turns, the delegated question and the backend answer.
It exits nonzero if the unit stops being paused and idle, the backend answer
is not spoken, or a speech's first word reaches the own-speech filter more
than 300 ms after its audio starts. Remove the private directory afterwards.

## Accept the meeting voice

`deploy/pi/accept-voice.sh` runs slice 1 of the meeting voice on the loopback, so nothing plays in
the room and nothing of the room is recorded. The Pi's release does not hold the driver, so copy it
to the Pi from a tailnet member device, in a checkout of the commit the Pi runs (its
`/api/version`), with `recording-times.awk`, which the driver reads beside itself:

```bash
d=$(ssh ghost-wispr@ghost-wispr 'mktemp -d') &&
scp deploy/pi/accept-voice.sh deploy/pi/recording-times.awk "ghost-wispr@ghost-wispr:$d/" &&
printf '%s\n' "$d"
```

Run the steps below on the Pi as `ghost-wispr`, in the directory that the block printed, outside
meetings, with the unit paused and no session active. The driver needs `curl`, `jq` and `python3`
on the Pi, and refuses when one is missing. Remove the directory afterwards
(`ssh ghost-wispr@ghost-wispr "rm -r -- '$d'"`).

1. `sudo modprobe snd-aloop`, as in "Inject speech for acceptance".
2. Make a WAV of the question, such as "Wispr, what is happening with the issue about the meeting
   voice?", and optionally one of "Wispr, go ahead.", with any text-to-speech engine.
3. `bash accept-voice.sh --question question.wav [--go-ahead go-ahead.wav]`. It switches `.env` to the
   loopback (microphone `Loopback`, speaker `plughw:CARD=Loopback,DEV=0,SUBDEV=1`, voice on),
   restarts the service, resumes, plays the question in, and records the speaker's side. It prints
   one `PASS` or `FAIL` line for each of `voice_wake`, `voice_exchange_started` and `voice_spoken` on
   `/ws` (session id only, no words), the chime after the wake (at most 400 ms) and the speech after
   the question (at most 3000 ms), and with `--go-ahead`, `voice_answer_ready` and the held answer.
   `UNKNOWN` means it read nothing.
4. Whatever the result, and on Ctrl-C, it pauses capture, stops the service, writes `.env` back
   byte for byte and the saved capture state it read, starts the service and checks it reads paused.
   Then unload the loopback: `sudo modprobe -r snd-aloop`.
5. The after-merge checks of slice 1 each run one step: `bash accept-voice.sh --step NAME ...`;
   `--help` lists the steps and what each needs. They need more inputs than the question:
   - spoken clips made with any speech synthesizer: `--second-question`, `--never-mind` and
     `--stop-clip` WAVs, and the `--question-dir`, `--talkover-dir` and `--backchannel-dir`
     directories of WAVs;
   - for `wake-sound`, `scripted`, `barge-in`, `playback-cancel` and `joined`, a USB microphone
     about 30 cm from the Jabra, named by `--rig-mic` (such as `plughw:CARD=Mic,DEV=0`). These
     steps play through the Jabra, so run them with the room empty. The step asks you to confirm
     that, measures the room and sets its sound threshold 10 dB above the room's peak;
   - for `holder-cases` and `ask-holder`, scratch issues in Dispatch: one claimed by a live agent
     session (`--issue-live`), one by a stopped session (`--issue-stopped`), one by a person
     (`--issue-person`) and one with no session (`--issue-none`). The steps only read them, and ask
     the live agent one question.

   `reach`, `holder-cases` and `tts-first-audio` change nothing on the unit; the others restore it as
   in point 4. `holder-cases` runs `ghost-wispr issue-holder KEY`, which prints the Concierge's
   answer for the issue's holder without opening an Envoy session; `tts-first-audio` runs
   `ghost-wispr tts-first-audio --model PROVIDER/MODEL` for each `--model` (by default the unit's
   `tts.model`), which times each sentence to its first audio frame and plays nothing. To compare
   Vertex AI's locations and Aura, and run the whole-clip control:

   ```bash
   bash accept-voice.sh --step tts-first-audio --model vertex-us/gemini-3.8-flash-tts \
     --model vertex/gemini-3.8-flash-tts --model deepgram/aura-2-harmonia-en --clip
   ```

   `playback-cancel` starts with `ghost-wispr play-sound wake`, which plays the
   wake sound once on the speaker as the voice plays it and returns once the speaker's stream has
   closed: how long the rig hears that sound alone is the step's calibration, and a chime after a
   stop may be heard at most 20 ms longer. The steps that read the service's log lines (the voice
   timing lines, the speaker's stream and underrun lines, the Concierge's cancel line) read the
   system journal with
   `sudo -n journalctl _SYSTEMD_USER_UNIT=ghost-wispr.service _PID=<MainPID> --boot=0`, the
   running process's lines in the current boot: the Pi keeps no user journal, and a process ID can
   recur after a reboot. The underrun check reports UNKNOWN if its journal read fails. The windows
   and thresholds of the rig steps are not yet calibrated: the first run on the Pi calibrates them.
   The driver reads each recording itself, with Python's `wave` module, in exact sample indices at
   any length; it needs no ffmpeg, whose printed times are good only to 10 ms past 1000 s.
   Every time the rig steps take from a recording lies between two bounds. The late bound comes
   from the anchors that tie the file to the Pi's clock, and is late by however late the driver
   read them. The early bound comes from the recording's start: the driver's last read of the
   file's size before arecord wrote its header, since arecord writes its whole header before the
   read that starts its capture. It is early by up to the time between two reads (10 ms on an idle
   Pi), the time arecord took from its header to its first read, and up to 400 ppm of the time into
   the recording. A read of the size that fails moves the start nowhere. When `/proc/asound` shows
   the capture substream arecord holds, every read of the size also bounds the times after it from
   below, while continuity holds: arecord reports no overrun and no suspend (it runs without `-q`,
   so that it prints both, and with `LC_ALL=C`, so that it prints them in the English the driver
   reads whatever the caller's locale), and each read of the size is followed by a read of the substream's
   state that shows it `RUNNING`, from the `trigger_time` of the first such read. arecord may not
   live to report an overrun, as when the step stops it in a stalled write and it closes the
   substream, and the kernel sets `trigger_time` at every start, stop, suspend and resume. It is
   compared exactly, as the kernel prints it: whole seconds (no leading zero, within signed 64
   bits) and nine digits of nanoseconds. A state read that shows anything else, another trigger,
   or no `trigger_time` in that form (which can show no restart) turns this bound off for the
   whole recording. The first `trigger_time` counts only for samples of its own run, so before the
   first samples every read of the size is preceded by a state read too, and each must show no
   substream, one never started (`trigger_time` 0, as the kernel shows it before the first start),
   or `RUNNING` from the trigger the first anchor's read shows; and samples that land before the
   sampler knows arecord's process, and so before it can read the state at all, turn the bound off
   too. A restart that lands between the last read that found no sample and the state read before
   the next stays unseen; it needs a start, a 10 ms chunk, a stall past the buffer (40 ms) and a new
   start within it, so it needs the sampler about 40 ms late for one of its reads 10 ms apart, and
   arecord's overrun line still turns the bound off there whenever arecord lives to print it.
   No substream at all is arecord closing its capture as it ends, which it does after its last
   write: it turns the bound off only if the file grows after it. A sample not yet in the file was
   captured at most two periods and the buffer before (61 ms with arecord's 40 ms buffer and 10 ms
   periods). That includes 1 ms for the interrupt that delivers a period, an assumption about the
   Pi, not a measured bound. The size
   read is the file's length, never arecord's offset in it: arecord seeks back to rewrite the
   header as it ends, and a read that finds only the header counts only before the first sample.
   The recording's end lies between the start plus its length
   and the last anchor, and a silence's length is never longer than its length in the file.
   Every check passes or fails only when its verdict holds for every time between the bounds, and
   otherwise reports UNKNOWN with how late the recording's times may be. `playback-cancel` passes a
   stop when the late time of its tail is within 20 ms, and fails it when even the early time is
   over. `wake-sound`, `scripted` and the exchange step's chime and speech pass a delay when its late
   bound is within the bound, and fail it when its early bound is over. `barge-in` counts a round as
   stopped or as speech that went on only when the bounds decide it. A backchannel that may have
   stopped the speech counts against the allowance, and a talk-over that may not have stopped it
   makes the step UNKNOWN. `joined` passes when even the early time of the words' onset is at or
   after the end of the go-ahead and the late time is before `voice_spoken`. It fails when no sound
   longer than the wake sound can start between them, and otherwise reports UNKNOWN.
   So an arecord slow to open its device makes no time early, and one held back between its header
   and its first read makes no time early by more than those 61 ms, unless it overran or the
   substream is not shown: then these steps can read UNKNOWN.
   The clips the driver plays are timed the same way. A clip begins no earlier than when the
   driver starts `aplay`, and ends no later than when `aplay` returns, since `aplay` returns only
   after it drains the clip. Its WAV length gives the other bound of each: the clip begins no later
   than its length before `aplay` returned, and ends no earlier than its length after `aplay` began.
   So the clips must be WAV files. A delay from a clip's end, such as the exchange step's speech,
   `scripted`'s first fact and `joined`'s 1 s gap before the hand sound, and a delay from a clip's
   start, such as `barge-in`'s talk-over median, lies between the two. `aplay`'s own start and exit
   on a loaded Pi widen the bounds and can make these checks UNKNOWN.
   Every time the driver compares is on the Pi's monotonic clock (`CLOCK_MONOTONIC`): its own,
   read by a Python process, and the voice's, which each timing line, `/ws` voice event and test
   agent step carries as `at_mono_ns`. A step of the system clock (an NTP step, `settimeofday`)
   moves none of them, so it changes no verdict. The driver uses the wall clock only for
   journalctl's `--since`. A build whose events lack `at_mono_ns` reads UNKNOWN.
   The rig's times are when each sound reached the rig microphone. A USB microphone captures a
   sound about 1 to 2 ms after it arrives: its converter's filter and the USB audio class transfer,
   which moves audio in 1 ms frames. The driver allows 2 ms for that delay: every early bound comes
   2 ms earlier, and every silence's lower bound 2 ms shorter. So a time within 2 ms of a bound reads
   UNKNOWN: words that begin less than 2 ms after the end of the go-ahead do not pass `joined`, and a
   playback-cancel tail that may be 18 to 20 ms is not a FAIL. The 2 ms is an assumption about the
   rig, not a measured bound. A microphone slower than that can make an early bound late by the
   difference.
   `playback-cancel` also checks that nothing starts after the next question's chime before its
   answer can be heard. The service logs an `audible` timing line as each sound on the chime or the
   voice starts: when its first sample left the speaker's buffer, by the stream's own account (its
   delay, read while it runs, with the clock read around it), how much earlier a resampled voice can
   sound (its filter's span), how far off the time can be, and the disruptions (replays, underruns,
   failures) the speaker had had. The rig hears both sounds a latency after that, the same for both,
   so the earliest the answer can be heard is the chime's onset in the same recording plus the time
   between their audible lines, counted in samples. A leak that starts within about 9 ms of the
   answer is not told apart from it: 6 ms for the chime's 5 ms attack, 1 ms for each audible line,
   about 1.4 ms for the voice's filter, and 200 ppm. The 1 ms is an assumption about the USB speaker,
   which moves audio in 1 ms frames, not a measured bound. A disruption between the two lines can
   make the chime play later than its line says, so the check then falls back to the answer's
   `first_audio`, with a NOTE, as it does for a build without the lines: a leak within the speaker's
   queue before `first_audio`, about 40 ms, is then not seen. After an overrun the recording's
   times may not tell which of two sounds near the chime is the chime. Between a stop and the next
   answer the rig may hear only that chime, so two sounds that each end more than 20 ms after the
   stop and start before the answer's `first_audio` fail the stop: one of them is audio of the
   cancelled answer, whichever is the chime.
6. Slice 4's two steps check the three capture states and the paused listener, with the
   listener turned on for the run only (`GHOST_WISPR_VOICE_PAUSED_LISTENER=true`, beside the
   voice). Both keep the microphone on the loopback, so the listener hears only what the step plays
   in, and the room is never captured:
   - `--step states --phrase start.wav --speech stop.wav` needs a person at the Pi. It moves the
     unit through every cell of the control table with the page's controls, `POST /api/close`,
     the phrase played in, and the person's presses of the power button and the Jabra's mute
     button, which it asks for on the terminal. After each move it reads `/api/status`, the light
     (`ghost-wispr light-state`), the saved state, whether the loopback's capture device is open,
     and asks the person whether the Jabra's ring shows Muted. In listening it checks that the
     service holds no socket to Deepgram or OpenAI, by the certificate each port-443 peer serves,
     and that its sockets to the Envoy hub send at most 2048 bytes while `--speech` plays for
     `--hub-seconds`. It also reads `LimitCORE=0` and `MemorySwapMax=0` from the unit.
   - `--step listener-cpu --speech speech.wav` reads the whole process's CPU time from
     `/proc/<MainPID>/stat` for `--cpu-seconds` (600 by default) in listening and again in closed,
     each quiet and with `--speech` played in, and passes when the listener's share of one core is
     at most 25 % in both. `--threads 4` runs the spotter on four threads: the check's control,
     whose reading must be at least twice the one-thread reading.

## Accept slice 1

Run the acceptance driver from a tailnet member device, in a Ghost Wispr checkout, after the
tailnet policy of "Prerequisites" (step 4) applies. The driver reads the Pi's settings from
`deploy/pi/files/pi.env`.

<!-- runbook_blocks_test.sh: accept -->
```bash
deploy/pi/accept-slice1.sh
```

The driver pauses and resumes the recorder, asks the operator to power-cycle it, checks the
light state, verifies the tagged identity and loopback listeners, and checks that the Pi reaches
the Envoy hub its config names (`envoy_hub_url`, `GET /healthz` from the Pi) and that
`/api/status` holds no Envoy warning. Before the pause it requires the Ghost Wispr Deepgram socket to grow by
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
and confirm a real `session.started` and `session.ended` notification.

A2 asks a person to open the unit's page and choose Recording rules. The page shows the rules
under `recording_rules` in the Pi's config, `/opt/ghost-wispr/ghost-wispr.yaml`, which
the nightly update renders from the release's `ghost-wispr.yaml`: each must appear verbatim and in
order. A unit whose config has no rules shows no Recording rules button.

A5 asks a person to reach the recorder from a phone that is off the tailnet and from a device that
carries `PI_DENIED_TAG`; both must fail. The Hindsight memory server's control-plane pods carry that
tag, in namespace `legion` of the `dispatch` GKE cluster. The tailscaled in their `tailscale`
container listens on the socket `/scratch/tailscaled.sock`, so the CLI there needs `--socket`. Run
the probe on a tailnet member whose kubectl context is that cluster and on which
`kubectl -n legion auth can-i create pods --subresource=exec` answers `yes`:

```bash
source "${GHOST_WISPR_PI_ENV:-deploy/pi/files/pi.env}"
recorder=$(tailscale ip -4 "${PI_HOST:?}")
member=$(tailscale ip -4)
pod=$(kubectl -n legion get pods -l app=hindsight-control-plane --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
ts() { kubectl -n legion exec "${pod:?}" -c tailscale -- tailscale --socket=/scratch/tailscaled.sock "$@"; }
netmap() { jq --arg tag "$PI_DENIED_TAG" --arg ip "${recorder:?}" \
  '{carries_denied_tag: (.Self.Tags // [] | index($tag) != null), online: .Self.Online, recorder_peers: [.Peer // {} | .[] | select(.TailscaleIPs | index($ip)) | {HostName, DNSName, Tags}]}'; }
tailscale status --json | netmap
ts status --json | netmap
ts ping --until-direct=false -c 3 "${member:?}"
ts ping -c 3 "${recorder:?}"
```

When a lookup finds nothing, each command that needs it prints `parameter null or not set` and
does not run. The first `netmap`, of this member device, is the control for the filter: its
`recorder_peers` must hold one device, whose `DNSName` is `PI_HOST` with a final dot and whose
`Tags` are `["tag:ghost-wispr"]`. The pod must carry the tag and be `online`, which means it is
connected to the coordination server and its netmap is current. Its `recorder_peers` must be
empty. The first ping is the control for the pod: it must get a pong from this member device and
exit 0, so kubectl prints no `command terminated` line. With `--until-direct=false`, pongs through
a DERP relay count too. The second ping must then say `no matching peer`.

## Accept slice 2

Run both from a tailnet member after the models step. The self-test plants a
service-account key, a key body inside binary data, a sweep that missed its control, Drive
sync disabled, a missing Drive folder, a status warning, a superseded model, the embedding model
on another provider, swapped batch and summarization models, deployed providers that differ from
the plan's, a removed setting at any depth of
the YAML, a provider-model mismatch, a running model that differs from the YAML, a running
provider that signs in otherwise than the plan or that the plan does not name, an environment
override of a model setting, a removed variable in the service environment, an unapproved Drive
writer, and a status with no
certificate warning; each planted condition must be rejected before the live driver runs. The
self-test also checks `deploy/pi/files/ghost-wispr.yaml` itself.

```bash
deploy/pi/accept-slice2.sh --self-test
deploy/pi/accept-slice2.sh --drive-members "$members_file" --readers-group "$readers_group" --cert-test
```

`--drive-members FILE` is the shared drive's member list, as the drive's Manager reads it on its
"Manage members" page in Google Drive (the app's `drive.file` scope cannot read it), one
`email|role` line each with Drive API role names (`organizer`, `fileOrganizer`, `writer`,
`commenter`, `reader`). `--readers-group EMAIL` is the readers' Google group that the drive's
Manager added as its one Viewer.
`--cert-test` reads `/api/status` from a test instance on the Pi
at `127.0.0.1:8081`. The binary has no `--config` flag. Put the test credential file and
its certificate config in a private temporary directory, point the copied certificate config
at the certificate that expires in 10 days, and use a copied YAML that sets its
`google_credentials_file`, separate `db_path` and `audio_dir`, and disables Drive sync, GC,
Envoy, speaker, indicator light, power button, and Jabra mute button. Before starting it, seed its
separate database as paused, and do not resume it: a paused instance opens the microphone, and
connects to Deepgram, only when someone resumes it.

The test instance must not open the Jabra at all. PortAudio opens every audio device once when an
instance first starts its microphone: at start-up, or, for an instance seeded paused, at its
first resume. The recorder, running beside it with the Jabra button on, cannot see that stream: a
light change while it is open makes the Jabra send Phone Mute when it closes, which the recorder
reads as a tap. So start it with no supplementary groups: the Pi's sound devices are
`root:audio` and its input nodes `root:input`, both mode 0660, so it can open neither. Also set
`GHOST_WISPR_JABRA_MUTE_BUTTON=false`, which overrides the YAML. Seed and start it like this:

<!-- accept_slice2_test.sh: cert-test instance -->
```bash
GHOST_WISPR_CONFIG="$test_dir/ghost-wispr.yaml" \
/opt/ghost-wispr/ghost-wispr seed-paused-state
sudo setpriv --reuid=ghost-wispr --regid=ghost-wispr --clear-groups \
  env HOME=/home/ghost-wispr \
  GHOST_WISPR_CONFIG="$test_dir/ghost-wispr.yaml" \
  GHOST_WISPR_ADDR=127.0.0.1:8081 \
  GHOST_WISPR_JABRA_MUTE_BUTTON=false \
  /opt/ghost-wispr/ghost-wispr
```

`seed-paused-state` resolves the config and its `db_path` exactly as the recorder does
(`GHOST_WISPR_CONFIG`, else `ghost-wispr.yaml` in the working directory; a relative `db_path`
against the working directory) and prints the absolute database path it wrote. Check that the
path is the copy's database, not the production one under `/opt/ghost-wispr`.

Then run the driver with `--cert-test`. Without that flag, the row prints `NEEDS-PERSON`. With
it, the row first reads the process that listens on `127.0.0.1:8081` and fails unless all its
user and group ids are `ghost-wispr`'s, it holds no capabilities and no supplementary groups, runs
with `GHOST_WISPR_JABRA_MUTE_BUTTON=false`, and holds no sound or input device. It also fails if
`ghost-wispr` with no groups can open any sound or input device, as it can while someone is logged
in at the Pi's own console as `ghost-wispr`: the console session gives that user ACLs on them.

The live driver reports `RAN`, `FAILED`, or `NEEDS-PERSON` for each slice 2 row.

- A9 plants a service-account control in `/tmp`, sweeps the whole root once, and requires
  the sweep to find that control and no other key material; a binary file that names a key
  field without a key body (the `ghost-wispr` binary's Go struct tags) is printed as a note.
  The configured credential must be an external-account X.509 credential.
- A6 requires `/api/config` to show Drive sync on, a folder ID, credentials present, and GC
  enabled with no age limit and the 20000 MB audio budget. After the real session ends, it
  requires `summary.ready`, a synced Drive folder, and a keyword-search result for that session.
- A8 requires `/api/status` to report no warnings, and the running process's `/api/config` to
  name the plan's provider/model for every role under `models` (live and batch transcription,
  summaries, embeddings, the Concierge, text-to-speech and its voice, voices B and C), and under
  `providers` to sign in to each provider as the plan says (Vertex AI in the Pi's project at
  `global` and `us`, OpenAI by workload identity, Deepgram with its key) and to name no other: a
  YAML edited after the service started does not change what runs. It also requires
  the deployed YAML to name the roles and exactly the plan's providers, each as the template sets
  it, and to set no removed setting, and the service environment to
  override none of them and set no removed variable, which is what the next start runs. It then
  starts one real session and calls summary, semantic search, speech, and batch refinement, and
  compares the summary's `summary_model`, semantic-search `model`, speech response headers and
  status model/voice, and refinement `refinement_model` with the model of each role's
  provider/model. A provider that does not report its model produces `not reported by provider`.
  The spec asks that the unit run the plan's models, so A8 passes that role on the model the
  running process reports for it.
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
token or key reaches a `curl` argument or the evidence, or if a Deepgram request does not send
`mip_opt_out=true`.

```bash
deploy/pi/accept-slice3.sh --self-test
deploy/pi/accept-slice3.sh
```

The driver uses `ssh ghost-wispr@ghost-wispr.tail12ec1c.ts.net`. It reaches Drive with the
Pi's own credential: on the Pi, it exchanges the X.509 certificate for a token and impersonates
`ghost-wispr@theorem-agent-infra.iam.gserviceaccount.com` with the read-only Drive scope. It
transcribes recordings on the Pi with the Deepgram key in `/opt/ghost-wispr/.env`, and opts each
request out of Deepgram's Model Improvement Program (`mip_opt_out=true`), as Ghost Wispr does.
Neither credential leaves the Pi, and the driver prints neither.

One run records one conversation for both rows:

1. The mode guard sends `DELETE` for a segment of a session that does not exist, once without
   a mode and once with `mode=bogus`. Both must answer 400 with an error that names `text` and
   `text_and_audio`. A build without the modes would read `mode=text` as "Delete text and
   audio", so the driver stops here, before it records anything.
2. The precheck requires that no conversation and no file in the database's directory already
   holds "orange tractor ninety" or "purple elephant seventeen" as adjacent words. If one does,
   delete that conversation from the unit and run again. It also notes which of "orange",
   "tractor", "purple" and "elephant" no file holds yet: only those are counted on their own
   after each delete (see the database check below), so another conversation that says "orange
   juice" neither stops the run nor fails it. Record no other conversation until the run ends:
   a counted word said in one reads as a word the delete left.
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
  again. A `NOTE` line says what a round did not check and why (for example, no word to count on
  its own with `--session`); it changes no verdict and is never counted as a check that passed.
- After a delete, a read that is empty, that failed, or that does not hold "blue harbor
  lantern" is UNKNOWN, not PASS.
- A11 text only checks the segments, the transcript, the refined transcript, the summary, the
  UI search and, with `grep` while Ghost Wispr runs, every file in the database's directory: the
  database, its write-ahead log and the pre-migration backup. It exports the Drive
  `transcript.md` and `summary.md` as text, and requires the recording's sha256 on the unit and
  every Drive file id to stay the same, each id still answering 200.
- A11 checks the segments, the transcript, the refined transcript, the summary, the title, the
  UI search, the same database files as A11 text only, a Deepgram transcription of the unit's
  recording and the Drive folder name. It exports the new `transcript.md` and `summary.md`, and
  downloads and transcribes the new recording. Each old file id must answer 404 to `files.get`
  and `revisions.list`, and each new file must hold one revision. It also checks that "orange
  tractor ninety" stays gone, the database files included.
- The database check counts each phrase twice in every file: as adjacent words, as a
  conversation's rows hold it, and each distinctive word on its own ("orange" and "tractor",
  "purple" and "elephant"; the number is left out, because live transcription may write it as
  digits), as SQLite's search index holds it: each word as its own term, in sorted order, never
  next to its neighbour. A word matches only where a word starts, so "contractor" is not
  "tractor". Only the words the precheck found in no file are counted on their own, because
  `grep` cannot tell which conversation a word in a binary file belongs to; with `--session` the
  precheck does not run, and no word is. On an index page each term after the first is stored as
  the part that differs from the term before it, so a word that shares its first letters with
  its neighbour can still escape `grep`. The UI search cannot see what is left in the index
  either: it joins the live rows, and a deleted row has none. So an A11 PASS does not show that
  the index is clean. What shows it is the delete's rebuild of the index and its Go tests
  (`internal/storage/search_index_erase_test.go`), which read the database, its log and the
  backup for a one-word secret.
