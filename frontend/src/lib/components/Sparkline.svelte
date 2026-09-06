<!-- Sparkline — minimal usage-over-time polyline for history-backed cards.
     Purely presentational: parents pass numeric samples + an accessible
     label. Stroke uses the accent token; no fill, no grid, no axes. -->
<script>
  let { values = [], width = 120, height = 30, label = "trend" } = $props();

  const pts = $derived.by(() => {
    const vs = (values ?? []).filter((v) => Number.isFinite(v));
    if (vs.length < 2) return "";
    const min = Math.min(...vs);
    const max = Math.max(...vs);
    const span = max - min || 1;
    const n = vs.length;
    return vs
      .map((v, i) => {
        const x = (i / (n - 1)) * (width - 4) + 2;
        const y = height - 3 - ((v - min) / span) * (height - 6);
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      })
      .join(" ");
  });
</script>

{#if pts}
  <svg
    {width}
    {height}
    viewBox={`0 0 ${width} ${height}`}
    role="img"
    aria-label={label}
    class="shrink-0"
  >
    <polyline
      points={pts}
      fill="none"
      stroke="var(--fp-accent)"
      stroke-width="1.5"
      stroke-linejoin="round"
      stroke-linecap="round"
    />
  </svg>
{/if}
