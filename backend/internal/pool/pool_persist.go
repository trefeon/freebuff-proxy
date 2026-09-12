package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"freebuff-proxy/backend/internal/upstream"
)

// pool_persist.go — pool runtime write-through cache (DB-unified-storage).
//
// The in-memory maps stay the hot path: every mutation only sets a dirty
// flag (markPersistDirty, a lock-free atomic store). A background flush —
// the maintain tick plus a best-effort pass in Shutdown — snapshots the
// allowlisted state and saves it through the PoolPersist interface. The
// pool never imports the store package (archtest leaf rule): the pool
// marshals its own opaque blobs, and the store package implements
// PoolPersist implicitly (no import in either direction). Nil store
// disables persistence (in-memory only). Save errors only warn and
// re-arm the dirty flag for the next pass — a DB failure degrades to
// live-only and never blocks the request hot path.
//
// Persist allowlist (parent-scoped): ledger counters, admissions counts,
// bridge daily usage plus survivors, burst hits, the smart-probe scheduler
// timer (lastProbe, backoff, overloadedAt, idleSlept) and the live
// per-token quota cache (QuotaByModel + QuotaSavedAt). Never persisted:
// live handles (channels, sync.Once, WaitGroup, CancelFunc,
// atomic.Pointer, Logger, Registry, tokenEntry pointers), the transient
// scheduler dispatch flags (kick, inflight — a persisted inflight would
// suppress ticks forever; restore hard-resets both), the boot flag
// (a restart always re-arms the boot round event; with the quota cache
// restored it probes nothing when warm), and the unfit registry (pure
// 5-minute TTL episode state; a restart starts servable and re-marks on
// the next upstream limited_ip refusal).
//
// Key namespace (stable strings; store never interprets values):
//
//	pool/ledger/<sha256hex(token)>       one AccountLedger blob per token
//	pool/admissions                       in-flight session admissions by model
//	pool/burst                            per-model sliding-window burst hits
//	pool/bridge/usage                     global bridge daily counter
//	pool/bridge/survivors                 evicted bridge usage survivors
//	pool/probe/scheduler                  smart-probe scheduler timer (pool-scoped)
//	pool/probe/quota/<sha256hex(token)>   live quota cache per token (SHA-keyed:
//	                                      roster order is unstable across restarts)
const (
	poolStateAdmissions      = "pool/admissions"
	poolStateBurst           = "pool/burst"
	poolStateBridgeUsage     = "pool/bridge/usage"
	poolStateBridgeSurvivors = "pool/bridge/survivors"
	poolLedgerPrefix         = "pool/ledger/"
	poolProbeScheduler       = "pool/probe/scheduler"
	poolProbeQuotaPrefix     = "pool/probe/quota/"
)

// PoolPersist is the persistence backend for pool runtime state. Values
// are opaque blobs the pool marshals itself; Load maps a missing row to
// ok=false (never an error).
type PoolPersist interface {
	SavePoolState(key string, value []byte) error
	LoadPoolState(key string) (value []byte, ok bool, err error)
	DeletePoolState(key string) error
	ListPoolState(prefix string) (map[string][]byte, error)
}

// poolSpendHit is one rolling-24h spend amount at Unix millis UTC.
type poolSpendHit struct {
	At     int64 `json:"at"`
	Tokens int64 `json:"tokens"`
}

// poolSpendBlob is the JSON-stable mirror of spendLedger. Field names stay
// snake_case and additive — old rows must still unmarshal after new
// counters land.
type poolSpendBlob struct {
	Rolling      []poolSpendHit `json:"rolling"`
	DayUsed      int64          `json:"day_used"`
	DayStart     int64          `json:"day_start"`
	WeekUsed     int64          `json:"week_used"`
	WeekStart    int64          `json:"week_start"`
	MonthUsed    int64          `json:"month_used"`
	MonthStart   int64          `json:"month_start"`
	SpendLimited int            `json:"spend_limited"`
}

