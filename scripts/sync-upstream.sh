#!/usr/bin/env bash
# sync-upstream.sh — fetch CodebuffAI/freebuff changes, update pinned registry
# files (backend/internal/registry/testdata/upstream/), review wire drift,
# extract new CLI/model features, verify hash parity, and run tests.
#
# Usage:
#   scripts/sync-upstream.sh [options] [ref] [clone-dir]
#
# Options:
#   -c, --check               Check drift only; do not modify any files
#   --no-test                 Skip running Go tests after syncing
#   --test-all                Run full test suite (go test ./backend/...) instead of registry only
#   --cli-info                Extract and display all upstream CLI commands, features, and quotas
#   --show-diff               Show git diffstat of all upstream files changed since previous sync
#   --review-wire             Run wire drift review (classify SAME/COMMENT-ONLY/FUNCTIONAL)
#   --update-wire-baseline    Refresh scripts/wire-baseline.tsv with latest wire file hashes
#   -h, --help                Show this help message
#
# Arguments:
#   ref                       Upstream branch or commit SHA (default: main)
#   clone-dir                 Local reference clone directory (default: reference/freebuff,
#                             $FREEBUFF_REFERENCE_DIR, or ../freebuff-reference)
#
# Examples:
#   bash scripts/sync-upstream.sh
#   bash scripts/sync-upstream.sh --check
#   bash scripts/sync-upstream.sh --cli-info
#   bash scripts/sync-upstream.sh --test-all
#   bash scripts/sync-upstream.sh --update-wire-baseline

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR_URL="https://github.com/CodebuffAI/freebuff.git"
UPSTREAM_PREFIX="common/src/constants"
PINNED_DIR="$REPO_ROOT/backend/internal/registry/testdata/upstream"

CHECK_ONLY=0
RUN_TESTS=1
TEST_ALL=0
SHOW_CLI_INFO=0
SHOW_DIFF=0
REVIEW_WIRE=0
UPDATE_WIRE_BASELINE=0
REF="main"
CLONE_DIR=""
REF_SET=""

# Parse flags
while [[ $# -gt 0 ]]; do
	case "$1" in
		-c|--check)
			CHECK_ONLY=1
			shift
			;;
		--no-test)
			RUN_TESTS=0
			shift
			;;
		--test-all)
			TEST_ALL=1
			shift
			;;
		--cli-info)
			SHOW_CLI_INFO=1
			shift
			;;
		--show-diff)
			SHOW_DIFF=1
			shift
			;;
		--review-wire)
			REVIEW_WIRE=1
			shift
			;;
		--update-wire-baseline)
			UPDATE_WIRE_BASELINE=1
			shift
			;;
		-h|--help)
			sed -n '2,/^set -euo/p' "$0" | sed 's/^# \?//' | sed '/^set -euo/d'
			exit 0
			;;
		-*)
			printf 'sync-upstream: unknown option: %s\n' "$1" >&2
			exit 2
			;;
		*)
			if [[ -z "$REF_SET" ]]; then
				REF="$1"
				REF_SET=1
			elif [[ -z "$CLONE_DIR" ]]; then
				CLONE_DIR="$1"
			fi
			shift
			;;
	esac
done

if [[ -z "$CLONE_DIR" ]]; then
	if [[ -n "${FREEBUFF_REFERENCE_DIR:-}" ]]; then
		CLONE_DIR="$FREEBUFF_REFERENCE_DIR"
	elif [[ -d "$REPO_ROOT/reference/freebuff/.git" ]]; then
		CLONE_DIR="$REPO_ROOT/reference/freebuff"
	else
		CLONE_DIR="$REPO_ROOT/../freebuff-reference"
	fi
fi

# Pinned files that the offline model registry mirrors
FILES=(
	free-agents.ts
	freebuff-model-ids.ts
	freebuff-models.ts
	gemini.ts
	model-config.ts
)

die() {
	printf 'sync-upstream: error: %s\n' "$1" >&2
	exit 2
}

command -v git >/dev/null 2>&1 || die "git not found on PATH"
if command -v sha256sum >/dev/null 2>&1; then
	SHA_CMD=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
	SHA_CMD=(shasum -a 256)
else
	die "need sha256sum or shasum on PATH"
fi

pin_hash() {
	tr -d '\r' <"$1" | "${SHA_CMD[@]}"
}

