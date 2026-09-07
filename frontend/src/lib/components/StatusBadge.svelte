<script>
  /**
   * StatusBadge — terminal bracket telemetry badge ([+] READY / [!] WARN).
   *
   * Tone derived from status when not given. Glyph + tinted hairline box;
   * text content unchanged so existing pins hold.
   *
   * @prop {string} status
   * @prop {'good'|'warn'|'bad'|'critical'|'info'|'idle'} [tone='info']
   * @prop {boolean} [pulse=false]
   */
  let { status, tone, pulse = false } = $props();

  const toneFromStatus = {
    idle: "good",
    leased: "good",
    active: "good",
    cooldown: "warn",
    locked: "warn",
    banned: "bad",
    error: "bad",
  };

  const glyphs = {
    good: "+",
    warn: "!",
    bad: "-",
    critical: "-",
    info: "*",
    idle: "○",
  };

  const toneClasses = {
    good: "text-[var(--fp-accent)] border-[var(--fp-accent)]/30 bg-[var(--fp-accent-dim)]",
    warn: "text-[var(--fp-warning)] border-[var(--fp-warning)]/30 bg-[var(--fp-warning)]/10",
    bad: "text-[var(--fp-error)] border-[var(--fp-error)]/30 bg-[var(--fp-error)]/10",
    critical:
      "text-[var(--fp-error)] border-[var(--fp-error)]/50 bg-[var(--fp-error)]/10",
    info: "text-[var(--fp-info)] border-[var(--fp-info)]/30 bg-[var(--fp-info)]/10",
    idle: "text-[var(--fp-dim)] border-[var(--fp-border)] bg-[var(--fp-surface)]",
  };

  let resolvedTone = $derived(tone || toneFromStatus[status] || "info");
</script>

<span
  class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded-[3px] border font-mono text-[11px] uppercase tracking-wider {toneClasses[
    resolvedTone
  ]} {pulse ? 'led-pulse' : ''}"
>
  <span aria-hidden="true">[{glyphs[resolvedTone]}]</span>
  <span>{status}</span>
</span>
