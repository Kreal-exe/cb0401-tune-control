#!/bin/bash
#
# ruview.sh — one command that sets up RuView's own dashboard (github.com/
# ruvnet/ruview) from scratch and points it at this router's real CFR
# capture data, exactly the way you'd do it on a fresh machine: nothing
# RuView-specific is vendored into this repo (see README.md for why: it's
# a separate, much larger project we don't want to fork or freeze a copy
# of) - this script clones it fresh into ./ruview/ (gitignored) every time
# it's missing, and reuses it otherwise.
#
# Entirely optional - nothing else in this project depends on it, and
# setup.sh/start.sh never run it. It's the "opt into Wi-Fi sensing" step.
#
# What lives where:
#   - ./router/cfr-trigger, ./router/cfr_capture_daemon.sh
#         The router-side half: arm CFR capture for every associated Wi-Fi
#         client and keep it armed. This script cross-builds cfr-trigger
#         and installs/refreshes both on the router (idempotent - reuploads
#         only when they changed, restarts the daemon only then).
#   - ./router/ruview-bridge, ./router/ruview-proxy, ./router/cfr-to-rvcsi
#         This project's own host-side drivers - committed to this repo,
#         nothing to download.
#   - ./ruview/  (created by this script, gitignored)
#         A fresh clone of github.com/ruvnet/ruview (+ the submodules its
#         v2 cargo workspace lists) and its built sensing-server binary.
#
# What this script does, in order:
#   1. Install/refresh the router-side capture (cfr-trigger + the daemon)
#      over SSH, unless --no-router-install.
#   2. Clone/update ./ruview/ and its submodules if missing.
#   3. Build wifi-densepose-sensing-server (release) - this is real,
#      third-party Rust code; the actual compiling happens when *you* run
#      this script in your own terminal, which is the point (see the
#      chat this script came from if you want the full story on why).
#   4. Build ./router/ruview-bridge and ./router/ruview-proxy (this
#      project's own Go code).
#   5. Start sensing-server with --source esp32 (the only explicit source
#      that binds the UDP ingest port - see below), serving RuView's own UI.
#   6. Start ruview-bridge, which pulls raw CFR dumps off the router over
#      SSH, converts real (non-empty) records to RuView's "QCS1" wire
#      format, and forwards them to sensing-server's UDP listener.
#   7. Unless --local: put the dashboard behind ruview-proxy, open a
#      Cloudflare quick tunnel to it, and send yourself the resulting link
#      (with a one-off access key) through the ntfy/Telegram backend
#      setup.sh already configured in gui/.env.
#   8. Open the dashboard in your browser.
#
# Usage: ./ruview.sh [--local] [--no-router-install]
#                    [--router-ip 192.168.31.1] [--router-key path/to/router_key]
#                    [--env-file path/to/gui/.env]
#
set -e
cd "$(dirname "$0")"
REPO_DIR="$(pwd)"

ROUTER_IP="${ROUTER_IP:-192.168.31.1}"
ROUTER_KEY=""
ENV_FILE=""
PUBLIC=1 # tunnel + link by default; --local keeps everything on this machine
ROUTER_INSTALL=1
while [ $# -gt 0 ]; do
  case "$1" in
  --router-ip) ROUTER_IP="$2"; shift 2 ;;
  --router-key) ROUTER_KEY="$2"; shift 2 ;;
  --env-file) ENV_FILE="$2"; shift 2 ;;
  --public) PUBLIC=1; shift ;; # the default; kept so older commands still work
  --local) PUBLIC=0; shift ;;
  --no-router-install) ROUTER_INSTALL=0; shift ;;
  -h|--help) sed -n '2,/^set -e/p' "$0" | sed 's/^# \{0,1\}//' | sed '$d'; exit 0 ;;
  *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

# router_key and gui/.env are gitignored (generated secrets, see the main
# README) so they only exist in whichever checkout setup.sh actually ran
# in - if this script is running from a worktree of that checkout (see
# git-worktree(1)), they live in the main one instead, three levels up
# from .claude/worktrees/<name>. Try both before giving up.
find_local_file() {
  for candidate in "$REPO_DIR/gui/$1" "$REPO_DIR/../../../gui/$1"; do
    if [ -f "$candidate" ]; then echo "$candidate"; return 0; fi
  done
  return 1
}
[ -n "$ROUTER_KEY" ] || ROUTER_KEY="$(find_local_file router_key || true)"
if [ -z "$ROUTER_KEY" ] || [ ! -f "$ROUTER_KEY" ]; then
  echo "Router SSH key not found (looked in $REPO_DIR/gui/ and its main checkout, if this is a worktree)." >&2
  echo "Run this project's own setup.sh first, or pass --router-key /path/to/router_key" >&2
  exit 1
fi
[ -n "$ENV_FILE" ] || ENV_FILE="$(find_local_file .env || true)"

