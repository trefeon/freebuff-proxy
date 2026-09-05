<script>
  import { onMount, onDestroy } from "svelte";
  import { RefreshCw, Save, X } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Button from "../components/Button.svelte";
  import Alert from "../components/Alert.svelte";
  import ConfigFileGuideCard from "../components/ConfigFileGuideCard.svelte";
  import SecurityCard from "../components/SecurityCard.svelte";
  import CommandCenterCard from "../components/CommandCenterCard.svelte";
  import GatewaySettings from "./settings/GatewaySettings.svelte";
  import TrafficSettings from "./settings/TrafficSettings.svelte";
  import { fetchAPI, postForm } from "../api/client.js";
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

  let saving = $state(false);
  let result = $state(null); // { ok, message, restart_only: string[] } — save outcome
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
      // When `effective` is missing entirely (old/mock payloads) nothing is
      // marked unset — the form stays fully editable.
      rawText = baseContent;
      formValues = deriveValues(baseContent);
      changedKeys = new Set();
    } catch (e) {
      if (firstLoad) error = e.message || $tr("Failed to fetch configuration");
    } finally {
      if (firstLoad) loading = false;
    }
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
    fetchData();
    window.addEventListener("beforeunload", handleBeforeUnload);
    window.addEventListener("keydown", handleKeyDown);
  });

  onDestroy(() => {
    window.removeEventListener("beforeunload", handleBeforeUnload);
    window.removeEventListener("keydown", handleKeyDown);
  });
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Settings")}
    description={$tr(
      "Gateway runtime behavior, protection, and model routing. Changes apply live without restart.",
    )}
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
  </PageHeader>

  {#if loading}
    <div class="space-y-6">
      <div class="skeleton skeleton-card"></div>
      <div class="skeleton skeleton-card"></div>
    </div>
  {:else if error}
    <div class="space-y-4">
      <Alert tone="error">{error}</Alert>
      <div>
        <Button variant="secondary" onclick={fetchData}>
          <RefreshCw size={15} />
          {$tr("Retry")}
        </Button>
      </div>
    </div>
  {:else}
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
      </Alert>
    {/if}

    <SecurityCard onSuccess={fetchData} />

    <!-- 2. Gateway & Protection (General - live reload) -->
    <GatewaySettings {formValues} {rawText} onField={setField} />

    <!-- 3. Traffic & Rate Limiting (Pool - live reload) -->
    <TrafficSettings {formValues} {rawText} onField={setField} />

    <!-- 4. Command Center (Lifecycle, updates & rollback) -->
    <CommandCenterCard />
    <!-- 5. Configuration & Deployment Guide -->
    <ConfigFileGuideCard />
  {/if}
</div>
