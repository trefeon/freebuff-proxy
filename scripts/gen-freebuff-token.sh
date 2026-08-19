#!/usr/bin/env bash
# gen-freebuff-token.sh - Generate a FreeBuff auth token via headless login flow
#
# Usage:
#   ./gen-freebuff-token.sh              # interactive: recommends options; Enter = auto-append to ./.env
#   ./gen-freebuff-token.sh --print      # generate token and print to screen only
#   ./gen-freebuff-token.sh --clipboard  # copy to clipboard (xclip/pbcopy)
#   ./gen-freebuff-token.sh --incognito  # do NOT auto-open the browser: print the login URL and wait
#                                        # for you to open it in a private/incognito window manually
#                                        # (prevents an existing logged-in GitHub session from being reused)
#   ./gen-freebuff-token.sh --save       # save to ~/.config/manicode/credentials.json
#   ./gen-freebuff-token.sh --append     # append to .env AUTH_TOKENS (auto-copies .env.example if missing)
#   ./gen-freebuff-token.sh --env /path/.env  # target .env for --append
#
# Each run generates a unique fingerprintId. Log into a DIFFERENT GitHub
# account in your browser to get a token for that account.
#
# WARNING: Using FreeBuff tokens through a proxy violates FreeBuff/Codebuff
# terms of service. Accounts may be suspended or banned.
set -euo pipefail

BASE_URL="${FREEBUFF_BASE_URL:-https://www.codebuff.com}"
TIMEOUT=300
POLL_INTERVAL=5
MODE="interactive"  # interactive (default) | print | save | clipboard | append | incognito
ENV_FILE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --print)     MODE="print"; shift ;;
    --save)      MODE="save"; shift ;;
    --clipboard) MODE="clipboard"; shift ;;
    --incognito) MODE="incognito"; shift ;;
    --append)    MODE="append"; shift ;;
    --env)       ENV_FILE="$2"; shift 2 ;;
    --env=*)     ENV_FILE="${1#--env=}"; shift ;;
    -h|--help)   head -15 "$0"; exit 0 ;;
    *)           echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

c()    { printf '\033[36m%s\033[0m\n' "$*"; }
ok()   { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
err()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }
gray() { printf '\033[90m%s\033[0m\n' "$*"; }

command -v curl >/dev/null || { err "curl is required"; exit 1; }
command -v jq >/dev/null   || { err "jq is required (apt install jq / brew install jq)"; exit 1; }

# --- 0. warning --------------------------------------------------------------
echo ""
c "FreeBuff Token Generator"
warn "WARNING: Using tokens through a proxy violates FreeBuff ToS."
warn "Accounts may be suspended or banned. You accept this risk."
echo ""

# --- 0.5 mode selection (recommend options for easier usage) -----------------
if [ "$MODE" = "interactive" ]; then
  if [ ! -t 0 ]; then
    # Non-interactive (piped/CI): auto-append to the current .env
    MODE="append"
  else
    c "Recommended options:"
    echo "  [Enter]  Append token to ./.env (auto-copy .env.example if missing)"
    echo "  1)       Copy token to clipboard"
    echo "  2)       Save to ~/.config/manicode/credentials.json"
    echo "  3)       Print token only"
    echo "  4)       Incognito login (no auto-open; use a private window)"
    printf "Choose [Enter]: "
    read -r CHOICE
    case "$CHOICE" in
      ""|append|a) MODE="append" ;;
      1|clipboard|c) MODE="clipboard" ;;
      2|save|s) MODE="save" ;;
      3|print|p) MODE="print" ;;
      4|incognito|i) MODE="incognito" ;;
      *) warn "Unknown choice '$CHOICE'; using recommended (append)."; MODE="append" ;;
    esac
    echo ""
  fi
fi

# --- 1. generate fingerprint + request login URL -----------------------------
# CLI-parity fingerprint: `enhanced-` + SHA-256-sized base64url payload (43
# chars, same charset as the official CLI's calculateEnhancedFingerprint —
# fingerprint.ts). Fresh per run so multiple accounts minted on one machine
# do not share an identifier. Random-only: no hardware correlation, by design.
FINGERPRINT_ID="enhanced-$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
gray "Fingerprint: $FINGERPRINT_ID"

c "Requesting login URL..."
# CLI-parity UA (ai-sdk/openai-compatible/1.0.0/codebuff): the auth surface is
# fingerprintable; a curl/PowerShell default UA reads as a third-party tool.
CODE_RESP=$(curl -sS -X POST "$BASE_URL/api/auth/cli/code" \
  -H "Content-Type: application/json" \
  -H "User-Agent: ai-sdk/openai-compatible/1.0.0/codebuff" \
  -d "{\"fingerprintId\":\"$FINGERPRINT_ID\"}")

LOGIN_URL=$(echo "$CODE_RESP" | jq -r '.loginUrl // empty')
FP_HASH=$(echo "$CODE_RESP" | jq -r '.fingerprintHash // empty')
EXPIRES_AT=$(echo "$CODE_RESP" | jq -r '.expiresAt // empty')

