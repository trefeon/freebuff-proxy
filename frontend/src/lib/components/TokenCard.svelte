<script>
  import {
    ChevronUp,
    ChevronDown,
    ChevronRight,
    Unlock,
    Lock,
    Trash2,
    Mail,
    Fingerprint,
    Clock,
    Copy,
  } from '@lucide/svelte';
  import Button from './Button.svelte';
  import CopyButton from './CopyButton.svelte';
  import StatusBadge from './StatusBadge.svelte';
  import TokenDetailsDrawer from './TokenDetailsDrawer.svelte';
  import { copyToClipboard } from '../utils/clipboard.js';
  import { formatLocalDate } from '../utils/format.js';
  import { tr } from '../i18n.js';

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
      const until = formatLocalDate(token.banned_until);
      return { label: until ? $tr('banned until {time}', { time: until }) : $tr('banned (temporary)'), tone: 'bad' };
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
    if (!token.cooldown_active || !token.cooldown_until) return '';
    const ms = new Date(token.cooldown_until).getTime() - now;
    if (ms <= 0) return $tr('expiring');
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

  let nowTick = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => {
      nowTick = Date.now();
    }, 1000);
    return () => clearInterval(t);
  });
  const sessionEndsAtMs = $derived(Date.now() + (token.session_remaining_seconds || 0) * 1000);
  const sessionRemaining = $derived(Math.max(0, Math.floor((sessionEndsAtMs - nowTick) / 1000)));

  function fmtCountdown(totalSeconds) {
    const h = Math.floor(totalSeconds / 3600);
    const m = Math.floor((totalSeconds % 3600) / 60);
    const s = totalSeconds % 60;
    if (h > 0) return `${h}h ${m}m ${s}s`;
    return `${m}m ${s}s`;
  }

  const st = $derived(statusFor(token));
  const initial = $derived((token.email || token.account_id || `#${idx}`)[0]?.toUpperCase() || '?');
  const displayEmail = $derived(token.email || token.account_id || $tr('Unknown account'));
  const shortInstance = $derived(token.session_instance ? `${token.session_instance.slice(0, 8)}…${token.session_instance.slice(-6)}` : '');
  const isPrimary = $derived(idx === 0);
  const cooldownText = $derived(cooldownLabel(token));
  const hasCooldown = $derived(token.cooldown_active);
  const cooldownIsWarn = $derived(cooldownTone(token) === 'warn');
</script>

