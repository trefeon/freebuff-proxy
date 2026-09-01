#!/usr/bin/env bash
# Classify upstream wire-file drift so a needs-port issue says what actually
# changed: COMMENT-ONLY drift needs a baseline refresh, FUNCTIONAL drift needs
# a Go-side port. Pairs with check-upstream.sh, which reports DRIFT but not
# what kind.
#
# Usage:
#   scripts/review-wire-drift.sh [baseline.tsv] [end_ref]
#
# Environment:
#   FREEBUFF_REFERENCE_DIR  upstream clone (default reference/freebuff);
#                           must have the end_ref fetched
#   FREEBUFF_REVIEW_END_REF explicit ref/SHA to classify against (default
#                           origin/main); a positional end_ref wins
#
# Per wire file in the baseline TSV (hash<TAB>path):
#   SAME              current origin/main content hashes to the baseline
#   COMMENT-ONLY      diff from the baseline-state commit strips to nothing
#                     (comments/docs only) — refresh the baseline, no port
#   FUNCTIONAL        the stripped diff is non-empty — needs a port; the
#                     stripped diff is printed below the line
#   UNKNOWN-BASELINE  no commit in the recent history of the file hashes to
#                     the baseline (baseline predates the fetched history —
#                     deepen the clone before classifying)
#
# Exit codes: 0 = nothing FUNCTIONAL/UNKNOWN, 1 = at least one, 2 = setup error.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASELINE_FILE="${1:-$REPO_ROOT/scripts/wire-baseline.tsv}"
CLONE_DIR="${FREEBUFF_REFERENCE_DIR:-$REPO_ROOT/reference/freebuff}"

[[ -f "$BASELINE_FILE" ]] || { echo "baseline file not found: $BASELINE_FILE" >&2; exit 2; }
if [[ -n "${2:-}" ]]; then
	END_REF="$2"
elif [[ -n "${FREEBUFF_REVIEW_END_REF:-}" ]]; then
	END_REF="$FREEBUFF_REVIEW_END_REF"
else
	END_REF="origin/main"
fi
if ! git -C "$CLONE_DIR" rev-parse --verify "$END_REF" >/dev/null 2>&1; then
	echo "$CLONE_DIR has no $END_REF — fetch the upstream clone first" >&2; exit 2
fi
END_REF="$(git -C "$CLONE_DIR" rev-parse "$END_REF")"

rc=0
while read -r baseline path; do
	[[ -z "$baseline" || "$path" == "" ]] && continue
	case "$baseline" in '#'*) continue ;; esac

	current="$(git -C "$CLONE_DIR" show "$END_REF:$path" | tr -d '\r' | sha256sum | cut -c1-12)"
	if [[ "$current" == "$baseline" ]]; then
		echo "SAME $path"
		continue
	fi

	anchor=""
	for c in $(git -C "$CLONE_DIR" log --format=%h -30 "$END_REF" -- "$path"); do
		h="$(git -C "$CLONE_DIR" show "$c:$path" | tr -d '\r' | sha256sum | cut -c1-12)"
		if [[ "$h" == "$baseline" ]]; then anchor="$c"; break; fi
	done
	if [[ -z "$anchor" ]]; then
		echo "UNKNOWN-BASELINE $path (baseline $baseline matches no commit in the last 30 touching the file)"
		rc=1
		continue
	fi

	stripped="$(git -C "$CLONE_DIR" diff "$anchor..$END_REF" -- "$path" \
		| grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' \
		| grep -vE '^[+-][[:space:]]*(/\*|\*|\*/|//|$)' || true)"
	if [[ -z "$stripped" ]]; then
		echo "COMMENT-ONLY $path (baseline anchor $anchor) — refresh baseline, no port"
	else
		echo "FUNCTIONAL $path (baseline anchor $anchor):"
		echo "$stripped"
		rc=1
	fi
done <"$BASELINE_FILE"
exit $rc
