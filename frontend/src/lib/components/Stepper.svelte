<script>
  import { Minus, Plus } from "@lucide/svelte";

  /**
   * Stepper — compact number stepper from the Stitch revamp set.
   *
   * Minus/plus buttons around a centered number input, clamped to
   * [min, max]. Composes fp-btn/fp-input; no new visual primitives.
   *
   * @prop {number} [value=0] — bindable value
   * @prop {number} [min=1]
   * @prop {number} [max=28]
   * @prop {boolean} [disabled=false]
   * @prop {string} [ariaLabel=""]
   * @prop {string} [decreaseLabel="Decrease"]
   * @prop {string} [increaseLabel="Increase"]
   */
  let {
    value = $bindable(0),
    min = 1,
    max = 28,
    disabled = false,
    ariaLabel = "",
    decreaseLabel = "Decrease",
    increaseLabel = "Increase",
  } = $props();

  function nudge(delta) {
    value = Math.min(max, Math.max(min, (Number(value) || 0) + delta));
  }
</script>

<div class="flex items-center gap-1.5">
  <button
    type="button"
    class="fp-btn fp-btn-secondary fp-btn-sm !px-2 shrink-0"
    {disabled}
    aria-label={decreaseLabel}
    onclick={() => nudge(-1)}
  >
    <Minus size={13} />
  </button>
  <input
    type="number"
    {min}
    {max}
    class="fp-input fp-num !h-8 !text-xs text-center min-w-12 flex-1"
    bind:value
    {disabled}
    aria-label={ariaLabel || undefined}
  />
  <button
    type="button"
    class="fp-btn fp-btn-secondary fp-btn-sm !px-2 shrink-0"
    {disabled}
    aria-label={increaseLabel}
    onclick={() => nudge(1)}
  >
    <Plus size={13} />
  </button>
</div>
