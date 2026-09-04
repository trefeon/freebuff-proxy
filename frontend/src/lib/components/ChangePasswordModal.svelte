<script>
  import { Lock, Key, Eye, EyeOff } from "@lucide/svelte";
  import Modal from "./Modal.svelte";
  import Button from "./Button.svelte";
  import Alert from "./Alert.svelte";
  import Field from "./Field.svelte";
  import { postAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  /**
   * ChangePasswordModal — modal for updating master administrative password.
   * Built on top of the canonical Modal template component.
   *
   * @prop {boolean} open
   * @prop {() => void} [onSuccess]
   * @prop {() => void} [onClose]
   */
  let { open = $bindable(false), onSuccess, onClose } = $props();

  let currentPassword = $state("");
  let newPassword = $state("");
  let confirmPassword = $state("");
  let submitting = $state(false);
  let showCurrentPassword = $state(false);
  let showNewPassword = $state(false);
  let showConfirmPassword = $state(false);
  let errorMsg = $state("");
  let successMsg = $state("");

  function resetForm() {
    currentPassword = "";
    newPassword = "";
    confirmPassword = "";
    errorMsg = "";
    successMsg = "";
  }

  function handleClose() {
    resetForm();
    open = false;
    onClose?.();
  }

  async function handleSubmit(e) {
    e.preventDefault();
    errorMsg = "";
    successMsg = "";

    if (!currentPassword.trim()) {
      errorMsg = $tr("Please enter your current password.");
      return;
    }
    if (newPassword.length < 6) {
      errorMsg = $tr("New password must be at least 6 characters.");
      return;
    }
    if (newPassword === "123456") {
      errorMsg = $tr("New password cannot be the default password (123456).");
      return;
    }
    if (newPassword !== confirmPassword) {
      errorMsg = $tr("New passwords do not match.");
      return;
    }

    submitting = true;
    try {
      const res = await postAPI(adminApi.changePassword, {
        current_password: currentPassword.trim(),
        new_password: newPassword.trim(),
      });

      if (res.ok) {
        successMsg = res.message || $tr("Admin password updated successfully!");
        onSuccess?.();
        setTimeout(() => {
          handleClose();
        }, 1200);
      } else {
        errorMsg = res.message || $tr("Failed to update password.");
      }
    } catch (err) {
      errorMsg =
        err.message || $tr("Could not update password. Check connection.");
    } finally {
      submitting = false;
    }
  }
</script>

<Modal
  bind:open
  title={$tr("Change Admin Password")}
  description={$tr("Update the master administrative password")}
  onClose={handleClose}
  loading={submitting}
  size="md"
>
  {#snippet icon()}
    <div
      class="w-8 h-8 rounded-lg bg-[var(--fp-accent)]/10 text-[var(--fp-accent)] flex items-center justify-center"
    >
      <Lock size={18} />
    </div>
  {/snippet}

  {#if errorMsg}
    <div class="mb-4">
      <Alert tone="error">{errorMsg}</Alert>
    </div>
  {/if}
  {#if successMsg}
    <div class="mb-4">
      <Alert tone="success">{successMsg}</Alert>
    </div>
  {/if}

  <form onsubmit={handleSubmit} class="space-y-4">
    <Field label={$tr("Current Password")} id="current-password">
      <div class="relative">
        <input
          id="current-password"
          data-autofocus
          type={showCurrentPassword ? "text" : "password"}
          autocomplete="current-password"
          bind:value={currentPassword}
          placeholder={$tr("Enter current password (default: 123456)")}
          required
          class="w-full px-3 py-2 pr-10 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
        />
        <button
          type="button"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded transition-colors"
          onclick={() => (showCurrentPassword = !showCurrentPassword)}
          aria-label={showCurrentPassword
            ? $tr("Hide password")
            : $tr("Show password")}
        >
          {#if showCurrentPassword}
            <EyeOff size={16} />
          {:else}
            <Eye size={16} />
          {/if}
        </button>
      </div>
    </Field>

    <Field
      label={$tr("New Password")}
      id="new-password"
      hint={$tr("Minimum 6 characters")}
    >
      <div class="relative">
        <input
          id="new-password"
          type={showNewPassword ? "text" : "password"}
          autocomplete="new-password"
          bind:value={newPassword}
          placeholder={$tr("Enter secure new password")}
          required
          minlength="6"
          class="w-full px-3 py-2 pr-10 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
        />
        <button
          type="button"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded transition-colors"
          onclick={() => (showNewPassword = !showNewPassword)}
          aria-label={showNewPassword
            ? $tr("Hide password")
            : $tr("Show password")}
        >
          {#if showNewPassword}
            <EyeOff size={16} />
          {:else}
            <Eye size={16} />
          {/if}
        </button>
      </div>
    </Field>

    <Field label={$tr("Confirm New Password")} id="confirm-password">
      <div class="relative">
        <input
          id="confirm-password"
          type={showConfirmPassword ? "text" : "password"}
          autocomplete="new-password"
          bind:value={confirmPassword}
          placeholder={$tr("Re-enter new password")}
          required
          minlength="6"
          class="w-full px-3 py-2 pr-10 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
        />
        <button
          type="button"
          class="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded transition-colors"
          onclick={() => (showConfirmPassword = !showConfirmPassword)}
          aria-label={showConfirmPassword
            ? $tr("Hide password")
            : $tr("Show password")}
        >
          {#if showConfirmPassword}
            <EyeOff size={16} />
          {:else}
            <Eye size={16} />
          {/if}
        </button>
      </div>
    </Field>

    <div
      class="flex items-center justify-end gap-3 pt-3 border-t border-[var(--fp-border)]"
    >
      <Button variant="ghost" onclick={handleClose} disabled={submitting}>
        {$tr("Cancel")}
      </Button>
      <Button variant="primary" type="submit" loading={submitting}>
        <Key size={14} />
        {$tr("Update Password")}
      </Button>
    </div>
  </form>
</Modal>
