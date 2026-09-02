<script>
  import {
    AlertCircle,
    CheckCircle2,
    AlertTriangle,
    Info,
  } from "@lucide/svelte";

  /**
   * Alert — status banner: icon + 2px tinted left border + 14% tone fill.
   *
   * @prop {'info'|'success'|'warning'|'error'} [tone='info']
   * @prop {string} [title]
   */
  let { tone = "info", title, children } = $props();

  const icons = {
    info: Info,
    success: CheckCircle2,
    warning: AlertTriangle,
    error: AlertCircle,
  };
  const fills = {
    info: "border-[var(--fp-info)]/60 bg-[var(--fp-info)]/14",
    success: "border-[var(--fp-success)]/60 bg-[var(--fp-success)]/14",
    warning: "border-[var(--fp-warning)]/60 bg-[var(--fp-warning)]/14",
    error: "border-[var(--fp-error)]/60 bg-[var(--fp-error)]/14",
  };
  const iconTones = {
    info: "text-[var(--fp-info)]",
    success: "text-[var(--fp-success)]",
    warning: "text-[var(--fp-warning)]",
    error: "text-[var(--fp-error)]",
  };

  let Icon = $derived(icons[tone] || Info);
</script>

<div
  role="alert"
  class="flex items-start gap-3 rounded-r border-l-2 px-4 py-3 {fills[tone] ||
    fills.info}"
>
  <Icon size={18} class="mt-0.5 shrink-0 {iconTones[tone] || iconTones.info}" />
  <div class="min-w-0 flex-1">
    {#if title}
      <p class="text-sm font-semibold text-[var(--fp-text)]">{title}</p>
    {/if}
    {#if children}
      <div class="text-sm text-[var(--fp-muted)]">{@render children()}</div>
    {/if}
  </div>
</div>
