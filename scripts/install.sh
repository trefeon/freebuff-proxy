#!/usr/bin/env bash
# install.sh - interactive easy-mode installer for freebuff-proxy.
#
# Flow (curl | bash compatible): reads prompts from the controlling terminal
# (/dev/tty), so it works even when piped:
#     curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install.sh | bash
#
# Menu: (1) Easy install (recommended)  (2) Manual binary  (3) Docker Compose
#       (4) Bridge mode (clients bring their own FreeBuff token)
#
# Order of operations (fail fast on the token BEFORE downloading anything):
#   1. pick the install method
#   2. TOKEN PREREQUISITE: reuse the CLI credentials file if you already have
#      one; otherwise 1-click browser login (headless OAuth), manual paste, or
#      bridge mode - zero extra dependencies.
#   3. install the proxy (release binary, or clone + docker compose)
#   4. write .env - always, for every option: copied from .env.example (fetched
#      from GitHub if the archive did not ship one), then filled with your token,
#      LISTEN_ADDR for containers, and the account-safety knobs.
#
# Non-interactive flags (scripted use):
#   --dir=<path> / --dir <path>   target directory (default: current dir)
#   --skip-token                  do not touch AUTH_TOKENS
#   --no-env                      do not create/update .env
#   --force                       re-download / overwrite even if present
#   --env-file=<path>             write .env to <path>
#   --method=binary|docker|bridge skip the menu
#   --no-prompt                   safe defaults, never read the terminal
#   --no-cli-install              legacy no-op kept for compatibility; the
#                                 installer never installs or waits for a CLI
#   -h|--help                     this header
#
# What it does NOT do: modify system paths, install services, or touch your
# token except writing it into the local .env (gitignored).
set -euo pipefail

REPO="trefeon/freebuff-proxy"
RAW_BASE="https://raw.githubusercontent.com/$REPO/main"
DIR=""
SKIP_TOKEN=0
NO_ENV=0
FORCE=0
LOGOUT=0
ENV_FILE=""
METHOD=""
NO_PROMPT=0
NO_CLI_INSTALL=0
TOKEN_VALUE=""       # filled by the token prerequisite, written into .env later
TOKEN_SOURCE=""
CLI_UA="ai-sdk/openai-compatible/1.0.0/codebuff"

while [ $# -gt 0 ]; do
  case "$1" in
    --dir=*) DIR="${1#*=}"; shift ;;
    --dir) DIR="${2:-}"; shift 2 ;;
    --skip-token) SKIP_TOKEN=1; shift ;;
    --no-env) NO_ENV=1; shift ;;
    --force) FORCE=1; shift ;;
    --logout) LOGOUT=1; shift ;;
    --env-file=*) ENV_FILE="${1#*=}"; shift ;;
    --env-file) ENV_FILE="${2:-}"; shift 2 ;;
    --method=*) METHOD="${1#*=}"; shift ;;
    --method) METHOD="${2:-}"; shift 2 ;;
    --no-prompt) NO_PROMPT=1; shift ;;
    --no-cli-install) NO_CLI_INSTALL=1; shift ;;
    --help) grep '^#' "$0" | head -36; exit 0 ;;
    *) echo "unknown arg: $1 (see header)" >&2; exit 1 ;;
  esac
done

if [ "$LOGOUT" = "1" ]; then
  for p in "$HOME/.config/manicode/credentials.json" "$HOME/.config/codebuff/credentials.json"; do
    [ -f "$p" ] && rm -f "$p" && ok "Cleared existing login credentials ($p)."
  done
fi

