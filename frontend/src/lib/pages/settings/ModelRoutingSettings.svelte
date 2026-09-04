<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import { Cpu } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Model Routing & Aliases settings card (Upstream group).
   * Built using the SettingsCard and SettingsRow template components.
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

<SettingsCard
  title={$tr("Upstream")}
  description={$tr(
    "Model aliases, access filtering, and reasoning format. Changes apply live without restart.",
  )}
>
  {#snippet icon()}
    <Cpu size={20} />
  {/snippet}

  <!-- Model Aliases -->
  <SettingsRow
    first
    align="start"
    label={$tr("Model Aliases")}
    description={$tr(
      "Map short or custom model names requested by AI clients to actual upstream model IDs. Comma-separated alias:model pairs.",
    )}
  >
    {#snippet badge()}
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
    {/snippet}

    <div class="w-full md:w-80">
      <input
        type="text"
        aria-label="MODEL_ALIASES"
        class="fp-input w-full !text-xs !py-1.5"
        placeholder="e.g. gpt-4o:openai/gpt-5.6-luna, sonnet:anthropic/claude-3.5-sonnet"
        value={modelAliases}
        oninput={(e) => onField("MODEL_ALIASES", e.currentTarget.value)}
      />
    </div>
  </SettingsRow>

  <!-- Allowed Models Filter -->
  <SettingsRow
    align="start"
    label={$tr("Allowed Models Filter")}
    description={$tr(
      "Only serve these models (comma-separated). Any request for an unlisted model is rejected with 404. Leave blank to allow all models.",
    )}
  >
    {#snippet badge()}
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
    {/snippet}

    <div class="w-full md:w-80">
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
  </SettingsRow>

  <!-- Fold Reasoning into Content -->
  <SettingsRow
    label={$tr("Fold Reasoning into Message Content")}
    description={$tr(
      "Wraps internal reasoning inside <think>...</think> tags in the message body for older AI clients that do not support dedicated reasoning stream blocks.",
    )}
  >
    {#snippet badge()}
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
    {/snippet}

    <div class="flex items-center gap-2.5">
      <ToggleSwitch
        checked={reasoningInContent}
        ariaLabel="REASONING_IN_CONTENT"
        onchange={(v) => onField("REASONING_IN_CONTENT", v ? "true" : "")}
      />
    </div>
  </SettingsRow>

  <!-- Model Locks -->
  <SettingsRow
    last
    align="start"
    label={$tr("Model Token Locks")}
    description={$tr(
      "Optionally reserve specific pool token slots for dedicated models (e.g. 0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash). Unpinned slots serve any model.",
    )}
  >
    {#snippet badge()}
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
    {/snippet}

    <div class="w-full md:w-80">
      <input
        type="text"
        aria-label="MODEL_LOCKS"
        class="fp-input w-full !text-xs !py-1.5"
        placeholder="e.g. 0:z-ai/glm-5.2;1:deepseek/deepseek-v4-flash"
        value={modelLocks}
        oninput={(e) => onField("MODEL_LOCKS", e.currentTarget.value)}
      />
    </div>
  </SettingsRow>
</SettingsCard>
