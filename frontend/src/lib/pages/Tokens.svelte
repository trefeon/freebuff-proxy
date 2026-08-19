<script>
  import { onDestroy } from 'svelte';
  import { Key, Unlock, Zap, Plus, Trash2, Layers, Network, Server, Sparkles, RefreshCw, ExternalLink } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import { fetchAPI, postAPI } from '../utils/api.js';
  import { usePolling } from '../utils/polling.js';
  import { formatLocalDate, generateRandomApiKey } from '../utils/format.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  let newToken = $state('');
  let actionMessage = $state('');
  let actionOK = $state(true);
  let actionPending = $state(false);
  let clientKeyMessage = $state('');
  let clientKeyOK = $state(true);
  let generatingKey = $state(false);

  // OAuth wizard state (moved from Setup page: token generation belongs
  // next to the pool it feeds)
  let oauthStarting = $state(false);
  let oauthStatus = $state(null);
  let oauthTimer = $state(null);

  async function startOAuthLogin() {
    oauthStarting = true;
    oauthStatus = { message: 'Starting headless login flow...', type: 'info' };

    try {
      const res = await fetch('/admin/login/start', { method: 'POST' });
      const result = await res.json();

      if (result.fingerprint && result.login_url) {
        oauthStatus = {
          loginUrl: result.login_url,
          fingerprint: result.fingerprint,
          message: 'Open this URL in your browser to sign in:',
          type: 'pending'
        };

        clearInterval(oauthTimer);
        oauthTimer = setInterval(async () => {
          try {
            const pollRes = await fetch(`/admin/login/status?fingerprint=${encodeURIComponent(result.fingerprint)}`);
            const pollData = await pollRes.json();

            if (pollData.status === 'completed') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: `✓ Token #${pollData.token_index} added to pool and saved to .env.`,
                type: 'success'
              };
              oauthStarting = false;
              fetchData();
            } else if (pollData.status === 'error') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: `Login failed: ${pollData.message || 'unknown error'}`,
                type: 'error'
              };
              oauthStarting = false;
            }
          } catch {
            // keep polling
          }
        }, 3000);
      } else {
        oauthStatus = {
          message: result.message || 'Failed to start login wizard.',
          type: 'error'
        };
        oauthStarting = false;
      }
    } catch (e) {
      oauthStatus = { message: `Network error: ${e.message}`, type: 'error' };
      oauthStarting = false;
    }
  }

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/tokens');
      error = '';
    } catch (e) {
      error = e.message || 'Failed to fetch tokens';
    } finally {
      loading = false;
    }
  }

  // Generate a fresh client API key (sk-fb-...) and append it to API_KEYS in
  // .env — the same generator as the Config studio, moved here where the
  // token pool lives so the omp credential is created next to the upstream
  // tokens that back it.
  async function generateClientKey() {
    if (generatingKey) return;
    generatingKey = true;
    clientKeyMessage = '';
    try {
      const newKey = generateRandomApiKey();
      const cfgRes = await fetchAPI('/admin/api/config');
      const envContent = cfgRes?.env_content || '';
      const regex = /^\s*API_KEYS=(.*)$/m;
      const match = envContent.match(regex);
      const existing = match ? match[1].trim() : '';
      const updated = existing ? `${existing},${newKey}` : newKey;
      const save = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: envContent.replace(regex, `API_KEYS=${updated}`) }),
      });
      const result = await save.json();
      clientKeyOK = save.ok && result.ok;
      clientKeyMessage = clientKeyOK
        ? `Generated & saved client API key: ${newKey}`
        : (result.message || 'Failed to save client API key');
      if (clientKeyOK) {
        try { await navigator.clipboard.writeText(newKey); } catch { /* clipboard unavailable */ }
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || 'Network error generating client key';
    } finally {
      generatingKey = false;
    }
  }


  async function addToken(e) {
    e.preventDefault();
    if (!newToken.trim() || actionPending) return;
    actionPending = true;
    try {
      const result = await postAPI('/admin/tokens/add', { token: newToken.trim() });
      actionOK = result.ok !== false;
      actionMessage = result.message || (actionOK ? 'Token added successfully' : 'Failed to add token');
      if (actionOK) { newToken = ''; fetchData(); }
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || 'Network error adding token';
    } finally {
      actionPending = false;
    }
  }

  async function triggerAction(url, body, confirmMsg) {
    if (confirmMsg && !confirm(confirmMsg)) return;
    actionPending = true;
    try {
      const result = await postAPI(url, body || undefined);
      actionOK = result.ok !== false;
      actionMessage = result.message || (actionOK ? 'Action completed' : 'Action failed');
      fetchData();
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || 'Network error executing action';
    } finally {
      actionPending = false;
    }
  }

  function handleModeSwitch(targetMode) {
    if (!data) return;
    const current = data.in_bridge ? 'bridge' : data.mode === 'hybrid' ? 'hybrid' : 'pooled';
    if (current === targetMode) return;

    if (targetMode === 'pooled') {
      if (!data.has_tokens) {
        actionOK = false;
        actionMessage = 'Pooled mode requires at least one token — paste a token into the form below first.';
        return;
      }
      triggerAction('/admin/mode', { mode: 'pooled' }, 'Switch to pooled mode? All client requests will share the server pool.');
    } else if (targetMode === 'hybrid') {
      triggerAction('/admin/mode', { mode: 'hybrid' }, 'Switch to hybrid mode? Client-provided tokens relay like bridge; token-less requests use the pool.');
    } else if (targetMode === 'bridge') {
      triggerAction('/admin/mode', { mode: 'bridge' }, 'Switch to bridge mode? Pooled tokens are cleared from memory and .env; clients send their own credentials.');
    }
  }

  usePolling(fetchData, 30000);

  onDestroy(() => {
    clearInterval(oauthTimer);
  });

  function modeVariant(d) {
    if (d?.in_bridge) return 'blue';
    if (d?.mode === 'hybrid') return 'purple';
    return 'amber';
  }

  function riskVariant(risk) {
    if (risk === 'low') return 'teal';
    if (risk === 'moderate') return 'amber';
    return 'red';
  }

  function riskDot(risk) {
    if (risk === 'low') return 'bg-[var(--fp-teal)]';
    if (risk === 'moderate') return 'bg-[var(--fp-amber)]';
    return 'bg-[var(--fp-red)]';
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Tokens & Quotas" subtitle="Manage upstream credentials, runtime routing modes, and per-model quotas">
    {#if data}
      <StatusBadge variant={modeVariant(data)}>{data.mode} mode</StatusBadge>
      {#if data.in_bridge}
        <StatusBadge variant="muted" mono>{data.bridge_tokens} client{data.bridge_tokens === 1 ? '' : 's'}</StatusBadge>
      {/if}
    {/if}
  </PageHeader>

  <!-- Action Status -->
  {#if actionMessage}
    <Alert
      variant={actionOK ? 'success' : 'error'}
      message={actionMessage}
      ondismiss={() => actionMessage = ''}
    />
  {/if}

  <!-- Mode Control & Pool Routing Bar (Always Visible) -->
  <div class="fp-card p-5 space-y-4">
    <div class="flex flex-col lg:flex-row lg:items-center justify-between gap-4">
      <div>
        <div class="flex items-center gap-2">
          <Layers size={18} class="text-[var(--fp-amber)]" />
          <h2 class="text-base font-semibold text-white">
            Routing Mode & Pool Configuration
            {#if data}
              <span class="text-xs font-normal text-[var(--fp-muted)] ml-1.5">
                ({data.token_count || 0} pooled token{data.token_count === 1 ? '' : 's'})
              </span>
            {/if}
          </h2>
        </div>
        <p class="text-xs text-[var(--fp-muted)] mt-1">
          Select how the proxy routes incoming requests to upstream models. Changes apply immediately and save to <code class="px-1.5 py-0.5 rounded fp-inset text-[var(--fp-amber)] font-mono">.env</code>.
        </p>
      </div>

      <!-- Mode Selector Buttons -->
      <div class="flex flex-wrap items-center gap-2">
        <!-- Pooled Mode -->
        <button
          type="button"
          onclick={() => handleModeSwitch('pooled')}
          class="px-3.5 py-2 rounded-lg text-xs font-semibold transition-all flex items-center gap-1.5
            {!data?.in_bridge && data?.mode !== 'hybrid'
              ? 'bg-[var(--fp-amber)]/20 text-[var(--fp-amber)] border border-[var(--fp-amber)]/50 shadow-sm'
              : 'bg-[var(--fp-surface-3)] hover:bg-[var(--fp-border-bright)] text-[var(--fp-muted)] hover:text-white border border-[var(--fp-border)]'}"
          title="All requests share the server token pool"
        >
          <Server size={14} />
          <span>Pooled</span>
          {!data?.in_bridge && data?.mode !== 'hybrid' ? '✓' : ''}
        </button>

        <!-- Hybrid Mode -->
        <button
          type="button"
          onclick={() => handleModeSwitch('hybrid')}
          class="px-3.5 py-2 rounded-lg text-xs font-semibold transition-all flex items-center gap-1.5
            {data?.mode === 'hybrid'
              ? 'bg-[#AC94FA]/20 text-[#AC94FA] border border-[#AC94FA]/50 shadow-sm'
              : 'bg-[var(--fp-surface-3)] hover:bg-[var(--fp-border-bright)] text-[var(--fp-muted)] hover:text-white border border-[var(--fp-border)]'}"
          title="Relays client tokens when sent, falls back to server pool"
        >
          <Layers size={14} />
          <span>Hybrid</span>
          {data?.mode === 'hybrid' ? '✓' : ''}
        </button>

        <!-- Bridge Mode -->
        <button
          type="button"
          onclick={() => handleModeSwitch('bridge')}
          class="px-3.5 py-2 rounded-lg text-xs font-semibold transition-all flex items-center gap-1.5
            {data?.in_bridge
              ? 'bg-[#60A5FA]/20 text-[#60A5FA] border border-[#60A5FA]/50 shadow-sm'
              : 'bg-[var(--fp-surface-3)] hover:bg-[var(--fp-border-bright)] text-[var(--fp-muted)] hover:text-white border border-[var(--fp-border)]'}"
          title="Stateless mode: clients provide their own tokens"
        >
          <Network size={14} />
          <span>Bridge</span>
          {data?.in_bridge ? '✓' : ''}
        </button>

        <!-- Batch actions for pooled/hybrid -->
        {#if data?.has_tokens}
          <div class="h-6 w-[1px] bg-[var(--fp-border)] mx-1 hidden sm:block"></div>
          <button
            type="button"
            onclick={() => triggerAction('/admin/tokens/test-all', {}, '')}
            class="fp-btn-secondary text-[var(--fp-amber)] border-[var(--fp-amber)]/30"
          >
            <Zap size={13} />
            <span>Test All</span>
          </button>
          <button
            type="button"
            onclick={() => triggerAction('/admin/tokens/remove', {}, 'Remove the last token from the pool and .env?')}
            class="fp-btn-danger"
          >
            <Trash2 size={13} />
            <span>Remove Last</span>
          </button>
        {/if}
      </div>
    </div>

    <!-- Active Mode Description Banner -->
    <div class="p-3 rounded-lg fp-inset text-xs flex items-center justify-between gap-2">
      {#if data?.in_bridge}
        <span class="text-[var(--fp-muted)]">
          <strong class="text-[#60A5FA]">Bridge Mode Active:</strong> Server pool is empty. Clients must provide their own FreeBuff token per request (<code class="font-mono text-[var(--fp-text)]">Authorization: Bearer cb_...</code>).
        </span>
      {:else if data?.mode === 'hybrid'}
        <span class="text-[var(--fp-muted)]">
          <strong class="text-[#AC94FA]">Hybrid Mode Active:</strong> Client-provided tokens relay directly; token-less requests safely use the {data?.token_count || 0} server pool token(s).
        </span>
      {:else}
        <span class="text-[var(--fp-muted)]">
          <strong class="text-[var(--fp-amber)]">Pooled Mode Active:</strong> All client requests share the {data?.token_count || 0} server token(s) with automatic load rotation and anti-ban safeguards.
        </span>
      {/if}
    </div>
  </div>

  <!-- Headless OAuth Token Generator -->
  <div class="fp-card p-5 space-y-3 border-[var(--fp-amber)]/30">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2">
        <Sparkles size={18} class="text-[var(--fp-amber)]" />
        <h2 class="text-base font-semibold text-white">Headless OAuth Token Generator</h2>
      </div>
    </div>
    <p class="text-xs text-[var(--fp-muted)]">
      Generate FreeBuff credentials directly in your browser without installing the Codebuff CLI. The token is automatically verified, added to the live pool, and saved to <code class="text-[var(--fp-amber)] font-mono">.env</code>.
    </p>

    <button
      onclick={startOAuthLogin}
      disabled={oauthStarting}
      class="fp-btn-primary"
    >
      {#if oauthStarting}
        <RefreshCw size={14} class="animate-spin" />
        <span>Authorizing...</span>
      {:else}
        <Sparkles size={14} />
        <span>Generate Token via Browser Login</span>
      {/if}
    </button>

    {#if oauthStatus}
      <div class="mt-3 p-4 rounded-lg fp-inset text-xs font-mono space-y-2">
        <p class="text-white">{oauthStatus.message}</p>
        {#if oauthStatus.loginUrl}
          <div class="flex items-center gap-2">
            <a
              href={oauthStatus.loginUrl}
              target="_blank"
              rel="noopener noreferrer"
              class="px-3 py-1.5 rounded bg-[var(--fp-amber)]/10 border border-[var(--fp-amber)]/30 text-[var(--fp-amber)] hover:underline flex items-center gap-1.5"
            >
              <span>{oauthStatus.loginUrl}</span>
              <ExternalLink size={12} />
            </a>
          </div>
        {/if}
      </div>
    {/if}
  </div>

  <!-- Client API Key Card -->
  <div class="fp-card p-5 border-[var(--fp-teal)]/30">
    <h2 class="text-base font-semibold text-white mb-1">Client API Key</h2>
    <p class="text-xs text-[var(--fp-muted)] mb-3">Generate a <code class="font-mono text-[var(--fp-teal)]">sk-fb-...</code> credential for clients (<code class="font-mono">omp</code>, curl) to authenticate against this proxy. Appended to <code class="font-mono text-[var(--fp-teal)]">API_KEYS</code> in <code class="font-mono">.env</code>.</p>
    {#if clientKeyMessage}
      <Alert variant={clientKeyOK ? 'success' : 'error'} message={clientKeyMessage} dismissable={false} />
    {/if}
    <button
      onclick={generateClientKey}
      disabled={generatingKey}
      class="fp-btn-primary bg-[var(--fp-teal)] border-[var(--fp-teal)] text-[#0A0F18]"
    >
      <Key size={16} />
      <span>{generatingKey ? 'Generating...' : 'Generate Client API Key'}</span>
    </button>
  </div>

  <!-- Add Token Card -->
  <div class="fp-card p-5 border-[var(--fp-amber)]/30">
    <h2 class="text-base font-semibold text-white mb-1">Add Token to Pool</h2>
    <p class="text-xs text-[var(--fp-muted)] mb-3">Paste a FreeBuff token (<code class="font-mono text-[var(--fp-amber)]">cb_...</code>) — dynamically validated, added to pool, and saved to <code class="font-mono text-[var(--fp-amber)]">.env</code>.</p>
    <form onsubmit={addToken} class="flex flex-col sm:flex-row gap-2">
      <input
        type="text"
        bind:value={newToken}
        placeholder="Paste FreeBuff token (cb_...)"
        autocomplete="off"
        spellcheck="false"
        class="fp-input fp-input-mono flex-1"
      />
      <button
        type="submit"
        disabled={actionPending || !newToken.trim()}
        class="fp-btn-primary"
      >
        <Plus size={16} />
        <span>Add Token</span>
      </button>
    </form>
  </div>

  <!-- Token Details List -->
  <div class="space-y-4">
    {#each data?.tokens || [] as token}
      <div class="fp-card p-5 space-y-4">
        <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 border-b border-[var(--fp-border)] pb-4">
          <div class="flex items-center gap-3">
            <div class="w-3 h-3 rounded-full {riskDot(token.risk_level)}"></div>
            <h3 class="text-lg font-bold text-white font-mono">Token #{token.index}</h3>
            <StatusBadge variant={riskVariant(token.risk_level)}>{token.risk_level}</StatusBadge>
            {#if token.has_standing}
              <StatusBadge variant="blue" uppercase={false}>
                trust {token.standing_label} ({Math.round(token.standing_score)}/100)
              </StatusBadge>
            {/if}
          </div>
          <div class="flex items-center gap-2">
            {#if token.cooldown_active}
              <button
                onclick={() => triggerAction(`/admin/tokens/${token.index}/unlock`, {}, `Unlock Token ${token.index}? Only do this if the lock is stale.`)}
                class="fp-btn-secondary text-[var(--fp-teal)] border-[var(--fp-teal)]/30"
              >
                <Unlock size={12} />
                <span>Unlock</span>
              </button>
            {/if}
            <button onclick={() => triggerAction(`/admin/tokens/${token.index}/test`, {}, '')} class="fp-btn-secondary">
              <Zap size={12} />
              <span>Test</span>
            </button>
            <button onclick={() => triggerAction(`/admin/tokens/${token.index}/finish`, {}, `Finish all active runs for Token ${token.index}?`)} class="fp-btn-secondary text-[var(--fp-muted)]">
              <span>Finish Runs</span>
            </button>
          </div>
        </div>

        <!-- Stats -->
        <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 text-xs font-mono">
          <div class="p-2.5 rounded-lg fp-inset">
            <span class="text-[var(--fp-dim)] block text-[10px] uppercase">Session ID</span>
            <span class="text-white font-medium truncate block">{token.session_instance || '—'}</span>
          </div>
          <div class="p-2.5 rounded-lg fp-inset">
            <span class="text-[var(--fp-dim)] block text-[10px] uppercase">Active Runs</span>
            <span class="text-white font-medium tabular-nums">{token.active_runs}</span>
          </div>
          <div class="p-2.5 rounded-lg fp-inset">
            <span class="text-[var(--fp-dim)] block text-[10px] uppercase">Requests</span>
            <span class="text-white font-medium tabular-nums">{token.requests}</span>
          </div>
          <div class="p-2.5 rounded-lg fp-inset">
            <span class="text-[var(--fp-dim)] block text-[10px] uppercase">24h Messages</span>
            <span class="text-white font-medium tabular-nums">{token.messages_24h}{token.daily_limit > 0 ? ` / ${token.daily_limit}` : ''}</span>
          </div>
        </div>

        <!-- Quota Table -->
        {#if token.has_quota && token.quota?.length > 0}
          <div class="mt-4">
            <h4 class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider mb-2">Model Session Quota</h4>
            <div class="overflow-x-auto border border-[var(--fp-border)] rounded-lg">
              <table class="fp-table">
                <thead>
                  <tr>
                    <th scope="col">Model</th>
                    <th scope="col">Recent</th>
                    <th scope="col">Limit</th>
                    <th scope="col">Period</th>
                    <th scope="col">Reset (Local)</th>
                    <th scope="col">Entitlement</th>
                  </tr>
                </thead>
                <tbody>
                  {#each token.quota as q}
                    <tr>
                      <td class="font-bold text-white">{q.model}</td>
                      <td class="text-[var(--fp-muted)] tabular-nums">{q.recent}</td>
                      <td class="text-[var(--fp-muted)] tabular-nums">{q.limit}</td>
                      <td class="text-[var(--fp-dim)]">{q.period}</td>
                      <td class="text-[var(--fp-amber)]">
                        {formatLocalDate(q.reset_at_utc) || q.reset_at}
                        {#if q.resets_in}
                          <span class="ml-1 text-xs opacity-75 font-sans">({q.resets_in})</span>
                        {/if}
                      </td>
                      <td class="text-[var(--fp-muted)]">{q.has_entitlement ? q.entitled : '—'}</td>
                    </tr>
                    {#if q.has_bar}
                      <tr class="bg-[var(--fp-input-bg)]/50">
                        <td colspan="6" class="px-2.5 py-1">
                          <div class="w-full bg-[var(--fp-surface)] h-1.5 rounded-full overflow-hidden">
                            <div class="h-full transition-all {q.near_limit ? 'bg-[var(--fp-red)]' : 'bg-[var(--fp-teal)]'}" style="width: {Math.min(q.usage_pct, 100)}%"></div>
                          </div>
                        </td>
                      </tr>
                    {/if}
                  {/each}
                </tbody>
              </table>
            </div>
          </div>
        {/if}
      </div>
    {/each}
  </div>
</div>
