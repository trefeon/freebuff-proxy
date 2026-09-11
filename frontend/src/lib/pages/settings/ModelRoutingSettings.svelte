<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbBadge from "../../components/DbOverrideBadge.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { Cpu } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Model Routing & Aliases settings card (Upstream group).
   * Built using the SettingsCard and SettingsRow template components.
   * All four keys apply live on reload (none is restart-only).
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
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  const ALIASES_LABEL = "Model Aliases";
  const ALIASES_DESC =
    "Map short or custom model names requested by AI clients to actual upstream model IDs. Comma-separated alias:model pairs.";
  const ALLOW_LABEL = "Allowed Models Filter";
  const ALLOW_DESC =
    "Only serve these models (comma-separated). Any request for an unlisted model is rejected with 404. Leave blank to allow all models.";
  const REASON_LABEL = "Fold Reasoning into Message Content";
  const REASON_DESC =
    "Wraps internal reasoning inside <think>...</think> tags in the message body for older AI clients that do not support dedicated reasoning stream blocks.";
  const LOCKS_LABEL = "Model Token Locks";
  const LOCKS_DESC =
    "Optionally reserve specific pool token slots for dedicated models (e.g. 0:z-ai/glm-5.2;1:upstage/solar-pro4,mimo/mimo-v2.5). Unpinned slots serve any model.";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showAliases = $derived(hit("MODEL_ALIASES", ALIASES_LABEL, ALIASES_DESC));
  let showAllow = $derived(hit("MODELS_ALLOW", ALLOW_LABEL, ALLOW_DESC));
  let showReason = $derived(
    hit("REASONING_IN_CONTENT", REASON_LABEL, REASON_DESC),
  );
  let showLocks = $derived(hit("MODEL_LOCKS", LOCKS_LABEL, LOCKS_DESC));
  let visibleKeys = $derived(
    [
      showAliases ? "MODEL_ALIASES" : null,
      showAllow ? "MODELS_ALLOW" : null,
      showReason ? "REASONING_IN_CONTENT" : null,
      showLocks ? "MODEL_LOCKS" : null,
    ].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Upstream")}
    description={$tr(
      "Model aliases, access filtering, and reasoning format. Changes apply live without restart.",
    )}
  >
    {#snippet icon()}
      <Cpu size={20} />
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

    <!-- Model Aliases -->
    {#if showAliases}
      <SettingsRow
        first={visibleKeys[0] === "MODEL_ALIASES"}
        last={visibleKeys[visibleKeys.length - 1] === "MODEL_ALIASES"}
        align="start"
        label={$tr(ALIASES_LABEL)}
        description={$tr(ALIASES_DESC)}
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
          {#if sources.MODEL_ALIASES === "db"}
            <DbBadge settingKey="MODEL_ALIASES" {onReset} />
          {/if}
        {/snippet}

        {#snippet extra()}
          <DbOverrideSave
            settingKey="MODEL_ALIASES"
            value={modelAliases}
            {onSaved}
          />
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
    {/if}

    <!-- Allowed Models Filter -->
    {#if showAllow}
      <SettingsRow
        first={visibleKeys[0] === "MODELS_ALLOW"}
        last={visibleKeys[visibleKeys.length - 1] === "MODELS_ALLOW"}
        align="start"
        label={$tr(ALLOW_LABEL)}
        description={$tr(ALLOW_DESC)}
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
          {#if sources.MODELS_ALLOW === "db"}
            <DbBadge settingKey="MODELS_ALLOW" {onReset} />
          {/if}
        {/snippet}

        {#snippet extra()}
          <DbOverrideSave
            settingKey="MODELS_ALLOW"
            value={modelsAllow}
            {onSaved}
          />
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
    {/if}

    <!-- Fold Reasoning into Content -->
    {#if showReason}
      <SettingsRow
        first={visibleKeys[0] === "REASONING_IN_CONTENT"}
        last={visibleKeys[visibleKeys.length - 1] === "REASONING_IN_CONTENT"}
        label={$tr(REASON_LABEL)}
        description={$tr(REASON_DESC)}
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
          {#if sources.REASONING_IN_CONTENT === "db"}
            <DbBadge settingKey="REASONING_IN_CONTENT" {onReset} />
          {/if}
        {/snippet}

        {#snippet extra()}
          <DbOverrideSave
            settingKey="REASONING_IN_CONTENT"
            value={formValues.REASONING_IN_CONTENT ?? ""}
            {onSaved}
          />
        {/snippet}

        <div class="flex items-center gap-2.5">
          <ToggleSwitch
            checked={reasoningInContent}
            ariaLabel="REASONING_IN_CONTENT"
            onchange={(v) => onField("REASONING_IN_CONTENT", v ? "true" : "")}
          />
        </div>
      </SettingsRow>
    {/if}

    <!-- Model Locks -->
    {#if showLocks}
      <SettingsRow
        first={visibleKeys[0] === "MODEL_LOCKS"}
        last={visibleKeys[visibleKeys.length - 1] === "MODEL_LOCKS"}
        align="start"
        label={$tr(LOCKS_LABEL)}
        description={$tr(LOCKS_DESC)}
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
          {#if sources.MODEL_LOCKS === "db"}
            <DbBadge settingKey="MODEL_LOCKS" {onReset} />
          {/if}
        {/snippet}

        {#snippet extra()}
          <DbOverrideSave
            settingKey="MODEL_LOCKS"
            value={modelLocks}
            {onSaved}
          />
        {/snippet}

        <div class="w-full md:w-80">
          <input
            type="text"
            aria-label="MODEL_LOCKS"
            class="fp-input w-full !text-xs !py-1.5"
            placeholder="e.g. 0:z-ai/glm-5.2;1:upstage/solar-pro4,mimo/mimo-v2.5"
            value={modelLocks}
            oninput={(e) => onField("MODEL_LOCKS", e.currentTarget.value)}
          />
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