c() { printf '\033[36m%s\033[0m\n' "$*"; }
ok() { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
die() { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

# --- interactive input -------------------------------------------------------
# curl|bash pipes the script on stdin, so prompts MUST come from /dev/tty.
TTY_FD=3
if ! (exec 3< /dev/tty) 2>/dev/null; then
  if [ "$NO_PROMPT" = "0" ]; then
    echo ""
    echo "ERROR: no interactive terminal detected. Options:" >&2
    echo "  1. Run the script from a real terminal (download it first):" >&2
    echo "       curl -sSL -o install.sh $RAW_BASE/scripts/install.sh" >&2
    echo "       bash install.sh" >&2
    echo "  2. Run non-interactively with defaults:" >&2
    echo "       curl -sSL $RAW_BASE/scripts/install.sh | bash -s -- --no-prompt" >&2
    exit 1
  fi
  TTY_FD=0
else
  exec 3< /dev/tty
fi

# ask <var> <prompt> [<default>]
ask() {
  local __var="$1" __prompt="$2" __default="${3:-}" __answer=""
  if [ "$NO_PROMPT" = "1" ]; then eval "$__var=\"\$__default\""; return; fi
  printf '%s' "$__prompt"
  [ -n "$__default" ] && printf ' [%s]' "$__default"
  printf ': '
  read -r -u "$TTY_FD" __answer || true
  __answer="${__answer:-$__default}"
  eval "$__var=\"\$__answer\""
}

# confirm <prompt> - yes/no, default yes. Always true under --no-prompt.
confirm() {
  [ "$NO_PROMPT" = "1" ] && return 0
  local __a=""
  printf '%s [Y/n]: ' "$1"
  read -r -u "$TTY_FD" __a || true
  case "${__a:-y}" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

# menu <var> <title> <n> <label> ... <default>
menu() {
  local __var="$1" __title="$2" __default="${!#}" __answer=""
  shift 2
  echo ""
  c "$__title"
  local __n=1
  while [ $# -gt 1 ]; do
    echo "  $__n) $2"
    __n=$((__n + 1)); shift 2
  done
  if [ "$NO_PROMPT" = "1" ]; then eval "$__var=\"\$__default\""; return; fi
  printf 'Choose (default %s): ' "$__default"
  read -r -u "$TTY_FD" __answer || true
  __answer="${__answer:-$__default}"
  eval "$__var=\"\$__answer\""
}

pkg_install() {
  if command -v apt-get >/dev/null 2>&1; then sudo apt-get update -qq && sudo apt-get install -y -qq "$@"
  elif command -v dnf >/dev/null 2>&1; then sudo dnf install -y "$@"
  elif command -v yum >/dev/null 2>&1; then sudo yum install -y "$@"
  elif command -v apk >/dev/null 2>&1; then sudo apk add --no-cache "$@"
  elif command -v brew >/dev/null 2>&1; then brew install "$@"
  else return 1
  fi
}

# --- 0. warning --------------------------------------------------------------
echo ""
echo "WARNING: using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff" >&2
echo "terms of service. Accounts get suspended or banned (403 account_banned, dashboard shows" >&2
echo "'suspended'). Bans are per account, usually permanent, and there is no self-service" >&2
echo "unban. Use ONE account, keep usage modest, do not run the proxy 24/7, and expect the" >&2
echo "account to be banned eventually. You accept this risk by continuing." >&2
echo "" >&2

# --- 1. deployment method ----------------------------------------------------
if [ -z "$METHOD" ]; then
  menu METHOD "How do you want to install freebuff-proxy?" \
    1 "Easy install (recommended) - binary + token + safety defaults, one flow" \
    2 "Manual binary - download the latest release, fine-grained choices" \
    3 "Docker Compose - run in a container on this host" \
    4 "Bridge mode - proxy only; each client sends their own FreeBuff token" \
    "1"
fi
BRIDGE=0
case "$METHOD" in
  1|easy) METHOD="easy" ;;
  2|binary) METHOD="binary" ;;
  3|docker) METHOD="docker" ;;
  4|bridge) BRIDGE=1
    if [ "$NO_PROMPT" = "1" ]; then
      METHOD="binary"
    else
      menu BRIDGE_BASE "Bridge mode on top of which runtime?" \
        1 "Release binary" \
        2 "Docker Compose" \
        "1"
      case "$BRIDGE_BASE" in 2|docker) METHOD="docker" ;; *) METHOD="binary" ;; esac
    fi ;;
  *) die "unknown method: $METHOD (binary|docker|bridge)" ;;