// poolLedgerBlob is the JSON-stable mirror of AccountLedger. Timestamps
// are Unix millis UTC.
type poolLedgerBlob struct {
	Usage       []int64       `json:"usage"`
	Spend       poolSpendBlob `json:"spend"`
	Requests    []int64       `json:"requests"`
	ReqDayStart int64         `json:"req_day_start"`
	ReqDayCount int64         `json:"req_day_count"`
}

// poolBurstHit is one burst admission at Unix millis UTC.
type poolBurstHit struct {
	At  int64 `json:"at"`
	Tok int   `json:"tok"`
}

// poolSurvivorBlob is one evicted bridge entry's carried usage.
type poolSurvivorBlob struct {
	Count   int   `json:"count"`
	Evicted int64 `json:"evicted"`
}

// poolSmartProbeBlob is the JSON-stable mirror of the persisted scheduler
// timer fields (Unix millis UTC, 0 = zero time). Only the timer crosses a
// restart: bootDone is excluded (a restart re-arms the boot round event),
// and kick/inflight are transient dispatch state (restore hard-resets
// both — a persisted inflight would suppress ticks forever).
type poolSmartProbeBlob struct {
	LastProbe    int64 `json:"last_probe"`
	Backoff      int   `json:"backoff"`
	IdleSlept    bool  `json:"idle_slept"`
	OverloadedAt int64 `json:"overloaded_at"`
}

// poolQuotaRow mirrors one live quota row. ResetAt is Unix millis UTC
// (0 = absent, preserved as the zero time on restore).
type poolQuotaRow struct {
	Model       string             `json:"model"`
	Limit       float64            `json:"limit"`
	RecentCount float64            `json:"recent_count"`
	ResetAt     int64              `json:"reset_at"`
	Period      string             `json:"period,omitempty"`
	Pool        string             `json:"pool,omitempty"`
	PoolLabel   string             `json:"pool_label,omitempty"`
	Entitlement map[string]float64 `json:"entitlement,omitempty"`
}

// poolQuotaBlob is one token's live quota cache: the QuotaByModel map plus
// the QuotaSavedAt write time (Unix millis UTC). Field names stay
// snake_case and additive — old rows must still unmarshal after new
// counters land.
type poolQuotaBlob struct {
	SavedAt int64                   `json:"saved_at"`
	Quota   map[string]poolQuotaRow `json:"quota_by_model"`
}

// poolTokenHash keys a token's per-token rows by the SHA-256 hex of the
// token value: raw tokens never cross the persistence boundary.
func poolTokenHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// poolLedgerKey returns the pool_state key for one token's ledger blob.
func poolLedgerKey(tokenHash string) string { return poolLedgerPrefix + tokenHash }

// poolProbeQuotaKey returns the pool_state key for one token's live quota
// cache blob. SHA-keyed like the ledger rows: roster order is unstable
// across restarts (config reloads, removals), so a positional index key
// would mis-target another account.
func poolProbeQuotaKey(tokenHash string) string { return poolProbeQuotaPrefix + tokenHash }

// SetPoolPersist wires the runtime persistence backend (nil disables).
// Start restores the persisted state automatically after wiring, so
// counters and the probe cache survive restarts; direct RestorePoolPersist
// calls remain for tests and pre-Start restores.
func (p *Pool) SetPoolPersist(s PoolPersist) {
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	p.persist = s
}

// markPersistDirty arms the background flush. Lock-free so the request hot
// path never blocks on persistence.
func (p *Pool) markPersistDirty() { p.persistDirty.Store(true) }

// poolKV is one key/blob pair staged for save.
type poolKV struct {
	key string
	val []byte
}

func mustMarshalPool(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// Pool blobs are plain ints/strings/slices: unmarshalable only on
		// programmer error. Fall back to an empty object so one bad shape
		// cannot wedge the whole flush (restore skips it as corrupt).
		return []byte(`{}`)
	}
	return raw
}

