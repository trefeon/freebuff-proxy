// Wire emitter (Wave D slice S3): regenerates upstream/wirecodes_gen.go and
// upstream/notices_gen.go from the pinned snapshots. Snapshot-backed values
// are extracted and cross-checked; server-observed body markers stay pinned.
// Any drift fails explicitly with file:line and the commit; outputs get
// nothing on error. Adds EmitWire only (S2 append-only pattern).
package wirefacts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Snapshot paths read by EmitWire, vendor-relative under wireDir.
const (
	wireSessionFile  = "common/src/types/freebuff-session.ts"
	wireCeilingsFile = "common/src/constants/freebuff-spend-ceilings.ts"
	wireAvailFile    = "common/src/util/freebuff-model-availability.ts"
	wirePeakFile     = "common/src/constants/freebuff-peak-hours.ts"
)

// wireCode pins one WireCode constant: its Go name, wire value, doc comment,
// and where the value was verified. snapshotFile is empty for server-observed
// body markers absent from the pinned snapshots.
type wireCode struct {
	goName       string
	value        string
	doc          string
	snapshotFile string
}

// wireCodes is the full WireCode vocabulary in wirecodes.go declaration
// order. Entries with snapshotFile are cross-checked against the extracted
// snapshot literals on every run; the rest are pinned server-observed body
// markers (classifyError matches them on live refusal bodies).
var wireCodes = []wireCode{
	{"WireCodeDeploymentOutsideHours", "deployment_outside_hours", "WireCodeDeploymentOutsideHours: free tier is outside operating hours.", ""},
	{"WireCodeFreeModeRunFanout", "free_mode_run_fanout", "WireCodeFreeModeRunFanout: the account's concurrent-run counter looked like proxy fanout. Also the RateLimitError.Status for the refusal.", ""},
	{"WireCodeFreeModeCapacityDeferred", "free_mode_capacity_deferred", "WireCodeFreeModeCapacityDeferred: free-tier transient capacity queue.", ""},
	{"WireCodeSessionLimitReached", "session_limit_reached", "WireCodeSessionLimitReached: the ACCOUNT is over its concurrent-tab budget (409).", wireSessionFile},
	{"WireCodeFreeModeCLIRequired", "free_mode_cli_required", "WireCodeFreeModeCLIRequired: the request lacked the CLI envelope.", ""},
	{"WireCodeCountryBlocked", "country_blocked", "WireCodeCountryBlocked: free mode not available from the egress region.", wireSessionFile},
	{"WireCodeIpCapped", "ip_capped", "WireCodeIpCapped: too many distinct users on the egress IP.", wireSessionFile},
	{"WireCodeWaitingRoomQueued", "waiting_room_queued", "WireCodeWaitingRoomQueued: transient admission race.", wireSessionFile},
	{"WireCodeWaitingRoomRequired", "waiting_room_required", "WireCodeWaitingRoomRequired: the account must walk the pre-session ad-chain + streak flow (428).", wireSessionFile},
	{"WireCodeSessionModelMismatch", "session_model_mismatch", "WireCodeSessionModelMismatch: the session row is bound to a different model (with a \"limited\" marker for the egress-IP case).", wireSessionFile},
	{"WireCodeFreeModeInvalidAgentModel", "free_mode_invalid_agent_model", "WireCodeFreeModeInvalidAgentModel: the (agent, model) pair is not in the allowlist. Also the RateLimitError.Status for the refusal.", ""},
	{"WireCodeSessionSuperseded", "session_superseded", "WireCodeSessionSuperseded: another instance took over the account (409).", wireSessionFile},
	{"WireCodeTurnSpendLimit", "turn_spend_limit", "WireCodeTurnSpendLimit: upstream killed a runaway turn (429 per-turn spend ceiling, usually a stuck agent loop).", ""},
	{"WireCodeFreebuffUpdateRequired", "freebuff_update_required", "WireCodeFreebuffUpdateRequired: the CLI app version is out of date.", ""},
	{"WireCodeSessionExpired", "session_expired", "WireCodeSessionExpired: the free session has expired.", wireSessionFile},
	{"WireCodeModelLocked", "model_locked", "WireCodeModelLocked: the model is locked for this account.", wireSessionFile},
	{"WireCodeFreeModeLegacyLunaAgent", "free_mode_legacy_luna_agent", "WireCodeFreeModeLegacyLunaAgent: the legacy Luna agent id was used.", ""},
	{"WireCodeFreeModeLegacyLuna", "free_mode_legacy_luna", "WireCodeFreeModeLegacyLuna: the legacy Luna id was used.", ""},
	{"WireCodeRunIDNotFound", "runid not found", "WireCodeRunIDNotFound: the run id is gone.", ""},
	{"WireCodeRunIDNotRunning", "runid not running", "WireCodeRunIDNotRunning: the run is not running.", ""},
	{"WireCodeInsufficientQuota", "insufficient_quota", "WireCodeInsufficientQuota: upstream load saturation (the body marker for a load-shedding refusal).", ""},
	{"WireCodeLimitBurstRate", "limit_burst_rate", "WireCodeLimitBurstRate: burst-rate cap hit (another load-shedding body marker).", ""},
	{"WireCodePeakHours", "peak hours", "WireCodePeakHours: peak-hours cap (\"peak hours\" body marker).", ""},
	{"WireCodePeakHoursStatus", "peak_hours", "WireCodePeakHoursStatus is the RateLimitError.Status for the peak-hours refusal (underscore form).", ""},
	{"WireCodeLoadShedding", "load_shedding", "WireCodeLoadShedding is the RateLimitError.Status for a load-shedding refusal (the body markers are insufficient_quota / limit_burst_rate).", ""},
	{"WireCodeRateLimited", "rate_limited", "WireCodeRateLimited: generic rate limit.", wireSessionFile},
	{"WireCodeSpendLimited", "spend_limited", "WireCodeSpendLimited: spend ceiling reached.", wireSessionFile},
	{"WireCodeBanned", "banned", "WireCodeBanned: the account is temporarily banned (the canonical {\"status\":\"banned\"} marker).", wireSessionFile},
	{"WireCodeAccountSuspended", "account_suspended", "WireCodeAccountSuspended: hard-ban shape ({\"error\":\"account_suspended\",...}).", ""},
}

