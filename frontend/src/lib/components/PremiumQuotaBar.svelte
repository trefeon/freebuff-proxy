<script>
  import { tr } from "../i18n.js";

  let { quota = null, freebucks = null, title = null, now = Date.now() } = $props();

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
    const limit = win.limit ?? win.Limit ?? 0;
    const spent = win.spent ?? win.Spent ?? 0;
    const remRaw = win.remaining ?? win.Remaining;
    const remaining = remRaw != null ? remRaw : limit - spent;
    const resetAt =
      win.reset_at ?? win.resetAt ?? win.reset_at_utc ?? win.resetAtUtc ?? win.resetAtUTC ?? null;
    let pct = win.percent_used ?? win.percentUsed ?? win.percent ?? null;
    if (pct == null && limit > 0) pct = (spent / limit) * 100;
    if (pct == null) pct = 0;
    pct = Math.min(100, Math.max(0, Number(pct) || 0));
    return { limit, spent, remaining, resetAt, pct };
  }

  // ----- Freebucks derived -----
  let hasFreebucks = $derived(!!freebucks);
  let fbBalance = $derived(
    freebucks?.balance ?? freebucks?.Balance ?? null
  );
  let fbBinding = $derived(
    ((freebucks?.binding_window ?? freebucks?.bindingWindow ?? freebucks?.BindingWindow ?? "") + "").toLowerCase()
  );
  let fbPlanDaily = $derived(
    freebucks?.plan_daily ?? freebucks?.planDaily ?? freebucks?.PlanDaily ?? null
  );
  let fbPrices = $derived(
    freebucks?.prices ?? freebucks?.Prices ?? null
  );
  let fbDaily = $derived(normalizeWindow(freebucks?.daily ?? freebucks?.Daily));
  let fbWeekly = $derived(normalizeWindow(freebucks?.weekly ?? freebucks?.Weekly));
  let fbMonthly = $derived(normalizeWindow(freebucks?.monthly ?? freebucks?.Monthly));

  let fbWindows = $derived.by(() => {
    const w = [];
    if (fbDaily) w.push({ key: "daily", label: "Daily", win: fbDaily });
    if (fbWeekly) w.push({ key: "weekly", label: "Weekly", win: fbWeekly });
    if (fbMonthly) w.push({ key: "monthly", label: "Monthly", win: fbMonthly });
    return w;
  });

  let fbLabel = $derived(title ?? $tr("Freebucks"));
  let fbBindingLabel = $derived(fbBinding ? fbBinding : "—");
  // ----- Legacy quota derived (fallback) -----
  let pct = $derived(Math.min(100, Math.max(0, quota?.percent_used ?? quota?.percentUsed ?? quota?.percent ?? 0)));
  let barColor = $derived(pctColor(pct));
  let rel = $derived(fmtRel(quota?.reset_at ?? quota?.resetAt ?? quota?.reset_at_utc ?? quota?.resetAtUtc, now));
  let badge = $derived(
    `${quota?.limit ?? "—"}/day ${quota?.period ?? ""}`.trim(),
  );
  let label = $derived(title ?? $tr("Premium pool"));
</script>

{#if hasFreebucks}
  <div class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3">
    <!-- Header -->
    <div class="flex flex-wrap items-center justify-between gap-2 mb-3">
      <div class="flex items-center gap-2 min-w-0">
        <p class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)] truncate">
          {fbLabel}
        </p>
        {#if fbBinding}
          <span
            class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            >binding: {fbBindingLabel}</span
          >
        {/if}
        {#if fbPlanDaily != null}
          <span
            class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
            >plan {fmtNum(fbPlanDaily)}/day</span
          >
        {/if}
      </div>
      <div class="flex items-center gap-2 shrink-0">
        {#if fbBalance != null}
          <span
            class="fp-num text-xs font-medium text-[var(--fp-text)] tabular-nums"
            >{$tr("Balance")} <span class="text-[var(--fp-accent)]">{fmtNum(fbBalance)}</span></span
          >
        {/if}
      </div>
    </div>

    <!-- Windows -->
    <div class="space-y-3">
      {#each fbWindows as item (item.key)}
        {@const isBinding = fbBinding === item.key}
        {@const w = item.win}
        {@const wPct = w.pct}
        {@const wColor = pctColor(wPct)}
        {@const wRel = fmtRel(w.resetAt, now)}
        <div
          class="rounded border {isBinding
            ? 'border-[var(--fp-accent)]/40 bg-[var(--fp-accent)]/5'
            : 'border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/40'} p-2.5"
        >
          <div class="flex flex-wrap items-center justify-between gap-2 mb-1.5">
            <div class="flex items-center gap-1.5 min-w-0">
              <span
                class="text-xs font-semibold uppercase tracking-wider {isBinding
                  ? 'text-[var(--fp-accent)]'
                  : 'text-[var(--fp-text)]'}"
                >{item.label}</span
              >
              {#if isBinding}
                <span
                  class="shrink-0 text-[10px] leading-none px-1 py-0.5 rounded bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30 uppercase tracking-wider font-semibold"
                  >{$tr("binding")}</span
                >
              {/if}
            </div>
            <span class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
              {$tr("Resets in")} {wRel} — {w.resetAt ?? "—"}
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

          <div class="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            <span class="fp-num text-[var(--fp-muted)] tabular-nums">
              {$tr("Used")} <span class="text-[var(--fp-text)] font-medium">{fmtNum(w.spent)}</span>
              / {fmtNum(w.limit)}
              • {$tr("Remaining")} <span class="text-[var(--fp-text)] font-medium">{fmtNum(w.remaining)}</span>
              • {Math.round(wPct * 100) / 100}%
            </span>
          </div>
        </div>
      {/each}
    </div>

    {#if fbPrices && typeof fbPrices === "object" && Object.keys(fbPrices).length > 0}
      <div class="mt-3 fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
        <span class="font-semibold uppercase tracking-wider text-[var(--fp-muted)]">{$tr("Prices")}: </span>
        {#each Object.entries(fbPrices).slice(0, 4) as [model, price] (model)}
          <span class="inline-flex items-center gap-1 mr-3">
            <code class="text-[var(--fp-text)] text-[11px]">{model}</code>
            <span class="text-[var(--fp-accent)]">{fmtNum(price)}</span>
          </span>
        {/each}
        {#if Object.keys(fbPrices).length > 4}
          <span class="text-[var(--fp-dim)]">+{Object.keys(fbPrices).length - 4} {$tr("more")}</span>
        {/if}
      </div>
    {/if}
  </div>
{:else if quota}
  <div class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3">
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
