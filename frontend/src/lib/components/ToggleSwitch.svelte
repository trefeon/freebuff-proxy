<script>
  import { tr } from "../i18n.js";

  /**
   * ToggleSwitch — the single boolean-switch control for the whole dashboard.
   *
   * Best-practice switch pattern: a real `button role="switch"` carrying
   * `aria-checked` + `aria-label`, a sliding thumb for position feedback in
   * addition to color (never color alone), a visible focus ring, and an
   * explicit disabled state — the same control, same visuals, on every page.
   *
   * @prop {boolean} [checked=false]
   * @prop {boolean} [disabled=false] — also covers the saving lock
   * @prop {boolean} [saving=false] — shows the "Saving…" caption
   * @prop {string} [ariaLabel=""] — accessible name (e.g. the env key)
   * @prop {(next: boolean) => void} [onchange]
   */
  let {
    checked = false,
    disabled = false,
    saving = false,
    ariaLabel = "",
    onchange,
  } = $props();
</script>

<span class="inline-flex items-center gap-2.5 select-none">
  <button
    type="button"
    role="switch"
    aria-checked={checked}
    aria-label={ariaLabel || undefined}
    disabled={disabled || saving}
    onclick={() => onchange?.(!checked)}
    class="relative h-6 w-11 shrink-0 rounded-full border transition-colors duration-150 cursor-pointer disabled:cursor-not-allowed disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus:ring-[var(--fp-accent)] focus-visible:ring-offset-1 focus-visible:ring-offset-[var(--fp-bg)] {checked
      ? 'bg-[var(--fp-accent)] border-transparent'
      : 'bg-[var(--fp-surface-2)] border-[var(--fp-border-bright)]'}"
  >
    <span
      aria-hidden="true"
      class="absolute top-0.5 left-0.5 size-5 rounded-full bg-white shadow transition-transform duration-150 {checked
        ? 'translate-x-5'
        : 'translate-x-0'}"
    ></span>
  </button>
  <span class="text-xs text-[var(--fp-muted)] min-w-[52px]">
    {#if saving}
      {$tr("Saving…")}
    {:else if checked}
      {$tr("Enabled")}
    {:else}
      {$tr("Disabled")}
    {/if}
  </span>
</span>
