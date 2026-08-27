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
    Eye,
    EyeOff,
    X,
  } from '@lucide/svelte';
  import Button from '../components/Button.svelte';
  import Card from '../components/Card.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import { fetchAPI, postAPI } from '../api/client.js';
  import { usePolling } from '../utils/polling.js';
  import { formatLocalDate, generateRandomApiKey } from '../utils/format.js';
  import { tr } from '../i18n.js';

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
  let deletingKey = $state('');
  let visibleKeys = $state({});
  let showGeneratedModal = $state(false);
  let modalEl = $state(null);
  let lastFocusedEl = null;

  function toggleKeyVisibility(key) {
    visibleKeys[key] = !visibleKeys[key];
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

  function banBadge(token) {
    if (token.ban_type === 'hard') {
      return { label: $tr('banned — appeal required'), tone: 'critical', pulse: true };
    }
    if (token.ban_type === 'temporary') {
      const until = formatLocalDate(token.banned_until);
      return { label: until ? $tr('banned until {time}', { time: until }) : $tr('banned (temporary)'), tone: 'bad' };
    }
    return null;
  }

  function statusFor(token) {
    const ban = banBadge(token);
    if (ban) return ban;
    if (token.locked) return { label: $tr('locked'), tone: 'warn' };
    if (token.cooldown_active) return { label: $tr('cooldown'), tone: 'warn' };
    const s = token.session_status || '';
    if (s === 'active') return { label: $tr('leased'), tone: 'good', pulse: true };
    if (s === 'queued') return { label: $tr('queued'), tone: 'info' };
    if (s === 'banned') return { label: $tr('banned'), tone: 'bad' };
    return { label: $tr('idle'), tone: 'idle' };
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
      error = e.message || $tr('Failed to fetch tokens');
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
      actionMessage = result.message || (actionOK ? $tr('Token added successfully') : $tr('Failed to add token'));
      if (actionOK) {
        newToken = '';
        fetchData();
      }
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || $tr('Network error adding token');
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
      actionMessage = result.message || (actionOK ? $tr('Action completed') : $tr('Action failed'));
      fetchData();
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || $tr('Network error executing action');
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

  async function startOAuthLogin() {
    oauthStarting = true;
    oauthStatus = { message: $tr('Starting headless login flow…'), type: 'info' };

    try {
      const res = await fetch('/admin/login/start', { method: 'POST' });
      const result = await res.json();

      if (result.fingerprint && result.login_url) {
        oauthStatus = {
          loginUrl: result.login_url,
          fingerprint: result.fingerprint,
          message: $tr('Open this URL in your browser to sign in:'),
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
                message: $tr('Token #{idx} added to pool and saved to .env.', { idx: pollData.token_index }),
                type: 'success',
              };
              oauthStarting = false;
              fetchData();
            } else if (pollData.status === 'error') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: $tr('Login failed: {message}', { message: pollData.message || $tr('unknown error') }),
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
          message: result.message || $tr('Failed to start login wizard.'),
          type: 'error',
        };
        oauthStarting = false;
      }
    } catch (e) {
      oauthStatus = { message: $tr('Network error: {message}', { message: e.message }), type: 'error' };
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

<svelte:window onkeydown={showGeneratedModal ? handleModalKeydown : undefined} />

<div class="page-enter">
  <div class="flex flex-col gap-6">
    <PageHeader title={$tr('Tokens')} description={$tr('Upstream credentials, device login, client API keys, and per-token session quotas')} />

    {#if actionMessage}
      <Alert tone={actionOK ? 'success' : 'error'} title={actionMessage} />
    {/if}
    {#if error}
      <Alert tone="error" title={error}>
        <Button variant="ghost" size="sm" onclick={() => { error = ''; fetchData(); }}>
          {$tr('Retry')}
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
            <CopyButton text={oauthStatus.loginUrl} label={$tr('Copy link')} />
            <a
              href={oauthStatus.loginUrl}
              target="_blank"
              rel="noopener noreferrer"
              class="inline-flex items-center gap-1 text-xs text-[var(--fp-accent)] hover:underline"
            >
              {$tr('Open')}
              <ExternalLink size={12} />
            </a>
          </div>
        {/if}
      </Alert>
    {/if}

    <!-- Add token form -->
    <Card title={$tr('Add Token to Pool')} description={$tr('Paste a FreeBuff auth token (cb_…) to add it to the shared pool and save it to .env. Adding burns no quota.')}>
      {#snippet actions()}
        <Button variant="secondary" size="sm" onclick={startOAuthLogin} disabled={oauthStarting}>
          {#if oauthStarting}
            <RefreshCw size={14} class="animate-spin" />
            <span>{$tr('Authorizing…')}</span>
          {:else}
            <LogIn size={14} />
            <span>{$tr('Device Login')}</span>
          {/if}
        </Button>
      {/snippet}
      <form onsubmit={addToken} class="flex flex-col gap-1.5">
        <label for="add-token-input" class="text-xs text-[var(--fp-muted)]">{$tr('Token')}</label>
        <div class="flex flex-col sm:flex-row gap-2 sm:items-end">
          <div class="flex-1 flex flex-col gap-1.5">
            <input
              id="add-token-input"
              type="text"
              bind:value={newToken}
              placeholder="cb_…"
              autocomplete="off"
              spellcheck="false"
              class="fp-input fp-num w-full"
              aria-describedby="add-token-input-hint"
              aria-invalid={tokenValid === false ? 'true' : undefined}
            />
          </div>
          <Button
            type="submit"
            variant="primary"
            disabled={adding || !newToken.trim() || tokenValid === false}
            loading={adding}
            class="shrink-0 self-end"
          >
            <Plus size={16} />
            <span>{$tr('Add Token')}</span>
          </Button>
        </div>
        <p id="add-token-input-hint" class="text-[11px] text-[var(--fp-dim)]">{$tr('Format: cb_…')}</p>
      </form>
    </Card>

    <!-- Client API-key management -->
    <Card
      title={$tr('Client API Keys')}
      description={$tr('sk-fb-… credentials for clients (omp, curl) to authenticate against this proxy. Stored in the API_KEYS line of .env.')}
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
      {/if}
      {#if clientKeyMessage}
        <Alert tone={clientKeyOK ? 'success' : 'error'} title={clientKeyMessage} />
      {/if}
    </Card>

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
            {$tr('Use this key to authenticate clients (omp, Claude Code CLI, Cursor, curl) against this proxy.')}
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

    <!-- Token table -->
    <Card
      title={$tr('Pool Tokens')}
      description={data ? $tr('{count} pooled token(s)', { count: data.token_count || 0 }) : ''}
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
          title={$tr('Could not load tokens')}
          description={error}
        >
          {#snippet action()}
            <Button variant="secondary" onclick={() => { error = ''; fetchData(); }}>
              {$tr('Retry')}
            </Button>
          {/snippet}
        </EmptyState>
      {:else if !data?.tokens || data.tokens.length === 0}
        <EmptyState
          title={$tr('No tokens in pool')}
          description={$tr('Add one above or use Device Login to generate credentials via browser.')}
        />
      {:else}
        <table class="fp-table w-full">
          <caption class="sr-only">{$tr('Pool tokens — token, status, instance, cooldown and actions')}</caption>
          <thead>
            <tr>
              <th scope="col" class="w-8"><span class="sr-only">{$tr('Expand')}</span></th>
              <th scope="col">{$tr('Token')}</th>
              <th scope="col">{$tr('Status')}</th>
              <th scope="col">{$tr('Instance')}</th>
              <th scope="col" class="num">{$tr('Cooldown')}</th>
              <th scope="col" class="text-right">{$tr('Actions')}</th>
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
                        onclick={() => triggerAction(`/admin/tokens/${idx}/unlock`, {}, $tr('Clear cooldown for token {idx}? Only do this if the lock is stale.', { idx }))}
                      >
                        <Unlock size={13} />
                        <span>{$tr('Clear')}</span>
                      </Button>
                    {/if}
                    {#if token.locked}
                      <Button
                        variant="secondary"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerAction(`/admin/tokens/${idx}/unlock-lock`, {}, $tr('Unlock token {idx}?', { idx }))}
                      >
                        <Unlock size={13} />
                        <span>{$tr('Unlock')}</span>
                      </Button>
                    {:else}
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerAction(`/admin/tokens/${idx}/lock`, {}, $tr('Lock token {idx}?', { idx }))}
                      >
                        <Lock size={13} />
                        <span>{$tr('Lock')}</span>
                      </Button>
                    {/if}
                    <Button
                      variant="danger"
                      size="sm"
                      disabled={actionPending}
                      onclick={() => triggerAction('/admin/tokens/remove', { token: token.index ?? i }, $tr('Remove token {idx} from the pool and .env?', { idx }))}
                    >
                      <Trash2 size={13} />
                      <span>{$tr('Remove')}</span>
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
                          <span>{$tr('Active Session:')} <code class="fp-num">{token.session_model}</code></span>
                          <span class="fp-num">{Math.floor(token.session_remaining_seconds / 60)}m {token.session_remaining_seconds % 60}s remaining</span>
                        </div>
                      {/if}
                      {#if token.has_standing}
                        <!-- Standing / trust block (issue #140 P3d): level,
                             score progress toward the next level, the cap
                             holding the account (capped_by), and upstream's
                             suggested earn-back actions. -->
                        <div class="mb-2 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
                          <div class="flex items-center justify-between gap-2 mb-1">
                            <p class="text-xs text-[var(--fp-muted)] uppercase tracking-wider font-semibold">{$tr('Account standing')}</p>
                            <span class="text-xs text-[var(--fp-text)] font-semibold">{token.standing_label || token.standing_level}</span>
                          </div>
                          {#if token.standing_score != null && token.standing_next_level}
                            <div class="flex items-center gap-2">
                              <div class="h-1.5 flex-1 rounded bg-[var(--fp-bg)] overflow-hidden">
                                <div
                                  class="h-full rounded bg-[var(--fp-accent)]"
                                  style={`width: ${Math.min(100, Math.max(0, token.standing_score))}%`}
                                ></div>
                              </div>
                              <span class="fp-num text-xs text-[var(--fp-muted)] shrink-0">
                                {$tr('score {score} → next: {level}', { score: token.standing_score, level: token.standing_next_level })}
                              </span>
                            </div>
                          {:else if token.standing_score != null}
                            <span class="fp-num text-xs text-[var(--fp-muted)]">{$tr('score {score}', { score: token.standing_score })}</span>
                          {/if}
                          {#if token.standing_blurb}
                            <p class="text-xs text-[var(--fp-dim)] mt-1">{token.standing_blurb}</p>
                          {/if}
                          {#if token.standing_capped_by}
                            <p class="text-xs mt-1 text-[var(--fp-warning)]">
                              {$tr('Capped by')} <code class="fp-num">{token.standing_capped_by}</code>{#if token.standing_capped_reason}: {token.standing_capped_reason}{/if}
                            </p>
                          {/if}
                          {#if token.standing_next_steps?.length > 0}
                            <ul class="mt-1.5 flex flex-col gap-1">
                              {#each token.standing_next_steps as step}
                                <li class="text-xs text-[var(--fp-text)] flex items-start gap-1.5">
                                  <span class="fp-num text-[var(--fp-accent)] shrink-0">+{step.points}</span>
                                  <span>
                                    {step.label}{#if step.detail} — <span class="text-[var(--fp-dim)]">{step.detail}</span>{/if}
                                    {#if step.href}
                                      <a href={step.href} target="_blank" rel="noopener noreferrer" class="ml-1 text-[var(--fp-accent)] hover:underline inline-flex items-center gap-0.5">
                                        <ExternalLink size={10} />
                                      </a>
                                    {/if}
                                  </span>
                                </li>
                              {/each}
                            </ul>
                          {/if}
                        </div>
                      {/if}
                      {#if token.has_quota && token.quota?.length > 0}
                        <div class="flex flex-col gap-2">
                          <p class="text-xs text-[var(--fp-muted)] uppercase tracking-wider font-semibold">{$tr('Session quotas')}</p>
                          {#each token.quota as q}
                            <div class="flex flex-col sm:flex-row sm:items-center gap-1 sm:gap-4 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
                              <code class="fp-num text-xs text-[var(--fp-text)] sm:w-48 shrink-0 truncate">{q.model}</code>
                              <span class="fp-num text-xs text-[var(--fp-muted)]">
                                <span class="text-[var(--fp-text)]">{q.recent}</span> / {q.limit}
                                {#if q.limit !== '0' && q.limit !== ''}
                                  {$tr('(remaining {count})', { count: Math.max(0, parseFloat(q.limit) - parseFloat(q.recent)) })}
                                {/if}
                              </span>
                              <span class="fp-num text-xs text-[var(--fp-dim)] sm:ml-auto">
                                {q.period}{#if q.has_entitlement} · {$tr('entitled')} {q.entitled}{/if}
                              </span>
                              <span class="fp-num text-xs text-[var(--fp-dim)]">
                                {#if q.resets_in}
                                  {$tr('reset')} {formatLocalDate(q.reset_at_utc) || q.reset_at} ({q.resets_in})
                                {:else}
                                  {$tr('reset')} {formatLocalDate(q.reset_at_utc) || q.reset_at}
                                {/if}
                              </span>
                            </div>
                          {/each}
                        </div>
                      {:else}
                        <p class="text-xs text-[var(--fp-dim)] italic">{$tr('No quota data available for this session.')}</p>
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
