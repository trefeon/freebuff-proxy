<script>
  /**
   * Overview — live proxy status, KPI row, and at-risk token cards.
   * Data: the overview endpoint (pooled snapshot + token cards), polled every 15s.
   * All KPIs/cards map to real response fields only.
   */
  import { onMount } from 'svelte';
  import { RefreshCw, ExternalLink, Key, Eye, EyeOff, Trash2 } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import GeneratedKeyModal from '../components/GeneratedKeyModal.svelte';
  import RiskCards from '../components/RiskCards.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Stat from '../components/Stat.svelte';
  import Card from '../components/Card.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Button from '../components/Button.svelte';
  import { fetchAPI, postForm } from '../api/client.js';
  import { adminApi, adminActions } from '../api/paths.js';
  import { generateRandomApiKey } from '../utils/format.js';
  import { usePolling } from '../utils/polling.js';
  import { tr } from '../i18n.js';
  import { getEnvValue, setEnvValue } from '../utils/env.js';
  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  // Client API-key management (API_KEYS in .env)
  let apiKeys = $state([]);
  let clientKeyMessage = $state('');
  let clientKeyOK = $state(true);
  let generatingKey = $state(false);
  let generatedKey = $state('');
  let deletingKey = $state('');
  let showGeneratedModal = $state(false);
  let visibleKeys = $state({});

  function toggleKeyVisibility(key) {
    visibleKeys = { ...visibleKeys, [key]: !visibleKeys[key] };
  }
  function maskKey(key) {
    if (visibleKeys[key]) return key;
    if (!key) return '';
    if (key.length <= 10) return '••••••••';
    const prefix = key.startsWith('sk-fb-') ? 'sk-fb-' : key.slice(0, 6);
    const suffix = key.slice(-4);
    const padding = '•'.repeat(Math.max(0, key.length - prefix.length - suffix.length));
    return `${prefix}${padding}${suffix}`;
  }

  function openGeneratedKeyModal(key) {
    generatedKey = key;
    showGeneratedModal = true;
  }

  function closeGeneratedKeyModal() {
    showGeneratedModal = false;
  }

  async function generateClientKey() {
    if (generatingKey) return;
    generatingKey = true;
    generatedKey = '';
    clientKeyMessage = '';
    try {
      const newKey = generateRandomApiKey();
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const existing = getEnvValue(envContent, 'API_KEYS') || '';
      const updated = existing ? `${existing},${newKey}` : newKey;
      const newContent = setEnvValue(envContent, 'API_KEYS', updated);
      const save = await postForm(adminActions.configSave, { content: newContent });
      const result = await save.json();
      const isSaved = save.ok;
      const isOverridden = result?.message && String(result.message).includes('overridden by the process environment');
      clientKeyOK = isSaved;
      if (clientKeyOK) {
        openGeneratedKeyModal(newKey);
        clientKeyMessage = isOverridden
          ? $tr('Generated & saved client API key (environment notice: server process environment takes precedence until restart)')
          : $tr('Generated & saved client API key');
        fetchData();
    fetchConfig();
      } else {
        clientKeyMessage = result?.message || $tr('Failed to save client API key');
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || $tr('Network error generating client key');
    } finally {
      generatingKey = false;
    }
  }

  async function deleteApiKey(target) {
    if (deletingKey) return;
    deletingKey = target;
    clientKeyMessage = '';
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const val = getEnvValue(envContent, 'API_KEYS') || '';
      const keys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
      const filtered = keys.filter((k) => k !== target);
      const updated = filtered.join(',');
      const newContent = setEnvValue(envContent, 'API_KEYS', updated);
      const save = await postForm(adminActions.configSave, { content: newContent });
      const result = await save.json();
      const isSaved = save.ok;
      const isOverridden = result?.message && String(result.message).includes('overridden by the process environment');
      clientKeyOK = isSaved;
      if (clientKeyOK) {
        clientKeyMessage = isOverridden
          ? $tr('Deleted client API key (environment notice: server process environment takes precedence until restart)')
          : $tr('Deleted client API key');
        apiKeys = filtered;
        fetchConfig();
      } else {
        clientKeyMessage = result?.message || $tr('Failed to delete client API key');
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || $tr('Network error deleting client key');
    } finally {
      deletingKey = '';
    }
  }

  // Config-derived display fields (apiKeys) change only on
  // save, so they are fetched once on mount instead of on every 15s poll.
  async function fetchConfig() {
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const m = envContent.match(/^\s*API_KEYS=(.*)$/m);
      const val = m ? m[1].trim() : '';
      apiKeys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
    } catch {
      apiKeys = [];
    }
  }
  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.overview);
    } catch (e) {
      error = e.message || $tr('Could not reach the proxy API. Check that the server is running.');
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    fetchData();
    fetchConfig();
  });

  function retry() {
    error = '';
    loading = true;
    fetchData();
  }

  usePolling(fetchData, 15000);

  // --- KPIs, derived from real overview fields (no invented fields) ---
  let poolTotal = $derived(data?.tokens?.length ?? 0);
  let busyTokens = $derived(data?.tokens?.filter((t) => t.active_runs > 0).length ?? 0);
  let cooldownTokens = $derived(data?.tokens?.filter((t) => t.cooldown_active).length ?? 0);
  let bannedTokens = $derived(data?.tokens?.filter((t) => t.risk_level === 'critical').length ?? 0);
  let requestsToday = $derived(data?.tokens?.reduce((s, t) => s + (t.requests || 0), 0) ?? 0);

  let atRiskTokens = $derived((data?.tokens ?? []).filter((t) => t.risk_level && t.risk_level !== 'low'));

  // Dynamic Base URL follows the browser's current host (VPS IP, domain, VPN reverse proxy)
  // as computed dynamically by the backend from the request headers (Host, X-Forwarded-Host/Proto).
  let dynamicBaseURL = $derived.by(() => {
    if (data?.base_url) {
      return data.base_url;
    }
    if (typeof window !== 'undefined' && window.location.host) {
      return `${window.location.protocol}//${window.location.host}/v1`;
    }
    return 'http://127.0.0.1:3457/v1';
  });
