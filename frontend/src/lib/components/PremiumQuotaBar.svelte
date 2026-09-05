<script>
  import { tr } from "../i18n.js";
  import { ChevronDown, ChevronUp } from "@lucide/svelte";

  let {
    quota = null,
    freebucks = null,
    freeWindows = null,
    subscription = null,
    title = null,
    now = Date.now(),
  } = $props();

  let poolsExpanded = $state(false);

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
  let fbWallet = $derived.by(() => {
    const w = freebucks?.wallet ?? freebucks?.Wallet ?? null;
    if (!w) return null;
    return {
      balance: w.balance ?? w.Balance ?? 0,
      monthlyBonus: w.monthly_bonus ?? w.monthlyBonus ?? w.MonthlyBonus ?? 0,
      nextBonusAt: w.next_bonus_at ?? w.nextBonusAt ?? w.NextBonusAt ?? null,
    };
  });
  let fbSpend = $derived.by(() => {
    const s = freebucks?.spend ?? freebucks?.Spend ?? null;
    if (!s) return null;
    return {
      limitUsd: s.limit_usd ?? s.limitUsd ?? s.LimitUsd ?? 0,
      resetAt: s.reset_at ?? s.resetAt ?? s.ResetAt ?? null,
    };
  });
  let fbPrices = $derived(freebucks?.prices ?? freebucks?.Prices ?? null);

  let fbWindows = $derived.by(() => {
    const w = [];
    if (fbDaily) w.push({ key: "daily", label: $tr("Daily"), win: fbDaily });
    if (fbMonthly)
      w.push({ key: "monthly", label: $tr("Monthly"), win: fbMonthly });
    return w;
  });

  let fbLabel = $derived(title ?? $tr("Freebucks"));

  // ----- Free session windows (issue #319) -----
  let fw = $derived(freeWindows ?? (freebucks ? null : null) ?? null);
  let fwWindows = $derived.by(() => {
    if (!fw) return [];
    const w = [];
    if (fw.day_limit != null)
      w.push({
        key: "day",
        label: $tr("Day"),
        used: fw.day_used,
        limit: fw.day_limit,
        resetAt: fw.day_reset_at,
      });
    if (fw.week_limit != null)
      w.push({
        key: "week",
        label: $tr("Week"),
        used: fw.week_used,
        limit: fw.week_limit,
        resetAt: null,
      });
    if (fw.month_limit != null)
      w.push({
        key: "month",
        label: $tr("Month"),
        used: fw.month_used,
        limit: fw.month_limit,
        resetAt: fw.month_reset_at,
      });
    return w;
  });
  let fwPct = (used, limit) =>
    limit > 0 ? Math.min(100, Math.max(0, (used / limit) * 100)) : 0;

  // ----- Subscription windows (issue #319) -----
  let sub = $derived(subscription ?? null);
  let subWindows = $derived.by(() => {
    if (!sub) return [];
    const w = [];
    if (sub.day_limit != null)
      w.push({
        key: "day",
        label: $tr("Day"),
        used: sub.day_used,
        limit: sub.day_limit,
        resetAt: sub.day_reset_at,
      });
    if (sub.five_day_limit != null)
      w.push({
        key: "5d",
        label: $tr("5-day"),
        used: sub.five_day_used,
        limit: sub.five_day_limit,
        resetAt: null,
      });
    if (sub.month_limit != null)
      w.push({
        key: "month",
        label: $tr("Month"),
        used: sub.month_used,
        limit: sub.month_limit,
        resetAt: sub.period_ends_at,
      });
    if (sub.day_premium_limit != null)
      w.push({
        key: "premium",
        label: $tr("Premium"),
        used: sub.day_premium_used,
        limit: sub.day_premium_limit,
        resetAt: null,
      });
    return w;
  });
  let subSpend = $derived(
    sub && sub.month_spend_limit_usd != null
      ? {
          used: sub.month_spend_usd,
          limit: sub.month_spend_limit_usd,
          pct:
            sub.month_spend_limit_usd > 0
              ? Math.min(
                  100,
                  Math.max(
                    0,
                    (sub.month_spend_usd / sub.month_spend_limit_usd) * 100,
                  ),
                )
              : 0,
        }
      : null,
  );
  // ----- Legacy quota derived (fallback) -----
  let pct = $derived(
    Math.min(
      100,
      Math.max(
        0,
        quota?.percent_used ?? quota?.percentUsed ?? quota?.percent ?? 0,
      ),
    ),
  );
  let barColor = $derived(pctColor(pct));
  let rel = $derived(
    fmtRel(
      quota?.reset_at ??
        quota?.resetAt ??
        quota?.reset_at_utc ??
        quota?.resetAtUtc,
      now,
    ),
  );
  let badge = $derived(
    `${quota?.limit ?? "—"}/day ${quota?.period ?? ""}`.trim(),
  );
  let label = $derived(title ?? $tr("Premium pool"));
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
        {#if fbExempt}
          <span
            class="shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            title={$tr(
              "Server-authorized: new sessions stay usable at zero balance",
            )}>{$tr("quota exempt")}</span
          >
        {/if}
        {#if fbSpend && fbSpend.limitUsd > 0}
          <span
            class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            >ceiling ${fmtNum(fbSpend.limitUsd)}</span
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
              {$tr("Resets in")}
              {wRel} — {w.resetAt ?? "—"}
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

    {#if fbPrices && typeof fbPrices === "object" && Object.keys(fbPrices).length > 0}
      <div class="mt-3 fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
        <span
          class="font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
          >{$tr("Prices")}:
        </span>
        {#each Object.entries(fbPrices).slice(0, 4) as [model, price] (model)}
          <span class="inline-flex items-center gap-1 mr-3">
            <code class="text-[var(--fp-text)] text-[11px]">{model}</code>
            <span class="text-[var(--fp-accent)]">{fmtNum(price)}</span>
          </span>
        {/each}
        {#if Object.keys(fbPrices).length > 4}
          <span class="text-[var(--fp-dim)]"
            >+{Object.keys(fbPrices).length - 4} {$tr("more")}</span
          >
        {/if}
      </div>
    {/if}
  </div>
{/if}
{#if fwWindows.length > 0 || subWindows.length > 0 || subSpend}
  <div
    class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3"
  >
    <div class="flex flex-wrap items-center justify-between gap-2 mb-3">
      <button
        type="button"
        class="flex items-center gap-2 min-w-0"
        aria-expanded={poolsExpanded}
        aria-controls={poolsExpanded ? "free-session-pools-details" : undefined}
        onclick={() => (poolsExpanded = !poolsExpanded)}
      >
        <span
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)] truncate"
        >
          {$tr("Free session pools")}
        </span>
        <span class="text-[var(--fp-dim)] shrink-0">
          {#if poolsExpanded}
            <ChevronUp size={14} />
          {:else}
            <ChevronDown size={14} />
          {/if}
        </span>
      </button>
      {#if subSpend}
        <span
          class="fp-num text-xs font-medium text-[var(--fp-text)] tabular-nums"
          >{$tr("Spend")}
          <span class="text-[var(--fp-accent)]">${fmtNum(subSpend.used)}</span>
          / ${fmtNum(subSpend.limit)}</span
        >
      {/if}
    </div>
    {#if !poolsExpanded}
      <div class="flex flex-wrap items-center gap-1.5 text-[11px]">
        {#each fwWindows as item (item.key)}
          {@const wp = fwPct(item.used, item.limit)}
          <span
            class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40 tabular-nums"
          >
            <span
              class="font-semibold uppercase tracking-wider text-[var(--fp-text)]"
              >{item.label}</span
            >
            <span class="text-[var(--fp-muted)]"
              >{fmtNum(item.used)}/{fmtNum(item.limit)}</span
            >
            <span style="color: {pctColor(wp)}"
              >{Math.round(wp * 100) / 100}%</span
            >
          </span>
        {/each}
        {#each subWindows as item (item.key)}
          {@const wp = fwPct(item.used, item.limit)}
          <span
            class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40 tabular-nums"
          >
            <span
              class="font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
              >{$tr("Sub")} {item.label}</span
            >
            <span class="text-[var(--fp-muted)]"
              >{fmtNum(item.used)}/{fmtNum(item.limit)}</span
            >
            <span style="color: {pctColor(wp)}"
              >{Math.round(wp * 100) / 100}%</span
            >
          </span>
        {/each}
      </div>
    {:else}
      <div id="free-session-pools-details" class="space-y-3">
        {#each fwWindows as item (item.key)}
          {@const wp = fwPct(item.used, item.limit)}
          {@const wc = pctColor(wp)}
          {@const wr = fmtRel(item.resetAt, now)}
          <div
            class="rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40 p-2.5"
          >
            <div
              class="flex flex-wrap items-center justify-between gap-2 mb-1.5"
            >
              <span
                class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)]"
                >{item.label}</span
              >
              <span
                class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums"
              >
                {$tr("Resets in")}
                {wr} — {item.resetAt ?? "—"}
              </span>
            </div>
            <div
              class="h-[6px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
              role="progressbar"
              aria-valuenow={Math.round(wp)}
              aria-valuemin="0"
              aria-valuemax="100"
              aria-label="{item.label} {Math.round(wp)}% used"
            >
              <div
                class="h-full rounded-full transition-all duration-300"
                style="width: {wp}%; background: {wc}"
              ></div>
            </div>
            <div
              class="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs"
            >
              <span class="fp-num text-[var(--fp-muted)] tabular-nums">
                {$tr("Used")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{fmtNum(item.used)}</span
                >
                / {fmtNum(item.limit)} • {Math.round(wp * 100) / 100}%
              </span>
            </div>
          </div>
        {/each}
        {#each subWindows as item (item.key)}
          {@const wp = fwPct(item.used, item.limit)}
          {@const wc = pctColor(wp)}
          {@const wr = fmtRel(item.resetAt, now)}
          <div
            class="rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40 p-2.5"
          >
            <div
              class="flex flex-wrap items-center justify-between gap-2 mb-1.5"
            >
              <span
                class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)]"
                >{$tr("Subscription")} {item.label}</span
              >
              <span
                class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums"
              >
                {$tr("Resets in")}
                {wr}
              </span>
            </div>
            <div
              class="h-[6px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
              role="progressbar"
              aria-valuenow={Math.round(wp)}
              aria-valuemin="0"
              aria-valuemax="100"
              aria-label="{item.label} {Math.round(wp)}% used"
            >
              <div
                class="h-full rounded-full transition-all duration-300"
                style="width: {wp}%; background: {wc}"
              ></div>
            </div>
            <div
              class="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs"
            >
              <span class="fp-num text-[var(--fp-muted)] tabular-nums">
                {$tr("Used")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{fmtNum(item.used)}</span
                >
                / {fmtNum(item.limit)} • {Math.round(wp * 100) / 100}%
              </span>
            </div>
          </div>
        {/each}
        {#if subSpend}
          <div
            class="rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40 p-2.5"
          >
            <div
              class="h-[6px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
              role="progressbar"
              aria-valuenow={Math.round(subSpend.pct)}
              aria-valuemin="0"
              aria-valuemax="100"
              aria-label="{$tr('Monthly spend')} {Math.round(
                subSpend.pct,
              )}% used"
            >
              <div
                class="h-full rounded-full transition-all duration-300"
                style="width: {subSpend.pct}%; background: {pctColor(
                  subSpend.pct,
                )}"
              ></div>
            </div>
            <div
              class="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs"
            >
              <span class="fp-num text-[var(--fp-muted)] tabular-nums">
                {$tr("Monthly spend")}
                <span class="text-[var(--fp-text)] font-medium"
                  >${fmtNum(subSpend.used)}</span
                >
                / ${fmtNum(subSpend.limit)} • {Math.round(subSpend.pct * 100) /
                  100}%
              </span>
            </div>
          </div>
        {/if}
      </div>
    {/if}
  </div>
{/if}
{#if quota && !hasFreebucks && fwWindows.length === 0 && subWindows.length === 0 && !subSpend}
  <div
    class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3"
  >
    <div class="flex items-center justify-between gap-2 mb-2">
      <div class="flex items-center gap-2 min-w-0">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)] truncate"
        >
          {label}
        </p>
        <span
          class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
          >{badge}</span
        >
      </div>
      {#if quota?.capped}
        <span
          class="shrink-0 text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-[#ef4444]/15 text-[#ef4444] border border-[#ef4444]/30"
          >{$tr("Quota exhausted")}</span
        >
      {/if}
    </div>

    <div
      class="h-[6px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
      role="progressbar"
      aria-valuenow={pct}
      aria-valuemin="0"
      aria-valuemax="100"
    >
      <div
        class="h-full rounded-full transition-all duration-300"
        style="width: {pct}%; background: {barColor}"
      ></div>
    </div>

    <div class="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
      <span class="fp-num text-[var(--fp-muted)] tabular-nums">
        Used <span class="text-[var(--fp-text)] font-medium"
          >{quota?.used ?? "—"}</span
        >
        / Limit
        <span class="text-[var(--fp-text)] font-medium"
          >{quota?.limit ?? "—"}</span
        >
        • {$tr("Remaining")}
        <span class="text-[var(--fp-text)] font-medium"
          >{quota?.remaining ?? "—"}</span
        >
      </span>
    </div>

    <div class="mt-1 fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
      {$tr("Resets in")}
      {rel} — {quota?.reset_at ?? quota?.resetAt ?? "—"}
    </div>
  </div>
{/if}
