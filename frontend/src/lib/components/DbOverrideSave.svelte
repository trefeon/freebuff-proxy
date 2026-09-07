<script>
  import Button from "./Button.svelte";
  import { postAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  /**
   * Per-key "Save as DB override" affordance (ADR-0019).
   *
   * Persists the row's current form value to the SQLite overlay via
   * POST /admin/api/settings and asks the parent to refetch, so badges,
   * effective values, and the .env document agree again. The classic
   * whole-file .env save flow is untouched — this is the row-level
   * alternative for operators who want one knob in the DB without
   * rewriting the file. A rejected value (POST 400, e.g. a malformed
   * lock map) surfaces inline on the row, never as a page error.
   *
   * @prop {string} settingKey - catalog key (e.g. "LOG_LEVEL")
   * @prop {string} value - current form value for the key
   * @prop {boolean} [restartOnly=false] - key applies after restart; the
   *   success note says so honestly instead of implying live-apply
   * @prop {(() => Promise<void>) | null} [onSaved=null] - parent refetch
   */
  let { settingKey, value, restartOnly = false, onSaved = null } = $props();

  let saving = $state(false);
  let status = $state(null); // { ok, text } — inline row outcome

  async function save() {
    if (saving) return;
    saving = true;
    status = null;
    try {
      const res = await postAPI(adminApi.settingsSave, {
        key: settingKey,
        value: value ?? "",
      });
      status = {
        ok: true,
        text: res?.message || $tr("Saved as DB override."),
      };
      await onSaved?.();
    } catch (e) {
      status = { ok: false, text: e.message || $tr("Save failed") };
    } finally {
      saving = false;
    }
  }
</script>

<div class="flex flex-wrap items-center gap-2">
  <Button
    variant="ghost"
    size="sm"
    onclick={save}
    disabled={saving}
    loading={saving}
    title={$tr(
      "Save this key to the DB overlay (wins over the .env file until reset)",
    )}
  >
    {$tr("Save as override")}
  </Button>
  {#if status}
    <span
      role="status"
      class="text-[11px] {status.ok
        ? 'text-[var(--fp-success)]'
        : 'text-[var(--fp-error)]'}"
      >{status.text}{#if status.ok && restartOnly}
        {$tr("Applies after restart.")}{/if}</span
    >
  {/if}
</div>
