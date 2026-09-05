<script>
  import {
    ChevronUp,
    ChevronDown,
    ChevronDown as ChevronExpand,
    Unlock,
    Lock,
    Trash2,
    GripVertical,
  } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import TokenDetailsDrawer from "./TokenDetailsDrawer.svelte";
  import { tr } from "../i18n.js";

  /**
   * TokenCardMobile — stacked card layout of one pooled token for < md
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

  function banBadge(token) {
    if (token.ban_type === "hard") {
      return {
        label: $tr("banned — appeal required"),
        tone: "critical",
        pulse: true,
      };
    }
    if (token.ban_type === "temporary") {
      return { label: $tr("banned (temporary)"), tone: "bad" };
    }
    return null;
  }

  function statusFor(token) {
    const ban = banBadge(token);
    if (ban) return ban;
    if (token.locked) return { label: $tr("locked"), tone: "warn" };
    if (token.cooldown_active) return { label: $tr("cooldown"), tone: "warn" };
    const s = token.session_status || "";
    if (s === "active")
      return { label: $tr("leased"), tone: "good", pulse: true };
    if (s === "queued") return { label: $tr("queued"), tone: "info" };
    if (s === "banned") return { label: $tr("banned"), tone: "bad" };
    if (s === "expired") return { label: $tr("expired"), tone: "idle" };
    if (s === "grace") return { label: $tr("grace drain"), tone: "warn" };
    return { label: $tr("idle"), tone: "idle" };
  }

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

  function cooldownLabel(token) {
    if (!token.cooldown_active || !token.cooldown_until) return "—";
    const ms = new Date(token.cooldown_until).getTime() - now;
    if (ms <= 0) return "expiring";
    const s = Math.floor(ms / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h >= 24) {
      const d = Math.floor(h / 24);
      const hr = h % 24;
      return hr > 0 ? `${d}d ${hr}h` : `${d}d`;
    }
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${sec}s`;
    return `${sec}s`;
  }

  function riskTone(risk) {
    switch (risk) {
      case "low":
        return "good";
      case "moderate":
        return "warn";
      case "high":
      case "critical":
        return "bad";
      default:
        return "idle";
    }
  }

  // Risk chip (moved from the standalone At-risk cards): shown when the
  // account carries a risk flag and no ban badge already claims the card.
  const riskBadge = $derived(
    banBadge(token)
      ? null
      : token.risk_level && token.risk_level !== "low"
        ? {
            label: token.risk_level,
            tone: riskTone(token.risk_level),
            pulse: token.risk_level === "critical",
          }
        : null,
  );
</script>

<div
  draggable={totalTokens > 1 && !actionPending}
  ondragstart={(e) => onDragStart?.(e, idx)}
  ondragover={(e) => onDragOver?.(e, idx)}
  ondragleave={(e) => onDragLeave?.(e, idx)}
  ondrop={(e) => onDrop?.(e, idx)}
  ondragend={onDragEnd}
  class="fp-inset rounded-lg p-3.5 flex flex-col gap-2.5 transition-all {dragging
    ? 'opacity-30 bg-[var(--fp-surface-2)]/60'
    : dragOver
      ? 'border-[var(--fp-accent)] ring-2 ring-[var(--fp-accent)] bg-[var(--fp-accent)]/5'
      : ''}"
>
  <!-- Header: identity + status + expand -->
  <div class="flex items-start justify-between gap-2">
    <div class="min-w-0 flex flex-col gap-1">
      <div class="flex items-center gap-1.5 flex-wrap">
        {#if totalTokens > 1}
          <div
            class="cursor-grab active:cursor-grabbing p-1 text-[var(--fp-dim)] hover:text-[var(--fp-accent)] rounded select-none -ml-1 hover:bg-[var(--fp-surface)] transition-colors"
            title={$tr("Drag to reorder account position")}
            aria-label={$tr("Drag to reorder")}
          >
            <GripVertical size={16} />
          </div>
        {/if}
        <span class="fp-num text-xs font-semibold text-[var(--fp-text)]"
          >Account #{idx + 1}</span
        >
        {#if idx === 0}
          <span
            class="inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-semibold bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30"
          >
            {$tr("Primary")}
          </span>
        {/if}
        <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
        {#if riskBadge}
          <StatusBadge
            status={riskBadge.label}
            tone={riskBadge.tone}
            pulse={riskBadge.pulse}
          />
        {/if}
      </div>
      {#if token.email || token.account_id}
        <span
          class="text-[11px] text-[var(--fp-muted)] break-words"
          title={token.email || token.account_id}
        >
          {token.email || token.account_id}
        </span>
      {/if}
    </div>
    <button
      type="button"
      onclick={onToggle}
      aria-expanded={expanded}
      aria-label={expanded
        ? `Collapse details for account ${idx + 1}`
        : `Expand details for account ${idx + 1}`}
      class="inline-flex items-center justify-center w-9 h-9 shrink-0 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] transition-colors"
    >
      {#if expanded}
        <ChevronExpand size={18} />
      {:else}
        <ChevronDown size={18} class="rotate-[-90deg]" />
      {/if}
    </button>
  </div>

  <!-- Risk/usage stats (moved from the standalone At-risk cards): live
       cooldown, 24h messages and run/request counts on every card. -->
  <div class="flex flex-col gap-2">
    {#if token.cooldown_active}
      {@const cd = cooldownLabel(token)}
      <div class="fp-inset px-2.5 py-1.5 text-xs text-[var(--fp-warning)]">
        {$tr("Cooldown")} —
        <span class="fp-num">{cd}</span>
        {#if cd !== "expiring" && cd !== "—"}{$tr("remaining")}{/if}
      </div>
    {/if}
    <div class="fp-inset px-2.5 py-1.5 text-xs text-[var(--fp-muted)]">
      {#if token.daily_limit > 0}
        <span class="fp-num text-[var(--fp-text)]"
          >{token.messages_24h}/{token.daily_limit}</span
        >
        {$tr("msgs today")}
        (<span class="fp-num">{token.usage_pct}%</span>)
      {:else}
        <span class="fp-num text-[var(--fp-text)]">{token.messages_24h}</span>
        {$tr("msgs 24h")}
      {/if}
    </div>
    <div class="flex justify-between text-xs text-[var(--fp-dim)]">
      <span
        >runs <span class="fp-num text-[var(--fp-text)]"
          >{token.active_runs}</span
        ></span
      >
      <span
        >reqs <span class="fp-num text-[var(--fp-text)]">{token.requests}</span
        ></span
      >
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

  <!-- Actions: full-width wrap, tap-friendly -->
  <div
    class="flex items-center justify-between gap-2 pt-0.5 border-t border-[var(--fp-border)]"
  >
    {#if totalTokens > 1}
      <div class="flex items-center gap-1">
        <button
          type="button"
          disabled={actionPending || idx === 0}
          title={$tr("Move Up / Prioritize")}
          aria-label={$tr("Move Up")}
          onclick={() => onSwap?.(idx, idx - 1)}
          class="inline-flex items-center justify-center w-9 h-9 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
        >
          <ChevronUp size={16} />
        </button>
        <button
          type="button"
          disabled={actionPending || idx >= totalTokens - 1}
          title={$tr("Move Down")}
          aria-label={$tr("Move Down")}
          onclick={() => onSwap?.(idx, idx + 1)}
          class="inline-flex items-center justify-center w-9 h-9 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
        >
          <ChevronDown size={16} />
        </button>
      </div>
    {:else}
      <span></span>
    {/if}
    <div class="flex items-center gap-1.5 flex-wrap justify-end">
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
