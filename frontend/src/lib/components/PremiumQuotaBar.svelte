<script>
  import { tr } from '../i18n.js';

  let { quota, title = null, now = Date.now() } = $props();

  let pct = $derived(Math.min(100, Math.max(0, quota?.percent_used ?? 0)));
  let barColor = $derived(
    pct >= 100 ? '#ef4444' : pct >= 80 ? '#f97316' : pct >= 60 ? '#f59e0b' : '#10b981'
  );

  function fmtRel(iso, nowMs) {
    if (!iso) return '—';
    const t = new Date(iso).getTime();
    if (isNaN(t)) return iso;
    const ms = t - nowMs;
    if (ms <= 0) return 'now';
    const mins = Math.floor(ms / 60000);
    const h = Math.floor(mins / 60);
    const m = mins % 60;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m`;
    return `${Math.max(1, Math.floor(ms / 1000))}s`;
  }

  let rel = $derived(fmtRel(quota?.reset_at, now));
  let badge = $derived(`${quota?.limit ?? '—'}/day ${quota?.period ?? ''}`.trim());
  let label = $derived(title ?? $tr('Premium pool'));
</script>

<div class="rounded border border-[var(--fp-border)] bg-[var(--fp-bg)]/60 p-3">
  <div class="flex items-center justify-between gap-2 mb-2">
    <div class="flex items-center gap-2 min-w-0">
      <p class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-text)] truncate">{label}</p>
      <span class="fp-num shrink-0 text-[10px] leading-none px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-muted)]">{badge}</span>
    </div>
    {#if quota?.capped}
      <span class="shrink-0 text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-[#ef4444]/15 text-[#ef4444] border border-[#ef4444]/30">{$tr('Quota exhausted')}</span>
    {/if}
  </div>

  <div class="h-[6px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden" role="progressbar" aria-valuenow={pct} aria-valuemin="0" aria-valuemax="100">
    <div class="h-full rounded-full transition-all duration-300" style="width: {pct}%; background: {barColor}"></div>
  </div>

  <div class="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
    <span class="fp-num text-[var(--fp-muted)] tabular-nums">
      Used <span class="text-[var(--fp-text)] font-medium">{quota?.used ?? '—'}</span> / Limit <span class="text-[var(--fp-text)] font-medium">{quota?.limit ?? '—'}</span> • {$tr('Remaining')} <span class="text-[var(--fp-text)] font-medium">{quota?.remaining ?? '—'}</span>
    </span>
  </div>

  <div class="mt-1 fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
    {$tr('Resets in')} {rel} — {quota?.reset_at ?? '—'}
  </div>
</div>
