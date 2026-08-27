<script>
  import { X, Lock, Key, AlertTriangle, Check } from '@lucide/svelte';
  import Button from './Button.svelte';
  import Alert from './Alert.svelte';
  import Field from './Field.svelte';
  import { postAPI } from '../api/client.js';
  import { tr } from '../i18n.js';

  /**
   * @prop {boolean} open
   * @prop {() => void} [onSuccess]
   * @prop {() => void} [onClose]
   */
  let { open = $bindable(false), onSuccess, onClose } = $props();

  let currentPassword = $state('');
  let newPassword = $state('');
  let confirmPassword = $state('');
  let submitting = $state(false);
  let errorMsg = $state('');
  let successMsg = $state('');
  let dialogEl = $state(null);
  let previouslyFocusedEl = null;

  function resetForm() {
    currentPassword = '';
    newPassword = '';
    confirmPassword = '';
    errorMsg = '';
    successMsg = '';
  }

  function handleClose() {
    resetForm();
    open = false;
    onClose?.();
  }

  async function handleSubmit(e) {
    e.preventDefault();
    errorMsg = '';
    successMsg = '';

    if (!currentPassword.trim()) {
      errorMsg = $tr('Please enter your current password.');
      return;
    }
    if (newPassword.length < 6) {
      errorMsg = $tr('New password must be at least 6 characters.');
      return;
    }
    if (newPassword === '123456') {
      errorMsg = $tr('New password cannot be the default password (123456).');
      return;
    }
    if (newPassword !== confirmPassword) {
      errorMsg = $tr('New passwords do not match.');
      return;
    }

    submitting = true;
    try {
      const res = await postAPI('/admin/api/change-password', {
        current_password: currentPassword.trim(),
        new_password: newPassword.trim(),
      });

      if (res.ok) {
        successMsg = res.message || $tr('Admin password updated successfully!');
        onSuccess?.();
        setTimeout(() => {
          handleClose();
        }, 1200);
      } else {
        errorMsg = res.message || $tr('Failed to update password.');
      }
    } catch (err) {
      errorMsg = err.message || $tr('Could not update password. Check connection.');
    } finally {
      submitting = false;
    }
  }

  function getFocusable(container) {
    if (!container) return [];
    const selector =
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    return Array.from(container.querySelectorAll(selector)).filter((el) => {
      // filter out hidden elements
      if (el.hasAttribute('hidden')) return false;
      // offsetParent null means display:none; keep if it's the active element
      // Use getComputedStyle as fallback for visibility
      try {
        const style = getComputedStyle(el);
        if (style.visibility === 'hidden' || style.display === 'none') return false;
      } catch {}
      return true;
    });
  }

  function trapFocus(e) {
    if (e.key !== 'Tab' || !dialogEl) return;
    const focusables = getFocusable(dialogEl);
    if (focusables.length === 0) {
      e.preventDefault();
      return;
    }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (e.shiftKey) {
      if (active === first) {
        e.preventDefault();
        last.focus();
      }
    } else {
      if (active === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  function setBackgroundInert(enabled) {
    try {
      const targets = [
        document.getElementById('main-content'),
        document.querySelector('aside[aria-label="Sidebar"]'),
        document.querySelector('header'),
      ];
      for (const el of targets) {
        if (!el) continue;
        // Avoid inert-ing the dialog itself (dialog is not inside these targets)
        if (enabled) {
          if ('inert' in el) el.inert = true;
          else el.setAttribute('inert', '');
          el.setAttribute('aria-hidden', 'true');
        } else {
          if ('inert' in el) el.inert = false;
          el.removeAttribute('inert');
          el.removeAttribute('aria-hidden');
        }
      }
    } catch {}
  }

  $effect(() => {
    if (!open) return;
    previouslyFocusedEl =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;

    // Defer focus and inert until after the dialog is mounted
    queueMicrotask(() => {
      try {
        const focusables = getFocusable(dialogEl);
        const target = focusables[0] ?? dialogEl;
        target?.focus();
        setBackgroundInert(true);
      } catch {}
    });

    const handleKeydown = (e) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        handleClose();
        return;
      }
      if (e.key === 'Tab') trapFocus(e);
    };

    document.addEventListener('keydown', handleKeydown);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';

    return () => {
      document.removeEventListener('keydown', handleKeydown);
      document.body.style.overflow = prevOverflow;
      try {
        setBackgroundInert(false);
      } catch {}
      queueMicrotask(() => {
        try {
          previouslyFocusedEl?.focus();
        } catch {}
      });
    };
  });
</script>

{#if open}
  <div class="fixed inset-0 z-50 flex items-center justify-center p-4">
    <!-- Backdrop -->
    <button
      type="button"
      class="fixed inset-0 bg-black/75 backdrop-blur-sm transition-opacity border-0 p-0 m-0 w-full h-full cursor-default"
      onclick={handleClose}
      aria-label={$tr('Close dialog backdrop')}
      tabindex="-1"
    ></button>

    <!-- Modal Card -->
    <div
      bind:this={dialogEl}
      tabindex="-1"
      class="relative w-full max-w-md bg-[var(--fp-card)] border border-[var(--fp-border)] rounded-xl shadow-2xl p-6 z-10 space-y-5 page-enter focus:outline-none"
      role="dialog"
      aria-modal="true"
      aria-labelledby="modal-title"
    >
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-[var(--fp-border)] pb-4">
        <div class="flex items-center gap-2.5">
          <div class="w-8 h-8 rounded-lg bg-[var(--fp-accent)]/10 text-[var(--fp-accent)] flex items-center justify-center">
            <Lock size={18} />
          </div>
          <div>
            <h2 id="modal-title" class="text-sm font-semibold text-[var(--fp-text)]">{$tr('Change Admin Password')}</h2>
            <p class="text-xs text-[var(--fp-muted)]">{$tr('Update the master administrative password')}</p>
          </div>
        </div>
        <button
          type="button"
          onclick={handleClose}
          class="min-w-11 min-h-11 w-11 h-11 flex items-center justify-center text-[var(--fp-dim)] hover:text-[var(--fp-text)] rounded-lg hover:bg-[var(--fp-border)]/50 transition-colors shrink-0"
          aria-label={$tr('Close modal')}
        >
          <X size={16} />
        </button>
      </div>

      <!-- Messages -->
      {#if errorMsg}
        <Alert tone="error">{errorMsg}</Alert>
      {/if}
      {#if successMsg}
        <Alert tone="success">{successMsg}</Alert>
      {/if}

      <!-- Form -->
      <form onsubmit={handleSubmit} class="space-y-4">
        <Field label={$tr('Current Password')} id="current-password">
          <input
            id="current-password"
            type="password"
            autocomplete="current-password"
            bind:value={currentPassword}
            placeholder={$tr('Enter current password (default: 123456)')}
            required
            class="w-full px-3 py-2 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
          />
        </Field>

        <Field label={$tr('New Password')} id="new-password" hint={$tr('Minimum 6 characters')}>
          <input
            id="new-password"
            type="password"
            autocomplete="new-password"
            bind:value={newPassword}
            placeholder={$tr('Enter secure new password')}
            required
            minlength="6"
            class="w-full px-3 py-2 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
          />
        </Field>

        <Field label={$tr('Confirm New Password')} id="confirm-password">
          <input
            id="confirm-password"
            type="password"
            autocomplete="new-password"
            bind:value={confirmPassword}
            placeholder={$tr('Re-enter new password')}
            required
            minlength="6"
            class="w-full px-3 py-2 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
          />
        </Field>

        <div class="flex items-center justify-end gap-3 pt-3 border-t border-[var(--fp-border)]">
          <Button variant="ghost" onclick={handleClose} disabled={submitting}>
            {$tr('Cancel')}
          </Button>
          <Button variant="primary" type="submit" loading={submitting}>
            <Key size={14} />
            {$tr('Update Password')}
          </Button>
        </div>
      </form>
    </div>
  </div>
{/if}