// FlushPoolPersist snapshots the allowlisted state and saves it. Locks are
// never held across store I/O: each subsystem is copied under its own
// lock, marshalled, then written. Errors re-arm the dirty flag for the
// next pass and are returned for tests; background callers (maintain tick,
// Shutdown) log-and-continue so a DB failure degrades to live-only.
func (p *Pool) FlushPoolPersist() error {
	p.persistMu.Lock()
	st := p.persist
	p.persistMu.Unlock()
	if st == nil || !p.persistDirty.Swap(false) {
		return nil
	}
	staged, liveLedgers, liveQuotas := p.snapshotPoolState()
	for _, kv := range staged {
		if err := st.SavePoolState(kv.key, kv.val); err != nil {
			p.persistDirty.Store(true)
			p.logger.Warn("pool: runtime persist flush failed (live-only until next pass)", "key", kv.key, "error", err)
			return err
		}
	}
	// Best-effort orphan prune: per-token rows whose token left the roster
	// (config reload, RemoveLastToken) must not accumulate across
	// restarts. Failure is not fatal — the next pass retries.
	if rows, err := st.ListPoolState(poolLedgerPrefix); err == nil {
		for key := range rows {
			if !liveLedgers[key] {
				_ = st.DeletePoolState(key)
			}
		}
	}
	if rows, err := st.ListPoolState(poolProbeQuotaPrefix); err == nil {
		for key := range rows {
			if !liveQuotas[key] {
				_ = st.DeletePoolState(key)
			}
		}
	}
	return nil
}

