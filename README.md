# CB0401 Tune + Control

A self-hosted setup script and web dashboard ("Xiaomi 5G CPE Pro Control") for the **Xiaomi 5G CPE Pro** router family — model **CB0401V2** (this project's own test device, sold under Deutsche Telekom's Magenta branding in Germany/Austria as the "Magenta Internet Box AX5400", used with any carrier SIM — the test device itself runs on an o2-DE SIM) and, very likely, the original **CB0401** (v1): both share the same Qualcomm IPQ5018 SoC and Quectel RG520N-family modem, and are listed under the identical "Xiaomi 5G CPE Pro" name in xmir-patcher's own device database — differing only in a modem firmware revision (R01 vs R03) that doesn't affect the AT-command surface this toolkit uses. Not independently verified on real CB0401 (v1) hardware, though.

For people who own the hardware and want to actually control it: persistent root SSH, a clean local admin panel for cellular/Wi-Fi settings that the stock UI doesn't expose, and **removal of the telemetry and junk cron jobs the stock firmware phones home with by default**.

Everything runs **locally** and is **fully self-contained** — no third-party exploit tool, no Python required for the core setup. The GUI is a single native binary bound to `127.0.0.1`, driven entirely by SSH commands to your own router. Nothing here talks to any third-party server except the router itself and, if you turn on push notifications, either [ntfy.sh](https://ntfy.sh) or the Telegram Bot API (your choice).

![CB0401 Tune + Control GUI](docs/screenshot.png)

## What this actually does

Run once, `setup.sh` (macOS/Linux) or `setup.ps1` (Windows) will:

1. Open persistent root SSH on the router (`bootstrap/open_ssh.sh` / `.ps1`) — see [How SSH access is opened](#how-ssh-access-is-opened) below for exactly how and why this works. Skipped if SSH already works; falls back to a password-based key install if it doesn't apply. A dedicated SSH key for the toolkit is installed in the same step, so every later step (and the GUI itself) never needs your password again.
2. Set up push notifications and remote device-block commands: pick either a random, private [ntfy.sh](https://ntfy.sh) topic (zero setup) or a Telegram bot (needs a token + your chat ID), wire an instant new-device alert straight into dnsmasq (fires the moment a device gets a DHCP lease, no polling), and start a small listener on the router that lets you reply with commands like `trust`, `block` or `red alert` to react from your phone (a one-line cron watchdog keeps it running). Switchable later from the GUI's Device monitor card without re-running setup — see [Push notifications & remote commands](#push-notifications--remote-commands-optional).
3. Copy `router/cleanup.sh` onto the router and run it, removing telemetry uploads and dead cron jobs (see [What gets cleaned up](#what-gets-cleaned-up) below).
4. Build (if needed) and launch the GUI at `http://127.0.0.1:5757` — a single Go binary, nothing to install to run it.

Everything is idempotent — re-running the setup script is safe and just verifies/repairs each step.

## How SSH access is opened

This isn't a novel exploit — it's documented Xiaomi router behavior that's been public knowledge in the router-hacking community for years:

1. The router's web UI exposes an **unauthenticated** endpoint, `api/xqsystem/init_info`, which includes the device's serial number.
2. Xiaomi's own firmware-imaging tool (`mkxqimage`) derives a default root/Telnet password from that serial number: `md5(serial + "6d2df50a-250f-4a30-a5e6-d44fb0960aa0")`, first 8 hex characters. That salt is a hardcoded GUID with its segments reversed, found by reverse-engineering `mkxqimage` itself — not something this project invented.
3. Stock firmware ships with Telnet (port 23) always enabled, and root accepts that derived password.
4. `bootstrap/open_ssh.sh`/`.ps1` log in over Telnet with it and apply the same "soft" persistence patch this project always used: enable dropbear (removing its release-build gate), set `nvram ssh_en=1`, and install a cron job + firewall include hook that keeps re-applying that every minute so it survives reboots. It then installs this toolkit's own SSH key directly, while already at a root shell.

This whole flow (the password derivation, the always-on Telnet, the dropbear/nvram patch) is well-established prior art — the same mechanism `xmir-patcher` itself relies on, used successfully against this exact router earlier in this project's development. `bootstrap/open_ssh.sh`/`.ps1` are a direct, faithful port of that logic to plain bash/PowerShell, with the password derivation and the unauthenticated HTTP fetch verified byte-for-byte identical between both implementations against real hardware. If it ever doesn't apply to your specific router (e.g. Telnet already closed some other way), `setup.sh`/`setup.ps1` fall back to asking for the router's password directly.

## The GUI

A single-page dashboard covering:

- **System** — real-time CPU%, memory, SoC temperature, uptime, reboot button, a version-spoof button that lifts the stock updater's downgrade block (memory-only, reverts on reboot — see [Firmware downgrade & 5G band unlock](#firmware-downgrade--5g-band-unlock)), plus a 2.4GHz spectrum scan with a channel recommendation (scores every candidate channel by overlap-weighted, signal-weighted interference from every network your router can see, not just exact-channel matches).
- **Cellular** — Standalone (SA) 5G toggle, region-based band presets (Europe/America/Asia, built from GSMA/3GPP allocation tables and, for Europe, this project's own factory-default bands), and raw band chips for full manual control via `AT+QNWPREFCFG`.
- **Wi-Fi** — 2.4GHz and 5GHz channel/width, live client count, read-only TX power display (see [Known hardware limitations](#known-hardware-limitations) for why it's read-only).
- **SSH** — one-click-copy commands for both key-based and password-based access, pre-filled with the `ssh-rsa` compatibility flags this router's old dropbear needs to work with a modern OpenSSH client, plus a field to change the router's root password (see [Changing the root password](#changing-the-root-password)).
- **Device monitor** — every device currently on your network, with a checkbox whitelist; unlisted devices trigger a push notification. Also where you switch between ntfy.sh and Telegram, update your Telegram bot token/chat ID, or turn incoming-SMS forwarding on/off, at any time — see [Incoming SMS](#incoming-sms).
- **Advanced** — a raw SSH command box and a full `uci show` config dump, for anything the rest of the UI doesn't cover.

![Device monitor card](docs/screenshot-2.png)

`setup.sh`/`setup.ps1` build and launch [`gui/`](gui/) — a single native Go binary, nothing to install to *run* it (only to *build* it, once, if you don't use a prebuilt binary — see [`gui/README.md`](gui/README.md)).

## Push notifications & remote commands (optional)

The router will ping you the instant an unrecognized device joins your network — the alert is wired directly into dnsmasq's own `--dhcp-script` hook (`dhcp_notify.sh`), which fires exactly once per DHCP lease granted, not on a polling timer. There's no delay waiting for a periodic check, and nothing runs on the router in between actual connection events. You can reply directly from the notification, and the reply is handled just as fast: `command_watcher.sh` runs as a small always-on listener that keeps one long-polling request to Telegram (or an open ntfy stream) waiting, so a reply is acted on within about a second. The connection sits idle in between (about 1 MB of RAM, no polling), and a once-a-minute cron watchdog restarts the listener after a reboot. The new-device alert also makes sure it's running the moment it fires.

| Reply | Effect |
|---|---|
| `trust` | Adds the last unrecognized device seen to the whitelist, so it never alerts again (and lifts its block, if it had one) |
| `<MAC or IP> trust` | Same, for a specific device |
| `block` | Blocks the last unrecognized device seen |
| `<MAC or IP> block` | Blocks that specific device (resolves an IP to its current MAC first, and blocks by MAC — an IP-only block would stop protecting the device the moment its DHCP lease changes) |
| `red alert` | Locks Wi-Fi down to only the devices on your whitelist. This requires a full radio reload on this hardware, so it will briefly disconnect *every* device, including trusted ones — there's no way around that on this platform. |
| `all clear` | Removes the lockdown, back to normal |

You choose the delivery backend during setup (or later, from the GUI's Device monitor card — the change takes effect within a minute, no re-running setup or restarting anything):

- **ntfy.sh** — zero setup: just install the free [ntfy app](https://ntfy.sh) and a random topic name is generated for you. The topic *is* the access control — anyone who knows it can read your alerts and send these commands, so treat it like a password. Since it's a shared pub/sub topic, the router's own outgoing alerts also come back to it; `command_watcher.sh` filters those out by their exact notification title so they aren't mistaken for a command.
- **Telegram** — create a bot with [@BotFather](https://t.me/BotFather) (free, one-time) to get a bot token, then message your new bot once so it can see your chat ID. Commands are only ever accepted from that one chat ID, which is a tighter access model than a shared topic string — and since a bot never receives its own outgoing messages back, there's no self-message filtering needed on this path.

Either way, the credential (ntfy topic, or Telegram bot token + chat ID) is stored only in `gui/.env` on your machine and `/etc/crontabs/patches/notify.conf` on the router — both gitignored/generated locally, never part of this repository.

The first time you set a backend up (or whenever you switch it, or change the Telegram token/chat ID), you'll get a one-time welcome message through it listing the commands above — so you don't have to come back to this README to remember them once an actual alert shows up.

`device_monitor.sh` is still on the router alongside `dhcp_notify.sh`, but only runs once during setup — it scans whatever's already connected at that point so those devices are seeded as "already seen" instead of all alerting at once the moment the dnsmasq hook goes live. After that, it's unused unless you re-run setup (or run it manually over SSH, as a re-scan).

"Already seen" isn't forever, though: a device you haven't whitelisted (or blocked) gets re-alerted if it reconnects more than 4 hours after its last alert. The 4-hour window exists specifically so a router reboot - which makes every already-connected device request a fresh lease at once, since the lease file itself lives on `/tmp` and doesn't survive one - doesn't look like all of them just showed up for the first time; it isn't meant to mean "you've decided about this device, don't ask again" the way whitelisting or blocking it does.

## Incoming SMS

If the number this SIM is on ever gets a text (a carrier notice, a 2FA code, anything), it's forwarded to your ntfy/Telegram backend too — checked via the "Forward incoming SMS here too" checkbox next to the notification backend on the Device monitor card (on by default once you've set up a backend).

The stock firmware's own SMS handling (`/usr/sbin/mobile`) is compiled/encrypted Lua this project can't hook into or extend, so there's no equivalent of the dnsmasq hook above - `sms_notify.sh` polls instead, but cheaply: every 4 seconds it runs [`sms-reader`](router/sms-reader/) (a small, purpose-built, dependency-free Go binary this project ships prebuilt for the router) against `/data/etc/mobile/xqSMS.db`, the small SQLite file that daemon already maintains. There's no `sqlite3` CLI on this router and installing one would mean either a multi-MB dependency or a foreign binary's libc against this firmware's userland - so `sms-reader` implements just enough of the SQLite file format to read new rows of that one known table, in a static binary built with `CGO_ENABLED=0` (no libc dependency of its own either). See its own doc comment for the exact scope and what it deliberately doesn't try to handle (a table that's grown past a single b-tree page, essentially - not a realistic size for a personal SMS inbox).

Like the other listeners, `sms_notify.sh` runs under the same `--daemon`/`--ensure`/`--stop` convention with a once-a-minute cron watchdog, and a one-shot run at setup time seeds "already seen" from whatever's already in the database so it doesn't forward old messages the moment it goes live.

## What gets cleaned up

By default (`sh cleanup.sh`, no flags), the following is disabled — all verified to have zero effect on routing, Wi-Fi, or cellular functionality:

- **`sp_check.sh`** — a cron job that gzips and uploads `web.log` / `rom.log` / `privacy.log` / `pri_rom.log` to a Xiaomi telemetry endpoint every 5 minutes.
- **`otapredownload`** — automatic firmware pre-download, which also has the side effect of silently overwriting any manual firmware/config changes you've made.
- **`breakpad`** — Google Breakpad crash reporter, uploads crash dumps to Xiaomi.
- A couple of dead cron entries left over from other hardware variants (referencing scripts and directories that don't even exist on this firmware).

Two extra, opt-in flags exist for things that are genuinely useful for *some* people and not others:

- `--disable-mesh` — disables `cab_meshd` / `miwifi-discovery` / `miwifi-roam`, which run at boot unconditionally even if you have no Xiaomi Mesh satellite node paired. Skip this if you actually use Mesh.
- `--disable-messagingagent` — disables the router's MQTT connection to Xiaomi's cloud. This may be what the Mi Home app or your carrier's remote support tooling relies on — only disable it if you don't need either.
- `--all` — both of the above.

Pass these to `setup.sh`/`setup.ps1` via the `CLEANUP_FLAGS` environment variable (e.g. `CLEANUP_FLAGS=--all ./setup.sh`) so you don't need a manual follow-up SSH session.

**On persistence:** service disables (`--disable-mesh`/`--disable-messagingagent`/breakpad) only take effect through `/etc/init.d/X disable`, which lives on this router's ramfs-mounted `/etc` — confirmed live that they silently revert to "enabled" after a reboot, the same root cause as the [root password not sticking without the direct-write fix](#changing-the-root-password). `cleanup.sh` works around this the same way `bootstrap/open_ssh.sh` keeps SSH open: it installs a small cron job that reapplies the disables within a minute of every boot. The telemetry/dead-cron removals above don't need this — they edit `/etc/crontabs/root`, which is a symlink into persistent storage and survives reboots on its own.

## Known hardware limitations

These were all confirmed through direct testing on real hardware this project was built against, not assumed from documentation:

- **TX power cannot be changed from software.** `iw set txpower fixed` returns success but has zero measurable effect; the vendor `cfg80211tool s_txpow` returns `EINVAL`; `get_maxpower`/`get_minpower` are unimplemented stubs. The GUI shows the real value read-only.
- **`qca_spectral`** is loaded and referenced by the driver stack, but exposes no usable userspace interface anywhere (checked `cfg80211tool`, `iwpriv`, and the full `debugfs` tree) — it appears to be purely an internal DFS radar-detection dependency, not something you can point at a spectrum analyzer.
- **No Docker/Entware/Node.js.** Persistent storage is a single ~20MB partition; there simply isn't room, and the vendor's `opkg` feed is dead (404).
- **A `wifi reload` briefly drops every client**, trusted or not. This is triggered by changing Wi-Fi channel/width and by the `red alert`/`all clear` commands — it's a platform limitation of this driver stack, not a bug in this toolkit.

## Changing the root password

`/etc/shadow` is a symlink into the router's tiny persistent partition, but it sits under a `ramfs`-mounted `/etc` — a password change made the usual way (`passwd`, or editing that path) gets lost on the next reboot. The GUI's "Change root password" button works around this by writing the new hash directly to `/data/etc/shadow` (the real persistent path the symlink points at) instead of going through `/etc/shadow` itself — confirmed live to survive a reboot, unlike the naive approach.

The GUI always logs in with its own SSH key first, and only falls back to a password (to silently reinstall the key) if the key stops working — for instance after a factory-reset-like event that wipes `/etc`. Whenever you change the password from the GUI, it updates the toolkit's own `.env` in the same step, so that fallback keeps using the current password rather than the stale factory one.

This password fallback needs the `sshpass` tool, which `setup.sh` installs for you on macOS/Linux. It isn't available on Windows by default, so on Windows this one specific fallback is a no-op — the GUI still works normally, and if the router ever does lose the key, just re-run `setup.ps1` to reinstall it.

## Getting started

### Prerequisites (both platforms)

- Your CB0401/CB0401V2 connected and reachable at its default gateway address, `192.168.31.1` (override with the `ROUTER_IP` environment variable if yours differs).
- An SSH client (`ssh`/`scp`) — already on macOS/Linux; on Windows, the built-in OpenSSH Client feature (present by default on Windows 10 1809+ and Windows 11; if `ssh` isn't recognized, enable it with `Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0` from an admin PowerShell).
- [Go](https://go.dev/dl/) 1.21+, **only** if you want `setup.sh`/`setup.ps1` to build the GUI from source on first run (the default). If you'd rather not install Go, build `gui` on another machine (`gui/build.sh`) and drop the resulting binary into `gui/` before running setup — it'll be used as-is.

That's it — no Python and no xmir-patcher needed at all, for anything.

### macOS / Linux

```bash
git clone https://github.com/Kreal-exe/cb0401-tune-control
cd cb0401-tune-control
./start.sh
```

### Windows

```powershell
git clone https://github.com/Kreal-exe/cb0401-tune-control
cd cb0401-tune-control
powershell -ExecutionPolicy Bypass -File start.ps1
```

`start.sh`/`start.ps1` is the one command for everything, every time: it checks whether the SSH key already works against the router, and either runs the full `setup.sh`/`setup.ps1` (first run) or skips straight to launching the GUI (every run after that) — no need to remember which script to use.

The full setup opens SSH automatically (see [How SSH access is opened](#how-ssh-access-is-opened)); if that doesn't apply to your router, it asks for the router's SSH password exactly once, during the one-time key installation step — enter the derived default password from that section, or whatever you've since changed it to. Every step after that uses the key. It will also ask you to pick ntfy.sh or Telegram for notifications (see [Push notifications & remote commands](#push-notifications--remote-commands-optional)); for a non-interactive run, set `NOTIFY_BACKEND=telegram` plus `TELEGRAM_BOT_TOKEN`/`TELEGRAM_CHAT_ID` (or leave `NOTIFY_BACKEND` unset for ntfy) as environment variables beforehand.

Once setup finishes, the GUI opens automatically at `http://127.0.0.1:5757`.

### Firmware downgrade & 5G band unlock

Both live as one-click buttons in the GUI itself — no separate script, no xmir-patcher, since they only need the SSH access this toolkit already has:

- **System card → "Spoof version → 0.0.1"** — lifts the stock web updater's downgrade block (Settings > Update > Local update), if you need to flash an older firmware version. Memory-only (a bind-mount), reverts on its own at the next reboot.
- **Cellular card → "Unlock extra 5G bands + SA"** — installs a permanent hook (adapted from [davidohne/xiaomi_cb0401](https://github.com/davidohne/xiaomi_cb0401)) that unlocks Standalone mode and enables an extra-band list on the cellular modem (n1/n3/n7/n28/n38/n75/n78 by default, or whatever's already configured if you install this after picking your own bands). It reapplies on every `wan_2` reconnect, including after a reboot — and from then on, applying a region preset or manual band selection from the Cellular card keeps that hook in sync, so a reconnect never reverts you back to the installed default.

Both modify firmware/modem configuration and aren't covered by the same idempotency guarantees as the rest of this toolkit — read the button's confirmation prompt before clicking.

## Security notes

- This project modifies firmware behavior on a device that may be leased from, or branded by, your carrier. Check your terms of service; use at your own risk.
- Opening SSH relies on a default password derived from your router's serial number (see [How SSH access is opened](#how-ssh-access-is-opened)) — this is stock Xiaomi firmware behavior, not something this project introduces, but it does mean anyone on your LAN who can reach the router's web UI before you run setup could derive that password too. Run setup promptly after unboxing/resetting the device.
- The GUI has no authentication of its own — it relies entirely on binding to `127.0.0.1`. Do not expose port 5757 to your network.
- `gui/router_key`, `gui/router_key.pub`, and `gui/.env` (which holds the router's root password, plus your ntfy topic or Telegram bot token) are generated locally by setup and are gitignored — never commit or share them.
- SMS forwarding (see [Incoming SMS](#incoming-sms)) means anything sent to this SIM - including 2FA/login codes - ends up in your ntfy topic or Telegram chat too. Turn it off from the Device monitor card if that's not something you want going through a third-party push service.

## License

MIT — see [LICENSE](LICENSE).
