<script>
  import { onMount, onDestroy } from 'svelte';
  import {
    Settings, Save, RefreshCw, Info, Check, Shield, Zap, Bug,
    Sliders, Code, Search, Sparkles, AlertCircle, HelpCircle, ArrowRight, X
  } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import { fetchAPI } from '../utils/api.js';
  import { generateRandomApiKey, generateRandomAdminToken } from '../utils/format.js';
  import { copyToClipboard } from '../utils/clipboard.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  let envContent = $state('');
  let saving = $state(false);
  let saveResult = $state(null);
  let originalContent = $state('');
  let hasUnsavedChanges = $derived(envContent !== originalContent && envContent !== '');

  let searchQuery = $state('');
  let selectedSettingKey = $state('SAFE_MODE');
  let hoveredSettingKey = $state(null);
  let presetToast = $state(null);
  let presetToastTimeout = null;

  const settingDocs = {
    SAFE_MODE: {
      name: 'Safe Mode (Anti-Ban)',
      desc: 'Enforces recommended anti-ban protections: stealth TLS profile rotation, randomized request jitter, and transient backoff retries.',
      defaultValue: 'true',
      category: 'Stealth & Safety',
      type: 'boolean',
    },
    COST_MODE: {
      name: 'Cost Mode',
      desc: 'Billing tier declaration for upstream calls. "free" avoids 402 Payment Required / Out of Credits errors on free-tier accounts.',
      defaultValue: 'free',
      category: 'Stealth & Safety',
      type: 'enum',
      options: ['free', 'paid'],
    },
    TLS_FINGERPRINT: {
      name: 'TLS Fingerprint Emulation',
      desc: 'Emulates genuine browser JA3/JA4 TLS handshake signatures to bypass Cloudflare anti-bot checks.',
      defaultValue: 'auto',
      category: 'Stealth & Safety',
      type: 'enum',
      options: ['auto', 'chrome120', 'safari', 'firefox', 'ios'],
    },
    REQUEST_JITTER: {
      name: 'Request Jitter',
      desc: 'Randomized artificial delay added before forwarding requests to mimic human behavior and avoid robotic burst flags.',
      defaultValue: '2s',
      category: 'Stealth & Safety',
      type: 'preset',
      options: ['0s', '500ms', '1s', '2s', '3s', '5s'],
    },
    HYBRID_MODE: {
      name: 'Hybrid Routing Mode',
      desc: 'When enabled, requests with client-supplied Bearer tokens relay directly (bridge), while token-less requests use the server pool.',
      defaultValue: 'false',
      category: 'Routing & Pool',
      type: 'boolean',
    },
    ROTATION_INTERVAL: {
      name: 'Token Rotation Interval',
      desc: 'How often active upstream tokens in the pool rotate to distribute usage evenly across credentials.',
      defaultValue: '6h',
      category: 'Routing & Pool',
      type: 'preset',
      options: ['15m', '30m', '1h', '2h', '6h', '12h', '24h'],
    },
    IDLE_ROTATION_TIMEOUT: {
      name: 'Idle Rotation Timeout',
      desc: 'Rotates away from an inactive token after this period of inactivity (0 disables).',
      defaultValue: '30m',
      category: 'Routing & Pool',
      type: 'preset',
      options: ['0', '15m', '30m', '1h', '2h'],
    },
    MAX_MESSAGES_PER_DAY: {
      name: 'Daily Message Limit',
      desc: 'Maximum messages per token allowed in a rolling 24-hour window (0 = unlimited).',
      defaultValue: '0',
      category: 'Limits',
      type: 'preset',
      options: ['0', '50', '100', '250', '500', '1000'],
    },
    REQUEST_TIMEOUT: {
      name: 'Request Timeout',
      desc: 'Maximum total duration for an upstream generation before terminating with 504 Gateway Timeout.',
      defaultValue: '15m',
      category: 'Timeouts',
      type: 'preset',
      options: ['2m', '5m', '10m', '15m', '30m'],
    },
    SESSION_CALL_TIMEOUT: {
      name: 'Session Call Timeout',
      desc: 'Maximum timeout for individual session connection negotiation.',
      defaultValue: '30s',
      category: 'Timeouts',
      type: 'preset',
      options: ['10s', '15s', '30s', '60s'],
    },
    LOG_LEVEL: {
      name: 'Log Level',
      desc: 'Console and memory logging output verbosity.',
      defaultValue: 'info',
      category: 'Logging & Debug',
      type: 'enum',
      options: ['debug', 'info', 'warn', 'error'],
    },
    DEBUG_DUMP: {
      name: 'Debug Payload Dump',
      desc: 'Logs full raw JSON request and response payloads to standard error. Warning: very verbose.',
      defaultValue: 'false',
      category: 'Logging & Debug',
      type: 'boolean',
    },
    TRANSIENT_RETRIES: {
      name: 'Transient Retries',
      desc: 'Automatic retry attempts on transient network timeouts or upstream 502/503 errors.',
      defaultValue: '1',
      category: 'Resilience',
      type: 'preset',
      options: ['0', '1', '2', '3', '5'],
    },
    LISTEN_ADDR: {
      name: 'Listen Address',
      desc: 'Local network interface and TCP port for the proxy and admin UI.',
      defaultValue: '127.0.0.1:3457',
      category: 'Network',
      type: 'preset',
      options: ['127.0.0.1:3457', '0.0.0.0:3457', 'localhost:3457'],
    },
    UPSTREAM_BASE_URL: {
      name: 'Upstream Base URL',
      desc: 'Upstream Codebuff endpoint to relay requests to.',
      defaultValue: 'https://www.codebuff.com',
      category: 'Network',
      type: 'string',
    },
    REGISTRY_REFRESH: {
      name: 'Registry Refresh Interval',
      desc: 'Interval for re-fetching the upstream model catalog and agent mappings.',
      defaultValue: '6h',
      category: 'Network',
      type: 'preset',
      options: ['1h', '3h', '6h', '12h', '24h'],
    },
    CLI_VERSION: {
      name: 'Emulated CLI Version',
      desc: 'The User-Agent / X-Codebuff-Version header sent upstream.',
      defaultValue: '0.10.7',
      category: 'Stealth & Safety',
      type: 'string',
    },
    ADMIN_TOKEN: {
      name: 'Admin Token',
      desc: 'Password protection for the web UI and administrative endpoints. Leave blank for loopback-only access.',
      defaultValue: 'unset',
      category: 'Security',
      type: 'secret',
    },
    AUTH_TOKENS: {
      name: 'Auth Tokens',
      desc: 'Comma-separated upstream FreeBuff / Codebuff account tokens.',
      defaultValue: 'empty',
      category: 'Security',
      type: 'secret',
    },
    API_KEYS: {
      name: 'Client API Keys',
      desc: 'Comma-separated client authorization keys. If configured, clients must provide one.',
      defaultValue: 'empty',
      category: 'Security',
      type: 'secret',
    },
  };

  const presets = [
    {
      id: 'stealth',
      label: '🛡️ Stealth Anti-Ban',
      desc: 'Recommended safe defaults (TLS Auto, Jitter 2s, SafeMode true, Cost free)',
      apply: () => {
        setEnvValue('SAFE_MODE', 'true');
        setEnvValue('TLS_FINGERPRINT', 'auto');
        setEnvValue('REQUEST_JITTER', '2s');
        setEnvValue('COST_MODE', 'free');
        setEnvValue('IDLE_ROTATION_TIMEOUT', '30m');
      }
    },
    {
      id: 'fast',
      label: '⚡ Maximum Speed (0 Jitter)',
      desc: 'Minimal latency for local development (Jitter 0s, SafeMode false, Retries 0)',
      apply: () => {
        setEnvValue('REQUEST_JITTER', '0s');
        setEnvValue('SAFE_MODE', 'false');
        setEnvValue('TRANSIENT_RETRIES', '0');
      }
    },
    {
      id: 'debug',
      label: '🐞 Deep Debugging',
      desc: 'Enables debug logging and raw JSON payload dumps',
      apply: () => {
        setEnvValue('LOG_LEVEL', 'debug');
        setEnvValue('DEBUG_DUMP', 'true');
      }
    },
    {
      id: 'hybrid',
      label: '🔄 Hybrid Relay',
      desc: 'Relays client credentials alongside the shared server pool',
      apply: () => {
        setEnvValue('HYBRID_MODE', 'true');
        setEnvValue('SAFE_MODE', 'true');
      }
    }
  ];

  function getCategoryBadgeStyle(category) {
    switch (category) {
      case 'Stealth & Safety':
        return 'bg-[var(--fp-teal)]/10 text-[var(--fp-teal)] border border-[var(--fp-teal)]/25';
      case 'Routing & Pool':
        return 'bg-indigo-500/10 text-indigo-400 border border-indigo-500/25';
      case 'Logging & Debug':
        return 'bg-purple-500/10 text-purple-400 border border-purple-500/25';
      case 'Limits':
      case 'Timeouts':
        return 'bg-[var(--fp-amber)]/10 text-[var(--fp-amber)] border border-[var(--fp-amber)]/25';
      case 'Security':
        return 'bg-rose-500/10 text-rose-400 border border-rose-500/25';
      case 'Network':
      case 'Resilience':
      default:
        return 'bg-sky-500/10 text-sky-400 border border-sky-500/25';
    }
  }

  function showToast(message) {
    if (presetToastTimeout) clearTimeout(presetToastTimeout);
    presetToast = message;
    presetToastTimeout = setTimeout(() => {
      presetToast = null;
    }, 3500);
  }

  function applyPreset(preset) {
    preset.apply();
    showToast(`Applied preset: ${preset.label}`);
  }

  function handleGenerateApiKey() {
    const newKey = generateRandomApiKey();
    const currentVal = getEnvValue('API_KEYS');
    const hasExisting = currentVal && currentVal !== 'empty' && currentVal !== 'unset' && currentVal.trim() !== '';
    const updatedVal = hasExisting ? `${currentVal.trim()},${newKey}` : newKey;
    setEnvValue('API_KEYS', updatedVal);
    copyToClipboard(newKey);
    showToast(`Generated & copied API Key: ${newKey}`);
  }

  function handleGenerateAdminToken() {
    const newToken = generateRandomAdminToken();
    setEnvValue('ADMIN_TOKEN', newToken);
    copyToClipboard(newToken);
    showToast(`Generated & copied Admin Token: ${newToken}`);
  }

  // Helper to read current value of a key from envContent or fallback to effective
  function getEnvValue(key) {
    const regex = new RegExp(`^\\s*${key}=(.*)$`, 'm');
    const match = envContent.match(regex);
    if (match) return match[1].trim();

    const eff = data?.effective?.find(kv => kv.key === key);
    if (eff && !eff.secret) return eff.value;
    return settingDocs[key]?.defaultValue || '';
  }

  // Helper to update a key in envContent
  function setEnvValue(key, value) {
    let lines = envContent.split('\n');
    let found = false;
    const regex = new RegExp(`^#?\\s*${key}=.*$`);
    lines = lines.map(line => {
      if (regex.test(line)) {
        found = true;
        return `${key}=${value}`;
      }
      return line;
    });
    if (!found) {
      lines.push(`${key}=${value}`);
    }
    envContent = lines.join('\n');
  }

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/config');
      envContent = data.env_content || '';
      originalContent = envContent;
    } catch (e) {
      error = e.message || 'Failed to fetch configuration';
    } finally {
      loading = false;
    }
  }

  async function saveConfig(e) {
    e?.preventDefault();
    if (saving) return;
    saving = true;
    saveResult = null;

    try {
      const res = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: envContent }),
      });
      const result = await res.json();
      saveResult = {
        ok: res.ok && result.ok,
        message: result.message || (res.ok ? 'Configuration saved and reloaded.' : 'Save failed'),
      };
      if (saveResult.ok) fetchData();
    } catch (e) {
      saveResult = { ok: false, message: e.message || 'Network error saving configuration' };
    } finally {
      saving = false;
    }
  }

  function handleBeforeUnload(e) {
    if (hasUnsavedChanges) {
      e.preventDefault();
      e.returnValue = '';
    }
  }

  function handleKeyDown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') {
      e.preventDefault();
      if (hasUnsavedChanges && !saving) {
        saveConfig();
      }
    }
  }

  // Filtered settings for quick inspector
  let filteredSettings = $derived(() => {
    const list = data?.effective || [];
    if (!searchQuery.trim()) return list;
    const q = searchQuery.toLowerCase().trim();
    return list.filter(kv => {
      const doc = settingDocs[kv.key];
      return kv.key.toLowerCase().includes(q) ||
             (doc?.name && doc.name.toLowerCase().includes(q)) ||
             (doc?.desc && doc.desc.toLowerCase().includes(q)) ||
             (doc?.category && doc.category.toLowerCase().includes(q));
    });
  });

  let activeDoc = $derived(settingDocs[selectedSettingKey] || {
    name: selectedSettingKey,
    desc: 'Configuration parameter for freebuff-proxy.',
    category: 'General',
    defaultValue: '—'
  });

  onMount(() => {
    fetchData();
    window.addEventListener('beforeunload', handleBeforeUnload);
    window.addEventListener('keydown', handleKeyDown);
  });

  onDestroy(() => {
    window.removeEventListener('beforeunload', handleBeforeUnload);
    window.removeEventListener('keydown', handleKeyDown);
    if (presetToastTimeout) clearTimeout(presetToastTimeout);
  });
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Configuration Studio" subtitle="Runtime settings inspector, quick presets & hot-reloading .env editor with atomic rollback">
    {#if data}
      <StatusBadge variant={data.has_env_file ? 'teal' : 'amber'}>
        {data.has_env_file ? '✓ .env loaded' : 'no .env file yet'}
      </StatusBadge>
      {#if hasUnsavedChanges}
        <span class="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-semibold bg-[var(--fp-red)]/15 text-[var(--fp-red)] border border-[var(--fp-red)]/30 animate-pulse">
          <span class="w-1.5 h-1.5 rounded-full bg-[var(--fp-red)]"></span>
          Unsaved Changes
        </span>
      {/if}
    {/if}
  </PageHeader>

  <!-- Save Status Alert -->
  {#if saveResult}
    <Alert
      variant={saveResult.ok ? 'success' : 'error'}
      message={saveResult.message}
      ondismiss={() => saveResult = null}
    />
  {/if}

  <!-- Preset Applied Micro-Toast Alert -->
  {#if presetToast}
    <div class="p-3 rounded-lg bg-[var(--fp-amber)]/15 border border-[var(--fp-amber)]/30 text-white text-xs flex items-center justify-between animate-fadeIn">
      <div class="flex items-center gap-2">
        <Sparkles size={15} class="text-[var(--fp-amber)]" />
        <span class="font-semibold">{presetToast}</span>
        <span class="text-[11px] text-[var(--fp-muted)]">(Press Ctrl+S to save)</span>
      </div>
      <button
        type="button"
        onclick={() => presetToast = null}
        class="text-[var(--fp-dim)] hover:text-white transition-colors"
        aria-label="Dismiss notification"
      >
        <X size={14} />
      </button>
    </div>
  {/if}

  <!-- Quick Presets Bar -->
  <div class="fp-card p-4 space-y-3">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2">
        <Sparkles size={16} class="text-[var(--fp-amber)]" />
        <h2 class="text-sm font-semibold text-white">One-Click Configuration Presets</h2>
      </div>
      <span class="text-[11px] text-[var(--fp-dim)]">Click a preset to populate optimal values</span>
    </div>
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-2.5">
      {#each presets as preset}
        <button
          type="button"
          onclick={() => applyPreset(preset)}
          class="p-3 rounded-lg fp-inset hover:border-[var(--fp-amber)]/40 focus-visible:ring-2 focus-visible:ring-[var(--fp-amber)] focus-visible:outline-none transition-all text-left group flex flex-col justify-between"
        >
          <div>
            <div class="text-xs font-bold text-white group-hover:text-[var(--fp-amber)] transition-colors">{preset.label}</div>
            <p class="text-[11px] text-[var(--fp-muted)] mt-1 line-clamp-2 leading-relaxed">{preset.desc}</p>
          </div>
          <div class="text-[10px] text-[var(--fp-amber)] font-medium mt-2 flex items-center gap-1 opacity-80 group-hover:opacity-100">
            <span>Apply Preset</span>
            <ArrowRight size={11} />
          </div>
        </button>
      {/each}
    </div>
  </div>

  <!-- Main Work Area -->
  <div class="grid grid-cols-1 lg:grid-cols-12 gap-6">
    <!-- Left: Visual Inspector with Quick Controls & Hover Info (7 cols) -->
    <div class="lg:col-span-7 space-y-4">
      <div class="fp-card p-5 space-y-4">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 border-b border-[var(--fp-border)] pb-4">
          <div>
            <h2 class="text-base font-semibold text-white">Interactive Settings & Quick Knobs</h2>
            <p class="text-xs text-[var(--fp-muted)] mt-0.5">Hover or click any setting to view documentation and adjust values.</p>
          </div>
          <!-- Search box -->
          <div class="relative w-full sm:w-60 shrink-0">
            <label for="config-search" class="sr-only">Search settings</label>
            <div class="relative flex items-center">
              <Search size={14} class="absolute left-3 text-[var(--fp-dim)] pointer-events-none" />
              <input
                id="config-search"
                type="text"
                bind:value={searchQuery}
                placeholder="Search setting..."
                class="fp-input text-xs pl-9 pr-8 py-1.5 h-8.5 focus-visible:ring-2 focus-visible:ring-[var(--fp-amber)]"
              />
              {#if searchQuery}
                <button
                  type="button"
                  onclick={() => searchQuery = ''}
                  class="absolute right-2 p-1 rounded hover:bg-[var(--fp-surface-3)] text-[var(--fp-dim)] hover:text-white transition-colors"
                  aria-label="Clear search"
                >
                  <X size={13} />
                </button>
              {/if}
            </div>
          </div>
        </div>

        <!-- Settings List -->
        <div class="space-y-2 max-h-[520px] overflow-y-auto pr-1">
          {#if filteredSettings().length === 0}
            <div class="py-10 text-center space-y-3">
              <Search size={24} class="mx-auto text-[var(--fp-dim)] opacity-60" />
              <p class="text-xs text-[var(--fp-muted)]">No configuration settings match "<span class="text-white font-mono">{searchQuery}</span>"</p>
              <button
                type="button"
                onclick={() => searchQuery = ''}
                class="fp-btn-secondary text-xs"
              >
                Clear Search Filter
              </button>
            </div>
          {:else}
            {#each filteredSettings() as kv}
              {@const doc = settingDocs[kv.key]}
              {@const curVal = getEnvValue(kv.key)}
              {@const isSelected = selectedSettingKey === kv.key}
              <div
                role="button"
                tabindex="0"
                onclick={() => selectedSettingKey = kv.key}
                onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') selectedSettingKey = kv.key; }}
                onmouseenter={() => hoveredSettingKey = kv.key}
                onmouseleave={() => hoveredSettingKey = null}
                class="p-3 rounded-lg border transition-all cursor-pointer text-left focus-visible:ring-2 focus-visible:ring-[var(--fp-amber)] focus-visible:outline-none
                  {isSelected
                    ? 'bg-[var(--fp-amber)]/8 border-[var(--fp-amber)]/40 shadow-sm'
                    : 'fp-inset hover:border-[var(--fp-border-bright)]'}"
              >
                <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
                  <!-- Name & Key -->
                  <div class="flex items-center gap-2 min-w-0">
                    <span class="font-mono text-xs font-bold text-white truncate">{kv.key}</span>
                    {#if doc?.category}
                      <span class="text-[10px] px-1.5 py-0.5 rounded font-sans font-medium {getCategoryBadgeStyle(doc.category)}">
                        {doc.category}
                      </span>
                    {/if}
                    {#if isSelected}
                      <span class="text-[10px] text-[var(--fp-amber)] font-semibold">● Active</span>
                    {/if}
                  </div>

                  <!-- Quick Interactive Controls -->
                  <div class="flex items-center gap-1.5 shrink-0">
                    {#if doc?.type === 'boolean'}
                      <!-- Boolean Toggle Switch -->
                      <div class="flex items-center rounded-lg fp-inset p-0.5">
                        <button
                          type="button"
                          onclick={(e) => { e.stopPropagation(); setEnvValue(kv.key, 'true'); }}
                          class="px-2 py-0.5 rounded text-[11px] font-mono font-semibold transition-all focus-visible:ring-1 focus-visible:ring-[var(--fp-teal)]
                            {curVal === 'true' ? 'bg-[var(--fp-teal)] text-[#1A1A1A]' : 'text-[var(--fp-muted)] hover:text-white'}"
                        >
                          true
                        </button>
                        <button
                          type="button"
                          onclick={(e) => { e.stopPropagation(); setEnvValue(kv.key, 'false'); }}
                          class="px-2 py-0.5 rounded text-[11px] font-mono font-semibold transition-all focus-visible:ring-1 focus-visible:ring-[var(--fp-red)]
                            {curVal === 'false' ? 'bg-[var(--fp-red)] text-white' : 'text-[var(--fp-muted)] hover:text-white'}"
                        >
                          false
                        </button>
                      </div>
                    {:else if doc?.type === 'enum' || doc?.type === 'preset'}
                      <!-- Quick Pill Selector -->
                      <div class="flex flex-wrap items-center gap-1">
                        {#each doc.options?.slice(0, 4) || [] as opt}
                          <button
                            type="button"
                            onclick={(e) => { e.stopPropagation(); setEnvValue(kv.key, opt); }}
                            class="px-1.5 py-0.5 rounded text-[10px] font-mono transition-all focus-visible:ring-1 focus-visible:ring-[var(--fp-amber)]
                              {curVal === opt
                                ? 'bg-[var(--fp-amber)] text-[#1A1A1A] font-bold'
                                : 'fp-inset text-[var(--fp-muted)] hover:text-white'}"
                          >
                            {opt}
                          </button>
                        {/each}
                      </div>
                    {:else if kv.secret}
                      <span class="px-2 py-0.5 rounded fp-inset text-[var(--fp-muted)] font-mono text-[11px]">
                        {kv.value}
                      </span>
                    {:else}
                      <span class="text-xs font-mono font-bold text-[var(--fp-teal)] tabular-nums">
                        {curVal || kv.value || '—'}
                      </span>
                    {/if}
                  </div>
                </div>

                <!-- Quick preview description -->
                {#if doc?.desc}
                  <p class="text-[11px] text-[var(--fp-muted)] mt-1.5 line-clamp-1">
                    {doc.desc}
                  </p>
                {/if}
              </div>
            {/each}
          {/if}
        </div>
      </div>

      <!-- Quick Info Detail Box -->
      <div class="fp-card p-4 space-y-2.5 bg-[var(--fp-surface-2)]">
        <div class="flex items-center justify-between border-b border-[var(--fp-border)] pb-2">
          <div class="flex items-center gap-2">
            <Info size={15} class="text-[var(--fp-amber)]" />
            <h3 class="text-sm font-bold text-white font-mono">{selectedSettingKey}</h3>
            {#if activeDoc.name}
              <span class="text-xs text-[var(--fp-muted)]">({activeDoc.name})</span>
            {/if}
          </div>
          <span class="text-[10px] text-[var(--fp-dim)] uppercase tracking-wider font-semibold">
            Default: <strong class="text-white font-mono">{activeDoc.defaultValue}</strong>
          </span>
        </div>
        <p class="text-xs text-[var(--fp-muted)] leading-relaxed">
          {activeDoc.desc}
        </p>
        {#if activeDoc.options?.length > 0}
          <div class="flex items-center gap-2 pt-1">
            <span class="text-[11px] text-[var(--fp-dim)]">Quick Set:</span>
            <div class="flex flex-wrap gap-1.5">
              {#each activeDoc.options as opt}
                <button
                  type="button"
                  onclick={() => setEnvValue(selectedSettingKey, opt)}
                  class="px-2 py-0.5 rounded text-xs font-mono transition-all focus-visible:ring-1 focus-visible:ring-[var(--fp-amber)]
                    {getEnvValue(selectedSettingKey) === opt
                      ? 'bg-[var(--fp-amber)] text-[#1A1A1A] font-bold shadow-sm'
                      : 'fp-btn-secondary text-[var(--fp-muted)]'}"
                >
                  {opt}
                </button>
              {/each}
            </div>
          </div>
        {/if}
        {#if selectedSettingKey === 'API_KEYS'}
          <div class="pt-1">
            <button
              type="button"
              onclick={handleGenerateApiKey}
              class="fp-btn-primary text-xs py-1.5 px-3 flex items-center gap-1.5"
            >
              <Zap size={13} />
              <span>⚡ Generate Random API Key</span>
            </button>
          </div>
        {:else if selectedSettingKey === 'ADMIN_TOKEN'}
          <div class="pt-1">
            <button
              type="button"
              onclick={handleGenerateAdminToken}
              class="fp-btn-primary text-xs py-1.5 px-3 flex items-center gap-1.5"
            >
              <Zap size={13} />
              <span>⚡ Generate Random Admin Token</span>
            </button>
          </div>
        {/if}
      </div>
    </div>

    <!-- Right: Live Synchronized .env Editor (5 cols) -->
    <div class="lg:col-span-5 flex flex-col">
      <div class="fp-card p-4 flex-1 flex flex-col space-y-3">
        <div class="flex items-center justify-between border-b border-[var(--fp-border)] pb-3">
          <div>
            <h2 class="text-base font-semibold text-white">.env Editor</h2>
            <p class="text-xs text-[var(--fp-muted)] mt-0.5">Live file synced with quick knobs above. (<kbd class="px-1 py-0.5 rounded bg-[var(--fp-surface-3)] text-[10px] font-mono text-[var(--fp-dim)]">Ctrl+S</kbd> to save)</p>
          </div>
          <div class="flex items-center gap-2">
            <button
              type="button"
              onclick={() => envContent = originalContent}
              disabled={!hasUnsavedChanges}
              class="fp-btn-secondary text-[11px]"
            >
              Reset
            </button>
          </div>
        </div>

        <form onsubmit={saveConfig} class="flex-1 flex flex-col space-y-3">
          <textarea
            bind:value={envContent}
            rows="22"
            spellcheck="false"
            required
            class="fp-input fp-input-mono flex-1 text-xs leading-relaxed p-3.5 font-mono focus-visible:ring-2 focus-visible:ring-[var(--fp-amber)]"
            placeholder="# Configuration variables..."
          ></textarea>

          <div class="flex items-center justify-between pt-1">
            <span class="text-xs font-mono text-[var(--fp-dim)]">
              {#if hasUnsavedChanges}
                <span class="text-[var(--fp-red)] font-semibold">● Unsaved changes</span>
              {:else}
                <span class="text-[var(--fp-teal)]">✓ In sync with disk</span>
              {/if}
            </span>

            <button
              type="submit"
              disabled={saving || !hasUnsavedChanges}
              class="fp-btn-primary"
            >
              {#if saving}
                <RefreshCw size={15} class="animate-spin" />
                <span>Saving...</span>
              {:else}
                <Save size={15} />
                <span>Save & Reload</span>
              {/if}
            </button>
          </div>
        </form>
      </div>
    </div>
  </div>
</div>
