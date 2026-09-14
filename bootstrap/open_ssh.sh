#!/bin/bash
#
# open_ssh.sh — opens persistent root SSH on a factory-fresh CB0401V2,
# without xmir-patcher or any Python dependency.
#
# How this works (documented Xiaomi router behavior, not a novel exploit):
#
#   1. The router's web UI exposes an UNAUTHENTICATED endpoint,
#      api/xqsystem/init_info, which includes the device's serial number.
#   2. Xiaomi's firmware-imaging tool (mkxqimage) derives a default
#      root/Telnet password from that serial number:
#         salt     = 6d2df50a-250f-4a30-a5e6-d44fb0960aa0
#                    (the segments of a hardcoded GUID, reversed)
#         password = md5(serial + salt), first 8 hex characters
#   3. Stock firmware ships with Telnet (port 23) always enabled, and root
#      accepts that derived password.
#   4. Once logged in over Telnet, this script writes the same "soft"
#      persistence patch used by xmir-patcher's install_ssh.py: it enables
#      dropbear (patching /etc/init.d/dropbear's release-build gate),
#      sets nvram ssh_en=1, and installs a cron job (+ a firewall include
#      hook) that keeps re-applying that every minute, so it survives
#      reboots. It then installs this toolkit's own SSH key directly,
#      while we already have root - no separate password step needed
#      afterward.
#
# This intentionally skips xmir-patcher's OTHER install path (a custom
# kernel module patching the immutable "bdata" NVRAM partition) - that one
# failed in testing on this hardware ("sections missing", kernel build
# mismatch) and was never the part that actually worked.
#
# Usage: ./open_ssh.sh [router_ip] [pubkey_file]
#
set -e

ROUTER_IP="${1:-${ROUTER_IP:-192.168.31.1}}"
PUBKEY_FILE="${2:-}"
TELNET_TIMEOUT="${TELNET_TIMEOUT:-8}"