// snapshotPoolState copies the allowlisted state under each subsystem's
// own lock and marshals it. No store I/O happens here. It also returns
// the live per-token key sets for orphan pruning.
func (p *Pool) snapshotPoolState() (staged []poolKV, liveLedgers, liveQuotas map[string]bool) {
	liveLedgers = make(map[string]bool)
	liveQuotas = make(map[string]bool)

	// Per-token ledgers (roster lock; entry pointers stay in memory —
	// only the counters cross into blobs). The entry list is also copied
	// for the quota snapshot below (sessions read outside this lock).
	p.roster.mu.Lock()
	ledgers := make([]poolKV, 0)
	quotaEntries := make([]*tokenEntry, 0)
	for _, entry := range *p.roster.toks.Load() {
		if entry == nil {
			continue
		}
		quotaEntries = append(quotaEntries, entry)
		if entry.ledger == nil {
			continue
		}
		key := poolLedgerKey(poolTokenHash(entry.token))
		blob := marshalLedger(entry.ledger)
		ledgers = append(ledgers, poolKV{key: key, val: mustMarshalPool(blob)})
		liveLedgers[key] = true
	}
	p.roster.mu.Unlock()
	staged = append(staged, ledgers...)

	// Live per-token quota cache (session snapshots read outside the
	// roster lock — the established Snapshot() order — so a slow session
	// lock never wedges roster mutations). Tokens with neither quota rows
	// nor a write time stage nothing: a missing row reads as a fresh boot.
	for _, entry := range quotaEntries {
		if entry.session == nil {
			continue
		}
		snap := entry.session.Snapshot()
		if len(snap.QuotaByModel) == 0 && snap.QuotaSavedAt.IsZero() {
			continue
		}
		blob := poolQuotaBlob{Quota: make(map[string]poolQuotaRow, len(snap.QuotaByModel))}
		if !snap.QuotaSavedAt.IsZero() {
			blob.SavedAt = snap.QuotaSavedAt.UnixMilli()
		}
		for model, q := range snap.QuotaByModel {
			if model == "" {
				continue
			}
			row := poolQuotaRow{
				Model:       q.Model,
				Limit:       q.Limit,
				RecentCount: q.RecentCount,
				Period:      q.Period,
				Pool:        q.Pool,
				PoolLabel:   q.PoolLabel,
				Entitlement: q.Entitlement,
			}
			if row.Model == "" {
				row.Model = model
			}
			if !q.ResetAt.IsZero() {
				row.ResetAt = q.ResetAt.UnixMilli()
			}
			blob.Quota[model] = row
		}
		key := poolProbeQuotaKey(poolTokenHash(entry.token))
		staged = append(staged, poolKV{key: key, val: mustMarshalPool(blob)})
		liveQuotas[key] = true
	}

	// Smart-probe scheduler timer (own mutex; bootDone/kick/inflight are
	// transient and never staged — see poolSmartProbeBlob).
	p.smartProbe.mu.Lock()
	sched := poolSmartProbeBlob{
		Backoff:   p.smartProbe.backoff,
		IdleSlept: p.smartProbe.idleSlept,
	}
	if !p.smartProbe.lastProbe.IsZero() {
		sched.LastProbe = p.smartProbe.lastProbe.UnixMilli()
	}
	if !p.smartProbe.overloadedAt.IsZero() {
		sched.OverloadedAt = p.smartProbe.overloadedAt.UnixMilli()
	}
	p.smartProbe.mu.Unlock()
	staged = append(staged, poolKV{key: poolProbeScheduler, val: mustMarshalPool(sched)})

	// Admissions (transient in-flight counts; restored as-is, self-heals
	// on the next admission cycle).
	p.admissionsMu.Lock()
	adm := make(map[string]int, len(p.admissions))
	for m, idx := range p.admissions {
		adm[m] = idx
	}
	p.admissionsMu.Unlock()
	staged = append(staged, poolKV{key: poolStateAdmissions, val: mustMarshalPool(adm)})

	// Burst hits (millis; restore prunes out-of-window hits).
	p.burstMu.Lock()
	burst := make(map[string][]poolBurstHit, len(p.burstHits))
	for m, hits := range p.burstHits {
		cp := make([]poolBurstHit, 0, len(hits))
		for _, h := range hits {
			cp = append(cp, poolBurstHit{At: h.at.UnixMilli(), Tok: h.tok})
		}
		burst[m] = cp
	}
	p.burstMu.Unlock()
	staged = append(staged, poolKV{key: poolStateBurst, val: mustMarshalPool(burst)})

	// Bridge daily usage + survivors (survivor eviction times as millis).
	p.bridgeMu.Lock()
	usage := p.bridgeDailyUsage
	survivors := make([]poolSurvivorBlob, 0, len(p.bridgeSurvivors))
	for _, s := range p.bridgeSurvivors {
		survivors = append(survivors, poolSurvivorBlob{Count: s.count, Evicted: s.evicted.UnixMilli()})
	}
	p.bridgeMu.Unlock()
	staged = append(staged,
		poolKV{key: poolStateBridgeUsage, val: mustMarshalPool(usage)},
		poolKV{key: poolStateBridgeSurvivors, val: mustMarshalPool(survivors)},
	)
	return staged, liveLedgers, liveQuotas
}

// marshalLedger copies one ledger's counters into its blob form. Caller
// holds the roster (or bridge) mutex.
func marshalLedger(l *AccountLedger) poolLedgerBlob {
	blob := poolLedgerBlob{
		ReqDayStart: l.reqDayStart,
		ReqDayCount: l.reqDayCount,
	}
	for _, t := range l.usage {
		blob.Usage = append(blob.Usage, t.UnixMilli())
	}
	for _, t := range l.requests {
		blob.Requests = append(blob.Requests, t.UnixMilli())
	}
	if l.spend != nil {
		sp := poolSpendBlob{
			DayUsed:      l.spend.dayUsed,
			DayStart:     l.spend.dayStart,
			WeekUsed:     l.spend.weekUsed,
			WeekStart:    l.spend.weekStart,
			MonthUsed:    l.spend.monthUsed,
			MonthStart:   l.spend.monthStart,
			SpendLimited: l.spend.spendLimited,
		}
		for _, e := range l.spend.rolling {
			sp.Rolling = append(sp.Rolling, poolSpendHit{At: e.at.UnixMilli(), Tokens: e.tokens})
		}
		blob.Spend = sp
	}
	return blob
}

