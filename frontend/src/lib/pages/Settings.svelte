<script>
  import { onMount, onDestroy } from "svelte";
  import { RefreshCw, Save, X } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Button from "../components/Button.svelte";
  import Card from "../components/Card.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import CopyButton from "../components/CopyButton.svelte";
  import ConfigEditor from "./settings/ConfigEditor.svelte";
  import RawEditor from "./settings/RawEditor.svelte";
  import { fetchAPI, postForm } from "../api/client.js";
  import { adminApi, adminActions } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { formatTime } from "../utils/format.js";
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
  let lastSavedTime = $state(null);
  let reveal = $state({}); // secret key → revealed

  // Fallback group order used only when the catalog is absent; the live
  // catalog order (issue #291) is canonical whenever meta is present.
  const FALLBACK_GROUP_ORDER = [
    "general",
    "pool",
    "quota",
    "upstream",
    "security",
  ];

  // Minimalist settings filter & view modes
  let searchQuery = $state("");
  let viewMode = $state("essential"); // 'essential' | 'all'
  let expandedGroups = $state.raw(new Set());

  function isKeyEssential(entry) {
    return Boolean(entry?.essential);
  }
  function toggleGroup(g) {
    // eslint-disable-next-line svelte/prefer-svelte-reactivity -- transient copy, reassigned whole
    const next = new Set(expandedGroups);
    if (next.has(g)) next.delete(g);
    else next.add(g);
    expandedGroups = next;
  }
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

  function onRawInput(value) {
    rawText = value;
    const env = parseEnv(rawText);
    for (const entry of meta) {
      const v = env[entry.key];
      if (v !== undefined) {
        formValues[entry.key] = displayFor(entry, v);
      } else if (changedKeys.has(entry.key)) {
        formValues[entry.key] = "";
      } else {
        formValues[entry.key] = displayFor(
          entry,
          effectiveMap.get(entry.key)?.value ?? entry.default ?? "",
        );
      }
    }
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

  // Live client-side .env parse — same rules as the legacy editor (separators,
  // key syntax, duplicates). The server has no validate-only mode; Save posts
  // the raw content and surfaces the server's decision.
  let validationErrors = $derived.by(() => {
    const errors = [];
    const lines = rawText.split("\n");
    // eslint-disable-next-line svelte/prefer-svelte-reactivity -- transient local dedup set
    const seenKeys = new Set();
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line || line.startsWith("#")) continue;
      const eqIdx = line.indexOf("=");
      if (eqIdx === -1) {
        errors.push(`Line ${i + 1}: Missing '=' separator`);
        continue;
      }
      const key = line.substring(0, eqIdx).trim();
      if (!key) {
        errors.push(`Line ${i + 1}: Empty key name`);
        continue;
      }
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key))
        errors.push(
          `Line ${i + 1}: Invalid key "${key}" (use A-Z, a-z, 0-9, _)`,
        );
      if (seenKeys.has(key))
        errors.push(`Line ${i + 1}: Duplicate key "${key}"`);
      seenKeys.add(key);
    }
    return errors;
  });
  let envValid = $derived(
    validationErrors.length === 0 && rawText.trim().length > 0,
  );
  let keyCount = $derived.by(
    () =>
      rawText
        .split("\n")
        .filter((l) => l.trim() && !l.trim().startsWith("#") && l.includes("="))
        .length,
  );

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

  // Group order from the server-emitted catalog (issue #291): entries appear
  // in catalog order, so groups are ordered by first appearance. Only when the
  // catalog is absent do we fall back to the documented order.
  let groupOrder = $derived.by(() => {
    const seen = [];
    for (const e of meta) {
      if (e.group && !seen.includes(e.group)) seen.push(e.group);
    }
    return seen.length ? seen : FALLBACK_GROUP_ORDER;
  });

  let groups = $derived.by(() => {
    const q = searchQuery.trim().toLowerCase();
    return groupOrder
      .map((g) => {
        let entries = meta.filter((e) => e.group === g && !e.hidden);
        if (q) {
          entries = entries.filter(
            (e) =>
              e.key.toLowerCase().includes(q) ||
              (e.description && e.description.toLowerCase().includes(q)),
          );
        }
        if (!entries.length) return null;

        const essential = entries.filter((e) => isKeyEssential(e));
        const advanced = entries.filter((e) => !isKeyEssential(e));
        const isExpanded =
          viewMode === "all" || Boolean(q) || expandedGroups.has(g);
        const displayed = isExpanded
          ? entries
          : essential.length
            ? essential
            : entries;

        return {
          name: g,
          entries,
          essential,
          advanced,
          isExpanded,
          displayed,
        };
      })
      .filter(Boolean);
  });
  let lastSavedTimeStr = $derived(
    lastSavedTime ? formatTime(lastSavedTime) : "",
  );

  // ---------------------------------------------------------------------------
  // Data
  // ---------------------------------------------------------------------------
  async function fetchData() {
    loading = true;
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
      error = e.message || $tr("Failed to fetch configuration");
    } finally {
      loading = false;
    }
  }

  function validateConfig() {
    if (saving) return;
    if (!rawText.trim()) {
      result = {
        ok: false,
        message: $tr("Configuration is empty — nothing to save."),
        restart_only: [],
      };
      return;
    }
    if (validationErrors.length === 0) {
      result = {
        ok: true,
        message: $tr("Configuration is valid — {count} key(s) parsed.", {
          count: keyCount,
        }),
        restart_only: [],
      };
    } else {
      const shown = validationErrors.slice(0, 5).join(" · ");
      const more =
        validationErrors.length > 5
          ? ` (+${validationErrors.length - 5} more)`
          : "";
      result = {
        ok: false,
        message: $tr("Configuration invalid ({count}): {detail}", {
          count: validationErrors.length,
          detail: `${shown}${more}`,
        }),
        restart_only: [],
      };
    }
  }

  async function saveConfig(e, opts = {}) {
    e?.preventDefault();
    if (saving || !dirty) return;
    if (
      opts.confirm !== false &&
      !window.confirm(
        $tr("Save the .env file and reload the proxy with these changes?"),
      )
    ) {
      return;
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
        lastSavedTime = new Date();
        await fetchData();
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

  function toggleReveal(key) {
    reveal[key] = !reveal[key];
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Settings")}
    description={$tr(
      "Runtime configuration — Save writes the .env file and reloads the running proxy.",
    )}
  >
    {#snippet actions()}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Refresh")}
      </Button>
      <Button
        variant="primary"
        onclick={saveConfig}
        disabled={saving || !dirty}
        loading={saving}
      >
        <Save size={15} />
        {$tr("Save")}
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
              "{count} key(s) differ from the saved .env. Save to persist, or Discard to reset.",
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

    <ConfigEditor
      {meta}
      {groups}
      {formValues}
      {rawText}
      {reveal}
      {searchQuery}
      {viewMode}
      onSearch={(v) => {
        searchQuery = v;
      }}
      onViewMode={(m) => {
        viewMode = m;
      }}
      onField={setField}
      onToggleReveal={toggleReveal}
      onToggleGroup={toggleGroup}
      onResetFilters={() => {
        searchQuery = "";
        viewMode = "essential";
      }}
    />

    <!-- Current values: read-only effective config, secrets masked -->
    <Card
      title={$tr("Current Values")}
      description={$tr(
        "Read-only view of the running configuration. Secret values are masked.",
      )}
      pad="none"
    >
      <div class="overflow-auto max-h-96 min-h-0" style="contain: paint;">
        {#if data?.effective?.length}
          <table class="fp-table">
            <caption class="sr-only"
              >{$tr("Effective configuration — key and value")}</caption
            >
            <thead>
              <tr>
                <th scope="col">{$tr("Key")}</th>
                <th scope="col">{$tr("Value")}</th>
              </tr>
            </thead>
            <tbody>
              {#each data.effective as kv (kv.key)}
                <tr>
                  <td>
                    <div class="flex items-center gap-2 min-w-0">
                      <span
                        class="fp-num text-[11px] font-semibold text-[var(--fp-text)] truncate"
                        >{kv.key}</span
                      >
                      {#if kv.secret}
                        <span
                          class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-error)]/40 bg-[var(--fp-error)]/15 text-[#FCA5A5] font-semibold uppercase tracking-wider shrink-0"
                          >{$tr("secret")}</span
                        >
                      {/if}
                    </div>
                  </td>
                  <td>
                    <div class="flex items-center gap-2 min-w-0">
                      <span
                        class="fp-num text-[11px] text-[var(--fp-muted)] truncate max-w-[180px]"
                      >
                        {kv.secret ? "••••••••" : kv.value || "—"}
                      </span>
                      {#if kv.secret}
                        <span
                          class="text-[10px] text-[var(--fp-dim)] uppercase tracking-wider"
                          >{$tr("redacted")}</span
                        >
                      {:else if kv.value}
                        <span class="shrink-0">
                          <CopyButton text={kv.value} label={$tr("copy")} />
                        </span>
                      {/if}
                    </div>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        {:else}
          <div class="p-5">
            <EmptyState
              title={$tr("No effective configuration")}
              description={$tr("Start the proxy to populate this view.")}
            >
              {#snippet action()}
                <Button variant="secondary" onclick={fetchData}>
                  <RefreshCw size={15} />
                  {$tr("Refresh")}
                </Button>
              {/snippet}
            </EmptyState>
          </div>
        {/if}
      </div>
    </Card>

    <RawEditor
      {rawText}
      {onRawInput}
      {validationErrors}
      {envValid}
      {keyCount}
      {lastSavedTimeStr}
      onValidate={validateConfig}
      {dirty}
      {changedKeysCount}
      {data}
      {saving}
    />
  {/if}
</div>
