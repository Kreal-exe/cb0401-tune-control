#!/bin/bash
#
# setup.sh — one-shot, fully self-contained setup for the CB0401 Tune + Control,
# for macOS and Linux. No Python, no third-party exploit tool - just bash,
# an SSH client, and (optionally) Go if you want to build the GUI from
# source instead of using a prebuilt binary.
#
# What this does, in order:
#   1. Checks prerequisites (ssh/scp, curl).
#   2. Opens persistent root SSH on the router (bootstrap/open_ssh.sh) -
#      see that script's header comment for exactly how and why this
#      works. Skipped if SSH already works.
#   3. Falls back to a password-based key install if step 2 wasn't needed
#      (SSH already open with the factory default password) or didn't
#      apply (Telnet already closed) - this needs the router's password
#      exactly once. We never change this password - see the README for
#      why.
#   4. Sets up push notifications and device-block commands, either via a
#      random private ntfy.sh topic or a Telegram bot (your choice), and
#      copies router/*.sh onto the router with that config baked in (over
#      the key-based connection from step 2/3 — no more passwords needed
#      after this point).
#   5. Runs router/cleanup.sh on the router to remove telemetry/dead cron
#      jobs (safe by default — see cleanup.sh's own flags for optional
#      extras).
#   6. Builds (if needed) and launches the GUI at gui/.
#
# Safe to re-run: every step is idempotent.
#
set -e

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROUTER_IP="${ROUTER_IP:-192.168.31.1}"
KEY_PATH="$REPO_DIR/gui/router_key"
ENV_FILE="$REPO_DIR/gui/.env"

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -o ConnectTimeout=5)

