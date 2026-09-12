<script>
  import {
    AlertCircle,
    CheckCircle2,
    AlertTriangle,
    Info,
  } from "@lucide/svelte";

  /**
   * Alert — status banner: icon + 2px tinted left border + 14% tone fill.
   * Auto-dismisses with a fade after dismissAfter ms unless sticky.
   *
   * @prop {'info'|'success'|'warning'|'error'} [tone='info']
   * @prop {string} [title]
   * @prop {number} [dismissAfter=10000] ms before fade-out starts; <=0 disables
   * @prop {boolean} [sticky=false] never auto-dismiss (blocking states only)
   */
  let {
    tone = "info",
    title,
    children,
    dismissAfter = 10000,
    sticky = false,
  } = $props();

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
  };

  let Icon = $derived(icons[tone] || Info);

  let visible = $state(true);
  let fading = $state(false);
  $effect(() => {
    // Re-subscribe to the message: every new title restarts the countdown.
    void title;
    if (sticky || dismissAfter <= 0) return;
    visible = true;
    fading = false;
    const fadeTimer = setTimeout(
      () => (fading = true),
      Math.max(0, dismissAfter - 400),
    );
    const hideTimer = setTimeout(() => (visible = false), dismissAfter);
    return () => {
      clearTimeout(fadeTimer);
      clearTimeout(hideTimer);
    };
  });
</script>

{#if visible}
  <div
    role="alert"
    class="flex items-start gap-3 rounded-r border-l-2 px-4 py-3 transition-opacity duration-300 {fading
      ? 'opacity-0'
      : 'opacity-100'} {fills[tone] || fills.info}"
  >
    <Icon
      size={18}
      class="mt-0.5 shrink-0 {iconTones[tone] || iconTones.info}"
    />
    <div class="min-w-0 flex-1">
      {#if title}
        <p class="text-sm font-semibold text-[var(--fp-text)]">{title}</p>
      {/if}
      {#if children}
        <div class="text-sm text-[var(--fp-muted)]">{@render children()}</div>
      {/if}
    </div>
  </div>
{/if}
