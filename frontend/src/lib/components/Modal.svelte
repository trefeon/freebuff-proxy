<script>
  import { X } from "@lucide/svelte";
  import { tr } from "../i18n.js";

  /**
   * Modal — Canonical accessible dialog template for the freebuff dashboard.
   *
   * Features:
   * - ARIA compliant: role="dialog" or "alertdialog", aria-modal="true"
   * - Focus trap: Tab and Shift+Tab cycle within the modal card
   * - Escape key handling (when closable and not loading)
   * - Focus restoration: returns focus to the trigger element on close (Issue #224)
   * - Body scroll locking while open
   * - Backdrop click to close (when closable and not loading)
   * - Svelte 5 snippets for icon, header, children (body), and footer
   *
   * @prop {boolean} [open=false] — bindable open state
   * @prop {string} [title=""] — dialog title
   * @prop {string} [description=""] — dialog subtitle / hint
   * @prop {'sm'|'md'|'lg'|'xl'|'full'} [size='md'] — max-width constraint
   * @prop {'dialog'|'alertdialog'} [role='dialog']
   * @prop {boolean} [closable=true] — show close button and allow Esc / backdrop close
   * @prop {boolean} [loading=false] — disables closing while in-flight
   * @prop {string} [class=""] — additional classes for modal card
   * @prop {() => void} [onClose] — callback when closed
   */
  let {
    open = $bindable(false),
    title = "",
    description = "",
    size = "md",
    role = "dialog",
    closable = true,
    loading = false,
    class: className = "",
    onClose,
    icon,
    header,
    children,
    footer,
  } = $props();

  const sizeClasses = {
    sm: "max-w-sm",
    md: "max-w-md",
    lg: "max-w-lg",
    xl: "max-w-xl",
    full: "max-w-3xl",
  };

  let modalEl = $state(null);
  let lastFocusedEl = null;

  function handleClose() {
    if (loading || !closable) return;
    open = false;
    onClose?.();
    restoreFocus();
  }

  function restoreFocus() {
    if (lastFocusedEl && typeof lastFocusedEl.focus === "function") {
      lastFocusedEl.focus();
      lastFocusedEl = null;
    }
  }

  function handleKeydown(e) {
    if (!open) return;
    if (e.key === "Escape") {
      if (closable && !loading) {
        e.preventDefault();
        handleClose();
      }
      return;
    }
    if (e.key === "Tab" && modalEl) {
      const focusable = modalEl.querySelectorAll(
        'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
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

    // Lock body scrolling
    const origOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";

    // Auto-focus preferred target or fallback to first focusable element
    queueMicrotask(() => {
      if (!open || !modalEl) return;
      const autoFocusEl = modalEl.querySelector("[data-autofocus]");
      if (autoFocusEl && typeof autoFocusEl.focus === "function") {
        autoFocusEl.focus();
        return;
      }
      const firstFocusable = modalEl.querySelector(
        'input:not([disabled]), button:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (firstFocusable && typeof firstFocusable.focus === "function") {
        firstFocusable.focus();
      } else {
        modalEl.focus();
      }
    });

    return () => {
      document.body.style.overflow = origOverflow;
    };
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
      onclick={handleClose}
      aria-label={$tr("Close dialog")}
      tabindex="-1"
    ></button>

    <!-- Dialog Card -->
    <div
      bind:this={modalEl}
      tabindex="-1"
      class="relative w-full {sizeClasses[size] ||
        sizeClasses.md} bg-[var(--fp-surface)] border border-[var(--fp-border)] rounded-xl shadow-2xl p-6 z-10 space-y-4 page-enter focus:outline-none {className}"
      {role}
      aria-modal="true"
      aria-label={title || undefined}
    >
      <!-- Header -->
      {#if header}
        {@render header()}
      {:else if title || icon || closable}
        <div class="flex items-start justify-between gap-3.5">
          <div class="flex items-center gap-3 min-w-0 flex-1">
            {#if icon}
              <div class="shrink-0">
                {@render icon()}
              </div>
            {/if}
            <div class="space-y-0.5 min-w-0 flex-1">
              {#if title}
                <h2
                  class="text-base font-semibold text-[var(--fp-text)] truncate"
                >
                  {title}
                </h2>
              {/if}
              {#if description}
                <p class="text-xs text-[var(--fp-muted)] leading-relaxed">
                  {description}
                </p>
              {/if}
            </div>
          </div>

          {#if closable}
            <button
              type="button"
              class="text-[var(--fp-muted)] hover:text-[var(--fp-text)] p-1.5 rounded-lg hover:bg-[var(--fp-surface-2)] transition-colors shrink-0 disabled:opacity-50 disabled:cursor-not-allowed"
              onclick={handleClose}
              disabled={loading}
              aria-label={$tr("Close dialog")}
            >
              <X size={18} />
            </button>
          {/if}
        </div>
      {/if}

      <!-- Body / Children -->
      {#if children}
        <div class="min-h-0 flex-1">
          {@render children()}
        </div>
      {/if}

      <!-- Footer -->
      {#if footer}
        <div
          class="flex items-center justify-end gap-2.5 pt-3 border-t border-[var(--fp-border)]"
        >
          {@render footer()}
        </div>
      {/if}
    </div>
  </div>
{/if}