say() { echo; echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

# Modern OpenSSH (9.0+, the default on macOS 13+ and recent Linux distros)
# switched scp to an SFTP-based transfer by default, which needs
# /usr/libexec/sftp-server on the remote side - this router's dropbear does
# not have one ("ash: /usr/libexec/sftp-server: not found"), only the
# legacy SCP protocol works against it (-O). Older OpenSSH clients (<9.0)
# don't recognize -O at all and use the legacy protocol anyway, so this
# tries -O first and falls back to plain scp if the local client rejects
# the flag outright.
scp_to_router() {
  local err
  err="$(scp -O "${SSH_OPTS[@]}" -i "$KEY_PATH" "$@" 2>&1)" && return 0
  if echo "$err" | grep -qi 'unknown option'; then
    scp "${SSH_OPTS[@]}" -i "$KEY_PATH" "$@"
  else
    echo "$err" >&2
    return 1
  fi
}

say "CB0401 Tune + Control setup"
echo "Router IP:      $ROUTER_IP"
echo "GUI key path:   $KEY_PATH"

# --- 1. Prerequisites -------------------------------------------------------

say "Checking prerequisites"
command -v ssh >/dev/null 2>&1 || die "an OpenSSH client (ssh/scp) is required. Install it and re-run."
command -v curl >/dev/null 2>&1 || die "curl is required. Install it and re-run."

if ! command -v sshpass >/dev/null 2>&1; then
  say "Installing sshpass (used by the GUI to self-heal if it ever loses SSH key access)"
  if command -v brew >/dev/null 2>&1; then
    brew install sshpass 2>/dev/null || brew install hudochenkov/sshpass/sshpass || true
  elif command -v apt-get >/dev/null 2>&1; then
    sudo apt-get update && sudo apt-get install -y sshpass || true
  fi
  command -v sshpass >/dev/null 2>&1 || echo "Could not install sshpass automatically — the GUI will still work, it just won't be able to self-heal a lost SSH key. Install sshpass manually to enable that."
fi

mkdir -p "$(dirname "$KEY_PATH")"
if [ ! -f "$KEY_PATH" ]; then
  ssh-keygen -t ed25519 -f "$KEY_PATH" -N "" -C "cb0401-tune-control-gui" -q
fi

# --- 2. Open SSH on the router -----------------------------------------------

if ssh "${SSH_OPTS[@]}" -i "$KEY_PATH" -o BatchMode=yes "root@$ROUTER_IP" true 2>/dev/null; then
  say "SSH already works via key — nothing to open."
else
  say "Opening SSH access on the router"
  if "$REPO_DIR/bootstrap/open_ssh.sh" "$ROUTER_IP" "$KEY_PATH.pub"; then
    say "SSH opened and this toolkit's key installed."
  else
    echo "The automatic bootstrap didn't apply (e.g. Telnet is already closed, which is"
    echo "normal if SSH is already enabled with the factory password some other way)."

    # --- 3. Fall back to a password-based key install -------------------------

    say "Falling back to installing the key over SSH with a password"
    PUBKEY="$(cat "$KEY_PATH.pub")"
    INSTALL_CMD="mkdir -p /etc/dropbear; grep -qF '$PUBKEY' /etc/dropbear/authorized_keys 2>/dev/null || echo '$PUBKEY' >> /etc/dropbear/authorized_keys; chmod 600 /etc/dropbear/authorized_keys"

    # If a previous run on this router already recorded a password (e.g.
    # changed through the GUI's own "Change root password" field), try that
    # first via sshpass - no need to retype it by hand every time just
    # because this particular checkout's key hasn't been installed yet.
    KNOWN_PASSWORD=""
    if [ -f "$ENV_FILE" ] && grep -q '^ROUTER_ROOT_PASSWORD=' "$ENV_FILE"; then
      KNOWN_PASSWORD="$(grep '^ROUTER_ROOT_PASSWORD=' "$ENV_FILE" | cut -d= -f2-)"
    fi
    if [ -n "$KNOWN_PASSWORD" ] && command -v sshpass >/dev/null 2>&1 \
      && sshpass -p "$KNOWN_PASSWORD" ssh "${SSH_OPTS[@]}" "root@$ROUTER_IP" "$INSTALL_CMD" 2>/dev/null; then
      say "Installed the key using the password already on file."
    else
      echo "You will be asked for the router's SSH password now — the derived default"
      echo "described in the README's 'How SSH access is opened' section, or whatever"
      echo "you've since changed it to."
      echo
      ssh "${SSH_OPTS[@]}" "root@$ROUTER_IP" "$INSTALL_CMD" \
        || die "Could not reach the router over SSH with that password either. Check that it's reachable at $ROUTER_IP."
    fi
  fi
fi

ssh "${SSH_OPTS[@]}" -i "$KEY_PATH" -o BatchMode=yes "root@$ROUTER_IP" true \
  || die "Key-based login still fails after installing the key — check the router's dropbear config."
echo "Key-based SSH login confirmed. No more passwords needed from here on."

# --- 4. Notifications: ntfy.sh or Telegram, + device-monitor scripts -------

say "Setting up push notifications"
# Precedence: an explicit NOTIFY_BACKEND env var always wins (needed for a
# non-interactive re-run that switches backend); otherwise reuse whatever
# was chosen last time; otherwise ask, if we can; otherwise default to ntfy.
if [ -n "${NOTIFY_BACKEND:-}" ]; then
  echo "Using notification backend from the NOTIFY_BACKEND environment variable: $NOTIFY_BACKEND"
elif [ -f "$ENV_FILE" ] && grep -q '^NOTIFY_BACKEND=' "$ENV_FILE"; then
  NOTIFY_BACKEND="$(grep '^NOTIFY_BACKEND=' "$ENV_FILE" | cut -d= -f2-)"
  echo "Reusing existing notification backend from $ENV_FILE: $NOTIFY_BACKEND"
elif [ -t 0 ]; then
  echo "Choose how you want to receive device alerts and send block/red-alert commands:"
  echo "  1) ntfy.sh  - zero setup: just a free app and a random shared topic (default)"
  echo "  2) Telegram - needs a bot token + your chat ID, but ties access to your account"
  read -r -p "Choice [1]: " choice
  case "$choice" in
    2) NOTIFY_BACKEND="telegram" ;;
    *) NOTIFY_BACKEND="ntfy" ;;
  esac