<div class="group flex flex-col gap-0 rounded-xl border bg-[var(--fp-surface)] overflow-hidden transition-colors {expanded ? 'border-[var(--fp-border-bright)] shadow-sm' : 'border-[var(--fp-border)] hover:border-[var(--fp-border-bright)]'}">
  <!-- Header: identity + status + reorder -->
  <div class="flex items-start justify-between gap-3 px-4 pt-4">
    <div class="flex items-center gap-3 min-w-0 flex-1">
      <!-- Avatar -->
      <div class="w-10 h-10 rounded-full flex items-center justify-center shrink-0 text-sm font-semibold border {isPrimary ? 'bg-[var(--fp-accent)] text-white border-[var(--fp-accent)]' : 'bg-[var(--fp-surface-2)] text-[var(--fp-text)] border-[var(--fp-border)]'}">
        {initial}
      </div>
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-2 flex-wrap">
          <span class="text-sm font-semibold text-[var(--fp-text)]">#{idx}</span>
          {#if isPrimary}
            <span class="inline-flex items-center rounded-full px-2 py-0.5 text-[10px] font-bold tracking-wider uppercase bg-amber-500/15 text-amber-600 border border-amber-500/30 dark:text-amber-400">
              {$tr('Primary')}
            </span>
          {/if}
          <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
        </div>
        <div class="flex items-center gap-1.5 mt-0.5 min-w-0">
          <Mail size={12} class="shrink-0 text-[var(--fp-dim)]" />
          <span class="text-xs text-[var(--fp-muted)] truncate" title={displayEmail}>{displayEmail}</span>
          <button
            type="button"
            class="shrink-0 inline-flex items-center justify-center w-5 h-5 rounded hover:bg-[var(--fp-surface-2)] text-[var(--fp-dim)] hover:text-[var(--fp-text)] transition-colors"
            title={$tr('Copy email')}
            aria-label={$tr('Copy email')}
            onclick={async () => await copyToClipboard(displayEmail)}
          >
            <Copy size={12} />
          </button>
        </div>
      </div>
    </div>

    <div class="flex items-center gap-1 shrink-0">
      {#if totalTokens > 1}
        <div class="hidden sm:flex flex-col -my-1 mr-1">
          <button
            type="button"
            disabled={actionPending || idx === 0}
            title={$tr('Move Up / Prioritize')}
            aria-label={$tr('Move Up')}
            onclick={() => onSwap?.(idx, idx - 1)}
            class="inline-flex items-center justify-center w-7 h-7 rounded-md text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
          >
            <ChevronUp size={14} />
          </button>
          <button
            type="button"
            disabled={actionPending || idx >= totalTokens - 1}
            title={$tr('Move Down')}
            aria-label={$tr('Move Down')}
            onclick={() => onSwap?.(idx, idx + 1)}
            class="inline-flex items-center justify-center w-7 h-7 rounded-md text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] disabled:opacity-20 disabled:cursor-not-allowed transition-colors"
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
        class="inline-flex items-center justify-center w-8 h-8 rounded-lg border text-[var(--fp-muted)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] transition-colors {expanded ? 'border-[var(--fp-border-bright)] bg-[var(--fp-surface-2)]' : 'border-[var(--fp-border)] bg-transparent'}"
      >
        <span class="transition-transform {expanded ? 'rotate-90' : ''}">
          <ChevronRight size={16} />
        </span>
      </button>
    </div>
  </div>

  <!-- Key facts grid: Instance + Cooldown/Session -->
  <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 px-4 pt-3">
    <div class="flex flex-col gap-1 min-w-0">
      <span class="text-[10px] font-semibold tracking-widest uppercase text-[var(--fp-dim)] flex items-center gap-1">
        <Fingerprint size={10} /> {$tr('Instance')}
      </span>
      {#if token.session_instance}
        <div class="flex items-center gap-1.5 min-w-0">
          <code class="fp-num text-xs text-[var(--fp-text)] truncate flex-1" title={token.session_instance}>{shortInstance}</code>
          <CopyButton text={token.session_instance} label={$tr('Copy instance ID')} />
        </div>
        {#if token.session_remaining_seconds > 0 && token.session_model}
          <span class="text-[11px] text-[var(--fp-accent)]">
            {token.session_model} · {fmtCountdown(sessionRemaining)} {$tr('remaining')}
          </span>
        {:else if token.session_remaining_seconds > 0}
          <span class="text-[11px] text-[var(--fp-muted)]">
            {fmtCountdown(sessionRemaining)} {$tr('remaining')}
          </span>
        {/if}
      {:else}
        <span class="text-xs text-[var(--fp-dim)] italic">{$tr('No active session')}</span>
      {/if}
    </div>

    <div class="flex flex-col gap-1">
      <span class="text-[10px] font-semibold tracking-widest uppercase text-[var(--fp-dim)] flex items-center gap-1">
        <Clock size={10} /> {$tr('Cooldown')}
      </span>
      {#if hasCooldown}
        <span class="inline-flex items-center gap-1.5 fp-num text-sm font-medium {cooldownIsWarn ? 'text-[var(--fp-warning)]' : 'text-[var(--fp-text)]'}">
          <span class="w-1.5 h-1.5 rounded-full {cooldownIsWarn ? 'bg-[var(--fp-warning)] animate-pulse' : 'bg-[var(--fp-warning)]'}"></span>
          {cooldownText}
        </span>
        <span class="text-[11px] text-[var(--fp-muted)]">{$tr('Resets')} {formatLocalDate(token.cooldown_until)}</span>
      {:else}
        <span class="inline-flex items-center gap-1.5 text-sm text-[var(--fp-success)] font-medium">
          <span class="w-1.5 h-1.5 rounded-full bg-[var(--fp-success)]"></span>
          {$tr('Available — no cooldown')}
        </span>
        <span class="text-[11px] text-[var(--fp-dim)]">{$tr('Ready to take requests')}</span>
      {/if}
    </div>
  </div>

  <!-- Actions -->
  <div class="flex items-center justify-between gap-2 px-4 py-3 mt-1 bg-[var(--fp-bg)]/40 border-t border-[var(--fp-border)]">
    <div class="flex items-center gap-1.5">
      {#if hasCooldown}
        <Button variant="ghost" size="sm" disabled={actionPending} onclick={() => onAction('clear')}>
          <Unlock size={13} /> <span>{$tr('Clear cooldown')}</span>
        </Button>
      {/if}
      {#if token.locked}
        <Button variant="secondary" size="sm" disabled={actionPending} onclick={() => onAction('unlock')}>
          <Unlock size={13} /> <span>{$tr('Unlock')}</span>
        </Button>
      {:else}
        <Button variant="ghost" size="sm" disabled={actionPending} onclick={() => onAction('lock')} title={$tr('Pause this token from receiving requests')}>
          <Lock size={13} /> <span>{$tr('Pause')}</span>
        </Button>
      {/if}
    </div>
    <Button variant="danger" size="sm" disabled={actionPending} onclick={() => onAction('remove')} title={$tr('Remove token from pool')}>
      <Trash2 size={13} /> <span>{$tr('Remove')}</span>
    </Button>
  </div>

  {#if expanded}
    <div class="border-t border-[var(--fp-border)] bg-[var(--fp-surface)]">
      <div class="p-3">
        <TokenDetailsDrawer {token} bind:spawnModel {actionPending} {devToolsEnabled} {onSpawn} {onRefresh} {onDropSession} {sessionRemaining} />
      </div>
    </div>
  {/if}
</div>