# Push notices (dashboard link, calibration steps) go through whatever
# setup.sh configured in gui/.env - ntfy topic or Telegram bot - using the
# same notify() the router's own alerts use. notify_common.sh sets its
# defaults and then reads the router-side notify.conf, which doesn't
# exist here, so the .env values are applied after sourcing it.
NOTIFY_READY=0
if [ -n "$ENV_FILE" ] && [ -f "$ENV_FILE" ]; then
  # shellcheck disable=SC1091
  . "$REPO_DIR/router/notify_common.sh"
  NOTIFY_BACKEND="$(grep '^NOTIFY_BACKEND=' "$ENV_FILE" | cut -d= -f2-)"
  NTFY_TOPIC="$(grep '^NTFY_TOPIC=' "$ENV_FILE" | cut -d= -f2-)"
  TELEGRAM_BOT_TOKEN="$(grep '^TELEGRAM_BOT_TOKEN=' "$ENV_FILE" | cut -d= -f2-)"
  TELEGRAM_CHAT_ID="$(grep '^TELEGRAM_CHAT_ID=' "$ENV_FILE" | cut -d= -f2-)"
  NOTIFY_BACKEND="${NOTIFY_BACKEND:-ntfy}"
  NOTIFY_READY=1
fi
# send_notice <title> <tag> <body>: push it if a backend is configured,
# always print it here too.
send_notice() {
  echo "[$1] $3"
  [ "$NOTIFY_READY" = 1 ] || return 1
  notify "$1" "$2" "$3"
}
if ! command -v cargo >/dev/null 2>&1; then
  echo "Rust/cargo not found. Install it first: https://rustup.rs (curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh)" >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  echo "Go not found. Install it first: https://go.dev/dl/" >&2
  exit 1
fi
if [ "$PUBLIC" = 1 ] && ! command -v cloudflared >/dev/null 2>&1; then
  echo "The public link needs cloudflared (Cloudflare's tunnel client) and it isn't installed." >&2
  echo "macOS: brew install cloudflared   Linux/other: https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/install-and-setup/installation/" >&2
  exit 1
fi

RUVIEW_DIR="$REPO_DIR/ruview"
UI_PATH="$RUVIEW_DIR/ui"
SENSING_SERVER_BIN="$RUVIEW_DIR/v2/target/release/sensing-server" # binary name from [[bin]] in Cargo.toml, not the crate name
HTTP_PORT="${RUVIEW_HTTP_PORT:-3000}"        # the dashboard address you open (served by ruview-proxy)
SERVER_PORT="${RUVIEW_SERVER_PORT:-3002}"    # sensing-server's own HTTP port, behind the proxy
UDP_PORT="${RUVIEW_UDP_PORT:-5005}"
PROXY_PORT="${RUVIEW_PROXY_PORT:-3080}"
# The sensing stream is a *separate* WebSocket listener (--ws-port, default
# 8765), not a route on the HTTP port - and RuView's own UI hard-codes which
# port it dials from the port it was served on (ui/services/
# sensing.service.js, SENSING_WS_PORT_BY_HTTP_PORT: 3000 -> 3001 for the
# Docker image, 8080 -> 8765 for the Python stack, anything else -> the same
# port as the page, which this server never serves it on). Confirmed live:
# with the default --ws-port 8765 behind the UI on :3000, the dashboard
# dialed ws://localhost:3001 forever and sat on "RECONNECTING" while the
# server was perfectly healthy. So the WebSocket port has to follow the
# HTTP port the way the UI expects, not the server's default.
case "$HTTP_PORT" in
  3000) WS_PORT_DEFAULT=3001 ;;
  8080) WS_PORT_DEFAULT=8765 ;;
  *)    WS_PORT_DEFAULT="" ;;
esac
WS_PORT="${RUVIEW_WS_PORT:-$WS_PORT_DEFAULT}"
if [ -z "$WS_PORT" ]; then
  echo "RUVIEW_HTTP_PORT=$HTTP_PORT: RuView's UI only knows where to find the sensing stream for HTTP ports 3000 (-> 3001) and 8080 (-> 8765). Use one of those." >&2
  exit 1
fi

