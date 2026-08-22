# gen-freebuff-token.ps1 - Generate a FreeBuff auth token via headless login flow
# Default: the fresh token is NEVER sent back to the server after login.
# Pass -Verify to opt in to the post-auth /api/v1/freebuff/session probe
# (ban/tier check, no session slot claimed).
[CmdletBinding()]
param(
    [switch]$Save,
    [switch]$ToClipboard,
    [switch]$Incognito,
    [switch]$Append,
    [switch]$Verify,
    [string]$EnvFile = "",
    [string]$BaseUrl = $(if ($env:FREEBUFF_BASE_URL) { $env:FREEBUFF_BASE_URL } else { "https://www.codebuff.com" }),
    [int]$TimeoutSeconds = 300,
    [int]$PollIntervalMs = 5000
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
# Windows PowerShell 5.1 does not load System.Net.Http into the default
# AppDomain; Invoke-FreebuffApi's HttpClient needs it. No-op on PS 7.
Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue
$BaseUrl = $BaseUrl.TrimEnd('/')

# --- helpers -----------------------------------------------------------------
function Get-HomeDirectory {
    if ($env:HOME) { return $env:HOME }
    if ($env:USERPROFILE) { return $env:USERPROFILE }
    return [Environment]::GetFolderPath("UserProfile")
}

function Get-ConfigDir {
    if ($env:XDG_CONFIG_HOME -and $env:XDG_CONFIG_HOME.Trim()) {
        return Join-Path $env:XDG_CONFIG_HOME "manicode"
    }
    return Join-Path (Join-Path (Get-HomeDirectory) ".config") "manicode"
}

function Get-CredentialsPath {
    return Join-Path (Get-ConfigDir) "credentials.json"
}

function Generate-FingerprintId {
    $bytes = [byte[]]::new(32)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    $b64 = [Convert]::ToBase64String($bytes).Replace('+','-').Replace('/','_').TrimEnd('=')
    # 32 bytes -> 43 chars base64url
    return "enhanced-$b64"
}

# PowerShell's User-Agent handling can mangle or reject non-.NET UA grammar,
# and these values must match the proxy's wire identity byte-for-byte, so
# send them raw via HttpClient + TryAddWithoutValidation, which works on
# both PS 5.1 and 7. Auth/session paths carry Bun/1.3.14 (the proxy's
# newRequest default, mirroring the upstream CLI); the chat ai-sdk UA never
# appears on this script's endpoints.
# Throws on HTTP >= 400 with an HttpStatusCode property the poll loop reads.
function Invoke-FreebuffApi {
    param(
        [string]$Uri,
        [string]$Method = "GET",
        [hashtable]$Headers = @{},
        [object]$Body = $null,
        [int]$TimeoutSec = 30
    )
    $client = [System.Net.Http.HttpClient]::new()
    try {
        $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSec)
        $req = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($Method), $Uri)
        foreach ($k in $Headers.Keys) {
            $null = $req.Headers.TryAddWithoutValidation([string]$k, [string]$Headers[$k])
        }
        if ($null -ne $Body) {
            $json = if ($Body -is [string]) { $Body } else { $Body | ConvertTo-Json -Compress }
            $req.Content = [System.Net.Http.StringContent]::new($json, [System.Text.Encoding]::UTF8, "application/json")
        }
        $resp = $client.SendAsync($req).GetAwaiter().GetResult()
        $status = [int]$resp.StatusCode
        if ($status -ge 400) {
            $ex = [System.Net.Http.HttpRequestException]::new("HTTP error $status")
            $ex | Add-Member -NotePropertyName HttpStatusCode -NotePropertyValue $status -Force
            throw $ex
        }
        $text = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        if ([string]::IsNullOrWhiteSpace($text)) { return $null }
        return ($text | ConvertFrom-Json)
    } finally {
        $client.Dispose()
    }
}

