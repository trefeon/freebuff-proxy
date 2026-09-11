<script>
  import {
    ChevronUp,
    ChevronDown,
    ChevronDown as ChevronExpand,
    Unlock,
    Lock,
    Trash2,
  } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import TokenDetailsDrawer from "./TokenDetailsDrawer.svelte";
  import {
    statusFor,
    riskBadgeFor,
    cooldownLabel,
  } from "../utils/tokenStatus.js";
  import { tr } from "../i18n.js";

  /**
   * TokenCardMobile — stacked card layout of one pooled token for < lg
   * viewports. Identity + status + actions are always visible; secondary
   * columns (instance, cooldown) and the detail drawer live behind the
   * expand chevron, so nothing ever scrolls horizontally.
   *
   * @prop {object} token
   * @prop {number} idx
   * @prop {number} [totalTokens=1]
   * @prop {boolean} expanded
   * @prop {string} [spawnModel] — bindable
   * @prop {boolean} [actionPending]
   * @prop {boolean} [devToolsEnabled=false]
   * @prop {number} now
   * @prop {() => void} onToggle
   * @prop {(action: string) => void} onAction
   * @prop {(model: string) => void} onSpawn
   * @prop {(action: string) => void} onRefresh
   * @prop {(from: number, to: number) => void} [onSwap]
   */
  let {
    token,
    idx,
    totalTokens = 1,
    expanded,
    spawnModel = $bindable(""),
    actionPending,
    devToolsEnabled = false,
    now,
    onToggle,
    onAction,
    onSpawn,
    onRefresh,
    onDropSession,
    onSwap,
    dragging = false,
    dragOver = false,
    onDragStart,
    onDragOver,
    onDragLeave,
    onDrop,
    onDragEnd,
  } = $props();

  const st = $derived(statusFor(token));

  // Live session countdown (same tick model as the desktop card). Anchor to
  // the server's ABSOLUTE expiry when present (relative re-anchors every
  // poll); fall back for older backends.
  let nowTick = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => {
      nowTick = Date.now();
    }, 1000);
    return () => clearInterval(t);
  });
  const sessionEndsAtMs = $derived(
    token.session_expires_at
      ? new Date(token.session_expires_at).getTime()
      : Date.now() + (token.session_remaining_seconds || 0) * 1000,
  );
  const sessionRemaining = $derived(
    Math.max(0, Math.floor((sessionEndsAtMs - nowTick) / 1000)),
  );

  // Risk chip (moved from the standalone At-risk cards): shown when the
  // account carries a risk flag and no ban badge already claims the card.
  const riskBadge = $derived(riskBadgeFor(token));
</script>

<div
  draggable={totalTokens > 1 && !actionPending}
  ondragstart={(e) => onDragStart?.(e, idx)}
  ondragover={(e) => onDragOver?.(e, idx)}
  ondragleave={(e) => onDragLeave?.(e, idx)}
  ondrop={(e) => onDrop?.(e, idx)}
  ondragend={onDragEnd}
  title={totalTokens > 1 ? $tr("Drag to reorder account position") : undefined}
  aria-label={totalTokens > 1
    ? $tr("Draggable account card {index}", { index: idx + 1 })
    : undefined}
  class="fp-inset rounded p-3.5 flex flex-col gap-2.5 transition-all {totalTokens >
    1 && !actionPending
    ? 'cursor-grab active:cursor-grabbing'
    : ''} {dragging
    ? 'opacity-30 bg-[var(--fp-surface-2)]/60'
    : dragOver
      ? 'border-[var(--fp-accent)] ring-2 ring-[var(--fp-accent)] bg-[var(--fp-accent)]/5'
      : ''}"
