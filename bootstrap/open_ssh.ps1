#
# open_ssh.ps1 — opens persistent root SSH on a factory-fresh CB0401V2,
# without xmir-patcher or any Python dependency. Windows equivalent of
# open_ssh.sh - see that file for the full explanation of *why* this works
# (documented Xiaomi router behavior: an unauthenticated endpoint leaks the
# serial number, which deterministically derives the default root/Telnet
# password via a hardcoded salt from Xiaomi's own firmware-imaging tool).
#
# Usage: powershell -ExecutionPolicy Bypass -File open_ssh.ps1 [-RouterIp <ip>] [-PubKeyFile <path>]
#
param(
    [string]$RouterIp = $(if ($env:ROUTER_IP) { $env:ROUTER_IP } else { '192.168.31.1' }),
    [string]$PubKeyFile = '',
    [int]$TelnetTimeoutSec = 8
)
$ErrorActionPreference = 'Stop'

function Say($msg) { Write-Host "==> $msg" }
function Die($msg) { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }

# setup.ps1 checks $LASTEXITCODE after running this script to decide
# whether to fall back to a password-based key install - which only works
# if we guarantee a real process exit code even on a totally unexpected
# error, since a PowerShell script that dies to an uncaught exception
# (rather than an explicit `exit`) does not reliably set $LASTEXITCODE.
try {

# --- 1. Fetch the serial number (no auth needed) ----------------------------

Say "Fetching device info from $RouterIp..."
try {
    $infoJson = Invoke-RestMethod -Uri "http://$RouterIp/cgi-bin/luci/api/xqsystem/init_info" -TimeoutSec 8
} catch {
    Die "No response from the router's web UI. Is it powered on and reachable at $RouterIp ($($_.Exception.Message))?"
}
$serial = $infoJson.id
$hardware = $infoJson.hardware
if (-not $serial) {
    Die "Could not find a serial number in the router's response - this may not be a supported Xiaomi/MiWiFi device, or it isn't in factory state yet (finish the initial setup at http://$RouterIp first)."
}
Say "Device: $(if ($hardware) { $hardware } else { 'unknown' }) (serial: $serial)"

# --- 2. Derive the default root/Telnet password -----------------------------

$salt = '6d2df50a-250f-4a30-a5e6-d44fb0960aa0'
$md5 = [System.Security.Cryptography.MD5]::Create()
$hashBytes = $md5.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$serial$salt"))
$md5Hex = -join ($hashBytes | ForEach-Object { $_.ToString('x2') })
$defaultPassword = $md5Hex.Substring(0, 8)
Say 'Derived default root password (from the serial number, not a secret we invented).'

# --- 3. Log in over Telnet and patch the router -----------------------------

if (-not $PubKeyFile) {
    $PubKeyFile = Join-Path $PSScriptRoot '..\gui\router_key.pub'
}
if (-not (Test-Path $PubKeyFile)) {
    Die "SSH public key not found at $PubKeyFile. Generate one first: ssh-keygen -t ed25519 -f router_key -N '""' -C cb0401-tune-control-gui"
}
$pubKey = (Get-Content $PubKeyFile -Raw).Trim()

Say "Connecting to Telnet on ${RouterIp}:23 ..."
$client = New-Object System.Net.Sockets.TcpClient
try {
    $connectTask = $client.ConnectAsync($RouterIp, 23)
    if (-not $connectTask.Wait(5000)) {
        Die "Could not open a TCP connection to ${RouterIp}:23 (Telnet) within 5s. It may already be disabled - if SSH already works, you don't need this script."
    }
} catch {
    Die "Could not open a TCP connection to ${RouterIp}:23 (Telnet): $($_.Exception.Message). It may already be disabled - if SSH already works, you don't need this script."
}
$stream = $client.GetStream()

# Reads from the raw socket one byte at a time until $needle is seen in the
# accumulated buffer, or $TelnetTimeoutSec seconds pass. Deliberately dumb
# about Telnet IAC option negotiation (this is a plain TCP stream, it
# doesn't speak Telnet) - embedded telnetd implementations like this
# router's are normally lenient about a client that never responds to
# negotiation and just waits for plain-text prompts, which is all this
# needs.
function Wait-ForText($needle) {
    $buf = New-Object System.Text.StringBuilder
    $deadline = (Get-Date).AddSeconds($TelnetTimeoutSec)
    $byteBuf = New-Object byte[] 1
    while ((Get-Date) -lt $deadline) {
        if ($stream.DataAvailable) {
            $n = $stream.Read($byteBuf, 0, 1)
            if ($n -gt 0) {
                [void]$buf.Append([char]$byteBuf[0])
                if ($buf.ToString().Contains($needle)) { return $true }
            }
        } else {
            Start-Sleep -Milliseconds 50
        }
    }
    return $false
}

function Send-Line($text) {
    $bytes = [System.Text.Encoding]::ASCII.GetBytes("$text`r`n")
    $stream.Write($bytes, 0, $bytes.Length)
}

$prompt = '# '

if (Wait-ForText 'login:') {
    Send-Line 'root'
    if (-not (Wait-ForText 'assword:')) { Die "Telnet didn't ask for a password after the username - unexpected prompt sequence." }
    Send-Line $defaultPassword
    if (-not (Wait-ForText $prompt)) { Die "Login with the derived default password was rejected. Either this router's password differs from the documented derivation, or it's already locked down." }
} elseif (Wait-ForText $prompt) {
    # some devices drop you straight into a root shell, no login prompt
} else {
    Die "Telnet didn't present a recognizable login or shell prompt within ${TelnetTimeoutSec}s."
}

Say 'Logged in over Telnet. Patching the router...'

Send-Line 'mkdir -p /etc/crontabs/patches'
Wait-ForText $prompt | Out-Null

# Same "soft" persistence patch as xmir-patcher's install_ssh.py: enable
# dropbear (removing its release-build gate), flip nvram ssh_en, and keep
# re-applying that every minute via cron + a firewall include hook so it
# survives reboots.
$sshPatchScript = @"
cat > /etc/crontabs/patches/ssh_patch.sh << 'CB0401_SSH_PATCH_EOF'
#!/bin/sh
LOG_FN=/tmp/ssh_patch.log
[ -e `$LOG_FN ] && return 0
: > `$LOG_FN
SSH_EN=``nvram get ssh_en``
if [ "`$SSH_EN" != "1" ]; then
    nvram set ssh_en=1
    nvram commit
fi
if grep -q '= "release"' /etc/init.d/dropbear ; then
    sed -i 's/= "release"/= "XXXXXX"/g'  /etc/init.d/dropbear
fi
/etc/init.d/dropbear enable
/etc/init.d/dropbear restart
echo "ssh enabled" > `$LOG_FN
CB0401_SSH_PATCH_EOF
"@
Send-Line $sshPatchScript
Wait-ForText $prompt | Out-Null

Send-Line 'chmod +x /etc/crontabs/patches/ssh_patch.sh'
Wait-ForText $prompt | Out-Null

Send-Line 'grep -v "/ssh_patch.sh" /etc/crontabs/root > /etc/crontabs/root.new 2>/dev/null || echo "" > /etc/crontabs/root.new; echo "*/1 * * * * /etc/crontabs/patches/ssh_patch.sh >/dev/null 2>&1" >> /etc/crontabs/root.new; mv /etc/crontabs/root.new /etc/crontabs/root'
Wait-ForText $prompt | Out-Null

Send-Line "uci set firewall.auto_ssh_patch=include; uci set firewall.auto_ssh_patch.type='script'; uci set firewall.auto_ssh_patch.path='/etc/crontabs/patches/ssh_patch.sh'; uci set firewall.auto_ssh_patch.enabled='1'; uci commit firewall"
Wait-ForText $prompt | Out-Null

Send-Line 'rm -f /tmp/ssh_patch.log; sh /etc/crontabs/patches/ssh_patch.sh'
Wait-ForText $prompt | Out-Null

Say "Installing this toolkit's SSH key..."
Send-Line "mkdir -p /etc/dropbear; grep -qF '$pubKey' /etc/dropbear/authorized_keys 2>/dev/null || echo '$pubKey' >> /etc/dropbear/authorized_keys; chmod 600 /etc/dropbear/authorized_keys"
Wait-ForText $prompt | Out-Null

Send-Line 'exit'
$stream.Close()
$client.Close()

Say 'Done. SSH should now be enabled and this toolkit''s key installed.'
Say "Verify with: ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -i <key> root@$RouterIp"
exit 0

} catch {
    Die "Unexpected error: $($_.Exception.Message)"
}
