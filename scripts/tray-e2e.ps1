# Assisted E2E for the Windows tray experience.
#
# Launches the installed ainyrouter interactively (REAL console window + tray
# icon on your desktop), prints click-by-click instructions, and measures the
# objective signals that can be observed programmatically:
#   - port 18400 comes up after start
#   - port 18400 goes DOWN after you quit via the tray Exit item
#
# Usage:  pwsh scripts\tray-e2e.ps1
# Result: PASS/FAIL summary. The clicks themselves are yours.

$ErrorActionPreference = "Stop"
$bin = Join-Path $env:LOCALAPPDATA "Programs\AinyRouter\bin\ainyrouter.exe"
if (-not (Test-Path $bin)) { throw "installed binary not found: $bin" }
$cfg = Join-Path $env:LOCALAPPDATA "AinyRouter\config.yaml"

Get-Process ainyrouter -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep 2

Write-Host "== Tray assisted E2E ==" -ForegroundColor Cyan
Write-Host "Launching interactive console..."
$p = Start-Process -FilePath $bin -ArgumentList "-menu", "--config", $cfg -PassThru

function Test-Port {
    Get-NetTCPConnection -LocalPort 18400 -State Listen -ErrorAction SilentlyContinue |
        Select-Object -First 1
}

# server must come up
$up = $false
foreach ($i in 1..20) {
    Start-Sleep 1
    if (Test-Port) { $up = $true; break }
}
if (-not $up) {
    Write-Host "FAIL: server never listened on 18400" -ForegroundColor Red
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    exit 1
}
Write-Host "PASS: server is listening (pid $($p.Id))" -ForegroundColor Green

Write-Host ""
Write-Host "NOW, IN THE AINYROUTER CONSOLE WINDOW:" -ForegroundColor Yellow
Write-Host "  1. Press  2  then Enter   -> console hides to tray"
Write-Host "  2. Click the tray icon    -> Open Web UI / Show Console"
Write-Host "  3. Choose Show Console    -> window returns"
Write-Host "  4. Open tray again, Exit  -> app quits"
Write-Host ""
Write-Host "This script waits up to 120s for the process to exit via tray."

$exited = $p.WaitForExit(120000)
if (-not $exited) {
    Write-Host "TIMEOUT: still running after 120s." -ForegroundColor Red
    Write-Host "If everything else worked, stop it now:  taskkill /IM ainyrouter.exe /F"
    exit 1
}

Start-Sleep 2
if (Test-Port) {
    Write-Host "FAIL: process exited but port 18400 still listening" -ForegroundColor Red
    exit 1
}

Write-Host ""
Write-Host "PASS: hide-to-tray cycle completed and tray Exit stopped the server." -ForegroundColor Green
Write-Host "(Confirm visually: Web UI opened from tray showed the dashboard.)"