</script>

<div class="space-y-6 page-enter">
  <PageHeader title={$tr('Overview')} description={$tr('Live proxy status and token pool telemetry')}>
    {#snippet actions()}
      {#if data}
        <StatusBadge
          status={data.mode}
          tone={data.in_bridge ? 'good' : 'info'}
        />
        <span class="fp-num text-xs text-[var(--fp-dim)]">up {data.uptime}</span>
      {/if}
    {/snippet}
  </PageHeader>

  <!-- Loading skeleton — live region announces loading without duplicating Alert -->
  {#if loading}
    <div aria-live="polite" aria-busy="true">
      <span class="sr-only">{$tr('Loading overview…')}</span>
      <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4" aria-hidden="true">
        {#each [1, 2, 3, 4, 5, 6] as _}
          <div class="skeleton skeleton-card"></div>
        {/each}
      </div>
      <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4 mt-4" aria-hidden="true">
        {#each [1, 2, 3] as _}
          <div class="skeleton skeleton-card"></div>
        {/each}
      </div>
    </div>
  {/if}

  <!-- Upstream sync banner: warns operators that the running build is
       behind CodebuffAI/freebuff@main. Data ships compiled into the
       binary (see backend/internal/dashboard/data/upstream_drift.json) and is
       refreshed by .github/workflows/upstream-drift.yml. -->
  {#if data?.upstream_sync}
    {@const us = data.upstream_sync}
    {#if us.has_drift}
      <Alert
        tone={us.has_registry_drift ? 'error' : 'warning'}
        title={$tr('Upstream has updates — your build is behind')}
      >
        <p class="mb-2">
          {$tr('CodebuffAI/freebuff moved past vendor {sha} (checked {when}). This build knows about {pinned} upstream SHAs; a newer one is on main.', {
            sha: us.upstream_sha,
            when: us.checked_at,
            pinned: us.drifted_files?.length ?? 0,
          })}
        </p>
        {#if us.drifted_files && us.drifted_files.length > 0}
          <ul class="text-xs space-y-1 mb-3">
            {#each us.drifted_files as f}
              <li>
                <span class="fp-num text-[var(--fp-muted)]">[{f.group}]</span>
                <code class="fp-num text-xs">{f.file}</code>
                {#if f.pinned_sha}
                  <span class="fp-num text-[var(--fp-dim)]">({f.pinned_sha} -> {f.vendor_sha})</span>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
        <p class="text-xs text-[var(--fp-muted)]">
          {$tr('Registry pin drift can land via the auto-synced PR; wire-shape drift needs a human to port (the upstream-drift workflow opens a needs-port issue for each).')}
        </p>
        <a
          href={us.releases_url}
          target="_blank"
          rel="noopener noreferrer"
          class="mt-2 inline-flex items-center gap-1 text-xs text-[var(--fp-accent)] hover:underline"
        >
          {$tr('Open releases page')}
          <ExternalLink size={12} />
        </a>
      </Alert>
    {/if}
  {/if}

  <!-- Fetch error with retry -->
  {#if error}
    <Alert tone="error" title={$tr('Overview unavailable')}>
      <p>{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={retry}>
          <RefreshCw size={16} />
          {$tr('Retry')}
        </Button>
      </div>
    </Alert>
  {/if}

  {#if data && !loading}
    {#if data.has_tokens}
      <!-- KPI row (pooled tokens active) -->
      <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4">
        <Stat label={$tr('Pool total')} value={poolTotal} big />
        <Stat label={$tr('Busy')} value={busyTokens} hint={$tr('tokens with active runs')} big />
        <Stat label={$tr('Cooldown')} value={cooldownTokens} tone={cooldownTokens > 0 ? 'warn' : 'default'} big />
        <Stat label={$tr('Banned')} value={bannedTokens} hint={$tr('critical risk')} tone={bannedTokens > 0 ? 'bad' : 'default'} big />
        <Stat label={$tr('Requests today')} value={requestsToday} big />
        <Stat label={$tr('Models')} value={data.model_count ?? 0} big />
      </div>

      <!-- Hybrid mode: pool summary above plus a compact bridge-relay card -->
      {#if data.mode === 'hybrid'}
        <Card title={$tr('Bridge relay')}>
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr('{count} active bridge client(s) relaying their own FreeBuff tokens', { count: data.bridge_tokens ?? 0 })}
          </p>
          {#if data.bridge_token_cards?.length}
            <ul class="mt-2 flex flex-col gap-1.5">
              {#each data.bridge_token_cards.slice(0, 4) as bc (bc.key)}
                <li class="flex flex-wrap items-center gap-2 text-xs">
                  <StatusBadge status={bc.status} />
                  <code class="fp-num font-mono text-[var(--fp-text)]">{bc.key}</code>
                  {#if bc.model}
                    <code class="fp-num font-mono text-[var(--fp-muted)]">{bc.model}</code>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
        </Card>
      {/if}

      <!-- Token risk cards -->
      <RiskCards tokens={atRiskTokens} total={poolTotal} />
    {:else}
      <!-- Bridge mode / empty pool summary -->
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <Stat label={$tr('Relay Mode')} value={data.in_bridge ? 'Bridge' : 'Hybrid'} hint={data.in_bridge ? $tr('client-supplied tokens') : $tr('shared pool + bridge')} big />
        <Stat label={$tr('Active Bridge Clients')} value={data.bridge_tokens ?? 0} hint={$tr('relaying upstream sessions')} big />
        <Stat label={$tr('Served Models')} value={data.model_count ?? 0} hint={$tr('OpenAI & Anthropic')} big />
      </div>

      <Card
        title={$tr('Gateway Ready — Bridge & Pooled Relay')}
        description={$tr('The gateway is online and ready for traffic. Connect your tools directly in Bridge mode, or add FreeBuff accounts to create a shared token pool.')}
      >
        {#snippet actions()}
          <a href="#tokens" class="fp-btn fp-btn-secondary fp-btn-sm inline-flex items-center gap-1.5">
            <span>{$tr('Manage Tokens')}</span>
          </a>
        {/snippet}
        <div class="text-xs text-[var(--fp-muted)] space-y-2">
          <p>
            <strong class="text-[var(--fp-text)]">{$tr('Bridge Mode (Active):')}</strong> {$tr('Clients can send requests using their own FreeBuff token as the Bearer or x-api-key credential.')}
          </p>
          <p>
            <strong class="text-[var(--fp-text)]">{$tr('Pooled Mode (Ready):')}</strong> {$tr('Add FreeBuff accounts in Tokens (via Device Login or pasting tokens) to enable shared pool rotation, admission coercion, and Client API Key routing.')}
          </p>
        </div>
      </Card>

      {#if data.bridge_token_cards?.length}
        <Card title={$tr('Active Bridge Clients')}>
          <ul class="flex flex-col gap-1.5">
            {#each data.bridge_token_cards as bc (bc.key)}
              <li class="flex flex-wrap items-center gap-2 text-xs">
                <StatusBadge status={bc.status} />
                <code class="fp-num font-mono text-[var(--fp-text)]">{bc.key}</code>
                {#if bc.model}
                  <code class="fp-num font-mono text-[var(--fp-muted)]">{bc.model}</code>
                {/if}
              </li>
            {/each}
          </ul>
        </Card>
      {/if}
    {/if}

    <!-- Universal Client Integration & Endpoints Card (Always Available) -->
    <section aria-label="Client integration">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold text-[var(--fp-text)]">{$tr('Client Integration')}</h2>
        <span class="text-xs font-mono text-[var(--fp-muted)]">OpenAI & Anthropic Compatible</span>
      </div>

      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <!-- Gateway Base URL -->
        <Card title={$tr('Gateway Base URL')} description={$tr('Universal base endpoint for any OpenAI or Anthropic client, SDK, or CLI tool.')}>
          <div class="flex items-center gap-2">
            <div class="fp-inset flex-1 px-3 py-2 overflow-x-auto">
              <code class="fp-num text-xs text-[var(--fp-accent)] font-mono font-semibold">{dynamicBaseURL}</code>
            </div>
            <CopyButton text={dynamicBaseURL} label={$tr('Copy URL')} />
          </div>
          <p class="mt-3 text-xs text-[var(--fp-muted)]">
            {$tr('Authentication: Use any Client API Key below via Bearer token or x-api-key header.')}
          </p>
        </Card>

        <!-- Supported Protocols & Routes -->
        <Card title={$tr('Supported Wire Protocols')} description={$tr('Dual-protocol translation handled transparently by the gateway.')}>
          <div class="space-y-2">
            <div class="fp-inset px-3 py-2 flex items-center justify-between gap-2 text-xs">
              <div class="flex items-center gap-2 min-w-0">
                <span class="px-1.5 py-0.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] font-mono text-[10px] text-[var(--fp-accent)]">OpenAI</span>
                <span class="font-mono text-[var(--fp-text)] truncate">POST /v1/chat/completions</span>
              </div>
              <span class="text-[var(--fp-dim)] text-[11px] shrink-0">Cursor, Aider, OMP</span>
            </div>
            <div class="fp-inset px-3 py-2 flex items-center justify-between gap-2 text-xs">
              <div class="flex items-center gap-2 min-w-0">
                <span class="px-1.5 py-0.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] font-mono text-[10px] text-[#A78BFA]">Anthropic</span>
                <span class="font-mono text-[var(--fp-text)] truncate">POST /v1/messages</span>
              </div>
              <span class="text-[var(--fp-dim)] text-[11px] shrink-0">Claude Code, Cline</span>
            </div>
          </div>
        </Card>
      </div>


      <div class="mt-4">
        <Card
          title={$tr('Client API Keys')}
          description={$tr('sk-fb-… credentials for clients (omp, Cursor, Claude Code, curl) to authenticate against this gateway. Stored in API_KEYS in .env.')}
        >
          {#snippet actions()}
            <Button variant="primary" size="sm" onclick={generateClientKey} disabled={generatingKey}>
              {#if generatingKey}
                <RefreshCw size={14} class="animate-spin" />
                <span>{$tr('Generating…')}</span>
              {:else}
                <Key size={14} />
                <span>{$tr('Generate API Key')}</span>
              {/if}
            </Button>
          {/snippet}

          {#if apiKeys.length > 0}
            <div class="flex flex-col gap-2 mb-3">
              {#each apiKeys as key (key)}
                <div class="fp-inset rounded flex items-center justify-between gap-2 px-3 py-2">
                  <code class="fp-num text-xs truncate flex-1 select-all font-mono">{maskKey(key)}</code>
                  <div class="flex items-center gap-1 shrink-0">
                    <Button
                      variant="ghost"
                      size="sm"
                      onclick={() => toggleKeyVisibility(key)}
                      aria-label={visibleKeys[key] ? $tr('Hide API key') : $tr('Show API key')}
                      title={visibleKeys[key] ? $tr('Hide API key') : $tr('Show API key')}
                    >
                      {#if visibleKeys[key]}
                        <EyeOff size={14} />
                      {:else}
                        <Eye size={14} />
                      {/if}
                    </Button>
                    <CopyButton text={key} label="Copy" />
                    <Button
                      variant="ghost"
                      size="sm"
                      onclick={() => deleteApiKey(key)}
                      disabled={deletingKey === key}
                      aria-label={$tr('Delete API key')}
                      title={$tr('Delete API key')}
                    >
                      <Trash2 size={14} />
                      <span>{$tr('Delete')}</span>
                    </Button>
                  </div>
                </div>
              {/each}
            </div>
          {:else}
            <p class="text-xs text-[var(--fp-dim)] mb-3">
              {$tr('No client API keys configured. In open mode, clients can authenticate with any key or leave it unset.')}
            </p>
          {/if}

          {#if clientKeyMessage}
            <Alert tone={clientKeyOK ? 'success' : 'error'} title={clientKeyMessage} />
          {/if}
        </Card>
      </div>
    </section>

    <!-- Pop-up modal for newly generated API key -->
    <GeneratedKeyModal bind:open={showGeneratedModal} key={generatedKey} onClose={closeGeneratedKeyModal} />
  {/if}
</div>