esac
if [ "$BRIDGE" = "1" ]; then
  warn "Bridge mode: the proxy will NOT store any token. Clients must send their own"
  warn "FreeBuff token as 'Authorization: Bearer <token>' on every chat request."
  SKIP_TOKEN=1
fi

# --- 2. target directory ------------------------------------------------------
if [ -z "$DIR" ]; then DIR="$(pwd)"; fi
mkdir -p "$DIR"

# --- 3. TOKEN PREREQUISITE (before installing anything) -----------------------
# The proxy is useless without a FreeBuff token. Order: reuse an existing
# credentials.json if present, else 1-click headless browser OAuth, else manual
# paste, else bridge mode - all before we download the proxy.
CREDS_PATHS="$HOME/.config/manicode/credentials.json $HOME/.config/codebuff/credentials.json"
[ -n "${USERPROFILE:-}" ] && CREDS_PATHS="$CREDS_PATHS $USERPROFILE/.config/manicode/credentials.json"

creds_file() {
  for p in $CREDS_PATHS; do [ -f "$p" ] && { echo "$p"; return 0; }; done
  return 1
}

read_token() {
  local creds="$1" t=""
  if command -v python3 >/dev/null 2>&1; then
    t="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
    acct = d.get("default") or next(iter(d.values()), {})
    t = acct.get("authToken") if isinstance(acct, dict) else None
    if t: sys.stdout.write(t)
except Exception:
    pass
' "$creds" 2>/dev/null || true)"
  fi
  printf '%s' "$t"
}

token_from_creds() {
  local creds tok
  creds="$(creds_file)" || return 1
  tok="$(read_token "$creds")"
  [ -n "$tok" ] && [ "${#tok}" -gt 12 ] || return 1
  TOKEN_VALUE="$tok"
  TOKEN_SOURCE="$creds"
  return 0
}

# urlencode <str> - minimal percent-encoding for query-string values (no deps).
urlencode() {
  printf '%s' "$1" | sed \
    -e 's/%/%25/g' \
    -e 's/ /%20/g' \
    -e 's/&/%26/g' \
    -e 's/+/%2B/g' \
    -e 's/=/%3D/g' \
    -e 's/#/%23/g' \
    -e 's/:/%3A/g' \
    -e 's/,/%2C/g' \
    -e 's|/|%2F|g' \
    -e 's/?/%3F/g'
}