// wireGate pins one FREEBUFF_GATE_CODES row: code plus its HTTP status and
// endsTheSession flag, verified against freebuff-session.ts on every run.
type wireGate struct {
	status int
	ends   bool
}

// wireGates mirrors FREEBUFF_GATE_CODES in freebuff-session.ts declaration
// order. Model_unavailable is a known gate row with no WireCode constant —
// it rides availableHours prose, not a body marker — so it is verified but
// not emitted.
var wireGates = []struct {
	code string
	gate wireGate
}{
	{"waiting_room_required", wireGate{428, true}},
	{"session_expired", wireGate{410, true}},
	{"session_superseded", wireGate{409, true}},
	{"session_model_mismatch", wireGate{409, true}},
	{"session_limit_reached", wireGate{409, false}},
	{"waiting_room_queued", wireGate{429, false}},
	{"model_unavailable", wireGate{410, false}},
}

// wireGateBacked lists WireCode values verified through the
// FREEBUFF_GATE_CODES table rather than a status literal: the gate rows
// carry the same code strings as keys with their HTTP status and
// endsTheSession flag, which the gate check below verifies row by row.
var wireGateBacked = map[string]bool{
	"session_limit_reached": true, "waiting_room_queued": true,
	"waiting_room_required": true, "session_model_mismatch": true,
	"session_superseded": true, "session_expired": true,
}

// wireKnownStatuses are status literals in freebuff-session.ts that carry no
// WireCode: lifecycle states plus admission-only shapes that never appear as
// classifyError body markers (superseded is the server-response shape while
// session_superseded is the gate/chat error code; model_unavailable rides
// availableHours prose; premium_slot_taken is Desktop-only). Anything outside
// this set plus the snapshot-verified wire values fails the run as unknown.
var wireKnownStatuses = map[string]bool{
	"none": true, "active": true, "ended": true,
	"superseded": true, "model_unavailable": true, "premium_slot_taken": true,
}

// wireNotice pins one notice constant: its Go name, upstream export, source
// file, and verbatim copy at the pinned commit.
type wireNotice struct {
	goName string
	export string
	file   string
	value  string
	doc    string
}

