<script>
  import { onDestroy } from 'svelte';
  import {
    LogIn,
    Key,
    Lock,
    Plus,
    Trash2,
    Unlock,
    ExternalLink,
    ChevronDown,
    ChevronRight,
    RefreshCw,
  } from '@lucide/svelte';
  import Button from '../components/Button.svelte';
  import Card from '../components/Card.svelte';
  import Field from '../components/Field.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import { fetchAPI, postAPI } from '../api/client.js';
  import { usePolling } from '../utils/polling.js';
  import { formatLocalDate, generateRandomApiKey } from '../utils/format.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  // Add-token form
  let newToken = $state('');
  let adding = $state(false);
  let actionMessage = $state('');
  let actionOK = $state(true);

  // Client API-key management (API_KEYS in .env)
  let apiKeys = $state([]);
  let clientKeyMessage = $state('');
  let clientKeyOK = $state(true);
  let generatingKey = $state(false);
  let generatedKey = $state('');

  // Device login flow
  let oauthStarting = $state(false);
  let oauthStatus = $state(null);
  let oauthTimer = $state(null);

  // Token table
  let expandedToken = $state(null);
  let actionPending = $state(false);
  let now = $state(Date.now());

  const tokenValid = $derived(
    newToken.trim() === ''
      ? null
      : /^cb_[A-Za-z0-9_-]{20,}$/.test(newToken.trim())
  );

  function statusFor(token) {
    if (token.locked) return { label: 'locked', tone: 'warn' };
    if (token.cooldown_active) return { label: 'cooldown', tone: 'warn' };
    const s = token.session_status || '';
    if (s === 'active') return { label: 'leased', tone: 'good', pulse: true };
    if (s === 'queued') return { label: 'queued', tone: 'info' };
    if (s === 'banned') return { label: 'banned', tone: 'bad' };
    return { label: 'idle', tone: 'idle' };
  }

  function cooldownLabel(token) {
    if (!token.cooldown_active || !token.cooldown_until) return '—';
    const ms = new Date(token.cooldown_until).getTime() - now;
    if (ms <= 0) return 'expiring';
    const s = Math.floor(ms / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${sec}s`;
    return `${sec}s`;
  }

  function cooldownTone(token) {
    if (!token.cooldown_until) return 'default';
    const ms = new Date(token.cooldown_until).getTime() - now;
    if (ms >= 0 && ms < 5 * 60_000) return 'warn';
    return 'default';
  }

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/tokens');
      try {
        const cfgRes = await fetchAPI('/admin/api/config');
        const envContent = cfgRes?.env_content || '';
        const m = envContent.match(/^\s*API_KEYS=(.*)$/m);
        const val = m ? m[1].trim() : '';
        apiKeys = val ? val.split(',').map((s) => s.trim()).filter(Boolean) : [];
      } catch {
        apiKeys = [];
      }
      error = '';
    } catch (e) {
      error = e.message || 'Failed to fetch tokens';
    } finally {
      loading = false;
    }
  }

  async function addToken(e) {
    e.preventDefault();
    if (!newToken.trim() || tokenValid === false || adding) return;
    adding = true;
    try {
      const result = await postAPI('/admin/tokens/add', { token: newToken.trim() });
      actionOK = result.ok !== false;
      actionMessage = result.message || (actionOK ? 'Token added successfully' : 'Failed to add token');
      if (actionOK) {
        newToken = '';
        fetchData();
      }
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || 'Network error adding token';
    } finally {
      adding = false;
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
      const save = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: envContent.replace(regex, `API_KEYS=${updated}`) }),
      });
      const result = await save.json();
      clientKeyOK = save.ok && result.ok;
      clientKeyMessage = clientKeyOK
        ? `Generated & saved client API key`
        : (result.message || 'Failed to save client API key');
      if (clientKeyOK) {
        generatedKey = newKey;
        fetchData();
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || 'Network error generating client key';
    } finally {
      generatingKey = false;
    }
  }

  async function startOAuthLogin() {
    oauthStarting = true;
    oauthStatus = { message: 'Starting headless login flow…', type: 'info' };

    try {
      const res = await fetch('/admin/login/start', { method: 'POST' });
      const result = await res.json();

      if (result.fingerprint && result.login_url) {
        oauthStatus = {
          loginUrl: result.login_url,
          fingerprint: result.fingerprint,
          message: 'Open this URL in your browser to sign in:',
          type: 'pending',
        };

        clearInterval(oauthTimer);
        oauthTimer = setInterval(async () => {
          try {
            const pollRes = await fetch(`/admin/login/status?fingerprint=${encodeURIComponent(result.fingerprint)}`);
            const pollData = await pollRes.json();

            if (pollData.status === 'completed') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: `Token #${pollData.token_index} added to pool and saved to .env.`,
                type: 'success',
              };
              oauthStarting = false;
              fetchData();
            } else if (pollData.status === 'error') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: `Login failed: ${pollData.message || 'unknown error'}`,
                type: 'error',
              };
              oauthStarting = false;
            }
          } catch {
            // transient poll failure — keep polling
          }
        }, 3000);
      } else {
        oauthStatus = {
          message: result.message || 'Failed to start login wizard.',
          type: 'error',
        };
        oauthStarting = false;
      }
    } catch (e) {
      oauthStatus = { message: `Network error: ${e.message}`, type: 'error' };
      oauthStarting = false;
    }
  }

  function toggleExpand(idx) {
    expandedToken = expandedToken === idx ? null : idx;
  }

  usePolling(fetchData, 10000);
  const tick = setInterval(() => { now = Date.now(); }, 1000);

  onDestroy(() => {
    clearInterval(oauthTimer);
    clearInterval(tick);
  });