obtain_headless_token() {
  [ "$NO_PROMPT" = "1" ] && { warn "Non-interactive mode: skipping browser login; AUTH_TOKENS stays empty."; return 1; }
  c "Requesting login URL for browser authentication..."
  local fp="enhanced-$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
  local code_resp
  code_resp=$(curl -sS -X POST "https://www.codebuff.com/api/auth/cli/code" \
    -H "User-Agent: $CLI_UA" \
    -H "Content-Type: application/json" \
    -d "{\"fingerprintId\":\"$fp\"}" 2>/dev/null || true)
  local login_url fp_hash expires_at
  login_url=$(echo "$code_resp" | sed -n 's/.*"loginUrl": *"\([^"]*\)".*/\1/p')
  fp_hash=$(echo "$code_resp" | sed -n 's/.*"fingerprintHash": *"\([^"]*\)".*/\1/p')
  expires_at=$(echo "$code_resp" | sed -n 's/.*"expiresAt": *"\([^"]*\)".*/\1/p')

  if [ -z "$login_url" ]; then
    warn "Could not obtain login URL from upstream server."
    return 1
  fi

  # percent-encode the poll params (expiresAt is ISO-8601, may carry ':'/'+')
  local enc_fp enc_hash enc_exp
  enc_fp="$(urlencode "$fp")"
  enc_hash="$(urlencode "$fp_hash")"
  enc_exp="$(urlencode "$expires_at")"

  echo ""
  ok "Opening browser for FreeBuff GitHub login..."
  echo "URL: $login_url"
  echo ""
  if command -v xdg-open >/dev/null 2>&1; then
    xdg-open "$login_url" 2>/dev/null &
  elif command -v open >/dev/null 2>&1; then
    open "$login_url" 2>/dev/null &
  else
    warn "Cannot open browser automatically. Please open the URL above manually."
  fi

  c "Waiting for authentication in browser (timeout: 300s)..."
  local start_time elapsed status_resp tok
  start_time=$(date +%s)
  while true; do
    elapsed=$(( $(date +%s) - start_time ))
    if [ "$elapsed" -ge 300 ]; then
      warn "Login timed out after 300s."
      return 1
    fi
    sleep 5
    status_resp=$(curl -sS -H "User-Agent: $CLI_UA" "https://www.codebuff.com/api/auth/cli/status?fingerprintId=$enc_fp&fingerprintHash=$enc_hash&expiresAt=$enc_exp" 2>/dev/null || true)
    tok=$(echo "$status_resp" | sed -n 's/.*"authToken": *"\([^"]*\)".*/\1/p')
    if [ -n "$tok" ] && [ "${#tok}" -gt 12 ]; then
      TOKEN_VALUE="$tok"
      TOKEN_SOURCE="headless OAuth login"
      ok "Authentication successful! Token acquired."
      return 0
    fi
  done
}

paste_token() {
  [ "$NO_PROMPT" = "1" ] && { warn "No token and --no-prompt: AUTH_TOKENS stays empty. Chat will not work until you set it."; return 1; }
  echo ""
  warn "Manual token entry: paste your FreeBuff authToken from credentials.json"
  local pasted=""
  ask pasted "Paste your authToken (Enter to skip)" ""
  [ -n "$pasted" ] || { warn "Skipped - you can set AUTH_TOKENS in .env later."; return 1; }
  pasted="$(echo "$pasted" | tr -d '[:space:]"')"
  if [ "${#pasted}" -gt 8 ]; then
    TOKEN_VALUE="$pasted"
    TOKEN_SOURCE="pasted"
    ok "Token accepted (value hidden)."
  else
    warn "That does not look like a token; skipped."
    return 1
  fi
}

if [ "$BRIDGE" = "1" ]; then
  c "Bridge mode: no token needed on this machine (clients bring their own)."
elif [ "$SKIP_TOKEN" = "1" ] || [ "$NO_ENV" = "1" ]; then
  warn "Token step skipped by flag."
else
  echo ""
  c "Step 1/3: FreeBuff token (required before installing the proxy)"
  if token_from_creds; then
    ok "Existing freebuff CLI login found ($TOKEN_SOURCE) - reusing its token."
  elif [ "$NO_PROMPT" = "0" ]; then
    echo "  1) Generate token now via browser login (recommended, zero extra dependencies)"
    echo "  2) Paste an existing FreeBuff authToken"
    echo "  3) Bridge mode (skip token; clients supply their own per request)"
    tok_choice=""
    ask tok_choice "Choose" "1"
    case "$tok_choice" in
      1|"") obtain_headless_token || paste_token || warn "Continuing without a token." ;;
      2) paste_token || warn "Continuing without a token." ;;
      3) BRIDGE=1; SKIP_TOKEN=1; ok "Switched to Bridge mode." ;;
      *) obtain_headless_token || paste_token || warn "Continuing without a token." ;;
    esac
  else
    obtain_headless_token || paste_token || warn "Continuing without a token - the proxy will start but chat will fail until AUTH_TOKENS is set."
  fi
fi

