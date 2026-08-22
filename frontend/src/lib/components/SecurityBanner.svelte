<script>
  import { AlertTriangle } from '@lucide/svelte';
  import { tr } from '../i18n.js';
  import Button from './Button.svelte';

  /**
   * @prop {() => void} [onChangePassword]
   */
  let { onChangePassword } = $props();
</script>

<div
  class="bg-[#fbbf24]/10 border border-[#fbbf24]/30 rounded-lg px-4 py-3 flex flex-col sm:flex-row items-start sm:items-center justify-between gap-3"
  role="alert"
>
  <div class="flex items-center gap-3">
    <div class="w-8 h-8 rounded-lg bg-[#fbbf24]/20 text-[#fbbf24] flex items-center justify-center shrink-0">
      <AlertTriangle size={18} />
    </div>
    <div class="text-sm">
      <span class="font-semibold text-[#fbbf24]">{$tr('Security Warning')}</span>
      <span class="text-[var(--fp-muted)]"> — {$tr('You are using the default admin password (123456). Change it immediately to secure this instance.')}</span>
    </div>
  </div>
  <Button variant="primary" size="sm" onclick={onChangePassword}>
    {$tr('Change Password')}
  </Button>
</div>
