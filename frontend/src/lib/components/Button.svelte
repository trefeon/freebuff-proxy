<script>
  import Spinner from "./Spinner.svelte";

  /**
   * Button — ranked by importance, never colored by meaning.
   *
   * @prop {'primary'|'secondary'|'ghost'|'danger'} [variant='secondary']
   * @prop {'sm'|'md'} [size='md']
   * @prop {boolean} [disabled=false]
   * @prop {boolean} [loading=false]
   * @prop {string} [type='button']
   * @prop {string} [class]
   */
  let {
    variant = "secondary",
    size = "md",
    disabled = false,
    loading = false,
    type = "button",
    class: className = "",
    children,
    ...rest
  } = $props();
</script>

<button
  {type}
  class="fp-btn fp-btn-{variant} {size === 'sm' ? 'fp-btn-sm' : ''} {className}"
  disabled={disabled || loading}
  aria-busy={loading || undefined}
  {...rest}
>
  {#if loading}
    <Spinner size="sm" />
  {/if}
  {#if children}
    {@render children()}
  {/if}
</button>
