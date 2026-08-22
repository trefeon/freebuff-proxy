<script>
  import { onMount } from 'svelte';
  import { Check, Zap } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Card from '../components/Card.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Button from '../components/Button.svelte';
  import Field from '../components/Field.svelte';
  import { fetchAPI } from '../api/client.js';
  import { tr } from '../i18n.js';
  import { generateRandomApiKey } from '../utils/format.js';
  import { copyToClipboard } from '../utils/clipboard.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');
  let apiKey = $state('not-needed');
  let copiedModel = $state('');

  async function fetchData() {
    loading = true;
    error = '';
    try {
      data = await fetchAPI('/admin/api/setup');
    } catch (e) {
      error = e.message || $tr('Failed to load setup data');
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    fetchData();
  });

  function copyModel(m) {
    copyToClipboard(m).then((ok) => {
      if (!ok) return;
      copiedModel = m;
      setTimeout(() => {
        if (copiedModel === m) copiedModel = '';
      }, 1800);
    });
  }

  function generateKey() {
    apiKey = generateRandomApiKey();
  }

  function resetKey() {
    apiKey = 'not-needed';
  }

  // Mode facts straight from the /admin/api/setup payload.
  const isBridge = $derived(data?.bridge ?? false);
  const modeTone = $derived(isBridge ? 'info' : 'good');
  const modeBlurb = $derived(
    isBridge
      ? $tr('Bridge mode — no token pool. Each client sends its own FreeBuff token; the proxy relays the Authorization header straight upstream.')
      : $tr('Pooled mode — the proxy holds the upstream AUTH_TOKENS and selects one per request; clients authenticate with any key.')
  );

  // Snippet templates are the real strings from the previous Setup page,
  // interpolated with the live payload and the chosen client key.
  const baseURL = $derived(data?.base_url ?? '');
  const model = $derived(data?.model ?? '');
  const snippets = $derived([
    {
      name: 'OpenCode',
      text: `"freebuff": {"type": "openai", "options": {"baseURL": "${baseURL}", "apiKey": "${apiKey}"}}`,
    },
    {
      name: 'Claude Code (env)',
      text: `export ANTHROPIC_BASE_URL="${baseURL}"\nexport ANTHROPIC_API_KEY="${apiKey}"`,
    },
    {
      name: 'omp',
      text: `"freebuff": {"baseUrl": "${baseURL}", "api": "openai-completions", "apiKey": "${apiKey}"}`,
    },
    {
      name: 'Continue / Cline',
      text: `models:\n  - title: "FreeBuff"\n    provider: "openai"\n    model: "${model}"\n    apiBase: "${baseURL}"\n    apiKey: "${apiKey}"`,
    },
    {
      name: 'aider',
      text: `openai-api-base: ${baseURL}\nopenai-api-key: ${apiKey}\nmodel: ${model}`,
    },
    {
      name: '9router',
      text: `Name: freebuff\nPrefix: freebuff\nAPI type: Chat Completions\nBase URL: ${baseURL}\nAPI Key: ${apiKey}\nModel ID: ${model}`,
    },
    {
      name: 'cURL',
      wide: true,
      text: `curl -N ${baseURL}/chat/completions -H "Authorization: Bearer ${apiKey}" -H "Content-Type: application/json" -d '{"model":"${model}","messages":[{"role":"user","content":"hi"}],"stream":true}'`,
    },
  ]);
</script>

