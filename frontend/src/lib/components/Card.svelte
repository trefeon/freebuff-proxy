<script>
  /**
   * Card — hairline-bordered surface, radius 8px (defined edge, no shadow).
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
    pad = 'md',
    class: className = '',
    actions,
    children,
    footer,
  } = $props();
</script>

<section class="fp-card {className}">
  {#if title || description || actions}
    <header class="flex items-start justify-between gap-4 px-5 pt-4">
      <div>
        {#if title}
          <h2 class="text-[15px] font-semibold text-[var(--fp-text)]">{title}</h2>
        {/if}
        {#if description}
          <p class="mt-0.5 text-sm text-[var(--fp-muted)]">{description}</p>
        {/if}
      </div>
      {#if actions}
        <div class="flex shrink-0 items-center gap-2">
          {@render actions()}
        </div>
      {/if}
    </header>
  {/if}
  <div class="flex-1 flex flex-col {pad === 'md' ? 'p-5' : ''}">
    {#if children}
      {@render children()}
    {/if}
  </div>
  {#if footer}
    <footer class="border-t border-[var(--fp-border)] {pad === 'md' ? 'px-5 py-3' : ''}">
      {@render footer()}
    </footer>
  {/if}
</section>