fi
NOTIFY_BACKEND="${NOTIFY_BACKEND:-ntfy}"

NTFY_TOPIC=""
TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-}"
TELEGRAM_CHAT_ID="${TELEGRAM_CHAT_ID:-}"

if [ "$NOTIFY_BACKEND" = "telegram" ]; then
  if [ -z "$TELEGRAM_BOT_TOKEN" ] && [ -f "$ENV_FILE" ] && grep -q '^TELEGRAM_BOT_TOKEN=' "$ENV_FILE"; then
    TELEGRAM_BOT_TOKEN="$(grep '^TELEGRAM_BOT_TOKEN=' "$ENV_FILE" | cut -d= -f2-)"
    TELEGRAM_CHAT_ID="$(grep '^TELEGRAM_CHAT_ID=' "$ENV_FILE" | cut -d= -f2-)"
    echo "Reusing existing Telegram bot config from $ENV_FILE"
  fi
  if [ -z "$TELEGRAM_BOT_TOKEN" ] && [ -t 0 ]; then
    echo "Create a bot with @BotFather on Telegram (free, one-time) to get a token,"
    echo "then send it any message so it can see your chat ID."
    read -r -p "Telegram bot token: " TELEGRAM_BOT_TOKEN
    read -r -p "Your Telegram chat ID: " TELEGRAM_CHAT_ID
  fi
  [ -n "$TELEGRAM_BOT_TOKEN" ] && [ -n "$TELEGRAM_CHAT_ID" ] || die "Telegram backend selected but TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID aren't set (export them as environment variables for a non-interactive run)."
  echo "Telegram bot configured."
  echo "(The bot token is a secret — anyone who has it can send messages as your bot"
  echo " and read what's sent to it. It's stored only in $ENV_FILE, which is gitignored.)"
else
  if [ -f "$ENV_FILE" ] && grep -q '^NTFY_TOPIC=' "$ENV_FILE"; then
    NTFY_TOPIC="$(grep '^NTFY_TOPIC=' "$ENV_FILE" | cut -d= -f2-)"
    echo "Reusing existing topic from $ENV_FILE"
  else
    if command -v openssl >/dev/null 2>&1; then
      NTFY_TOPIC="cb0401v2-$(openssl rand -hex 8)"
    else
      NTFY_TOPIC="cb0401v2-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    fi
  fi
  echo "ntfy.sh topic: $NTFY_TOPIC"
  echo "(This is a shared secret — anyone who knows it can read your device alerts"
  echo " and send block/red-alert commands. Keep it private; it is never committed to git.)"
fi

TMP_DIR="$(mktemp -d)"
cat > "$TMP_DIR/notify.conf" <<EOF
NOTIFY_BACKEND=$NOTIFY_BACKEND
NTFY_TOPIC=$NTFY_TOPIC
TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN
TELEGRAM_CHAT_ID=$TELEGRAM_CHAT_ID
EOF
cp "$REPO_DIR/router/notify_common.sh" "$TMP_DIR/notify_common.sh"
cp "$REPO_DIR/router/device_monitor.sh" "$TMP_DIR/device_monitor.sh"
cp "$REPO_DIR/router/dhcp_notify.sh" "$TMP_DIR/dhcp_notify.sh"
cp "$REPO_DIR/router/command_watcher.sh" "$TMP_DIR/command_watcher.sh"
cp "$REPO_DIR/router/cleanup.sh" "$TMP_DIR/cleanup.sh"

scp_to_router \
  "$TMP_DIR"/notify.conf "$TMP_DIR"/notify_common.sh "$TMP_DIR"/device_monitor.sh \
  "$TMP_DIR"/dhcp_notify.sh "$TMP_DIR"/command_watcher.sh "$TMP_DIR"/cleanup.sh \
  "root@$ROUTER_IP:/tmp/" >/dev/null
rm -rf "$TMP_DIR"

