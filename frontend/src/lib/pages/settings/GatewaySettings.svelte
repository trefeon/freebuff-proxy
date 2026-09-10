<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbBadge from "../../components/DbOverrideBadge.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
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
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - DB override reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key DB-overlay save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   */
  let {
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
  } = $props();

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
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  const SAFE_MODE_LABEL = "Anti-Ban Safe Mode";
  const SAFE_MODE_DESC =
    "Enforces 200ms request jitter and 30-minute idle session rotation to match official CLI behavior and avoid upstream account flagging.";
  const LOG_LEVEL_LABEL = "Server Log Level";
  const LOG_LEVEL_DESC =
    "Controls the detail level of server console output and the live Logs page.";
  const HTTP_TIMEOUT_LABEL = "HTTP Read Timeout";
  const HTTP_TIMEOUT_DESC =
    "How long the server waits for slow clients uploading request bodies (far-away harnesses, images). Takes effect after a container restart; 0 disables the timeout.";
  const BRIDGE_LABEL = "Allow Client-Provided Tokens (Bridge Mode)";
  const BRIDGE_DESC =
    "Enforces hybrid access: client apps can pass their personal FreeBuff account tokens via the Authorization header, saving your server's shared pool quota.";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showSafeMode = $derived(
    hit("SAFE_MODE", SAFE_MODE_LABEL, SAFE_MODE_DESC),
  );
  let showLogLevel = $derived(
    hit("LOG_LEVEL", LOG_LEVEL_LABEL, LOG_LEVEL_DESC),
  );
  let showHttpTimeout = $derived(
    hit("HTTP_READ_TIMEOUT", HTTP_TIMEOUT_LABEL, HTTP_TIMEOUT_DESC),
  );
  let showBridge = $derived(hit("BRIDGE_ENABLED", BRIDGE_LABEL, BRIDGE_DESC));
  let visibleKeys = $derived(
    [
      showSafeMode ? "SAFE_MODE" : null,
      showLogLevel ? "LOG_LEVEL" : null,
      showHttpTimeout ? "HTTP_READ_TIMEOUT" : null,
      showBridge ? "BRIDGE_ENABLED" : null,
    ].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("General")}
    description={$tr(
      "Gateway runtime behavior and account protection. Most keys apply live without restart; restart-marked keys apply after a container restart.",
    )}
  >
    {#snippet icon()}
      <ShieldCheck size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 4 })}</span
        >
      {/if}
    {/snippet}

    <!-- Safe Mode -->
    {#if showSafeMode}
      <SettingsRow
        first={visibleKeys[0] === "SAFE_MODE"}
        label={$tr(SAFE_MODE_LABEL)}
        description={$tr(SAFE_MODE_DESC)}
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
          {#if sources.SAFE_MODE === "db"}
            <DbBadge settingKey="SAFE_MODE" {onReset} />
          {/if}
        {/snippet}

        {#snippet extra()}
          <DbOverrideSave
            settingKey="SAFE_MODE"
            value={formValues.SAFE_MODE ?? "true"}
            {onSaved}
          />
        {/snippet}

        <div class="flex items-center gap-2.5">
          <ToggleSwitch
            checked={safeMode}
            ariaLabel="SAFE_MODE"
            onchange={(v) => onField("SAFE_MODE", v ? "true" : "false")}
          />
        </div>
      </SettingsRow>
    {/if}

    <!-- Log Level -->
    {#if showLogLevel}
      <SettingsRow
        first={visibleKeys[0] === "LOG_LEVEL"}
        last={visibleKeys[visibleKeys.length - 1] === "LOG_LEVEL"}
        label={$tr(LOG_LEVEL_LABEL)}
        description={$tr(LOG_LEVEL_DESC)}
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
          {#if sources.LOG_LEVEL === "db"}
            <DbBadge settingKey="LOG_LEVEL" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave settingKey="LOG_LEVEL" value={logLevel} {onSaved} />
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
            <option value="debug" class="bg-[#141a25] text-[#e9edf3]"
              >debug</option
            >
            <option value="warn" class="bg-[#141a25] text-[#e9edf3]"
              >warn</option
            >
            <option value="error" class="bg-[#141a25] text-[#e9edf3]"
              >error</option
            >
            <option value="trace" class="bg-[#141a25] text-[#e9edf3]"
              >trace</option
            >
          </select>
        </div>
      </SettingsRow>
    {/if}

    <!-- HTTP Read Timeout -->
    {#if showHttpTimeout}
      <SettingsRow
        first={visibleKeys[0] === "HTTP_READ_TIMEOUT"}
        last={visibleKeys[visibleKeys.length - 1] === "HTTP_READ_TIMEOUT"}
        label={$tr(HTTP_TIMEOUT_LABEL)}
        description={$tr(HTTP_TIMEOUT_DESC)}
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
          {#if sources.HTTP_READ_TIMEOUT === "db"}
            <DbBadge settingKey="HTTP_READ_TIMEOUT" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="HTTP_READ_TIMEOUT"
            value={httpReadTimeout}
            restartOnly
            {onSaved}
          />
        {/snippet}

        <div class="w-full sm:w-48">
          <select
            aria-label="HTTP_READ_TIMEOUT"
            class="fp-select w-full !text-xs !h-9 !px-3 bg-[var(--fp-input-bg)] text-[var(--fp-text)] border border-[var(--fp-border-bright)] rounded-[var(--fp-radius-sm)] focus:border-[var(--fp-accent)] focus:outline-none font-medium cursor-pointer"
            value={normalizeTimeout(httpReadTimeout)}
            onchange={(e) =>
              onField("HTTP_READ_TIMEOUT", e.currentTarget.value)}
          >
            {#each TIMEOUT_OPTIONS as opt (opt.value)}
              <option value={opt.value} class="bg-[#141a25] text-[#e9edf3]">
                {opt.label}
              </option>
            {/each}
            {#if !TIMEOUT_OPTIONS.some((opt) => opt.value === normalizeTimeout(httpReadTimeout))}
              <option
                value={httpReadTimeout}
                class="bg-[#141a25] text-[#e9edf3]"
              >
                {httpReadTimeout} (custom)
              </option>
            {/if}
          </select>
        </div>
      </SettingsRow>
    {/if}

    <!-- Bridge Mode -->
    {#if showBridge}
      <SettingsRow
        last={visibleKeys[visibleKeys.length - 1] === "BRIDGE_ENABLED"}
        label={$tr(BRIDGE_LABEL)}
        description={$tr(BRIDGE_DESC)}
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
          {#if sources.BRIDGE_ENABLED === "db"}
            <DbBadge settingKey="BRIDGE_ENABLED" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="BRIDGE_ENABLED"
            value={formValues.BRIDGE_ENABLED ?? "true"}
            {onSaved}
          />
        {/snippet}

        <div class="flex items-center gap-2.5">
          <ToggleSwitch
            checked={bridgeEnabled}
            ariaLabel="BRIDGE_ENABLED"
            onchange={(v) => onField("BRIDGE_ENABLED", v ? "true" : "false")}
          />
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