// RestorePoolPersist loads persisted runtime state into the live maps.
// Start calls it automatically after the owner wires the store with
// SetPoolPersist; direct calls remain for tests and pre-Start restores.
// Missing rows stay zero-valued (a fresh boot behaves exactly as before);
// corrupt rows warn and are skipped — restore never fails the boot.
// TTL/expiry is enforced on the way in: out-of-window usage/request/burst/
// survivor timestamps are dropped and stale spend buckets roll, so a
// restart never resurrects expired windows. Restored quota rows seed the
// session cache as last-known (stale-marked, dashboard-visible at once);
// the scheduler's freshness gate re-probes them only once aged out.
func (p *Pool) RestorePoolPersist() {
	p.persistMu.Lock()
	st := p.persist
	p.persistMu.Unlock()
	if st == nil {
		return
	}
	now := time.Now()
	p.restoreLedgers(st, now)
	p.restoreAdmissions(st)
	p.restoreBurst(st, now)
	p.restoreBridge(st, now)
	p.restoreSmartProbe(st)
	p.restoreProbeQuota(st)
}

// restoreSmartProbe loads the scheduler timer persisted by the last flush.
// A missing row is a fresh boot (zero state, current behavior); a corrupt
// row warns and is skipped. The transient dispatch flags are hard-reset:
// kick=false (no spurious forced round) and inflight=false (a persisted
// inflight would suppress ticks forever). bootDone is never persisted —
// a restart always re-arms the boot round event, and the restored quota
// cache keeps that round a zero-probe no-op while warm.
func (p *Pool) restoreSmartProbe(st PoolPersist) {
	raw, ok, err := st.LoadPoolState(poolProbeScheduler)
	if err != nil {
		p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", poolProbeScheduler, "error", err)
		return
	}
	if !ok {
		return
	}
	var blob poolSmartProbeBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", poolProbeScheduler, "error", err)
		return
	}
	p.smartProbe.mu.Lock()
	defer p.smartProbe.mu.Unlock()
	if blob.LastProbe > 0 {
		p.smartProbe.lastProbe = time.UnixMilli(blob.LastProbe)
	}
	if blob.Backoff >= 0 {
		if blob.Backoff > maxSmartProbeBackoff {
			p.smartProbe.backoff = maxSmartProbeBackoff
		} else {
			p.smartProbe.backoff = blob.Backoff
		}
	}
	if blob.OverloadedAt > 0 {
		p.smartProbe.overloadedAt = time.UnixMilli(blob.OverloadedAt)
	}
	p.smartProbe.idleSlept = blob.IdleSlept
	p.smartProbe.kick = false
	p.smartProbe.inflight = false
}