>
  <!-- Header: identity, status cluster and email share one wrap row;
       reorder chevrons sit right. The whole card is the drag handle. -->
  <div class="flex items-center justify-between gap-2">
    <div class="min-w-0 flex items-center gap-1.5 flex-wrap">
      <span class="fp-num text-xs font-semibold text-[var(--fp-text)]"
        >Account #{idx + 1}</span
      >
      <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
      {#if riskBadge}
        <StatusBadge
          status={riskBadge.label}
          tone={riskBadge.tone}
          pulse={riskBadge.pulse}
        />
      {/if}
      {#if token.session_model}
        <StatusBadge tone="info" status={token.session_model} />
      {/if}
      {#if token.email || token.account_id}
        <span
          class="text-[11px] text-[var(--fp-muted)] truncate max-w-[160px]"
          title={token.email || token.account_id}
        >
          {token.email || token.account_id}
        </span>
      {/if}
    </div>
    <div class="flex items-center gap-1 shrink-0">
      {#if totalTokens > 1}
        <button
          type="button"
          disabled={actionPending || idx === 0}
          title={$tr("Move Up / Prioritize")}
          aria-label={$tr("Move Up")}
          onclick={() => onSwap?.(idx, idx - 1)}
          class="inline-flex items-center justify-center w-10 h-10 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
        >
          <ChevronUp size={16} />
        </button>
        <button
          type="button"
          disabled={actionPending || idx >= totalTokens - 1}
          title={$tr("Move Down")}
          aria-label={$tr("Move Down")}
          onclick={() => onSwap?.(idx, idx + 1)}
          class="inline-flex items-center justify-center w-10 h-10 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
        >
          <ChevronDown size={16} />
        </button>
      {/if}
    </div>
  </div>

  <!-- Usage stats: cooldown banner, then msgs / runs+reqs in one grid.
       Freebucks live on the Plans page. -->
  <div class="flex flex-col gap-2">
    {#if token.cooldown_active}
      {@const cd = cooldownLabel(token, now)}
      <div class="fp-inset px-2.5 py-1.5 text-xs text-[var(--fp-warning)]">
        {$tr("Cooldown")} —
        <span class="fp-num">{cd}</span>
        {#if cd !== "expiring" && cd !== "—"}{$tr("remaining")}{/if}
      </div>
    {/if}
    <div class="fp-inset px-2.5 py-2 text-xs">
      <div class="grid grid-cols-2 gap-2">
        <div class="min-w-0 text-[var(--fp-muted)]">
          {#if token.daily_limit > 0}
            <span class="fp-num text-[var(--fp-text)]"
              >{token.messages_24h}/{token.daily_limit}</span
            >
            {$tr("msgs today")}
            (<span class="fp-num">{token.usage_pct}%</span>)
          {:else}
            <span class="fp-num text-[var(--fp-text)]"
              >{token.messages_24h}</span
            >
            {$tr("msgs 24h")}
          {/if}
        </div>
        <div class="min-w-0 text-[var(--fp-dim)]">
          <span
            >runs <span class="fp-num text-[var(--fp-text)]"
              >{token.active_runs}</span
            ></span
          >
          <span class="ml-2">
            reqs <span class="fp-num text-[var(--fp-text)]"
              >{token.requests}</span
            >
            {#if token.requests_per_minute_limit > 0 || token.requests_per_day_limit > 0}
              <span class="text-[10px] text-[var(--fp-muted)]">
                ({#if token.requests_per_minute_limit > 0}{token.requests_per_minute}/{token.requests_per_minute_limit}m{/if}{#if token.requests_per_minute_limit > 0 && token.requests_per_day_limit > 0}
                  ·
                {/if}{#if token.requests_per_day_limit > 0}{token.requests_per_day}/{token.requests_per_day_limit}d{/if})</span
              >
            {/if}</span
          >
        </div>
      </div>
    </div>
  </div>

  <!-- Details (secondary info + drawer) behind the expand chevron -->
  {#if expanded}
    <div class="flex flex-col gap-2">
      <div class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-xs">
        <span class="text-[var(--fp-muted)]">{$tr("Instance")}</span>
        <span class="min-w-0">
          {#if token.session_instance}
            <code class="fp-num break-all select-all"
              >{token.session_instance}</code
            >
          {:else}
            <span class="text-[var(--fp-dim)]">—</span>
          {/if}
        </span>
      </div>
      <TokenDetailsDrawer
        {token}
        bind:spawnModel
        {actionPending}
        {devToolsEnabled}
        {onSpawn}
        {onRefresh}
        {onDropSession}
        {sessionRemaining}
      />
    </div>
  {/if}
  <!-- Footer: expand chevron left, actions right -->
  <div
    class="flex items-center justify-between gap-2 pt-0.5 border-t border-[var(--fp-border)]"
  >
    <button
      type="button"
      onclick={onToggle}
      aria-expanded={expanded}
      aria-label={expanded
        ? `Collapse details for account ${idx + 1}`
        : `Expand details for account ${idx + 1}`}
      class="inline-flex items-center justify-center w-11 h-11 shrink-0 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] transition-colors"
    >
      {#if expanded}
        <ChevronExpand size={17} class="rotate-180" />
      {:else}
        <ChevronDown size={17} class="rotate-[-90deg]" />
      {/if}
    </button>
    <div
      class="flex items-center gap-1.5 flex-wrap justify-end [&_.fp-btn]:min-h-[44px]"
    >
      {#if token.cooldown_active}
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction("clear")}
        >
          <Unlock size={13} />
          <span>{$tr("Clear")}</span>
        </Button>
      {/if}
      {#if token.locked}
        <Button
          variant="secondary"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction("unlock")}
        >
          <Unlock size={13} />
          <span>{$tr("Unlock")}</span>
        </Button>
      {:else}
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction("lock")}
        >
          <Lock size={13} />
          <span>{$tr("Lock")}</span>
        </Button>
      {/if}
      <Button
        variant="danger"
        size="sm"
        disabled={actionPending}
        onclick={() => onAction("remove")}
      >
        <Trash2 size={13} />
        <span>{$tr("Remove")}</span>
      </Button>
    </div>
  </div>
</div>