var wireNotices = []wireNotice{
	{"TierChangeNotice", "FREEBUFF_TIER_CHANGE_NOTICE", wireAvailFile,
		"Solar Pro 4 is now unmetered at full access and available with limited access. GPT-5.6 Luna still uses your shared premium allowance, charging partial time rounded up to a tenth. —❤️ Freebuff Team",
		"TierChangeNotice is FREEBUFF_TIER_CHANGE_NOTICE from upstream."},
	{"CapacityNotice", "FREEBUFF_CAPACITY_NOTICE", wireCeilingsFile,
		"Capacity is now limited per account — sustained automated abuse forced us to cap how much any one account can use.",
		"CapacityNotice is FREEBUFF_CAPACITY_NOTICE."},
	{"RestrictedNotice", "FREEBUFF_RESTRICTED_NOTICE", wireCeilingsFile,
		"This account has reduced capacity: it was flagged for VPN or proxy usage, a restricted location, or an email domain commonly used by bot farms. If you are on a VPN, connecting directly restores normal limits.",
		"RestrictedNotice is FREEBUFF_RESTRICTED_NOTICE."},
	{"BudgetNotice", "FREEBUFF_BUDGET_NOTICE", wireCeilingsFile,
		"You have used all of today’s free usage on this account.",
		"BudgetNotice is FREEBUFF_BUDGET_NOTICE."},
	{"FreebucksCeilingNotice", "FREEBUFF_FREEBUCKS_CEILING_NOTICE", wireCeilingsFile,
		"This account hit today’s hard usage cap. Freebucks pay for sessions, but the compute a day can draw is capped at three times what its Freebucks are worth, to protect the service from runaway usage.",
		"FreebucksCeilingNotice is FREEBUFF_FREEBUCKS_CEILING_NOTICE. Upstream retired the per-model caps to soft pacing targets; the spend field itself is @deprecated on the wire."},
}

// Pinned DeepSeek peak facts from freebuff-peak-hours.ts at the pinned
// commit: the pricing ranges, the 1h admission lead, and the derived
// expensive window [min(starts)-lead, max(ends)).
var wirePeakRanges = [][2]int{{1, 4}, {6, 10}}

const wirePeakLead = 1
const wirePeakExpStart, wirePeakExpEnd = 0, 10

var (
	wireStatusRE = regexp.MustCompile(`status:\s*'([^']+)'`)
	wireGateRE   = regexp.MustCompile(`^(\w+):\s*\{\s*status:\s*(\d+),\s*endsTheSession:\s*(true|false)\s*\},?\s*$`)
	wirePairRE   = regexp.MustCompile(`\[(\d+),\s*(\d+)\]`)
	wireLeadRE   = regexp.MustCompile(`DEEPSEEK_EXPENSIVE_WINDOW_LEAD_HOURS\s*=\s*(\d+)`)
)

// EmitWire regenerates wirecodes_gen.go into wireOut and notices_gen.go into
// noticesOut for upstreamSHA. Either output receives nothing on error, so
// callers can exit fatal without emitting half-written files.
func EmitWire(upstreamSHA, wireDir string, wireOut, noticesOut io.Writer) error {
	raw, err := os.ReadFile(filepath.Join(wireDir, "snapshots.json"))
	if err != nil {
		return fmt.Errorf("wiregen: read snapshots manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("wiregen: parse snapshots manifest: %w", err)
	}
	if upstreamSHA != m.UpstreamSHA {
		return fmt.Errorf("wiregen: -upstream %q does not match manifest upstream_sha %q (testdata/wire/snapshots.json); re-pin the snapshots before regenerating", upstreamSHA, m.UpstreamSHA)
	}
	bySHA := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		bySHA[f.Path] = f.SHA256
	}
	session, err := wireReadVerified(wireDir, bySHA, wireSessionFile, m.UpstreamSHA)
	if err != nil {
		return err
	}
	ceilings, err := wireReadVerified(wireDir, bySHA, wireCeilingsFile, m.UpstreamSHA)
	if err != nil {
		return err
	}
	avail, err := wireReadVerified(wireDir, bySHA, wireAvailFile, m.UpstreamSHA)
	if err != nil {
		return err
	}
	peak, err := wireReadVerified(wireDir, bySHA, wirePeakFile, m.UpstreamSHA)
	if err != nil {
		return err
	}
	if err := wireVerifySession(session, m.UpstreamSHA); err != nil {
		return err
	}
	notices, ranges, lead, expStart, expEnd, err := wireVerifyNotices(ceilings, avail, peak, m.UpstreamSHA)
	if err != nil {
		return err
	}
	wireSrc, err := format.Source(wireEmitCodes(upstreamSHA, m.UpstreamSHA))
	if err != nil {
		return fmt.Errorf("wiregen: format wirecodes_gen.go: %w", err)
	}
	noticesSrc, err := format.Source(wireEmitNotices(upstreamSHA, m.UpstreamSHA, notices, ranges, lead, expStart, expEnd))
	if err != nil {
		return fmt.Errorf("wiregen: format notices_gen.go: %w", err)
	}
	if _, err := wireOut.Write(wireSrc); err != nil {
		return err
	}
	_, err = noticesOut.Write(noticesSrc)
	return err
}