if [ -z "$LOGIN_URL" ]; then
  err "No loginUrl in response. Server may be down."
  echo "$CODE_RESP" | jq . 2>/dev/null || echo "$CODE_RESP"
  exit 1
fi

# --- 2. open browser ---------------------------------------------------------
echo ""
if [ "$MODE" = "incognito" ]; then
  # Issue #43: never auto-open — the default browser may reuse an existing
  # logged-in GitHub session, minting a token for the WRONG account. The user
  # opens the URL in a private/incognito window manually; the longer timeout
  # accounts for the manual step.
  TIMEOUT=600
  c "Incognito mode: open the URL below in a PRIVATE/INCOGNITO window manually."
  gray "URL: $LOGIN_URL"
  echo ""
  warn "  -> Open the URL in a private/incognito window (Ctrl+Shift+N / Cmd+Shift+N)."
  warn "  -> Log in with the GitHub account you want a token for."
  warn "  -> This run waits up to ${TIMEOUT}s for you."
  echo ""
else
  ok "Opening browser for GitHub login..."
  gray "URL: $LOGIN_URL"
  echo ""
  warn "  -> Log in with the GitHub account you want a token for."
  warn "  -> If you want a DIFFERENT account, sign out of GitHub first!"
  echo ""

  # Cross-platform browser open
  if command -v xdg-open >/dev/null 2>&1; then
    xdg-open "$LOGIN_URL" 2>/dev/null &
  elif command -v open >/dev/null 2>&1; then
    open "$LOGIN_URL"
  else
    warn "Cannot open browser automatically. Open this URL manually:"
    echo "  $LOGIN_URL"
  fi
fi

# --- 3. poll for auth completion ---------------------------------------------
c "Waiting for login (timeout: ${TIMEOUT}s)..."
START_TIME=$(date +%s)
ATTEMPTS=0
USER_JSON=""

ENCODED_FP=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$FINGERPRINT_ID'))" 2>/dev/null || echo "$FINGERPRINT_ID")
ENCODED_HASH=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$FP_HASH'))" 2>/dev/null || echo "$FP_HASH")
ENCODED_EXPIRES=$(python3 -c "import urllib.parse; print(urllib.parse.quote('$EXPIRES_AT'))" 2>/dev/null || echo "$EXPIRES_AT")

while true; do
  ELAPSED=$(( $(date +%s) - START_TIME ))
  if [ "$ELAPSED" -ge "$TIMEOUT" ]; then
    err "Login timed out after ${TIMEOUT}s."
    exit 1
  fi

  ATTEMPTS=$((ATTEMPTS + 1))
  sleep "$POLL_INTERVAL"

  STATUS_RESP=$(curl -sS -w "\n%{http_code}" \
    -H "User-Agent: ai-sdk/openai-compatible/1.0.0/codebuff" \
    "$BASE_URL/api/auth/cli/status?fingerprintId=$ENCODED_FP&fingerprintHash=$ENCODED_HASH&expiresAt=$ENCODED_EXPIRES" 2>/dev/null || echo -e "\n000")

  HTTP_CODE=$(echo "$STATUS_RESP" | tail -1)
  BODY=$(echo "$STATUS_RESP" | sed '$d')

  if [ "$HTTP_CODE" = "401" ] || [ "$HTTP_CODE" = "000" ]; then
    gray "  Polling ($ATTEMPTS)... not yet authenticated"
    continue
  fi

  AUTH_TOKEN=$(echo "$BODY" | jq -r '.user.authToken // empty' 2>/dev/null)
  if [ -n "$AUTH_TOKEN" ]; then
    USER_JSON="$BODY"
    break
  fi
  gray "  Polling ($ATTEMPTS)... waiting for browser login"
done

# --- 4. extract token --------------------------------------------------------
USER_NAME=$(echo "$USER_JSON" | jq -r '.user.name // "unknown"')
USER_EMAIL=$(echo "$USER_JSON" | jq -r '.user.email // "unknown"')
USER_ID=$(echo "$USER_JSON" | jq -r '.user.id // "unknown"')

echo ""
ok "Login successful!"
c "  Account: $USER_NAME ($USER_EMAIL)"
echo "  Token:   $AUTH_TOKEN"

# --- 4.5 zero-cost post-auth verification (anti-ban) -------------------------
# Probe /api/v1/freebuff/session WITHOUT x-freebuff-instance-id so no session
# slot is claimed (matches the proxy's -test-token probe). Refuses to save a
# banned/spend-limited account — a fresh token check here beats discovering a
# dead token in a chat after it burned pool cooldowns.
PROBE_RESP=$(curl -sS --max-time 15 \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "User-Agent: ai-sdk/openai-compatible/1.0.0/codebuff" \
  "$BASE_URL/api/v1/freebuff/session" 2>/dev/null || true)
