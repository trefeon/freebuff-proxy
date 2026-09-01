<#
.SYNOPSIS
    Fetches CodebuffAI/freebuff upstream changes, syncs pinned registry files
    (backend/internal/registry/testdata/upstream/), verifies hash parity, and runs tests.

.DESCRIPTION
    Automates synchronizing the freebuff-proxy repository with upstream FreeBuff CLI
    model registry definitions and vendor changes.

.PARAMETER Ref
    Upstream branch or commit SHA (default: "main").

.PARAMETER CloneDir
    Local clone directory for CodebuffAI/freebuff reference repo.

.PARAMETER CheckOnly
    Check drift only; do not write any files.

.PARAMETER NoTest
    Skip running tests after sync.

.PARAMETER TestAll
    Run the entire test suite (go test ./backend/...) instead of only registry tests.

.EXAMPLE
    .\scripts\sync-upstream.ps1
    .\scripts\sync-upstream.ps1 -CheckOnly
    .\scripts\sync-upstream.ps1 -TestAll
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Ref = "main",

    [Parameter(Position = 1)]
    [string]$CloneDir = "",

    [switch]$CheckOnly,
    [switch]$NoTest,
    [switch]$TestAll
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
$VendorUrl = "https://github.com/CodebuffAI/freebuff.git"
$UpstreamPrefix = "common/src/constants"
$PinnedDir = Join-Path $RepoRoot "backend\internal\registry\testdata\upstream"

if (-not $CloneDir) {
    if ($env:FREEBUFF_REFERENCE_DIR) {
        $CloneDir = $env:FREEBUFF_REFERENCE_DIR
    }
    elseif (Test-Path (Join-Path $RepoRoot "reference\freebuff\.git")) {
        $CloneDir = Join-Path $RepoRoot "reference\freebuff"
    }
    else {
        $CloneDir = Join-Path $RepoRoot "..\freebuff-reference"
    }
}

$Files = @(
    "free-agents.ts",
    "freebuff-model-ids.ts",
    "freebuff-models.ts",
    "gemini.ts",
    "model-config.ts"
)

function Get-NormalizedSha256([string]$Content) {
    $cleanContent = $Content -replace "`r", ""
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($cleanContent)
    $hasher = [System.Security.Cryptography.SHA256]::Create()
    $hashBytes = $hasher.ComputeHash($bytes)
    return -join ($hashBytes | ForEach-Object { "{0:x2}" -f $_ })
}

function Get-FileNormalizedSha256([string]$Path) {
    if (-not (Test-Path $Path)) { return "-" }
    $raw = [System.IO.File]::ReadAllText($Path, [System.Text.Encoding]::UTF8)
    return Get-NormalizedSha256 $raw
}

Write-Host "==> 1. Checking upstream repository ($VendorUrl)" -ForegroundColor Cyan
if (-not (Test-Path (Join-Path $CloneDir ".git"))) {
    Write-Host "    Cloning into $CloneDir (--depth 50)..."
    git clone --depth 50 -- $VendorUrl $CloneDir
    if ($LASTEXITCODE -ne 0) { throw "git clone failed" }
}
else {
    Write-Host "    Fetching '$Ref' in $CloneDir..."
    $isFullSha = $Ref -match '^[0-9a-fA-F]{40}$'
    $hasCommit = $false
    if ($isFullSha) {
        git -C $CloneDir cat-file -e "$($Ref)^{commit}" 2>$null
        if ($LASTEXITCODE -eq 0) { $hasCommit = $true }
    }
    if (-not $hasCommit) {
        git -C $CloneDir fetch origin -- $Ref
        if ($LASTEXITCODE -ne 0) { throw "git fetch failed" }
    }
}

$UpstreamSha = $Ref
if (-not ($Ref -match '^[0-9a-fA-F]{40}$')) {
    $resolved = git -C $CloneDir rev-parse --verify "origin/$($Ref)^{commit}" 2>$null
    if ($LASTEXITCODE -ne 0 -or -not $resolved) {
        $resolved = git -C $CloneDir rev-parse --verify "$($Ref)^{commit}" 2>$null
    }
    if ($LASTEXITCODE -ne 0 -or -not $resolved) {
        throw "Cannot resolve ref '$Ref' in $CloneDir"
    }
    $UpstreamSha = ($resolved -split "`n")[0].Trim()
}

# Sync working tree if checkout is present
$isWorkTree = git -C $CloneDir rev-parse --is-inside-work-tree 2>$null
if ($LASTEXITCODE -eq 0) {
    $currentHead = (git -C $CloneDir rev-parse HEAD 2>$null)
    if ($currentHead -ne $UpstreamSha) {
        git -C $CloneDir checkout -q $UpstreamSha 2>$null
        if ($LASTEXITCODE -ne 0) {
            git -C $CloneDir reset -q --hard $UpstreamSha 2>$null
        }
    }
}

