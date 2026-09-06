<script>
  import {
    ChevronUp,
    ChevronDown,
    ChevronRight,
    Unlock,
    Lock,
    Trash2,
    GripVertical,
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
   * TokenCard — desktop (<tr>) row of one pooled token for the md+ table.
   * Mobile uses TokenCardMobile; the expanded drawer is shared via
   * TokenDetailsDrawer.
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
   * @prop {() => void} [onDropSession]
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

  // Risk chip (moved from the standalone At-risk cards): shown when the
  // account carries a risk flag and no ban badge already claims the row.
  const riskBadge = $derived(riskBadgeFor(token));

  // Live session countdown (freebuff TUI parity). Anchor to the server's
  // ABSOLUTE expiry when the snapshot carries one (issue: a relative
  // `now + remaining_seconds` re-anchors on every poll and freezes the
  // timer at the admission value); fall back to the relative form for
  // older backends.
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

  const st = $derived(statusFor(token));
</script>

<tr
  draggable={totalTokens > 1 && !actionPending}
  ondragstart={(e) => onDragStart?.(e, idx)}
  ondragover={(e) => onDragOver?.(e, idx)}
  ondragleave={(e) => onDragLeave?.(e, idx)}
  ondrop={(e) => onDrop?.(e, idx)}
  ondragend={onDragEnd}
  class="transition-colors {dragging
    ? 'opacity-30 bg-[var(--fp-surface-2)]/60'
    : dragOver
      ? 'bg-[var(--fp-accent)]/15 border-y-2 border-[var(--fp-accent)]'
      : ''}"
