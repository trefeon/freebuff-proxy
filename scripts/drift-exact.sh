#!/usr/bin/env bash
# drift-exact.sh — exact export-level drift between two upstream refs.
#
# Usage:
#   scripts/drift-exact.sh [old_ref] [new_ref] [clone-dir]
#
#   old_ref   upstream commit the pins currently record
#             (default: upstream_sha from backend/internal/wirefacts/testdata/wire/snapshots.json)
#   new_ref   upstream ref to compare against (default: origin/main)
#   clone-dir local clone of https://github.com/CodebuffAI/freebuff
#             (default: $FREEBUFF_REFERENCE_DIR, else <repo>/upstream/freebuff)
#
# What "exact" means: each watched file is split into export blocks (one
# `export ...` statement plus its attached doc comment) at both refs.
# Added/removed/changed export names are reported; changed blocks are
# strip-tested (comments and blanks removed) into COMMENT_ONLY vs FUNCTIONAL,
# and only functional hunks are printed. Registry model files get MODEL vs
# PRICE labels from the export name, so the bot announces "price change only"
# instead of "models drifted".
#
# Watch sets mirror check-upstream.sh (registry group + wire baseline); path
# noise (package.json, bun.lock, docs, tests, e2e, assets) is counted as
# ignored, never actionable. Blobs come from `git show`, so CRLF checkouts
# cannot fake drift.
#
# Deps: git, jq. No bun/node. JSON report to $EXACT_REPORT
# (default $REPO_ROOT/.exact-drift.json).
# Exit: 0 nothing actionable, 1 action needed, 2 setup error.
#
# Windows: run under Git Bash like check-upstream.sh.

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd | sed 's|\\|/|g')"
VENDOR_URL="https://github.com/CodebuffAI/freebuff.git"
SNAPSHOTS="$REPO_ROOT/backend/internal/wirefacts/testdata/wire/snapshots.json"
EXACT_REPORT="${EXACT_REPORT:-$REPO_ROOT/.exact-drift.json}"
HUNK_CAP=150

die() { printf 'drift-exact: error: %s\n' "$1" >&2; exit 2; }
command -v git >/dev/null 2>&1 || die "git not found on PATH"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"
command -v awk >/dev/null 2>&1 || die "awk not found on PATH"

# ---- clone ----
if [[ -n "${3:-}" ]]; then CLONE_DIR="$3"
elif [[ -n "${FREEBUFF_REFERENCE_DIR:-}" ]]; then CLONE_DIR="$FREEBUFF_REFERENCE_DIR"
elif [[ -d "$REPO_ROOT/upstream/freebuff/.git" ]]; then CLONE_DIR="$REPO_ROOT/upstream/freebuff"
else CLONE_DIR="$REPO_ROOT/../freebuff-reference"; fi

# Resolve relative to the repo so callers work from any cwd.
if [[ ! "$CLONE_DIR" =~ ^(/|[A-Za-z]:/) ]]; then CLONE_DIR="$REPO_ROOT/$CLONE_DIR"; fi
if [[ ! -d "$CLONE_DIR/.git" ]]; then
	echo "drift-exact: cloning $VENDOR_URL (--depth 500)..." >&2
	git clone --depth 500 -- "$VENDOR_URL" "$CLONE_DIR" || die "clone failed"
fi