function Ensure-EnvFile {
    param([string]$Path)
    if (Test-Path -LiteralPath $Path) { return }
    $scriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
    $candidates = @(
        (Join-Path $scriptDir ".env.example"),
        (Join-Path (Split-Path -Parent $scriptDir -ErrorAction SilentlyContinue) ".env.example"),
        (Join-Path (Get-Location).Path ".env.example")
    )
    foreach ($c in $candidates) {
        if ($c -and (Test-Path -LiteralPath $c)) {
            Copy-Item -LiteralPath $c -Destination $Path -Force
            Write-Host " No .env found; created $Path from $c" -ForegroundColor Yellow
            return
        }
    }
    New-Item -ItemType File -Path $Path -Force | Out-Null
    Write-Host " No .env found; created empty $Path" -ForegroundColor Yellow
}

function Resolve-TargetEnvPath {
    param([string]$CustomPath)
    if ($CustomPath -and $CustomPath.Trim()) {
        $p = $CustomPath.Trim()
        # Join-Path glues an absolute -EnvFile onto the CWD ("D:\x" -> "C:\cwd\D:\x"),
        # and GetFullPath then rejects the embedded drive colon. Use absolute
        # paths as-is; only relative ones join the working directory.
        if ([System.IO.Path]::IsPathRooted($p)) {
            return [System.IO.Path]::GetFullPath($p)
        }
        return [System.IO.Path]::GetFullPath((Join-Path (Get-Location).Path $p))
    }
    return [System.IO.Path]::GetFullPath((Join-Path (Get-Location).Path ".env"))
}

function Copy-TokenToClipboard {
    param([string]$Token)
    try {
        if (Get-Command Set-Clipboard -ErrorAction SilentlyContinue) {
            Set-Clipboard -Value $Token
            return $true
        }
    } catch {}
    # Fallbacks for macOS / Linux
    try {
        if (Get-Command pbcopy -ErrorAction SilentlyContinue) { $Token | pbcopy; return $true }
        if (Get-Command wl-copy -ErrorAction SilentlyContinue) { $Token | wl-copy; return $true }
        if (Get-Command xclip -ErrorAction SilentlyContinue) { $Token | xclip -selection clipboard; return $true }
        if (Get-Command xsel -ErrorAction SilentlyContinue) { $Token | xsel --clipboard --input; return $true }
    } catch {}
    return $false
}

function Update-EnvFileWithToken {
    param([string]$EnvPath, [string]$Token)

    Ensure-EnvFile -Path $EnvPath
    $content = ""
    try { $content = [System.IO.File]::ReadAllText($EnvPath, [System.Text.Encoding]::UTF8) } catch { $content = "" }
    if (-not $content) { $content = "" }

    if ($content.Contains($Token)) {
        Write-Host " Token already present in $EnvPath; skipped." -ForegroundColor DarkGray
        return
    }

    if ($content -match '(?m)^\s*AUTH_TOKENS\s*=\s*(.*)$') {
        # (.*) can capture an inline comment ("AUTH_TOKENS= # note") or — when
        # the value line is empty and followed by a blank line then a comment —
        # the comment itself; strip any trailing # comment before appending.
        $existingRaw = ($Matches[1] -replace '\s*#.*$', '').Trim().Trim('"').Trim("'")
        $newValue = if ($existingRaw) { "$existingRaw,$Token" } else { $Token }
        # clean double commas / spaces
        $newValue = ($newValue -replace ',\s*,', ',' -replace '^\s*,|,\s*$', '').Trim()
        # MatchEvaluator: a $-interpolated replacement string would let a token
        # containing $&/$1/$$ be re-expanded as match/group tokens and corrupt
        # AUTH_TOKENS. The delegate emits the literal value.
        $newContent = [regex]::Replace($content, '(?m)^\s*AUTH_TOKENS\s*=.*$', { param($m) "AUTH_TOKENS=" + $newValue })
        [System.IO.File]::WriteAllText($EnvPath, $newContent, (New-Object System.Text.UTF8Encoding($false)))
    } else {
        # Plain string concatenation (no regex substitution semantics), so a
        # token containing $&/$1/$$/backtick is written verbatim.
        $sep = if ($content.EndsWith("`n") -or $content -eq "") { "" } else { "`n" }
        $content = $content + "$sep`AUTH_TOKENS=$Token`n"
        [System.IO.File]::WriteAllText($EnvPath, $content, (New-Object System.Text.UTF8Encoding($false)))
    }
    Write-Host " Appended to: $EnvPath" -ForegroundColor Green
}