>
  <td class="w-[84px]">
    <div class="inline-flex items-center gap-1">
      {#if totalTokens > 1}
        <div
          class="cursor-grab active:cursor-grabbing p-1 text-[var(--fp-dim)] hover:text-[var(--fp-accent)] rounded select-none hover:bg-[var(--fp-surface-2)] transition-colors"
          title={$tr("Drag to reorder account position")}
          aria-label={$tr("Drag to reorder")}
        >
          <GripVertical size={15} />
        </div>
        <div class="flex flex-col shrink-0 -my-1">
          <button
            type="button"
            disabled={actionPending || idx === 0}
            title={$tr("Move Up / Prioritize")}
            aria-label={$tr("Move Up")}
            onclick={() => onSwap?.(idx, idx - 1)}
            class="inline-flex items-center justify-center w-5 h-5 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
          >
            <ChevronUp size={13} />
          </button>
          <button
            type="button"
            disabled={actionPending || idx >= totalTokens - 1}
            title={$tr("Move Down")}
            aria-label={$tr("Move Down")}
            onclick={() => onSwap?.(idx, idx + 1)}
            class="inline-flex items-center justify-center w-5 h-5 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
          >
            <ChevronDown size={13} />
          </button>
        </div>
      {/if}
      <button
        type="button"
        onclick={onToggle}
        aria-expanded={expanded}
        aria-label={expanded
          ? `Collapse details for account ${idx + 1}`
          : `Expand details for account ${idx + 1}`}
        class="inline-flex items-center justify-center w-8 h-8 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] transition-colors"
      >
        {#if expanded}
          <ChevronDown size={16} />
        {:else}
          <ChevronRight size={16} />
        {/if}
      </button>
    </div>
  </td>
  <td>
    <div class="flex flex-col gap-0.5">
      <div class="flex items-center gap-1.5">
        <span
          class="fp-num text-xs font-semibold whitespace-nowrap text-[var(--fp-text)]"
          >Account #{idx + 1}</span
        >
        {#if idx === 0}
          <span
            class="inline-flex items-center rounded px-1.5 py-0.2 text-[10px] font-semibold bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30"
          >
            {$tr("Primary")}
          </span>
        {/if}
      </div>
      {#if token.email || token.account_id}
        <span
          class="text-[11px] text-[var(--fp-muted)] truncate max-w-[160px]"
          title={token.email || token.account_id}
        >
          {token.email || token.account_id}
        </span>
      {/if}
    </div>
  </td>
  <td>
    <div class="flex items-center gap-1.5">
      <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
      {#if riskBadge}
        <StatusBadge
          status={riskBadge.label}
          tone={riskBadge.tone}
          pulse={riskBadge.pulse}
        />
      {/if}
    </div>
  </td>
  <td>
    {#if token.session_instance}
      <code
        class="fp-num text-xs text-[var(--fp-muted)] truncate block max-w-full select-all"
        title={token.session_instance}>{token.session_instance}</code
      >
    {:else}
      <span class="text-xs text-[var(--fp-dim)]">—</span>
    {/if}
  </td>
  <td class="num">
    {#if token.cooldown_active}
      {@const cd = cooldownLabel(token, now)}
      <span class="fp-num text-xs text-[var(--fp-warning)] whitespace-nowrap">
        {cd}{#if cd !== "expiring" && cd !== "—"}{" " + $tr("remaining")}{/if}
      </span>
    {:else}
      <span class="fp-num text-xs text-[var(--fp-dim)]">—</span>
    {/if}
  </td>
  <td class="num">
    <div class="flex flex-col items-end gap-0.5">
      <span class="text-xs text-[var(--fp-muted)]">
        {#if token.daily_limit > 0}
          <span class="fp-num text-[var(--fp-text)]"
            >{token.messages_24h}/{token.daily_limit}</span
          >
          {$tr("msgs today")}
          (<span class="fp-num text-[var(--fp-text)]">{token.usage_pct}%</span>)
        {:else}
          <span class="fp-num text-[var(--fp-text)]">{token.messages_24h}</span>
          {$tr("msgs 24h")}
        {/if}
      </span>
      <span class="text-[11px] text-[var(--fp-dim)]">
        runs <span class="fp-num text-[var(--fp-text)]"
          >{token.active_runs}</span
        >
        · reqs
        <span class="fp-num text-[var(--fp-text)]">{token.requests}</span>
        {#if token.requests_per_minute_limit > 0 || token.requests_per_day_limit > 0}
          <span class="text-[10px] text-[var(--fp-muted)]">
            ({#if token.requests_per_minute_limit > 0}<span
                title={$tr(
                  "Admitted requests in the last 60s / per-minute cap",
                )}
                >{token.requests_per_minute}/{token.requests_per_minute_limit}m</span
              >{/if}{#if token.requests_per_minute_limit > 0 && token.requests_per_day_limit > 0}
              ·
            {/if}{#if token.requests_per_day_limit > 0}<span
                title={$tr("Successful requests today / per-day cap")}
                >{token.requests_per_day}/{token.requests_per_day_limit}d</span
              >{/if})
          </span>
        {/if}
      </span>
    </div>
  </td>
  <td class="text-right whitespace-nowrap">
    <div class="inline-flex items-center gap-1.5 justify-end">
      {#if token.cooldown_active}
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction("clear")}
          title={$tr("Clear cooldown")}
        >
          <Unlock size={13} />
          <span class="hidden @min-[840px]:inline">{$tr("Clear")}</span>
        </Button>
      {/if}
      {#if token.locked}
        <Button
          variant="secondary"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction("unlock")}
          title={$tr("Unlock")}
        >
          <Unlock size={13} />
          <span class="hidden @min-[840px]:inline">{$tr("Unlock")}</span>
        </Button>
      {:else}
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction("lock")}
          title={$tr("Lock")}
        >
          <Lock size={13} />
          <span class="hidden @min-[840px]:inline">{$tr("Lock")}</span>
        </Button>
      {/if}
      <Button
        variant="danger"
        size="sm"
        disabled={actionPending}
        onclick={() => onAction("remove")}
        title={$tr("Remove")}
      >
        <Trash2 size={13} />
        <span class="hidden @min-[840px]:inline">{$tr("Remove")}</span>
      </Button>
    </div>
  </td>
</tr>
{#if expanded}
  <tr>
    <td colspan="7" class="!p-0">
      <div class="m-2">
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
    </td>
  </tr>
{/if}
