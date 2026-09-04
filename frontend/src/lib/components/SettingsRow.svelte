<script>
  /**
   * SettingsRow — Standard row template inside a settings card section.
   *
   * Formats a single setting row with:
   * - Title / label with optional status or restart-only badges
   * - Descriptive explanation
   * - Responsive alignment: mobile stacked, desktop inline (center or top aligned)
   * - Dedicated slot for the interactive control (switch, select, input)
   * - Optional extra slot for sub-content or collapsible details
   *
   * @prop {string} [label=""]
   * @prop {string} [description=""]
   * @prop {'center'|'start'} [align='center'] — desktop vertical alignment of label vs control
   * @prop {boolean} [first=false] — removes top padding on first row
   * @prop {boolean} [last=false] — removes bottom padding on last row
   * @prop {string} [class=""]
   * @slot badge — badges or chips rendered next to the label
   * @slot default — interactive control
   * @slot extra — sub-content placed below the description
   */
  let {
    label = "",
    description = "",
    align = "center",
    first = false,
    last = false,
    class: className = "",
    badge,
    children,
    extra,
  } = $props();
</script>

<div
  class="py-4 {first ? 'first:pt-0' : ''} {last
    ? 'last:pb-0'
    : ''} flex flex-col {align === 'start'
    ? 'md:flex-row md:items-start'
    : 'sm:flex-row sm:items-center'} justify-between gap-4 {className}"
>
  <div class="flex-1 min-w-0">
    <div class="flex flex-wrap items-center gap-2">
      {#if label}
        <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
          {label}
        </span>
      {/if}
      {#if badge}
        {@render badge()}
      {/if}
    </div>
    {#if description}
      <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
        {description}
      </p>
    {/if}
    {#if extra}
      <div class="mt-2">
        {@render extra()}
      </div>
    {/if}
  </div>

  {#if children}
    <div class="shrink-0 flex items-center gap-3">
      {@render children()}
    </div>
  {/if}
</div>
