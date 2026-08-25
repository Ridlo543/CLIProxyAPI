# Installs AinyRouter for the current Windows user:
#   - copies ainyrouter.exe into %LOCALAPPDATA%\Programs\AinyRouter\bin
#   - creates %LOCALAPPDATA%\AinyRouter\config.yaml (+ panel) on first run
#   - adds the bin folder to the USER PATH so `ainyrouter` works everywhere
#
# Usage:
#   pwsh scripts\install-user.ps1                    # build fresh, then install
#   pwsh scripts\install-user.ps1 -UseExisting       # reuse an already-built exe
#   pwsh scripts\install-user.ps1 -ExePath C:\path\ainyrouter.exe

param(
    [string]$ExePath = "",
    [switch]$UseExisting,
    [switch]$NoPath
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$installRoot = Join-Path $env:LOCALAPPDATA "Programs\AinyRouter"
$binDir = Join-Path $installRoot "bin"
$appDir = Join-Path $env:LOCALAPPDATA "AinyRouter"

Write-Host "== 1/4 binary =="
if (-not $UseExisting -and -not $ExePath) {
    pwsh -NoProfile -File (Join-Path $PSScriptRoot "build-local.ps1") -OutputDir (Join-Path $repoRoot "build")
    if ($LASTEXITCODE -ne 0) { throw "build failed" }
    $ExePath = Join-Path $repoRoot "build\ainyrouter.exe"
}
if (-not $ExePath) { throw "no binary: pass -ExePath or drop -UseExisting" }
if (-not (Test-Path $ExePath)) { throw "exe not found: $ExePath" }

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
Copy-Item $ExePath (Join-Path $binDir "ainyrouter.exe") -Force
Write-Host "binary -> $binDir"

Write-Host "== 2/4 config =="
$appDataCfg = Join-Path $appDir "config.yaml"
if (-not (Test-Path $appDataCfg)) {
    New-Item -ItemType Directory -Force -Path $appDir | Out-Null
    @"
host: `"127.0.0.1`"
port: 18400
auth-dir: `"$($appDir -replace '\\','/')`/auths`"
api-keys:
  - `"sk-ainy-local`"
remote-management:
  secret-key: `"ainy-local`"
  disable-auto-update-panel: true
"@ | Out-File -FilePath $appDataCfg -Encoding utf8
    Write-Host "config created -> $appDataCfg"
} else {
    Write-Host "config kept   -> $appDataCfg"
}

Write-Host "== 3/4 panel =="
& (Join-Path $binDir "ainyrouter.exe") -panel-install --config $appDataCfg
if ($LASTEXITCODE -ne 0) {
    Write-Warning "panel not embedded in this binary; web UI will fall back to upstream asset unless disabled"
}

Write-Host "== 4/4 PATH (user) =="
if (-not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ";") -notcontains $binDir) {
        [Environment]::SetEnvironmentVariable(
            "Path",
            ($(if ($userPath) { $userPath.TrimEnd(";") + ";" } else { "" }) + $binDir),
            "User")
        Write-Host "added to USER PATH: $binDir"
        Write-Host "open a NEW terminal window for 'ainyrouter' to resolve."
    } else {
        Write-Host "already on USER PATH"
    }
}

Write-Host ""
Write-Host "Done. Open a NEW terminal and run:  ainyrouter"
Write-Host "Dashboard:                          http://localhost:18400/"
