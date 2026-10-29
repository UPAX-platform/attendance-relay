# UPAX attendance relay

`upax-relay` connects attendance terminals on your local network to UPAX. It
runs on any always-on computer at the site (a PC, a Mac or a Raspberry Pi),
reads punches from the terminals over the LAN, and sends them to UPAX over
HTTPS. Every request is signed with a key that only this computer holds.

```
 terminals ──LAN──▶  upax-relay  ──HTTPS, signed──▶  UPAX
 (K14 Pro, …)        (PC / Mac / Pi)                  <your-org>.upax.io
```

* **No secrets on the terminals, none in the config.** The relay makes its own
  key pair when you pair it. UPAX stores only the public half, and an admin
  activates the relay after checking its fingerprint.
* **Outbound only.** No firewall or router changes are needed.
* **Nothing is lost.** Punches stay on the terminal (or in the relay's spool)
  until UPAX has them, through outages, restarts and reinstalls.
* **Vendor-neutral.** Terminals are reached through drivers. ZKTeco is
  supported today: terminals that speak the standalone SDK protocol on
  TCP 4370 (K14, K14 Pro, K40, MB series, …), and push-capable ZKTeco
  terminals through the relay's LAN listener.

It's a single self-contained executable with no runtime to install. It runs as
a native background service: a Windows Service, a systemd unit on Linux, or a
launchd daemon on macOS.

## Install

You need:

* your organisation's UPAX address, like `https://<your-org>.upax.io`,
* a **pairing code** from UPAX: *HR › Devices › Relays › Add relay*. It looks
  like `RLY-XXXX-XXXX-XXXX` and works once, for 15 minutes,
* each terminal's IP address (ZKTeco: *Menu › Comm. › Ethernet*), or let setup
  scan the network.

**Linux / macOS / Raspberry Pi**

```sh
curl -fsSL https://github.com/UPAX-platform/attendance-relay/releases/latest/download/install.sh | sh
```

**Windows**: open PowerShell with *Run as administrator* and run:

```powershell
irm https://github.com/UPAX-platform/attendance-relay/releases/latest/download/install.ps1 | iex
```

The installer downloads the binary, checks it against the release's
`checksums.txt`, and starts `upax-relay setup`. Setup:

1. **pairs** the relay: it generates the relay's key, sends the public half to
   UPAX with the pairing code, and shows the key's **fingerprint**,
2. **adds terminals**: it connects to each one, or scans the network, and asks
   for the terminal's comm key if one is set,
3. asks which punches already stored on the terminals to upload,
4. optionally turns on the **push listener** (see below),
5. installs the binary to a fixed place (`/usr/local/bin` or
   `%ProgramFiles%\UPAX Relay`), installs and starts the service, and waits
   until it has reached UPAX.

Then, in UPAX:

1. open the relay under *HR › Devices › Relays*, check that the fingerprint
   matches the one setup showed (or `upax-relay status`), and **activate** it,
2. add the terminals the relay discovered. They appear under the relay with
   their serial numbers.

Until the relay and a terminal are active in UPAX, the relay keeps reporting
in, so UPAX shows them as reachable, but it uploads nothing: punches stay on
the terminal and go up as soon as both are active. The same goes for anything
that is *paused* in UPAX.

Running the installer again upgrades the relay in place.

Non-interactive setup, for scripted rollouts:

```sh
sudo upax-relay setup --upax https://<your-org>.upax.io --code RLY-XXXX-XXXX-XXXX \
     --terminal 192.168.1.201 --name "Front door" --password 0 --backfill all \
     --listener no --install yes
```

## Day to day

| Command | What it does |
|---|---|
| `upax-relay status` | The relay's state in UPAX and its fingerprint, then per terminal: its state in UPAX, whether it's reachable, punches uploaded or pending, clock, last error. Works without administrator rights |
| `upax-relay test` | Connects to each terminal and makes one signed call to UPAX |
| `upax-relay scan` | Finds terminals on the local network |
| `upax-relay setup` | Adds or changes terminals. On a paired relay it skips pairing |
| `upax-relay pair` | Pairs again with a new code, e.g. after the relay was unpaired in UPAX. Keeps the same key (and fingerprint) unless you pass `--new-key` |
| `upax-relay unpair` | Deletes this relay's key and pairing. Also unpair it in UPAX |
| `upax-relay rotate-key` | Replaces the relay's key. The service also does this once a year by itself |
| `upax-relay show-comm-key <terminal>` | Prints a terminal's comm key, e.g. for a vendor tool |
| `upax-relay restart` | Restarts the service, e.g. after editing the config |
| `upax-relay uninstall` | Removes the service (the config and key are kept) |

Commands that change the relay need an administrator: use `sudo` on
Linux/macOS, or *Run as administrator* on Windows.

## Configuration

| OS | Config, key, state and `relay.log` |
|---|---|
| Windows | `C:\ProgramData\UPAX Relay\` |
| macOS | `/Library/Application Support/UPAX Relay/` |
| Linux | `/etc/upax-relay/` |

```yaml
version: 2
upax: https://<your-org>.upax.io     # your organisation's UPAX address
relay_id: 01q2…                      # written by pairing; not a secret
terminals:                           # terminals the relay reads over the LAN
  - name: Front door
    vendor: zkteco                   # the default
    host: 192.168.1.201
    port: 4370                       # default: the vendor's port
    password: 0                      # the terminal's comm key, 0 = none
manage_comm_keys: false              # set and keep a random comm key on each terminal
listener:                            # push terminals on the LAN; off by default
  enabled: false
  bind: 0.0.0.0:8081
backfill: all                        # first upload: all, none, or since:2026-01-01
poll_interval: 5s                    # how often each terminal's record counter is checked
```

* `relay.key` (next to the config) is the relay's private key. Only the
  service account can read it. Never copy it to another machine: pair the
  other machine instead.
* The config holds no UPAX secret, but it does hold terminal comm keys, so it
  is readable by administrators only.

### Comm keys (optional)

A terminal with no comm key answers anyone on the LAN. With
`manage_comm_keys: true` (or `manage_comm_key: true` on one terminal), the
relay sets a random comm key on each terminal that has none, saves it to the
config first, and proves it by reconnecting with it. If the terminal doesn't
take it, the relay keeps working without one and says so in `status`. Turning
the option off leaves the key as it is. `show-comm-key` prints it when a vendor
tool needs it. UPAX never sees it.

### Terminal clocks (set in UPAX)

The relay measures each terminal's clock against its zone in UPAX and reports
how far off it is. When clock sync is on for the terminal in UPAX, the relay
also corrects it when it drifts more than 30 seconds, at most once an hour.
The relay won't touch terminal clocks while this computer's own clock disagrees
with UPAX's: turn on automatic time on it.

### Push terminals (optional)

Push-capable terminals normally talk to UPAX directly. With
`listener.enabled: true` they can talk to the relay instead, so they need no
internet access and hold no UPAX credential. Point the terminal's cloud server
setting at this computer's LAN address, port 8081.

* The listener only answers private (LAN) addresses.
* Punches from a terminal that isn't added or active in UPAX are refused, so
  they stay on the terminal and come again later. Once the terminal is active,
  the relay saves its punches to disk before acknowledging them (the spool
  next to the config), then uploads them.
* The relay keeps only attendance punches. Fingerprint and face templates,
  photos and the operation log are acknowledged and discarded, never stored or
  forwarded.

A terminal can still be connected to UPAX directly without a relay (set its
cloud server to the address UPAX shows for a direct device). That is less
secure: use the relay where you can.

## How it works

* **Pulled terminals.** The relay keeps a session with each terminal and
  checks its record counter every `poll_interval`. When the counter changes, it
  downloads the attendance log and sends anything new. It also does a full
  check every 10 minutes. The ZKTeco driver is a Go port of the
  [pyzk](https://github.com/fananimi/pyzk) protocol.
* **UPAX.** Every `poll_seconds` (UPAX decides, default 30 s) the relay calls
  `sync`. That call reports the relay and each terminal (serial, reachability,
  model, firmware, counters, clock) and brings back their status in UPAX,
  their clock settings, and any commands. Punches go up in batches of up to
  500. Requests are signed with RFC 9421 HTTP message signatures (Ed25519),
  with an RFC 9530 `Content-Digest` of the body.
* **Commands.** The relay runs what UPAX sends: `DATA UPDATE USERINFO` and
  `DATA DELETE USERINFO`, which keep the terminal's user list in step with HR
  (an administrator on the terminal stays one), and `INFO`. Anything else gets
  the reply "not supported". For push terminals, commands are passed through
  as they are.
* **No lost punches.** After each upload, the relay saves its position in the
  terminal log to `state-<serial>.json`.
  * If UPAX or the internet is down, punches stay on the terminal and go up
    when the connection is back.
  * If the terminal's oldest records roll off, the relay finds the last punch
    it sent and continues from there.
  * Whenever it can't tell where it left off (the log was cleared, the state
    file is gone, or it was installed again), it sends the whole log again.
    UPAX skips punches it already has, so re-sending is safe. Skipping would
    not be, because the terminal clock may have been set back.
* **Health.** The service writes `health-*.json` next to the config. That's
  what `status` and `setup` read. These files hold no keys.

## Development

```sh
make test           # unit + end-to-end tests (simulated terminal + fake UPAX that verifies signatures)
make sim            # simulated K14 Pro on 127.0.0.1:4370; type a PIN to punch
make dist           # release archives for all platforms into dist/
```

Layout:

| Path | What |
|---|---|
| `cmd/upax-relay` | the CLI and service |
| `internal/relay` | the sync loop, terminal readers, push terminals, state and health |
| `internal/upax` | the UPAX relay API client; `sig` is the request signer, `upaxtest` a verifying fake |
| `internal/identity` | the relay key, fingerprint and pairing codes |
| `internal/device` | the vendor-neutral terminal interface and driver registry |
| `internal/device/zkteco` | the ZKTeco driver: `sdk` (TCP 4370), `adms` (push), `zksim` (simulator) |
| `internal/push` | the LAN listener and spool for push terminals |
| `internal/config` | the config file |

A new vendor is a package under `internal/device/<vendor>` that implements
`device.Driver` and registers itself, plus one import line in
`internal/device/drivers`.

`internal/upax/sig/testdata/vectors.json` pins the signature format. UPAX's
verifier tests read the same file, so a change to it is a change on both sides.

Push a `v*` tag to build a GitHub release with
`.github/workflows/release.yml`. The install scripts download from
`UPAX_RELAY_BASE_URL`. It defaults to this repo's GitHub releases, but you can
point it at any static host.