$commitSummary = (git -C $CloneDir log -1 --format="%h - %s (%an, %cr)" $UpstreamSha 2>$null)
Write-Host "    Target upstream commit: $commitSummary"
Write-Host ""

Write-Host "==> 2. Comparing pinned snapshot against upstream" -ForegroundColor Cyan
"{0,-26} {1,-14} {2,-14} {3}" -f "FILE", "PINNED-SHA", "VENDOR-SHA", "STATUS"
"{0,-26} {1,-14} {2,-14} {3}" -f "-------------------------", "-------------", "-------------", "------"

if (-not (Test-Path $PinnedDir)) {
    New-Item -ItemType Directory -Path $PinnedDir -Force | Out-Null
}

$driftCount = 0
$updatedCount = 0
$tempFile = [System.IO.Path]::GetTempFileName()

try {
    foreach ($f in $Files) {
        $pinnedFile = Join-Path $PinnedDir $f
        $pinnedSha = Get-FileNormalizedSha256 $pinnedFile

        git -C $CloneDir cat-file -e "${UpstreamSha}:${UpstreamPrefix}/$f" 2>$null
        if ($LASTEXITCODE -ne 0) {
            $pShort = if ($pinnedSha -ne "-") { $pinnedSha.Substring(0, 12) } else { "-" }
            "{0,-26} {1,-14} {2,-14} {3}" -f $f, $pShort, "-", "MISSING"
            $driftCount++
            continue
        }

        # Extract vendor content safely
        cmd.exe /c "git -C `"$CloneDir`" show `"${UpstreamSha}:${UpstreamPrefix}/$f`" > `"$tempFile`""
        $vendorRaw = [System.IO.File]::ReadAllText($tempFile, [System.Text.Encoding]::UTF8)
        $vendorSha = Get-NormalizedSha256 $vendorRaw

        $pShort = if ($pinnedSha -ne "-") { $pinnedSha.Substring(0, 12) } else { "-" }
        $vShort = $vendorSha.Substring(0, 12)

        if ($pinnedSha -eq $vendorSha) {
            "{0,-26} {1,-14} {2,-14} {3}" -f $f, $pShort, $vShort, "SAME"
        }
        else {
            "{0,-26} {1,-14} {2,-14} {3}" -f $f, $pShort, $vShort, "DRIFT"
            $driftCount++

            if (-not $CheckOnly) {
                # Strip CR and write with LF encoding
                $cleanLf = $vendorRaw -replace "`r", ""
                [System.IO.File]::WriteAllBytes($pinnedFile, [System.Text.Encoding]::UTF8.GetBytes($cleanLf))
                $updatedCount++
            }
        }
    }
}
finally {
    if (Test-Path $tempFile) {
        Remove-Item -Force $tempFile -ErrorAction SilentlyContinue
    }
}

# 2b. Vendor npm wrapper version (freebuff CLI package). Best-effort: npm
#     not on PATH is fine; in sync mode the pin auto-updates.
$vendorVersionFile = Join-Path $RepoRoot "scripts\vendor-version.txt"
$npmVersion = ""
if (Get-Command npm -ErrorAction SilentlyContinue) {
    $npmVersion = (npm view freebuff version 2>$null)
    if (-not $npmVersion) { $npmVersion = "" }
}
$pinnedVersion = ""
if (Test-Path $vendorVersionFile) {
    $pinnedVersion = ([System.IO.File]::ReadAllText($vendorVersionFile)).Trim()
}
if ($npmVersion) {
    if ($pinnedVersion -and $npmVersion -ne $pinnedVersion) {
        Write-Host "    npm freebuff@$npmVersion (pinned $pinnedVersion) - VERSION DRIFT"
        $driftCount++
        if (-not $CheckOnly) {
            [System.IO.File]::WriteAllText($vendorVersionFile, $npmVersion + "`n", [System.Text.Encoding]::UTF8)
            Write-Host "    Updated scripts/vendor-version.txt to $npmVersion"
        }
    }
    else {
        Write-Host "    npm freebuff@$npmVersion matches pin $pinnedVersion"
    }
}
else {
    Write-Host "    npm not on PATH - skipping vendor npm version check"
}

Write-Host ""

if ($CheckOnly) {
    if ($driftCount -gt 0) {
        Write-Warning "DRIFT detected in $driftCount file(s). Run without -CheckOnly to synchronize."
        exit 1
    }
    else {
        Write-Host "All pinned files match upstream perfectly." -ForegroundColor Green
    }
}
else {
    if ($updatedCount -gt 0) {
        Write-Host "==> 3. Updated $updatedCount pinned file(s) in backend\internal\registry\testdata\upstream\" -ForegroundColor Green
    }
    else {
        Write-Host "==> 3. All pinned files are already up-to-date (0 files updated)."
    }
}

