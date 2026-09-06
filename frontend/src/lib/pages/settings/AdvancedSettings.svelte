<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import { SlidersHorizontal } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Advanced settings: every catalog key the curated sections do not own.
   * Rows render generically from /admin/api/config/meta (bool → switch,
   * select → dropdown, int → number, text/list → input) with the catalog
   * default and restart-only badges. Secrets never reach this list (the
   * catalog flags them; Tokens/Security own their surfaces).
   *
   * @prop {Array} meta - config catalog entries
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   */
  let { meta = [], formValues, rawText = "", onField } = $props();

  // Keys owned by the curated section components above; Advanced shows
  // everything else the catalog exposes.
  const COVERED = new Set([
    "BRIDGE_ENABLED",
    "HTTP_READ_TIMEOUT",
    "LOG_LEVEL",
    "MAX_REQUESTS_PER_DAY",
    "MAX_REQUESTS_PER_MINUTE",
    "MODELS_ALLOW",
    "MODEL_ALIASES",
    "MODEL_LOCKS",
    "RATE_LIMIT_PER_IP",
    "REASONING_IN_CONTENT",
    "SAFE_MODE",
  ]);

  const GROUP_TITLES = {
    general: "General",
    pool: "Pool",
    quota: "Quota",
    upstream: "Upstream",
    security: "Security",
  };

  let env = $derived(parseEnv(rawText));
  let rows = $derived(
    (meta ?? []).filter(
      (e) => e && e.key && !e.hidden && !e.secret && !COVERED.has(e.key),
    ),
  );
  let groups = $derived.by(() => {
    const seen = [];
    for (const e of rows) {
      if (!seen.includes(e.group)) seen.push(e.group);
    }
    return seen;
  });

  function labelFor(key) {
    return key
      .toLowerCase()
      .split("_")
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(" ");
  }
  function val(key, entry) {
    const v = formValues[key];
    if (v !== undefined && v !== null && v !== "") return String(v);
    return entry?.default ?? "";
  }
  function boolVal(key, entry) {
    const v = val(key, entry);
    if (v === "") return (entry?.default ?? "true") !== "false";
    return v !== "false";
  }
</script>

<SettingsCard
  title={$tr("Advanced")}
  description={$tr(
    "Every remaining tunable with its decided default. Restart-only keys need a container restart; the rest apply on save.",
  )}
>
  {#snippet icon()}
    <SlidersHorizontal size={20} />
  {/snippet}

  {#if rows.length === 0}
    <p class="text-xs text-[var(--fp-dim)]">
      {$tr("No advanced keys exposed by the catalog.")}
    </p>
  {:else}
    {#each groups as group, gi (group)}
      {#if gi > 0}
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pt-4"
        >
          {GROUP_TITLES[group] ?? group}
        </p>
      {/if}
      {#each rows.filter((e) => e.group === group) as entry, ei (entry.key)}
        {@const isFirst = gi === 0 && ei === 0}
        <SettingsRow
          first={isFirst}
          label={labelFor(entry.key)}
          description={entry.description ?? ""}
        >
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >{entry.key}</code
            >
            {#if !env[entry.key]}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
            {#if entry.restart_only}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-warning)]/40 bg-[var(--fp-warning)]/10 text-[var(--fp-warning)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("restart")}</span
              >
            {/if}
          {/snippet}
          {#if entry.kind === "bool"}
            <ToggleSwitch
              checked={boolVal(entry.key, entry)}
              ariaLabel={entry.key}
              onchange={(v) => onField(entry.key, v ? "true" : "false")}
            />
          {:else if entry.kind === "select"}
            <select
              class="fp-select"
              value={val(entry.key, entry)}
              aria-label={entry.key}
              onchange={(e) => onField(entry.key, e.currentTarget.value)}
            >
              {#each entry.enum ?? [] as opt (opt)}
                <option value={opt}>{opt}</option>
              {/each}
            </select>
          {:else if entry.kind === "int"}
            <input
              type="number"
              class="fp-input fp-num"
              value={val(entry.key, entry)}
              aria-label={entry.key}
              placeholder={entry.default ?? ""}
              oninput={(e) => onField(entry.key, e.currentTarget.value)}
            />
          {:else}
            <input
              type="text"
              class="fp-input fp-mono"
              value={val(entry.key, entry)}
              aria-label={entry.key}
              placeholder={entry.default ?? ""}
              oninput={(e) => onField(entry.key, e.currentTarget.value)}
            />
          {/if}
        </SettingsRow>
      {/each}
    {/each}
  {/if}
</SettingsCard>
