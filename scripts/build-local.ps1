# Builds ainyrouter.exe with the management panel embedded.
#
# Usage:  pwsh scripts/build-local.ps1 [-OutputDir <dir>]
#
# Steps:
#   1. Build the single-file panel (apps/control-panel -> dist-management)
#   2. Inject it into internal/panelasset/panel/management.html with the
#      embedded marker so panelasset.Available() reports true
#   3. go build ./cmd/server -> ainyrouter.exe

param(
    [string]$OutputDir = "."
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot

Write-Host "== 1/3 building management panel =="
Push-Location (Join-Path $repoRoot "..\..\apps\control-panel")
try {
    if (-not (Test-Path node_modules)) { npm ci }
    npm run build:management
    if ($LASTEXITCODE -ne 0) { throw "npm run build:management failed" }
} finally {
    Pop-Location
}

Write-Host "== 2/3 injecting panel into go embed =="
$marker = "<!-- ainyrouter-panel-embedded -->"
$panelDir = Join-Path $repoRoot "..\..\apps\control-panel"
$htmlPath = Join-Path $panelDir "dist-management\index.html"
if (-not (Test-Path $htmlPath)) { throw "panel output missing: $htmlPath" }
$targetDir = Join-Path $PSScriptRoot "..\internal\panelasset\panel"
New-Item -ItemType Directory -Force -Path $targetDir | Out-Null
"$marker`n" + (Get-Content $htmlPath -Raw -Encoding utf8) |
    Out-File -FilePath (Join-Path $targetDir "management.html") -Encoding utf8 -NoNewline

Write-Host "== 3/3 building ainyrouter.exe =="
Push-Location $repoRoot
try {
    go build -ldflags "-s -w" -o (Join-Path $OutputDir "ainyrouter.exe") ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally {
    Pop-Location
}

$outExe = Join-Path $OutputDir "ainyrouter.exe"
Write-Host "done: $outExe ($([math]::Round((Get-Item $outExe).Length/1MB,1)) MB)"
