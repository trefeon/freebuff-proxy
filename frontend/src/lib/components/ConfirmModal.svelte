<script>
  import { AlertTriangle, AlertCircle, HelpCircle, X } from "@lucide/svelte";
  import { tr } from "../i18n.js";
  import { confirmState } from "../stores/confirm.js";

  /**
   * ConfirmModal — Accessible Confirmation Dialog ("Are you sure?" popup).
   *
   * Can be mounted once globally in App.svelte (reads from $confirmState),
   * or used directly in any component with local props.
   *
   * @prop {boolean} [open]
   * @prop {string} [title]
   * @prop {string} [message]
   * @prop {string} [confirmText]
   * @prop {string} [cancelText]
   * @prop {'danger'|'warn'|'neutral'} [tone]
   * @prop {boolean} [loading]
   * @prop {() => void} [onConfirm]
   * @prop {() => void} [onCancel]
   */
  let {
    open = $bindable(undefined),
    title = undefined,
    message = undefined,
    confirmText = undefined,
    cancelText = undefined,
    tone = undefined,
    loading = undefined,
    onConfirm = undefined,
    onCancel = undefined,
  } = $props();

  // Resolve values: local props override global store
  const isOpen = $derived(open !== undefined ? open : $confirmState.open);
  const resolvedTitle = $derived(
    title !== undefined ? title : $confirmState.title || $tr("Are you sure?"),
  );
  const resolvedMessage = $derived(
    message !== undefined ? message : $confirmState.message,
  );
  const resolvedConfirmText = $derived(
    confirmText !== undefined
      ? confirmText
      : $confirmState.confirmText || $tr("Confirm"),
  );
  const resolvedCancelText = $derived(
    cancelText !== undefined
      ? cancelText
      : $confirmState.cancelText || $tr("Cancel"),
  );
  const resolvedTone = $derived(
    tone !== undefined ? tone : $confirmState.tone || "danger",
  );
  const isLoading = $derived(
    loading !== undefined ? loading : $confirmState.loading,
  );

  let modalEl = $state(null);
  let cancelBtnEl = $state(null);
  let lastFocusedEl = null;

  function handleCancel() {
    if (isLoading) return;
    if (open !== undefined) {
      open = false;
      onCancel?.();
    } else {
      $confirmState.onCancel?.();
    }
    restoreFocus();
  }

  function handleConfirm() {
    if (isLoading) return;
    if (open !== undefined) {
      onConfirm?.();
    } else {
      $confirmState.onConfirm?.();
    }
  }

  function restoreFocus() {
    if (lastFocusedEl && typeof lastFocusedEl.focus === "function") {
      lastFocusedEl.focus();
      lastFocusedEl = null;
    }
  }

  function handleKeydown(e) {
    if (!isOpen) return;
    if (e.key === "Escape") {
      handleCancel();
      return;
    }
    if (e.key === "Tab" && modalEl) {
      const focusable = modalEl.querySelectorAll(
        'button:not([disabled]), [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
      );
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  $effect(() => {
    if (!isOpen) return;
    lastFocusedEl = document.activeElement;
    // Initial focus on Cancel button so hitting Enter doesn't accidentally trigger a destructive action
    queueMicrotask(() => {
      if (isOpen && cancelBtnEl) {
        cancelBtnEl.focus();
      } else if (isOpen && modalEl) {
        modalEl.focus();
      }
    });
  });
</script>

<svelte:window onkeydown={isOpen ? handleKeydown : undefined} />

{#if isOpen}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center p-4"
    role="presentation"
  >
    <!-- Backdrop -->
    <button
      type="button"
      class="fixed inset-0 bg-black/75 backdrop-blur-sm transition-opacity border-0 p-0 m-0 w-full h-full cursor-default"
      onclick={handleCancel}
      aria-label={$tr("Close dialog")}
      tabindex="-1"
    ></button>

    <!-- Modal Card (Alert Dialog) -->
    <div
      bind:this={modalEl}
      tabindex="-1"
      class="relative w-full max-w-md bg-[var(--fp-surface)] border border-[var(--fp-border)] rounded-xl shadow-2xl p-6 z-10 space-y-4 page-enter focus:outline-none"
      role="alertdialog"
      aria-modal="true"
      aria-labelledby="confirm-modal-title"
      aria-describedby="confirm-modal-desc"
    >
      <!-- Header with Tone Icon -->
      <div class="flex items-start gap-3.5">
        <div
          class="p-2.5 rounded-lg shrink-0 {resolvedTone === 'danger'
            ? 'bg-red-500/15 text-red-400 border border-red-500/30'
            : resolvedTone === 'warn'
              ? 'bg-amber-500/15 text-amber-400 border border-amber-500/30'
              : 'bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] border border-[var(--fp-accent)]/30'}"
        >
          {#if resolvedTone === "danger"}
            <AlertTriangle size={20} />
          {:else if resolvedTone === "warn"}
            <AlertCircle size={20} />
          {:else}
            <HelpCircle size={20} />
          {/if}
        </div>

        <div class="space-y-1 flex-1 min-w-0">
          <h2
            id="confirm-modal-title"
            class="text-base font-semibold text-[var(--fp-text)]"
          >
            {resolvedTitle}
          </h2>
          {#if resolvedMessage}
            <p
              id="confirm-modal-desc"
              class="text-xs text-[var(--fp-muted)] leading-relaxed"
            >
              {resolvedMessage}
            </p>
          {/if}
        </div>

        <button
          type="button"
          class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded-md hover:bg-[var(--fp-surface-2)] transition-colors shrink-0"
          onclick={handleCancel}
          disabled={isLoading}
          aria-label={$tr("Cancel")}
        >
          <X size={16} />
        </button>
      </div>

      <!-- Action Buttons -->
      <div
        class="flex items-center justify-end gap-2.5 pt-3 border-t border-[var(--fp-border)]"
      >
        <button
          type="button"
          bind:this={cancelBtnEl}
          class="fp-btn fp-btn-secondary !text-xs !py-1.5 !px-3.5"
          onclick={handleCancel}
          disabled={isLoading}
        >
          {resolvedCancelText}
        </button>

        <button
          type="button"
          class="fp-btn !text-xs !py-1.5 !px-4 {resolvedTone === 'danger'
            ? 'bg-red-600 hover:bg-red-500 text-white border-red-600'
            : resolvedTone === 'warn'
              ? 'bg-amber-600 hover:bg-amber-500 text-white border-amber-600'
              : 'fp-btn-primary'}"
          onclick={handleConfirm}
          disabled={isLoading}
        >
          {#if isLoading}
            <span class="inline-block animate-spin mr-1.5">⏳</span>
          {/if}
          {resolvedConfirmText}
        </button>
      </div>
    </div>
  </div>
{/if}