# New-device alerts fire the instant dnsmasq grants a lease (dhcp_notify.sh,
# wired as dnsmasq's own --dhcp-script hook below) instead of on a polling
# timer - no delay, and nothing runs on the router between actual events.
# device_monitor.sh is kept only for the one-shot baseline scan right after
# install (so devices already connected before setup get seeded into
# notified_macs.txt instead of alerting the moment the hook goes live) and
# as a manual re-scan escape hatch; the old cron entry that used to poll it
# every 3 minutes is removed if this is a re-run of an older install.
ssh "${SSH_OPTS[@]}" -i "$KEY_PATH" "root@$ROUTER_IP" '
  mkdir -p /etc/crontabs/patches
  cp /tmp/notify.conf /etc/crontabs/patches/notify.conf
  chmod 600 /etc/crontabs/patches/notify.conf
  cp /tmp/notify_common.sh /etc/crontabs/patches/notify_common.sh
  cp /tmp/device_monitor.sh /etc/crontabs/patches/device_monitor.sh
  cp /tmp/dhcp_notify.sh /etc/crontabs/patches/dhcp_notify.sh
  cp /tmp/command_watcher.sh /etc/crontabs/patches/command_watcher.sh
  rm -f /etc/crontabs/patches/ntfy_command_watcher.sh
  chmod +x /etc/crontabs/patches/notify_common.sh /etc/crontabs/patches/device_monitor.sh /etc/crontabs/patches/dhcp_notify.sh /etc/crontabs/patches/command_watcher.sh
  touch /etc/crontabs/patches/known_macs.txt
  sh /etc/crontabs/patches/device_monitor.sh
  uci set dhcp.@dnsmasq[0].dhcpscript="/etc/crontabs/patches/dhcp_notify.sh"
  uci commit dhcp
  /etc/init.d/dnsmasq reload >/dev/null 2>&1 || /etc/init.d/dnsmasq restart >/dev/null 2>&1 || true
  sed -i "/ntfy_command_watcher\.sh/d; /\/device_monitor\.sh/d" /etc/crontabs/root 2>/dev/null || true
  grep -q command_watcher.sh /etc/crontabs/root 2>/dev/null || echo "*/2 * * * * sh /etc/crontabs/patches/command_watcher.sh" >> /etc/crontabs/root
  /etc/init.d/cron restart >/dev/null 2>&1 || true
'
echo "New-device alerts wired to dnsmasq (instant, no polling); command watcher installed on the router's crontab."

# --- 5. Cleanup --------------------------------------------------------

say "Cleaning up telemetry and dead cron jobs on the router"
# CLEANUP_FLAGS lets you pre-choose the opt-in items (--disable-mesh /
# --disable-messagingagent / --all) so re-running setup doesn't need a
# manual follow-up SSH each time. Leave unset for the safe-only default.
ssh "${SSH_OPTS[@]}" -i "$KEY_PATH" "root@$ROUTER_IP" "sh /tmp/cleanup.sh ${CLEANUP_FLAGS:-}"
if [ -z "${CLEANUP_FLAGS:-}" ]; then
  echo "(To also disable Xiaomi Mesh daemons or messagingagent, either set"
  echo " CLEANUP_FLAGS='--disable-mesh'/'--disable-messagingagent'/'--all' and"
  echo " re-run this script, or SSH in and run cleanup.sh with those flags"
  echo " directly — see router/cleanup.sh for what each one actually does"
  echo " before opting in. These now persist across reboots on their own.)"
fi

# --- 6. GUI --------------------------------------------------------------

say "Setting up the local GUI"

cat > "$ENV_FILE" <<EOF
ROUTER_IP=$ROUTER_IP
ROUTER_ROOT_PASSWORD=root
NOTIFY_BACKEND=$NOTIFY_BACKEND
NTFY_TOPIC=$NTFY_TOPIC
TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN
TELEGRAM_CHAT_ID=$TELEGRAM_CHAT_ID
EOF
chmod 600 "$ENV_FILE"

say "Setup complete. Starting the GUI..."
exec "$REPO_DIR/gui/start_gui.sh"
