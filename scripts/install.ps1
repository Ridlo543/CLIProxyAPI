# AinyRouter one-line installer for Windows (PowerShell 7+).
#
#   irm https://raw.githubusercontent.com/Ridlo543/CLIProxyAPI/<ref>/install.ps1 | iex
#
# Downloads the latest ainyrouter-windows-amd64.zip release asset, installs it
# for the current user, adds it to PATH, generates a local config with a fresh
# management key, and installs the embedded panel.

$ErrorActionPreference = "Stop"
$Repo = "Ridlo543/CLIProxyAPI"

Write-Host "== AinyRouter installer ==" -ForegroundColor Cyan

# 1. locate latest release asset
$headers = @{ "User-Agent" = "ainyrouter-installer" }
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers $headers
$asset = $release.assets | Where-Object { $_.name -eq "ainyrouter-windows-amd64.zip" } | Select-Object -First 1
if (-not $asset) { throw "release asset not found in $($release.tag_name)" }
Write-Host "latest release: $($release.tag_name)"

# 2. download + extract
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ainyrouter-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$zip = Join-Path $tmp $asset.name
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip -Headers $headers
Expand-Archive -Path $zip -DestinationPath $tmp\unpacked -Force

# 3. install layout
$binDir = Join-Path $env:LOCALAPPDATA "Programs\AinyRouter\bin"
$appDir = Join-Path $env:LOCALAPPDATA "AinyRouter"
New-Item -ItemType Directory -Force -Path $binDir, $appDir, (Join-Path $appDir "auths"), (Join-Path $appDir "static") | Out-Null
Copy-Item (Join-Path $tmp\unpacked "ainyrouter.exe") $binDir -Force

# 4. config on first run (fresh random management key)
$cfg = Join-Path $appDir "config.yaml"
if (-not (Test-Path $cfg)) {
    $keyBytes = New-Object byte[] 24
    [Security.Cryptography.RandomNumberGenerator]::Fill($keyBytes)
    $key = [Convert]::ToBase64String($keyBytes).Replace("+", "").Replace("/", "").Substring(0, 32)
    @"
host: `"127.0.0.1`"
port: 18400
auth-dir: `"$($appDir -replace '\\','/')`/auths`"
api-keys:
  - `"sk-ainy-local`"
remote-management:
  secret-key: `"$key`"
  disable-auto-update-panel: true
"@ | Out-File -FilePath $cfg -Encoding utf8
    Write-Host ""
    Write-Host "  your management key (also needed by the panel):" -ForegroundColor Yellow
    Write-Host "  $key" -ForegroundColor Yellow
    Write-Host "  (stored in $cfg)" -ForegroundColor DarkGray
}

# 5. panel files from the same install (embedded -> -panel-install)
& (Join-Path $binDir "ainyrouter.exe") -panel-install --config $cfg | Out-Null

# 6. USER PATH
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ";") -notcontains $binDir) {
    [Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(";") + ";" + $binDir), "User")
    Write-Host "added to USER PATH (open a NEW terminal for 'ainyrouter')"
}

Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "Done. Open a NEW terminal and run:  ainyrouter" -ForegroundColor Green
Write-Host "Dashboard:                          http://localhost:18400/"
