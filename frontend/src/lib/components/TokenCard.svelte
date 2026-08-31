<script>
  import {
    ChevronUp,
    ChevronDown,
    ChevronRight,
    Unlock,
    Lock,
    Trash2,
  } from '@lucide/svelte';
  import Button from './Button.svelte';
  import StatusBadge from './StatusBadge.svelte';
  import TokenDetailsDrawer from './TokenDetailsDrawer.svelte';
  import { tr } from '../i18n.js';

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
    spawnModel = $bindable(''),
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
    if (token.ban_type === 'hard') {
      return { label: $tr('banned — appeal required'), tone: 'critical', pulse: true };
    }
    if (token.ban_type === 'temporary') {
      return { label: $tr('banned (temporary)'), tone: 'bad' };
    }
    return null;
  }

  function statusFor(token) {
    const ban = banBadge(token);
    if (ban) return ban;
    if (token.locked) return { label: $tr('locked'), tone: 'warn' };
    if (token.cooldown_active) return { label: $tr('cooldown'), tone: 'warn' };
    const s = token.session_status || '';
    if (s === 'active') return { label: $tr('leased'), tone: 'good', pulse: true };
    if (s === 'queued') return { label: $tr('queued'), tone: 'info' };
    if (s === 'banned') return { label: $tr('banned'), tone: 'bad' };
    return { label: $tr('idle'), tone: 'idle' };
  }

  function cooldownLabel(token) {
    if (!token.cooldown_active || !token.cooldown_until) return '—';
    const ms = new Date(token.cooldown_until).getTime() - now;
    if (ms <= 0) return 'expiring';
    const s = Math.floor(ms / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${sec}s`;
    return `${sec}s`;
  }

  function cooldownTone(token) {
    if (!token.cooldown_until) return 'default';
    const ms = new Date(token.cooldown_until).getTime() - now;
    if (ms >= 0 && ms < 5 * 60_000) return 'warn';
    return 'default';
  }

  // Live session countdown (freebuff TUI parity).
  let nowTick = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => {
      nowTick = Date.now();
    }, 1000);
    return () => clearInterval(t);
  });
  const sessionEndsAtMs = $derived(Date.now() + (token.session_remaining_seconds || 0) * 1000);
  const sessionRemaining = $derived(Math.max(0, Math.floor((sessionEndsAtMs - nowTick) / 1000)));

  const st = $derived(statusFor(token));
</script>

<tr>
  <td class="w-14">
    <div class="inline-flex items-center gap-0.5">
      {#if totalTokens > 1}
        <div class="flex flex-col shrink-0 -my-1">
          <button
            type="button"
            disabled={actionPending || idx === 0}
            title={$tr('Move Up / Prioritize')}
            aria-label={$tr('Move Up')}
            onclick={() => onSwap?.(idx, idx - 1)}
            class="inline-flex items-center justify-center w-6 h-6 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
          >
            <ChevronUp size={14} />
          </button>
          <button
            type="button"
            disabled={actionPending || idx >= totalTokens - 1}
            title={$tr('Move Down')}
            aria-label={$tr('Move Down')}
            onclick={() => onSwap?.(idx, idx + 1)}
            class="inline-flex items-center justify-center w-6 h-6 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
          >
            <ChevronDown size={14} />
          </button>
        </div>
      {/if}
      <button
        type="button"
        onclick={onToggle}
        aria-expanded={expanded}
        aria-label={expanded ? `Collapse details for token ${idx}` : `Expand details for token ${idx}`}
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
        <span class="fp-num text-xs font-semibold text-[var(--fp-text)]">#{idx}</span>
        {#if idx === 0}
          <span class="inline-flex items-center rounded px-1.5 py-0.2 text-[10px] font-semibold bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30">
            {$tr('Primary')}
          </span>
        {/if}
      </div>
      {#if token.email || token.account_id}
        <span class="text-[11px] text-[var(--fp-muted)] truncate max-w-[160px]" title={token.email || token.account_id}>
          {token.email || token.account_id}
        </span>
      {/if}
    </div>
  </td>
  <td>
    <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
  </td>
  <td>
    {#if token.session_instance}
      <code class="fp-num text-xs text-[var(--fp-muted)] break-all select-all">{token.session_instance}</code>
    {:else}
      <span class="text-xs text-[var(--fp-dim)]">—</span>
    {/if}
  </td>
  <td class="num">
    {#if token.cooldown_active}
      <span class="fp-num text-xs {cooldownTone(token) === 'warn' ? 'text-[var(--fp-warning)]' : 'text-[var(--fp-text)]'}">
        {cooldownLabel(token)}
      </span>
    {:else}
      <span class="fp-num text-xs text-[var(--fp-dim)]">—</span>
    {/if}
  </td>
  <td class="text-right">
    <div class="inline-flex items-center gap-1.5 justify-end">
      {#if token.cooldown_active}
        <Button variant="ghost" size="sm" disabled={actionPending} onclick={() => onAction('clear')}>
          <Unlock size={13} />
          <span>{$tr('Clear')}</span>
        </Button>
      {/if}
      {#if token.locked}
        <Button variant="secondary" size="sm" disabled={actionPending} onclick={() => onAction('unlock')}>
          <Unlock size={13} />
          <span>{$tr('Unlock')}</span>
        </Button>
      {:else}
        <Button variant="ghost" size="sm" disabled={actionPending} onclick={() => onAction('lock')}>
          <Lock size={13} />
          <span>{$tr('Lock')}</span>
        </Button>
      {/if}
      <Button variant="danger" size="sm" disabled={actionPending} onclick={() => onAction('remove')}>
        <Trash2 size={13} />
        <span>{$tr('Remove')}</span>
      </Button>
    </div>
  </td>
</tr>
{#if expanded}
  <tr>
    <td colspan="6" class="!p-0">
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