# 1. Fetch / clone upstream repository
echo "==> 1. Checking upstream repository ($VENDOR_URL)"
PREV_UPSTREAM_SHA=""
if [[ -d "$CLONE_DIR/.git" ]]; then
	PREV_UPSTREAM_SHA="$(git -C "$CLONE_DIR" rev-parse HEAD 2>/dev/null || true)"
fi

if [[ ! -d "$CLONE_DIR/.git" ]]; then
	echo "    Cloning into $CLONE_DIR (--depth 50)..."
	git clone --depth 50 -- "$VENDOR_URL" "$CLONE_DIR"
else
	echo "    Fetching '$REF' in $CLONE_DIR..."
	if [[ "$REF" =~ ^[0-9a-fA-F]{40}$ ]] && git -C "$CLONE_DIR" cat-file -e "${REF}^{commit}" 2>/dev/null; then
		: # full SHA already present
	else
		git -C "$CLONE_DIR" fetch origin -- "$REF"
	fi
fi

# Resolve UPSTREAM_SHA
UPSTREAM_SHA="$REF"
if ! [[ "$REF" =~ ^[0-9a-fA-F]{40}$ ]]; then
	UPSTREAM_SHA="$(git -C "$CLONE_DIR" rev-parse --verify "origin/${REF}^{commit}" 2>/dev/null ||
		git -C "$CLONE_DIR" rev-parse --verify "${REF}^{commit}")" ||
		die "cannot resolve ref '$REF' in $CLONE_DIR"
fi

# Fast-forward/sync reference clone to match UPSTREAM_SHA
if [[ -d "$CLONE_DIR" ]] && git -C "$CLONE_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	CURRENT_LOCAL="$(git -C "$CLONE_DIR" rev-parse HEAD 2>/dev/null || echo "")"
	if [[ "$CURRENT_LOCAL" != "$UPSTREAM_SHA" ]]; then
		git -C "$CLONE_DIR" checkout -q "$UPSTREAM_SHA" 2>/dev/null ||
			git -C "$CLONE_DIR" reset -q --hard "$UPSTREAM_SHA" 2>/dev/null || true
	fi
fi

COMMIT_SUMMARY="$(git -C "$CLONE_DIR" log -1 --format="%h - %s (%an, %cr)" "$UPSTREAM_SHA" 2>/dev/null || echo "${UPSTREAM_SHA:0:12}")"
echo "    Target upstream commit: $COMMIT_SUMMARY"

# Print upstream commit history if we updated to a newer commit
if [[ -n "$PREV_UPSTREAM_SHA" && "$PREV_UPSTREAM_SHA" != "$UPSTREAM_SHA" ]]; then
	COMMIT_COUNT="$(git -C "$CLONE_DIR" rev-list --count "${PREV_UPSTREAM_SHA}..${UPSTREAM_SHA}" 2>/dev/null || echo "several")"
	echo "    Upstream progressed by $COMMIT_COUNT commit(s) (${PREV_UPSTREAM_SHA:0:7} -> ${UPSTREAM_SHA:0:7})"
fi
echo

# 1b. Inspect changed files across upstream subtrees
if [[ -n "$PREV_UPSTREAM_SHA" && "$PREV_UPSTREAM_SHA" != "$UPSTREAM_SHA" ]]; then
	echo "==> 1b. Upstream File Changes (${PREV_UPSTREAM_SHA:0:7}..${UPSTREAM_SHA:0:7})"
	CHANGED_FILES="$(git -C "$CLONE_DIR" diff --name-only "$PREV_UPSTREAM_SHA..$UPSTREAM_SHA" 2>/dev/null || true)"
	if [[ -n "$CHANGED_FILES" ]]; then
		echo "    Changed subtrees:"
		for category in \
			"constants:common/src/constants" \
			"types:common/src/types" \
			"cli-commands:cli/src/commands" \
			"cli-ui:cli/src/components" \
			"agent-runtime:packages/agent-runtime"; do
			tag="${category%%:*}"
			prefix="${category#*:}"
			count="$(echo "$CHANGED_FILES" | grep -c "^$prefix" || true)"
			if ((count > 0)); then
				printf '      [%-14s] %d file(s) changed\n' "$tag" "$count"
			fi
		done
		if ((SHOW_DIFF)); then
			echo
			echo "    Full Diffstat:"
			git -C "$CLONE_DIR" diff --stat "$PREV_UPSTREAM_SHA..$UPSTREAM_SHA" | sed 's/^/      /'
		fi
	fi
	echo
