<script>
  /**
   * SegmentedControl — Pill-style segmented switch / button group template.
   *
   * @prop {string} [value=""] — bindable active option id
   * @prop {Array<{id: string, label: string, icon?: any}>|string[]} [options=[]]
   * @prop {'xs'|'sm'|'md'} [size='sm']
   * @prop {string} [ariaLabel=""]
   * @prop {string} [class=""]
   * @prop {(val: string) => void} [onchange]
   */
  let {
    value = $bindable(""),
    options = [],
    size = "sm",
    ariaLabel = "",
    class: className = "",
    onchange,
  } = $props();

  const normalizedOptions = $derived(
    options.map((opt) =>
      typeof opt === "string" ? { id: opt, label: opt } : opt,
    ),
  );

  const sizeClasses = {
    xs: "px-2 py-0.5 text-[11px]",
    sm: "px-2.5 py-1 text-xs",
    md: "px-3 py-1.5 text-sm",
  };

  function select(id) {
    if (value === id) return;
    value = id;
    onchange?.(id);
  }
</script>

<div
  role="group"
  aria-label={ariaLabel || undefined}
  class="inline-flex items-center gap-1 bg-[var(--fp-surface-2)] p-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] {className}"
>
  {#each normalizedOptions as opt (opt.id)}
    {@const active = value === opt.id}
    <button
      type="button"
      aria-pressed={active}
      class="{sizeClasses[size] ||
        sizeClasses.sm} font-mono rounded transition-colors flex items-center gap-1.5 {active
        ? 'bg-[var(--fp-surface)] text-[var(--fp-accent)] font-semibold shadow-sm'
        : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
      onclick={() => select(opt.id)}
    >
      {#if opt.icon}
        {@const Icon = opt.icon}
        <Icon size={size === "xs" ? 12 : 14} class="shrink-0" />
      {/if}
      <span>{opt.label}</span>
    </button>
  {/each}
</div>