// restoreProbeQuota seeds each live token's session quota cache from its
// SHA-keyed pool_state row, so a restart shows warm data immediately and
// the first tick probes nothing while the cache is fresh. Rows for tokens
// no longer in the roster are ignored (the flush prunes them); quota
// history stays append-only and untouched. Seeding rides the session
// manager's SeedQuota ts-compare, so a newer live write always wins and
// re-restores are no-ops.
func (p *Pool) restoreProbeQuota(st PoolPersist) {
	// Index the live roster by quota key first (no store I/O under the
	// roster lock). SHA-keyed, so a roster reorder across restarts still
	// seeds the right account.
	p.roster.mu.Lock()
	byKey := make(map[string]*tokenEntry)
	for _, entry := range *p.roster.toks.Load() {
		if entry == nil || entry.session == nil {
			continue
		}
		byKey[poolProbeQuotaKey(poolTokenHash(entry.token))] = entry
	}
	p.roster.mu.Unlock()

	for key, entry := range byKey {
		raw, ok, err := st.LoadPoolState(key)
		if err != nil {
			p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", key, "error", err)
			continue
		}
		if !ok {
			continue
		}
		var blob poolQuotaBlob
		if err := json.Unmarshal(raw, &blob); err != nil {
			p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", key, "error", err)
			continue
		}
		if len(blob.Quota) == 0 || blob.SavedAt <= 0 {
			continue
		}
		savedAt := time.UnixMilli(blob.SavedAt)
		for model, row := range blob.Quota {
			if model == "" {
				continue
			}
			q := upstream.ModelQuota{
				Model:       model,
				Limit:       row.Limit,
				RecentCount: row.RecentCount,
				Period:      row.Period,
				Pool:        row.Pool,
				PoolLabel:   row.PoolLabel,
			}
			if row.ResetAt > 0 {
				q.ResetAt = time.UnixMilli(row.ResetAt)
			}
			if len(row.Entitlement) > 0 {
				q.Entitlement = make(map[string]float64, len(row.Entitlement))
				for k, v := range row.Entitlement {
					q.Entitlement[k] = v
				}
			}
			entry.session.SeedQuota(q, savedAt)
		}
	}
}

func (p *Pool) restoreLedgers(st PoolPersist, now time.Time) {
	// Index the live roster by ledger key first (no store I/O under the
	// roster lock).
	p.roster.mu.Lock()
	byKey := make(map[string]*AccountLedger)
	for _, entry := range *p.roster.toks.Load() {
		if entry == nil {
			continue
		}
		if entry.ledger == nil {
			entry.ledger = newAccountLedger()
		}
		byKey[poolLedgerKey(poolTokenHash(entry.token))] = entry.ledger
	}
	p.roster.mu.Unlock()

	for key, ledger := range byKey {
		raw, ok, err := st.LoadPoolState(key)
		if err != nil {
			p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", key, "error", err)
			continue
		}
		if !ok {
			continue
		}
		var blob poolLedgerBlob
		if err := json.Unmarshal(raw, &blob); err != nil {
			p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", key, "error", err)
			continue
		}
		p.roster.mu.Lock()
		installLedger(ledger, blob, now)
		p.roster.mu.Unlock()
	}
}

// installLedger replaces a ledger's counters from its blob, dropping
// expired window timestamps and rolling stale spend buckets. Caller holds
// the roster (or bridge) mutex.
func installLedger(l *AccountLedger, blob poolLedgerBlob, now time.Time) {
	usageCutoff := now.Add(-usageWindow)
	rpmCutoff := now.Add(-rpmWindow)
	l.usage = l.usage[:0]
	for _, ms := range blob.Usage {
		if t := time.UnixMilli(ms); !t.Before(usageCutoff) {
			l.usage = append(l.usage, t)
		}
	}
	l.requests = l.requests[:0]
	for _, ms := range blob.Requests {
		if t := time.UnixMilli(ms); !t.Before(rpmCutoff) {
			l.requests = append(l.requests, t)
		}
	}
	// Pacific-day bucket: dayRequestCount rolls a stale bucket on read as a
	// side effect, so discard the return and keep the normalized state.
	l.reqDayStart, l.reqDayCount = blob.ReqDayStart, blob.ReqDayCount
	_ = l.dayRequestCount(now)
	if l.spend == nil {
		l.spend = newSpendLedger()
	}
	sp := l.spend
	sp.rolling = sp.rolling[:0]
	for _, e := range blob.Spend.Rolling {
		if t := time.UnixMilli(e.At); !t.Before(usageCutoff) && e.Tokens > 0 {
			sp.rolling = append(sp.rolling, spendEntry{at: t, tokens: e.Tokens})
		}
	}
	sp.dayUsed, sp.dayStart = rollSpendBucket(blob.Spend.DayUsed, blob.Spend.DayStart, "day", now)
	sp.weekUsed, sp.weekStart = rollSpendBucket(blob.Spend.WeekUsed, blob.Spend.WeekStart, "week", now)
	sp.monthUsed, sp.monthStart = rollSpendBucket(blob.Spend.MonthUsed, blob.Spend.MonthStart, "month", now)
	sp.spendLimited = blob.Spend.SpendLimited
}