Write-Host ""
Write-Host "==> 4. Verifying pin parity..." -ForegroundColor Cyan

# Run check-upstream.sh via Git Bash if available. After a sync, verify only
# the registry group: the wire files are deliberately not touched by this
# sync, so wire drift arriving in the same upstream batch (tracked by the
# drift workflow as a separate needs-port issue) must not fail the registry
# sync. Check-only mode keeps the full check so drift reports still fail on
# wire drift.
$gitBash = "C:\Program Files\Git\bin\bash.exe"
if (Test-Path $gitBash) {
    $checkArgs = @()
    if (-not $CheckOnly) { $checkArgs = @("--group", "registry") }
    & $gitBash "$RepoRoot/scripts/check-upstream.sh" @checkArgs $UpstreamSha $CloneDir
    if ($LASTEXITCODE -ne 0) {
        throw "check-upstream.sh reported failure after sync"
    }
}
else {
    Write-Host "    (Git Bash not detected; re-verifying pinned files via native SHA-256)..."
    $reVerifyFailures = 0
    foreach ($f in $Files) {
        $pinnedFile = Join-Path $PinnedDir $f
        $pinnedSha = Get-FileNormalizedSha256 $pinnedFile
        git -C $CloneDir cat-file -e "${UpstreamSha}:${UpstreamPrefix}/$f" 2>$null
        if ($LASTEXITCODE -ne 0) {
            if ($pinnedSha -ne "-") {
                Write-Host "    $f : still present locally but MISSING upstream" -ForegroundColor Red
                $reVerifyFailures++
            }
            continue
        }
        cmd.exe /c "git -C `"$CloneDir`" show `"${UpstreamSha}:${UpstreamPrefix}/$f`" > `"$tempFile`""
        $vendorRaw = [System.IO.File]::ReadAllText($tempFile, [System.Text.Encoding]::UTF8)
        $vendorSha = Get-NormalizedSha256 $vendorRaw
        if ($pinnedSha -ne $vendorSha) {
            Write-Host "    $f : pin $($pinnedSha.Substring(0,12)) != vendor $($vendorSha.Substring(0,12))" -ForegroundColor Red
            $reVerifyFailures++
        }
    }
    if ($reVerifyFailures -gt 0) {
        throw "post-sync verification failed for $reVerifyFailures file(s)"
    }
    Write-Host "    Post-sync verification passed for all pinned files." -ForegroundColor Green
}

if (-not $NoTest) {
    Write-Host ""
    Write-Host "==> 5. Running test suite..." -ForegroundColor Cyan
    $go = Get-Command "go" -ErrorAction SilentlyContinue
    if (-not $go) {
        Write-Warning "'go' command not found on PATH. Skipping test execution."
    }
    else {
        # Clear env variables during test
        $oldAuth = $env:AUTH_TOKENS
        $oldAdmin = $env:ADMIN_TOKEN
        try {
            $env:AUTH_TOKENS = ""
            $env:ADMIN_TOKEN = ""

            if ($TestAll) {
                Write-Host "    Executing: go test ./backend/..."
                go test ./backend/...
                if ($LASTEXITCODE -ne 0) {
                    throw "Full test suite failed."
                }
                Write-Host "    [PASS] Full test suite passed cleanly." -ForegroundColor Green
            }
            else {
                Write-Host "    Executing: go test ./backend/internal/registry/..."
                go test ./backend/internal/registry/...
                if ($LASTEXITCODE -ne 0) {
                    Write-Error "Registry tests failed. If upstream added/removed models or agents, update fallbackAgents or fallbackRootByModel in backend/internal/registry/registry.go."
                    throw "Registry test failure"
                }
                Write-Host "    [PASS] Registry tests and fallback parity check passed." -ForegroundColor Green
            }
        }
        finally {
            $env:AUTH_TOKENS = $oldAuth
            $env:ADMIN_TOKEN = $oldAdmin
        }
    }
}

Write-Host ""
Write-Host "==> Upstream Sync Complete!" -ForegroundColor Green

$diffOut = git -C $RepoRoot diff --name-only "backend/internal/registry/testdata/upstream"
if (-not $diffOut) {
    Write-Host "No working tree changes (pins were already identical to upstream)."
}
else {
    Write-Host "Working tree changes in backend\internal\registry\testdata\upstream\:" -ForegroundColor Yellow
    git -C $RepoRoot status --short "backend/internal/registry/testdata/upstream"
    Write-Host ""
    Write-Host "Suggested commit command:"
    Write-Host "  git add backend/internal/registry/testdata/upstream"
    Write-Host "  git commit -m `"chore(registry): sync pinned upstream models to vendor $($UpstreamSha.Substring(0, 7))`""
}
