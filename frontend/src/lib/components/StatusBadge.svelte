<script>
  /**
   * StatusBadge — 7px LED dot + uppercase mono label. Tone derived from status when not given.
   *
   * @prop {string} status
   * @prop {'good'|'warn'|'bad'|'critical'|'info'|'idle'} [tone='info']
   * @prop {boolean} [pulse=false]
   */
  let { status, tone, pulse = false } = $props();

  const toneFromStatus = {
    idle: 'good',
    leased: 'good',
    active: 'good',
    cooldown: 'warn',
    locked: 'warn',
    banned: 'bad',
    error: 'bad',
  };

  let resolvedTone = $derived(tone || toneFromStatus[status] || 'info');
</script>

<span class="inline-flex items-center gap-1.5">
  <span class="led led-{resolvedTone} {pulse ? 'led-pulse' : ''}" aria-hidden="true"></span>
  {#if resolvedTone === 'critical'}
    <span class="font-mono text-[11px] uppercase tracking-wider font-semibold text-[var(--fp-error)]">{status}</span>
  {:else}
    <span class="font-mono text-[11px] uppercase tracking-wider text-[var(--fp-muted)]">{status}</span>
  {/if}
</span>
