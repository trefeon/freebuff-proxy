<script>
  /**
   * FieldBox — inset field card from the Stitch revamp set.
   *
   * A labeled control inside an `fp-inset` box: title row (label left,
   * unit/hint right) plus the control child. Composes existing fp tokens;
   * no new visual primitives.
   *
   * @prop {string} label — box title
   * @prop {string} [unit] — right-aligned unit/caption in the title row
   * @prop {string} [class] — extra classes on the box
   * @prop {import('svelte').Snippet} [children] — the control
   */
  let { label, unit = "", class: className = "", children } = $props();
</script>

<div class="fp-inset rounded p-2.5 flex flex-col gap-1.5 {className}">
  <div class="flex items-center justify-between gap-2">
    <span class="text-[11px] font-semibold text-[var(--fp-muted)]">{label}</span
    >
    {#if unit}
      <span class="text-[10px] uppercase font-mono text-[var(--fp-dim)]"
        >{unit}</span
      >
    {/if}
  </div>
  {#if children}
    {@render children()}
  {/if}
</div>
