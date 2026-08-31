<script>
  import { onDestroy, onMount } from 'svelte';
  import {
    LogIn,
    Plus,
    ExternalLink,
    RefreshCw,
  } from '@lucide/svelte';
  import Button from '../components/Button.svelte';
  import Card from '../components/Card.svelte';
  import Field from '../components/Field.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import TokenCard from '../components/TokenCard.svelte';
  import BridgeTokenCard from '../components/BridgeTokenCard.svelte';
  import { fetchAPI, postAPI, postForm, csrfHeader } from '../api/client.js';
  import { adminApi, adminActions, tokenActions } from '../api/paths.js';
  import { usePolling } from '../utils/polling.js';
  import { useEventStream } from '../utils/events.js';
  import { tr } from '../i18n.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');
  let unsubEvents = null;

  // Add-token form
  let newToken = $state('');
  let adding = $state(false);
  let actionMessage = $state('');
  let actionOK = $state(true);
  // Dev Tools surfaces (per-token session spawn toolbar) are hidden unless
  // the operator enables DEVTOOLS_ENABLED=true in .env (same gate as the
  // sidebar's Dev Tools tab and the server-side DevTools route).
  let devToolsEnabled = $state(false);
  // Token rotation strategy (TOKEN_ROTATION in .env)
  let tokenRotation = $state('drain');
  let savingRotation = $state(false);

  async function setTokenRotation(newMode) {
    if (savingRotation || tokenRotation === newMode) return;
    savingRotation = true;
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const regex = /^\s*TOKEN_ROTATION=(.*)$/m;
      const match = envContent.match(regex);
      const newContent = match
        ? envContent.replace(regex, `TOKEN_ROTATION=${newMode}`)
        : (envContent ? `${envContent}\nTOKEN_ROTATION=${newMode}` : `TOKEN_ROTATION=${newMode}`);
      const save = await postForm(adminActions.configSave, { content: newContent });
      if (save.ok) {
        tokenRotation = newMode;
      }
    } catch (e) {
      console.warn('Failed to update token rotation', e);
    } finally {
      savingRotation = false;
    }
  }

  // Device login flow
  let oauthStarting = $state(false);
  let oauthStatus = $state(null);
  let oauthTimer = null;

  // Token table
  let expandedToken = $state(null);
  let spawnModels = $state({});
  let actionPending = $state(false);
  let now = $state(Date.now());

  const tokenValid = $derived(
    newToken.trim() === ''
      ? null
      : !newToken.trim().toLowerCase().startsWith('bearer ') &&
        !/[,\s]/.test(newToken.trim()) &&
        newToken.trim().length >= 10
  );

  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.tokens);
      // Seed the per-token spawn-model map so no TokenCard binding ever sees
      // undefined — Svelte 5 rejects bind:spawnModel={undefined} for a prop
      // with a fallback (props_invalid_value) and unmounts the table.
      (data?.tokens ?? []).forEach((t, i) => {
        const idx = t.index ?? i;
        if (!(idx in spawnModels)) spawnModels[idx] = '';
      });
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
      const result = await postAPI(adminActions.tokenAdd, { token: newToken.trim() });
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

  function handleTokenAction(token, idx, action) {
    switch (action) {
      case 'clear':
        return triggerAction(tokenActions.unlock(idx), {}, $tr('Clear cooldown for token {idx}? Only do this if the lock is stale.', { idx }));
      case 'unlock':
        return triggerAction(tokenActions.unlockLock(idx), {}, $tr('Unlock token {idx}?', { idx }));
      case 'lock':
        return triggerAction(tokenActions.lock(idx), {}, $tr('Lock token {idx}?', { idx }));
      case 'remove':
        return triggerAction(adminActions.tokenRemove, { token: idx }, $tr('Remove token {idx} from the pool and .env?', { idx }));
      default:
        return;
    }
  }

  function handleSpawn(idx, model) {
    const m = model || 'mimo/mimo-v2.5';
    triggerAction(tokenActions.session(idx), { model: m }, $tr('Create upstream session for token #{idx} on {model}?', { idx, model: m }));
  }

  function handleRefresh(idx, action) {
    if (action === 'probe') {
      return triggerAction(tokenActions.test(idx), {}, $tr('Probe token #{idx} against upstream?', { idx }));
    }
    return triggerAction(tokenActions.finish(idx), {}, $tr('Finish active runs on token #{idx}?', { idx }));
  }

  async function startOAuthLogin() {
    oauthStarting = true;
    oauthStatus = { message: $tr('Starting headless login flow…'), type: 'info' };

    try {
      const res = await fetch(adminActions.loginStart, { method: 'POST', headers: csrfHeader('POST') });
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
            const pollRes = await fetch(`${adminApi.loginStatus}?fingerprint=${encodeURIComponent(result.fingerprint)}`);
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
  onMount(async () => {
    unsubEvents = useEventStream({
      onTokens: (freshData) => {
        data = freshData;
        loading = false;
        error = '';
      },
    });
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      const m = envContent.match(/^\s*DEVTOOLS_ENABLED=(.*)$/m);
      const val = m ? m[1].trim().toLowerCase() : '';
      devToolsEnabled = val === 'true' || val === '1';

      const mRot = envContent.match(/^\s*TOKEN_ROTATION=(.*)$/m);
      const rotVal = mRot ? mRot[1].trim().toLowerCase() : 'drain';
      tokenRotation = ['drain', 'round_robin', 'least_used', 'random'].includes(rotVal) ? rotVal : 'drain';
    } catch {
      devToolsEnabled = false;
      tokenRotation = 'drain';
    }
  });

  onDestroy(() => {
    unsubEvents?.();
    clearInterval(oauthTimer);
    clearInterval(tick);
  });
</script>

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
          <div class="flex flex-col gap-2 mt-2">
            <div class="flex flex-wrap items-center gap-2">
              <code class="fp-num text-xs break-all max-w-full">{oauthStatus.loginUrl}</code>
              <CopyButton text={oauthStatus.loginUrl} label={$tr('Copy link')} />
              <a
                href={oauthStatus.loginUrl}
                target="_blank"
                rel="noopener noreferrer"
                class="inline-flex items-center gap-1 text-xs text-[var(--fp-accent)] hover:underline font-medium"
              >
                {$tr('Open in New Tab')}
                <ExternalLink size={12} />
              </a>
            </div>
            <p class="text-xs text-[var(--fp-dim)]">
              {$tr('Tip: To add a different FreeBuff account, open this link in an Incognito / Private window so your browser does not reuse an existing GitHub session.')}
            </p>
          </div>
        {/if}
      </Alert>
    {/if}

    <!-- Add token form -->
    <Card
      title={$tr('Add Token to Pool')}
      description={$tr('Paste a FreeBuff auth token (from credentials.json or CLI) to add it to the shared pool and save it to .env. Adding burns no quota.')}
    >
      {#snippet actions()}
        <Button
          variant="secondary"
          size="sm"
          onclick={startOAuthLogin}
          disabled={oauthStarting}
        >
          {#if oauthStarting}
            <RefreshCw size={14} class="animate-spin" />
            <span>{$tr('Authorizing…')}</span>
          {:else}
            <LogIn size={14} />
            <span>{$tr('Device Login')}</span>
          {/if}
        </Button>
      {/snippet}
      <form onsubmit={addToken} class="flex flex-col sm:flex-row items-start sm:items-center gap-3">
        <div class="flex-1 w-full">
          <Field
            label={$tr('Token')}
            hint={tokenValid === true ? $tr('Valid format') : tokenValid === false ? $tr('Invalid format') : $tr('UUID or session token from ~/.config/codebuff/credentials.json')}
            error={tokenValid === false ? $tr('Token must be at least 10 characters and must not contain spaces, commas, or Bearer prefix') : ''}
            id="add-token-input"
          >
            <input
              id="add-token-input"
              type="text"
              bind:value={newToken}
              placeholder="e.g. a94d808e-8a86-455b-80fb-a9df4422bfcb"
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
          <span>{$tr('Add Token')}</span>
        </Button>
      </form>
    </Card>

    <!-- Token Rotation Scheme & Handling Policy Card -->
    <Card
      title={$tr('Token Rotation & Handling Policy')}
      description={$tr('Strategy used by the gateway to select upstream accounts for model requests.')}
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
              <strong class="text-[var(--fp-text)]">{$tr('Drain Mode (Default & Recommended):')}</strong> {$tr('Exhausts one account completely (e.g. 5/5 Luna sessions) before rotating to the next token. Mimics authentic single-user behavior and provides the strongest anti-ban protection.')}
            </p>
          {:else if tokenRotation === 'round_robin'}
            <p class="leading-relaxed text-[var(--fp-warning)]">
              <strong class="text-[var(--fp-text)]">{$tr('Round-Robin Mode:')}</strong> {$tr('Rotates to the next token on every single session (1:1). Warning: rapid alternating requests across healthy accounts may trigger upstream anomaly detection.')}
            </p>
          {:else if tokenRotation === 'least_used'}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]">{$tr('Least-Used Mode:')}</strong> {$tr('Routes requests to the token with the lowest daily usage or active run count. Maximizes concurrency and distributes quota consumption evenly.')}
            </p>
          {:else if tokenRotation === 'random'}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]">{$tr('Random Mode:')}</strong> {$tr('Selects an available healthy token at random per request. Provides stochastic load balancing.')}
            </p>
          {/if}
        </div>
      </div>
    </Card>

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
          <thead>
            <tr>
              <th class="w-8"></th>
              <th>{$tr('Token')}</th>
              <th>{$tr('Status')}</th>
              <th>{$tr('Instance')}</th>
              <th class="num">{$tr('Cooldown')}</th>
              <th class="text-right">{$tr('Actions')}</th>
            </tr>
          </thead>
          <tbody>
            {#each data.tokens as token, i (token.index ?? i)}
              {@const idx = token.index ?? i}
              <TokenCard
                {token}
                {idx}
                expanded={expandedToken === idx}
                bind:spawnModel={spawnModels[idx]}
                {actionPending}
                {now}
                {devToolsEnabled}
                onToggle={() => toggleExpand(idx)}
                onAction={(action) => handleTokenAction(token, idx, action)}
                onSpawn={(model) => handleSpawn(idx, model)}
                onRefresh={(action) => handleRefresh(idx, action)}
              />
            {/each}
          </tbody>
        </table>
      {/if}
    </Card>
    {#if data?.show_bridge && data?.bridge_token_cards?.length > 0}
      <Card
        title={$tr('Bridge Clients')}
        description={$tr('{count} active bridge client(s) relaying their own FreeBuff tokens', { count: data.bridge_token_cards.length })}
        pad="none"
      >
        <div class="flex flex-col gap-3 p-4">
          {#each data.bridge_token_cards as bc}
            <BridgeTokenCard card={bc} {now} />
          {/each}
        </div>
      </Card>
    {/if}
  </div>
</div>