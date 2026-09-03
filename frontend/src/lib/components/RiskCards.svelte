<script>
  import StatusBadge from "./StatusBadge.svelte";
  import Card from "./Card.svelte";
  import { formatLocalDate } from "../utils/format.js";
  import { tr } from "../i18n.js";

  /**
   * RiskCards — at-risk token cards (cooldown / banned / risk-level status).
   *
   * @prop {Array} tokens — pool tokens with risk_level !== 'low'
   * @prop {number} total — total pool size, shown as a counter
   */
  let { tokens, total } = $props();

  function riskTone(risk) {
    switch (risk) {
      case "low":
        return "good";
      case "moderate":
        return "warn";
      case "high":
      case "critical":
        return "bad";
      default:
        return "idle";
    }
  }

  function banBadge(t) {
    if (t.ban_type === "hard") {
      return {
        label: $tr("banned — appeal required"),
        tone: "critical",
        pulse: true,
      };
    }
    if (t.ban_type === "temporary") {
      const until = formatLocalDate(t.banned_until);
      return {
        label: until
          ? $tr("banned until {time}", { time: until })
          : $tr("banned (temporary)"),
        tone: "bad",
      };
    }
    return null;
  }

  let nowTick = $state(Date.now());
  $effect(() => {
    const timer = setInterval(() => {
      nowTick = Date.now();
    }, 1000);
    return () => clearInterval(timer);
  });

  function formatCooldown(until) {
    if (!until) return null;
    const diff = new Date(until).getTime() - nowTick;
    if (diff <= 0) return { label: $tr("expiring..."), active: false };
    const s = Math.floor(diff / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h >= 24) {
      const d = Math.floor(h / 24);
      const hr = h % 24;
      return { label: hr > 0 ? `${d}d ${hr}h` : `${d}d`, active: true };
    }
    if (h > 0) return { label: `${h}h ${m}m`, active: true };
    if (m > 0) return { label: `${m}m ${sec}s`, active: true };
    return { label: `${sec}s`, active: true };
  }
</script>

<section aria-label="At-risk tokens">
  <div class="flex items-center justify-between mb-3">
    <h2 class="text-lg font-semibold text-[var(--fp-text)]">
      {$tr("Account risk")}
    </h2>
    <span class="fp-num text-xs text-[var(--fp-dim)]"
      >{tokens.length}/{total}</span
    >
  </div>

  {#if tokens.length > 0}
    <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4">
      {#each tokens as t (t.index)}
        <Card>
          <div class="space-y-3">
            <div class="flex items-center justify-between gap-2">
              <span class="fp-num text-sm font-semibold text-[var(--fp-text)]"
                >{$tr("Account #{num}", { num: (t.index ?? 0) + 1 })}</span
              >
              {#if banBadge(t)}
                <StatusBadge
                  status={banBadge(t).label}
                  tone={banBadge(t).tone}
                  pulse={banBadge(t).pulse}
                />
              {:else}
                <StatusBadge
                  status={t.risk_level}
                  tone={riskTone(t.risk_level)}
                  pulse={t.risk_level === "critical"}
                />
              {/if}
            </div>

            {#if t.cooldown_active}
              {@const cd = formatCooldown(t.cooldown_until)}
              {#if cd}
                <div
                  class="fp-inset px-2.5 py-2 text-xs text-[var(--fp-warning)]"
                >
                  {$tr("Cooldown")} —
                  <span class="fp-num">{cd.label}</span>
                  {#if cd.active}
                    {$tr("remaining")}
                  {/if}
                </div>
              {/if}
            {/if}

            <div class="fp-inset px-2.5 py-2 text-xs text-[var(--fp-muted)]">
              {#if t.daily_limit > 0}
                <span class="fp-num text-[var(--fp-text)]"
                  >{t.messages_24h}/{t.daily_limit}</span
                >
                {$tr("msgs today")}
                (<span class="fp-num">{t.usage_pct}%</span>)
              {:else}
                <span class="fp-num text-[var(--fp-text)]"
                  >{t.messages_24h}</span
                >
                {$tr("msgs 24h")}
              {/if}
            </div>

            <div class="flex justify-between text-xs text-[var(--fp-dim)]">
              <span
                >runs <span class="fp-num text-[var(--fp-text)]"
                  >{t.active_runs}</span
                ></span
              >
              <span
                >reqs <span class="fp-num text-[var(--fp-text)]"
                  >{t.requests}</span
                ></span
              >
            </div>
          </div>
        </Card>
      {/each}
    </div>
  {:else}
    <Card>
      <div class="flex items-center gap-2 text-sm text-[var(--fp-muted)]">
        <span class="led led-good" aria-hidden="true"></span>
        {$tr("All tokens healthy — no risk flags.")}
      </div>
    </Card>
  {/if}
</section>