<div class="space-y-6 page-enter">
  <PageHeader title={$tr('Setup')} description={$tr("Client configuration for AI coding tools — copy a block into your tool's config.")}>
    <svelte:fragment slot="actions">
      {#if data}
        <StatusBadge status={data.mode} tone={modeTone} />
      {:else if loading}
        <span class="skeleton skeleton-text w-20"></span>
      {/if}
    </svelte:fragment>
  </PageHeader>

  <!-- Loading -->
  {#if loading}
    <div aria-busy="true">
      <Card class="space-y-3">
        <div class="flex items-center gap-2">
          <div class="skeleton skeleton-text w-24"></div>
          <div class="skeleton skeleton-text w-16"></div>
        </div>
        <div class="skeleton skeleton-line w-3/4"></div>
        <div class="skeleton skeleton-line w-1/2"></div>
        <div class="skeleton skeleton-line w-full"></div>
      </Card>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4 mt-4">
        {#each Array(4) as _}
          <div class="skeleton skeleton-card"></div>
        {/each}
      </div>
      <span class="sr-only">Loading setup data</span>
    </div>
  {/if}

  <!-- Error -->
  {#if error}
    <div class="space-y-3">
      <Alert tone="error" title={$tr('Failed to load setup data')}>{error}</Alert>
      <Button variant="secondary" size="sm" onclick={fetchData}>{$tr('Retry')}</Button>
    </div>
  {/if}

  {#if data}
    <!-- Mode -->
    <Card title={$tr('Mode')} description={$tr('How clients authenticate to this gateway')}>
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
        <StatusBadge status={data.mode} tone={modeTone} />
        {#if isBridge}
          <span class="text-xs text-[var(--fp-dim)]">bridge tokens <span class="fp-num">{data.bridge_tokens}</span></span>
        {:else}
          <span class="text-xs text-[var(--fp-dim)]">pool size <span class="fp-num">{data.token_count}</span></span>
        {/if}
      </div>
      <p class="text-sm text-[var(--fp-muted)] mt-3">{modeBlurb}</p>
      <p class="fp-inset mt-3 px-3 py-2 text-xs font-mono text-[var(--fp-muted)]">Key: {data.key_hint}</p>
    </Card>

    <!-- Client API key -->
    <Card title={$tr('Client API Key')} description={$tr('The key embedded in every snippet below.')}>
      <Field label={$tr('Client API key')} id="setup-api-key" hint={data.key_hint}>
        <div class="flex flex-col sm:flex-row gap-2">
          <input
            id="setup-api-key"
            type="text"
            spellcheck="false"
            bind:value={apiKey}
            placeholder="not-needed"
            class="fp-input fp-mono flex-1"
          />
          <Button variant="secondary" size="sm" onclick={generateKey}>
            <Zap size={16} />Generate
          </Button>
          <Button variant="ghost" size="sm" disabled={apiKey === 'not-needed'} onclick={resetKey}>Reset</Button>
        </div>
      </Field>
    </Card>

    <!-- Quick start -->
    <h2 class="text-[11px] font-mono uppercase tracking-[0.25em] text-[var(--fp-dim)]">{$tr('Quick Start')}</h2>
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
      <Card title={$tr('Base URL')} description={$tr('OpenAI-compatible endpoint — same for every tool.')}>
        <div class="flex items-center gap-2">
          <div class="fp-inset flex-1 px-3 py-2 overflow-x-auto">
            <code class="text-xs">{data.base_url}</code>
          </div>
          <CopyButton text={data.base_url} label="Copy URL" />
        </div>
      </Card>

      <Card title={$tr('Models')} description={$tr('Model IDs available to clients')}>
        {#if data.models.length > 0}
          <p class="text-xs text-[var(--fp-dim)] mb-3">
            <span class="fp-num">{data.models.length}</span> served
          </p>
          <div class="flex flex-wrap gap-2">
            {#each data.models as m}
              <button
                type="button"
                onclick={() => copyModel(m)}
                title="Copy model ID"
                class="min-h-6 px-2.5 py-1 rounded-[var(--fp-radius-sm)] text-xs font-mono transition-colors border
                  {copiedModel === m
                    ? 'border-[var(--fp-success)]/50 bg-[var(--fp-success)]/15 text-[var(--fp-success)]'
                    : 'border-[var(--fp-border)] bg-[var(--fp-input-bg)] text-[var(--fp-muted)] hover:border-[var(--fp-border-bright)] hover:text-[var(--fp-text)]'}"
              >
                {#if copiedModel === m}
                  <Check size={12} class="inline mr-1" />Copied
                {:else}
                  <span class="fp-num">{m}</span>
                  {#if m === model}
                    <span class="ml-1.5 text-[9px] uppercase tracking-wider text-[var(--fp-accent)]">default</span>
                  {/if}
                {/if}
              </button>
            {/each}
          </div>
        {:else}
          <EmptyState title="No models served" description="No model IDs are currently served by this gateway." />
        {/if}
      </Card>

      {#each snippets as s (s.name)}
        <Card title={s.name} class={s.wide ? 'md:col-span-2' : ''}>
          <svelte:fragment slot="actions">
            <CopyButton text={s.text} />
          </svelte:fragment>
          <pre class="fp-inset p-3 text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap break-words">{s.text}</pre>
        </Card>
      {/each}
    </div>
  {/if}
</div>