// wireReadVerified reads wireDir/rel and requires its sha256 to match the
// manifest entry, so extraction never runs on unpinned bytes.
func wireReadVerified(wireDir string, bySHA map[string]string, rel, commit string) ([]byte, error) {
	want, ok := bySHA[rel]
	if !ok {
		return nil, fmt.Errorf("wiregen: wire snapshot %s missing from testdata/wire/snapshots.json at upstream commit %s; re-pin before regenerating", rel, commit)
	}
	src, err := os.ReadFile(filepath.Join(wireDir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("wiregen: read wire snapshot %s: %w", rel, err)
	}
	if got := sha256Hex(src); got != want {
		return nil, fmt.Errorf("wiregen: wire snapshot %s hash %s != manifest %s at upstream commit %s; re-pin before regenerating", rel, got, want, commit)
	}
	return src, nil
}

// wireVerifySession cross-checks the admission status literals and the
// FREEBUFF_GATE_CODES rows against the pinned tables. Unknown literals fail
// explicitly; a missing expected literal fails as drift.
func wireVerifySession(src []byte, commit string) error {
	seen := map[string]bool{}
	for _, loc := range wireStatusRE.FindAllSubmatchIndex(src, -1) {
		val := string(src[loc[2]:loc[3]])
		seen[val] = true
		if wireKnownStatuses[val] || wireGateBacked[val] || wireSnapshotValue(val) {
			continue
		}
		line := wireLineOf(src, loc[0])
		return fmt.Errorf("wiregen: unknown status literal %q in %s:%d at upstream commit %s; pin a WireCode or allowlist it before regenerating", val, wireSessionFile, line, commit)
	}
	for _, want := range wireSnapshotValues() {
		if !seen[want] {
			return fmt.Errorf("wiregen: expected status literal %q gone from %s at upstream commit %s; re-pin the WireCode table before regenerating", want, wireSessionFile, commit)
		}
	}
	inGates := false
	got := map[string]wireGate{}
	for i, line := range strings.Split(string(src), "\n") {
		trim := strings.TrimSpace(line)
		if strings.Contains(trim, "FREEBUFF_GATE_CODES") && strings.Contains(trim, "{") {
			inGates = true
			continue
		}
		if !inGates {
			continue
		}
		if strings.HasPrefix(trim, "}") {
			break
		}
		mm := wireGateRE.FindStringSubmatch(trim)
		if mm == nil {
			if trim == "" || strings.HasPrefix(trim, "/") || strings.HasPrefix(trim, "*") {
				continue
			}
			return fmt.Errorf("wiregen: unknown gate row %q in %s:%d at upstream commit %s; teach the generator before regenerating", trim, wireSessionFile, i+1, commit)
		}
		st, _ := strconv.Atoi(mm[2])
		got[mm[1]] = wireGate{st, mm[3] == "true"}
	}
	if len(got) != len(wireGates) {
		return fmt.Errorf("wiregen: FREEBUFF_GATE_CODES has %d rows in %s, want %d at upstream commit %s; re-pin before regenerating", len(got), wireSessionFile, len(wireGates), commit)
	}
	for _, want := range wireGates {
		if got[want.code] != want.gate {
			return fmt.Errorf("wiregen: FREEBUFF_GATE_CODES[%q] is %+v in %s, want %+v at upstream commit %s; re-pin before regenerating", want.code, got[want.code], wireSessionFile, want.gate, commit)
		}
	}
	return nil
}

// wireSnapshotValues returns the wire values verified against snapshot
// status literals (gate rows are verified separately).
func wireSnapshotValues() []string {
	var out []string
	for _, c := range wireCodes {
		if c.snapshotFile == wireSessionFile && !wireGateBacked[c.value] {
			out = append(out, c.value)
		}
	}
	return out
}

// wireSnapshotValue reports whether a status literal backs a WireCode.
func wireSnapshotValue(val string) bool {
	for _, c := range wireCodes {
		if c.snapshotFile == wireSessionFile && c.value == val {
			return true
		}
	}
	return false
}

// wireVerifyNotices extracts the five notice strings and the peak facts,
// requiring byte-exact matches with the pinned tables.
func wireVerifyNotices(ceilings, avail, peak []byte, commit string) ([]wireNotice, [][2]int, int, int, int, error) {
	files := map[string][]byte{wireCeilingsFile: ceilings, wireAvailFile: avail}
	for _, n := range wireNotices {
		src := files[n.file]
		s, line, ok := wireExtractConst(src, n.export)
		if !ok {
			return nil, nil, 0, 0, 0, fmt.Errorf("wiregen: %s missing from %s at upstream commit %s; re-pin before regenerating", n.export, n.file, commit)
		}
		if s != n.value {
			return nil, nil, 0, 0, 0, fmt.Errorf("wiregen: %s drifted in %s:%d at upstream commit %s; re-pin the notice copy before regenerating", n.export, n.file, line, commit)
		}
	}
	ranges, lead, err := wireExtractPeak(peak)
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}
	if lead != wirePeakLead {
		return nil, nil, 0, 0, 0, fmt.Errorf("wiregen: DEEPSEEK_EXPENSIVE_WINDOW_LEAD_HOURS is %d in %s, want %d at upstream commit %s; re-pin before regenerating", lead, wirePeakFile, wirePeakLead, commit)
	}
	if len(ranges) != len(wirePeakRanges) {
		return nil, nil, 0, 0, 0, fmt.Errorf("wiregen: DEEPSEEK_PEAK_HOUR_RANGES_UTC has %d ranges in %s, want %d at upstream commit %s; re-pin before regenerating", len(ranges), wirePeakFile, len(wirePeakRanges), commit)
	}
	minStart, maxEnd := ranges[0][0], ranges[0][1]
	for i, r := range ranges {
		if r != wirePeakRanges[i] {
			return nil, nil, 0, 0, 0, fmt.Errorf("wiregen: DEEPSEEK_PEAK_HOUR_RANGES_UTC range %d is [%d %d] in %s, want [%d %d] at upstream commit %s; re-pin before regenerating", i, r[0], r[1], wirePeakFile, wirePeakRanges[i][0], wirePeakRanges[i][1], commit)
		}
		if r[0] < minStart {
			minStart = r[0]
		}
		if r[1] > maxEnd {
			maxEnd = r[1]
		}
	}
	if expStart, expEnd := minStart-lead, maxEnd; expStart != wirePeakExpStart || expEnd != wirePeakExpEnd {
		return nil, nil, 0, 0, 0, fmt.Errorf("wiregen: derived expensive window is [%d %d] from %s, want [%d %d] at upstream commit %s; re-pin before regenerating", expStart, expEnd, wirePeakFile, wirePeakExpStart, wirePeakExpEnd, commit)
	}
	return wireNotices, ranges, lead, wirePeakExpStart, wirePeakExpEnd, nil
}

