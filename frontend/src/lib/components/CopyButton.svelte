<script>
  import { Clipboard, Check } from '@lucide/svelte';
  import Button from './Button.svelte';
  import { copyToClipboard } from '../utils/clipboard.js';

  /**
   * CopyButton — ghost button that copies text and confirms with a check for 1.5s.
   *
   * @prop {string} text
   * @prop {string} [label='Copy']
   */
  let { text, label = 'Copy' } = $props();
  let copied = $state(false);
  let timer;

  async function handleCopy() {
    const ok = await copyToClipboard(text);
    if (ok) {
      copied = true;
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
  aria-label={copied ? 'Copied' : label}
  title={label}
>
  {#if copied}
    <Check size={14} class="text-[var(--fp-success)]" aria-hidden="true" />
    <span>Copied</span>
  {:else}
    <Clipboard size={14} aria-hidden="true" />
    <span>{label}</span>
  {/if}
</Button>
<span aria-live="polite" aria-atomic="true" class="sr-only">{copied ? 'Copied' : ''}</span>