# --- 0. warning --------------------------------------------------------------
Write-Host ""
Write-Host "FreeBuff Token Generator" -ForegroundColor Cyan
Write-Host "WARNING: Using tokens through a proxy violates FreeBuff ToS." -ForegroundColor Yellow
Write-Host "Accounts may be suspended or banned. You accept this risk." -ForegroundColor Yellow
Write-Host ""

# --- 0.5 recommend options ---------------------------------------------------
if (-not $Save -and -not $ToClipboard -and -not $Append -and -not $Incognito) {
    if ([Console]::IsInputRedirected) {
        $Append = $true
    } else {
        Write-Host "Recommended options:" -ForegroundColor Cyan
        Write-Host " [Enter] Append token to .\.env (auto-copy .env.example if missing)" -ForegroundColor Green
        Write-Host " 1) Copy token to clipboard" -ForegroundColor Gray
        Write-Host " 2) Save to ~/.config/manicode/credentials.json" -ForegroundColor Gray
        Write-Host " 3) Print token only" -ForegroundColor Gray
        Write-Host " 4) Incognito login (no auto-open; use a private window)" -ForegroundColor Gray
        $choice = Read-Host "Choose [Enter]"
        switch ($choice.Trim().ToLower()) {
            { $_ -in "", "append", "a" } { $Append = $true }
            { $_ -in "1", "clipboard", "c" } { $ToClipboard = $true }
            { $_ -in "2", "save", "s" } { $Save = $true }
            { $_ -in "3", "print", "p" } { }
            { $_ -in "4", "incognito", "i" } { $Incognito = $true }
            default {
                Write-Host "Unknown choice '$choice'; using recommended (append)." -ForegroundColor Yellow
                $Append = $true
            }
        }
        Write-Host ""
    }
}

# --- 1. generate fingerprint + request login URL -----------------------------
$fingerprintId = Generate-FingerprintId
Write-Host "Fingerprint: $fingerprintId" -ForegroundColor DarkGray
Write-Host "Requesting login URL..." -ForegroundColor Cyan

$codeHeaders = @{
    "User-Agent" = "Bun/1.3.14"
    "Accept" = "application/json"
}

try {
    $codeBody = @{ fingerprintId = $fingerprintId } | ConvertTo-Json -Compress
    $codeResp = Invoke-FreebuffApi -Uri "$BaseUrl/api/auth/cli/code" -Method POST -Headers $codeHeaders -Body $codeBody
} catch {
    $errMsg = try { $_.Exception.Message } catch { "$_" }
    Write-Host "Failed to get login URL: $errMsg" -ForegroundColor Red
    if ($_.ErrorDetails -and $_.ErrorDetails.Message) { Write-Host $_.ErrorDetails.Message -ForegroundColor DarkGray }
    exit 1
}

$loginUrl = try { [string]$codeResp.loginUrl } catch { "" }
if (-not $loginUrl) { $loginUrl = try { [string]$codeResp.login_url } catch { "" } }
$fingerprintHash = try { [string]$codeResp.fingerprintHash } catch { "" }
$expiresAt = try { [string]$codeResp.expiresAt } catch { "" }

