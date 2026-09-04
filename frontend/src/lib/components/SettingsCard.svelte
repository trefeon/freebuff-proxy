<script>
  /**
   * SettingsCard — Standard card template for configuration & settings sections.
   *
   * Provides a consistent card layout:
   * - Rounded container with defined border and soft shadow
   * - Consistent header with icon badge, title, and descriptive subtitle
   * - Optional top-right action buttons
   * - Divided row container for child setting rows
   *
   * @prop {string} [title=""]
   * @prop {string} [description=""]
   * @prop {string} [class=""]
   * @slot icon — icon element rendered inside the badge
   * @slot actions — top-right actions
   * @slot default — settings rows (wrapped in divide-y)
   * @slot footer — optional card footer
   */
  let {
    title = "",
    description = "",
    class: className = "",
    icon,
    actions,
    children,
    footer,
  } = $props();
</script>

<div
  class="bg-surface border border-border-subtle rounded-[14px] shadow-[var(--shadow-soft)] p-6 page-enter {className}"
>
  <!-- Card Header -->
  {#if title || icon || actions}
    <div class="flex items-start justify-between gap-4 mb-5">
      <div class="flex items-center gap-3 min-w-0 flex-1">
        {#if icon}
          <div class="p-2 rounded-lg bg-primary/10 text-primary shrink-0">
            {@render icon()}
          </div>
        {/if}
        <div class="min-w-0 flex-1">
          {#if title}
            <h2
              class="text-base sm:text-lg font-semibold text-[var(--fp-text)]"
            >
              {title}
            </h2>
          {/if}
          {#if description}
            <p class="text-xs sm:text-sm text-text-muted mt-0.5">
              {description}
            </p>
          {/if}
        </div>
      </div>
      {#if actions}
        <div class="flex shrink-0 items-center gap-2">
          {@render actions()}
        </div>
      {/if}
    </div>
  {/if}

  <!-- Rows / Body -->
  {#if children}
    <div class="flex flex-col divide-y divide-border-subtle/50">
      {@render children()}
    </div>
  {/if}

  <!-- Footer -->
  {#if footer}
    <div class="mt-5 pt-4 border-t border-border-subtle/50">
      {@render footer()}
    </div>
  {/if}
</div>
