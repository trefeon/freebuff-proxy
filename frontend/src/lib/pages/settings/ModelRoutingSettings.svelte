<script>
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import { Cpu } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Model Routing & Aliases settings card (Upstream group).
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   */
  let { formValues, rawText = "", onField } = $props();

  let env = $derived(parseEnv(rawText));
  let modelAliases = $derived(formValues.MODEL_ALIASES ?? "");
  let modelsAllow = $derived(formValues.MODELS_ALLOW ?? "");
  let reasoningInContent = $derived(
    Boolean(
      formValues.REASONING_IN_CONTENT &&
      formValues.REASONING_IN_CONTENT !== "false" &&
      formValues.REASONING_IN_CONTENT !== "off",
    ),
  );
  let modelLocks = $derived(formValues.MODEL_LOCKS ?? "");
</script>

<div
  class="bg-surface border border-border-subtle rounded-[14px] shadow-[var(--shadow-soft)] p-6 page-enter"
>
  <!-- Card Header -->
  <div class="flex items-center gap-3 mb-5">
    <div class="p-2 rounded-lg bg-primary/10 text-primary shrink-0">
      <Cpu size={20} />
    </div>
    <div>
      <h2 class="text-base sm:text-lg font-semibold text-[var(--fp-text)]">
        {$tr("Upstream")}
      </h2>
      <p class="text-xs sm:text-sm text-text-muted mt-0.5">
        {$tr(
          "Model aliases, allowlists, and thinking format. Changes apply live without restart.",
        )}
      </p>
    </div>
  </div>

  <div class="flex flex-col divide-y divide-border-subtle/50">
    <!-- Model Aliases -->
    <div
      class="py-4 first:pt-0 flex flex-col md:flex-row md:items-start justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Model Aliases")}
          </span>
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >MODEL_ALIASES</code
          >
          {#if !env.MODEL_ALIASES}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Map short or custom model names requested by AI clients to actual upstream model IDs. Comma-separated alias:model pairs.",
          )}
        </p>
      </div>
      <div class="w-full md:w-80 shrink-0">
        <input
          type="text"
          aria-label="MODEL_ALIASES"
          class="fp-input w-full !text-xs !py-1.5"
          placeholder="e.g. gpt-4o:openai/gpt-5.6-luna, sonnet:anthropic/claude-3.5-sonnet"
          value={modelAliases}
          oninput={(e) => onField("MODEL_ALIASES", e.currentTarget.value)}
        />
      </div>
    </div>

    <!-- Allowed Models Filter -->
    <div
      class="py-4 flex flex-col md:flex-row md:items-start justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Allowed Models Filter")}
          </span>
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >MODELS_ALLOW</code
          >
          {#if !env.MODELS_ALLOW}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Only serve these models (comma-separated). Any request for an unlisted model is rejected with 404. Leave blank to allow all models.",
          )}
        </p>
      </div>
      <div class="w-full md:w-80 shrink-0">
        <input
          type="text"
          aria-label="MODELS_ALLOW"
          class="fp-input w-full !text-xs !py-1.5"
          placeholder={$tr(
            "Leave blank for all models (or e.g. openai/gpt-5.6-luna)",
          )}
          value={modelsAllow}
          oninput={(e) => onField("MODELS_ALLOW", e.currentTarget.value)}
        />
      </div>
    </div>

    <!-- Fold Reasoning into Content -->
    <div
      class="py-4 flex flex-col sm:flex-row sm:items-center justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Fold Reasoning into Message Content")}
          </span>
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >REASONING_IN_CONTENT</code
          >
          {#if !env.REASONING_IN_CONTENT}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Wraps internal reasoning inside <think>...</think> tags in the message body for older AI clients that do not support dedicated reasoning stream blocks.",
          )}
        </p>
      </div>
      <div class="shrink-0 flex items-center gap-2.5">
        <ToggleSwitch
          checked={reasoningInContent}
          ariaLabel="REASONING_IN_CONTENT"
          onchange={(v) => onField("REASONING_IN_CONTENT", v ? "true" : "")}
        />
      </div>
    </div>

    <!-- Model Locks -->
    <div
      class="py-4 last:pb-0 flex flex-col md:flex-row md:items-start justify-between gap-4"
    >
      <div class="flex-1 min-w-0">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
            {$tr("Model Token Locks")}
          </span>
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >MODEL_LOCKS</code
          >
          {#if !env.MODEL_LOCKS}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        </div>
        <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
          {$tr(
            "Optionally reserve specific pool token slots for dedicated models (e.g. 0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash). Unpinned slots serve any model.",
          )}
        </p>
      </div>
      <div class="w-full md:w-80 shrink-0">
        <input
          type="text"
          aria-label="MODEL_LOCKS"
          class="fp-input w-full !text-xs !py-1.5"
          placeholder="e.g. 0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash"
          value={modelLocks}
          oninput={(e) => onField("MODEL_LOCKS", e.currentTarget.value)}
        />
      </div>
    </div>
  </div>
</div>