# ---- refs ----
OLD_REF="${1:-}"
if [[ -z "$OLD_REF" ]]; then
	[[ -f "$SNAPSHOTS" ]] || die "missing $SNAPSHOTS and no old_ref given"
	OLD_REF="$(grep -o '"upstream_sha"[[:space:]]*:[[:space:]]*"[^"]*"' "$SNAPSHOTS" | head -1 | sed 's/.*"\(.*\)"$/\1/')"
	[[ -n "$OLD_REF" ]] || die "cannot read upstream_sha from $SNAPSHOTS"
fi
NEW_REF="${2:-origin/main}"

resolve() {
	local r="$1" sha=""
	if [[ "$r" =~ ^[0-9a-fA-F]{40}$ ]]; then
		if ! git -C "$CLONE_DIR" cat-file -e "${r}^{commit}" 2>/dev/null; then
			# Shallow clones may predate the pin; deepen before fetching it.
			git -C "$CLONE_DIR" fetch --unshallow 2>/dev/null || true
			git -C "$CLONE_DIR" fetch origin "$r" 2>/dev/null || true
		fi
		sha="$(git -C "$CLONE_DIR" rev-parse --verify "${r}^{commit}" 2>/dev/null || true)"
	else
		git -C "$CLONE_DIR" fetch origin -- "$r" 2>/dev/null || true
		sha="$(git -C "$CLONE_DIR" rev-parse --verify "origin/${r}^{commit}" 2>/dev/null || git -C "$CLONE_DIR" rev-parse --verify "${r}^{commit}" 2>/dev/null || true)"
	fi
	[[ -n "$sha" ]] || die "cannot resolve ref '$r' in $CLONE_DIR (fetch it first)"
	printf '%s' "$sha"
}
OLD_SHA="$(resolve "$OLD_REF")"
NEW_SHA="$(resolve "$NEW_REF")"

# ---- watch sets (mirror check-upstream.sh groups) ----
REGISTRY_FILES=(
	free-agents.ts
	freebuff-model-ids.ts
	freebuff-models.ts
	gemini.ts
	model-config.ts
	freebuff-model-entitlements.ts
)
WIRE_FILES=(
	cli/src/components/freebuff-model-selector.tsx
	common/src/constants/foreign-client-signals.ts
	common/src/constants/freebuff-peak-hours.ts
	common/src/constants/freebuff-signup-block.ts
	common/src/constants/freebuff-spend-ceilings.ts
	common/src/constants/freebuff-standing.ts
	common/src/tools/constants.ts
	common/src/types/freebuff-session.ts
	common/src/util/freebuff-model-availability.ts
	packages/agent-runtime/src/constants.ts
	packages/agent-runtime/src/prompt-agent-stream.ts
	packages/agent-runtime/src/run-agent-step.ts
	packages/agent-runtime/src/run-programmatic-step.ts
)

group_of() {
	local p="$1" f
	for f in "${REGISTRY_FILES[@]}"; do
		[[ "$p" == "common/src/constants/$f" ]] && { printf 'registry'; return; }
	done
	for f in "${WIRE_FILES[@]}"; do
		[[ "$p" == "$f" ]] && { printf 'wire'; return; }
	done
	printf 'unwatched'
}

is_noise() {
	local p="$1"
	[[ "$p" == "package.json" || "$p" == "bun.lock" ]] && return 0
	[[ "$p" == *.md ]] && return 0
	[[ "$p" == *.test.ts || "$p" == *.test.tsx ]] && return 0
	[[ "$p" == *__tests__* || "$p" == */test/* || "$p" == e2e/* || "$p" == */e2e/* || "$p" == docs/* || "$p" == assets/* ]] && return 0
	return 1
}
# Reads file content on stdin, writes one file per export block into $1 and
# the ordered block names to $1/MANIFEST. A block is the `export ...`
# statement plus its attached leading comments; lines before the first export
# form __header__.
split_blocks() {
	awk -v out="$1" '
	function fname(k) { gsub(/[^A-Za-z0-9_#+.,=-]/, "_", k); return k }
	function flush() {
		if (buf == "" && name == "") return
		if (name == "") name = "__header__"
		key = name
		if (key in seen) { seen[key]++; key = key "#" seen[key] } else seen[key] = 1
		print buf > (out "/" fname(key))
		order[++n] = key
	}
	BEGIN { buf = ""; name = ""; n = 0 }
	/^export / {
		flush()
		buf = $0 "\n"
		line = $0
		sub(/^export[ \t]+(default[ \t]+)?(async[ \t]+)?/, "", line)
		if (line ~ /^\{/) name = "reexport:" line
		else {
			nw = split(line, w, /[^A-Za-z0-9_]+/)
			if (w[1] ~ /^(const|let|var|function|class|interface|type|enum|abstract|declare|async)$/ && nw >= 2) name = w[2]
			else if (nw >= 1 && w[1] != "") name = w[1]
			else name = ("exportline:" NR)
		}
		next
	}
	{ buf = buf $0 "\n" }
	END { flush(); for (i = 1; i <= n; i++) print order[i] }
	' >"$1/MANIFEST"
}

# COMMENT_ONLY when the block diff strips to nothing (same rule as
# review-wire-drift.sh: drop +/- markers, file headers, comment/blank lines).
kind_of() {
	if diff "$1" "$2" 2>/dev/null | grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' | grep -vE '^[+-][[:space:]]*(/\*|\*|\*/|//|$)' | grep -q .; then
		printf 'functional'
	else
		printf 'comment'
	fi
}

# MODEL vs PRICE vs WIRE label from the export name (heuristic, documented).
label_of() {
	case "$2" in
	*MODEL_ID* | *MODELS* | *MODEL_IDS* | *AGENT* | *ENTITLEMENT* | *REWARD* | *PAUSED* | *SUPPORTED* | *LIMITED*) printf 'MODEL' ;;
	*PRICE* | *CAP* | *SPEND* | *CEILING* | *POOL* | *STIPEND* | *COST*) printf 'PRICE' ;;
	*) [[ "$1" == "wire" ]] && printf 'WIRE' || printf 'OTHER' ;;
	esac
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "drift-exact: $OLD_SHA -> $NEW_SHA"
FILES_JSON=()
ANNOUNCE=()
IGNORED=()
functional_files=0
comment_files=0

if [[ "$OLD_SHA" == "$NEW_SHA" ]]; then
	echo "drift-exact: refs identical, nothing to compare"
else
	while IFS=$'\t' read -r status path; do
		[[ -z "$path" ]] && continue
		group="$(group_of "$path")"
		if [[ "$group" == "unwatched" ]]; then
			is_noise "$path" && IGNORED+=("$path")
			continue
		fi
		added=() removed=() changed_c=() changed_f=() hunks=""
		if [[ "$status" == "A" ]]; then
			added+=("(whole file)")
			fstatus="FUNCTIONAL"
		elif [[ "$status" == "D" ]]; then
			removed+=("(whole file)")
			fstatus="FUNCTIONAL"
		else
			mkdir -p "$TMP/old" "$TMP/new"
			rm -f "$TMP"/old/* "$TMP"/new/*
			git -C "$CLONE_DIR" show "$OLD_SHA:$path" >"$TMP/full_old" 2>/dev/null || die "no $path at $OLD_SHA"
			git -C "$CLONE_DIR" show "$NEW_SHA:$path" >"$TMP/full_new" 2>/dev/null || die "no $path at $NEW_SHA"
			split_blocks "$TMP/old" <"$TMP/full_old"
			split_blocks "$TMP/new" <"$TMP/full_new"
			while IFS= read -r b; do
				[[ -z "$b" ]] && continue
				fb="$(printf '%s' "$b" | sed 's/[^A-Za-z0-9_#+.,=-]/_/g')"
				if [[ ! -f "$TMP/new/$fb" ]]; then
					removed+=("$b")
				elif cmp -s "$TMP/old/$fb" "$TMP/new/$fb"; then
					:
				elif [[ "$(kind_of "$TMP/old/$fb" "$TMP/new/$fb")" == "comment" ]]; then
					changed_c+=("$b")
				else
					changed_f+=("$b")
					h="$(diff -U3 --label "a/$b" --label "b/$b" "$TMP/old/$fb" "$TMP/new/$fb" || true)"
					hunks+="$h"$'\n'
				fi
			done <"$TMP/old/MANIFEST"
			while IFS= read -r b; do
				[[ -z "$b" ]] && continue
				fb="$(printf '%s' "$b" | sed 's/[^A-Za-z0-9_#+.,=-]/_/g')"
				[[ -f "$TMP/old/$fb" ]] || added+=("$b")
			done <"$TMP/new/MANIFEST"
			# Reads of the same sanitized filename from both dirs: names are
			# unique per manifest (split_blocks dedupes with #2), so a shared
			# filename means the same block.
			if ((${#added[@]} + ${#removed[@]} + ${#changed_f[@]} == 0)); then
				if ((${#changed_c[@]} == 0)); then fstatus="SAME"; else fstatus="COMMENT_ONLY"; fi
			else
				fstatus="FUNCTIONAL"
			fi
		fi
		case "$fstatus" in
		FUNCTIONAL) functional_files=$((functional_files + 1)) ;;
		COMMENT_ONLY) comment_files=$((comment_files + 1)) ;;
		esac
		for b in ${added[@]+"${added[@]}"}; do ANNOUNCE+=("$(label_of "$group" "$b") $path: +$b (added)"); done
		for b in ${removed[@]+"${removed[@]}"}; do ANNOUNCE+=("$(label_of "$group" "$b") $path: -$b (removed)"); done
		for b in ${changed_f[@]+"${changed_f[@]}"}; do ANNOUNCE+=("$(label_of "$group" "$b") $path: ~$b"); done
		if ((${#changed_c[@]} > 8)); then ANNOUNCE+=("DOC $path: ${#changed_c[@]} comment-only blocks"); else for b in ${changed_c[@]+"${changed_c[@]}"}; do ANNOUNCE+=("DOC $path: ~$b (comment-only)"); done; fi
		hunk_lines="$(printf '%s' "$hunks" | wc -l)"
		if ((hunk_lines > HUNK_CAP)); then
			hunks="$(printf '%s' "$hunks" | head -$HUNK_CAP)"$'\n…(hunks truncated at '"$HUNK_CAP"' lines)'
		fi
		FILES_JSON+=("$(jq -n --arg path "$path" --arg group "$group" --arg status "$fstatus" \
			--argjson added "$(printf '%s\n' ${added[@]+"${added[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson removed "$(printf '%s\n' ${removed[@]+"${removed[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson changed_functional "$(printf '%s\n' ${changed_f[@]+"${changed_f[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson changed_comment "$(printf '%s\n' ${changed_c[@]+"${changed_c[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--arg hunks "$hunks" \
			'{path:$path,group:$group,status:$status,added:$added,removed:$removed,changed_functional:$changed_functional,changed_comment:$changed_comment,hunks:$hunks}')")
	done < <(git -C "$CLONE_DIR" diff --name-status "$OLD_SHA" "$NEW_SHA" -- || die "diff failed")
fi

action="false"
((functional_files > 0)) && action="true"
jq -n --arg old "$OLD_SHA" --arg new "$NEW_SHA" \
	--argjson files "$(printf '%s\n' ${FILES_JSON[@]+"${FILES_JSON[@]}"} | jq -s .)" \
	--argjson ignored "$(printf '%s\n' ${IGNORED[@]+"${IGNORED[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	--argjson announce "$(printf '%s\n' ${ANNOUNCE[@]+"${ANNOUNCE[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	--argjson functional_files "$functional_files" --argjson comment_files "$comment_files" \
	--argjson action_needed "$action" \
	'{old_sha:$old,new_sha:$new,checked_at:(now|todate),files:$files,ignored_paths:$ignored,announce:$announce,
    summary:{functional_files:$functional_files,comment_only_files:$comment_files,action_needed:$action_needed}}' >"$EXACT_REPORT"

echo "report: $EXACT_REPORT"
echo "functional_files=$functional_files comment_only_files=$comment_files action_needed=$action"
if ((${#ANNOUNCE[@]})); then printf '  - %s\n' "${ANNOUNCE[@]}"; fi
if [[ "$action" == "true" ]]; then exit 1; else exit 0; fi
