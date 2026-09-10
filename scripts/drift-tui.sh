#!/usr/bin/env bash
# drift-tui.sh — picker wireframe drift between two upstream refs.
#
# Usage:
#   scripts/drift-tui.sh [old_ref] [new_ref] [clone-dir]
#
#   old_ref   upstream commit the pins currently record
#             (default: upstream_sha from backend/internal/wirefacts/testdata/wire/snapshots.json)
#   new_ref   upstream ref to compare against (default: origin/main)
#   clone-dir local clone of https://github.com/CodebuffAI/freebuff
#             (default: $FREEBUFF_REFERENCE_DIR, else <repo>/upstream/freebuff)
#
# What it extracts: the static picker wireframe per access tier, mirrored from
# getFreebuffModelsForAccessTier plus the CLI reward append:
#   full          = FREEBUFF_MODELS in catalog order (+ reward marker)
#   limited       = LIMITED_FREEBUFF_MODEL_IDS resolved over SUPPORTED rows
#   limited_paid  = limited + plan rows (FREEBUFF_PLAN_METERED_CATALOG_MODEL_IDS)
# Per row: id, displayName, tagline, premium, availability, reasoningEffort,
# multimodal (Images badge), isNew (NEW badge), experimental, warning.
# Conditional entries (MIMO UI flag, Solar entitlement gate) evaluate true,
# the deployed state, and are noted in the report.
#
# Hard boundary: Freebucks/hr PRICES are server-driven
# (freebucks.prices[modelId] in the live session response) and cannot be read
# from source. This script detects row membership, order, hero, reward, and
# display-field changes; price moves arrive via the registry sync
# (sync-upstream.sh), which mirrors the wire `prices` map.
#
# Repo refresh checklist (printed with every actionable change):
#   rows/order/hero/reward/premium -> sync-upstream.sh + wiregen + dashboard copy
#   display/tagline/warning/badge copy -> dashboard static copy
#
# Deps: git, jq, awk. No bun/node. JSON report to $TUI_REPORT
# (default $REPO_ROOT/.tui-drift.json).
# Exit: 0 nothing actionable, 1 action needed, 2 setup error.
#
# Windows: run under Git Bash like check-upstream.sh.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd | sed 's|\\|/|g')"
VENDOR_URL="https://github.com/CodebuffAI/freebuff.git"
SNAPSHOTS="$REPO_ROOT/backend/internal/wirefacts/testdata/wire/snapshots.json"
TUI_REPORT="${TUI_REPORT:-$REPO_ROOT/.tui-drift.json}"
MODELS_PATH="common/src/constants/freebuff-models.ts"
ENT_PATH="common/src/constants/freebuff-model-entitlements.ts"
IDS_PATH="common/src/constants/freebuff-model-ids.ts"
CONFIG_PATH="common/src/constants/model-config.ts"

die() { printf 'drift-tui: error: %s\n' "$1" >&2; exit 2; }
command -v git >/dev/null 2>&1 || die "git not found on PATH"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"
command -v awk >/dev/null 2>&1 || die "awk not found on PATH"

if [[ -n "${3:-}" ]]; then CLONE_DIR="$3"
elif [[ -n "${FREEBUFF_REFERENCE_DIR:-}" ]]; then CLONE_DIR="$FREEBUFF_REFERENCE_DIR"
elif [[ -d "$REPO_ROOT/upstream/freebuff/.git" ]]; then CLONE_DIR="$REPO_ROOT/upstream/freebuff"
else CLONE_DIR="$REPO_ROOT/../freebuff-reference"; fi
if [[ ! "$CLONE_DIR" =~ ^(/|[A-Za-z]:/) ]]; then CLONE_DIR="$REPO_ROOT/$CLONE_DIR"; fi

if [[ ! -d "$CLONE_DIR/.git" ]]; then
	echo "drift-tui: cloning $VENDOR_URL (--depth 500)..." >&2
	git clone --depth 500 -- "$VENDOR_URL" "$CLONE_DIR" || die "clone failed"
fi

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

