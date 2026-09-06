#!/usr/bin/env bash
# Fast dev deploy to acerblue-local. Skips ALL gates (svelte-check, e2e,
# prettier, go vet/test, PR, CI) — full validation runs once before the real
# PR. ACERBLUE ONLY. Never point this at vps-sg (prod).
#
# How it ships: vite build locally (dist is embedded in the binary), then a
# tarball of CHANGED files only (no commits, no push) piped over ssh. Secrets
# can never ride along: .env*, *.db and session state are filtered even if
# present in the worktree.
#
# Usage:
#   bash scripts/dev-acer.sh            # build + ship + swap + verify
#   DRY_RUN=1 bash scripts/dev-acer.sh  # show what would transfer, ship nothing
set -euo pipefail
cd "$(dirname "$0")/.."

HOST="${ACER_HOST:-acerblue-local}"
REMOTE_DIR="${ACER_DIR:-~/freebuff-proxy}"
BASE="$(git rev-parse --short HEAD)"
VERSION="${BASE}-dev"

echo "[1/4] vite build (dist is embedded, ~10s)"
npm --prefix frontend run build >/dev/null 2>&1
echo "        dist fresh"

echo "[2/4] collecting changed files (tracked-modified + untracked, secrets filtered)"
FILES="$(git status --porcelain | awk '{print $2}' | grep -v -E '^\.env|.*\.db$|session-state|test-results|^reference/|^devdocs/' || true)"
if [ -z "$FILES" ]; then
  echo "        tree clean vs $BASE — shipping dist rebuild only"
else
  echo "$FILES" | sed 's/^/        /'
fi

if [ "${DRY_RUN:-0}" = "1" ]; then
  echo "[dry-run] would build $VERSION from the files above; shipping nothing"
  exit 0
fi

if [ -z "$FILES" ]; then
  echo "[3/4] nothing changed — skipping transfer and build"
else
  echo "[3/4] transfer + docker build on $HOST"
  # NOTE: $REMOTE_DIR stays unquoted on the remote side so a leading ~ expands.
  # pipefail on both ends: a failed build must never reach the swap below.
  echo "$FILES" | tar -cz -C . -T - | ssh -o ConnectTimeout=10 "$HOST" 'set -o pipefail; mkdir -p '"$REMOTE_DIR"' && tar -xz -C '"$REMOTE_DIR"' && docker build --network=host --build-arg VERSION='"$VERSION"' -t freebuff-proxy:ship '"$REMOTE_DIR"' 2>&1 | tail -n 1'
fi

echo "[4/4] swap + verify"
ssh -o ConnectTimeout=10 "$HOST" 'docker tag freebuff-proxy:ship freebuff-proxy:latest && docker compose -f '"$REMOTE_DIR"'/docker-compose.yml up -d 2>&1 | tail -n 1 && sleep 20 && curl -s -o /dev/null -w "ACER:%{http_code} " http://127.0.0.1:3457/healthz; docker logs freebuff-proxy --since 3m 2>&1 | grep -o "\"version\":\"[^\"]*\"" | head -1'
echo "done ($VERSION live on acerblue)"
