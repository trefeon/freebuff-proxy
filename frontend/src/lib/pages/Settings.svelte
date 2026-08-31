<script>
  import { onMount, onDestroy } from 'svelte';
  import { RefreshCw, Save, X, Eye, EyeOff, Search, Sparkles, SlidersHorizontal, ChevronDown, ChevronUp } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Button from '../components/Button.svelte';
  import Card from '../components/Card.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import { fetchAPI, postForm } from '../api/client.js';
  import { adminApi, adminActions } from '../api/paths.js';
  import { tr } from '../i18n.js';
  import { formatTime } from '../utils/format.js';

  // ---------------------------------------------------------------------------
  // State
  // ---------------------------------------------------------------------------
  let meta = $state([]);        // key catalog from /admin/api/config/meta
  let data = $state(null);      // /admin/api/config payload
  let loading = $state(true);
  let error = $state('');

  let rawText = $state('');        // canonical .env document (single source of truth)
  let baseContent = $state('');    // last-saved server env_content
  let formValues = $state({});     // meta key → display value
  let changedKeys = $state.raw(new Set()); // form-touched keys — only these are serialized into the document
  let effectiveMap = $state.raw(new Map()); // key → { value, secret }
  let unsetKeys = $state.raw(new Set());    // meta keys absent from the effective config

  let saving = $state(false);
  let result = $state(null); // { ok, message, restart_only: string[] } — save outcome
  let lastSavedTime = $state(null);
  let reveal = $state({});   // secret key → revealed

  const GROUP_ORDER = ['general', 'pool', 'quota', 'upstream', 'security'];

  // Minimalist settings filter & view modes
  let searchQuery = $state('');
  let viewMode = $state('essential'); // 'essential' | 'all'
  let expandedGroups = $state.raw(new Set());

  function isKeyEssential(entry) {
    return Boolean(entry?.essential);
  }
  function toggleGroup(g) {
    const next = new Set(expandedGroups);
    if (next.has(g)) next.delete(g);
    else next.add(g);
    expandedGroups = next;
  }
  // ---------------------------------------------------------------------------
  // .env parsing / merging (line-replace, comments preserved for untouched lines)
  // ---------------------------------------------------------------------------
  function parseEnv(content) {
    const map = {};
    for (const line of (content || '').split('\n')) {
      const t = line.trim();
      if (!t || t.startsWith('#')) continue;
      const eq = t.indexOf('=');
      if (eq === -1) continue;
      map[t.slice(0, eq).trim()] = t.slice(eq + 1).trim();
    }
    return map;
  }

  function escapeRegex(s) {
    return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }

  function setEnvLine(content, key, value) {
    const re = new RegExp(`^\\s*${escapeRegex(key)}=.*$`, 'm');
    const line = `${key}=${value}`;
    if (re.test(content)) return content.replace(re, line);
    if (!content) return line;
    return content.endsWith('\n') ? content + line : content + '\n' + line;
  }

  function isTruthy(v) {
    return v === 'true' || v === '1' || v === 'on' || v === 'yes';
  }

  function serializeFor(entry, val) {
    if (entry.kind === 'bool') return isTruthy(val) ? 'true' : 'false';
    if (entry.kind === 'list') {
      return String(val ?? '')
        .split(',')
        .map((s) => s.trim())
        .join(',');
    }
    return String(val ?? '');
  }

  function displayFor(entry, raw) {
    if (entry.kind === 'bool') return isTruthy(raw) ? 'true' : 'false';
    return raw;
  }

  // Form values derived from a .env document (+ effective/default fallbacks).
  function deriveValues(content) {
    const env = parseEnv(content);
    const vals = {};
    for (const entry of meta) {
      let raw = env[entry.key];
      if (raw === undefined) {
        raw = effectiveMap.get(entry.key)?.value ?? entry.default ?? '';
      }
      vals[entry.key] = displayFor(entry, raw);
    }
    return vals;
  }

  function rebuildRaw() {
    let out = rawText;
    for (const entry of meta) {
      if (!changedKeys.has(entry.key)) continue;
      out = setEnvLine(out, entry.key, serializeFor(entry, formValues[entry.key]));
    }
    rawText = out;
  }

  function onRawInput(e) {
    rawText = e.currentTarget.value;
    const env = parseEnv(rawText);
    for (const entry of meta) {
      const v = env[entry.key];
      if (v !== undefined) {
        formValues[entry.key] = displayFor(entry, v);
      } else if (changedKeys.has(entry.key)) {
        formValues[entry.key] = '';
      } else {
        formValues[entry.key] = displayFor(entry, effectiveMap.get(entry.key)?.value ?? entry.default ?? '');
      }
    }
  }

  function setField(key, value) {
    formValues[key] = value;
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
      if ((a[k] ?? '') !== (b[k] ?? '')) n++;
    }
    return n;
  });

  let groups = $derived.by(() => {
    const q = searchQuery.trim().toLowerCase();
    return GROUP_ORDER.map((g) => {
      let entries = meta.filter((e) => e.group === g && !e.hidden);
      if (q) {
        entries = entries.filter(
          (e) =>
            e.key.toLowerCase().includes(q) ||
            (e.description && e.description.toLowerCase().includes(q))
        );
      }
      if (!entries.length) return null;

      const essential = entries.filter((e) => isKeyEssential(e));
      const advanced = entries.filter((e) => !isKeyEssential(e));
      const isExpanded = viewMode === 'all' || Boolean(q) || expandedGroups.has(g);
      const displayed = isExpanded ? entries : (essential.length ? essential : entries);

      return {
        name: g,
        entries,
        essential,
        advanced,
        isExpanded,
        displayed,
      };
    }).filter(Boolean);
  });
  let lastSavedTimeStr = $derived(lastSavedTime ? formatTime(lastSavedTime) : '');

  // Live client-side .env parse — same rules as the legacy editor (separators,
  // key syntax, duplicates). The server has no validate-only mode; Save posts
  // the raw content and surfaces the server's decision.
  let validationErrors = $derived.by(() => {
    const errors = [];
    const lines = rawText.split('\n');
    const seenKeys = new Set();
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line || line.startsWith('#')) continue;
      const eqIdx = line.indexOf('=');
      if (eqIdx === -1) { errors.push(`Line ${i + 1}: Missing '=' separator`); continue; }
      const key = line.substring(0, eqIdx).trim();
      if (!key) { errors.push(`Line ${i + 1}: Empty key name`); continue; }
      if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) errors.push(`Line ${i + 1}: Invalid key "${key}" (use A-Z, a-z, 0-9, _)`);
      if (seenKeys.has(key)) errors.push(`Line ${i + 1}: Duplicate key "${key}"`);
      seenKeys.add(key);
    }
    return errors;
  });
  let envValid = $derived(validationErrors.length === 0 && rawText.trim().length > 0);
  let keyCount = $derived.by(() => rawText.split('\n').filter((l) => l.trim() && !l.trim().startsWith('#') && l.includes('=')).length);

  // ---------------------------------------------------------------------------
  // Data
  // ---------------------------------------------------------------------------
  async function fetchData() {
    loading = true;
    error = '';
    try {
      const [metaRes, cfgRes] = await Promise.all([
        fetchAPI(adminApi.configMeta),
        fetchAPI(adminApi.config),
      ]);
      meta = Array.isArray(metaRes) ? metaRes : (metaRes?.entries ?? []);
      data = cfgRes;
      baseContent = cfgRes.env_content || '';
      effectiveMap = new Map();
      for (const kv of cfgRes.effective ?? []) {
        effectiveMap.set(kv.key, kv);
      }
      // Keys absent from the effective config keep an informational "not set" badge;
	// the controls stay editable so the operator can set them from the form.
      // When `effective` is missing entirely (old/mock payloads) nothing is
      // marked unset — the form stays fully editable.
      if (Array.isArray(cfgRes.effective) && cfgRes.effective.length > 0) {
        unsetKeys = new Set(meta.filter((e) => !effectiveMap.has(e.key)).map((e) => e.key));
      } else {
        unsetKeys = new Set();
      }

      rawText = baseContent;
      formValues = deriveValues(baseContent);
      changedKeys = new Set();
    } catch (e) {
      error = e.message || $tr('Failed to fetch configuration');
    } finally {
      loading = false;
    }
  }

  function validateConfig() {
    if (saving) return;
    if (!rawText.trim()) {
      result = { ok: false, message: $tr('Configuration is empty — nothing to save.'), restart_only: [] };
      return;
    }
    if (validationErrors.length === 0) {
      result = { ok: true, message: $tr('Configuration is valid — {count} key(s) parsed.', { count: keyCount }), restart_only: [] };
    } else {
      const shown = validationErrors.slice(0, 5).join(' · ');
      const more = validationErrors.length > 5 ? ` (+${validationErrors.length - 5} more)` : '';
      result = { ok: false, message: $tr('Configuration invalid ({count}): {detail}', { count: validationErrors.length, detail: `${shown}${more}` }), restart_only: [] };
    }
  }

  async function saveConfig(e, opts = {}) {
    e?.preventDefault();
    if (saving || !dirty) return;
    if (opts.confirm !== false && !window.confirm($tr('Save the .env file and reload the proxy with these changes?'))) {
      return;
    }
    saving = true;
    result = null;

    try {
      const res = await postForm(adminActions.configSave, { content: rawText });
      const json = await res.json();
      result = {
        ok: res.ok && json.ok,
        message: json.message || (res.ok ? $tr('Configuration saved and reloaded.') : $tr('Save failed')),
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
      result = { ok: false, message: e.message || $tr('Network error saving configuration'), restart_only: [] };
    } finally {
      saving = false;
    }
  }

  function handleBeforeUnload(e) {
    if (dirty) {
      e.preventDefault();
      e.returnValue = '';
    }
  }

  function handleKeyDown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') {
      e.preventDefault();
      if (dirty && !saving) {
        saveConfig(null, { confirm: false });
      }
    }
  }

  onMount(() => {
    fetchData();
    window.addEventListener('beforeunload', handleBeforeUnload);
    window.addEventListener('keydown', handleKeyDown);
  });

  onDestroy(() => {
    window.removeEventListener('beforeunload', handleBeforeUnload);
    window.removeEventListener('keydown', handleKeyDown);
  });

  const GROUP_LABELS = {
    general: 'General',
    pool: 'Pool',
    quota: 'Quota',
    upstream: 'Upstream',
    security: 'Security',
  };
  const GROUP_DESCRIPTIONS = {
    general: 'Runtime and logging knobs.',
    pool: 'Token pool, sessions and bridge mode.',
    quota: 'Quota and rate limits.',
    upstream: 'Upstream gateway and model routing.',
    security: 'Access control and secrets.',
  };

  function groupLabel(name) {
    return GROUP_LABELS[name] || name;
  }

  function groupDescription(name) {
    return GROUP_DESCRIPTIONS[name] || '';
  }

  function toggleReveal(key) {
    reveal[key] = !reveal[key];
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title={$tr('Settings')} description={$tr('Runtime configuration — Save writes the .env file and reloads the running proxy.')}>
    {#snippet actions()}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr('Refresh')}
      </Button>
      <Button
        variant="primary"
        onclick={saveConfig}
        disabled={saving || !dirty}
        loading={saving}
      >
        <Save size={15} />
        {$tr('Save')}
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
          {$tr('Retry')}
        </Button>
      </div>
    </div>
  {:else}
    {#if result}
      <Alert tone={result.ok ? (result.restart_only.length ? 'warning' : 'success') : 'error'}>
        <div class="flex items-start justify-between gap-3">
          <div>
            {result.message}
            {#if result.ok && result.restart_only.length}
              <p class="mt-1 text-xs">{$tr('Applies after restart: {keys}', { keys: result.restart_only.join(', ') })}</p>
            {/if}
          </div>
          <button
            type="button"
            onclick={() => result = null}
            class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] transition-colors shrink-0"
            aria-label={$tr('Dismiss alert')}
          >
            <X size={14} />
          </button>
        </div>
      </Alert>
    {/if}

    {#if dirty}
      <Alert tone="warning" title={$tr('Unsaved changes')}>
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
          <span>{$tr('{count} key(s) differ from the saved .env. Save to persist, or Discard to reset.', { count: changedKeysCount })}</span>
          <div class="flex items-center gap-2 shrink-0">
            <Button variant="secondary" size="sm" onclick={discard}>
              <X size={14} />
              {$tr('Discard')}
            </Button>
          </div>
        </div>
      </Alert>
    {/if}

    <!-- Filter and Search Toolbar -->
    <div class="flex flex-col sm:flex-row gap-3 items-stretch sm:items-center justify-between pb-1">
      <!-- Mode Toggle -->
      <div class="flex items-center gap-1 p-1 rounded-lg bg-[var(--fp-surface)] border border-[var(--fp-border)]">
        <button
          type="button"
          class="px-3 py-1 rounded-md text-xs font-medium transition-colors flex items-center gap-1.5 {viewMode === 'essential' ? 'bg-[var(--fp-accent)] text-[var(--fp-accent-fg,white)] font-semibold shadow-sm' : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
          onclick={() => viewMode = 'essential'}
        >
          <Sparkles size={13} />
          <span>{$tr('Essential ({count})', { count: meta.filter(e => !e.hidden && isKeyEssential(e)).length })}</span>
        </button>
        <button
          type="button"
          class="px-3 py-1 rounded-md text-xs font-medium transition-colors flex items-center gap-1.5 {viewMode === 'all' ? 'bg-[var(--fp-accent)] text-[var(--fp-accent-fg,white)] font-semibold shadow-sm' : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
          onclick={() => viewMode = 'all'}
        >
          <SlidersHorizontal size={13} />
          <span>{$tr('All Settings ({count})', { count: meta.filter(e => !e.hidden).length })}</span>
        </button>
      </div>

      <!-- Search Box -->
      <div class="relative min-w-[220px] sm:w-72">
        <input
          type="text"
          bind:value={searchQuery}
          placeholder={$tr('Search settings by key or description…')}
          class="fp-input !pl-8 !pr-8 !py-1.5 !text-xs !w-full"
        />
        <Search size={14} class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] pointer-events-none" />
        {#if searchQuery}
          <button
            type="button"
            onclick={() => searchQuery = ''}
            class="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-0.5"
            aria-label={$tr('Clear search')}
          >
            <X size={13} />
          </button>
        {/if}
      </div>
    </div>

    <!-- Key catalog groups: one Card per group, one typed control per key -->
    <div class="space-y-6">
      {#each groups as group}
        <Card title={groupLabel(group.name)} description={groupDescription(group.name)}>
          <div class="divide-y divide-[var(--fp-border)]">
            {#each group.displayed as entry (entry.key)}
              <div class="py-3.5 first:pt-0 last:pb-0 grid grid-cols-1 md:grid-cols-12 gap-3 md:gap-6 items-start">
                <div class="md:col-span-6 min-w-0">
                  <div class="flex flex-wrap items-center gap-2">
                    <span id={`setting-${entry.key}`} class="fp-num text-[12px] font-semibold text-[var(--fp-text)]">{entry.key}</span>
                    {#if entry.restart_only}
                      <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-warning)]/40 bg-[var(--fp-warning)]/15 text-[#FCD34D] font-semibold uppercase tracking-wider shrink-0">{$tr('restart')}</span>
                    {/if}
                    {#if entry.secret}
                      <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-error)]/40 bg-[var(--fp-error)]/15 text-[#FCA5A5] font-semibold uppercase tracking-wider shrink-0">{$tr('secret')}</span>
                    {/if}
                    {#if !parseEnv(rawText)[entry.key]}
                      <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0">{$tr('default')}</span>
                    {/if}
                  </div>
                  {#if entry.description}
                    <p class="mt-1 text-[11px] text-[var(--fp-dim)] leading-relaxed">{entry.description}</p>
                  {/if}
                </div>

                <div class="md:col-span-6 min-w-0">
                  {#if entry.kind === 'bool'}
                    <label class="inline-flex items-center gap-2 cursor-pointer select-none">
                      <input
                        type="checkbox"
                        class="fp-checkbox"
                        checked={formValues[entry.key] === 'true'}
                        onchange={(e) => setField(entry.key, e.currentTarget.checked ? 'true' : 'false')}
                        aria-label={entry.key}
                      />
                      <span class="text-xs text-[var(--fp-muted)]">
                        {formValues[entry.key] === 'true' ? $tr('enabled') : $tr('disabled')}
                      </span>
                    </label>
                  {:else if entry.kind === 'select'}
                    <select
                      class="fp-input text-[13px] py-2"
                      value={formValues[entry.key]}
                      onchange={(e) => setField(entry.key, e.currentTarget.value)}
                      aria-label={entry.key}
                    >
                      {#if entry.enum && !entry.enum.includes(formValues[entry.key])}
                        <option value={formValues[entry.key]}>{formValues[entry.key]}</option>
                      {/if}
                      {#each entry.enum ?? [] as opt}
                        <option value={opt}>{opt}</option>
                      {/each}
                    </select>
                  {:else if entry.kind === 'int'}
                    <input
                      type="number"
                      step="1"
                      class="fp-input fp-num text-[13px] py-2"
                      value={formValues[entry.key]}
                      oninput={(e) => setField(entry.key, e.currentTarget.value)}
                      aria-label={entry.key}
                    />
                  {:else if entry.kind === 'secret'}
                    <div class="flex items-center gap-2">
                      <input
                        type={reveal[entry.key] ? 'text' : 'password'}
                        class="fp-input fp-mono text-[12px] py-2"
                        value={formValues[entry.key]}
                        oninput={(e) => setField(entry.key, e.currentTarget.value)}
                        aria-label={entry.key}
                        autocomplete="off"
                      />
                      <button
                        type="button"
                        class="fp-btn fp-btn-ghost fp-btn-sm shrink-0"
                        onclick={() => toggleReveal(entry.key)}
                        aria-label={reveal[entry.key] ? $tr('Hide {key}', { key: entry.key }) : $tr('Reveal {key}', { key: entry.key })}
                        title={reveal[entry.key] ? $tr('Hide') : $tr('Reveal')}
                      >
                        {#if reveal[entry.key]}
                          <EyeOff size={15} />
                        {:else}
                          <Eye size={15} />
                        {/if}
                      </button>
                    </div>
                  {:else if entry.kind === 'list'}
                    <div>
                      <textarea
                        class="fp-input fp-mono text-[12px] py-2 min-h-[56px]"
                        rows="2"
                        value={formValues[entry.key]}
                        oninput={(e) => setField(entry.key, e.currentTarget.value)}
                        aria-label={entry.key}
                        spellcheck="false"
                        placeholder="val1, val2, val3"
                      ></textarea>
                      <p class="mt-1 text-[10px] text-[var(--fp-dim)]">{$tr('Comma-separated values.')}</p>
                    </div>
                  {:else}
                    <input
                      type="text"
                      class="fp-input fp-mono text-[12px] py-2"
                      value={formValues[entry.key]}
                      oninput={(e) => setField(entry.key, e.currentTarget.value)}
                      aria-label={entry.key}
                      spellcheck="false"
                      placeholder={entry.default || $tr('unset — default applies')}
                    />
                  {/if}
                </div>
              </div>
            {/each}
          </div>
          {#if !group.isExpanded && group.advanced.length > 0}
            <div class="pt-3 border-t border-[var(--fp-border)]/60 flex flex-col sm:flex-row sm:items-center justify-between gap-2">
              <button
                type="button"
                class="inline-flex items-center gap-1.5 text-xs text-[var(--fp-accent)] hover:underline font-medium"
                onclick={() => toggleGroup(group.name)}
              >
                <ChevronDown size={14} />
                <span>{$tr('Show {count} more advanced settings in {group}', { count: group.advanced.length, group: groupLabel(group.name) })}</span>
              </button>
              <span class="text-[11px] text-[var(--fp-dim)] font-mono truncate max-w-xs">
                {group.advanced.slice(0, 3).map((e) => e.key).join(', ')}{group.advanced.length > 3 ? '…' : ''}
              </span>
            </div>
          {:else if group.isExpanded && group.advanced.length > 0 && viewMode === 'essential'}
            <div class="pt-3 border-t border-[var(--fp-border)]/60">
              <button
                type="button"
                class="inline-flex items-center gap-1.5 text-xs text-[var(--fp-dim)] hover:text-[var(--fp-text)] font-medium"
                onclick={() => toggleGroup(group.name)}
              >
                <ChevronUp size={14} />
                <span>{$tr('Hide advanced settings')}</span>
              </button>
            </div>
          {/if}
        </Card>
      {/each}


      {#if !groups.length}
        <Card>
          <EmptyState title={$tr('No matching configuration keys')} description={$tr('No settings match the active search or category filter.')}>
            {#snippet action()}
              <Button variant="secondary" onclick={() => { searchQuery = ''; viewMode = 'essential'; }}>
                <RefreshCw size={14} />
                {$tr('Reset filters')}
              </Button>
            {/snippet}
          </EmptyState>
        </Card>
      {/if}
    </div>
    <!-- Current values: read-only effective config, secrets masked -->
    <Card title={$tr('Current Values')} description={$tr('Read-only view of the running configuration. Secret values are masked.')} pad="none">
      <div class="overflow-x-auto max-h-96 overflow-y-auto">
        {#if data?.effective?.length}
          <table class="fp-table">
            <caption class="sr-only">{$tr('Effective configuration — key and value')}</caption>
            <thead>
              <tr>
                <th scope="col">{$tr('Key')}</th>
                <th scope="col">{$tr('Value')}</th>
              </tr>
            </thead>
            <tbody>
              {#each data.effective as kv}
                <tr>
                  <td>
                    <div class="flex items-center gap-2 min-w-0">
                      <span class="fp-num text-[11px] font-semibold text-[var(--fp-text)] truncate">{kv.key}</span>
                      {#if kv.secret}
                        <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-error)]/40 bg-[var(--fp-error)]/15 text-[#FCA5A5] font-semibold uppercase tracking-wider shrink-0">{$tr('secret')}</span>
                      {/if}
                    </div>
                  </td>
                  <td>
                    <div class="flex items-center gap-2 min-w-0">
                      <span class="fp-num text-[11px] text-[var(--fp-muted)] truncate max-w-[180px]">
                        {kv.secret ? '••••••••' : (kv.value || '—')}
                      </span>
                      {#if kv.secret}
                        <span class="text-[10px] text-[var(--fp-dim)] uppercase tracking-wider">{$tr('redacted')}</span>
                      {:else if kv.value}
                        <span class="shrink-0">
                          <CopyButton text={kv.value} label={$tr('copy')} />
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
            <EmptyState title={$tr('No effective configuration')} description={$tr('Start the proxy to populate this view.')}>
              {#snippet action()}
                <Button variant="secondary" onclick={fetchData}>
                  <RefreshCw size={15} />
                  {$tr('Refresh')}
                </Button>
              {/snippet}
            </EmptyState>
          </div>
        {/if}
      </div>
    </Card>
    <!-- Advanced: raw .env editor (same rules as the legacy editor) -->
    <details class="fp-card">
      <summary class="flex items-center justify-between gap-3 px-5 py-4 cursor-pointer text-sm font-medium text-[var(--fp-text)] list-none">
        <span class="flex items-center gap-2">
          <span>{$tr('Advanced: raw .env editor')}</span>
          {#if dirty}
            <StatusBadge status={$tr('{count} changed', { count: changedKeysCount })} tone="warn" pulse />
          {/if}
        </span>
        <span class="text-[11px] text-[var(--fp-dim)]">{$tr('Direct editing. Form fields above mirror this text.')}</span>
      </summary>
      <div class="border-t border-[var(--fp-border)] px-5 py-4">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 mb-3">
          <div>
            <p class="text-xs text-[var(--fp-muted)]">
              {$tr('Edit environment variables directly. Save validates server-side and reloads; rejected writes are rolled back.')}
            </p>
            <p class="mt-1 text-[11px] text-[var(--fp-dim)]">
              {$tr('Changes take effect after save.')} <kbd class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]">Ctrl+S</kbd> {$tr('saves from the keyboard')}.
            </p>
          </div>
          <div class="flex items-center gap-2 shrink-0">
            {#if data}
              <StatusBadge
                status={data.has_env_file ? $tr('env loaded') : $tr('no env file')}
                tone={data.has_env_file ? 'good' : 'warn'}
              />
            {/if}
            <Button variant="secondary" size="sm" onclick={validateConfig} disabled={saving}>
              {$tr('Validate')}
            </Button>
          </div>
        </div>

        <label for="config-env" class="sr-only">{$tr('Environment file content')}</label>
        <textarea
          id="config-env"
          bind:value={rawText}
          oninput={onRawInput}
          spellcheck="false"
          class="fp-input fp-mono w-full min-h-[220px] text-[13px] p-3.5 resize-y
            {validationErrors.length > 0 ? 'border-[var(--fp-error)]/60' : envValid ? 'border-[var(--fp-success)]/40' : ''}"
          placeholder="# Configuration variables..."
        ></textarea>

        {#if validationErrors.length > 0}
          <div role="alert" aria-live="polite" class="mt-3 p-3 rounded-[var(--fp-radius-sm)] fp-inset border-[var(--fp-error)]/30 space-y-1">
            <p class="text-xs font-semibold text-[var(--fp-error)]">
              {$tr('{count} validation error(s):', { count: validationErrors.length })}
            </p>
            {#each validationErrors.slice(0, 5) as err}
              <p class="text-[11px] font-mono text-[var(--fp-error)]/80">{err}</p>
            {/each}
            {#if validationErrors.length > 5}
              <p class="text-[11px] text-[var(--fp-dim)]">… {$tr('and {count} more', { count: validationErrors.length - 5 })}</p>
            {/if}
          </div>
        {/if}

        <div class="mt-3 flex items-center justify-between gap-3 text-[11px] text-[var(--fp-dim)] font-mono">
          <span>{keyCount} {$tr('keys')} {#if lastSavedTimeStr}<span class="text-[var(--fp-border-bright)]">|</span> {$tr('saved {time}', { time: lastSavedTimeStr })}{/if}</span>
        </div>
      </div>
    </details>
  {/if}
</div>

<style>
  .fp-checkbox {
    width: 18px;
    height: 18px;
    accent-color: var(--fp-accent);
    cursor: pointer;
  }
  .fp-checkbox:disabled {
    cursor: not-allowed;
  }
</style>
