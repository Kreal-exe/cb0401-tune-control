#!/bin/sh
#
# cleanup.sh — removes unnecessary telemetry, dead cron jobs, and (optionally)
# unused background daemons from a Xiaomi CB0401/CB0401V2 running stock
# MiWiFi firmware. Meant to be copied onto the router and run there
# directly (see ../setup.sh / ../setup.ps1, which do this for you).
#
# Safe by default: only touches things with zero functional impact on the
# router's actual job (routing/Wi-Fi/cellular). Riskier items that MIGHT
# matter to some users are behind explicit flags — read the warnings below
# before turning them on.
#
# Usage (run ON the router, as root):
#   sh cleanup.sh                        # safe telemetry + dead cron only
#   sh cleanup.sh --disable-mesh         # + Xiaomi Mesh daemons (only if you
#                                          don't use additional Mesh satellite
#                                          nodes with this router)
#   sh cleanup.sh --disable-messagingagent
#                                        # + the MQTT cloud channel (only if
#                                          you don't rely on the Mi Home app
#                                          or carrier remote support for this
#                                          device)
#   sh cleanup.sh --all                  # everything above
#
set -e

DISABLE_MESH=0
DISABLE_MSGAGENT=0
for arg in "$@"; do
  case "$arg" in
    --disable-mesh) DISABLE_MESH=1 ;;
    --disable-messagingagent) DISABLE_MSGAGENT=1 ;;
    --all) DISABLE_MESH=1; DISABLE_MSGAGENT=1 ;;
  esac
done

DIR=/etc/crontabs/patches

say() { echo "==> $*"; }

disable_service() {
  # $1 = init.d service name
  if [ -f "/etc/init.d/$1" ]; then
    /etc/init.d/"$1" stop >/dev/null 2>&1 || true
    /etc/init.d/"$1" disable >/dev/null 2>&1 || true
    say "disabled service: $1"
  fi
}

remove_cron_matching() {
  # $1 = grep pattern to remove from the root crontab
  if [ -f /etc/crontabs/root ] && grep -q "$1" /etc/crontabs/root 2>/dev/null; then
    grep -v "$1" /etc/crontabs/root > /etc/crontabs/root.new
    mv /etc/crontabs/root.new /etc/crontabs/root
    say "removed cron entry matching: $1"
  fi
}

say "Starting cleanup (mesh=$DISABLE_MESH, messagingagent=$DISABLE_MSGAGENT)"

# --- Telemetry: safe to remove, no functional impact -----------------------
# (These edit /etc/crontabs/root, which is a symlink into this router's
# persistent /data partition, so — unlike the service disables below —
# they survive a reboot on their own, no extra persistence needed.)

# Usage-statistics uploader (web.log/rom.log/privacy.log -> Xiaomi cloud)
remove_cron_matching 'sp_check.sh'
uci set misc.features='features' 2>/dev/null || true
uci set misc.features.statpointsNoLog='1' 2>/dev/null || true
uci commit misc 2>/dev/null || true

# Automatic firmware pre-download check (also protects any manual firmware
# downgrade/config from being silently overwritten by an OTA update)
remove_cron_matching 'otapredownload'

# Google Breakpad crash reporter (uploads crash dumps to Xiaomi)
disable_service breakpad

# --- Dead cron entries: reference files that don't exist on this build -----

remove_cron_matching 'mobile_accel.sh'
remove_cron_matching 'run-parts'   # /etc/periodic doesn't exist on this firmware

say "Safe cleanup done."

# --- Optional: Xiaomi Mesh daemons ------------------------------------------
# cab_meshd / miwifi-discovery / miwifi-roam run unconditionally at boot even
# if you have no additional Mesh satellite node. If you DO use Mesh, do not
# pass --disable-mesh.
if [ "$DISABLE_MESH" = "1" ]; then
  disable_service cab_meshd
  disable_service miwifi-discovery
  disable_service miwifi-roam
  say "Mesh daemons disabled."
fi

# --- Optional: messagingagent (MQTT cloud channel) --------------------------
# This may be used by the Mi Home app or by your carrier for remote support
# on a leased/branded CPE. Only disable it if you don't need either.
if [ "$DISABLE_MSGAGENT" = "1" ]; then
  disable_service messagingagent.sh
  say "messagingagent disabled."
fi

# --- Make the service disables above survive a reboot -----------------------
# `/etc/init.d/X disable` only updates a symlink under /etc/rc.d, and this
# router's /etc is entirely ramfs (ephemeral) - same root cause as the
# root-password persistence issue documented in the README. Confirmed live:
# breakpad/mesh/messagingagent silently came back "enabled" after a reboot
# despite being disabled here. Fixed the same way ssh_patch.sh keeps SSH
# open: a small script + a once-a-minute cron job that's a no-op after the
# first run each boot (a /tmp lock file, cleared on every reboot since /tmp
# is ramfs too), so it reapplies within a minute of booting and then goes
# quiet.
mkdir -p "$DIR"
cat > "$DIR/cleanup.conf" <<EOF
DISABLE_MESH=$DISABLE_MESH
DISABLE_MSGAGENT=$DISABLE_MSGAGENT
EOF

cat > "$DIR/cleanup_persist.sh" <<'PERSIST_EOF'
#!/bin/sh
LOG_FN=/tmp/cleanup_persist.log
[ -e "$LOG_FN" ] && exit 0
: > "$LOG_FN"
DIR=/etc/crontabs/patches
[ -f "$DIR/cleanup.conf" ] && . "$DIR/cleanup.conf"
disable_service() {
    if [ -f "/etc/init.d/$1" ]; then
        /etc/init.d/"$1" stop >/dev/null 2>&1 || true
        /etc/init.d/"$1" disable >/dev/null 2>&1 || true
    fi
}
disable_service breakpad
if [ "$DISABLE_MESH" = "1" ]; then
    disable_service cab_meshd
    disable_service miwifi-discovery
    disable_service miwifi-roam
fi
if [ "$DISABLE_MSGAGENT" = "1" ]; then
    disable_service messagingagent.sh
fi
echo done > "$LOG_FN"
PERSIST_EOF
chmod +x "$DIR/cleanup_persist.sh"

grep -q cleanup_persist.sh /etc/crontabs/root 2>/dev/null || echo "*/1 * * * * $DIR/cleanup_persist.sh >/dev/null 2>&1" >> /etc/crontabs/root
/etc/init.d/cron restart >/dev/null 2>&1 || true

say "Done. The telemetry/cron cleanup persists on its own; the service disables above now also reapply automatically within a minute of every reboot."
