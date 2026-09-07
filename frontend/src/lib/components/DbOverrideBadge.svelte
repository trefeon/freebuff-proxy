<script>
  import { RotateCcw } from "@lucide/svelte";
  import { tr } from "../i18n.js";

  /**
   * DB-override badge + reset (ADR-0019).
   *
   * Rendered inside a SettingsRow badge slot when the settings source map
   * reports `db` for the row's key: the effective value comes from the SQLite
   * overlay and wins over the .env document until reset. Reset issues
   * DELETE /admin/api/settings/:key through the parent's onReset and the
   * parent refetches, so this component holds no data state of its own.
   *
   * @prop {string} settingKey - catalog key (e.g. "SAFE_MODE")
   * @prop {(key: string) => Promise<void>} [onReset] - parent reset handler
   */
  let { settingKey, onReset = null } = $props();

  let resetting = $state(false);

  async function reset() {
    if (resetting || !onReset) return;
    resetting = true;
    try {
      await onReset(settingKey);
    } finally {
      resetting = false;
    }
  }
</script>

<span
  class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-accent)]/40 bg-[var(--fp-accent)]/10 text-[var(--fp-accent)] font-semibold uppercase tracking-wider shrink-0"
  title={$tr(
    "This value comes from the DB overlay and wins over the .env file",
  )}>{$tr("DB override")}</span
>
{#if onReset}
  <button
    type="button"
    class="text-[10px] font-semibold uppercase tracking-wider shrink-0 inline-flex items-center gap-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)] disabled:opacity-50 cursor-pointer"
    onclick={reset}
    disabled={resetting}
    title={$tr("Delete the DB override; the value falls back to file/env")}
  >
    <RotateCcw size={11} />
    {$tr("Reset")}
  </button>
{/if}
