<script>
  /**
   * Pips — dot progress + count from the Stitch revamp set.
   *
   * A fixed row of `dots` (default 7, one per week-day) filled
   * proportionally to value/total, plus a mono `value/total` readout.
   * Filled dots use the accent; empty dots the surface/border. No new
   * visual primitives.
   *
   * @prop {number} [value=0]
   * @prop {number} [total=7]
   * @prop {number} [dots=7]
   * @prop {string} [label=""] — title tooltip
   */
  let { value = 0, total = 7, dots = 7, label = "" } = $props();

  let filled = $derived(
    total > 0 ? Math.round(((value ?? 0) / total) * dots) : 0,
  );
</script>

<span class="flex items-center gap-1.5" title={label || undefined}>
  <span class="flex items-center gap-1" aria-hidden="true">
    {#each Array(dots) as _, i (i)}
      <span
        class="w-1.5 h-1.5 rounded-full {i < filled
          ? 'bg-[var(--fp-accent)]'
          : 'bg-[var(--fp-surface-2)] border border-[var(--fp-border)]'}"
      ></span>
    {/each}
  </span>
  <span class="fp-num text-[11px] text-[var(--fp-muted)]">
    {value ?? 0}/{total}
  </span>
</span>
