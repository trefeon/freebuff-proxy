<script>
  /**
   * Overview — live proxy status, KPI row, and at-risk token cards.
   * Data: GET /admin/api/overview (pooled snapshot + token cards), polled every 15s.
   * All KPIs/cards map to real response fields only.
   */
  import { RefreshCw, ExternalLink, Key, Eye, EyeOff, Trash2, X } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Stat from '../components/Stat.svelte';
  import Card from '../components/Card.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Button from '../components/Button.svelte';
  import { fetchAPI } from '../api/client.js';
  import { formatLocalDate, generateRandomApiKey } from '../utils/format.js';
  import { usePolling } from '../utils/polling.js';
  import { tr } from '../i18n.js';
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
  let visibleKeys = $state({});
  let showGeneratedModal = $state(false);
  let tokenRotation = $state('drain');
  let savingRotation = $state(false);
  let modalEl = $state(null);
  let lastFocusedEl = null;

  function toggleKeyVisibility(key) {
    visibleKeys = { ...visibleKeys, [key]: !visibleKeys[key] };
  }
  function maskKey(key) {
    if (visibleKeys[key]) return key;
    if (!key) return '';
    if (key.length <= 10) return '••••••••';
    const prefix = key.startsWith('sk-fb-') ? 'sk-fb-' : key.slice(0, 6);
    const suffix = key.slice(-4);
    return `${prefix}••••••••••••••••••••••••${suffix}`;
  }

  function openGeneratedKeyModal(key) {
    generatedKey = key;
    showGeneratedModal = true;
    lastFocusedEl = typeof document !== 'undefined' ? document.activeElement : null;
  }

  function closeGeneratedKeyModal() {
    showGeneratedModal = false;
    if (lastFocusedEl && typeof lastFocusedEl.focus === 'function') {
      lastFocusedEl.focus();
    }
  }

  function handleModalKeydown(e) {
    if (e.key === 'Escape') {
      closeGeneratedKeyModal();
      return;
    }
    if (e.key === 'Tab' && modalEl) {
      const focusable = modalEl.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])');
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  async function generateClientKey() {
    if (generatingKey) return;
    generatingKey = true;
    generatedKey = '';
    clientKeyMessage = '';
    try {
      const newKey = generateRandomApiKey();
      const cfgRes = await fetchAPI('/admin/api/config');
      const envContent = cfgRes?.env_content || '';
      const regex = /^\s*API_KEYS=(.*)$/m;
      const match = envContent.match(regex);
      const existing = match ? match[1].trim() : '';
      const updated = existing ? `${existing},${newKey}` : newKey;
      const newContent = match ? envContent.replace(regex, `API_KEYS=${updated}`) : (envContent ? `${envContent}\nAPI_KEYS=${updated}` : `API_KEYS=${updated}`);
      const save = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: newContent }),
      });
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
      const cfgRes = await fetchAPI('/admin/api/config');
      const envContent = cfgRes?.env_content || '';
      const regex = /^\s*API_KEYS=(.*)$/m;
      const match = envContent.match(regex);
      const val = match ? match[1].trim() : '';
      const keys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
      const filtered = keys.filter((k) => k !== target);
      const updated = filtered.join(',');
      const newContent = match ? envContent.replace(regex, `API_KEYS=${updated}`) : (envContent ? `${envContent}\nAPI_KEYS=${updated}` : `API_KEYS=${updated}`);
      const save = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: newContent }),
      });
      const result = await save.json();
      const isSaved = save.ok;
      const isOverridden = result?.message && String(result.message).includes('overridden by the process environment');
      clientKeyOK = isSaved;
      if (clientKeyOK) {
        clientKeyMessage = isOverridden
          ? $tr('Deleted client API key (environment notice: server process environment takes precedence until restart)')
          : $tr('Deleted client API key');
        fetchData();
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

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/overview');
      try {
        const cfgRes = await fetchAPI('/admin/api/config');
        const envContent = cfgRes?.env_content || '';
        const m = envContent.match(/^\s*API_KEYS=(.*)$/m);
        const val = m ? m[1].trim() : '';
        apiKeys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
        const mRot = envContent.match(/^\s*TOKEN_ROTATION=(.*)$/m);
        const rotVal = mRot ? mRot[1].trim().toLowerCase() : 'drain';
        tokenRotation = ['drain', 'round_robin', 'least_used', 'random'].includes(rotVal) ? rotVal : 'drain';
      } catch {
        apiKeys = [];
        tokenRotation = 'drain';
      }
    } catch (e) {
      error = e.message || $tr('Could not reach the proxy API. Check that the server is running.');
    } finally {
      loading = false;
    }
  }
  async function setTokenRotation(newMode) {
    if (savingRotation || tokenRotation === newMode) return;
    savingRotation = true;
    try {
      const cfgRes = await fetchAPI('/admin/api/config');
      const envContent = cfgRes?.env_content || '';
      const regex = /^\s*TOKEN_ROTATION=(.*)$/m;
      const match = envContent.match(regex);
      const newContent = match
        ? envContent.replace(regex, `TOKEN_ROTATION=${newMode}`)
        : (envContent ? `${envContent}\nTOKEN_ROTATION=${newMode}` : `TOKEN_ROTATION=${newMode}`);
      const save = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: newContent }),
      });
      if (save.ok) {
        tokenRotation = newMode;
      }
    } catch (e) {
      console.warn('Failed to update token rotation', e);
    } finally {
      savingRotation = false;
    }
  }

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

  function riskTone(risk) {
    switch (risk) {
      case 'low':
        return 'good';
      case 'moderate':
        return 'warn';
      case 'high':
      case 'critical':
        return 'bad';
      default:
        return 'idle';
    }
  }

  function banBadge(t) {
    if (t.ban_type === 'hard') {
      return { label: $tr('banned — appeal required'), tone: 'critical', pulse: true };
    }
    if (t.ban_type === 'temporary') {
      const until = formatLocalDate(t.banned_until);
      return { label: until ? $tr('banned until {time}', { time: until }) : $tr('banned (temporary)'), tone: 'bad' };
    }
    return null;
  }

  function formatCooldown(until) {
    if (!until) return '';
    const diff = new Date(until).getTime() - Date.now();
    if (diff <= 0) return '';
  }
