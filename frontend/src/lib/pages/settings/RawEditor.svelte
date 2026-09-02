<script>
  import StatusBadge from "../../components/StatusBadge.svelte";
  import Button from "../../components/Button.svelte";
  import { tr } from "../../i18n.js";

  /**
   * RawEditor - the advanced raw .env editor (issue #287 split). Mirrors the
   * form fields above: the textarea is the same canonical document the shell
   * owns; every edit flows through the shell's onRawInput so the typed form
   * stays in sync, and Save posts this same content server-side.
   *
   * @prop {string} rawText
   * @prop {(value: string) => void} onRawInput
   * @prop {Array} validationErrors
   * @prop {boolean} envValid
   * @prop {number} keyCount
   * @prop {string} lastSavedTimeStr
   * @prop {() => void} onValidate
   * @prop {boolean} dirty
   * @prop {number} changedKeysCount
   * @prop {Object|null} data
   * @prop {boolean} saving
   */
  let {
    rawText,
    onRawInput,
    validationErrors = [],
    envValid = false,
    keyCount = 0,
    lastSavedTimeStr = "",
    onValidate,
    dirty = false,
    changedKeysCount = 0,
    data = null,
    saving = false,
  } = $props();
</script>

<details class="fp-card">
  <summary
    class="flex items-center justify-between gap-3 px-5 py-4 cursor-pointer text-sm font-medium text-[var(--fp-text)] list-none"
  >
    <span class="flex items-center gap-2">
      <span>{$tr("Advanced: raw .env editor")}</span>
      {#if dirty}
        <StatusBadge
          status={$tr("{count} changed", { count: changedKeysCount })}
          tone="warn"
          pulse
        />
      {/if}
    </span>
    <span class="text-[11px] text-[var(--fp-dim)]"
      >{$tr("Direct editing. Form fields above mirror this text.")}</span
    >
  </summary>
  <div class="border-t border-[var(--fp-border)] px-5 py-4">
    <div
      class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 mb-3"
    >
      <div>
        <p class="text-xs text-[var(--fp-muted)]">
          {$tr(
            "Edit environment variables directly. Save validates server-side and reloads; rejected writes are rolled back.",
          )}
        </p>
        <p class="mt-1 text-[11px] text-[var(--fp-dim)]">
          {$tr("Changes take effect after save.")}
          <kbd
            class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]"
            >Ctrl+S</kbd
          >
          {$tr("saves from the keyboard")}.
        </p>
      </div>
      <div class="flex items-center gap-2 shrink-0">
        {#if data}
          <StatusBadge
            status={data.has_env_file ? $tr("env loaded") : $tr("no env file")}
            tone={data.has_env_file ? "good" : "warn"}
          />
        {/if}
        <Button
          variant="secondary"
          size="sm"
          onclick={onValidate}
          disabled={saving}
        >
          {$tr("Validate")}
        </Button>
      </div>
    </div>

    <label for="config-env" class="sr-only"
      >{$tr("Environment file content")}</label
    >
    <textarea
      id="config-env"
      value={rawText}
      oninput={(e) => onRawInput(e.currentTarget.value)}
      spellcheck="false"
      class="fp-input fp-mono w-full min-h-[220px] text-[13px] p-3.5 resize-y
        {validationErrors.length > 0
        ? 'border-[var(--fp-error)]/60'
        : envValid
          ? 'border-[var(--fp-success)]/40'
          : ''}"
      placeholder="# Configuration variables..."></textarea>

    {#if validationErrors.length > 0}
      <div
        role="alert"
        aria-live="polite"
        class="mt-3 p-3 rounded-[var(--fp-radius-sm)] fp-inset border-[var(--fp-error)]/30 space-y-1"
      >
        <p class="text-xs font-semibold text-[var(--fp-error)]">
          {$tr("{count} validation error(s):", {
            count: validationErrors.length,
          })}
        </p>
        {#each validationErrors.slice(0, 5) as err (err)}
          <p class="text-[11px] font-mono text-[var(--fp-error)]/80">{err}</p>
        {/each}
        {#if validationErrors.length > 5}
          <p class="text-[11px] text-[var(--fp-dim)]">
            … {$tr("and {count} more", { count: validationErrors.length - 5 })}
          </p>
        {/if}
      </div>
    {/if}

    <div
      class="mt-3 flex items-center justify-between gap-3 text-[11px] text-[var(--fp-dim)] font-mono"
    >
      <span
        >{keyCount}
        {$tr("keys")}
        {#if lastSavedTimeStr}<span class="text-[var(--fp-border-bright)]"
            >|</span
          >
          {$tr("saved {time}", { time: lastSavedTimeStr })}{/if}</span
      >
    </div>
  </div>
</details>
