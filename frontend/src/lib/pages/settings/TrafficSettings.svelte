<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import { Activity } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Traffic & Rate Limiting settings card (Pool group).
   * Built using the SettingsCard and SettingsRow template components.
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

<SettingsCard
  title={$tr("Pool")}
  description={$tr(
    "Traffic limits and pool failover settings. Changes apply live without restart.",
  )}
>
  {#snippet icon()}
    <Activity size={20} />
  {/snippet}

  <!-- Client IP Rate Limit -->
  <SettingsRow
    first
    label={$tr("Rate Limit per Client IP")}
    description={$tr(
      "Maximum requests per second allowed from any single client IP address. Prevents rapid agent loops from depleting the pool. Set to 0 for unlimited.",
    )}
  >
    {#snippet badge()}
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
    {/snippet}

    <div class="w-full sm:w-44">
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
  </SettingsRow>

  <!-- 429 Failover -->
  <SettingsRow
    last
    label={$tr("Auto-Failover on 429 Rate Limits")}
    description={$tr(
      "When an upstream account hits a rate limit, the proxy automatically rotates and leases another ready token in your pool so in-flight requests succeed without error.",
    )}
  >
    {#snippet badge()}
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
    {/snippet}

    <div class="flex items-center gap-2.5">
      <ToggleSwitch
        checked={rateLimitFailover}
        ariaLabel="RATE_LIMIT_FAILOVER"
        onchange={(v) => onField("RATE_LIMIT_FAILOVER", v ? "true" : "false")}
      />
    </div>
  </SettingsRow>
</SettingsCard>