// wireExtractConst finds `export const NAME =` then the single-quoted string
// on the following line, returning its unescaped value and 1-based line.
func wireExtractConst(src []byte, name string) (string, int, bool) {
	idx := bytes.Index(src, []byte("export const "+name))
	if idx < 0 {
		return "", 0, false
	}
	rest := src[idx:]
	q := bytes.IndexByte(rest, '\'')
	if q < 0 {
		return "", 0, false
	}
	var b strings.Builder
	esc := false
	for _, c := range rest[q+1:] {
		if esc {
			b.WriteByte(c)
			esc = false
			continue
		}
		if c == '\\' {
			esc = true
			continue
		}
		if c == '\'' {
			return b.String(), wireLineOf(src, idx), true
		}
		if c == '\n' {
			return "", 0, false
		}
		b.WriteByte(c)
	}
	return "", 0, false
}

// wireExtractPeak reads the pricing ranges and admission lead from the peak
// snapshot. Only the declaration block is scanned: later destructuring uses
// of DEEPSEEK_EXPENSIVE_WINDOW_UTC carry no numbers.
func wireExtractPeak(src []byte) ([][2]int, int, error) {
	decl := bytes.Index(src, []byte("DEEPSEEK_PEAK_HOUR_RANGES_UTC"))
	if decl < 0 {
		return nil, 0, fmt.Errorf("wiregen: DEEPSEEK_PEAK_HOUR_RANGES_UTC missing from %s; teach the generator before regenerating", wirePeakFile)
	}
	end := bytes.Index(src[decl:], []byte("as const"))
	block := src[decl:]
	if end >= 0 {
		block = src[decl : decl+end]
	}
	var ranges [][2]int
	for _, m := range wirePairRE.FindAllSubmatch(block, -1) {
		a, _ := strconv.Atoi(string(m[1]))
		b, _ := strconv.Atoi(string(m[2]))
		ranges = append(ranges, [2]int{a, b})
	}
	if len(ranges) == 0 {
		return nil, 0, fmt.Errorf("wiregen: no hour ranges parsed from %s; teach the generator before regenerating", wirePeakFile)
	}
	lm := wireLeadRE.FindSubmatch(src)
	if lm == nil {
		return nil, 0, fmt.Errorf("wiregen: DEEPSEEK_EXPENSIVE_WINDOW_LEAD_HOURS missing from %s; teach the generator before regenerating", wirePeakFile)
	}
	lead, _ := strconv.Atoi(string(lm[1]))
	return ranges, lead, nil
}

