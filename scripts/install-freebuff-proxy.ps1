# install-freebuff-proxy.ps1 - download the latest freebuff-proxy release for
# this machine, verify it, set up .env, and print the next steps.
#
# Zero-knowledge user flow:
#   1. Open PowerShell (Windows Terminal / pwsh or powershell.exe)
#   2. Run:
#        irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
#   3. Read what it prints. It reuses existing FreeBuff CLI credentials when
#      found, otherwise opens a headless-browser OAuth login (zero extra
#      dependencies), downloads the binary, creates .env from the example,
#      and either stores the token or leaves AUTH_TOKENS empty for bridge mode.
#
# What it does NOT do: modify system paths, install services, or touch your
# token except writing it into the local .env (gitignored).

param(
  [string]$Dir = "",          # install directory; default: current directory
  [switch]$SkipToken,         # do not look for a token (set AUTH_TOKENS later)
  [switch]$NoEnv,             # do not create .env (advanced)
  [switch]$Force,             # re-download even if the binary already exists
  [switch]$Logout,            # clear existing CLI credentials to log in with a new account
  [string]$EnvFile = ""       # explicit .env target (advanced)
)
$ErrorActionPreference = "Stop"
$Repo = "trefeon/freebuff-proxy"

function Find-CredentialsFile {
  $candidates = @(
    (Join-Path $env:USERPROFILE ".config\manicode\credentials.json"),
    (Join-Path $env:USERPROFILE ".config\codebuff\credentials.json"),
    (Join-Path $HOME ".config/manicode\credentials.json")
  )
  foreach ($p in $candidates) { if (Test-Path $p) { return $p } }
  return $null
}

if ($Logout) {
  $creds = Find-CredentialsFile
  if ($creds -and (Test-Path $creds)) {
    Remove-Item -LiteralPath $creds -Force -ErrorAction SilentlyContinue
    Write-Host "Cleared existing login credentials ($creds)." -ForegroundColor Yellow
  }
}

function Get-AuthToken([string]$path) {
  try {
    $data = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
  } catch { return $null }
  $acct = $null
  if ($null -ne $data.default) { $acct = $data.default }
  if ($null -eq $acct -and $null -ne $data) {
    $acct = $data.PSObject.Properties | ForEach-Object { $_.Value } |
      Where-Object { $_ -and $_.authToken } | Select-Object -First 1
  }
  if ($acct -and $acct.authToken) { return [string]$acct.authToken }
  return $null
}

Write-Host ""
Write-Host "Installing freebuff-proxy (latest release)..." -ForegroundColor Cyan
Write-Host ""

# --- 0. warning -------------------------------------------------------------
Write-Host "WARNING: using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff" -ForegroundColor Red
Write-Host "terms of service. Accounts get suspended or banned (403 account_banned, dashboard shows" -ForegroundColor Red
Write-Host "'suspended'). Bans are per account, usually permanent, and there is no self-service" -ForegroundColor Red
Write-Host "unban. Use ONE account, keep usage modest, do not run the proxy 24/7, and expect the" -ForegroundColor Red
Write-Host "account to be banned eventually. You accept this risk by continuing." -ForegroundColor Red
Write-Host ""

# --- 1. target directory -----------------------------------------------------
if (-not $Dir) { $Dir = (Get-Location).Path }
New-Item -ItemType Directory -Force -Path $Dir | Out-Null

# --- 2. TOKEN PREREQUISITE (before downloading the proxy) --------------------
# Reuse existing CLI credentials when present; otherwise offer headless-browser
# OAuth login, paste an existing token, or bridge mode (empty AUTH_TOKENS).
# This fails early instead of installing a proxy that can only return
# "Invalid API key" later.
$token = $null
$creds = Find-CredentialsFile
if ($creds) { $token = Get-AuthToken $creds }