# Refuse to start on top of a previous run. sensing-server itself only
# panics into its log file when a port is taken ("Failed to bind WebSocket
# port: Address already in use") - and because it's started with `&`
# below, this script used to sail straight on to "Running" with a dead
# server, while whatever older sensing-server still held the ports kept
# serving the dashboard (confirmed live: an orphaned one from an earlier
# run, started with different flags, was what the browser was actually
# talking to). Name the culprit and stop instead.
busy=""
ports="tcp:$HTTP_PORT tcp:$SERVER_PORT tcp:$WS_PORT udp:$UDP_PORT"
[ "$PUBLIC" = 1 ] && ports="$ports tcp:$PROXY_PORT"
for port in $ports; do
  case "$port" in
  tcp:*) owner="$(lsof -nP -iTCP:"${port#tcp:}" -sTCP:LISTEN 2>/dev/null | awk 'NR>1 {print $2 " (" $1 ")"}' | sort -u | tr '\n' ' ')" ;;
  udp:*) owner="$(lsof -nP -iUDP:"${port#udp:}" 2>/dev/null | awk 'NR>1 {print $2 " (" $1 ")"}' | sort -u | tr '\n' ' ')" ;;
  esac
  if [ -n "$owner" ]; then
    busy="$busy
  $port: pid $owner"
  fi
done
if [ -n "$busy" ]; then
  echo "Ports needed by RuView are already in use:$busy" >&2
  echo "Stop that first (kill <pid>; a previous ruview.sh run leaves its pids in /tmp/ruview-pids), then rerun." >&2
  exit 1
fi

say() { echo; echo "=== $* ==="; }

# Same SSH flags as start.sh/setup.sh (this router's dropbear only speaks
# ssh-rsa), plus BatchMode so a missing/rejected key fails instead of
# prompting for a password mid-script.
SSH_OPTS=(-i "$ROUTER_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -o ConnectTimeout=5 -o BatchMode=yes)
ssh_router() { ssh "${SSH_OPTS[@]}" "root@$ROUTER_IP" "$@"; }
scp_to_router() {
  # -O forces the legacy scp protocol modern OpenSSH no longer defaults to
  # (this router has no sftp-server); older scp builds don't know the flag.
  local err
  err="$(scp -O "${SSH_OPTS[@]}" "$@" 2>&1)" && return 0
  if echo "$err" | grep -qi 'unknown option'; then scp "${SSH_OPTS[@]}" "$@"; else echo "$err" >&2; return 1; fi
}
# Everything below talks to the router with this key (the capture install
# and ruview-bridge). If it's no longer accepted - seen after a router
# reboot/reset wiped /etc/dropbear/authorized_keys - say so up front
# instead of failing halfway with a bare scp/ssh error.
if ! ssh_router true 2>/dev/null; then
  echo "The router at $ROUTER_IP doesn't accept this toolkit's SSH key anymore ($ROUTER_KEY)." >&2
  echo "Run ./start.sh once - it re-installs the key (and may ask for the router's password) - then rerun this script." >&2
  exit 1
fi
file_sum() { if command -v md5sum >/dev/null 2>&1; then md5sum "$1" | cut -d' ' -f1; else md5 -q "$1"; fi; }

if [ "$ROUTER_INSTALL" = 1 ]; then
  say "Installing router-side CFR capture on $ROUTER_IP (cfr-trigger + cfr_capture_daemon.sh)"
  TMP_DIR="$(mktemp -d)"
  # Cross-build for the router's CPU (IPQ5018, ARMv7) - static, syscall-only
  # binary, no toolchain needed; same recipe as router/cfr-trigger/build.sh.
  ( cd "$REPO_DIR/router/cfr-trigger" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o "$TMP_DIR/cfr-trigger" . )
  cp "$REPO_DIR/router/cfr_capture_daemon.sh" "$TMP_DIR/cfr_capture_daemon.sh"
  want="$(file_sum "$TMP_DIR/cfr-trigger") $(file_sum "$TMP_DIR/cfr_capture_daemon.sh")"
  have="$(ssh_router 'cd /etc/crontabs/patches 2>/dev/null && md5sum cfr-trigger cfr_capture_daemon.sh 2>/dev/null | cut -d" " -f1 | tr "\n" " " | sed "s/ $//"' 2>/dev/null || true)"
  if [ "$want" = "$have" ]; then
    echo "already up to date on the router"
  else
    scp_to_router "$TMP_DIR/cfr-trigger" "$TMP_DIR/cfr_capture_daemon.sh" "root@$ROUTER_IP:/tmp/" >/dev/null
    # Stop a running daemon first, then mv (not cp-over): sh reads its
    # script incrementally, so overwriting a running one in place would
    # feed it a half-new file.
    ssh_router 'mkdir -p /etc/crontabs/patches
      sh /etc/crontabs/patches/cfr_capture_daemon.sh --stop 2>/dev/null
      mv /tmp/cfr-trigger /tmp/cfr_capture_daemon.sh /etc/crontabs/patches/
      chmod +x /etc/crontabs/patches/cfr-trigger /etc/crontabs/patches/cfr_capture_daemon.sh'
    echo "installed"
  fi
  # Capture runs only while this script runs - started right before the
  # bridge below, stopped (and the firmware's CFR timer switched off) on
  # exit. Earlier versions kept it running permanently from a cron line,
  # so it also restarted by itself after every router boot; the router
  # rebooted twice within 20 minutes with that running (2026-09-26), so no
  # more always-on capture. Remove that cron line from older installs.
  ssh_router 'if grep -q cfr_capture_daemon /etc/crontabs/root 2>/dev/null; then
      sed -i "/cfr_capture_daemon/d" /etc/crontabs/root
      /etc/init.d/cron restart >/dev/null 2>&1 || true
    fi'
  rm -rf "$TMP_DIR"
fi

say "Fetching RuView"
if [ ! -d "$RUVIEW_DIR/.git" ]; then
  git clone --filter=blob:limit=1m https://github.com/ruvnet/ruview.git "$RUVIEW_DIR"
else
  echo "already present at $RUVIEW_DIR - pulling latest"
  # Drop our own local patches (applied below) first so the pull can't
  # conflict with them; they're re-applied to whatever comes down.
  git -C "$RUVIEW_DIR" checkout -- ui/observatory/js/main.js v2/crates/wifi-densepose-sensing-server/src/main.rs 2>/dev/null || true
  git -C "$RUVIEW_DIR" pull --ff-only || echo "(pull failed/skipped, using existing checkout)"
fi
# cargo has to parse every workspace member's Cargo.toml just to resolve
# v2/Cargo.toml's workspace, even when building only one package with -p -
# so every submodule v2/Cargo.toml's `members` list touches has to be
# present, not just the one wifi-densepose-sensing-server itself depends
# on (confirmed live: fetching only vendor/rufield still failed on the
# next uninitialized one, v2/crates/ruview-swarm). Fetch all of them.
git -C "$RUVIEW_DIR" submodule update --init --depth 1

# One local patch to RuView's Observatory page (ui/observatory/js/main.js).
# Upstream, any WebSocket close - a browser hiccup, a server restart -
# flips the page to its built-in demo generator for good, and the same
# happens if the server wasn't up yet when the page loaded. The demo then
# shows invented numbers (heart rate 81, "Security Patrol" scenes) under
# a small DEMO badge, which looked exactly like broken live data
# (confirmed live). Patched: stay on the live source, show RECONNECTING,
# retry every few seconds; the last live frame stays on screen meanwhile.
# Idempotent; skipped with a note if upstream changed those lines.
python3 - "$RUVIEW_DIR/ui/observatory/js/main.js" <<'EOF' || echo "(Observatory reconnect patch not applied)"
import sys
p = sys.argv[1]
s = open(p).read()
if "cb0401-tune-control patch" in s:
    sys.exit(0)
old_close = """        console.log('[Observatory] WebSocket closed, falling back to demo');
        this._ws = null;
        this.settings.dataSource = 'demo';
        this._hud.updateSourceBadge('demo', null);"""
new_close = """        // cb0401-tune-control patch: stay live and reconnect instead of demo
        console.log('[Observatory] WebSocket closed, reconnecting');
        this._ws = null;
        this._hud.updateSourceBadge('demo', null);
        const lbl = document.getElementById('data-source-label');
        if (lbl) lbl.textContent = 'RECONNECTING';
        clearTimeout(this._reconnectTimer);
        this._reconnectTimer = setTimeout(() => this._autoDetectLive(), 3000);"""
old_none = """        console.log('[Observatory] No sensing server detected, using demo mode');
        return;"""
new_none = """        // cb0401-tune-control patch: keep looking instead of settling on demo
        console.log('[Observatory] No sensing server detected yet, retrying');
        clearTimeout(this._reconnectTimer);
        this._reconnectTimer = setTimeout(() => this._autoDetectLive(), 5000);
        return;"""
if old_close not in s or old_none not in s:
    print("Observatory source changed upstream - reconnect patch not applied")
    sys.exit(0)
open(p, "w").write(s.replace(old_close, new_close).replace(old_none, new_none))
print("Observatory reconnect patch applied")
EOF

# Second local patch, to sensing-server itself (src/main.rs). Three parts:
#
# 1. Vitals without an empty room. Upstream publishes breathing/heart rate
#    only after a 10-minute calibration on a completely empty room, and
#    only while it then counts exactly one person. At a router by a window
#    - people passing outside, other people at home - that can never hold.
#    With RUVIEW_VITALS_WITHOUT_CALIBRATION=1 (ruview.sh sets it unless you
#    choose RUVIEW_CALIBRATE=1) it publishes whenever someone is detected
#    and RuView's own quality gates still pass (signal quality >= 0.40,
#    rate confidence >= 0.35 here, RuView's default is 0.55, override with
#    RUVIEW_VITALS_MIN_CONFIDENCE), labelled "uncalibrated_estimate". Not
#    medical data: with several people in range it can be any of them.
# 2. /api/v1/vital-signs also reports the candidate values RuView computed
#    before those gates ("candidates"), so an empty readout says why.
# 3. Never more skeletons than persons estimated. RuView's pose tracker
#    splits one procedural pose into several tracks at the ~10-50 Hz this
#    source updates at: the Observatory drew 3 frozen figures side by side
#    while the server itself estimated 1 person (confirmed live). The
#    oldest (most stable) tracks are kept, up to estimated_persons.
python3 - "$RUVIEW_DIR/v2/crates/wifi-densepose-sensing-server/src/main.rs" <<'EOF' || echo "(sensing-server patch not applied)"
import sys
p = sys.argv[1]
s = open(p).read()
if "cb0401-tune-control patch" in s:
    sys.exit(0)
edits = [
    ("""    if !explicit_calibration_fresh || person_count != 1 {
        return None;
    }""", """    // cb0401-tune-control patch: opt-in publication without explicit calibration
    static WITHOUT_CALIBRATION: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    let without_calibration = *WITHOUT_CALIBRATION.get_or_init(|| {
        std::env::var("RUVIEW_VITALS_WITHOUT_CALIBRATION").map(|v| v == "1").unwrap_or(false)
    });
    if without_calibration {
        if person_count == 0 {
            return None;
        }
    } else if !explicit_calibration_fresh || person_count != 1 {
        return None;
    }""", 1),
    ("""        "authority": if published.is_some() { "explicit_calibration" } else { "abstained" },""",
     """        "authority": if published.is_none() { "abstained" } else if explicit_calibration_fresh { "explicit_calibration" } else { "uncalibrated_estimate" },
        "candidates": {
            "breathing_rate_bpm": s.latest_vitals.breathing_rate_bpm,
            "heart_rate_bpm": s.latest_vitals.heart_rate_bpm,
            "breathing_confidence": s.latest_vitals.breathing_confidence,
            "heartbeat_confidence": s.latest_vitals.heartbeat_confidence,
            "signal_quality": s.latest_vitals.signal_quality,
            "person_count": person_count,
            "gates": {"min_signal_quality": VITAL_PUBLICATION_MIN_SIGNAL_QUALITY, "min_confidence": vitals_min_confidence()},
        },""", 1),
    ("update.persons = Some(tracked);",
     "update.persons = Some(cap_tracked_persons(tracked, update.estimated_persons));", None),
    ("""        >= VITAL_PUBLICATION_MIN_CONFIDENCE)""", """        >= vitals_min_confidence())""", 1),
    ("""        (candidates.heartbeat_confidence >= VITAL_PUBLICATION_MIN_CONFIDENCE)""", """        (candidates.heartbeat_confidence >= vitals_min_confidence())""", 1),
    ("""fn vitals_for_publication(""", """// cb0401-tune-control patch: RUVIEW_VITALS_MIN_CONFIDENCE overrides the rate
// confidence gate (default: RuView's own 0.55)
fn vitals_min_confidence() -> f64 {
    static MIN: std::sync::OnceLock<f64> = std::sync::OnceLock::new();
    *MIN.get_or_init(|| {
        std::env::var("RUVIEW_VITALS_MIN_CONFIDENCE")
            .ok()
            .and_then(|v| v.parse::<f64>().ok())
            .filter(|v| v.is_finite() && *v >= 0.0 && *v <= 1.0)
            .unwrap_or(VITAL_PUBLICATION_MIN_CONFIDENCE)
    })
}

// cb0401-tune-control patch: never more tracked skeletons than persons estimated
fn cap_tracked_persons(mut tracked: Vec<PersonDetection>, estimated: Option<usize>) -> Vec<PersonDetection> {
    tracked.sort_by_key(|p| p.id);
    tracked.truncate(estimated.unwrap_or(1).max(1));
    tracked
}

fn vitals_for_publication(""", 1),
]
for old, new, count in edits:
    n = s.count(old)
    if n == 0 or (count is not None and n != count):
        print("sensing-server source changed upstream - local patch not applied")
        sys.exit(0)
for old, new, count in edits:
    s = s.replace(old, new)
open(p, "w").write(s)
print("sensing-server patch applied")
EOF

say "Building RuView's sensing-server (this compiles a large third-party Rust workspace - first run takes a while)"
( cd "$RUVIEW_DIR/v2" && cargo build -p wifi-densepose-sensing-server --no-default-features --release )

say "Building this project's own ruview-bridge and ruview-proxy"
( cd "$REPO_DIR/router/ruview-bridge" && go build -o ruview-bridge . )
( cd "$REPO_DIR/router/ruview-proxy" && go build -o ruview-proxy . )

say "Fetching RuView's pretrained model (Hugging Face ruvnet/wifi-densepose-pretrained)"
# Of everything RuView publishes, this is the newest bundle its docs
# target (v2.0.1; ruv/ruview holds an older copy of the same encoder, the
# MM-Fi pose model is a PyTorch .pt for 3 antennas x 114 subcarriers at
# 100 Hz that this server can't load, WiFlow is a JSON format it can't
# read either). Honest caveat, read out of sensing-server's own code: in
# this version no published weights drive live inference - --model and
# --load-rvf parse the container (Model Library, /api/v1/model/info,
# "Model Inference" badge) while pose and vitals stay signal-derived.
# It's loaded anyway so the dashboard runs in the mode RuView ships for.
# Both flags are needed: --model sets the "loaded" state, --load-rvf is
# what /api/v1/model/info reports (a server started without them showed
# "no_model" - confirmed live).
MODELS_DIR="$REPO_DIR/data/models"
MODEL_RVF="$MODELS_DIR/wifi-densepose-pretrained.rvf"
mkdir -p "$MODELS_DIR/pretrained"
for f in model.safetensors config.json presence-head.json; do
  if [ ! -s "$MODELS_DIR/pretrained/$f" ]; then
    curl -fsSL -m 120 -o "$MODELS_DIR/pretrained/$f" "https://huggingface.co/ruvnet/wifi-densepose-pretrained/resolve/main/$f" \
      || echo "(couldn't download $f - continuing without it)"
  fi
done
rm -f "$MODELS_DIR/ruview-encoder.rvf" # older ruv/ruview copy from previous versions of this script
if [ ! -s "$MODEL_RVF" ] && [ -s "$MODELS_DIR/pretrained/model.safetensors" ]; then
  "$SENSING_SERVER_BIN" --convert-model "$MODELS_DIR/pretrained/model.safetensors" --convert-out "$MODEL_RVF" \
    || echo "(model conversion failed - the dashboard will run without a loaded model)"
fi
MODEL_ARGS=()
if [ -s "$MODEL_RVF" ]; then
  MODEL_ARGS=(--model "$MODEL_RVF" --load-rvf "$MODEL_RVF" --progressive)
  echo "model: $MODEL_RVF"
fi

say "Starting sensing-server (HTTP on :$SERVER_PORT behind the dashboard proxy, sensing stream on :$WS_PORT, UDP ingest on :$UDP_PORT)"
# --source esp32, not wifi: the source name decides which input tasks the
# server runs (main.rs plan_source). "wifi" means *this Mac's own* WiFi
# RSSI scan and does not bind the UDP port at all, so everything
# ruview-bridge sends would be dropped on the floor; "esp32" is the one
# explicit source that binds it, and the receiver then recognises our
# QCS1 datagrams by their magic bytes regardless of that name.
if [ "${RUVIEW_CALIBRATE:-0}" = 1 ]; then
  export RUVIEW_VITALS_WITHOUT_CALIBRATION=0
else
  export RUVIEW_VITALS_WITHOUT_CALIBRATION=1
  # RuView's rate-confidence gate is 0.55; live estimates here sat at
  # 0.38-0.50 with plausible values (heart 76, breathing 10 per minute) and
  # signal quality passing, so nothing was ever shown. In this uncalibrated
  # mode show them from 0.35 up - they're labelled as estimates and the
  # dashboards show the confidence next to them. Override as you like.
  export RUVIEW_VITALS_MIN_CONFIDENCE="${RUVIEW_VITALS_MIN_CONFIDENCE:-0.35}"
fi
"$SENSING_SERVER_BIN" \
  --source esp32 \
  --tick-ms 100 \
  --ui-path "$UI_PATH" \
  --http-port "$SERVER_PORT" \
  --ws-port "$WS_PORT" \
  --udp-port "$UDP_PORT" \
  --bind-addr 127.0.0.1 \
  "${MODEL_ARGS[@]}" \
  > /tmp/ruview-sensing-server.log 2>&1 &
SENSING_PID=$!
PIDS="$SENSING_PID"
sleep 1
if ! kill -0 "$SENSING_PID" 2>/dev/null; then
  echo "sensing-server exited immediately - its log (/tmp/ruview-sensing-server.log) ends with:" >&2
  tail -5 /tmp/ruview-sensing-server.log >&2
  exit 1
fi
echo "sensing-server pid=$SENSING_PID (log: /tmp/ruview-sensing-server.log)"

say "Starting ruview-bridge (pulls real CFR captures from $ROUTER_IP and forwards them)"
ssh_router 'sh /etc/crontabs/patches/cfr_capture_daemon.sh --ensure' \
  || { echo "Couldn't start CFR capture on the router." >&2; exit 1; }
CAPTURE_STARTED=1
"$REPO_DIR/router/ruview-bridge/ruview-bridge" \
  -key "$ROUTER_KEY" \
  -host "root@$ROUTER_IP" \
  -udp "127.0.0.1:$UDP_PORT" \
  > /tmp/ruview-bridge-log.log 2>&1 &
BRIDGE_PID=$!
PIDS="$PIDS $BRIDGE_PID"
echo "ruview-bridge pid=$BRIDGE_PID (log: /tmp/ruview-bridge-log.log)"

# Stop everything we started on Ctrl+C/terminal close - see the end of
# this script for why it stays in the foreground. Installed here, before
# the tunnel step, so a failure there still tears the rest down.
cleanup() {
  echo
  echo "Stopping RuView processes ($PIDS)..."
  # shellcheck disable=SC2086
  kill $PIDS 2>/dev/null
  if [ "${CAPTURE_STARTED:-0}" = 1 ]; then
    echo "Stopping CFR capture on the router..."
    ssh_router 'sh /etc/crontabs/patches/cfr_capture_daemon.sh --stop' 2>/dev/null \
      || echo "(couldn't reach the router to stop capture - it stops at the next router reboot, or run: ssh root@$ROUTER_IP sh /etc/crontabs/patches/cfr_capture_daemon.sh --stop)"
  fi
}
trap cleanup EXIT INT TERM

# The dashboard itself is served by ruview-proxy, not sensing-server
# directly: RuView's pages disagree on where the sensing WebSocket lives.
# The main UI maps page port 3000 -> 3001, but the Observatory page just
# dials ws://<its own origin>/ws/sensing - which sensing-server's HTTP
# port never serves - and silently falls back to its demo generator
# (confirmed live on http://localhost:3000/ui/observatory.html). Behind the
# proxy every page finds the stream on the origin it came from. With
# the public link (the default) the same proxy also serves the key-gated tunnel side.
ACCESS_KEY=""
PROXY_ARGS=(-local "127.0.0.1:$HTTP_PORT" -http "127.0.0.1:$SERVER_PORT" -ws "127.0.0.1:$WS_PORT")
if [ "$PUBLIC" = 1 ]; then
  # The access key is the only thing standing between "anyone who
  # guesses/sees the tunnel hostname" and RuView's unauthenticated API,
  # so it's random per run and only ever travels inside the link.
  ACCESS_KEY="$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  PROXY_ARGS+=(-listen "127.0.0.1:$PROXY_PORT" -key "$ACCESS_KEY")
else
  PROXY_ARGS+=(-listen "")
fi
"$REPO_DIR/router/ruview-proxy/ruview-proxy" "${PROXY_ARGS[@]}" > /tmp/ruview-proxy.log 2>&1 &
PIDS="$PIDS $!"

PUBLIC_LINK=""
if [ "$PUBLIC" = 1 ]; then
  say "Publishing the dashboard through a Cloudflare quick tunnel"
  # --no-autoupdate: cloudflared otherwise replaces its own binary and
  # restarts mid-run when a new version is out, dropping the tunnel.
  cloudflared tunnel --url "http://127.0.0.1:$PROXY_PORT" --no-autoupdate \
    > /tmp/ruview-cloudflared.log 2>&1 &
  TUNNEL_PID=$!
  PIDS="$PIDS $TUNNEL_PID"
  PUBLIC_URL=""
  for _ in $(seq 1 40); do
    PUBLIC_URL="$(grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' /tmp/ruview-cloudflared.log 2>/dev/null | head -1 || true)"
    [ -n "$PUBLIC_URL" ] && break
    if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then break; fi
    sleep 1
  done
  if [ -z "$PUBLIC_URL" ]; then
    echo "cloudflared didn't report a tunnel URL within 40s - its log (/tmp/ruview-cloudflared.log) ends with:" >&2
    tail -5 /tmp/ruview-cloudflared.log >&2
    exit 1
  fi
  PUBLIC_LINK="$PUBLIC_URL/ui/index.html?k=$ACCESS_KEY"
  echo "Public link (valid while this script runs): $PUBLIC_LINK"

  if send_notice "RuView dashboard" "satellite" "$PUBLIC_LINK
Live while ruview.sh keeps running on $(hostname). Anyone with this exact link can open the dashboard - don't forward it."; then
    echo "Link sent via $NOTIFY_BACKEND."
  elif [ "$NOTIFY_READY" = 1 ]; then
    echo "Couldn't send the link via $NOTIFY_BACKEND (HTTP ${NOTIFY_LAST_HTTP:-none}) - copy it from above by hand." >&2
  else
    echo "No gui/.env found (run setup.sh once to configure ntfy/Telegram, or pass --env-file) - copy the link from above by hand."
  fi
fi

# --- Automatic empty-room calibration -----------------------------------
# RuView refuses to publish breathing/heart rate ("fresh explicit
# calibration with exactly one occupant ... required") until it has
# calibrated on an EMPTY room; afterwards it counts exactly one person
# from the data itself. From its code: >= 600 s and >= 1000 frames of
# empty room on one steadily streaming node; held in memory only (every
# server start needs a new one); vitals allowed for 12 h after it
# (status turns stale at half of the 24 h expiry). No bypass exists.
# So: announce it, give people time to leave, calibrate the node with the
# steadiest stream, report back, and repeat every 11.5 h. Every step is
# pushed via ntfy/Telegram (and printed here).
#
# OFF by default: a truly empty room is not achievable for a router by a
# window with people at home and outside, and a calibration taken with
# people around teaches RuView that they are the background. By default
# vitals are published without calibration instead (see the
# sensing-server patch above). RUVIEW_CALIBRATE=1 turns this strict mode
# back on; RUVIEW_CALIBRATION_DELAY sets the warning time (s, default 120).
jfield() { python3 -c 'import sys,json
try: v=json.loads(sys.argv[1]).get(sys.argv[2])
except Exception: v=None
print("" if v is None else (str(v).lower() if isinstance(v,bool) else v))' "$1" "$2"; }

calibrate_loop() {
  local base="http://127.0.0.1:$SERVER_PORT/api/v1" delay="${RUVIEW_CALIBRATION_DELAY:-120}"
  local digest n1 n2 node st boot resp sess ok i fc mf el md
  digest="$(printf 'cb0401-ruview-%s' "$(hostname)" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-64)"
  while :; do
    send_notice "RuView calibration" "hourglass" "In $((delay / 60)) min RuView calibrates the empty room for 10 min. Please leave the room then and keep it empty (people and pets) for about 11 minutes. I'll message when it's done." || true
    sleep "$delay"
    n1="$(curl -s -m 5 "$base/nodes")"; sleep 15; n2="$(curl -s -m 5 "$base/nodes")"
    node="$(python3 - "$n1" "$n2" <<'EOF'
import sys, json
try:
    a = {n["node_id"]: n for n in json.loads(sys.argv[1])["nodes"]}
    b = json.loads(sys.argv[2])["nodes"]
except Exception:
    sys.exit()
best = max(((n["csi_sequence"] - a[n["node_id"]]["csi_sequence"], n["node_id"])
            for n in b if n.get("status") == "active" and n["node_id"] in a), default=None)
if best and best[0] >= 150:  # >= 10 frames/s over the 15 s sample
    print(best[1])
EOF
)"
    if [ -z "$node" ]; then
      send_notice "RuView calibration" "warning" "No node is streaming steadily enough (>= 10 frames/s) to calibrate on - retrying in 5 min." || true
      sleep 300; continue
    fi
    st="$(curl -s -m 5 "$base/calibration/status")"; boot="$(jfield "$st" boot_epoch)"
    curl -s -m 10 -X POST "$base/calibration/reset" -H 'content-type: application/json' -d "{\"boot_epoch\":\"$boot\"}" >/dev/null
    resp="$(curl -s -m 10 -X POST "$base/calibration/start?source_node_id=$node" -H 'content-type: application/json' \
      -d "{\"binding_digest\":\"$digest\",\"source_node_ids\":[$node]}")"
    ok="$(jfield "$resp" success)"; sess="$(jfield "$resp" session_id)"; boot="$(jfield "$resp" boot_epoch)"
    if [ "$ok" != "true" ] || [ -z "$sess" ]; then
      send_notice "RuView calibration" "warning" "Couldn't start calibration on node $node: $(jfield "$resp" error_code) $(jfield "$resp" error)$(jfield "$resp" message) - retrying in 5 min." || true
      sleep 300; continue
    fi
    send_notice "RuView calibration" "hourglass" "Calibrating now on node $node - keep the room empty for 10 minutes." || true
    for i in $(seq 1 90); do # up to ~22 min
      sleep 15
      st="$(curl -s -m 5 "$base/calibration/status")"
      fc="$(jfield "$st" frame_count)"; mf="$(jfield "$st" min_frames)"; el="$(jfield "$st" elapsed_s)"; md="$(jfield "$st" min_duration_s)"
      if python3 -c "import sys; fc,mf,el,md=(float(x or 0) for x in sys.argv[1:]); sys.exit(0 if mf>0 and md>0 and fc>=mf and el>=md else 1)" "$fc" "$mf" "$el" "$md"; then
        break
      fi
    done
    resp="$(curl -s -m 20 -X POST "$base/calibration/stop" -H 'content-type: application/json' \
      -d "{\"boot_epoch\":\"$boot\",\"session_id\":\"$sess\",\"binding_digest\":\"$digest\",\"source_node_ids\":[$node]}")"
    if [ "$(jfield "$resp" success)" = "true" ]; then
      send_notice "RuView calibration" "white_check_mark" "Calibrated on node $node. For the next 12 h RuView shows breathing and heart rate when exactly one person is in the room (Sensing tab). It recalibrates automatically in 11.5 h - you'll get a heads-up first." || true
      sleep 41400
    else
      send_notice "RuView calibration" "warning" "Calibration didn't finish ($(jfield "$resp" error_code) $(jfield "$resp" error)$(jfield "$resp" message); frames $fc/$mf, ${el}/${md} s) - retrying in 5 min." || true
      sleep 300
    fi
  done
}

if [ "${RUVIEW_CALIBRATE:-0}" = 1 ]; then
  if command -v python3 >/dev/null 2>&1; then
    calibrate_loop >>/tmp/ruview-calibration.log 2>&1 &
    PIDS="$PIDS $!"
    echo "Automatic calibration scheduled in ${RUVIEW_CALIBRATION_DELAY:-120} s (progress: /tmp/ruview-calibration.log and your ntfy/Telegram)."
  else
    echo "python3 not found - skipping automatic calibration (RuView won't show breathing/heart rate without it)."
  fi
else
  echo "Vitals: published without empty-room calibration (uncalibrated estimates). RUVIEW_CALIBRATE=1 switches to RuView's strict calibrated mode."
fi

# shellcheck disable=SC2086
printf '%s\n' $PIDS > /tmp/ruview-pids

say "Opening the dashboard"
sleep 1
DASHBOARD_URL="http://localhost:$HTTP_PORT/ui/index.html"
if command -v open >/dev/null 2>&1; then open "$DASHBOARD_URL"
elif command -v xdg-open >/dev/null 2>&1; then xdg-open "$DASHBOARD_URL"
else echo "Open $DASHBOARD_URL in your browser"
fi

echo
echo "Running - everything above is a child of this shell, not detached."
echo "Dashboard: $DASHBOARD_URL"
[ -n "$PUBLIC_LINK" ] && echo "Public:    $PUBLIC_LINK"
echo "Look at the 'WiFi Sensing' and 'Live Demo' tabs for live signal/presence data from your own network."
echo "Press Ctrl+C, or close this terminal, to stop everything."

# Foreground on purpose: with `&` alone, this script returns to your
# shell prompt immediately, and it's not obvious from that whether
# sensing-server/ruview-bridge are still alive, crashed, or were never
# reachable in the first place (confirmed live: they kept running fine in
# that case, but there was no visible sign of it). Wait here instead, and
# stop everything cleanly on Ctrl+C/terminal close rather than leaving it
# running invisibly in the background.
# shellcheck disable=SC2086
wait $PIDS
