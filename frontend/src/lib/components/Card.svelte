<script>
  /**
   * Card — hairline-bordered surface, radius 4px (defined edge, no shadow).
   *
   * @prop {string} [title]
   * @prop {string} [description]
   * @prop {'md'|'none'} [pad='md'] — internal padding; 'none' leaves spacing to the page
   * @prop {string} [class]
   * @slot actions — top-right
   * @slot default — body
   * @slot footer
   */
  let {
    title,
    description,
    pad = "md",
    class: className = "",
    actions,
    children,
    footer,
  } = $props();
</script>

<section class="fp-card overflow-hidden {className}">
  {#if title || description || actions}
    <header
      class="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 px-5 pt-4"
    >
      <!-- Title block flexes and wraps; the actions span stays shrink-0 so
        the two never collide (Maturity account title vs status badges).
        basis-full stacks the header below sm; sm:basis-28 (~title width)
        shares the row wherever the status fits, wrapping instead of
        crowding where it does not. -->
      <div class="min-w-0 flex-1 basis-full sm:basis-28">
        {#if title}
          <h2 class="text-[15px] font-semibold text-[var(--fp-text)]">
            {title}
          </h2>
        {/if}
        {#if description}
          <p class="mt-0.5 text-sm text-[var(--fp-muted)]">{description}</p>
        {/if}
      </div>
      {#if actions}
        <div
          class="flex min-w-0 shrink-0 flex-wrap items-center justify-start gap-2 sm:justify-end"
        >
          {@render actions()}
        </div>
      {/if}
    </header>
  {/if}
  <div class="flex-1 min-h-0 flex flex-col {pad === 'md' ? 'p-5' : ''}">
    {#if children}
      {@render children()}
    {/if}
  </div>
  {#if footer}
    <footer
      class="border-t border-[var(--fp-border)] {pad === 'md'
        ? 'px-5 py-3'
        : ''}"
    >
      {@render footer()}
    </footer>
  {/if}
</section>
