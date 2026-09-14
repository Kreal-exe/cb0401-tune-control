#
# Builds (if needed) and starts the router admin GUI, then opens it in the
# browser (Windows). No Python, no venv, no pip - just a Go binary.
#
# Usage: powershell -ExecutionPolicy Bypass -File start_gui.ps1
#
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$Bin = Join-Path $PSScriptRoot 'cb0401-tune-control.exe'
$Port = 5757
$PidFile = Join-Path $env:TEMP 'cb0401_tune_control_gui.pid'
$LogFile = Join-Path $env:TEMP 'cb0401_tune_control_gui.log'

if (-not (Test-Path $Bin)) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        Write-Host "ERROR: no compiled GUI found at $Bin, and 'go' isn't installed to build one."
        Write-Host "Either install Go (https://go.dev/dl/) and re-run this script, or copy a"
        Write-Host "prebuilt binary from dist/ (see build.sh) to $Bin."
        exit 1
    }
    Write-Host 'Building the GUI (one-time)...'
    & go build -o $Bin .
    if ($LASTEXITCODE -ne 0) { Write-Host 'ERROR: go build failed.'; exit 1 }
}

# Stop a stale instance from a previous run, if any, so we don't end up
# talking to old code or hit a port conflict.
if (Test-Path $PidFile) {
    $oldPid = Get-Content $PidFile -ErrorAction SilentlyContinue
    if ($oldPid) {
        $proc = Get-Process -Id $oldPid -ErrorAction SilentlyContinue
        if ($proc) {
            Write-Host "Stopping previous instance (PID $oldPid)..."
            Stop-Process -Id $oldPid -Force -ErrorAction SilentlyContinue
            Start-Sleep -Seconds 1
        }
    }
}
$existing = Get-NetTCPConnection -LocalPort $Port -ErrorAction SilentlyContinue | Select-Object -First 1
if ($existing) {
    Write-Host "Port $Port is in use (PID $($existing.OwningProcess)) - freeing it..."
    Stop-Process -Id $existing.OwningProcess -Force -ErrorAction SilentlyContinue
}

Write-Host "Starting the GUI on http://127.0.0.1:$Port ..."
$proc = Start-Process -FilePath $Bin -WorkingDirectory $PSScriptRoot -WindowStyle Hidden -PassThru `
    -RedirectStandardOutput $LogFile -RedirectStandardError "$LogFile.err"
$proc.Id | Out-File -FilePath $PidFile -Encoding ascii

for ($i = 0; $i -lt 20; $i++) {
    try {
        $resp = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/" -UseBasicParsing -TimeoutSec 1
        if ($resp.StatusCode -eq 200) { break }
    } catch {}
    Start-Sleep -Milliseconds 300
}

Start-Process "http://127.0.0.1:$Port"

# Stay attached to the GUI process instead of returning to the prompt right
# away - closing this window or hitting Ctrl+C stops the GUI too, rather
# than leaving it running invisibly in the background.
Write-Host "GUI is running (PID $($proc.Id)), logs: $LogFile / $LogFile.err"
Write-Host "Press Ctrl+C to stop it."
try {
    Wait-Process -Id $proc.Id
} finally {
    Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
}