fi

# 1c. Extract CLI Info / Upstream Features (Slash commands, model selector, session types)
if ((SHOW_CLI_INFO)) || [[ -n "$PREV_UPSTREAM_SHA" && "$PREV_UPSTREAM_SHA" != "$UPSTREAM_SHA" ]]; then
	echo "==> 1c. Upstream CLI & Wire Feature Snapshot"
	SLASH_CMD_FILE="$CLONE_DIR/cli/src/data/slash-commands.ts"
	if [[ -f "$SLASH_CMD_FILE" ]]; then
		cmd_ids=$(grep -E "^[[:space:]]+id: '[^']+'" "$SLASH_CMD_FILE" | sed "s/.*id: '\([^']*\)'.*/\1/" | tr '\n' ' ')
		echo "    Available CLI slash commands:"
		echo "      $cmd_ids"
	fi

	SESSION_TYPE_FILE="$CLONE_DIR/common/src/types/freebuff-session.ts"
	if [[ -f "$SESSION_TYPE_FILE" ]]; then
		has_freebucks=$(grep -c "FreebuffFreebucks" "$SESSION_TYPE_FILE" || true)
		if ((has_freebucks > 0)); then
			echo "    Quota & Pricing:"
			echo "      Freebucks accounting detected in freebuff-session.ts"
		fi
	fi

	RUNTIME_COMPACT_FILE="$CLONE_DIR/packages/agent-runtime/src/compact-history.ts"
	if [[ -f "$RUNTIME_COMPACT_FILE" ]]; then
		cache_expiry=$(grep "DEFAULT_CACHE_EXPIRY_MS =" "$RUNTIME_COMPACT_FILE" | head -1 | sed 's/^[[:space:]]*//')
		if [[ -n "$cache_expiry" ]]; then
			echo "    Context Pruning:"
			echo "      $cache_expiry"
		fi
	fi
	echo
fi

# 2. Check drift across pinned files
echo "==> 2. Comparing pinned snapshot against upstream"
printf '%-26s %-14s %-14s %s\n' FILE PINNED-SHA VENDOR-SHA STATUS
printf '%-26s %-14s %-14s %s\n' '-------------------------' '-------------' '-------------' '------'

mkdir -p "$PINNED_DIR"
drift_count=0
updated_count=0

for f in "${FILES[@]}"; do
	pinned_file="$PINNED_DIR/$f"
	if [[ ! -f "$pinned_file" ]]; then
		pinned_sha="-"
	else
		pinned_sha=$(pin_hash "$pinned_file" | awk '{print substr($1,1,12)}')
	fi

	vendor_file="$CLONE_DIR/$UPSTREAM_PREFIX/$f"
	if [[ ! -f "$vendor_file" ]]; then
		vendor_sha="-"
		status="MISSING"
		drift_count=$((drift_count + 1))
	else
		vendor_sha=$(pin_hash "$vendor_file" | awk '{print substr($1,1,12)}')
		if [[ "$pinned_sha" == "$vendor_sha" ]]; then
			status="SAME"
		else
			status="DRIFT"
			drift_count=$((drift_count + 1))
			if ((!CHECK_ONLY)); then
				cp "$vendor_file" "$pinned_file"
				pinned_sha="$vendor_sha"
				status="UPDATED"
				updated_count=$((updated_count + 1))
			fi
		fi
	fi
	printf '%-26s %-14s %-14s %s\n' "$f" "$pinned_sha" "$vendor_sha" "$status"
done

echo

# 2b. Vendor npm wrapper version (freebuff CLI package). Best-effort: npm
#     not on PATH is fine; in sync mode the pin auto-updates.
VENDOR_VERSION_FILE="$REPO_ROOT/scripts/vendor-version.txt"
NPM_VERSION=""
if command -v npm >/dev/null 2>&1; then
	NPM_VERSION="$(npm view freebuff version 2>/dev/null || true)"
fi
PINNED_VERSION=""
if [[ -f "$VENDOR_VERSION_FILE" ]]; then
	PINNED_VERSION="$(tr -d '\r\n' <"$VENDOR_VERSION_FILE")"