</script>

<div class="page-enter">
  <div class="flex flex-col gap-6">
    <PageHeader title="Tokens" description="Upstream credentials, device login, client API keys, and per-token session quotas">
      {#snippet actions()}
        <Button
          variant="secondary"
          size="sm"
          onclick={startOAuthLogin}
          disabled={oauthStarting}
        >
          {#if oauthStarting}
            <RefreshCw size={14} class="animate-spin" />
            <span>Authorizing…</span>
          {:else}
            <LogIn size={14} />
            <span>Device Login</span>
          {/if}
        </Button>
        <Button
          variant="primary"
          size="sm"
          onclick={generateClientKey}
          disabled={generatingKey}
        >
          {#if generatingKey}
            <RefreshCw size={14} class="animate-spin" />
            <span>Generating…</span>
          {:else}
            <Key size={14} />
            <span>Generate API Key</span>
          {/if}
        </Button>
      {/snippet}
    </PageHeader>

    {#if actionMessage}
      <Alert tone={actionOK ? 'success' : 'error'} title={actionMessage} />
    {/if}
    {#if error}
      <Alert tone="error" title={error}>
        <Button variant="ghost" size="sm" onclick={() => { error = ''; fetchData(); }}>
          Retry
        </Button>
      </Alert>
    {/if}

    {#if oauthStatus}
      <Alert
        tone={oauthStatus.type === 'success' ? 'success' : oauthStatus.type === 'error' ? 'error' : 'info'}
        title={oauthStatus.message}
      >
        {#if oauthStatus.loginUrl}
          <div class="flex flex-wrap items-center gap-2">
            <code class="fp-num text-xs break-all max-w-full">{oauthStatus.loginUrl}</code>
            <CopyButton text={oauthStatus.loginUrl} label="Copy link" />
            <a
              href={oauthStatus.loginUrl}
              target="_blank"
              rel="noopener noreferrer"
              class="inline-flex items-center gap-1 text-xs text-[var(--fp-accent)] hover:underline"
            >
              Open
              <ExternalLink size={12} />
            </a>
          </div>
        {/if}
      </Alert>
    {/if}

    <!-- Add token form -->
    <Card title="Add Token to Pool" description="Paste a FreeBuff auth token (cb_…) to add it to the shared pool and save it to .env. Adding burns no quota.">
      <form onsubmit={addToken} class="flex flex-col sm:flex-row items-start sm:items-end gap-3">
        <div class="flex-1 w-full">
          <Field
            label="Token"
            hint={tokenValid === true ? 'Valid format' : tokenValid === false ? 'Invalid format' : 'Format: cb_…'}
            error={tokenValid === false ? 'Token must match cb_… with at least 20 characters' : ''}
            id="add-token-input"
          >
            <input
              id="add-token-input"
              type="text"
              bind:value={newToken}
              placeholder="cb_…"
              autocomplete="off"
              spellcheck="false"
              class="fp-input fp-num w-full"
            />
          </Field>
        </div>
        <Button
          type="submit"
          variant="primary"
          disabled={adding || !newToken.trim() || tokenValid === false}
          loading={adding}
        >
          <Plus size={16} />
          <span>Add Token</span>
        </Button>
      </form>
    </Card>

    <!-- Client API-key management -->
    <Card
      title="Client API Keys"
      description="sk-fb-… credentials for clients (omp, curl) to authenticate against this proxy. Stored in the API_KEYS line of .env."
    >
      {#if generatedKey}
        <div class="fp-inset rounded p-3 mb-3 flex flex-wrap items-center gap-2">
          <span class="text-xs text-[var(--fp-muted)]">New key:</span>
          <code class="fp-num text-xs text-[var(--fp-accent)] break-all">{generatedKey}</code>
          <CopyButton text={generatedKey} label="Copy" />
        </div>
      {/if}
      {#if apiKeys.length > 0}
        <div class="flex flex-col gap-2 mb-3">
          {#each apiKeys as key}
            <div class="fp-inset rounded flex items-center justify-between gap-2 px-3 py-2">
              <code class="fp-num text-xs truncate">{key}</code>
              <CopyButton text={key} label="Copy" />
            </div>
          {/each}
        </div>
      {/if}
      {#if clientKeyMessage}
        <Alert tone={clientKeyOK ? 'success' : 'error'} title={clientKeyMessage} />
      {/if}
    </Card>

    <!-- Token table -->
    <Card
      title="Pool Tokens"
      description={data ? `${data.token_count || 0} pooled token${data.token_count === 1 ? '' : 's'}` : ''}
      pad="none"
    >
      {#if loading}
        <div class="p-4 flex flex-col gap-3">
          <div class="skeleton skeleton-text w-1/3"></div>
          <div class="skeleton skeleton-line"></div>
          <div class="skeleton skeleton-line"></div>
          <div class="skeleton skeleton-line"></div>
          <div class="skeleton skeleton-line"></div>
        </div>
      {:else if error}
        <EmptyState
          title="Could not load tokens"
          description={error}
        >
          {#snippet action()}
            <Button variant="secondary" onclick={() => { error = ''; fetchData(); }}>
              Retry
            </Button>
          {/snippet}
        </EmptyState>
      {:else if !data?.tokens || data.tokens.length === 0}
        <EmptyState
          title="No tokens in pool"
          description="Add one above or use Device Login to generate credentials via browser."
        />
      {:else}
        <table class="fp-table w-full">
          <thead>
            <tr>
              <th class="w-8"></th>
              <th>Token</th>
              <th>Status</th>
              <th>Instance</th>
              <th class="num">Cooldown</th>
              <th class="text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#each data.tokens as token, i (token.index ?? i)}
              {@const idx = token.index ?? i}
              {@const st = statusFor(token)}
              {@const isExpanded = expandedToken === idx}
              <tr>
                <td class="w-8">
                  <button
                    type="button"
                    onclick={() => toggleExpand(idx)}
                    aria-expanded={isExpanded}
                    aria-label={isExpanded ? `Collapse quotas for token ${idx}` : `Expand quotas for token ${idx}`}
                    class="inline-flex items-center justify-center w-6 h-6 text-[var(--fp-dim)] hover:text-[var(--fp-text)]"
                  >
                    {#if isExpanded}
                      <ChevronDown size={16} />
                    {:else}
                      <ChevronRight size={16} />
                    {/if}
                  </button>
                </td>
                <td>
                  <span class="fp-num text-xs text-[var(--fp-text)]">#{idx}</span>
                </td>
                <td>
                  <StatusBadge status={st.label} tone={st.tone} pulse={st.pulse} />
                </td>
                <td>
                  {#if token.session_instance}
                    <span class="inline-flex items-center gap-2">
                      <code class="fp-num text-xs text-[var(--fp-muted)]">{token.session_instance}</code>
                      <CopyButton text={token.session_instance} label="Copy" />
                    </span>
                  {:else}
                    <span class="text-xs text-[var(--fp-dim)]">—</span>
                  {/if}
                </td>
                <td class="num">
                  {#if token.cooldown_active}
                    <span class="fp-num text-xs {cooldownTone(token) === 'warn' ? 'text-[var(--fp-warning)]' : 'text-[var(--fp-text)]'}">
                      {cooldownLabel(token)}
                    </span>
                  {:else}
                    <span class="fp-num text-xs text-[var(--fp-dim)]">—</span>
                  {/if}
                </td>
                <td class="text-right">
                  <div class="inline-flex items-center gap-1.5 justify-end">
                    {#if token.cooldown_active}
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerAction(`/admin/tokens/${idx}/unlock`, {}, `Clear cooldown for token ${idx}? Only do this if the lock is stale.`)}
                      >
                        <Unlock size={13} />
                        <span>Clear</span>
                      </Button>
                    {/if}
                    {#if token.locked}
                      <Button
                        variant="secondary"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerAction(`/admin/tokens/${idx}/unlock-lock`, {}, `Unlock token ${idx}?`)}
                      >
                        <Unlock size={13} />
                        <span>Unlock</span>
                      </Button>
                    {:else}
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerAction(`/admin/tokens/${idx}/lock`, {}, `Lock token ${idx}?`)}
                      >
                        <Lock size={13} />
                        <span>Lock</span>
                      </Button>
                    {/if}
                    <Button
                      variant="danger"
                      size="sm"
                      disabled={actionPending}
                      onclick={() => triggerAction('/admin/tokens/remove', { token: token.index ?? i }, `Remove token ${idx} from the pool and .env?`)}
                    >
                      <Trash2 size={13} />
                      <span>Remove</span>
                    </Button>
                  </div>
                </td>
              </tr>
              {#if isExpanded}
                <tr>
                  <td colspan="6" class="!p-0">
                    <div class="fp-inset m-2 rounded p-3">
                      {#if token.session_remaining_seconds > 0 && token.session_model}
                        <div class="mb-2 px-2 py-1 rounded bg-[var(--fp-accent)]/10 text-xs text-[var(--fp-accent)] flex items-center justify-between">
                          <span>Active Session: <code class="fp-num">{token.session_model}</code></span>
                          <span class="fp-num">{Math.floor(token.session_remaining_seconds / 60)}m {token.session_remaining_seconds % 60}s remaining</span>
                        </div>
                      {/if}
                      {#if token.has_quota && token.quota?.length > 0}
                        <div class="flex flex-col gap-2">
                          <p class="text-xs text-[var(--fp-muted)] uppercase tracking-wider font-semibold">Session quotas</p>
                          {#each token.quota as q}
                            <div class="flex flex-col sm:flex-row sm:items-center gap-1 sm:gap-4 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
                              <code class="fp-num text-xs text-[var(--fp-text)] sm:w-48 shrink-0 truncate">{q.model}</code>
                              <span class="fp-num text-xs text-[var(--fp-muted)]">
                                <span class="text-[var(--fp-text)]">{q.recent}</span> / {q.limit}
                                {#if q.limit !== '0' && q.limit !== ''}
                                  (remaining <span class="text-[var(--fp-text)]">{Math.max(0, parseFloat(q.limit) - parseFloat(q.recent))}</span>)
                                {/if}
                              </span>
                              <span class="fp-num text-xs text-[var(--fp-dim)] sm:ml-auto">
                                {q.period}{#if q.has_entitlement} · entitled {q.entitled}{/if}
                              </span>
                              <span class="fp-num text-xs text-[var(--fp-dim)]">
                                {#if q.resets_in}
                                  reset {formatLocalDate(q.reset_at_utc) || q.reset_at} ({q.resets_in})
                                {:else}
                                  reset {formatLocalDate(q.reset_at_utc) || q.reset_at}
                                {/if}
                              </span>
                            </div>
                          {/each}
                        </div>
                      {:else}
                        <p class="text-xs text-[var(--fp-dim)] italic">No quota data available for this session.</p>
                      {/if}
                    </div>
                  </td>
                </tr>
              {/if}
            {/each}
          </tbody>
        </table>
      {/if}
    </Card>
  </div>
</div>
