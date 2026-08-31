<script>
  import { Zap, RefreshCw, Check, ExternalLink } from '@lucide/svelte';
  import Button from './Button.svelte';
  import { fallbackModelOptions, fetchModelOptions } from '../modelOptions.js';
  import { tr } from '../i18n.js';
  import { onMount } from 'svelte';

  /**
   * TokenDetailsDrawer — expanded details for one pooled token: the Dev Tools
   * toolbar (when enabled), the live session countdown, the account-standing
   * block, and the empty-state message. Shared by the desktop table row
   * (TokenCard) and the mobile stacked card (TokenCardMobile).
   *
   * @prop {object} token — dashboard tokenCard payload
   * @prop {string} [spawnModel] — bindable dev-spawn model selection
   * @prop {boolean} [actionPending]
   * @prop {boolean} [devToolsEnabled=false]
   * @prop {(model: string) => void} [onSpawn]
   * @prop {(action: string) => void} [onRefresh]
   * @prop {number} sessionRemaining — live seconds remaining on the session
   */
  let {
    token,
    spawnModel = $bindable(''),
    actionPending,
    devToolsEnabled = false,
    onSpawn,
    onRefresh,
    onDropSession,
    sessionRemaining,
  } = $props();
  let modelOptions = $state(fallbackModelOptions);
  onMount(() => {
    fetchModelOptions().then((rows) => (modelOptions = rows));
  });

  function fmtCountdown(totalSeconds) {
    const h = Math.floor(totalSeconds / 3600);
    const m = Math.floor((totalSeconds % 3600) / 60);
    const s = totalSeconds % 60;
    if (h > 0) return `${h}h ${m}m ${s}s remaining`;
    return `${m}m ${s}s remaining`;
  }
</script>

<div class="fp-inset rounded p-3">
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
          onclick={() => onSpawn?.(spawnModel || 'mimo/mimo-v2.5')}
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
          onclick={() => onRefresh?.('probe')}
        >
          <RefreshCw size={12} />
          <span>{$tr('Probe')}</span>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          class="!h-7 !text-xs !px-2"
          disabled={actionPending}
          onclick={() => onRefresh?.('finish')}
        >
          <Check size={12} />
          <span>{$tr('Finish Runs')}</span>
        </Button>
      </div>
    </div>
  {/if}
  {#if token.session_remaining_seconds > 0 && token.session_model}
    <div class="mb-2 px-2 py-1 rounded bg-[var(--fp-accent)]/10 text-xs text-[var(--fp-accent)] flex items-center justify-between gap-2 flex-wrap">
      <span>{$tr('Active Session:')} <code class="fp-num">{token.session_model}</code></span>
      <span class="fp-num">{fmtCountdown(sessionRemaining)}</span>
    </div>
  {/if}
  {#if token.session_remaining_seconds > 0}
    <div class="mb-2 flex justify-end">
      <Button
        variant="danger"
        size="sm"
        class="!h-7 !text-xs !px-2"
        disabled={actionPending}
        onclick={() => onDropSession?.()}
      >
        <span>{$tr('Drop Session')}</span>
      </Button>
    </div>
  {/if}
  {#if token.has_standing}
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