# ---- wireframe extractor (awk): vendor source -> picker JSON ----
# Quote-free throughout: Q holds a single quote so no pattern or replacement
# ever embeds a literal quote character.
wireframe() {
	local ref="$1"
	{
		echo "###ENTITLEMENTS###"
		git -C "$CLONE_DIR" show "$ref:$ENT_PATH" || die "no $ENT_PATH at $ref"
		echo "###IDS###"
		git -C "$CLONE_DIR" show "$ref:$IDS_PATH" || die "no $IDS_PATH at $ref"
		echo "###CONFIG###"
		git -C "$CLONE_DIR" show "$ref:$CONFIG_PATH" || die "no $CONFIG_PATH at $ref"
		echo "###MODELS###"
		git -C "$CLONE_DIR" show "$ref:$MODELS_PATH" || die "no $MODELS_PATH at $ref"
	} | awk -v ref="$ref" '
	function jstr(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
	function unq(v) {
		if (substr(v, 1, 1) == Q) { v = substr(v, 2); q = index(v, Q); if (q > 0) v = substr(v, 1, q - 1) }
		return v
	}
	function resolve_id(t,   m, en) {
		if (substr(t, 1, 1) == Q) return unq(t)
		if (t in ids) return ids[t]
		if (t ~ /^mimoModels\./) { m = t; sub(/^mimoModels\./, "", m); if (m in mimo) return mimo[m] }
		if (t ~ /\.modelId$/) { en = t; sub(/\.modelId$/, "", en); if (en in entid) return entid[en] }
		return "ref:" t
	}
	function resolve_row_id(key) {
		if (!(key in rowid)) return "ref:" key
		return resolve_id(rowid[key])
	}
	function rowjson(key,   id, sec) {
		id = resolve_row_id(key)
		sec = (rowf[key, "premium"] == "true") ? "PREMIUM" : ((rowf[key, "premium"] == "false") ? "UNLIMITED" : "UNKNOWN")
		return "{\"key\":" jstr(key) ",\"id\":" jstr(id) \
			",\"display\":" jstr(rowf[key, "display"]) \
			",\"tagline\":" jstr(rowf[key, "tagline"]) \
			",\"premium\":" jstr(rowf[key, "premium"]) \
			",\"section\":" jstr(sec) \
			",\"availability\":" jstr(rowf[key, "availability"]) \
			",\"reasoning\":" jstr(rowf[key, "reasoning"]) \
			",\"images\":" ((rowf[key, "images"] == "true") ? "true" : "false") \
			",\"new\":" (((key ",isNew") in rowseen) ? "true" : "false") \
			",\"experimental\":" (((key ",experimental") in rowseen) ? "true" : "false") \
			",\"warning\":" jstr(rowf[key, "warning"]) "}"
	}
	BEGIN { Q = sprintf("%c", 39); mode = ""; inrow = ""; inlist = ""; inent = ""; pendref = ""; pendid = "" }
	/^###ENTITLEMENTS###$/ { mode = "ent"; pendid = ""; next }
	/^###IDS###$/ { mode = "ids"; pendid = ""; next }
	/^###CONFIG###$/ { mode = "cfg"; pendid = ""; next }
	/^###MODELS###$/ { mode = "models"; pendid = ""; next }
	mode == "ent" && /^export const [A-Za-z0-9_]+ = \{$/ {
		t = $0; sub(/^export const /, "", t); sub(/ = \{$/, "", t); inent = t; next
	}
	mode == "ent" && inent != "" && /modelId: / {
		t = $0; sub(/^.*modelId: /, "", t); sub(/,[ \t]*$/, "", t); entid[inent] = unq(t); next
	}
	mode == "ent" && inent != "" && /premium: (true|false)/ {
		t = $0; sub(/^.*premium: /, "", t); sub(/[^a-z].*$/, "", t); entprem[inent] = t; next
	}
	mode == "ent" && inent != "" && /limitedAccess: (true|false)/ {
		t = $0; sub(/^.*limitedAccess: /, "", t); sub(/[^a-z].*$/, "", t); entlim[inent] = t; next
	}
	mode == "ent" && /^\}( as const)?;?$/ { inent = ""; next }
	/^export const [A-Z0-9_]+_MODEL_ID =$/ { t = $0; sub(/^export const /, "", t); sub(/ =$/, "", t); pendid = t; next }
	pendid != "" && /^[ \t]*[^ \t#\/\*,}]/ { t = $0; gsub(/[ \t,]/, "", t); ids[pendid] = unq(t); pendid = ""; next }
	mode == "cfg" && /mimoModels = \{$/ { incfg = 1; next }
	mode == "cfg" && incfg && /^\}/ { incfg = ""; next }
	mode == "cfg" && incfg && /: / { t = $0; gsub(/[ \t,]/, "", t); n = split(t, w, /:/); mimo[w[1]] = unq(w[2]); next }
	mode == "models" && /^export const [A-Z0-9_]+_MODEL_ID = / && $0 !~ /(DEFAULT_FREEBUFF_MODEL_ID|LIMITED_FREEBUFF_HERO_MODEL_ID|FALLBACK_FREEBUFF_MODEL_ID|FREEBUFF_REWARD_MODEL_ID) =/ {
		t = $0; sub(/^.*const /, "", t); sub(/ .*$/, "", t); v = $0; sub(/^.*=[ \t]*/, "", v); sub(/,[ \t]*$/, "", v)
		ids[t] = unq(v); next
	}
	mode != "models" { next }
	pendref != "" && /^[A-Za-z0-9_.]+,?$/ {
		t = $0; sub(/,$/, "", t); refs[pendref] = t; pendref = ""; next
	}
	pendref != "" { v = $0; gsub(/[ \t,]/, "", v); refs[pendref] = unq(v); pendref = ""; next }
	/^(export )?const [A-Z0-9_]+_MODEL = \{$/ {
		t = $0; sub(/^(export )?const /, "", t); sub(/ = \{$/, "", t); inrow = t; next
	}
	inrow != "" && /\} as const satisfies FreebuffModelOption/ { inrow = ""; next }
	inrow != "" && /id: [A-Za-z0-9_.]+,?[ \t]*$/ {
		t = $0; sub(/^.*id: /, "", t); sub(/,[ \t]*$/, "", t); rowid[inrow] = t; next
	}
	inrow != "" && /displayName: / {
		t = $0; sub(/^.*displayName: /, "", t); sub(/,[ \t]*$/, "", t)
		if (substr(t, 1, 1) == Q) rowf[inrow, "display"] = unq(t); else rowf[inrow, "display"] = "ref:" t
		next
	}
	inrow != "" && /tagline: / {
		t = $0; sub(/^.*tagline: /, "", t); sub(/,[ \t]*$/, "", t)
		if (substr(t, 1, 1) == Q) rowf[inrow, "tagline"] = unq(t); else rowf[inrow, "tagline"] = "ref:" t
		next
	}
	inrow != "" && /availability: / {
		t = $0; sub(/^.*availability: /, "", t); sub(/,[ \t]*$/, "", t); rowf[inrow, "availability"] = unq(t); next
	}
	inrow != "" && /premium: (true|false)/ {
		t = $0; sub(/^.*premium: /, "", t); sub(/[^a-z].*$/, "", t); rowf[inrow, "premium"] = t; next
	}
	inrow != "" && /premium: / {
		t = $0; sub(/^.*premium: /, "", t); sub(/,[ \t]*$/, "", t)
		if (t ~ /\.fullAccess\.premium$/) { ev = t; sub(/\.fullAccess\.premium$/, "", ev); rowf[inrow, "premium"] = entprem[ev] }
		else rowf[inrow, "premium"] = "ref:" t
		next
	}
	inrow != "" && /multimodal: (true|false)/ {
		t = $0; sub(/^.*multimodal: /, "", t); sub(/[^a-z].*$/, "", t); rowf[inrow, "images"] = t; next
	}
	inrow != "" && /reasoningEffort: / {
		t = $0; sub(/^.*reasoningEffort: /, "", t); sub(/,[ \t]*$/, "", t)
		if (substr(t, 1, 1) == Q) rowf[inrow, "reasoning"] = unq(t); else rowf[inrow, "reasoning"] = "ref:" t
		next
	}
	inrow != "" && /defaultEffort: / && !((inrow ",reasoning") in rowf) {
		t = $0; sub(/^.*defaultEffort: /, "", t); sub(/,[ \t]*$/, "", t); rowf[inrow, "reasoning"] = unq(t); next
	}
	inrow != "" && /isNew: true/ { rowseen[inrow ",isNew"] = 1; next }
	inrow != "" && /experimental: true/ { rowseen[inrow ",experimental"] = 1; next }
	inrow != "" && /warning: / {
		t = $0; sub(/^.*warning: /, "", t); sub(/,[ \t]*$/, "", t)
		if (substr(t, 1, 1) == Q) rowf[inrow, "warning"] = unq(t); else rowf[inrow, "warning"] = "ref:" t
		next
	}
	/^export const (FREEBUFF_MODELS|LIMITED_FREEBUFF_MODEL_IDS|FREEBUFF_PLAN_METERED_CATALOG_MODEL_IDS)[: \[]/ {
		t = $0; sub(/^export const /, "", t); n = split(t, w, /[^A-Za-z0-9_]+/); t = w[1]
		if ($0 ~ /\[$/) inlist = t; else pendlist = t
		next
	}
	pendlist != "" && /Object\.freeze\(\[$/ { inlist = pendlist; pendlist = ""; next }
	inlist != "" && (/^\] as const/ || /^[ \t]*\]\)/) { inlist = ""; pendlist = ""; next }
	inlist != "" && /FREEBUFF_ENABLE_MIMO_MODELS_IN_UI/ {
		lists[inlist] = lists[inlist] "COND:MIMO:MIMO_V25_MODEL|"
		if (inlist == "FREEBUFF_MODELS") notes["mimo"] = "MIMO row gated on FREEBUFF_ENABLE_MIMO_MODELS_IN_UI (evaluated true, deployed state)"
		next
	}
	inlist != "" && /limitedAccess$/ { next }
	inlist != "" && /\? \[/ {
		t = $0; sub(/^.*\? *\[/, "", t); sub(/\].*$/, "", t); gsub(/[ \t,]/, "", t)
		lists[inlist] = lists[inlist] "COND:ENT:" t "|"; next
	}
	inlist != "" && /^[ \t]*[A-Z][A-Za-z0-9_]*,?[ \t]*$/ {
		t = $0; gsub(/[ \t,]/, "", t); lists[inlist] = lists[inlist] t "|"; next
	}
	inlist != "" && /^[ \t]*[^ \t\/\*]+,?[ \t]*$/ {
		t = $0; gsub(/[ \t,]/, "", t); lists[inlist] = lists[inlist] "STR:" unq(t) "|"; next
	}
	/^export const (DEFAULT_FREEBUFF_MODEL_ID|LIMITED_FREEBUFF_HERO_MODEL_ID|FALLBACK_FREEBUFF_MODEL_ID|FREEBUFF_REWARD_MODEL_ID)[: =]/ {
		t = $0; sub(/^export const /, "", t); n = split(t, w, /[^A-Za-z0-9_]+/); name = w[1]
		if ($0 ~ /=[ \t]*$/) pendref = name
		else { v = $0; sub(/^.*=[ \t]*/, "", v); sub(/,[ \t]*$/, "", v); refs[name] = v }
		next
	}
	END {
	for (k in ids) {
		if (ids[k] ~ /\.modelId$/) { en = ids[k]; sub(/\.modelId$/, "", en); if (en in entid) ids[k] = entid[en] }
		else if (ids[k] ~ /^mimoModels\./) { m = ids[k]; sub(/^mimoModels\./, "", m); if (m in mimo) ids[k] = mimo[m] }
	}
		reward = resolve_id(refs["FREEBUFF_REWARD_MODEL_ID"])
		fb = resolve_id(refs["FALLBACK_FREEBUFF_MODEL_ID"])
		herof = resolve_id(refs["DEFAULT_FREEBUFF_MODEL_ID"])
		herol = resolve_id(refs["LIMITED_FREEBUFF_HERO_MODEL_ID"])
		full = ""; n = split(lists["FREEBUFF_MODELS"], e, /\|/)
		for (i = 1; i <= n; i++) {
			if (e[i] ~ /^COND:MIMO:/) { full = full rowjson("MIMO_V25_MODEL") ","; continue }
			if (e[i] == "") continue
			full = full rowjson(e[i]) ","
		}
		sub(/,$/, "", full)
		for (k in rowid) id2row[resolve_row_id(k)] = k
		lim = ""; n = split(lists["LIMITED_FREEBUFF_MODEL_IDS"], e, /\|/)
		for (i = 1; i <= n; i++) {
			if (e[i] ~ /^COND:ENT:/) {
				t = e[i]; sub(/^COND:ENT:/, "", t)
				if (t ~ /\.modelId$/) { en = t; sub(/\.modelId$/, "", en); id = entid[en] }
				else id = resolve_id(t)
				k = id2row[id]
				lim = lim ((k == "") ? "{\"key\":\"COND:" t "\",\"id\":" jstr(id) "}" : rowjson(k)) ","
				continue
			}
			if (e[i] == "") continue
			id = resolve_id(e[i]); k = id2row[id]
			lim = lim ((k == "") ? "{\"key\":\"UNKNOWN:" e[i] "\",\"id\":" jstr(id) "}" : rowjson(k)) ","
		}
		sub(/,$/, "", lim)
		split(lists["FREEBUFF_PLAN_METERED_CATALOG_MODEL_IDS"], pe, /\|/)
		for (i in pe) { if (pe[i] != "") plan[resolve_id(pe[i])] = 1 }
		split(lists["LIMITED_FREEBUFF_MODEL_IDS"], le, /\|/)
		for (i in le) {
			if (le[i] ~ /^COND:ENT:/) { t = le[i]; sub(/^COND:ENT:/, "", t); have[resolve_id(t)] = 1 }
			else if (le[i] != "") have[resolve_id(le[i])] = 1
		}
		lp = lim
		n = split(lists["FREEBUFF_MODELS"], e, /\|/)
		for (i = 1; i <= n; i++) {
			k = e[i]; if (k ~ /^COND/) k = "MIMO_V25_MODEL"
			if (k == "") continue
			id = resolve_row_id(k)
			if ((id in plan) && !(id in have)) { lp = lp "," rowjson(k); have[id] = 1 }
		}
		notelist = ""; for (t in notes) notelist = notelist jstr(notes[t]) ","
		notelist = notelist jstr("prices (Freebucks/hr) are server-driven and not in source")
		printf "{\"ref\":%s,\"reward\":%s,\"fallback\":%s,\"hero\":{\"full\":%s,\"limited\":%s},\"notes\":[%s],\"tiers\":{\"full\":[%s],\"limited\":[%s],\"limited_paid\":[%s]}}\n", \
			jstr(ref), jstr(reward), jstr(fb), jstr(herof), jstr(herol), notelist, full, lim, lp
	}'
}

TMP="$REPO_ROOT/.tmp-drift-tui-$$"
mkdir -p "$TMP" || die "cannot create $TMP"
trap 'rm -rf "$TMP"' EXIT

echo "drift-tui: $OLD_SHA -> $NEW_SHA"
wireframe "$OLD_SHA" >"$TMP/old.json" || die "extract failed at $OLD_SHA"
wireframe "$NEW_SHA" >"$TMP/new.json" || die "extract failed at $NEW_SHA"
jq -e . "$TMP/old.json" >/dev/null || { echo "drift-tui: old wireframe is not JSON"; cat "$TMP/old.json"; exit 2; }
jq -e . "$TMP/new.json" >/dev/null || { echo "drift-tui: new wireframe is not JSON"; cat "$TMP/new.json"; exit 2; }

ANNOUNCE=()
CHECKLIST=()
action=0
for tier in full limited limited_paid; do
	old_ids="$(jq -r ".tiers.$tier[].id" "$TMP/old.json" | tr -d '\r')"
	new_ids="$(jq -r ".tiers.$tier[].id" "$TMP/new.json" | tr -d '\r')"
	while IFS= read -r id; do
		[[ -z "$id" ]] && continue
		echo "$old_ids" | grep -qxF "$id" || {
			disp="$(jq -r ".tiers.$tier[] | select(.id==\"$id\") | .display" "$TMP/new.json" | head -1 | tr -d '\r')"
			ANNOUNCE+=("TUI $tier: +$disp ($id)")
			CHECKLIST+=("registry")
			action=1
		}
	done <<<"$new_ids"
	while IFS= read -r id; do
		[[ -z "$id" ]] && continue
		echo "$new_ids" | grep -qxF "$id" || {
			disp="$(jq -r ".tiers.$tier[] | select(.id==\"$id\") | .display" "$TMP/old.json" | head -1)"
			ANNOUNCE+=("TUI $tier: -$disp ($id)")
			CHECKLIST+=("registry")
			action=1
		}
	done <<<"$old_ids"
	if [[ "$(echo "$old_ids" | sort | md5sum)" == "$(echo "$new_ids" | sort | md5sum)" ]] && [[ "$old_ids" != "$new_ids" ]]; then
		ANNOUNCE+=("TUI $tier: row order changed")
		CHECKLIST+=("dashboard")
		action=1
	fi
	for id in $(echo "$new_ids" | sort -u); do
		echo "$old_ids" | grep -qxF "$id" || continue
		for f in display tagline premium availability reasoning images new experimental warning; do
			o="$(jq -r ".tiers.$tier[] | select(.id==\"$id\") | .$f" "$TMP/old.json" | head -1)"
			v="$(jq -r ".tiers.$tier[] | select(.id==\"$id\") | .$f" "$TMP/new.json" | head -1)"
			if [[ "$o" != "$v" ]]; then
				ANNOUNCE+=("TUI row $id: $f '$o' -> '$v'")
				case "$f" in display | tagline | warning | new | experimental | availability) CHECKLIST+=("dashboard") ;; *) CHECKLIST+=("registry") ;; esac
				action=1
			fi
		done
	done
done
for k in hero.full hero.limited reward fallback; do
	o="$(jq -r ".$k" "$TMP/old.json")"; v="$(jq -r ".$k" "$TMP/new.json")"
	if [[ "$o" != "$v" ]]; then
		ANNOUNCE+=("TUI $k: $o -> $v")
		CHECKLIST+=("registry" "dashboard")
		action=1
	fi
done

NEEDS_REG=0; NEEDS_DASH=0
for c in ${CHECKLIST[@]+"${CHECKLIST[@]}"}; do
	[[ "$c" == "registry" ]] && NEEDS_REG=1
	[[ "$c" == "dashboard" ]] && NEEDS_DASH=1
done
REFRESH=()
((NEEDS_REG)) && REFRESH+=("bash scripts/sync-upstream.sh (registry pins) + go run ./backend/cmd/wiregen -upstream <sha> (catalog)")
((NEEDS_DASH)) && REFRESH+=("dashboard static picker copy (frontend/src + vite build + dist)")
((!NEEDS_REG && !NEEDS_DASH)) && REFRESH+=("none")

ANNOUNCE=( "${ANNOUNCE[@]%$'\r'}" )
jq -n --arg old "$OLD_SHA" --arg new "$NEW_SHA" \
	--slurpfile oldwf "$TMP/old.json" --slurpfile newwf "$TMP/new.json" \
	--argjson announce "$(printf '%s\n' ${ANNOUNCE[@]+"${ANNOUNCE[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	--argjson refresh "$(printf '%s\n' ${REFRESH[@]+"${REFRESH[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	--argjson action "$([[ $action == 1 ]] && echo true || echo false)" \
	'{old_sha:$old,new_sha:$new,checked_at:(now|todate),announce:$announce,refresh:$refresh,action_needed:$action,old:$oldwf[0],new:$newwf[0]}' >"$TUI_REPORT"

echo "report: $TUI_REPORT"
echo "action_needed=$([[ $action == 1 ]] && echo true || echo false)"
if ((${#ANNOUNCE[@]})); then printf '  - %s\n' "${ANNOUNCE[@]}"; fi
echo "refresh:"
if ((${#REFRESH[@]})); then printf '  - %s\n' "${REFRESH[@]}"; fi
if ((action)); then exit 1; else exit 0; fi