// wireLineOf returns the 1-based line number of offset off in src.
func wireLineOf(src []byte, off int) int {
	return bytes.Count(src[:off], []byte("\n")) + 1
}

// wireEmitCodes renders wirecodes_gen.go: the full WireCode block with per-
// entry provenance (snapshot-verified vs pinned server-observed marker).
func wireEmitCodes(flagSHA, commit string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by cmd/wiregen -upstream %s; DO NOT EDIT.
// Wire vocabulary at upstream commit %s: snapshot-backed values are
// extracted from common/src/types/freebuff-session.ts (status literals and
// FREEBUFF_GATE_CODES) and cross-checked on every run; the remaining body
// markers are pinned server-observed refusal strings absent from the
// snapshots. A new status/gate literal fails the generator explicitly.
package upstream

const (
`, flagSHA, commit)
	for _, c := range wireCodes {
		prov := "pinned server-observed body marker (absent from the snapshots)"
		if c.snapshotFile != "" {
			prov = "snapshot: " + c.snapshotFile
		}
		fmt.Fprintf(&b, "\t// %s\n\t// %s.\n\t%s WireCode = %q\n", c.doc, prov, c.goName, c.value)
	}
	b.WriteString(")\n")
	return []byte(b.String())
}

// wireEmitNotices renders notices_gen.go: the five notice strings plus the
// DeepSeek peak window the live EvaluateDeepSeekPeak evaluates.
func wireEmitNotices(flagSHA, commit string, notices []wireNotice, ranges [][2]int, lead, expStart, expEnd int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by cmd/wiregen -upstream %s; DO NOT EDIT.
// Notice copy at upstream commit %s, extracted verbatim from
// common/src/constants/freebuff-spend-ceilings.ts,
// common/src/util/freebuff-model-availability.ts, and
// common/src/constants/freebuff-peak-hours.ts. A reworded notice or moved
// peak window fails the generator explicitly.
package upstream

const (
`, flagSHA, commit)
	for _, n := range notices {
		fmt.Fprintf(&b, "\t// %s Upstream %s (%s).\n\t%s = %q\n", n.doc, n.export, n.file, n.goName, n.value)
	}
	fmt.Fprintf(&b, `
	// DeepSeekExpensiveWindowStartUTC and DeepSeekExpensiveWindowEndUTC bound
	// the single weekday window [start, end) UTC in which DeepSeek is most
	// expensive: min(DEEPSEEK_PEAK_HOUR_RANGES_UTC starts) minus
	// DEEPSEEK_EXPENSIVE_WINDOW_LEAD_HOURS through max(ends).
	DeepSeekExpensiveWindowStartUTC = %d
	DeepSeekExpensiveWindowEndUTC = %d
	// DeepSeekExpensiveWindowLeadHours is how long before peak opens the
	// admission window starts standing sessions off.
	DeepSeekExpensiveWindowLeadHours = %d
)

// DeepSeekPeakHourRangesUTC mirrors DEEPSEEK_PEAK_HOUR_RANGES_UTC: the
// half-open [start, end) UTC pricing windows behind the expensive window.
var DeepSeekPeakHourRangesUTC = [][2]int{`, expStart, expEnd, lead)
	for _, r := range ranges {
		fmt.Fprintf(&b, "{%d, %d}, ", r[0], r[1])
	}
	b.WriteString("}\n")
	return []byte(b.String())
}