fi
if [[ -n "$NPM_VERSION" ]]; then
	if [[ "$NPM_VERSION" == "$PINNED_VERSION" ]]; then
		echo "Vendor npm package: $NPM_VERSION (SAME)"
	else
		if ((CHECK_ONLY)); then
			echo "Vendor npm package: $NPM_VERSION (DRIFT from pinned $PINNED_VERSION)"
			drift_count=$((drift_count + 1))
		else
			printf '%s\n' "$NPM_VERSION" >"$VENDOR_VERSION_FILE"
			echo "Vendor npm package: updated $PINNED_VERSION -> $NPM_VERSION"
			updated_count=$((updated_count + 1))
		fi
	fi
else
	echo "Vendor npm package: npm not on PATH — skipping version check (pinned: ${PINNED_VERSION:-unknown})"
fi
echo

# 3. If in check-only mode and drift found, exit
if ((CHECK_ONLY)); then
	if ((drift_count > 0)); then
		echo "sync-upstream: DRIFT detected in $drift_count file(s)/pin(s). Run without --check to synchronize."
		exit 1
	else
		echo "sync-upstream: All pinned files match upstream perfectly."
	fi
else
	if ((updated_count > 0)); then
		echo "==> 3. Updated $updated_count pinned file(s) in backend/internal/registry/testdata/upstream/"
	else
		echo "==> 3. All pinned files are already up-to-date (0 files updated)."
	fi
fi

# 4. Verify parity via check-upstream.sh
CHECK_ARGS=()
if ((!CHECK_ONLY)); then
	CHECK_ARGS=(--group registry)
fi
echo
echo "==> 4. Verifying pin parity..."
if ! bash "$REPO_ROOT/scripts/check-upstream.sh" "${CHECK_ARGS[@]}" "$UPSTREAM_SHA" "$CLONE_DIR"; then
	die "check-upstream.sh reported failure after sync"
fi

# 4b. Wire drift review (optional or on --review-wire)
if ((REVIEW_WIRE)) || ((UPDATE_WIRE_BASELINE)); then
	echo
	echo "==> 4b. Reviewing wire drift..."
	if [[ -f "$REPO_ROOT/scripts/review-wire-drift.sh" ]]; then
		bash "$REPO_ROOT/scripts/review-wire-drift.sh" "$REPO_ROOT/scripts/wire-baseline.tsv" "$UPSTREAM_SHA" || true
	fi
	if ((UPDATE_WIRE_BASELINE)); then
		echo "    Updating wire baseline (scripts/wire-baseline.tsv)..."
		bash "$REPO_ROOT/scripts/check-upstream.sh" --update-wire-baseline "$UPSTREAM_SHA" "$CLONE_DIR"
		echo "    Wire baseline updated successfully."
	fi
fi

# 5. Run tests
if ((RUN_TESTS)); then
	echo
	echo "==> 5. Running test suite..."
	if ! command -v go >/dev/null 2>&1; then
		echo "    WARNING: 'go' binary not found on PATH. Skipping test execution."
	else
		if ((TEST_ALL)); then
			echo "    Executing: env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/..."
			if env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...; then
				echo "    [PASS] Full test suite passed cleanly."
			else
				echo "    [FAIL] Full test suite failed. Review test output above." >&2
				exit 1
			fi
		else
			echo "    Executing: env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/internal/registry/..."
			if env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/internal/registry/...; then
				echo "    [PASS] Registry tests and fallback parity check passed."
			else
				echo "    [FAIL] Registry tests failed. If upstream added/removed models or agents," >&2
				echo "           update fallbackAgents / fallbackRootByModel in backend/internal/registry/registry.go" >&2
				echo "           until TestFallbackParityWithPinnedUpstream passes." >&2
				exit 1
			fi
		fi
	fi
fi

# 6. Show git status summary of changes
echo
echo "==> Upstream Sync Complete!"
if git -C "$REPO_ROOT" diff --quiet backend/internal/registry/testdata/upstream; then
	echo "No working tree changes (pins were already identical to upstream)."
else
	echo "Working tree changes in backend/internal/registry/testdata/upstream/:"
	git -C "$REPO_ROOT" status --short backend/internal/registry/testdata/upstream
	echo
	echo "Suggested commit command:"
	echo "  git add backend/internal/registry/testdata/upstream"
	echo "  git commit -m \"chore(registry): sync pinned upstream models to vendor ${UPSTREAM_SHA:0:7}\""
fi
