#!/usr/bin/env bash
# check-upstream.sh — detect drift between the pinned upstream registry files
# (backend/internal/registry/testdata/upstream/) and CodebuffAI/freebuff at a ref.
#
# Usage:
#   scripts/check-upstream.sh [--update-wire-baseline] [--group <registry|wire>]
#                             [ref] [clone-dir]
#
#   ref        upstream branch or full commit SHA to compare against
#              (default: main)
#   clone-dir  local clone of https://github.com/CodebuffAI/freebuff
#              (default: $FREEBUFF_REFERENCE_DIR, else <repo>/../freebuff-reference).
#              Missing → shallow-cloned with --depth 50; present → fetched.
#   --group <registry|wire>
#              Check only one file group. Default (no flag) checks both.
#              sync-upstream's post-sync verification passes --group registry
#              so concurrent wire drift cannot fail a registry-only sync;
#              unqualified drift detection still fails on any drift.
#   --update-wire-baseline
#              Refresh scripts/wire-baseline.tsv with the current upstream
#              content hash of every wire file, then exit 0 regardless of
#              drift. Run this by hand after porting/reviewing an upstream
#              wire change so future runs only flag genuinely new drift.
#
# Prints one table row per pinned file: file | pinned-sha | vendor-sha |
# status (SAME/DRIFT/MISSING). Exit codes: 0 all SAME, 1 any DRIFT/MISSING,
# 2 environment error.
#
# Windows: run under Git Bash, e.g.
#   "C:\Program Files\Git\bin\bash.exe" scripts/check-upstream.sh
# Requires only git plus sha256sum (or shasum).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
	printf 'check-upstream: error: %s\n' "$1" >&2
	exit 2
}

UPDATE_BASELINE=0
if [[ "${1:-}" == "--update-wire-baseline" ]]; then
	UPDATE_BASELINE=1
	shift
fi
GROUP_FILTER=""
if [[ "${1:-}" == "--group" ]]; then
	shift
	case "${1:-}" in
		registry|wire) GROUP_FILTER="$1"; shift ;;
		*) die "unknown --group value '${1:-}' (expected 'registry' or 'wire')" ;;
	esac
fi
if ((UPDATE_BASELINE)) && [[ -n "$GROUP_FILTER" && "$GROUP_FILTER" != "wire" ]]; then
	die "--update-wire-baseline writes the wire baseline; cannot combine with --group $GROUP_FILTER"
fi
REF="${1:-main}"
VENDOR_URL="https://github.com/CodebuffAI/freebuff.git"
BASELINE_FILE="$REPO_ROOT/scripts/wire-baseline.tsv"
# Vendor npm wrapper version (freebuff CLI package). The published npm version
# moves in lockstep-ish with the public snapshot; a mismatch against the
# recorded pin (scripts/vendor-version.txt) means upstream shipped a new
# wrapper the sync hasn't recorded yet. Informational — best-effort: npm not
# on PATH is fine, and a mismatch never fails the check (the sync records it).
VENDOR_VERSION_FILE="$REPO_ROOT/scripts/vendor-version.txt"
if [[ -n "${2:-}" ]]; then
	CLONE_DIR="$2"
elif [[ -n "${FREEBUFF_REFERENCE_DIR:-}" ]]; then
	CLONE_DIR="$FREEBUFF_REFERENCE_DIR"
elif [[ -d "$REPO_ROOT/reference/freebuff/.git" ]]; then
	CLONE_DIR="$REPO_ROOT/reference/freebuff"
else
	CLONE_DIR="$REPO_ROOT/../freebuff-reference"
fi
UPSTREAM_PREFIX="common/src/constants"
PINNED_DIR="$REPO_ROOT/backend/internal/registry/testdata/upstream"

# Registry mirror files: pinned into backend/internal/registry/testdata/upstream/ and
# diffed hash-for-hash. Keep in sync with sourceFiles in
# backend/internal/registry/registry.go.
REGISTRY_FILES=(
	free-agents.ts
	freebuff-model-ids.ts
	freebuff-models.ts
	gemini.ts
	model-config.ts
)
# Wire-shape files the proxy reads at runtime but does NOT pin. Drift here
# changes the answer to "what does the upstream wire look like" without
# breaking the registry parity test. The drift workflow still flags them; a
# human applies the change (every Phase 1+ fix in issue #140 used to live
# here: freebuff-standing.ts (renamed from freebuff-trust.ts), foreign-client-signals.ts, prompt-agent-stream.ts,
# tools/constants.ts for cb_easp).
WIRE_FILES=(
	common/src/constants/freebuff-standing.ts
	common/src/constants/foreign-client-signals.ts
	common/src/constants/freebuff-spend-ceilings.ts
	common/src/constants/freebuff-signup-block.ts
	common/src/constants/freebuff-peak-hours.ts
	common/src/util/freebuff-model-availability.ts
	cli/src/components/freebuff-model-selector.tsx
	common/src/types/freebuff-session.ts
	packages/agent-runtime/src/constants.ts
	packages/agent-runtime/src/prompt-agent-stream.ts
	packages/agent-runtime/src/run-agent-step.ts
	packages/agent-runtime/src/run-programmatic-step.ts
	common/src/tools/constants.ts
)