// rollSpendBucket replays one spend period bucket at restore: a rolled-over
// window resets to the current bucket instead of resurrecting a stale sum.
func rollSpendBucket(used, start int64, period string, now time.Time) (int64, int64) {
	if needsRollover(start, period, now) {
		return 0, bucketStart(now, period)
	}
	return used, start
}

func (p *Pool) restoreAdmissions(st PoolPersist) {
	raw, ok, err := st.LoadPoolState(poolStateAdmissions)
	if err != nil {
		p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", poolStateAdmissions, "error", err)
		return
	}
	if !ok {
		return
	}
	var adm map[string]int
	if err := json.Unmarshal(raw, &adm); err != nil {
		p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", poolStateAdmissions, "error", err)
		return
	}
	p.admissionsMu.Lock()
	defer p.admissionsMu.Unlock()
	if p.admissions == nil {
		p.admissions = make(map[string]int)
	}
	for m, idx := range adm {
		p.admissions[m] = idx
	}
}

func (p *Pool) restoreBurst(st PoolPersist, now time.Time) {
	raw, ok, err := st.LoadPoolState(poolStateBurst)
	if err != nil {
		p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", poolStateBurst, "error", err)
		return
	}
	if !ok {
		return
	}
	var stored map[string][]poolBurstHit
	if err := json.Unmarshal(raw, &stored); err != nil {
		p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", poolStateBurst, "error", err)
		return
	}
	window := defaultBurstWindow
	if cfg := p.cfg.Load(); cfg != nil {
		if w, _, _ := burstLimits(cfg); w > 0 {
			window = w
		}
	}
	cutoff := now.Add(-window)
	p.burstMu.Lock()
	defer p.burstMu.Unlock()
	if p.burstHits == nil {
		p.burstHits = make(map[string][]burstHit)
	}
	for m, hits := range stored {
		for _, h := range hits {
			if t := time.UnixMilli(h.At); t.After(cutoff) {
				p.burstHits[m] = append(p.burstHits[m], burstHit{at: t, tok: h.Tok})
			}
		}
	}
}

func (p *Pool) restoreBridge(st PoolPersist, now time.Time) {
	if raw, ok, err := st.LoadPoolState(poolStateBridgeUsage); err != nil {
		p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", poolStateBridgeUsage, "error", err)
	} else if ok {
		var usage int
		if err := json.Unmarshal(raw, &usage); err != nil {
			p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", poolStateBridgeUsage, "error", err)
		} else if usage > 0 {
			p.bridgeMu.Lock()
			p.bridgeDailyUsage = usage
			p.bridgeMu.Unlock()
		}
	}
	raw, ok, err := st.LoadPoolState(poolStateBridgeSurvivors)
	if err != nil {
		p.logger.Warn("pool: runtime persist restore failed (starting fresh)", "key", poolStateBridgeSurvivors, "error", err)
		return
	}
	if !ok {
		return
	}
	var stored []poolSurvivorBlob
	if err := json.Unmarshal(raw, &stored); err != nil {
		p.logger.Warn("pool: runtime persist row corrupt (starting fresh)", "key", poolStateBridgeSurvivors, "error", err)
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	for _, s := range stored {
		evicted := time.UnixMilli(s.Evicted)
		if now.Sub(evicted) < usageWindow && s.Count > 0 {
			p.bridgeSurvivors = append(p.bridgeSurvivors, bridgeSurvivor{count: s.Count, evicted: evicted})
			if len(p.bridgeSurvivors) >= maxBridgeSurvivors {
				break
			}
		}
	}
}
