<script>
  import { onMount, onDestroy } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { RefreshCw, Save, X } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import Button from "../components/Button.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import SecurityCard from "../components/SecurityCard.svelte";
  import CommandCenterCard from "../components/CommandCenterCard.svelte";
  import GatewaySettings from "./settings/GatewaySettings.svelte";
  import TrafficSettings from "./settings/TrafficSettings.svelte";
  import ModelRoutingSettings from "./settings/ModelRoutingSettings.svelte";
  import AdvancedSettings from "./settings/AdvancedSettings.svelte";
  import { fetchAPI, postForm, deleteAPI } from "../api/client.js";
  import { adminApi, adminActions } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { confirmAction } from "../stores/confirm.js";
  import { refreshTokens } from "../stores/tokens.js";
  import { parseEnv, setEnvValue as setEnvLine } from "../utils/env.js";

  // ---------------------------------------------------------------------------
  // State
  // ---------------------------------------------------------------------------
  let meta = $state([]); // key catalog from /admin/api/config/meta
  let data = $state(null); // /admin/api/config payload
  let loading = $state(true);
  let error = $state("");

  let rawText = $state(""); // canonical .env document (single source of truth)
  let baseContent = $state(""); // last-saved server env_content
  let formValues = $state({}); // meta key → display value
  // eslint-disable svelte/prefer-svelte-reactivity -- codebase idiom: $state.raw + full reassignment (changedKeys = next etc.), never in-place mutation of the wrapped collection
  let changedKeys = $state.raw(new Set()); // form-touched keys — only these are serialized into the document
  let effectiveMap = $state.raw(new Map()); // key → { value, secret }
  let settingSources = $state({}); // key → env|db|file|default (ADR-0019)
  // Live-only read-only flag from GET /admin/api/settings (ADR-0019): when
  // the store is nil the gateway serves file/env/default with degraded:true
  // and overlay writes 503 — banner it, keep the .env form usable.
  let settingsDegraded = $state(false);
  let saving = $state(false);
  let result = $state(null); // { ok, message, restart_only: string[] } — save outcome
  // Key search across all catalog sections (70 keys).
  let filterQuery = $state("");
  let searching = $derived(filterQuery.trim().length > 0);
  // Per-section visible-row counts (bound from the section components, -1
  // until mounted) for the global search empty state.
  let gatewayMatches = $state(-1);
  let trafficMatches = $state(-1);
  let routingMatches = $state(-1);
  let advancedMatches = $state(-1);
  let allEmpty = $derived(
    searching &&
      gatewayMatches === 0 &&
      trafficMatches === 0 &&
      routingMatches === 0 &&
      advancedMatches === 0,
  );
  // ---------------------------------------------------------------------------
  // .env parsing / merging — shared contract in ../utils/env.js (issue #234):
  // line-replace, comments preserved for untouched lines.
  // ---------------------------------------------------------------------------

  function isTruthy(v) {
    return v === "true" || v === "1" || v === "on" || v === "yes";
  }

  function serializeFor(entry, val) {
    if (entry.kind === "bool") return isTruthy(val) ? "true" : "false";
    if (entry.kind === "list") {
      return String(val ?? "")
        .split(",")
        .map((s) => s.trim())
        .join(",");
    }
    return String(val ?? "");
  }

  function displayFor(entry, raw) {
    if (entry.kind === "bool") return isTruthy(raw) ? "true" : "false";
    return raw;
  }

  // Form values derived from a .env document (+ effective/default fallbacks).
  function deriveValues(content) {
    const env = parseEnv(content);
    const vals = {};
    for (const entry of meta) {
      let raw = env[entry.key];
      if (raw === undefined) {
        raw = effectiveMap.get(entry.key)?.value ?? entry.default ?? "";
      }
      vals[entry.key] = displayFor(entry, raw);
    }
    return vals;
  }

  function rebuildRaw() {
    let out = rawText;
    for (const entry of meta) {
      if (!changedKeys.has(entry.key)) continue;
      out = setEnvLine(
        out,
        entry.key,
        serializeFor(entry, formValues[entry.key]),
      );
    }
    rawText = out;
  }

  function setField(key, value) {
    formValues[key] = value;
    // eslint-disable-next-line svelte/prefer-svelte-reactivity -- transient copy, reassigned whole
    const next = new Set(changedKeys);
    next.add(key);
    changedKeys = next;
    rebuildRaw();
  }

  function discard() {
    rawText = baseContent;
    formValues = deriveValues(baseContent);
    changedKeys = new Set();
    result = null;
  }

  // ---------------------------------------------------------------------------
  // Derived
  // ---------------------------------------------------------------------------
  let dirty = $derived(rawText !== baseContent);

  let changedKeysCount = $derived.by(() => {
    const a = parseEnv(baseContent);
    const b = parseEnv(rawText);
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    let n = 0;
    for (const k of keys) {
      if ((a[k] ?? "") !== (b[k] ?? "")) n++;
    }
    return n;
  });
  // Dirty key names for the restart summary: the same .env-document diff
  // the Save flow persists, partitioned by the catalog restart_only flag.
  let dirtyKeys = $derived.by(() => {
    const a = parseEnv(baseContent);
    const b = parseEnv(rawText);
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    return [...keys].filter((k) => (a[k] ?? "") !== (b[k] ?? ""));
  });
  function needsRestart(key) {
    return (meta ?? []).some((e) => e.key === key && e.restart_only);
  }
  let restartKeys = $derived(dirtyKeys.filter(needsRestart));
  let liveKeys = $derived(dirtyKeys.filter((k) => !needsRestart(k)));

  // How many keys the DB overlay currently wins (ADR-0019 badge count).
  let dbCount = $derived(
    Object.values(settingSources).filter((s) => s === "db").length,
  );

  // ---------------------------------------------------------------------------
  // Data
  // ---------------------------------------------------------------------------
  async function fetchData() {
    // Silent refetch once the form is mounted: flipping `loading` would
    // unmount the cards (wiping e.g. the password success alert after
    // SecurityCard's onSuccess refresh), and a failed background refresh
    // must not wipe a usable form — it keeps stale data instead.
    const firstLoad = data == null;
    if (firstLoad) loading = true;
    error = "";
    try {
      const [metaRes, cfgRes] = await Promise.all([
        fetchAPI(adminApi.configMeta),
        fetchAPI(adminApi.config),
      ]);
      meta = Array.isArray(metaRes) ? metaRes : (metaRes?.entries ?? []);
      data = cfgRes;
      baseContent = cfgRes.env_content || "";
      // eslint-disable-next-line svelte/prefer-svelte-reactivity -- rebuilt per load, reassigned whole
      effectiveMap = new Map();
      for (const kv of cfgRes.effective ?? []) {
        effectiveMap.set(kv.key, kv);
      }
      // Keys absent from the effective config keep an informational "not set" badge;
      // the controls stay editable so the operator can set them from the form.
      rawText = baseContent;
      formValues = deriveValues(baseContent);
      changedKeys = new Set();
      // Source tiers (env|db|file|default) hydrate fetch-only from the new
      // settings endpoint; a failure hides the DB badges, never the form.
      // degraded:true means live-only (nil store): overlay writes 503, so
      // banner the read-only state while the .env form stays usable.
      try {
        const setRes = await fetchAPI(adminApi.settings);
        const next = {};
        for (const e of setRes.settings ?? []) next[e.key] = e.source;
        settingSources = next;
        settingsDegraded = setRes.degraded === true;
      } catch {
        // Keep the last-known sources: a failed background refresh must not
        // wipe the DB badges (first load simply keeps the empty default).
      }
    } catch (e) {
      if (firstLoad) error = e.message || $tr("Failed to fetch configuration");
    } finally {
      if (firstLoad) loading = false;
    }
  }

  // Drop one DB overlay row (ADR-0019): the key falls back to file/env and
  // the whole form refetches, so badges, values, and the .env document
  // agree again. Failures surface in the save-result alert, never silent.
  async function resetSetting(key) {
    try {
      const res = await deleteAPI(adminApi.settingsDelete(key));
      result = {
        ok: true,
        message: res?.message || $tr("DB override removed."),
        restart_only: [],
      };
      await fetchData();
      refreshTokens();
    } catch (e) {
      result = {
        ok: false,
        message: e.message || $tr("Failed to reset override"),
        restart_only: [],
      };
    }
  }
  // Per-key DB-overlay save (row-level save buttons): refetch so badges,
  // effective values, and the .env document agree again — the same teardown
  // as resetSetting, minus its result alert (the row shows its own inline
  // status, including the restart note for restart-only keys).
  async function overlaySaved() {
    await fetchData();
    refreshTokens();
  }

  async function saveConfig(e, opts = {}) {
    e?.preventDefault();
    if (saving || !dirty) return;
    if (opts.confirm !== false) {
      const ok = await confirmAction({
        title: $tr("Save Configuration"),
        message: $tr(
          "Save the .env file and reload the proxy with these changes?",
        ),
        confirmText: $tr("Save & Reload"),
        tone: "warn",
      });
      if (!ok) return;
    }
    saving = true;
    result = null;

    try {
      const res = await postForm(adminActions.configSave, { content: rawText });
      const json = await res.json();
      result = {
        ok: res.ok && json.ok,
        message:
          json.message ||
          (res.ok
            ? $tr("Configuration saved and reloaded.")
            : $tr("Save failed")),
        restart_only: Array.isArray(json.restart_only) ? json.restart_only : [],
      };
      if (result.ok) {
        await fetchData();
        refreshTokens();
        if (typeof window !== "undefined") {
          window.dispatchEvent(new CustomEvent("fp-config-saved"));
        }
      } else {
        // The server rejected the write and rolled the .env file back;
        // mirror that in the document and the form.
        rawText = baseContent;
        formValues = deriveValues(baseContent);
        changedKeys = new Set();
      }
    } catch (e) {
      result = {
        ok: false,
        message: e.message || $tr("Network error saving configuration"),
        restart_only: [],
      };
    } finally {
      saving = false;
    }
  }

  function handleBeforeUnload(e) {
    if (dirty) {
      e.preventDefault();
      e.returnValue = "";
    }
  }

  function handleKeyDown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === "s") {
      e.preventDefault();
      if (dirty && !saving) {
        saveConfig(null, { confirm: false });
      }
    }
  }

  onMount(() => {
    recordPageVisit("settings");
    fetchData();
    window.addEventListener("beforeunload", handleBeforeUnload);
    window.addEventListener("keydown", handleKeyDown);
  });

  onDestroy(() => {
    window.removeEventListener("beforeunload", handleBeforeUnload);
    window.removeEventListener("keydown", handleKeyDown);
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / settings.conf"
  title={$tr("Settings")}
  description={$tr(
    "Gateway runtime behavior, protection, and model routing. Live-applying keys take effect on save without restart; restart-marked keys need a container restart.",
  )}
  {loading}
  {error}
  onRetry={fetchData}
>
  {#snippet actions()}
    {#if dirty}
      <Button variant="ghost" onclick={discard} disabled={saving}>
        <X size={15} />
        {$tr("Discard")}
      </Button>
    {:else}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Refresh")}
      </Button>
    {/if}
    <Button
      variant="primary"
      onclick={saveConfig}
      disabled={saving || !dirty}
      loading={saving}
    >
      <Save size={15} />
      {$tr("Save Changes")}
    </Button>
  {/snippet}

  {#if result}
    <Alert
      tone={result.ok
        ? result.restart_only.length
          ? "warning"
          : "success"
        : "error"}
    >
      <div class="flex items-start justify-between gap-3">
        <div>
          {result.message}
          {#if result.ok && result.restart_only.length}
            <p class="mt-1 text-xs">
              {$tr("Applies after restart: {keys}", {
                keys: result.restart_only.join(", "),
              })}
            </p>
          {/if}
        </div>
        <button
          type="button"
          onclick={() => (result = null)}
          class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] transition-colors shrink-0"
          aria-label={$tr("Dismiss alert")}
        >
          <X size={14} />
        </button>
      </div>
    </Alert>
  {/if}

  {#if dirty}
    <Alert tone="warning" title={$tr("Unsaved changes")}>
      <div
        class="flex flex-col sm:flex-row sm:items-center justify-between gap-2"
      >
        <span
          >{$tr(
            "{count} setting(s) modified. Click Save Changes to apply them immediately.",
            { count: changedKeysCount },
          )}</span
        >
        <div class="flex items-center gap-2 shrink-0">
          <Button variant="secondary" size="sm" onclick={discard}>
            <X size={14} />
            {$tr("Discard")}
          </Button>
        </div>
      </div>
      {#if restartKeys.length > 0 || liveKeys.length > 0}
        <div class="flex flex-col gap-0.5 mt-2 text-xs">
          {#if restartKeys.length > 0}
            <span
              >{$tr("Needs restart ({n}): {keys}", {
                n: restartKeys.length,
                keys: restartKeys.join(", "),
              })}</span
            >
          {/if}
          {#if liveKeys.length > 0}
            <span class="text-[var(--fp-dim)]"
              >{$tr("Live-applying ({n}): {keys}", {
                n: liveKeys.length,
                keys: liveKeys.join(", "),
              })}</span
            >
          {/if}
        </div>
      {/if}
    </Alert>
  {/if}

  {#if settingsDegraded}
    <Alert tone="warning" title={$tr("DB overlay unavailable")}>
      {$tr(
        "The settings store is offline — the dashboard runs live-only. Overlay saves and resets will fail; .env saves below still apply.",
      )}
    </Alert>
  {/if}

  {#if dbCount > 0}
    <Alert tone="info" title={$tr("DB overrides active")}>
      {#if dbCount === 1}
        {$tr(
          "1 setting comes from the DB overlay and wins over the .env file below until reset per row.",
        )}
      {:else}
        {$tr(
          "{count} settings come from the DB overlay and win over the .env file below until reset per row.",
          { count: dbCount },
        )}
      {/if}
    </Alert>
  {/if}

  <!-- Key search across all 70 catalog keys -->
  <div class="flex flex-col gap-1.5">
    <label
      for="settings-search"
      class="text-xs font-semibold text-[var(--fp-muted)]"
      >{$tr("Search settings")}</label
    >
    <input
      id="settings-search"
      type="search"
      autocomplete="off"
      placeholder={$tr("Search 70 keys…")}
      bind:value={filterQuery}
      class="fp-input w-full sm:max-w-md"
    />
  </div>

  <SecurityCard onSuccess={fetchData} />

  <!-- 2. Gateway & Protection (General - live reload) -->
  <GatewaySettings
    {formValues}
    {rawText}
    onField={setField}
    sources={settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (gatewayMatches = n)}
  />

  <!-- 3. Traffic & Rate Limiting (Pool - live reload) -->
  <TrafficSettings
    {formValues}
    {rawText}
    onField={setField}
    sources={settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (trafficMatches = n)}
  />

  <!-- 4. Model Routing & Aliases (Upstream - live reload) -->
  <ModelRoutingSettings
    {formValues}
    {rawText}
    onField={setField}
    sources={settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (routingMatches = n)}
  />

  <!-- 5. Advanced (every remaining catalog key with its default) -->
  <AdvancedSettings
    {meta}
    {formValues}
    {rawText}
    onField={setField}
    sources={settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (advancedMatches = n)}
  />

  {#if allEmpty}
    <EmptyState
      title={$tr('No settings match "{q}"', { q: filterQuery.trim() })}
      description={$tr(
        "Try a key name like TOKEN_ROTATION, or clear the search to see all sections.",
      )}
    >
      {#snippet action()}
        <Button
          variant="secondary"
          size="sm"
          onclick={() => (filterQuery = "")}
        >
          {$tr("Clear search")}
        </Button>
      {/snippet}
    </EmptyState>
  {/if}

  <!-- 6. Command Center (Lifecycle, updates & rollback) -->
  <CommandCenterCard />
</PageShell>
