<script>
  import { Key } from "@lucide/svelte";
  import Modal from "./Modal.svelte";
  import CopyButton from "./CopyButton.svelte";
  import Button from "./Button.svelte";
  import { tr } from "../i18n.js";

  /**
   * GeneratedKeyModal — modal shown after a new client API key is generated.
   * Built on top of the canonical Modal template component.
   *
   * @prop {boolean} open
   * @prop {string} key — the newly generated key to display/copy
   * @prop {() => void} [onClose]
   * @prop {() => void} [onCopy]
   */
  let { open = $bindable(false), key, onClose, onCopy } = $props();

  function close() {
    open = false;
    onClose?.();
  }
</script>

<Modal
  bind:open
  title={$tr("Client API Key Generated")}
  description={$tr("Saved to .env in API_KEYS")}
  onClose={close}
  size="md"
>
  {#snippet icon()}
    <div
      class="p-2 rounded-lg bg-[var(--fp-accent)]/10 text-[var(--fp-accent)]"
    >
      <Key size={18} />
    </div>
  {/snippet}

  <p class="text-sm text-[var(--fp-muted)] mb-3">
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

  {#snippet footer()}
    <Button variant="primary" size="md" onclick={close}>
      {$tr("Done")}
    </Button>
  {/snippet}
</Modal>
