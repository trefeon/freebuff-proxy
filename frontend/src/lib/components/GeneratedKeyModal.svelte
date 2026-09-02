<script>
  import { Key, X } from "@lucide/svelte";
  import CopyButton from "./CopyButton.svelte";
  import Button from "./Button.svelte";
  import { tr } from "../i18n.js";

  /**
   * GeneratedKeyModal — modal shown after a new client API key is generated.
   *
   * @prop {boolean} open
   * @prop {string} key — the newly generated key to display/copy
   * @prop {() => void} [onClose]
   * @prop {() => void} [onCopy]
   */
  let { open = $bindable(false), key, onClose, onCopy } = $props();

  let modalEl = $state(null);
  let lastFocusedEl = null;

  function close() {
    // Restore focus to the element that opened the dialog (Issue #224).
    if (lastFocusedEl && typeof lastFocusedEl.focus === "function") {
      lastFocusedEl.focus();
      lastFocusedEl = null;
    }
    open = false;
    onClose?.();
  }

  function handleKeydown(e) {
    if (e.key === "Escape") {
      close();
      return;
    }
    if (e.key === "Tab" && modalEl) {
      const focusable = modalEl.querySelectorAll(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
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
    if (!open) return;
    lastFocusedEl = document.activeElement;
    // Issue #224: move focus into the dialog on open so keyboard users see it.
    queueMicrotask(() => {
      if (open && modalEl) modalEl.focus();
    });
  });
</script>

<svelte:window onkeydown={open ? handleKeydown : undefined} />

{#if open}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center p-4"
    role="presentation"
  >
    <!-- Backdrop -->
    <button
      type="button"
      class="fixed inset-0 bg-black/75 backdrop-blur-sm transition-opacity border-0 p-0 m-0 w-full h-full cursor-default"
      onclick={close}
      aria-label={$tr("Close modal backdrop")}
      tabindex="-1"
    ></button>

    <!-- Modal Card -->
    <div
      bind:this={modalEl}
      tabindex="-1"
      class="relative w-full max-w-lg bg-[var(--fp-card)] border border-[var(--fp-border)] rounded-xl shadow-2xl p-6 z-10 space-y-4 page-enter focus:outline-none"
      role="dialog"
      aria-modal="true"
      aria-labelledby="generated-key-title"
    >
      <!-- Header -->
      <div
        class="flex items-center justify-between border-b border-[var(--fp-border)] pb-3"
      >
        <div class="flex items-center gap-2.5">
          <div
            class="p-2 rounded-lg bg-[var(--fp-accent)]/10 text-[var(--fp-accent)]"
          >
            <Key size={18} />
          </div>
          <div>
            <h2
              id="generated-key-title"
              class="text-base font-semibold text-[var(--fp-text)]"
            >
              {$tr("Client API Key Generated")}
            </h2>
            <p class="text-xs text-[var(--fp-muted)]">
              {$tr("Saved to .env in API_KEYS")}
            </p>
          </div>
        </div>
        <button
          type="button"
          class="text-[var(--fp-muted)] hover:text-[var(--fp-text)] p-1.5 rounded-lg hover:bg-[var(--fp-surface-2)] transition-colors"
          onclick={close}
          aria-label={$tr("Close dialog")}
        >
          <X size={18} />
        </button>
      </div>

      <!-- Content -->
      <p class="text-sm text-[var(--fp-muted)]">
        {$tr(
          "Use this key to authenticate clients (omp, Claude Code CLI, Cursor, curl) against this gateway.",
        )}
      </p>

      <div
        class="fp-inset rounded-lg p-3.5 flex flex-col gap-2 bg-[var(--fp-surface)] border border-[var(--fp-border)]"
      >
        <div class="flex items-center justify-between gap-2">
          <span
            class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider"
            >{$tr("API Key")}</span
          >
          <CopyButton text={key} label="Copy Key" oncopy={onCopy} />
        </div>
        <code
          class="fp-num text-sm text-[var(--fp-accent)] break-all font-mono select-all bg-[var(--fp-surface-2)] p-2.5 rounded border border-[var(--fp-border)]"
        >
          {key}
        </code>
      </div>

      <div
        class="flex items-center justify-end gap-2 pt-2 border-t border-[var(--fp-border)]"
      >
        <Button variant="primary" size="md" onclick={close}>
          {$tr("Done")}
        </Button>
      </div>
    </div>
  </div>
{/if}