say() { echo "==> $*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required."
command -v bash >/dev/null 2>&1 || die "bash is required."

# --- 1. Fetch the serial number (no auth needed) ----------------------------

say "Fetching device info from $ROUTER_IP..."
INFO_JSON="$(curl -s -m 8 "http://$ROUTER_IP/cgi-bin/luci/api/xqsystem/init_info")"
[ -n "$INFO_JSON" ] || die "No response from the router's web UI. Is it powered on and reachable at $ROUTER_IP?"

SERIAL="$(echo "$INFO_JSON" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"\(.*\)"/\1/')"
HARDWARE="$(echo "$INFO_JSON" | grep -o '"hardware":"[^"]*"' | head -1 | sed 's/"hardware":"\(.*\)"/\1/')"
[ -n "$SERIAL" ] || die "Could not find a serial number in the router's response - this may not be a supported Xiaomi/MiWiFi device, or it isn't in factory state yet (finish the initial setup at http://$ROUTER_IP first)."
say "Device: ${HARDWARE:-unknown} (serial: $SERIAL)"

# --- 2. Derive the default root/Telnet password -----------------------------

SALT="6d2df50a-250f-4a30-a5e6-d44fb0960aa0"
if command -v openssl >/dev/null 2>&1; then
  MD5_HEX="$(printf '%s' "${SERIAL}${SALT}" | openssl dgst -md5 -r | awk '{print $1}')"
elif command -v md5sum >/dev/null 2>&1; then
  MD5_HEX="$(printf '%s' "${SERIAL}${SALT}" | md5sum | awk '{print $1}')"
elif command -v md5 >/dev/null 2>&1; then
  MD5_HEX="$(printf '%s' "${SERIAL}${SALT}" | md5 -q)"
else
  die "Need one of: openssl, md5sum, md5 (none found) to derive the default password."
fi
DEFAULT_PASSWORD="${MD5_HEX:0:8}"
say "Derived default root password (from the serial number, not a secret we invented)."

# --- 3. Log in over Telnet and patch the router -----------------------------

if [ -z "$PUBKEY_FILE" ]; then
  PUBKEY_FILE="$(dirname "${BASH_SOURCE[0]}")/../gui/router_key.pub"
fi
[ -f "$PUBKEY_FILE" ] || die "SSH public key not found at $PUBKEY_FILE. Generate one first: ssh-keygen -t ed25519 -f router_key -N '' -C cb0401-tune-control-gui"
PUBKEY="$(cat "$PUBKEY_FILE")"

say "Connecting to Telnet on $ROUTER_IP:23 ..."
exec 3<>"/dev/tcp/$ROUTER_IP/23" || die "Could not open a TCP connection to $ROUTER_IP:23 (Telnet). It may already be disabled - if SSH already works, you don't need this script."

# Reads from fd 3 one byte at a time (no external tools needed) until
# $1 is seen in the accumulated buffer, or $TELNET_TIMEOUT seconds pass.
# This is deliberately dumb about Telnet IAC option negotiation (bash's
# /dev/tcp is a raw socket, it doesn't speak Telnet) - embedded telnetd
# implementations like this router's are normally lenient about a client
# that never responds to negotiation and just waits for plain-text
# prompts, which is all this needs.
tn_expect() {
  local needle="$1" buf="" ch deadline
  deadline=$(( $(date +%s) + TELNET_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if IFS= read -r -t 1 -N 1 ch <&3; then
      buf+="$ch"
      case "$buf" in
        *"$needle"*) TN_BUF="$buf"; return 0 ;;
      esac
    fi
  done
  TN_BUF="$buf"
  return 1
}

tn_send() { printf '%s\r\n' "$1" >&3; }

PROMPT='# '

if tn_expect 'login:'; then
  tn_send 'root'
  tn_expect 'assword:' || die "Telnet didn't ask for a password after the username - unexpected prompt sequence."
  tn_send "$DEFAULT_PASSWORD"
  tn_expect "$PROMPT" || die "Login with the derived default password was rejected. Either this router's password differs from the documented derivation, or it's already locked down."
elif tn_expect "$PROMPT"; then
  : # some devices drop you straight into a root shell, no login prompt
else
  die "Telnet didn't present a recognizable login or shell prompt within ${TELNET_TIMEOUT}s."
fi

say "Logged in over Telnet. Patching the router..."

tn_send 'mkdir -p /etc/crontabs/patches'
tn_expect "$PROMPT"

# Same "soft" persistence patch as xmir-patcher's install_ssh.py: enable
# dropbear (removing its release-build gate), flip nvram ssh_en, and keep
# re-applying that every minute via cron + a firewall include hook so it
# survives reboots.
tn_send "cat > /etc/crontabs/patches/ssh_patch.sh << 'CB0401_SSH_PATCH_EOF'
#!/bin/sh
LOG_FN=/tmp/ssh_patch.log
[ -e \$LOG_FN ] && return 0
: > \$LOG_FN
SSH_EN=\`nvram get ssh_en\`
if [ \"\$SSH_EN\" != \"1\" ]; then
    nvram set ssh_en=1
    nvram commit
fi
if grep -q '= \"release\"' /etc/init.d/dropbear ; then
    sed -i 's/= \"release\"/= \"XXXXXX\"/g'  /etc/init.d/dropbear
fi
/etc/init.d/dropbear enable
/etc/init.d/dropbear restart
echo \"ssh enabled\" > \$LOG_FN
CB0401_SSH_PATCH_EOF"
tn_expect "$PROMPT"

tn_send 'chmod +x /etc/crontabs/patches/ssh_patch.sh'
tn_expect "$PROMPT"

tn_send 'grep -v "/ssh_patch.sh" /etc/crontabs/root > /etc/crontabs/root.new 2>/dev/null || echo "" > /etc/crontabs/root.new; echo "*/1 * * * * /etc/crontabs/patches/ssh_patch.sh >/dev/null 2>&1" >> /etc/crontabs/root.new; mv /etc/crontabs/root.new /etc/crontabs/root'
tn_expect "$PROMPT"

tn_send "uci set firewall.auto_ssh_patch=include; uci set firewall.auto_ssh_patch.type='script'; uci set firewall.auto_ssh_patch.path='/etc/crontabs/patches/ssh_patch.sh'; uci set firewall.auto_ssh_patch.enabled='1'; uci commit firewall"
tn_expect "$PROMPT"

tn_send 'rm -f /tmp/ssh_patch.log; sh /etc/crontabs/patches/ssh_patch.sh'
tn_expect "$PROMPT"

say "Installing this toolkit's SSH key..."
tn_send "mkdir -p /etc/dropbear; grep -qF '$PUBKEY' /etc/dropbear/authorized_keys 2>/dev/null || echo '$PUBKEY' >> /etc/dropbear/authorized_keys; chmod 600 /etc/dropbear/authorized_keys"
tn_expect "$PROMPT"

tn_send 'exit'
exec 3<&- 3>&- 2>/dev/null || true

say "Done. SSH should now be enabled and this toolkit's key installed."
say "Verify with: ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -i <key> root@$ROUTER_IP"
