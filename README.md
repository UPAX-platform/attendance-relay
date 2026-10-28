# UPAX ZKTeco relay

Some ZKTeco terminals (e.g. **K14 Pro**) can't push data to a cloud server
themselves. `upax-relay` runs on any always-on computer on the same network,
reads punches from the device over its local SDK protocol (TCP 4370), and
forwards them to UPAX using the same ADMS/"iclock" push protocol that
push-capable terminals use. To UPAX it looks like a normal device.

```
 K14 Pro  ──TCP 4370──▶  upax-relay  ──HTTPS (iclock push)──▶  UPAX
 (LAN)                   (PC / Mac / Pi)                         upax.example.com/<device-address>
```

It's a single self-contained executable with no runtime to install. It runs
as a native background service: a Windows Service, a systemd unit on Linux,
or a launchd daemon on macOS. It only makes **outbound** connections, so you
don't need to change any firewall or router settings.

## Install

You need the device's IP address (on the device: *Menu › Comm. › Ethernet*)
and the device address UPAX issued for it (a URL like `https://upax.example.com/<device-address>`).

**Linux / macOS / Raspberry Pi**

```sh
curl -fsSL https://github.com/UPAX-platform/zk-relay/releases/latest/download/install.sh | sh
```

**Windows**: open PowerShell with *Run as administrator* and run:

```powershell
irm https://github.com/UPAX-platform/zk-relay/releases/latest/download/install.ps1 | iex
```

The installer downloads the binary, checks it against the release's
`checksums.txt`, and starts `upax-relay setup`. Setup:

1. connects to the device, or scans the network for it if you leave the IP blank,
2. checks that UPAX accepts the device address and tells you whether the device
   still needs activating in UPAX (*HR › Devices*),
3. asks which of the punches already stored on the device to upload,
4. writes the config, installs the binary to a fixed place (`/usr/local/bin` or
   `%ProgramFiles%\UPAX Relay`), installs and starts the background service, and
   waits until the service has reached UPAX.

A new device shows up in UPAX with its serial number as soon as setup checks
the address. It stays *pending* until an admin activates it. Until then the
relay keeps in touch with UPAX, so the device doesn't go stale, but it doesn't
upload anything: punches stay on the device and go up as soon as UPAX
activates it. The same goes for a device that is *paused* in UPAX.

Running the installer again upgrades the relay in place.

Non-interactive setup, for scripted rollouts:

```sh
sudo upax-relay setup --device 192.168.1.201 --url https://upax.example.com/<device-address-1> \
     --password 0 --backfill all --install yes
```

## Day to day

| Command | What it does |
|---|---|
| `upax-relay status` | Shows whether the service is running, the device's state in UPAX, when it last reached UPAX, and how many punches it has uploaded or still has to. Works without administrator rights |
| `upax-relay test` | Checks the connection to each device and to UPAX |
| `upax-relay scan` | Finds ZKTeco devices on the local network |
| `upax-relay setup` | Adds another device, or changes an existing one |
| `upax-relay restart` | Restarts the service, e.g. after editing the config |
| `upax-relay uninstall` | Removes the service (the config is kept) |

Commands that change the service need an administrator: use `sudo` on
Linux/macOS, or *Run as administrator* on Windows.

## Configuration

| OS | Config, state and `relay.log` |
|---|---|
| Windows | `C:\ProgramData\UPAX Relay\` |
| macOS | `/Library/Application Support/UPAX Relay/` |
| Linux | `/etc/upax-relay/` |

```yaml
devices:
  - name: Front door
    host: 192.168.1.201
    port: 4370            # default
    password: 0           # device comm key (Menu › Comm. › PC Connection), 0 = none
    upax_url: https://upax.example.com/<device-address-1>
  - name: Warehouse
    host: 192.168.1.202
    upax_url: https://upax.example.com/<device-address-2>
backfill: all             # first run: stored history (all), only new punches (none), or since:2026-01-01
poll_interval: 5s         # how often to check the device for new punches
```

One relay can serve any number of devices. Each device needs its own UPAX address.

If you point a device at a different UPAX address, the relay notices. When the
new address is a new device in UPAX, the relay uploads the punches stored on the
device to it again, as set by `backfill`. When it's the same device with a new
address, it carries on where it left off.

## How it works

* **Device side.** The relay keeps a TCP session to the device and checks its
  record counter every `poll_interval`. When the counter changes, it downloads
  the attendance log and sends anything new. It also does a full check every
  10 minutes as a safety net. This is the same protocol as
  [pyzk](https://github.com/fananimi/pyzk), ported to Go.
* **UPAX side.** The relay behaves like a push device:
  * `GET /iclock/cdata?options=all` for the handshake, carrying the device summary (`INFO=`).
    It also sends `UPAXRelay=1`, which asks UPAX to add a `UPAXStatus=` line
    (pending, active or paused). Real terminals never send it, so they never see that line.
    The relay repeats the handshake every hour, and every minute while the device isn't active,
  * `POST /iclock/cdata?table=ATTLOG` to upload punches, as tab-separated `PIN, time, state, verify, workcode` lines.
    A batch only counts as sent when UPAX replies `OK: <number of lines>`,
  * `GET /iclock/getrequest` as the heartbeat. It sends the device summary again only when the user or fingerprint count changes.

  It acts on the commands UPAX sends: `DATA UPDATE USERINFO` and `DATA DELETE USERINFO`
  (keeps the device's user list in step with HR; an administrator on the device stays one) and `INFO`.
  It also answers `CHECK`. Any other command gets the reply "not supported" (`-1002`).
* **No lost punches.** After each successful upload, the relay saves its
  position in the device log to `state-<serial>.json`.
  * If UPAX or the internet is down, punches stay on the device and are sent once the connection is back.
  * If the device's oldest records roll off, the relay finds the last punch it sent and continues from there.
  * Whenever it can't tell where it left off (the log was cleared, the state file is gone, or it was installed
    again or on another machine), it sends the whole log again. UPAX skips punches it already has, so re-sending
    is safe. Skipping would not be, because the device clock may have been set back.
* **Health.** The service writes `health-<device>.json` next to the config. That's what `status` and `setup` read.
  It holds no device address, since the address contains the device's credential.
* **Time.** Punches are sent with the device's own clock, exactly as a push
  device would send them. `upax-relay test` and `setup` warn you if the
  device clock is more than 2 minutes off.

## Development

```sh
make test           # unit + end-to-end tests (simulated device + fake UPAX)
make sim            # simulated K14 Pro on 127.0.0.1:4370; type a PIN to punch
make dist           # release archives for all platforms into dist/
```

Push a `v*` tag to build a GitHub release with
`.github/workflows/release.yml`. The install scripts download from
`UPAX_RELAY_BASE_URL`. It defaults to this repo's GitHub releases, but you
can point it at any static host.
