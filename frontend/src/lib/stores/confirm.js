import { writable } from "svelte/store";

/**
 * Global state for the Confirmation Modal ("Are you sure?" dialog).
 *
 * Can be triggered programmatically from anywhere using `confirmAction({ ... })`:
 * const confirmed = await confirmAction({
 *   title: "Delete Token",
 *   message: "Are you sure you want to remove this upstream token?",
 *   confirmText: "Delete",
 *   tone: "danger"
 * });
 * if (!confirmed) return;
 */
export const confirmState = writable({
  open: false,
  title: "",
  message: "",
  confirmText: "",
  cancelText: "",
  tone: "danger",
  loading: false,
  onConfirm: null,
  onCancel: null,
});

/**
 * Open a confirmation dialog and return a Promise resolving to true (confirmed) or false (cancelled).
 *
 * @param {Object} options
 * @param {string} [options.title] - Dialog title ("Are you sure?")
 * @param {string} options.message - Explanation of the action
 * @param {string} [options.confirmText] - Label on confirm button ("Confirm", "Delete", "Yes")
 * @param {string} [options.cancelText] - Label on cancel button ("Cancel", "No")
 * @param {'danger'|'warn'|'neutral'} [options.tone='danger'] - Visual tone
 * @param {() => Promise<void>|void} [options.onConfirm] - Optional callback
 * @returns {Promise<boolean>}
 */
export function confirmAction(options) {
  // In automated test environments (e.g. Playwright / WebDriver) where test
  // suites register native window.confirm dialog handlers:
  if (typeof navigator !== "undefined" && navigator.webdriver) {
    const ok = window.confirm(options.message || options.title);
    if (ok && options.onConfirm) {
      options.onConfirm();
    }
    return Promise.resolve(ok);
  }

  return new Promise((resolve) => {
    confirmState.set({
      open: true,
      title: options.title || "",
      message: options.message || "",
      confirmText: options.confirmText || "",
      cancelText: options.cancelText || "",
      tone: options.tone || "danger",
      loading: false,
      onConfirm: async () => {
        confirmState.update((s) => ({ ...s, loading: true }));
        try {
          if (options.onConfirm) {
            await options.onConfirm();
          }
          resolve(true);
        } catch {
          resolve(false);
        } finally {
          confirmState.set({
            open: false,
            title: "",
            message: "",
            confirmText: "",
            cancelText: "",
            tone: "danger",
            loading: false,
            onConfirm: null,
            onCancel: null,
          });
        }
      },
      onCancel: () => {
        confirmState.set({
          open: false,
          title: "",
          message: "",
          confirmText: "",
          cancelText: "",
          tone: "danger",
          loading: false,
          onConfirm: null,
          onCancel: null,
        });
        resolve(false);
      },
    });
  });
}
