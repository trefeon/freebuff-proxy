<script>
  import { Clipboard, Check } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import { copyToClipboard } from "../utils/clipboard.js";

  /**
   * CopyButton — ghost button that copies text and confirms with a check for 1.5s.
   *
   * @prop {string} text — text to copy
   * @prop {string} [label='Copy'] — accessible label; also the visible caption unless iconOnly
   * @prop {boolean} [iconOnly=false] — icon without visible caption (table cells, mobile cards)
   * @prop {() => void} [oncopy]
   */
  let { text, label = "Copy", oncopy, iconOnly = false } = $props();
  let copied = $state(false);
  let timer;

  async function handleCopy() {
    const ok = await copyToClipboard(text);
    if (ok) {
      copied = true;
      oncopy?.();
      clearTimeout(timer);
      timer = setTimeout(() => {
        copied = false;
      }, 1500);
    }
  }
</script>

<Button
  variant="ghost"
  size="sm"
  onclick={handleCopy}
  aria-label={copied ? "Copied" : label}
  title={label}
  class={iconOnly ? "fp-copy-icon" : ""}
>
  {#if copied}
    <Check size={14} class="text-[var(--fp-success)]" aria-hidden="true" />
    {#if !iconOnly}<span>Copied</span>{/if}
  {:else}
    <Clipboard size={14} aria-hidden="true" />
    {#if !iconOnly}<span>{label}</span>{/if}
  {/if}
</Button>
<span aria-live="polite" aria-atomic="true" class="sr-only"
  >{copied ? "Copied" : ""}</span
>