# --- 4. dependencies for the chosen method ------------------------------------
echo ""
c "Step 2/3: install the proxy"
if [ "$METHOD" = "docker" ]; then
  command -v docker >/dev/null 2>&1 || die "docker not found - install Docker first (https://docs.docker.com/engine/install/)."
  docker compose version >/dev/null 2>&1 || die "docker compose v2 not found - install the compose plugin."
  command -v git >/dev/null 2>&1 || pkg_install git || die "git not found and could not be installed."
  ok "Docker + Compose present."
else
  NEEDED=""
  for bin in curl tar sha256sum; do
    command -v "$bin" >/dev/null 2>&1 || NEEDED="$NEEDED $bin"
  done
  if [ -n "$NEEDED" ]; then
    warn "Missing dependencies:$NEEDED - installing via your package manager..."
    pkg_install curl tar coreutils || die "no supported package manager found. Install curl, tar and coreutils manually."
    for bin in curl tar sha256sum; do
      command -v "$bin" >/dev/null 2>&1 || die "$bin still missing after install."
    done
  fi
  ok "Dependencies OK (curl, tar, sha256sum)"
fi

# --- 5. install / materialize the files (so .env.example exists to copy) -------
CONFIG_DIR=""
BIN=""
REPO_DIR=""

if [ "$METHOD" = "docker" ]; then
  SCRIPT_DIR="$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo "")"
  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/../docker-compose.yml" ]; then
    REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
    ok "Using the repo checkout at $REPO_DIR"
  else
    REPO_DIR="$DIR/freebuff-proxy"
    [ -f "$DIR/docker-compose.yml" ] && REPO_DIR="$DIR"
  fi
  if [ ! -f "$REPO_DIR/docker-compose.yml" ]; then
    c "Cloning freebuff-proxy into $REPO_DIR..."
    git clone --quiet "https://github.com/$REPO.git" "$REPO_DIR" \
      || die "git clone failed. If github.com is blocked here, copy the repo manually and re-run with --dir <repo>."
  fi
  cd "$REPO_DIR"
  CONFIG_DIR="$REPO_DIR"
