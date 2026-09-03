<script>
  import { Activity } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Traffic & Rate Limiting settings card (Pool group).
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   */
  let { formValues, rawText = "", onField } = $props();

  let env = $derived(parseEnv(rawText));
  let rateLimitPerIp = $derived(formValues.RATE_LIMIT_PER_IP ?? "0");
  let rateLimitFailover = $derived(formValues.RATE_LIMIT_FAILOVER !== "false");
</script>

<div
  class="bg-surface border border-border-subtle rounded-[14px] shadow-[var(--shadow-soft)] p-6 page-enter"
>
  <!-- Card Header -->
  <div class="flex items-center gap-3 mb-5">
    <div class="p-2 rounded-lg bg-primary/10 text-primary shrink-0">
      <Activity size={20} />
    </div>
    <div>
      <h2 class="text-base sm:text-lg font-semibold text-[var(--fp-text)]">
        {$tr("Pool")}
      </h2>
      <p class="text-xs sm:text-sm text-text-muted mt-0.5">
        {$tr(
          "Traffic limits and pool failover settings. Changes apply live without restart.",
        )}
      </p>
    </div>
  </div>

  <div class="flex flex-col divide-y divide-border-subtle/50">
    <!-- Client IP Rate Limit -->
    <div
      class="py-4 first:pt-0 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Rate Limit per Client IP")}
          </span>
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >RATE_LIMIT_PER_IP</code
          >
          {#if !env.RATE_LIMIT_PER_IP}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Maximum requests per second allowed from any single client IP address. Prevents rapid agent loops from depleting the pool. Set to 0 for unlimited.",
          )}
        </p>
      </div>
      <div class="shrink-0 w-full sm:w-44">
        <div class="relative">
          <input
            type="number"
            min="0"
            step="1"
            aria-label="RATE_LIMIT_PER_IP"
            class="fp-input w-full !text-xs !py-1.5"
            placeholder="0 (Unlimited)"
            value={rateLimitPerIp === "0" ? "" : rateLimitPerIp}
            oninput={(e) => {
              const val = e.currentTarget.value.trim();
              onField("RATE_LIMIT_PER_IP", val === "" ? "0" : val);
            }}
          />
          <span
            class="absolute right-3 top-1/2 -translate-y-1/2 text-[10px] text-text-muted pointer-events-none"
            >req/s</span
          >
        </div>
      </div>
    </div>

    <!-- 429 Failover -->
    <div
      class="py-4 last:pb-0 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Auto-Failover on 429 Rate Limits")}
          </span>
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >RATE_LIMIT_FAILOVER</code
          >
          {#if !env.RATE_LIMIT_FAILOVER}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "When an upstream account hits a rate limit, the proxy automatically rotates and leases another ready token in your pool so in-flight requests succeed without error.",
          )}
        </p>
      </div>
      <div class="shrink-0 flex items-center gap-2.5">
        <label
          class="inline-flex items-center gap-2 cursor-pointer select-none"
        >
          <input
            type="checkbox"
            aria-label="RATE_LIMIT_FAILOVER"
            checked={rateLimitFailover}
            onchange={(e) =>
              onField("RATE_LIMIT_FAILOVER", e.currentTarget.checked ? "true" : "false")}
            class="h-5 w-5 rounded border-[var(--fp-border-bright)] bg-[var(--fp-input-bg)] text-[var(--fp-accent)] accent-[var(--fp-accent)] cursor-pointer focus:ring-2 focus:ring-[var(--fp-accent)] focus:ring-offset-1 transition-transform active:scale-95"
          />
          <span class="text-xs text-[var(--fp-muted)] min-w-[52px]">
            {rateLimitFailover ? $tr("enabled") : $tr("disabled")}
          </span>
        </label>
      </div>
    </div>
  </div>
</div>
