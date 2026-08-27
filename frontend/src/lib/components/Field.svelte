<script>
  /**
   * Field — labeled form control wrapper.
   *
   * Wires label `for` ↔ control `id`, and `aria-describedby` to hint/error
   * plus `aria-invalid` when in error state. Generates an id when none is
   * provided so hint/error associations remain valid.
   *
   * @prop {string} label
   * @prop {string} [hint]
   * @prop {string} [error]
   * @prop {string} [id] — wired to the control via `for` and to hint/error via {id}-hint/{id}-error; auto-generated if omitted
   */
  let { label, hint, error, id: idProp, children } = $props();

  const autoId = `field-${Math.random().toString(36).slice(2, 9)}`;
  let fieldId = $derived(idProp ?? autoId);
  let hintId = $derived(`${fieldId}-hint`);
  let errorId = $derived(`${fieldId}-error`);
  let describedBy = $derived(error ? errorId : hint ? hintId : undefined);
  let hasError = $derived(Boolean(error));

  let containerEl = $state(null);

  $effect(() => {
    const d = describedBy;
    const isInvalid = hasError;
    const fid = fieldId;
    const el = containerEl;
    if (!el) return;
    const control = el.querySelector('input, select, textarea');
    if (!control) return;
    if (!control.id) control.id = fid;
    // Keep for ↔ id in sync if caller passed divergent ids
    if (control.id !== fid) {
      // Field is source of truth for label/for; align control to field id
      control.id = fid;
    }
    if (d) control.setAttribute('aria-describedby', d);
    else control.removeAttribute('aria-describedby');
    if (isInvalid) control.setAttribute('aria-invalid', 'true');
    else control.removeAttribute('aria-invalid');
  });
</script>

<div bind:this={containerEl} class="flex flex-col gap-1.5">
  {#if label}
    <label for={fieldId} class="text-xs text-[var(--fp-muted)]">{label}</label>
  {/if}
  {#if children}
    {@render children()}
  {/if}
  {#if error}
    <p class="text-[11px] text-[var(--fp-error)]" id={errorId} role="alert">{error}</p>
  {:else if hint}
    <p class="text-[11px] text-[var(--fp-dim)]" id={hintId}>{hint}</p>
  {/if}
</div>
