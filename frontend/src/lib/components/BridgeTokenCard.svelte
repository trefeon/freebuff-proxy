<script>
  import StatusBadge from "./StatusBadge.svelte";
  import PremiumQuotaBar from "./PremiumQuotaBar.svelte";
  import { formatLocalDate } from "../utils/format.js";
  import { tr } from "../i18n.js";

  /**
   * BridgeTokenCard — display-only compact card for one live bridge client.
   *
   * @prop {object} card — bridgeTokenCard from the tokens/overview endpoints
   * @prop {number} [now=Date.now()]
   */
  let { card, now = Date.now() } = $props();

  let pct = $derived(Math.min(100, Math.max(0, card.spend_pct ?? 0)));
  let barColor = $derived(
    pct >= 100
      ? "#ef4444"
      : pct >= 80
        ? "#f97316"
        : pct >= 60
          ? "#f59e0b"
          : "#10b981",
  );
  let cooldown = $derived(
    card.cooldown_until ? formatLocalDate(card.cooldown_until) : "—",
  );
  let banUntil = $derived(
    card.banned_until ? formatLocalDate(card.banned_until) : "",
  );
</script>

<div
  class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3 flex flex-col gap-2.5"
>
  <div class="flex flex-wrap items-center gap-2 min-w-0">
    <StatusBadge status={card.status} />
    <code class="fp-num text-xs font-mono text-[var(--fp-text)] truncate"
      >{card.key}</code
    >
    {#if card.model}
      <code
        class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]"
        >{card.model}</code
      >
    {/if}
    {#if card.locked}
      <span
        class="shrink-0 text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-[#f59e0b]/15 text-[#f59e0b] border border-[#f59e0b]/30"
        >{$tr("Locked")}</span
      >
    {/if}
  </div>

  <div class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
    <span class="fp-num text-[var(--fp-muted)]">
      {$tr("Requests")}
      <span class="text-[var(--fp-text)] font-medium">{card.requests ?? 0}</span
      >
    </span>
    <span class="fp-num text-[var(--fp-muted)]">
      {$tr("Active runs")}
      <span class="text-[var(--fp-text)] font-medium"
        >{card.active_runs ?? 0}</span
      >
    </span>
    <span class="fp-num text-[var(--fp-muted)]">
      {$tr("Session active")}
      <span class="text-[var(--fp-text)] font-medium"
        >{card.session_active ? $tr("yes") : $tr("no")}</span
      >
    </span>
    <span class="fp-num text-[var(--fp-muted)]">
      {$tr("Cooldown until")}
      <span class="text-[var(--fp-text)] font-medium">{cooldown}</span>
    </span>
  </div>

  <div>
    <div class="flex items-center justify-between gap-2 mb-1">
      <p
        class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)]"
      >
        {$tr("Spend today")}
      </p>
      <span
        class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)] tabular-nums"
        >{card.spend_day ?? 0}</span
      >
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
  </div>

  {#if card.ban_type || card.banned_until}
    <p class="fp-num text-xs text-[var(--fp-error)]">
      {$tr("Banned")}
      {#if card.ban_type}<span class="uppercase tracking-wider">
          — {card.ban_type}</span
        >{/if}
      {#if banUntil}<span> — {banUntil}</span>{/if}
    </p>
  {/if}

  {#if card.premium_quota}
    <PremiumQuotaBar
      quota={card.premium_quota}
      title={$tr("Premium pool")}
      {now}
    />
  {/if}
</div>