else
  c "Resolving the latest release..."
  RELEASE="$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest")"
  VERSION="$(printf '%s' "$RELEASE" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1 | sed 's/^v//')"
  [ -n "$VERSION" ] || die "could not resolve the latest release from GitHub (offline? blocked?)."
  ok "Latest release: v$VERSION"

  OS="$(uname -s)"
  case "$OS" in
    Linux*) GOOS="linux" ;;
    Darwin*) GOOS="darwin" ;;
    MINGW*|MSYS*|CYGWIN*) GOOS="windows" ;;
    *) die "unsupported OS: $OS" ;;
  esac
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64) GOARCH="amd64" ;;
    aarch64|arm64) GOARCH="arm64" ;;
    *) die "unsupported arch: $ARCH" ;;
  esac
  if [ "$GOOS" = "windows" ]; then
    ASSET="freebuff-proxy_${VERSION}_${GOOS}_${GOARCH}.zip"
    command -v unzip >/dev/null 2>&1 || die "unzip not found - install it or use the PowerShell installer."
  else
    ASSET="freebuff-proxy_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
  fi
  ok "Asset: $ASSET"

  EXISTING_BIN="$(find "$DIR" -maxdepth 2 -type f -name 'freebuff-proxy*' 2>/dev/null | head -1)"
  if [ -n "$EXISTING_BIN" ] && [ "$FORCE" = "0" ]; then
    warn "freebuff-proxy already exists: $EXISTING_BIN"
    warn "Skipping the download (re-run with --force to update)."
    BIN="$EXISTING_BIN"
  else
    ASSET_URL="$(printf '%s' "$RELEASE" | tr ',' '\n' | sed -n "s/.*\"browser_download_url\": *\"\([^\"]*${ASSET}\"\).*/\1/p" | head -1 | tr -d '"')"
    SUMS_URL="$(printf '%s' "$RELEASE" | tr ',' '\n' | sed -n 's/.*"browser_download_url": *"\([^"]*checksums.txt"\).*/\1/p' | head -1 | tr -d '"')"
    [ -n "$ASSET_URL" ] || die "asset $ASSET not found in the release."

    TMP="$(mktemp -d)"
    echo "Downloading..."
    curl -sSL -o "$TMP/$ASSET" "$ASSET_URL"
    curl -sSL -o "$TMP/checksums.txt" "$SUMS_URL"
    HASH_FILE="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
    EXPECTED="$(sed -n "s/^\([a-f0-9]*\)  .*${ASSET}$/\1/p" "$TMP/checksums.txt" | head -1)"
    if [ -z "$EXPECTED" ]; then
      EXPECTED="$(awk -v a="$ASSET" '$2==a {print $1}' "$TMP/checksums.txt" | head -1)"
    fi
    if [ "$HASH_FILE" != "$EXPECTED" ]; then
      echo "ERROR: checksum mismatch for $ASSET" >&2
      echo "  expected: $EXPECTED" >&2
      echo "  actual:   $HASH_FILE" >&2
      exit 1
    fi
    ok "Checksum OK."
    if [ "$GOOS" = "windows" ]; then
      unzip -o -q "$TMP/$ASSET" -d "$DIR"
      BIN="$DIR/freebuff-proxy.exe"
      [ -f "$BIN" ] || BIN="$(find "$DIR" -maxdepth 2 -type f -name 'freebuff-proxy*.exe' | head -1)"
    else
      tar xzf "$TMP/$ASSET" -C "$DIR"
      BIN="$DIR/freebuff-proxy"
      [ -x "$BIN" ] || BIN="$(find "$DIR" -maxdepth 2 -type f -name freebuff-proxy | head -1)"
      [ -n "$BIN" ] && chmod +x "$BIN"
    fi
    c "Binary: $BIN"
  fi
  CONFIG_DIR="$DIR"
fi

# --- 6. .env - always produced, adapted to the chosen option -------------------
echo ""
c "Step 3/3: configuration (.env)"
ENVPATH="$ENV_FILE"
[ -z "$ENVPATH" ] && ENVPATH="$CONFIG_DIR/.env"

ensure_env_file() {
  [ "$NO_ENV" = "1" ] && return 0
  if [ -f "$ENVPATH" ] && [ "$FORCE" = "0" ]; then
    ok ".env already exists at $ENVPATH, keeping it (use --force to recreate)"
    return 0
  fi
  [ -f "$ENVPATH" ] && [ "$FORCE" = "1" ] && warn "Recreating $ENVPATH (--force)."
  for cand in "$CONFIG_DIR/.env.example" "$(dirname "$ENVPATH")/.env.example"; do
    if [ -f "$cand" ]; then
      cp "$cand" "$ENVPATH"
      ok ".env created from $cand"
      return 0
    fi
  done
  if curl -fsSL -o "$ENVPATH.tmp" "$RAW_BASE/.env.example" 2>/dev/null && [ -s "$ENVPATH.tmp" ]; then
    mv "$ENVPATH.tmp" "$ENVPATH"
    ok ".env created from the documented .env.example (fetched from GitHub)"
    return 0
  fi
  rm -f "$ENVPATH.tmp" 2>/dev/null || true
  cat > "$ENVPATH" <<'MINIENV'
# freebuff-proxy config (minimal fallback - see the README for every key)
AUTH_TOKENS=
LISTEN_ADDR=127.0.0.1:3457
COST_MODE=free
MAX_MESSAGES_PER_DAY=0
IDLE_ROTATION_TIMEOUT=0
MINIENV
  warn ".env created (minimal fallback - .env.example was not reachable)"
}
ensure_env_file

# esc_val <str> - escape sed replacement metacharacters (&, \ and the |
# delimiter used below) so a value can be spliced into a `s|...|...|` script
# verbatim without corrupting the .env.
esc_val() { printf '%s' "$1" | sed -e 's/[&\\|]/\\&/g'; }

set_env() {
  [ "$NO_ENV" = "1" ] && return 0
  local key="$1" val="$2"
  if grep -q "^$key=" "$ENVPATH" 2>/dev/null; then
    sed -i.bak "s|^$key=.*|$key=$(esc_val "$val")|" "$ENVPATH" && rm -f "$ENVPATH.bak"
  else
    printf '%s=%s\n' "$key" "$val" >> "$ENVPATH"
  fi
}

get_env() {
  sed -n "s|^$1=||p" "$ENVPATH" 2>/dev/null | head -1
}

# Old .env.example copies shipped AUTH_TOKENS=cb_xxx,cb_yyy. A placeholder is
# never a valid token: the proxy starts fine and /healthz is 200, but every chat
# fails with 502 wrapping "Invalid API key or user not found" - one of the most
# common "my setup does not work" reports.
is_placeholder_token() {
  case "$1" in
    cb_xxx*|*cb_yyy*|tok1*|changeme*|your-token*|your_token*|"<"*|*xxx,*|*xxx) return 0 ;;
    *) return 1 ;;
  esac
}

