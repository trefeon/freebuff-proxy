<script>
  import { tick } from "svelte";
  import { tr } from "../i18n.js";

  /**
   * ToggleSwitch — the single boolean-switch control for the whole dashboard.
   *
   * Flowbite toggle pattern (https://flowbite.com/docs/forms/toggle,
   * https://flowbite-svelte.com/docs/forms/toggle): a `label` wrapping an
   * `input.sr-only.peer` checkbox plus a track `div` driven by
   * `peer-checked:` and an `after:` sliding thumb. Default size (w-11 h-6,
   * thumb h-5 w-5); colors mapped to repo tokens (off surface-2, on accent).
   *
   * The input keeps `role="switch"` + explicit `aria-checked` so the
   * established `getByRole("switch")` / `aria-checked` contract (e2e + SR)
   * holds while the visuals follow Flowbite. Native checkbox behavior gives
   * free keyboard support and label-click toggling. Adopt-or-restore: after
   * `onchange`, a tick compares DOM checked against the prop — adopted
   * (parent moved the prop) leaves the thumb alone (no flicker), rejected
   * (prop stayed, e.g. failed save) snaps it back so track, caption, and
   * `aria-checked` never disagree.
   *
   * @prop {boolean} [checked=false]
   * @prop {boolean} [disabled=false] — also covers the saving lock
   * @prop {boolean} [saving=false] — shows the "Saving…" caption
   * @prop {string} [ariaLabel=""] — accessible name (e.g. the env key)
   * @prop {(next: boolean) => void} [onchange] — receives native checked value
   */
  let {
    checked = false,
    disabled = false,
    saving = false,
    ariaLabel = "",
    onchange,
  } = $props();

  let isDisabled = $derived(disabled || saving);
  let el = $state(null);

  // Adopt-or-restore: after the parent processes the change, the DOM must
  // agree with the prop. Adopted (prop moved) → leave it (no flicker).
  // Rejected (prop stayed, e.g. failed save) → snap the thumb back so
  // visuals never disagree with state and aria-checked.
  async function handleChange(e) {
    const next = e.currentTarget.checked;
    onchange?.(next);
    await tick();
    if (el && el.checked !== checked) el.checked = checked;
  }
</script>

<label
  class="inline-flex items-center gap-2.5 select-none {isDisabled
    ? 'cursor-not-allowed'
    : 'cursor-pointer'}"
>
  <!-- Relative box keeps Flowbite track visuals while the real checkbox sits
       as a full-track transparent overlay (z-10) so pointer, keyboard, and
       SR interaction hit the control itself — a 1px `sr-only` input is
       covered by the track and rejects programmatic clicks. -->
  <span class="relative inline-flex h-6 w-11 shrink-0">
    <input
      bind:this={el}
      type="checkbox"
      role="switch"
      class="peer absolute inset-0 z-10 h-full w-full cursor-pointer opacity-0 focus-visible:outline-none disabled:cursor-not-allowed"
      {checked}
      aria-checked={checked ? "true" : "false"}
      aria-label={ariaLabel || undefined}
      disabled={isDisabled}
      onchange={handleChange}
    />
    <div
      aria-hidden="true"
      class="relative flex items-center w-11 h-6 shrink-0 rounded-full border transition-colors duration-150 motion-safe:duration-150 bg-[var(--fp-surface-2)] border-[var(--fp-border-bright)] peer-checked:bg-[var(--fp-accent)] peer-checked:border-transparent peer-focus-visible:outline-none peer-focus-visible:ring-2 peer-focus-visible:ring-[var(--fp-accent)] peer-focus-visible:ring-offset-1 peer-focus-visible:ring-offset-[var(--fp-bg)] peer-disabled:opacity-50 peer-disabled:cursor-not-allowed after:content-[''] after:ms-[2px] after:h-5 after:w-5 after:shrink-0 after:rounded-full after:bg-white after:ring-1 after:ring-black/10 after:transition-transform after:duration-150 motion-safe:after:duration-150 peer-checked:after:translate-x-full rtl:peer-checked:after:-translate-x-full"
    ></div>
  </span>
  <span aria-hidden="true" class="text-xs text-[var(--fp-muted)] min-w-[52px]">
    {#if saving}
      {$tr("Saving…")}
    {:else if checked}
      {$tr("Enabled")}
    {:else}
      {$tr("Disabled")}
    {/if}
  </span>
</label>