function Get-HeadlessToken {
  Write-Host "Requesting login URL for browser authentication..." -ForegroundColor Cyan
  $bytes = New-Object byte[] 32
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  # CLI-parity base64url shape (43 chars) — same charset as the official
  # CLI fingerprint; no substring truncation needed.
  $hash = ([Convert]::ToBase64String($bytes) -replace '\+', '-' -replace '/', '_' -replace '=', '')
  $fingerprintId = "enhanced-$hash"
  $authHeaders = @{ "User-Agent" = "ai-sdk/openai-compatible/1.0.0/codebuff" }

  try {
    $codeBody = @{ fingerprintId = $fingerprintId } | ConvertTo-Json
    $codeResp = Invoke-RestMethod -Uri "https://www.codebuff.com/api/auth/cli/code" `
      -Method POST -Headers $authHeaders -ContentType "application/json" -Body $codeBody
  } catch {
    Write-Host "Failed to get login URL: $_" -ForegroundColor Red
    return $null
  }

  $loginUrl = $codeResp.loginUrl
  $fingerprintHash = $codeResp.fingerprintHash
  $expiresAt = $codeResp.expiresAt
  if (-not $loginUrl) {
    Write-Host "No login URL in response. Server may be down." -ForegroundColor Red
    return $null
  }

  Write-Host ""
  Write-Host "Opening browser for FreeBuff GitHub login..." -ForegroundColor Green
  Write-Host "URL: $loginUrl" -ForegroundColor DarkGray
  Write-Host "  -> Log in with the GitHub account you want a token for." -ForegroundColor Yellow
  Write-Host ""
  Start-Process $loginUrl

  Write-Host "Waiting for authentication in browser (timeout: 300s)..." -ForegroundColor Cyan
  $start = Get-Date
  while (((Get-Date) - $start).TotalSeconds -lt 300) {
    Start-Sleep -Seconds 5
    try {
      $query = "fingerprintId=$([Uri]::EscapeDataString($fingerprintId))&fingerprintHash=$([Uri]::EscapeDataString($fingerprintHash))&expiresAt=$([Uri]::EscapeDataString($expiresAt))"
      $statusUri = "https://www.codebuff.com/api/auth/cli/status?$query"
      $statusResp = Invoke-RestMethod -Uri $statusUri -Method GET -Headers $authHeaders
      if ($statusResp.user -and $statusResp.user.authToken) {
        Write-Host "Authentication successful! Token acquired." -ForegroundColor Green
        return [string]$statusResp.user.authToken
      }
    } catch {}
  }
  Write-Host "Login timed out after 300s." -ForegroundColor Red
  return $null
}

if (-not $SkipToken -and (-not $token -or $token.Length -le 12)) {
  Write-Host "Step 1/3: FreeBuff auth token" -ForegroundColor Cyan
  Write-Host "  1) Generate token now via browser login (recommended, zero extra dependencies)" -ForegroundColor Green
  Write-Host "  2) Paste an existing FreeBuff authToken" -ForegroundColor Gray
  Write-Host "  3) Bridge mode (skip token; clients supply their own per request)" -ForegroundColor Gray
  $ans = Read-Host "Choose [1]"
  switch ($ans.Trim().ToLower()) {
    "2" {
      $pasted = Read-Host "Paste your authToken"
      if ($pasted -and $pasted.Trim().Length -gt 8) {
        $token = $pasted.Trim()
      }
    }
    "3" {
      Write-Host "Bridge mode: proxy will run with empty AUTH_TOKENS." -ForegroundColor Yellow
      $token = ""
      $SkipToken = $true
    }
    default {
      $token = Get-HeadlessToken
      if (-not $token) {
        $pasted = Read-Host "Paste your authToken manually (or Enter to skip)"
        if ($pasted -and $pasted.Trim().Length -gt 8) {
          $token = $pasted.Trim()
        }
      }
    }
  }
}
if ($token -and $token.Length -gt 12) {
  Write-Host "FreeBuff login ready (token value hidden)." -ForegroundColor Green
}

# --- 3. proxy installer dependencies ----------------------------------------
Write-Host "Step 2/3: installing freebuff-proxy" -ForegroundColor Cyan
# --- 3. resolve the latest release ------------------------------------------
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "freebuff-proxy-installer" }
$version = $release.tag_name -replace '^v', ''  # assets use 0.1.1, tag is v0.1.1
Write-Host "Latest release: v$version" -ForegroundColor Green

$arch = $env:PROCESSOR_ARCHITECTURE
if ($arch -match "ARM64") { $goarch = "arm64" } elseif ($arch -match "AMD64|x86_64") { $goarch = "amd64" } else { $goarch = "amd64" }
$want = "freebuff-proxy_${version}_windows_${goarch}.zip"
Write-Host "Asset: $want" -ForegroundColor Green

# --- 4. already installed? ---------------------------------------------------
$exe = Join-Path $Dir "freebuff-proxy.exe"
if (Test-Path -LiteralPath $exe) {
  if (-not $Force) {
    Write-Host "freebuff-proxy already exists: $exe" -ForegroundColor Yellow
    Write-Host "Skipping the download (re-run with -Force to update)." -ForegroundColor Yellow
  } else {
    Write-Host "Re-downloading (forced)..." -ForegroundColor Cyan
  }
}
if (-not (Test-Path -LiteralPath $exe) -or $Force) {
  $asset = $release.assets | Where-Object { $_.name -eq $want } | Select-Object -First 1
  $checksumAsset = $release.assets | Where-Object { $_.name -eq "checksums.txt" } | Select-Object -First 1
  if (-not $asset) { Write-Host "ERROR: asset $want not found in the release." -ForegroundColor Red; exit 1 }

  # --- 5. download + verify ----------------------------------------------------
  $tmp = Join-Path $env:TEMP "freebuff-proxy-install"
  New-Item -ItemType Directory -Force -Path $tmp | Out-Null
  $zip = Join-Path $tmp $want
  $sums = Join-Path $tmp "checksums.txt"

  Write-Host "Downloading..." -ForegroundColor Cyan
  Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip
  Invoke-WebRequest -Uri $checksumAsset.browser_download_url -OutFile $sums

  # Get-FileHash is PS6+; compute SHA256 via .NET so Windows PowerShell 5.1 works too.
  $sha = [System.Security.Cryptography.SHA256]::Create()
  try {
    $stream = [System.IO.File]::OpenRead($zip)
    try {
      $hash = ([System.BitConverter]::ToString($sha.ComputeHash($stream))).Replace("-", "").ToLower()
    } finally {
      $stream.Dispose()
    }
  } finally {
    $sha.Dispose()
  }
  $expected = (Get-Content -LiteralPath $sums | Where-Object { $_ -like "*$want*" } | Select-Object -First 1).Split(" ")[0]
  if ($hash -ne $expected) {
    Write-Host "ERROR: checksum mismatch for $want" -ForegroundColor Red
    Write-Host "  expected: $expected" -ForegroundColor Red
    Write-Host "  actual:   $hash" -ForegroundColor Red
    exit 1
  }
  Write-Host "Checksum OK." -ForegroundColor Green

  # --- 6. extract --------------------------------------------------------------
  Expand-Archive -LiteralPath $zip -DestinationPath $Dir -Force
  if (-not (Test-Path -LiteralPath $exe)) {
    Write-Host "WARNING: freebuff-proxy.exe not found at $exe after extraction." -ForegroundColor Yellow
    Get-ChildItem -LiteralPath $Dir -Recurse -Filter "freebuff-proxy.exe" | ForEach-Object { Write-Host "  found: $($_.FullName)" -ForegroundColor Yellow }
  }
}

# --- 7. .env - always copied from the shipped example ------------------------
Write-Host "Step 3/3: configuration (.env)" -ForegroundColor Cyan
$envPath = $EnvFile
if (-not $envPath) { $envPath = Join-Path $Dir ".env" }
if (-not $NoEnv) {
  $example = Join-Path $Dir ".env.example"
  if (-not (Test-Path -LiteralPath $envPath) -or $Force) {
    if (Test-Path -LiteralPath $example) {
      Copy-Item -LiteralPath $example -Destination $envPath -Force
      Write-Host ".env copied from .env.example" -ForegroundColor Green
    } else {
      $exampleUrl = "https://raw.githubusercontent.com/$Repo/main/.env.example"
      try {
        Invoke-WebRequest -Uri $exampleUrl -OutFile $envPath
        Write-Host ".env downloaded from the documented .env.example" -ForegroundColor Green
      } catch {
        [System.IO.File]::WriteAllText($envPath, "AUTH_TOKENS=`nLISTEN_ADDR=127.0.0.1:3457`nCOST_MODE=free`nMAX_MESSAGES_PER_DAY=0`nIDLE_ROTATION_TIMEOUT=0`n", (New-Object System.Text.UTF8Encoding($false)))
        Write-Host ".env created (minimal fallback)" -ForegroundColor Yellow
      }
    }
  } else {
    Write-Host ".env already exists; keeping it (use -Force to recreate)." -ForegroundColor Green
  }
}

