# install-freebuff-proxy.ps1 - download the latest freebuff-proxy release for
# this machine, verify it, set up .env, and print the next steps.
#
# Zero-knowledge user flow:
#   1. Open PowerShell (Windows Terminal / pwsh or powershell.exe)
#   2. Run:
#        irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
#   3. Read what it prints. It checks the tools you need (installing Node.js
#      via winget if you want the freebuff CLI path), downloads the binary,
#      creates .env from the example, and either pulls your token from the
#      freebuff CLI login or asks you to paste it.
#
# What it does NOT do: modify system paths, install services, or touch your
# token except writing it into the local .env (gitignored).

param(
  [string]$Dir = "",          # install directory; default: current directory
  [switch]$SkipToken,         # do not look for a token (set AUTH_TOKENS later)
  [switch]$NoEnv,             # do not create .env (advanced)
  [switch]$Force,             # re-download even if the binary already exists
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
# If no authToken exists, make sure the official CLI is installed, then launch
# it so the user can log in. This fails early instead of installing a proxy that
# can only return "Invalid API key" later.
$token = $null
$creds = Find-CredentialsFile
if ($creds) { $token = Get-AuthToken $creds }

if (-not $SkipToken -and (-not $token -or $token.Length -le 12)) {
  Write-Host "Step 1/3: FreeBuff token (required before installing the proxy)" -ForegroundColor Cyan
  $haveWinget = (Get-Command winget -ErrorAction SilentlyContinue) -ne $null
  $haveNpm = (Get-Command npm -ErrorAction SilentlyContinue) -ne $null
  $haveFreebuff = (Get-Command freebuff -ErrorAction SilentlyContinue) -ne $null

  if (-not $haveFreebuff) {
    Write-Host "The official freebuff CLI is NOT installed." -ForegroundColor Yellow
    Write-Host "The installer needs it to mint/read your token. It will run:" -ForegroundColor Yellow
    Write-Host "  npm install -g freebuff" -ForegroundColor White
    $ans = Read-Host "Install the freebuff CLI now? [Y/n]"
    if ($ans -ne "n" -and $ans -ne "N") {
      if (-not $haveNpm) {
        if (-not $haveWinget) {
          Write-Host "ERROR: npm and winget are unavailable. Install Node.js 20+ from https://nodejs.org, then re-run." -ForegroundColor Red
          exit 1
        }
        Write-Host "Installing Node.js LTS via winget..." -ForegroundColor Cyan
        winget install --id OpenJS.NodeJS.LTS -e --accept-source-agreements --accept-package-agreements --silent
        $env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")
      }
      Write-Host "Installing the freebuff CLI (npm install -g freebuff)..." -ForegroundColor Cyan
      npm install -g freebuff
      if ($LASTEXITCODE -ne 0) { Write-Host "ERROR: npm install -g freebuff failed." -ForegroundColor Red; exit 1 }
      $env:Path = [System.Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path", "User")
      $haveFreebuff = (Get-Command freebuff -ErrorAction SilentlyContinue) -ne $null
      if (-not $haveFreebuff) { Write-Host "ERROR: freebuff installed but is not on PATH. Open a new terminal and re-run." -ForegroundColor Red; exit 1 }
    }
  } else {
    Write-Host "freebuff CLI detected: $((Get-Command freebuff).Source)" -ForegroundColor Green
  }

  if ($haveFreebuff) {
    Write-Host "No login token found. Starting 'freebuff' now." -ForegroundColor Yellow
    Write-Host "Complete the browser login, then quit the CLI (Ctrl+C or /exit) to continue." -ForegroundColor Yellow
    & freebuff
    $creds = Find-CredentialsFile
    if ($creds) { $token = Get-AuthToken $creds }
    if (-not $token -or $token.Length -le 12) {
      Write-Host "No authToken found after login. Open another terminal, run 'freebuff', finish login, then press Enter here." -ForegroundColor Yellow
      Read-Host | Out-Null
      $creds = Find-CredentialsFile
      if ($creds) { $token = Get-AuthToken $creds }
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

# --- 7. .env -----------------------------------------------------------------
$envPath = $EnvFile
if (-not $envPath) { $envPath = Join-Path $Dir ".env" }
if (-not $NoEnv) {
  $example = Join-Path $Dir ".env.example"
  if (-not (Test-Path -LiteralPath $envPath)) {
    if (Test-Path -LiteralPath $example) {
      Copy-Item -LiteralPath $example -Destination $envPath
      Write-Host ".env created from .env.example" -ForegroundColor Green
    } else {
      Set-Content -LiteralPath $envPath -Value "# freebuff-proxy config`n" -Encoding utf8
      Write-Host ".env created (no .env.example in release; wrote a minimal file)" -ForegroundColor Green
    }
  } else {
    Write-Host ".env already exists, leaving it as is" -ForegroundColor Green
  }
}

# --- 8. token ----------------------------------------------------------------
if (-not $SkipToken) {
  $token = $null
  $creds = Find-CredentialsFile
  if ($creds) { $token = Get-AuthToken $creds }
  if ($token -and $token.Length -gt 12) {
    $content = Get-Content -LiteralPath $envPath -Raw
    if ($content -match '(?m)^AUTH_TOKENS=.*$') {
      $content = $content -replace '(?m)^AUTH_TOKENS=.*$', "AUTH_TOKENS=$token"
    } else {
      $content = $content.TrimEnd() + "`nAUTH_TOKENS=$token`n"
    }
    Set-Content -LiteralPath $envPath -Value $content -NoNewline -Encoding utf8
    $masked = $token.Substring(0, 8) + "..." + $token.Substring($token.Length - 4)
    Write-Host "Token found from your freebuff CLI login: $masked" -ForegroundColor Green
    Write-Host "AUTH_TOKENS written into $envPath" -ForegroundColor Green
  } else {
    Write-Host "No freebuff CLI token found." -ForegroundColor Yellow
    Write-Host "Get one from the web page, then paste it here (or press Enter to skip and set it manually):" -ForegroundColor Yellow
    $pasted = Read-Host "Paste the login URL (https://freebuff.com/login?auth_code=...) or just the auth_code"
    $pasted = $pasted.Trim()
    if ($pasted) {
      if ($pasted -match 'auth_code=([^&\s]+)') { $pasted = $Matches[1] }
      if ($pasted.Length -gt 8) {
        $content = Get-Content -LiteralPath $envPath -Raw
        if ($content -match '(?m)^AUTH_TOKENS=.*$') {
          $content = $content -replace '(?m)^AUTH_TOKENS=.*$', "AUTH_TOKENS=$pasted"
        } else {
          $content = $content.TrimEnd() + "`nAUTH_TOKENS=$pasted`n"
        }
        Set-Content -LiteralPath $envPath -Value $content -NoNewline -Encoding utf8
        $masked = $pasted.Substring(0, 8) + "..." + $pasted.Substring($pasted.Length - 4)
        Write-Host "AUTH_TOKENS written to $envPath ($masked)" -ForegroundColor Green
      } else {
        Write-Host "That does not look like a token; skipping. Set AUTH_TOKENS in $envPath manually." -ForegroundColor Yellow
      }
    } else {
      Write-Host "Skipped. To add the token manually:" -ForegroundColor Yellow
      Write-Host "  1. Log in at https://freebuff.llm.pm, Freebuff Auth -> Generate login URL" -ForegroundColor Yellow
      Write-Host "  2. Copy the URL it shows (https://freebuff.com/login?auth_code=...)" -ForegroundColor Yellow
      Write-Host "  3. Edit $envPath and set:  AUTH_TOKENS=<the auth_code value from that link>" -ForegroundColor Yellow
      Write-Host "  No Bearer prefix, no quotes needed." -ForegroundColor Yellow
    }
  }
}

# --- 9. next steps -----------------------------------------------------------
Write-Host ""
Write-Host "Done. Next:" -ForegroundColor Cyan
Write-Host "  cd $Dir"
if (Test-Path -LiteralPath $exe) {
  Write-Host "  .\freebuff-proxy.exe"
} else {
  Write-Host "  .\freebuff-proxy.exe   (find the exe first, see the WARNING above)"
}
Write-Host ""
Write-Host "Then check it:"
Write-Host "  curl http://localhost:3457/healthz"
Write-Host "  curl http://localhost:3457/v1/models"
Write-Host ""
Write-Host "See the README (in the zip) for the full guide and 9router wiring."
