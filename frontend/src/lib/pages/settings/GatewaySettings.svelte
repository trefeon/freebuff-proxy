<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import { ShieldCheck } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Gateway & Protection settings card (General group).
   * Built using the SettingsCard and SettingsRow template components.
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   */
  let { formValues, rawText = "", onField } = $props();

  let env = $derived(parseEnv(rawText));
  let safeMode = $derived(formValues.SAFE_MODE !== "false");
  let logLevel = $derived(formValues.LOG_LEVEL || "info");
  let httpReadTimeout = $derived(formValues.HTTP_READ_TIMEOUT || "60s");
  let bridgeEnabled = $derived(formValues.BRIDGE_ENABLED !== "false");

  const TIMEOUT_OPTIONS = [
    { value: "60s", label: "60s (1m - default)" },
    { value: "90s", label: "90s (1.5m)" },
    { value: "120s", label: "120s (2m)" },
    { value: "180s", label: "180s (3m)" },
    { value: "300s", label: "300s (5m)" },
    { value: "0", label: "0 (disabled)" },
  ];

  function normalizeTimeout(val) {
    if (!val) return "60s";
    const s = String(val).trim().toLowerCase();
    if (s === "0" || s === "0s") return "0";
    if (s === "60s" || s === "1m" || s === "1m0s" || s === "60") return "60s";
    if (s === "90s" || s === "1m30s" || s === "1.5m" || s === "90")
      return "90s";
    if (s === "120s" || s === "2m" || s === "2m0s" || s === "120")
      return "120s";
    if (s === "180s" || s === "3m" || s === "3m0s" || s === "180")
      return "180s";
    if (s === "240s" || s === "4m" || s === "4m0s" || s === "240")
      return "240s";
    if (s === "300s" || s === "5m" || s === "5m0s" || s === "300")
      return "300s";
    return val;
  }
</script>

<SettingsCard
  title={$tr("General")}
  description={$tr(
    "Gateway runtime behavior and account protection. Changes apply live without restart.",
  )}
>
  {#snippet icon()}
    <ShieldCheck size={20} />
  {/snippet}

  <!-- Safe Mode -->
  <SettingsRow
    first
    label={$tr("Anti-Ban Safe Mode")}
    description={$tr(
      "Enforces 200ms request jitter and 30-minute idle session rotation to match official CLI behavior and avoid upstream account flagging.",
    )}
  >
    {#snippet badge()}
      <code
        class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
        >SAFE_MODE</code
      >
      {#if !env.SAFE_MODE}
        <span
          class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
          >{$tr("default")}</span
        >
      {/if}
    {/snippet}

    <div class="flex items-center gap-2.5">
      <ToggleSwitch
        checked={safeMode}
        ariaLabel="SAFE_MODE"
        onchange={(v) => onField("SAFE_MODE", v ? "true" : "false")}
      />
    </div>
  </SettingsRow>

  <!-- Log Level -->
  <SettingsRow
    label={$tr("Server Log Level")}
    description={$tr(
      "Controls the detail level of server console output and the live Logs page.",
    )}
  >
    {#snippet badge()}
      <code
        class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
        >LOG_LEVEL</code
      >
      {#if !env.LOG_LEVEL}
        <span
          class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
          >{$tr("default")}</span
        >
      {/if}
    {/snippet}

    <div class="w-full sm:w-48">
      <select
        aria-label="LOG_LEVEL"
        class="fp-input w-full !text-xs !h-9 !pl-3 !pr-8 bg-[var(--fp-input-bg)] text-[var(--fp-text)] border border-[var(--fp-border-bright)] rounded-[var(--fp-radius-sm)] focus:border-[var(--fp-accent)] focus:outline-none"
        value={logLevel}
        onchange={(e) => onField("LOG_LEVEL", e.currentTarget.value)}
      >
        <option value="info" class="bg-[#141a25] text-[#e9edf3]"
          >info (recommended)</option
        >
        <option value="debug" class="bg-[#141a25] text-[#e9edf3]">debug</option>
        <option value="warn" class="bg-[#141a25] text-[#e9edf3]">warn</option>
        <option value="error" class="bg-[#141a25] text-[#e9edf3]">error</option>
        <option value="trace" class="bg-[#141a25] text-[#e9edf3]">trace</option>
      </select>
    </div>
  </SettingsRow>

  <!-- HTTP Read Timeout -->
  <SettingsRow
    label={$tr("HTTP Read Timeout")}
    description={$tr(
      "How long the server waits for slow clients uploading request bodies (far-away harnesses, images). Takes effect after a container restart; 0 disables the timeout.",
    )}
  >
    {#snippet badge()}
      <code
        class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
        >HTTP_READ_TIMEOUT</code
      >
      {#if !env.HTTP_READ_TIMEOUT}
        <span
          class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
          >{$tr("default")}</span
        >
      {/if}
      <span
        class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-warning)]/50 bg-[var(--fp-warning)]/10 text-[var(--fp-warning)] font-semibold uppercase tracking-wider shrink-0"
        >{$tr("restart")}</span
      >
    {/snippet}

    <div class="w-full sm:w-48">
      <select
        aria-label="HTTP_READ_TIMEOUT"
        class="fp-select w-full !text-xs !h-9 !px-3 bg-[var(--fp-input-bg)] text-[var(--fp-text)] border border-[var(--fp-border-bright)] rounded-[var(--fp-radius-sm)] focus:border-[var(--fp-accent)] focus:outline-none font-medium cursor-pointer"
        value={normalizeTimeout(httpReadTimeout)}
        onchange={(e) => onField("HTTP_READ_TIMEOUT", e.currentTarget.value)}
      >
        {#each TIMEOUT_OPTIONS as opt (opt.value)}
          <option value={opt.value} class="bg-[#141a25] text-[#e9edf3]">
            {opt.label}
          </option>
        {/each}
        {#if !TIMEOUT_OPTIONS.some((opt) => opt.value === normalizeTimeout(httpReadTimeout))}
          <option value={httpReadTimeout} class="bg-[#141a25] text-[#e9edf3]">
            {httpReadTimeout} (custom)
          </option>
        {/if}
      </select>
    </div>
  </SettingsRow>

  <!-- Bridge Mode -->
  <SettingsRow
    last
    label={$tr("Allow Client-Provided Tokens (Bridge Mode)")}
    description={$tr(
      "Enforces hybrid access: client apps can pass their personal FreeBuff account tokens via the Authorization header, saving your server's shared pool quota.",
    )}
  >
    {#snippet badge()}
      <code
        class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
        >BRIDGE_ENABLED</code
      >
      {#if !env.BRIDGE_ENABLED}
        <span
          class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
          >{$tr("default")}</span
        >
      {/if}
    {/snippet}

    <div class="flex items-center gap-2.5">
      <ToggleSwitch
        checked={bridgeEnabled}
        ariaLabel="BRIDGE_ENABLED"
        onchange={(v) => onField("BRIDGE_ENABLED", v ? "true" : "false")}
      />
    </div>
  </SettingsRow>
</SettingsCard>
