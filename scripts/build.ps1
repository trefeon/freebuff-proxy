<#
.SYNOPSIS
    Builds the Svelte 5 frontend and compiles the freebuff-proxy binary.
.DESCRIPTION
    1. Installs/updates frontend dependencies if needed.
    2. Compiles the frontend SPA bundle to backend/internal/dashboard/dist.
    3. Compiles the Go binary to bin/freebuff-proxy.exe.
#>

[CmdletBinding()]
param (
    [switch]$SkipUI = $false,
    [string]$OutputPath = "bin/freebuff-proxy.exe"
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot

Write-Host "==> Starting build from $RepoRoot" -ForegroundColor Cyan

if (-not $SkipUI) {
    Write-Host "==> Building frontend assets..." -ForegroundColor Yellow
    Push-Location (Join-Path $RepoRoot "frontend")
    try {
        if (-not (Test-Path "node_modules")) {
            Write-Host "    Installing frontend npm dependencies..." -ForegroundColor Gray
            npm install
        }
        npm run build
    } finally {
        Pop-Location
    }
}

$OutDir = Split-Path -Parent (Join-Path $RepoRoot $OutputPath)
if (-not (Test-Path $OutDir)) {
    New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
}

Write-Host "==> Compiling Go binary to $OutputPath..." -ForegroundColor Yellow
Push-Location $RepoRoot
try {
    go build -o $OutputPath ./backend/cmd/freebuff-proxy
    Write-Host "==> Build successful: $OutputPath" -ForegroundColor Green
} finally {
    Pop-Location
}
