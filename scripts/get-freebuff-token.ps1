# get-freebuff-token.ps1 - Legacy alias forwarding to gen-freebuff-token.ps1
# (the canonical headless generator; the parameter set MUST mirror the
# callee — splatting switches the callee does not declare fails binding).
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

$scriptPath = Join-Path $PSScriptRoot "gen-freebuff-token.ps1"
& $scriptPath @PSBoundParameters
