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
    return { label: $tr("idle"), tone: "idle" };
  }

  const st = $derived(statusFor(token));

  // Live session countdown (same tick model as the desktop card).
  let nowTick = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => {
      nowTick = Date.now();
    }, 1000);
    return () => clearInterval(t);
  });
  const sessionEndsAtMs = $derived(
    Date.now() + (token.session_remaining_seconds || 0) * 1000,
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
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${sec}s`;
    return `${sec}s`;
  }
</script>

<div class="fp-inset rounded-lg p-3.5 flex flex-col gap-2.5">
  <!-- Header: identity + status + expand -->
  <div class="flex items-start justify-between gap-2">
    <div class="min-w-0 flex flex-col gap-1">
      <div class="flex items-center gap-1.5 flex-wrap">
        <span class="fp-num text-xs font-semibold text-[var(--fp-text)]"
          >#{idx}</span
        >
        {#if idx === 0}
          <span
            class="inline-flex items-center rounded px-1.5 py-0.5 text-[10px] font-semibold bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30"
          >
            {$tr("Primary")}
          </span>
        {/if}
        <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
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
        ? `Collapse details for token ${idx}`
        : `Expand details for token ${idx}`}
      class="inline-flex items-center justify-center w-9 h-9 shrink-0 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] transition-colors"
    >
      {#if expanded}
        <ChevronExpand size={18} />
      {:else}
        <ChevronDown size={18} class="rotate-[-90deg]" />
      {/if}
    </button>
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
        <span class="text-[var(--fp-muted)]">{$tr("Cooldown")}</span>
        <span
          class="fp-num {token.cooldown_active
            ? 'text-[var(--fp-text)]'
            : 'text-[var(--fp-dim)]'}"
        >
          {token.cooldown_active ? cooldownLabel(token) : "—"}
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
