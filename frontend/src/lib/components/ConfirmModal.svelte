<script>
  import { AlertTriangle, AlertCircle, HelpCircle } from "@lucide/svelte";
  import Modal from "./Modal.svelte";
  import { tr } from "../i18n.js";
  import { confirmState } from "../stores/confirm.js";

  /**
   * ConfirmModal — Accessible Confirmation Dialog ("Are you sure?" popup).
   * Built on top of the canonical Modal template component.
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

  function handleCancel() {
    if (isLoading) return;
    if (open !== undefined) {
      open = false;
      onCancel?.();
    } else {
      $confirmState.onCancel?.();
    }
  }

  function handleConfirm() {
    if (isLoading) return;
    if (open !== undefined) {
      onConfirm?.();
    } else {
      $confirmState.onConfirm?.();
    }
  }
</script>

<Modal
  open={isOpen}
  role="alertdialog"
  title={resolvedTitle}
  description={resolvedMessage}
  loading={isLoading}
  onClose={handleCancel}
  size="md"
>
  {#snippet icon()}
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
  {/snippet}

  {#snippet footer()}
    <button
      type="button"
      data-autofocus
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
  {/snippet}
</Modal>