</script>

<svelte:window onkeydown={showGeneratedModal ? handleModalKeydown : undefined} />

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
       binary (see internal/dashboard/data/upstream_drift.json) and is
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
    {#if data.in_bridge}
      <!-- Bridge mode: no pool snapshot to summarize -->
      <Card>
        <p class="text-sm text-[var(--fp-muted)]">
          {$tr('Bridge mode relays upstream tokens per client request')}
          (<span class="fp-num">{data.bridge_tokens}</span> {$tr('active bridge client(s)')}).
          {$tr('Session pools and quota tracking are client-scoped.')}
        </p>
      </Card>
    {:else if !data.has_tokens}
      <EmptyState
        title={$tr('No upstream tokens configured')}
        description={$tr('Add tokens to AUTH_TOKENS in Config to start the pooled relay.')}
      >
        {#snippet action()}
          <a href="#config" class="fp-btn fp-btn-secondary">{$tr('Go to Config')}</a>
        {/snippet}
      </EmptyState>
    {:else}
      <!-- KPI row -->
      <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4">
        <Stat label={$tr('Pool total')} value={poolTotal} big />
        <Stat label={$tr('Busy')} value={busyTokens} hint={$tr('tokens with active runs')} big />
        <Stat label={$tr('Cooldown')} value={cooldownTokens} tone={cooldownTokens > 0 ? 'warn' : 'default'} big />
        <Stat label={$tr('Banned')} value={bannedTokens} hint={$tr('critical risk')} tone={bannedTokens > 0 ? 'bad' : 'default'} big />
        <Stat label={$tr('Requests today')} value={requestsToday} big />
        <Stat label={$tr('Models')} value={data.model_count ?? 0} big />
      </div>

      <!-- Token risk cards -->
      <section aria-label="At-risk tokens">
        <div class="flex items-center justify-between mb-3">
          <h2 class="text-lg font-semibold text-[var(--fp-text)]">{$tr('Token risk')}</h2>
          <span class="fp-num text-xs text-[var(--fp-dim)]">{atRiskTokens.length}/{poolTotal}</span>
        </div>

        {#if atRiskTokens.length > 0}
          <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4">
            {#each atRiskTokens as t (t.index)}
              <Card>
                <div class="space-y-3">
                  <div class="flex items-center justify-between gap-2">
                    <span class="fp-num text-sm font-semibold text-[var(--fp-text)]">Token #{t.index}</span>
                    {#if banBadge(t)}
                      <StatusBadge status={banBadge(t).label} tone={banBadge(t).tone} pulse={banBadge(t).pulse} />
                    {:else}
                      <StatusBadge
                        status={t.risk_level}
                        tone={riskTone(t.risk_level)}
                        pulse={t.risk_level === 'critical'}
                      />
                    {/if}
                  </div>

                  {#if t.cooldown_active}
                    <div class="fp-inset px-2.5 py-2 text-xs text-[var(--fp-warning)]">
                      {$tr('Cooldown')} — <span class="fp-num">{formatCooldown(t.cooldown_until)}</span> {$tr('remaining')}
                    </div>
                  {/if}

                  <div class="fp-inset px-2.5 py-2 text-xs text-[var(--fp-muted)]">
                    {#if t.daily_limit > 0}
                      <span class="fp-num text-[var(--fp-text)]">{t.messages_24h}/{t.daily_limit}</span> {$tr('msgs today')}
                      (<span class="fp-num">{t.usage_pct}%</span>)
                    {:else}
                      <span class="fp-num text-[var(--fp-text)]">{t.messages_24h}</span> {$tr('msgs 24h')}
                    {/if}
                  </div>

                  <div class="flex justify-between text-xs text-[var(--fp-dim)]">
                    <span>runs <span class="fp-num text-[var(--fp-text)]">{t.active_runs}</span></span>
                    <span>reqs <span class="fp-num text-[var(--fp-text)]">{t.requests}</span></span>
                  </div>
                </div>
              </Card>
            {/each}
          </div>
        {:else}
          <Card>
            <div class="flex items-center gap-2 text-sm text-[var(--fp-muted)]">
              <span class="led led-good" aria-hidden="true"></span>
              {$tr('All tokens healthy — no risk flags.')}
            </div>
          </Card>
        {/if}
      </section>

      <!-- Universal Client Integration & Endpoints Card -->
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
                <code class="fp-num text-xs text-[var(--fp-accent)] font-mono font-semibold">{data?.base_url || 'http://127.0.0.1:3457/v1'}</code>
              </div>
              <CopyButton text={data?.base_url || 'http://127.0.0.1:3457/v1'} label={$tr('Copy URL')} />
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

        <!-- Token Rotation Scheme Selector -->
        <div class="mt-4">
          <Card
            title={$tr('Token Rotation Scheme')}
            description={$tr('Policy used by the gateway to select upstream accounts for model requests.')}
          >
            {#snippet actions()}
              <span class="inline-flex items-center gap-1.5 font-mono text-xs text-[var(--fp-muted)]">
                <span class="led {tokenRotation === 'drain' ? 'led-good' : 'led-idle'}"></span>
                <span class="uppercase tracking-wider font-semibold text-[var(--fp-accent)]">{tokenRotation}</span>
              </span>
            {/snippet}

            <div class="space-y-3">
              <div class="flex flex-wrap items-center gap-2" role="radiogroup" aria-label={$tr('Token Rotation Policy')}>
                <button
                  type="button"
                  role="radio"
                  aria-checked={tokenRotation === 'drain'}
                  disabled={savingRotation}
                  onclick={() => setTokenRotation('drain')}
                  class="fp-btn {tokenRotation === 'drain' ? 'fp-btn-primary' : 'fp-btn-ghost'} fp-btn-sm text-xs"
                >
                  {$tr('Drain (Safest)')}
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={tokenRotation === 'round_robin'}
                  disabled={savingRotation}
                  onclick={() => setTokenRotation('round_robin')}
                  class="fp-btn {tokenRotation === 'round_robin' ? 'fp-btn-primary' : 'fp-btn-ghost'} fp-btn-sm text-xs"
                >
                  {$tr('Round Robin (1:1)')}
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={tokenRotation === 'least_used'}
                  disabled={savingRotation}
                  onclick={() => setTokenRotation('least_used')}
                  class="fp-btn {tokenRotation === 'least_used' ? 'fp-btn-primary' : 'fp-btn-ghost'} fp-btn-sm text-xs"
                >
                  {$tr('Least Used (Max Quota)')}
                </button>
                <button
                  type="button"
                  role="radio"
                  aria-checked={tokenRotation === 'random'}
                  disabled={savingRotation}
                  onclick={() => setTokenRotation('random')}
                  class="fp-btn {tokenRotation === 'random' ? 'fp-btn-primary' : 'fp-btn-ghost'} fp-btn-sm text-xs"
                >
                  {$tr('Random (Stochastic)')}
                </button>
              </div>

              <div class="fp-inset p-3 rounded-lg text-xs text-[var(--fp-muted)] flex items-start gap-2">
                {#if tokenRotation === 'drain'}
                  <p class="leading-relaxed">
                    <strong class="text-[var(--fp-text)]">{$tr('Drain Mode (Default):')}</strong> {$tr('Drains one account completely (e.g. 5/5 Luna sessions) before switching to the next token. Mimics authentic single-user behavior and provides the strongest anti-ban protection.')}
                  </p>
                {:else if tokenRotation === 'round_robin'}
                  <p class="leading-relaxed text-[var(--fp-warning)]">
                    <strong class="text-[var(--fp-text)]">{$tr('Round-Robin Mode (Study):')}</strong> {$tr('Rotates to the next token on every single session (1:1). Note: rapid token switching across healthy accounts may trigger upstream farm detection.')}
                  </p>
                {:else if tokenRotation === 'least_used'}
                  <p class="leading-relaxed">
                    <strong class="text-[var(--fp-text)]">{$tr('Least-Used Mode:')}</strong> {$tr('Always selects the account with the largest remaining session quota to balance usage evenly.')}
                  </p>
                {:else if tokenRotation === 'random'}
                  <p class="leading-relaxed">
                    <strong class="text-[var(--fp-text)]">{$tr('Random Mode:')}</strong> {$tr('Stochastically selects among all eligible tokens with remaining quota to generate unpredictable noise.')}
                  </p>
                {/if}
              </div>
            </div>
          </Card>
        </div>
        <!-- Client API-key management -->
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
      {#if showGeneratedModal}
        <div
          class="fixed inset-0 z-50 flex items-center justify-center p-4"
          role="presentation"
        >
          <!-- Backdrop -->
          <button
            type="button"
            class="fixed inset-0 bg-black/75 backdrop-blur-sm transition-opacity border-0 p-0 m-0 w-full h-full cursor-default"
            onclick={closeGeneratedKeyModal}
            aria-label={$tr('Close modal backdrop')}
            tabindex="-1"
          ></button>

          <!-- Modal Card -->
          <div
            bind:this={modalEl}
            tabindex="-1"
            class="relative w-full max-w-lg bg-[var(--fp-card)] border border-[var(--fp-border)] rounded-xl shadow-2xl p-6 z-10 space-y-4 page-enter focus:outline-none"
            role="dialog"
            aria-modal="true"
            aria-labelledby="generated-key-title"
          >
            <!-- Header -->
            <div class="flex items-center justify-between border-b border-[var(--fp-border)] pb-3">
              <div class="flex items-center gap-2.5">
                <div class="p-2 rounded-lg bg-[var(--fp-accent)]/10 text-[var(--fp-accent)]">
                  <Key size={18} />
                </div>
                <div>
                  <h2 id="generated-key-title" class="text-base font-semibold text-[var(--fp-text)]">
                    {$tr('Client API Key Generated')}
                  </h2>
                  <p class="text-xs text-[var(--fp-muted)]">
                    {$tr('Saved to .env in API_KEYS')}
                  </p>
                </div>
              </div>
              <button
                type="button"
                class="text-[var(--fp-muted)] hover:text-[var(--fp-text)] p-1.5 rounded-lg hover:bg-[var(--fp-surface-2)] transition-colors"
                onclick={closeGeneratedKeyModal}
                aria-label={$tr('Close dialog')}
              >
                <X size={18} />
              </button>
            </div>

            <!-- Content -->
            <p class="text-sm text-[var(--fp-muted)]">
              {$tr('Use this key to authenticate clients (omp, Claude Code CLI, Cursor, curl) against this gateway.')}
            </p>

            <div class="fp-inset rounded-lg p-3.5 flex flex-col gap-2 bg-[var(--fp-surface)] border border-[var(--fp-border)]">
              <div class="flex items-center justify-between gap-2">
                <span class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider">{$tr('API Key')}</span>
                <CopyButton text={generatedKey} label="Copy Key" />
              </div>
              <code class="fp-num text-sm text-[var(--fp-accent)] break-all font-mono select-all bg-[var(--fp-surface-2)] p-2.5 rounded border border-[var(--fp-border)]">
                {generatedKey}
              </code>
            </div>

            <div class="flex items-center justify-end gap-2 pt-2 border-t border-[var(--fp-border)]">
              <Button variant="primary" size="md" onclick={closeGeneratedKeyModal}>
                {$tr('Done')}
              </Button>
            </div>
          </div>
        </div>
      {/if}
    {/if}
  {/if}
</div>
