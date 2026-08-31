<script>
  import {
    ChevronUp,
    ChevronDown,
    ChevronRight,
    Unlock,
    Lock,
    Trash2,
    Zap,
    RefreshCw,
    Check,
    ExternalLink,
  } from '@lucide/svelte';
  import Button from './Button.svelte';
  import StatusBadge from './StatusBadge.svelte';
  import { formatLocalDate } from '../utils/format.js';
  import { fallbackModelOptions, fetchModelOptions } from '../modelOptions.js';
  import { tr } from '../i18n.js';
  import { onMount } from 'svelte';

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
    onSwap,
  } = $props();

  let modelOptions = $state(fallbackModelOptions);
  onMount(() => {
    fetchModelOptions().then((rows) => (modelOptions = rows));
  });

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


  // Live session countdown (freebuff TUI parity): the tokens poll refreshes
  // session_remaining_seconds every ~10s; between polls this ticks locally
  // from the last server-anchor (expiresAt = fetch time + remaining), so the
  // readout moves every second instead of stepping in poll-sized chunks.
  // Each poll re-anchors to the fresh server value, correcting drift.
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
    if (h > 0) return `${h}h ${m}m ${s}s remaining`;
    return `${m}m ${s}s remaining`;
  }

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
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction('clear')}
        >
          <Unlock size={13} />
          <span>{$tr('Clear')}</span>
        </Button>
      {/if}
      {#if token.locked}
        <Button
          variant="secondary"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction('unlock')}
        >
          <Unlock size={13} />
          <span>{$tr('Unlock')}</span>
        </Button>
      {:else}
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onAction('lock')}
        >
          <Lock size={13} />
          <span>{$tr('Lock')}</span>
        </Button>
      {/if}
      <Button
        variant="danger"
        size="sm"
        disabled={actionPending}
        onclick={() => onAction('remove')}
      >
        <Trash2 size={13} />
        <span>{$tr('Remove')}</span>
      </Button>
    </div>
  </td>
</tr>
{#if expanded}
  <tr>
    <td colspan="6" class="!p-0">
      <div class="fp-inset m-2 rounded p-3">
        <!-- Dev Tools: Session Generator & Diagnostics Toolbar (hidden unless DEVTOOLS_ENABLED) -->
        {#if devToolsEnabled}
        <div class="mb-3 p-2.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] flex flex-wrap items-center justify-between gap-2.5">
          <div class="flex flex-wrap items-center gap-2">
            <span class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider">{$tr('Dev Session:')}</span>
            <select
              bind:value={spawnModel}
              class="fp-input !text-xs !py-1 !px-2 !h-7 !w-44 !inline-block"
            >
              {#each modelOptions as m (m.id)}
                <option value={m.id}>{m.label}</option>
              {/each}
            </select>
            <Button
              variant="secondary"
              size="sm"
              class="!h-7 !text-xs !px-2.5"
              disabled={actionPending}
              onclick={() => onSpawn(spawnModel || 'mimo/mimo-v2.5')}
            >
              <Zap size={12} />
              <span>{$tr('Make Session')}</span>
            </Button>
          </div>

          <div class="flex items-center gap-1.5">
            <Button
              variant="ghost"
              size="sm"
              class="!h-7 !text-xs !px-2"
              disabled={actionPending}
              onclick={() => onRefresh('probe')}
            >
              <RefreshCw size={12} />
              <span>{$tr('Probe')}</span>
            </Button>
            <Button
              variant="ghost"
              size="sm"
              class="!h-7 !text-xs !px-2"
              disabled={actionPending}
              onclick={() => onRefresh('finish')}
            >
              <Check size={12} />
              <span>{$tr('Finish Runs')}</span>
            </Button>
          </div>
        </div>
        {/if}
        {#if token.session_remaining_seconds > 0 && token.session_model}
          <div class="mb-2 px-2 py-1 rounded bg-[var(--fp-accent)]/10 text-xs text-[var(--fp-accent)] flex items-center justify-between">
            <span>{$tr('Active Session:')} <code class="fp-num">{token.session_model}</code></span>
            <span class="fp-num">{fmtCountdown(sessionRemaining)}</span>
          </div>
        {/if}
        {#if token.has_standing}
→
          <!-- Standing / trust block (issue #140): level,
               score progress toward the next level, the cap
               holding the account (capped_by), and upstream's
               suggested earn-back actions. -->
          <div class="mb-2 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
            <div class="flex items-center justify-between gap-2 mb-1">
              <p class="text-xs text-[var(--fp-muted)] uppercase tracking-wider font-semibold">{$tr('Account standing')}</p>
              <span class="text-xs text-[var(--fp-text)] font-semibold">{token.standing_label || token.standing_level}</span>
            </div>
            {#if token.standing_score != null && token.standing_next_level}
              <div class="flex items-center gap-2">
                <div class="h-1.5 flex-1 rounded bg-[var(--fp-bg)] overflow-hidden">
                  <div
                    class="h-full rounded bg-[var(--fp-accent)]"
                    style={`width: ${Math.min(100, Math.max(0, token.standing_score))}%`}
                  ></div>
                </div>
                <span class="fp-num text-xs text-[var(--fp-muted)] shrink-0">
                  {$tr('score {score} → next: {level}', { score: token.standing_score, level: token.standing_next_level })}
                </span>
              </div>
            {:else if token.standing_score != null}
              <span class="fp-num text-xs text-[var(--fp-muted)]">{$tr('score {score}', { score: token.standing_score })}</span>
            {/if}
            {#if token.standing_blurb}
              <p class="text-xs text-[var(--fp-dim)] mt-1">{token.standing_blurb}</p>
            {/if}
            {#if token.standing_capped_by}
              <p class="text-xs mt-1 text-[var(--fp-warning)]">
                {$tr('Capped by')} <code class="fp-num">{token.standing_capped_by}</code>{#if token.standing_capped_reason}: {token.standing_capped_reason}{/if}
              </p>
            {/if}
            {#if token.standing_next_steps?.length > 0}
              <ul class="mt-1.5 flex flex-col gap-1">
                {#each token.standing_next_steps as step}
                  <li class="text-xs text-[var(--fp-text)] flex items-start gap-1.5">
                    <span class="fp-num text-[var(--fp-accent)] shrink-0">+{step.points}</span>
                    <span>
                      {step.label}{#if step.detail} — <span class="text-[var(--fp-dim)]">{step.detail}</span>{/if}
                      {#if step.href}
                        <a href={step.href} target="_blank" rel="noopener noreferrer" class="ml-1 text-[var(--fp-accent)] hover:underline inline-flex items-center gap-0.5">
                          <ExternalLink size={10} />
                        </a>
                      {/if}
                    </span>
                  </li>
                {/each}
              </ul>
            {/if}
          </div>
        {/if}
        {#if !devToolsEnabled && !(token.session_remaining_seconds > 0 && token.session_model) && !token.has_standing}
          <p class="text-xs text-[var(--fp-dim)] italic">
            {$tr('No active session or run for this auth token.')}
          </p>
        {/if}
      </div>
    </td>
  </tr>
{/if}