if (-not $loginUrl) {
    Write-Host "No loginUrl in response. Server may be down." -ForegroundColor Red
    Write-Host ($codeResp | ConvertTo-Json -Depth 4) -ForegroundColor DarkGray
    exit 1
}

# --- 2. open browser ---------------------------------------------------------
Write-Host ""
if ($Incognito) {
    $TimeoutSeconds = 600
    Write-Host "Incognito mode: open the URL below in a PRIVATE/INCOGNITO window manually." -ForegroundColor Cyan
    Write-Host "URL: $loginUrl" -ForegroundColor White
    Write-Host ""
    Write-Host " -> Ctrl+Shift+N / Cmd+Shift+N for private window" -ForegroundColor Yellow
    Write-Host " -> Log in with the GitHub account you want a token for." -ForegroundColor Yellow
    Write-Host " -> Waiting up to ${TimeoutSeconds}s" -ForegroundColor Yellow
    Write-Host ""
} else {
    Write-Host "Opening browser for GitHub login..." -ForegroundColor Green
    Write-Host "URL: $loginUrl" -ForegroundColor DarkGray
    Write-Host ""
    Write-Host " -> Log in with the GitHub account you want a token for." -ForegroundColor Yellow
    Write-Host " -> If you want a DIFFERENT account, sign out of GitHub first!" -ForegroundColor Yellow
    Write-Host ""
    try { Start-Process $loginUrl -ErrorAction Stop | Out-Null } catch { Write-Host "Could not auto-open browser. Please open URL manually." -ForegroundColor Yellow }
}

# --- 3. poll for auth completion ---------------------------------------------
Write-Host "Waiting for login (timeout: ${TimeoutSeconds}s)..." -ForegroundColor Cyan
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$attempts = 0
$user = $null

while ($true) {
    if ($sw.Elapsed.TotalSeconds -ge $TimeoutSeconds) {
        Write-Host "Login timed out after ${TimeoutSeconds}s." -ForegroundColor Red
        exit 1
    }
    $attempts++
    Start-Sleep -Milliseconds $PollIntervalMs

    try {
        $qs = @("fingerprintId=$([Uri]::EscapeDataString($fingerprintId))")
        if ($fingerprintHash) { $qs += "fingerprintHash=$([Uri]::EscapeDataString("$fingerprintHash"))" }
        if ($expiresAt) { $qs += "expiresAt=$([Uri]::EscapeDataString("$expiresAt"))" }
        $query = $qs -join "&"

        $statusResp = Invoke-FreebuffApi -Uri "$BaseUrl/api/auth/cli/status?$query" -Method GET -Headers $codeHeaders

        # Handle different response shapes (StrictMode-safe: a 200 without
        # "user" must not throw PropertyNotFound, just keep polling).
        $candidate = $null
        try {
            if ($statusResp.user -and $statusResp.user.authToken) { $candidate = $statusResp.user }
            elseif ($statusResp.user -and $statusResp.user.token) { $candidate = $statusResp.user; $candidate | Add-Member -NotePropertyName authToken -NotePropertyValue $statusResp.user.token -Force }
            elseif ($statusResp.authToken) { $candidate = @{ authToken = $statusResp.authToken; id = $statusResp.id; name = $statusResp.name; email = $statusResp.email } }
        } catch { $candidate = $null }

        if ($candidate -and $candidate.authToken) {
            $user = $candidate
            break
        }
        Write-Host " Polling ($attempts)... waiting for browser login" -ForegroundColor DarkGray
    } catch {
        $statusCode = $null
        try { $statusCode = $_.Exception.HttpStatusCode } catch {}
        if ($statusCode -eq 401 -or $statusCode -eq 404) {
            Write-Host " Polling ($attempts)... not yet authenticated" -ForegroundColor DarkGray
            continue
        }
        Write-Host " Polling error ($attempts): $($_.Exception.Message)" -ForegroundColor DarkGray
        continue
    }
}