PROBE_STATUS=$(echo "$PROBE_RESP" | jq -r '.status // "unknown"' 2>/dev/null || echo "unknown")
PROBE_TIER=$(echo "$PROBE_RESP" | jq -r '.accessTier // ""' 2>/dev/null || echo "")
PROBE_RISK=$(echo "$PROBE_RESP" | jq -r '.currentRiskScore // ""' 2>/dev/null || echo "")
if [ "$PROBE_STATUS" = "banned" ] || echo "$PROBE_RESP" | grep -qi 'banned'; then
  err "ABORT: this account is BANNED upstream. Refusing to save the token."
  exit 1
fi
if [ "$PROBE_STATUS" = "unknown" ] && [ -z "$PROBE_TIER" ]; then
  warn "Probe response unreadable; continuing without tier confirmation:"
  gray "  $(echo "$PROBE_RESP" | tr -d '\n' | head -c 160)"
else
  c "Account check: status=$PROBE_STATUS tier=${PROBE_TIER:-?} risk=${PROBE_RISK:-?}"
fi

# --- 5. save credentials locally (opt-in with --save) ------------------------
if [ "$MODE" = "save" ]; then
  CONFIG_DIR="$HOME/.config/manicode"
  CRED_PATH="$CONFIG_DIR/credentials.json"
  mkdir -p "$CONFIG_DIR"
  ( umask 077; cat > "$CRED_PATH" <<CRED
{
  "default": {
    "id": "$USER_ID",
    "name": "$USER_NAME",
    "email": "$USER_EMAIL",
    "authToken": "$AUTH_TOKEN",
    "fingerprintId": "$FINGERPRINT_ID",
    "fingerprintHash": "$FP_HASH"
  }
}
CRED
  )
  chmod 600 "$CRED_PATH" 2>/dev/null || true
  gray "  Saved to: $CRED_PATH"
fi

# --- 6. output options -------------------------------------------------------
case "$MODE" in
  incognito)
    # Incognito is a browser-flow mode, not an output mode: after the login
    # completes, behave like the recommended default (append to ./.env).
    MODE="append"
    ;;
esac
case "$MODE" in
  clipboard)
    if command -v pbcopy >/dev/null 2>&1; then
      echo -n "$AUTH_TOKEN" | pbcopy
    elif command -v xclip >/dev/null 2>&1; then
      echo -n "$AUTH_TOKEN" | xclip -selection clipboard
    elif command -v xsel >/dev/null 2>&1; then
      echo -n "$AUTH_TOKEN" | xsel --clipboard
    else
      warn "No clipboard tool found. Token:"
      echo "$AUTH_TOKEN"
    fi
    ok "  Copied to clipboard!"
    ;;
  append)
    TARGET_ENV="${ENV_FILE:-$PWD/.env}"
    # Ensure .env exists: copy .env.example if available, else create empty
    if [ ! -f "$TARGET_ENV" ]; then
      SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
      EXAMPLE=""
      for cand in "$SCRIPT_DIR/.env.example" "$(dirname "$SCRIPT_DIR")/.env.example" "$PWD/.env.example"; do
        if [ -f "$cand" ]; then EXAMPLE="$cand"; break; fi
      done
      if [ -n "$EXAMPLE" ]; then
        cp "$EXAMPLE" "$TARGET_ENV"
        warn "  No .env found; created $TARGET_ENV from $EXAMPLE"
      else
        touch "$TARGET_ENV"
        warn "  No .env found; created empty $TARGET_ENV"
      fi
    fi
    # The proxy treats .env as 0600; floor perms before writing the token in.
    chmod 600 "$TARGET_ENV" 2>/dev/null || true
    if [ -n "$AUTH_TOKEN" ] && grep -qF "$AUTH_TOKEN" "$TARGET_ENV"; then
      gray "  Token already present in $TARGET_ENV; skipped."
    elif grep -q '^AUTH_TOKENS=' "$TARGET_ENV"; then
      EXISTING=$(grep '^AUTH_TOKENS=' "$TARGET_ENV" | head -1 | cut -d= -f2-)
      TMP_ENV="$(mktemp)"
      if [ -n "$EXISTING" ]; then
        sed "s|^AUTH_TOKENS=.*|AUTH_TOKENS=${EXISTING},${AUTH_TOKEN}|" "$TARGET_ENV" > "$TMP_ENV" && mv "$TMP_ENV" "$TARGET_ENV"
      else
        sed "s|^AUTH_TOKENS=.*|AUTH_TOKENS=${AUTH_TOKEN}|" "$TARGET_ENV" > "$TMP_ENV" && mv "$TMP_ENV" "$TARGET_ENV"
      fi
      ok "  Appended to: $TARGET_ENV"
    else
      echo "AUTH_TOKENS=$AUTH_TOKEN" >> "$TARGET_ENV"
      ok "  Appended to: $TARGET_ENV"
    fi
    ;;
esac

echo ""
c "Done! Add this token to your 9router or .env AUTH_TOKENS."
echo ""