function Set-EnvValue([string]$key, [string]$value) {
  if ($NoEnv) { return }
  $content = if (Test-Path -LiteralPath $envPath) { [System.IO.File]::ReadAllText($envPath, [System.Text.Encoding]::UTF8) } else { "" }
  if ($content -match "(?m)^$([regex]::Escape($key))=.*$") {
    $content = $content -replace "(?m)^$([regex]::Escape($key))=.*$", "$key=$value"
  } else {
    $content = $content.TrimEnd() + "`n$key=$value`n"
  }
  [System.IO.File]::WriteAllText($envPath, $content, (New-Object System.Text.UTF8Encoding($false)))
}

if (-not $NoEnv) {
  if ($SkipToken) {
    $existing = (Get-Content -LiteralPath $envPath | Where-Object { $_ -match '^AUTH_TOKENS=' } | Select-Object -First 1) -replace '^AUTH_TOKENS=', ''
    if ($existing -match 'cb_xxx|cb_yyy|changeme|your[-_]?token|^<') {
      Set-EnvValue "AUTH_TOKENS" ""
      Write-Host "Cleared the placeholder AUTH_TOKENS; empty is safer than a fake token." -ForegroundColor Yellow
    }
  } elseif ($token -and $token.Length -gt 12) {
    Set-EnvValue "AUTH_TOKENS" $token
    Write-Host "AUTH_TOKENS written into $envPath (value hidden)." -ForegroundColor Green
  } else {
    Write-Host "No CLI token available." -ForegroundColor Yellow
    $pasted = Read-Host "Paste your FreeBuff authToken (Enter to leave empty / bridge mode)"
    $pasted = $pasted.Trim().Trim('"').Trim("'")
    if ($pasted.Length -gt 8) {
      Set-EnvValue "AUTH_TOKENS" $pasted
      Write-Host "AUTH_TOKENS written into $envPath (value hidden)." -ForegroundColor Green
    } else {
      Set-EnvValue "AUTH_TOKENS" ""
      Write-Host "AUTH_TOKENS left empty. The proxy starts in bridge mode; clients must send their own token." -ForegroundColor Yellow
    }
  }
  Set-EnvValue "MAX_MESSAGES_PER_DAY" "0"
  Set-EnvValue "IDLE_ROTATION_TIMEOUT" "30m"
  Write-Host "Safety defaults: MAX_MESSAGES_PER_DAY=0 (unlimited with zero-spam 429 locks), IDLE_ROTATION_TIMEOUT=30m" -ForegroundColor Green
}
# --- 9. next steps & doctor check -------------------------------------------
Write-Host ""
Write-Host "Installation complete! Config: $envPath" -ForegroundColor Green
Write-Host ""

if (Test-Path -LiteralPath $exe) {
  Write-Host "Running self-diagnostic doctor..." -ForegroundColor Cyan
  & $exe -doctor
  Write-Host ""
}

Write-Host "Next steps:" -ForegroundColor Cyan
Write-Host "  1. 1-Click Client Setup:   cd $Dir; .\freebuff-proxy.exe -setup"
Write-Host "  2. Start the proxy server: cd $Dir; .\freebuff-proxy.exe"
Write-Host ""
Write-Host "Test the proxy:" -ForegroundColor Cyan
Write-Host "  curl http://localhost:3457/healthz"
Write-Host "  curl http://localhost:3457/v1/models"
Write-Host "  curl -s -X POST http://localhost:3457/v1/chat/completions -H 'Content-Type: application/json' -d '{\`"model\`":\`"deepseek/deepseek-v4-flash\`",\`"messages\`":[{\`"role\`":\`"user\`",\`"content\`":\`"Say hello\`"}],\`"stream\`":false}'"
Write-Host ""
Write-Host "Quick Integration: Point your AI tools (Continue, Cursor, OpenCode, Aider, 9router) to http://localhost:3457/v1" -ForegroundColor Green