# token (obtained in step 1) / bridge mode
if [ "$NO_ENV" = "0" ]; then
  CURRENT_TOKEN="$(get_env AUTH_TOKENS)"
  if [ "$BRIDGE" = "1" ]; then
    set_env "AUTH_TOKENS" ""
    ok "AUTH_TOKENS left empty (bridge mode: clients send their own token)"
  elif [ -n "$TOKEN_VALUE" ]; then
    set_env "AUTH_TOKENS" "$TOKEN_VALUE"
    ok "AUTH_TOKENS written to $ENVPATH (value hidden, source: $TOKEN_SOURCE)"
  elif [ -n "$CURRENT_TOKEN" ] && is_placeholder_token "$CURRENT_TOKEN"; then
    warn "$ENVPATH contains a PLACEHOLDER token, not a real one."
    warn "Left as-is it would start fine but fail every chat with 'Invalid API key'."
    set_env "AUTH_TOKENS" ""
    warn "Cleared it (empty = bridge mode, which fails loudly with a clear 401 instead)."
    warn "Run 'freebuff' to log in, then re-run this script to fill it in."
  elif [ -n "$CURRENT_TOKEN" ]; then
    ok "AUTH_TOKENS already set in $ENVPATH, keeping it"
  elif [ "$SKIP_TOKEN" = "0" ]; then
    warn "AUTH_TOKENS not set - run 'freebuff' to log in, then re-run this script or edit $ENVPATH"
  fi
fi

# containers must not bind loopback only (compose sets this too; plain
# `docker run --env-file .env` relies on the value being in the file)
if [ "$METHOD" = "docker" ] && [ "$NO_ENV" = "0" ]; then
  set_env "LISTEN_ADDR" ":3457"
  ok "LISTEN_ADDR=:3457 (a container binding 127.0.0.1 is unreachable from the host)"
fi

# account-safety knobs
if [ "$NO_ENV" = "0" ]; then
  if [ "$NO_PROMPT" = "1" ]; then
    KNOB_MAX="0"; KNOB_IDLE="30m"
  else
    echo ""
    c "Account-safety knobs (recommended to keep your account alive):"
    ask KNOB_MAX "Max messages per token per 24h (0 = unlimited / recommended, 0-spam safe)" "0"
    ask KNOB_IDLE "Pause background work after idle (e.g. 30m, 0 = never)" "30m"
  fi
  [ -n "$KNOB_MAX" ] && { set_env "MAX_MESSAGES_PER_DAY" "$KNOB_MAX"; ok "MAX_MESSAGES_PER_DAY=$KNOB_MAX"; }
  [ -n "$KNOB_IDLE" ] && { set_env "IDLE_ROTATION_TIMEOUT" "$KNOB_IDLE"; ok "IDLE_ROTATION_TIMEOUT=$KNOB_IDLE"; }