command -v git >/dev/null 2>&1 || die "git not found on PATH"
if command -v sha256sum >/dev/null 2>&1; then
	SHA_CMD=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
	SHA_CMD=(shasum -a 256)
else
	die "need sha256sum or shasum on PATH"
fi

# Hash via STDIN, never by filename: some sha256sum builds (busybox on
# Windows/Git-Bash setups) prefix binary-mode sums with '\' when given a file
# argument. CR is stripped so stale autocrlf working-tree copies compare equal
# to their committed LF form (.gitattributes pins eol=lf).
pin_hash() {
	tr -d '\r' <"$1" | "${SHA_CMD[@]}"
}

if [[ ! -d "$CLONE_DIR/.git" ]]; then
	echo "check-upstream: cloning $VENDOR_URL into $CLONE_DIR (--depth 50)"
	git clone --depth 50 -- "$VENDOR_URL" "$CLONE_DIR"
elif [[ "$REF" =~ ^[0-9a-fA-F]{40}$ ]] && git -C "$CLONE_DIR" cat-file -e "${REF}^{commit}" 2>/dev/null; then
	: # full SHA already present locally — nothing to fetch
else
	git -C "$CLONE_DIR" fetch origin -- "$REF"
fi

# Resolve REF against the fetched upstream state (origin/<branch>), never the
# possibly-stale local checkout inside the clone.
UPSTREAM_SHA="$REF"
if ! [[ "$REF" =~ ^[0-9a-fA-F]{40}$ ]]; then
	UPSTREAM_SHA="$(git -C "$CLONE_DIR" rev-parse --verify "origin/${REF}^{commit}" 2>/dev/null ||
		git -C "$CLONE_DIR" rev-parse --verify "${REF}^{commit}")" ||
		die "cannot resolve ref '$REF' in $CLONE_DIR (fetch failed?)"
fi

echo "check-upstream: comparing pins against CodebuffAI/freebuff @ $UPSTREAM_SHA (ref: $REF)"
echo
printf '%-12s %-64s %-14s %-14s %s\n' GROUP FILE PINNED-SHA VENDOR-SHA STATUS
printf '%-12s %-64s %-14s %-14s %s\n' '------------' '----------------------------------------------------------------' '-------------' '-------------' '------'

drift=0
# JSON accumulator for the drift-detection workflow (machine-readable
# handoff so the next step can create issues/PRs without re-running git).
declare -A WIRE_VENDOR=()
JSON_ENTRIES=()

# Generic per-file check. pinned_file may be empty (WIRE_FILES are unpinned).
# status is one of: SAME, DRIFT, MISSING_UPSTREAM.
#
# For registry files, vendor_sha comes from the local working-tree copy
# (testdata/upstream/). For wire files, vendor_sha comes from the live
# upstream file at the fetched commit. A wire file whose live hash differs
# from the last-reviewed baseline entry (scripts/wire-baseline.tsv) is
# DRIFT — that signal tells the human "upstream moved; re-read the file
# and port the change".
check_file() {
	local group="$1" file="$2" pinned_rel="$3"
	local pinned_file="" vendor_sha="" status="SAME"
	if [[ "$group" == "registry" && -n "$pinned_rel" ]]; then
		pinned_file="$PINNED_DIR/$pinned_rel"
	fi
	local pinned_sha=""
	if [[ -n "$pinned_file" && -f "$pinned_file" ]]; then
		pinned_sha="$(pin_hash "$pinned_file")"
		pinned_sha="${pinned_sha%% *}"
	fi
	if ! git -C "$CLONE_DIR" cat-file -e "$UPSTREAM_SHA:$file" 2>/dev/null; then
		status="MISSING_UPSTREAM"
	else
		vendor_sha="$(git -C "$CLONE_DIR" show "$UPSTREAM_SHA:$file" 2>/dev/null | tr -d '\r' | "${SHA_CMD[@]}")"
		vendor_sha="${vendor_sha%% *}"
		if [[ "$group" == "registry" ]]; then
			if [[ "$pinned_sha" != "$vendor_sha" ]]; then
				status="DRIFT"
			fi
		else
			# Wire files have no pinned copy: compare against the committed
			# baseline of last-reviewed upstream content
			# (scripts/wire-baseline.tsv, one "<sha12> <path>" per line).
			# A baseline entry that differs from the live upstream hash is
			# DRIFT ("the reviewed version changed upstream"); a missing
			# entry means we never recorded this file, so report SAME.
			local base_sha=""
			if [[ -f "$BASELINE_FILE" ]]; then
				while read -r sha path; do
					if [[ "$path" == "$file" ]]; then
						base_sha="$sha"
					fi
				done <"$BASELINE_FILE"
			fi
			if [[ -n "$base_sha" && "$base_sha" != "${vendor_sha:0:12}" ]]; then
				status="DRIFT"
			fi
			WIRE_VENDOR["$file"]="${vendor_sha:0:12}"
		fi
	fi
	if [[ "$status" != "SAME" ]]; then
		drift=1
	fi
	printf '%-12s %-64s %-14s %-14s %s\n' "$group" "$file" "${pinned_sha:0:12}" "${vendor_sha:0:12}" "$status"
	local esc_file="${file//\\/\\\\}"
	esc_file="${esc_file//\"/\\\"}"
	JSON_ENTRIES+=("{\"group\":\"$group\",\"file\":\"$esc_file\",\"pinned_sha\":\"${pinned_sha:0:12}\",\"vendor_sha\":\"${vendor_sha:0:12}\",\"status\":\"$status\"}")
}

