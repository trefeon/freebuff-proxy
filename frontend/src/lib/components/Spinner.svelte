<script>
  /**
   * Spinner — small loading arc in accent (2px).
   *
   * @prop {'sm'|'md'} [size='md']
   */
  let { size = "md" } = $props();
</script>

<span
  class="inline-block rounded-full border-2 border-[var(--fp-accent)] border-t-transparent animate-spin {size ===
  'md'
    ? 'size-4'
    : 'size-3.5'}"
  aria-hidden="true"
></span>