fi

# --- 7. start (docker) --------------------------------------------------------
if [ "$METHOD" = "docker" ]; then
  cd "$REPO_DIR"
  if [ ! -f .env ] && [ -f "$ENVPATH" ]; then cp "$ENVPATH" .env; fi
  echo ""
  c "Building the proxy image (this takes a minute the first time)..."
  docker compose up --build -d
  c "Waiting for the container to become healthy..."
  for i in $(seq 1 30); do
    STATUS="$(docker compose ps --format '{{.Status}}' 2>/dev/null | head -1)"
    case "$STATUS" in
      *healthy*) ok "Container healthy: $STATUS"; break ;;
      *) sleep 2 ;;
    esac
    [ "$i" = "30" ] && { echo "ERROR: container not healthy after 60s" >&2; docker compose logs --tail 20; exit 1; }
  done
  BRIDGE_GW="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || echo 172.17.0.1)"
  GATEWAY=""
  for net in $(docker network ls --format '{{.Name}}' | grep -i freebuff | head -3); do
    GATEWAY="$(docker network inspect "$net" --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true)"
    [ -n "$GATEWAY" ] && break
  done
  [ -z "$GATEWAY" ] && GATEWAY="$BRIDGE_GW"

  echo ""
  echo "============================================================"
  echo "  9router -> freebuff-proxy - fill the 'Add OpenAI Compatible'"
  echo "  form with these values (Dashboard -> Providers -> Add)"
  echo "============================================================"
  echo ""
  echo "  Name          : freebuff"
  echo "  Prefix        : freebuff"
  echo "  API Type      : Chat Completions          (NOT Responses API)"
  echo "  API Key       : any non-empty value"
  echo "  Model ID      : (leave empty - the proxy has /v1/models)"
  if [ "$BRIDGE" = "1" ]; then
    echo "  Bridge mode   : API Key = YOUR FreeBuff token (sent upstream as-is)"
  fi
  echo ""
  echo "  Base URL - pick ONE:"
  echo "    A) 9router as a process on this host: http://127.0.0.1:3457/v1"
  echo "    B) 9router in a container on this host: http://${GATEWAY}:3457/v1"
  echo "============================================================"
fi

# --- 8. next steps & doctor check -------------------------------------------
echo ""
ok "Installation complete! Config: $ENVPATH"
echo ""

if [ -n "${BIN:-}" ] && [ -x "$BIN" ]; then
  c "Running self-diagnostic doctor..."
  "$BIN" -doctor || true
  echo ""
fi

c "Next steps:"
if [ "$METHOD" = "docker" ]; then
  echo "  1. View container status:  cd $REPO_DIR && docker compose ps"
  echo "  2. Follow container logs:  docker compose logs -f"
else
  echo "  1. 1-Click Client Setup:   cd $CONFIG_DIR && ${BIN:-./freebuff-proxy} -setup"
  echo "  2. Start the proxy server: cd $CONFIG_DIR && ${BIN:-./freebuff-proxy}"
fi
echo ""
c "Test the proxy:"
echo "  curl http://localhost:3457/healthz"
echo "  curl http://localhost:3457/v1/models"
echo "  curl -s -X POST http://localhost:3457/v1/chat/completions \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"model\":\"deepseek/deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello\"}],\"stream\":false}'"
echo ""
if [ "$BRIDGE" = "1" ]; then
  c "Bridge mode - send your FreeBuff token in the Authorization header:"
  echo "  curl -s -X POST http://localhost:3457/v1/chat/completions \\"
  echo "    -H 'Authorization: Bearer <your-freebuff-token>' \\"
  echo "    -H 'Content-Type: application/json' \\"
  echo "    -d '{\"model\":\"deepseek/deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello\"}],\"stream\":false}'"
  echo ""
fi
ok "Quick Integration: Point your AI tools (Continue, Cursor, OpenCode, Aider, 9router) to http://localhost:3457/v1"
