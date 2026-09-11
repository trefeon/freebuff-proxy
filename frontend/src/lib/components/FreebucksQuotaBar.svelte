<script>
  import { formatLocalDate } from "../utils/format.js";
  import { tr } from "../i18n.js";
  let { freebucks = null, title = null, now = Date.now() } = $props();

  // ----- helpers -----
  function fmtRel(iso, nowMs) {
    if (!iso) return "—";
    const t = new Date(iso).getTime();
    if (isNaN(t)) return iso;
    const ms = t - nowMs;
    if (ms <= 0) return "now";
    const mins = Math.floor(ms / 60000);
    const h = Math.floor(mins / 60);
    const m = mins % 60;
    if (h >= 24) {
      const d = Math.floor(h / 24);
      const hr = h % 24;
      return hr > 0 ? `${d}d ${hr}h` : `${d}d`;
    }
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m`;
    return `${Math.max(1, Math.floor(ms / 1000))}s`;
  }

  function pctColor(p) {
    if (p >= 100) return "#ef4444";
    if (p >= 80) return "#f97316";
    if (p >= 60) return "#f59e0b";
    return "#10b981";
  }

  function fmtNum(v) {
    if (v == null || v === "") return "—";
    // Keep floats as-is but trim trailing zeros for display
    const n = Number(v);
    if (Number.isNaN(n)) return String(v);
    if (Number.isInteger(n)) return String(n);
    // Show up to 2 decimals, trim zeros
    return String(Math.round(n * 100) / 100);
  }

  // Normalise a FreebucksWindow that may be snake_case or camelCase
  function normalizeWindow(win) {
    if (!win) return null;
    const limit =
      win.limit ??
      win.Limit ??
      win.limit_usd ??
      win.limitUsd ??
      win.LimitUsd ??
      0;
    const spent =
      win.spent ??
      win.Spent ??
      win.spent_usd ??
      win.spentUsd ??
      win.SpentUsd ??
      0;
    const remRaw =
      win.remaining ??
      win.Remaining ??
      win.remaining_usd ??
      win.remainingUsd ??
      win.RemainingUsd;
    const remaining = remRaw != null ? remRaw : limit - spent;
    const resetAt =
      win.reset_at ??
      win.resetAt ??
      win.reset_at_utc ??
      win.resetAtUtc ??
      win.resetAtUTC ??
      null;
    let pct = win.percent_used ?? win.percentUsed ?? win.percent ?? null;
    if (pct == null && limit > 0) pct = (spent / limit) * 100;
    if (pct == null) pct = 0;
    pct = Math.min(100, Math.max(0, Number(pct) || 0));
    return { limit, spent, remaining, resetAt, pct };
  }

  // ----- Freebucks derived (issue #321 wire shape: daily pool + wallet +
  // spend ceiling + planId; the pre-drift weekly/monthly windows are gone) -----
  let hasFreebucks = $derived(!!freebucks);
  let fbBalance = $derived(freebucks?.balance ?? freebucks?.Balance ?? null);
  let fbDaily = $derived(normalizeWindow(freebucks?.daily ?? freebucks?.Daily));
  let fbMonthly = $derived(
    normalizeWindow(freebucks?.monthly ?? freebucks?.Monthly),
  );
  let fbPlanId = $derived(
    freebucks?.plan_id ?? freebucks?.planId ?? freebucks?.PlanID ?? null,
  );
  let fbExempt = $derived(
    freebucks?.quota_exempt ?? freebucks?.quotaExempt ?? false,
  );
  let fbClaimable = $derived(
    freebucks?.claimableGrantFreebucks ?? freebucks?.ClaimableGrant ?? 0,
  );
  let fbWallet = $derived.by(() => {
    const w = freebucks?.wallet ?? freebucks?.Wallet ?? null;
    if (!w) return null;
    return {
      balance: w.balance ?? w.Balance ?? 0,
      monthlyBonus: w.monthly_bonus ?? w.monthlyBonus ?? w.MonthlyBonus ?? 0,
      nextBonusAt: w.next_bonus_at ?? w.nextBonusAt ?? w.NextBonusAt ?? null,
    };
  });
  let fbWindows = $derived.by(() => {
    const w = [];
    if (fbDaily) w.push({ key: "daily", label: $tr("Daily"), win: fbDaily });
    if (fbMonthly)
      w.push({ key: "monthly", label: $tr("Monthly"), win: fbMonthly });
    return w;
  });

  let fbLabel = $derived(title ?? $tr("Freebucks"));
</script>

{#if hasFreebucks}
  <div
    class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3"
  >
    <!-- Header -->
    <div class="flex flex-wrap items-center justify-between gap-2 mb-3">
      <div class="flex items-center gap-2 min-w-0">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)] truncate"
        >
          {fbLabel}
        </p>
        {#if fbPlanId}
          <span
            class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            >plan {fbPlanId}</span
          >
        {/if}
        {#if fbWallet && fbWallet.monthlyBonus > 0}
          <span
            class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            >wallet {fmtNum(fbWallet.balance)} +{fmtNum(
              fbWallet.monthlyBonus,
            )}/mo</span
          >
        {/if}
        {#if fbClaimable > 0}
          <span
            class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            title={$tr(
              "Eligible earned grants admission may convert toward the session price",
            )}>{$tr("claimable")} {fmtNum(fbClaimable)}</span
          >
        {/if}
        {#if fbExempt}
          <span
            class="shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            title={$tr(
              "Server-authorized: new sessions stay usable at zero balance",
            )}>{$tr("quota exempt")}</span
          >
        {/if}
      </div>
      <div class="flex items-center gap-2 shrink-0">
        {#if fbBalance != null}
          <span
            class="fp-num text-xs font-medium text-[var(--fp-text)] tabular-nums"
            >{$tr("Balance")}
            <span class="text-[var(--fp-accent)]">{fmtNum(fbBalance)}</span
            ></span
          >
        {/if}
      </div>
    </div>

    <!-- Windows -->
    <div class="space-y-3">
      {#each fbWindows as item (item.key)}
        {@const w = item.win}
        {@const wPct = w.pct}
        {@const wColor = pctColor(wPct)}
        {@const wRel = fmtRel(w.resetAt, now)}
        {@const wReset = formatLocalDate(w.resetAt) || w.resetAt || "—"}
        <div
          class="rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40 p-2.5"
        >
          <div class="flex flex-wrap items-center justify-between gap-2 mb-1.5">
            <div class="flex items-center gap-1.5 min-w-0">
              <span
                class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)]"
                >{item.label}</span
              >
            </div>
            <span class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
              {#if wRel === "now"}
                {$tr("Reset")} {wReset}
              {:else}
                {$tr("Resets in")}
                {wRel} — {wReset}
              {/if}
            </span>
          </div>

          <div
            class="h-[6px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
            role="progressbar"
            aria-valuenow={wPct}
            aria-valuemin="0"
            aria-valuemax="100"
            aria-label="{item.label} {Math.round(wPct)}% used"
          >
            <div
              class="h-full rounded-full transition-all duration-300"
              style="width: {wPct}%; background: {wColor}"
            ></div>
          </div>

          <div
            class="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs"
          >
            <span class="fp-num text-[var(--fp-muted)] tabular-nums">
              {$tr("Used")}
              <span class="text-[var(--fp-text)] font-medium"
                >{fmtNum(w.spent)}</span
              >
              / {fmtNum(w.limit)}
              • {$tr("Remaining")}
              <span class="text-[var(--fp-text)] font-medium"
                >{fmtNum(w.remaining)}</span
              >
              • {Math.round(wPct * 100) / 100}%
            </span>
          </div>
        </div>
      {/each}
    </div>
  </div>
{/if}