# --- 4. extract token --------------------------------------------------------
# Normalize to strings first: StrictMode throws on missing object properties
# and the login response shape does not guarantee id/name/email (observed:
# user.authToken only). Hashtable keys read as $null, PSObject gaps throw —
# both collapse to "".
$authToken = try { [string]$user.authToken } catch { "" }
$userName = try { [string]$user.name } catch { "" }
if (-not $userName) { $userName = "unknown" }
$userEmail = try { [string]$user.email } catch { "" }
if (-not $userEmail) { $userEmail = "unknown" }
$userId = try { [string]$user.id } catch { "" }

Write-Host ""
Write-Host "Login successful!" -ForegroundColor Green
Write-Host " Account: $userName ($userEmail)" -ForegroundColor Cyan
Write-Host " Token: $authToken" -ForegroundColor White

# --- 4.5 opt-in post-auth verification (anti-ban) ----------------------------
# OFF by default: the fresh token is never sent back to the server. Pass
# -Verify to opt in — it probes /api/v1/freebuff/session (no
# x-freebuff-instance-id, so no session slot is claimed; matches the proxy's
# -test-token probe) and refuses to save a banned account.
if ($Verify) {
try {
    $probeResp = Invoke-FreebuffApi -Uri "$BaseUrl/api/v1/freebuff/session" -Method GET -Headers @{ "Authorization" = "Bearer $authToken"; "User-Agent" = "Bun/1.3.14" } -TimeoutSec 15
    # StrictMode-safe reads: the live session response carries status +
    # accessTier but not always currentRiskScore (observed: status=active,
    # accessTier=full, no risk field) — a missing key must not abort the
    # confirmation line.
    $probeStatus = try { [string]$probeResp.status } catch { "" }
    $probeTier = try { [string]$probeResp.accessTier } catch { "" }
    $probeRisk = try { [string]$probeResp.currentRiskScore } catch { "" }
    if ($probeStatus -eq "banned") {
        Write-Host "ABORT: this account is BANNED upstream. Refusing to save the token." -ForegroundColor Red
        exit 1
    }
    Write-Host "Account check: status=$probeStatus tier=$(if ($probeTier) { $probeTier } else { '?' }) risk=$(if ($probeRisk) { $probeRisk } else { '?' })" -ForegroundColor Cyan
} catch {
    Write-Host "Probe unreadable; continuing without tier confirmation: $($_.Exception.Message)" -ForegroundColor Yellow
}
}

# --- 5. save credentials locally ---------------------------------------------
if ($Save) {
    $configDir = Get-ConfigDir
    if (-not (Test-Path -LiteralPath $configDir)) { New-Item -ItemType Directory -Path $configDir -Force | Out-Null }
    $credPath = Get-CredentialsPath
    $credData = @{ default = @{ id = $userId; name = $userName; email = $userEmail; authToken = $authToken; fingerprintId = $fingerprintId; fingerprintHash = $fingerprintHash } } | ConvertTo-Json -Depth 5
    [System.IO.File]::WriteAllText($credPath, $credData, (New-Object System.Text.UTF8Encoding($false)))
    Write-Host " Saved to: $credPath" -ForegroundColor DarkGray
}

# --- 6. output options -------------------------------------------------------
if ($Incognito -and -not $ToClipboard -and -not $Save) { $Append = $true }

if ($ToClipboard) {
    if (Copy-TokenToClipboard -Token $authToken) {
        Write-Host " Copied to clipboard!" -ForegroundColor Green
    } else {
        Write-Host " Could not copy to clipboard (no clipboard tool found)" -ForegroundColor Yellow
    }
}

if ($Append) {
    $targetEnv = Resolve-TargetEnvPath -CustomPath $EnvFile
    Update-EnvFileWithToken -EnvPath $targetEnv -Token $authToken
}

Write-Host ""
Write-Host "Done! Add this token to your 9router or .env AUTH_TOKENS." -ForegroundColor Cyan
Write-Host ""
