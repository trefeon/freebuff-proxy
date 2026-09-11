<script>
  import { onMount } from "svelte";
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbBadge from "../../components/DbOverrideBadge.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { SlidersHorizontal } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";
  import { fetchAPI } from "../../api/client.js";
  import { adminApi } from "../../api/paths.js";
  import {
    touchOptions as sharedTouchOptions,
    touchLabel,
  } from "../../utils/touchModels.js";

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
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - DB override reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key DB-overlay save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   */
  let {
    meta = [],
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
  } = $props();

  // Keys owned by the curated section components above (Gateway, Traffic —
  // including the Rotation & Burst block — ModelRouting); Advanced shows
  // everything else the catalog exposes.
  const COVERED = new Set([
    "BRIDGE_ENABLED",
    "BURST_BALANCE_ENABLED",
    "BURST_MAX_TOKENS",
    "BURST_THRESHOLD",
    "BURST_WINDOW",
    "HTTP_READ_TIMEOUT",
    "LOG_LEVEL",
    "MAX_REQUESTS_PER_DAY",
    "MAX_REQUESTS_PER_MINUTE",
    "MODELS_ALLOW",
    "MODEL_ALIASES",
    "MODEL_LOCKS",
    "RATE_LIMIT_FAILOVER",
    "RATE_LIMIT_PER_IP",
    "REASONING_IN_CONTENT",
    "SAFE_MODE",
    "TOKEN_ROTATION",
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
    for (const e of filtered) {
      if (!seen.includes(e.group)) seen.push(e.group);
    }
    return seen;
  });

  // Key search across every remaining catalog key (key + label + description,
  // case-insensitive substring). Groups with no matches vanish with their
  // rows; an empty query renders exactly as before.
  let q = $derived(query.trim().toLowerCase());
  let filtered = $derived(
    !q
      ? rows
      : rows.filter((e) =>
          `${e.key} ${labelFor(e.key)} ${e.description ?? ""}`
            .toLowerCase()
            .includes(q),
        ),
  );
  $effect(() => {
    onMatchCount?.(filtered.length);
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
  // Deep-link focus from cross-page jump links: a link stashes a catalog
  // key in sessionStorage, then routes here.
  let pendingFocusKey = $state("");
  // Served-model catalog for the global MATURITY_TOUCH_MODEL select
  // (shared utils/touchModels.js, priced labels kept). Fetched here so the
  // generic catalog row can render a dropdown instead of a raw text input.
  let modelRows = $state([]);
  onMount(() => {
    try {
      pendingFocusKey = sessionStorage.getItem("fp-settings-focus") ?? "";
      sessionStorage.removeItem("fp-settings-focus");
    } catch {
      /* storage blocked: no deep focus, page still renders */
    }
    (async () => {
      try {
        const res = await fetchAPI(adminApi.models);
        modelRows = res?.models ?? [];
      } catch {
        modelRows = [];
      }
    })();
  });
  // Global touch options: this IS the global value, so no empty
  // fallback option — the current value (or catalog default) is selected.
  // Fail-open to the current value alone while the catalog loads.
  function globalTouchOpts(entry) {
    return sharedTouchOptions(modelRows, val(entry.key, entry));
  }
  $effect(() => {
    const rowCount = rows.length;
    if (!pendingFocusKey || rowCount === 0) return;
    const key = pendingFocusKey;
    pendingFocusKey = "";
    requestAnimationFrame(() => {
      // Scope to the row's own labeled control: the row also hosts the
      // per-key DbOverrideSave button, so a bare "input, button" selector
      // would focus the save button instead of the setting control.
      const el = document.getElementById(`setting-${key}`);
      const control = el?.querySelector(`[aria-label="${CSS.escape(key)}"]`);
      if (!el || !(control instanceof HTMLElement)) return;
      el.scrollIntoView({ block: "center" });
      control.focus({ preventScroll: true });
    });
  });
</script>

{#if !q || filtered.length > 0}
  <SettingsCard
    title={$tr("Advanced")}
    description={$tr(
      "Every remaining tunable with its decided default. Restart-only keys need a container restart; the rest apply on save.",
    )}
  >
    {#snippet icon()}
      <SlidersHorizontal size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", {
            visible: filtered.length,
            total: rows.length,
          })}</span
        >
      {/if}
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
        {#each filtered.filter((e) => e.group === group) as entry, ei (entry.key)}
          {@const isFirst = gi === 0 && ei === 0}
          <!-- Stable anchor for cross-page jump links (fp-settings-focus). -->
          <div id="setting-{entry.key}" class="scroll-mt-24">
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
                {#if sources[entry.key] === "db"}
                  <DbBadge settingKey={entry.key} {onReset} />
                {/if}
              {/snippet}
              {#snippet extra()}
                <DbOverrideSave
                  settingKey={entry.key}
                  value={val(entry.key, entry)}
                  restartOnly={entry.restart_only}
                  {onSaved}
                />
              {/snippet}
              {#if entry.key === "MATURITY_TOUCH_MODEL"}
                <!-- Global touch default: Auto plus priced options
                (shared helper). Saves through the existing row path. -->
                <select
                  class="fp-select"
                  value={val(entry.key, entry)}
                  aria-label={entry.key}
                  title={val(entry.key, entry)}
                  onchange={(e) => onField(entry.key, e.currentTarget.value)}
                >
                  <option value="auto">Auto (cheapest unmetered)</option>
                  {#each globalTouchOpts(entry) as opt (opt.id)}
                    <option value={opt.id}>{touchLabel(opt)}</option>
                  {/each}
                </select>
              {:else if entry.kind === "bool"}
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
                  title={val(entry.key, entry)}
                  aria-label={entry.key}
                  placeholder={entry.default ?? ""}
                  oninput={(e) => onField(entry.key, e.currentTarget.value)}
                />
              {/if}
            </SettingsRow>
          </div>
        {/each}
      {/each}
    {/if}
  </SettingsCard>
{/if}