if [[ -z "$GROUP_FILTER" || "$GROUP_FILTER" == "registry" ]]; then
	for f in "${REGISTRY_FILES[@]}"; do
		check_file "registry" "$UPSTREAM_PREFIX/$f" "$f"
	done
fi
if [[ -z "$GROUP_FILTER" || "$GROUP_FILTER" == "wire" ]]; then
	for f in "${WIRE_FILES[@]}"; do
		check_file "wire" "$f" ""
	done
fi

# Vendor npm wrapper version (informational; never fails the check).
NPM_VERSION=""
if command -v npm >/dev/null 2>&1; then
	NPM_VERSION="$(npm view freebuff version 2>/dev/null || true)"
fi
PINNED_VERSION=""
if [[ -f "$VENDOR_VERSION_FILE" ]]; then
	PINNED_VERSION="$(tr -d '\r\n' <"$VENDOR_VERSION_FILE")"
fi
if [[ -n "$NPM_VERSION" ]]; then
	if [[ -n "$PINNED_VERSION" && "$NPM_VERSION" != "$PINNED_VERSION" ]]; then
		printf '%-12s %-64s %-14s %-14s %s\n' "npm" "freebuff" "${PINNED_VERSION:-none}" "$NPM_VERSION" "VERSION"
		echo "check-upstream: npm freebuff@$NPM_VERSION != pinned ${PINNED_VERSION:-none} (scripts/vendor-version.txt) — record the bump on next sync"
	else
		printf '%-12s %-64s %-14s %-14s %s\n' "npm" "freebuff" "${PINNED_VERSION:-none}" "$NPM_VERSION" "SAME"
	fi
else
	echo "check-upstream: npm not on PATH — skipping vendor npm version check"
fi

# Emit machine-readable summary for the drift workflow. Path is honored
# by callers (CI sets DRIFT_REPORT; the dashboard loader reads the file from
# the runtime data dir).
DRIFT_REPORT="${DRIFT_REPORT:-$REPO_ROOT/.drift-report.json}"
{
	printf '{\n'
	printf '  "upstream": "%s",\n' "$VENDOR_URL"
	printf '  "upstream_sha": "%s",\n' "$UPSTREAM_SHA"
	printf '  "checked_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf '  "vendor_version": "%s",\n' "$NPM_VERSION"
	printf '  "vendor_version_pinned": "%s",\n' "$PINNED_VERSION"
	printf '  "files": [\n'
	i=0
	for entry in "${JSON_ENTRIES[@]}"; do
		if ((i > 0)); then printf ',\n'; fi
		printf '    %s' "$entry"
		i=$((i+1))
	done
	printf '\n  ]\n}\n'
} >"$DRIFT_REPORT"

echo
echo "report: $DRIFT_REPORT"
if ((UPDATE_BASELINE)); then
	{
		for f in "${WIRE_FILES[@]}"; do
			if [[ -n "${WIRE_VENDOR[$f]:-}" ]]; then
				printf '%s %s\n' "${WIRE_VENDOR[$f]}" "$f"
			fi
		done
	} | sort -k2 >"$BASELINE_FILE"
	echo "check-upstream: wire baseline written to $BASELINE_FILE (${#WIRE_VENDOR[@]} entries)."
	exit 0
fi
if ((drift)); then
	echo "check-upstream: DRIFT detected."
	echo "Registry pins: refresh by running scripts/sync-upstream.sh and updating"
	echo "fallbackAgents/fallbackRootByModel in backend/internal/registry/registry.go until"
	echo "TestFallbackParityWithPinnedUpstream passes."
	echo "Wire files: read the new file, apply the wire-shape change to the Go side"
	echo "(e.g. injectEnvelope, classifyError, parseSessionResponse), and add a test."
	echo
	echo "Recent upstream commits (last 30 on origin/${REF}) — what changed in the batch:"
	git -C "$CLONE_DIR" log --oneline -30 "origin/${REF}" 2>/dev/null || true
	echo
	exit 1
fi
echo "check-upstream: OK — all pins match $VENDOR_URL @ ${UPSTREAM_SHA:0:12}."
