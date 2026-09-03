<script>
  import { onMount } from "svelte";
  import { Shield, Key, Eye, EyeOff } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import Alert from "./Alert.svelte";
  import { postAPI, fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { updateAuthState } from "../stores/session.js";

  /**
   * @prop {boolean} [isDefaultAdminToken]
   * @prop {boolean} [hasPassword]
   * @prop {() => void} [onSuccess]
   */
  let {
    isDefaultAdminToken = $bindable(false),
    hasPassword = $bindable(true),
    onSuccess,
  } = $props();
  let currentPassword = $state("");
  let newPassword = $state("");
  let confirmPassword = $state("");

  let showCurrentPassword = $state(false);
  let showNewPassword = $state(false);
  let showConfirmPassword = $state(false);

  let submitting = $state(false);
  let errorMsg = $state("");
  let successMsg = $state("");

  onMount(async () => {
    try {
      const data = await fetchAPI(adminApi.authStatus);
      if (data) {
        if (data.is_default_admin_token !== undefined) {
          isDefaultAdminToken = Boolean(data.is_default_admin_token);
        }
        if (data.has_password !== undefined) {
          hasPassword = Boolean(data.has_password);
        }
        updateAuthState({
          isDefaultAdminToken: Boolean(data.is_default_admin_token),
          hasPassword: Boolean(data.has_password),
        });
      }
    } catch {}
  });
  let canSubmit = $derived.by(() => {
    if (hasPassword && !currentPassword.trim()) return false;
    if (!newPassword || newPassword.length < 6) return false;
    if (newPassword === "123456") return false;
    if (newPassword !== confirmPassword) return false;
    return true;
  });

  async function handleSubmit(e) {
    e.preventDefault();
    errorMsg = "";
    successMsg = "";

    if (hasPassword && !currentPassword.trim()) {
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
        current_password: hasPassword ? currentPassword.trim() : "",
        new_password: newPassword.trim(),
      });

      if (res.ok) {
        successMsg = res.message || $tr("Admin password updated successfully!");
        currentPassword = "";
        newPassword = "";
        confirmPassword = "";
        isDefaultAdminToken = false;
        hasPassword = true;
        updateAuthState({
          isDefaultAdminToken: false,
          hasPassword: true,
        });
        onSuccess?.();
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

<div
  class="bg-surface border border-border-subtle rounded-[14px] shadow-[var(--shadow-soft)] p-6 page-enter"
>
  <!-- Card Header -->
  <div class="flex items-center gap-3 mb-4">
    <div class="p-2 rounded-lg bg-primary/10 text-primary shrink-0">
      <Shield size={20} />
    </div>
    <h3 class="text-base sm:text-lg font-semibold text-[var(--fp-text)]">
      {$tr("Security")}
    </h3>
  </div>

  <div class="flex flex-col gap-4">
    <!-- Password Change Form -->
    <form onsubmit={handleSubmit} class="flex flex-col gap-4">
      {#if hasPassword}
        <div class="flex flex-col gap-2">
          <div class="flex items-center justify-between">
            <label
              for="sec-current-password"
              class="text-xs sm:text-sm font-medium text-[var(--fp-text)]"
            >
              {$tr("Current Password")}
            </label>
            {#if isDefaultAdminToken}
              <span
                class="text-[11px] text-[var(--fp-warning)] flex items-center gap-1 font-mono"
              >
                {$tr("(Default: 123456)")}
              </span>
            {/if}
          </div>
          <div class="relative">
            <input
              id="sec-current-password"
              type={showCurrentPassword ? "text" : "password"}
              bind:value={currentPassword}
              placeholder={$tr("Enter current password")}
              class="fp-input pr-10"
              autocomplete="current-password"
              disabled={submitting}
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
        </div>
      {/if}

      <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
        <div class="flex flex-col gap-2">
          <label
            for="sec-new-password"
            class="text-xs sm:text-sm font-medium text-[var(--fp-text)]"
          >
            {$tr("New Password")}
          </label>
          <div class="relative">
            <input
              id="sec-new-password"
              type={showNewPassword ? "text" : "password"}
              bind:value={newPassword}
              placeholder={$tr("Enter new password")}
              class="fp-input pr-10"
              autocomplete="new-password"
              disabled={submitting}
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
          {#if newPassword && newPassword.length < 6}
            <p class="text-[11px] text-[var(--fp-warning)]">
              {$tr("Minimum 6 characters")}
            </p>
          {:else if newPassword === "123456"}
            <p class="text-[11px] text-[var(--fp-error)]">
              {$tr("Cannot be factory default (123456)")}
            </p>
          {/if}
        </div>

        <div class="flex flex-col gap-2">
          <label
            for="sec-confirm-password"
            class="text-xs sm:text-sm font-medium text-[var(--fp-text)]"
          >
            {$tr("Confirm New Password")}
          </label>
          <div class="relative">
            <input
              id="sec-confirm-password"
              type={showConfirmPassword ? "text" : "password"}
              bind:value={confirmPassword}
              placeholder={$tr("Confirm new password")}
              class="fp-input pr-10"
              autocomplete="new-password"
              disabled={submitting}
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
          {#if confirmPassword && newPassword !== confirmPassword}
            <p class="text-[11px] text-[var(--fp-error)]">
              {$tr("Passwords do not match")}
            </p>
          {/if}
        </div>
      </div>

      {#if errorMsg}
        <Alert tone="error">{errorMsg}</Alert>
      {/if}
      {#if successMsg}
        <Alert tone="success">{successMsg}</Alert>
      {/if}

      <div class="pt-2">
        <Button
          type="submit"
          variant="primary"
          loading={submitting}
          disabled={!canSubmit || submitting}
        >
          <Key size={14} />
          {$tr("Update Password")}
        </Button>
      </div>
    </form>
  </div>
</div>
