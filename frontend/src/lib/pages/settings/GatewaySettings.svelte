<script>
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import { ShieldCheck } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Gateway & Protection settings card (General group).
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
</script>

<div
  class="bg-surface border border-border-subtle rounded-[14px] shadow-[var(--shadow-soft)] p-6 page-enter"
>
  <!-- Card Header -->
  <div class="flex items-center gap-3 mb-5">
    <div class="p-2 rounded-lg bg-primary/10 text-primary shrink-0">
      <ShieldCheck size={20} />
    </div>
    <div>
      <h2 class="text-base sm:text-lg font-semibold text-[var(--fp-text)]">
        {$tr("General")}
      </h2>
      <p class="text-xs sm:text-sm text-text-muted mt-0.5">
        {$tr(
          "Gateway runtime behavior and account protection. Changes apply live without restart.",
        )}
      </p>
    </div>
  </div>

  <div class="flex flex-col divide-y divide-border-subtle/50">
    <!-- Safe Mode -->
    <div
      class="py-4 first:pt-0 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Anti-Ban Safe Mode")}
          </span>
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
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Enforces 200ms request jitter and 30-minute idle session rotation to match official CLI behavior and avoid upstream account flagging.",
          )}
        </p>
      </div>
      <div class="shrink-0 flex items-center gap-2.5">
        <ToggleSwitch
          checked={safeMode}
          ariaLabel="SAFE_MODE"
          onchange={(v) => onField("SAFE_MODE", v ? "true" : "false")}
        />
      </div>
    </div>

    <!-- Log Level -->
    <div
      class="py-4 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Server Log Level")}
          </span>
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
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Controls the detail level of server console output and the live Logs page.",
          )}
        </p>
      </div>
      <div class="shrink-0 w-full sm:w-48">
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
          <option value="warn" class="bg-[#141a25] text-[#e9edf3]">warn</option>
          <option value="error" class="bg-[#141a25] text-[#e9edf3]"
            >error</option
          >
          <option value="trace" class="bg-[#141a25] text-[#e9edf3]"
            >trace</option
          >
        </select>
      </div>
    </div>

    <!-- HTTP Read Timeout -->
    <div
      class="py-4 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("HTTP Read Timeout")}
          </span>
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
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "How long the server waits for slow clients uploading request bodies (far-away harnesses, images). Takes effect after a container restart; 0 disables the timeout.",
          )}
        </p>
      </div>
      <div class="shrink-0 w-full sm:w-48">
        <input
          type="text"
          inputmode="text"
          aria-label="HTTP_READ_TIMEOUT"
          class="fp-input w-full !text-xs !h-9 !px-3 bg-[var(--fp-input-bg)] text-[var(--fp-text)] border border-[var(--fp-border-bright)] rounded-[var(--fp-radius-sm)] focus:border-[var(--fp-accent)] focus:outline-none font-mono"
          value={httpReadTimeout}
          placeholder="60s"
          onchange={(e) =>
            onField("HTTP_READ_TIMEOUT", e.currentTarget.value.trim())}
        />
      </div>
    </div>

    <!-- Bridge Mode -->
    <div
      class="py-4 last:pb-0 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Allow Client-Provided Tokens (Bridge Mode)")}
          </span>
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
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Enables hybrid access: client apps can pass their personal FreeBuff account tokens via the Authorization header, saving your server's shared pool quota.",
          )}
        </p>
      </div>
      <div class="shrink-0 flex items-center gap-2.5">
        <ToggleSwitch
          checked={bridgeEnabled}
          ariaLabel="BRIDGE_ENABLED"
          onchange={(v) => onField("BRIDGE_ENABLED", v ? "true" : "false")}
        />
      </div>
    </div>
  </div>
</div>
