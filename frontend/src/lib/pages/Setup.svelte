<script>
  import { onMount } from 'svelte';
  import { Copy, Check, Terminal, Play, Activity, Zap } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Alert from '../components/Alert.svelte';
  import { fetchAPI, postAPI } from '../utils/api.js';
  import { copyToClipboard } from '../utils/clipboard.js';
  import { generateRandomApiKey } from '../utils/format.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');
  let copiedIdx = $state(null);
  let copiedModel = $state('');

  let customApiKey = $state('not-needed');
  // Diagnostic state
  let diagRunning = $state(false);
  let diagChecks = $state(null);

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/setup');
    } catch (e) {
      error = e.message || 'Failed to fetch setup data';
    } finally {
      loading = false;
    }
  }

  function copySnippet(text, idx) {
    copyToClipboard(text.trim());
    copiedIdx = idx;
    setTimeout(() => {
      if (copiedIdx === idx) copiedIdx = null;
    }, 1800);
  }

  async function runDiag() {
    if (diagRunning) return;
    diagRunning = true;
    diagChecks = null;
    try {
      const result = await postAPI('/admin/diag', {});
      diagChecks = result.checks || [];
    } catch {
      diagChecks = [{ name: 'Diagnostics', ok: false, warn: false, message: 'Failed to run diagnostics endpoint' }];
    } finally {
      diagRunning = false;
    }
  }

  onMount(() => {
    fetchData();
  });
</script>

<div class="space-y-6 page-enter">
  <!-- Page Header -->
  <PageHeader title="Client Setup & Tool Integration" subtitle="Connect your favorite AI coding extensions and tools in seconds">
    {#if data}
      <StatusBadge variant="muted" mono>{data.base_url}</StatusBadge>
      <StatusBadge variant={data.bridge ? 'blue' : 'amber'}>
        {data.mode} mode
      </StatusBadge>
    {/if}
  </PageHeader>

  <!-- Universal Config (Top-Level Reference) -->
  {#if data}
    <div class="fp-card p-5 space-y-4">
      <div>
        <h2 class="text-base font-semibold text-white">Universal Configuration</h2>
        <p class="text-xs text-[var(--fp-muted)] mt-0.5">These values work with any OpenAI-compatible client or extension</p>
      </div>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
        <!-- Base URL -->
        <button
          type="button"
          onclick={() => copySnippet(data.base_url, 'url')}
          class="flex flex-col items-start gap-1 p-3.5 rounded-lg fp-inset hover:border-[var(--fp-amber)]/40 transition-all text-left group"
        >
          <span class="text-[10px] uppercase font-semibold text-[var(--fp-dim)] tracking-wider">Base URL</span>
          <span class="font-mono text-sm text-[var(--fp-amber)] group-hover:text-[var(--fp-amber-hover)] transition-colors truncate w-full">{data.base_url}</span>
          <span class="text-[10px] text-[var(--fp-dim)] flex items-center gap-1 mt-1">
            {#if copiedIdx === 'url'}
              <Check size={10} class="text-[var(--fp-teal)]" /> <span class="text-[var(--fp-teal)]">Copied</span>
            {:else}
              <Copy size={10} /> Click to copy
            {/if}
          </span>
        </button>
        <!-- API Key -->
        <button
          type="button"
          onclick={() => copySnippet(customApiKey, 'apikey')}
          class="flex flex-col items-start gap-1 p-3.5 rounded-lg fp-inset hover:border-[var(--fp-amber)]/40 transition-all text-left group"
        >
          <span class="text-[10px] uppercase font-semibold text-[var(--fp-dim)] tracking-wider">API Key</span>
          <span class="font-mono text-sm text-[var(--fp-amber)] group-hover:text-[var(--fp-amber-hover)] transition-colors truncate w-full">{customApiKey}</span>
          <span class="text-[10px] text-[var(--fp-dim)] flex items-center gap-1 mt-1">
            {#if copiedIdx === 'apikey'}
              <Check size={10} class="text-[var(--fp-teal)]" /> <span class="text-[var(--fp-teal)]">Copied</span>
            {:else}
              <Copy size={10} /> Click to copy
            {/if}
          </span>
        </button>
        <!-- Default Model -->
        <button
          type="button"
          onclick={() => copySnippet(data.model, 'model')}
          class="flex flex-col items-start gap-1 p-3.5 rounded-lg fp-inset hover:border-[var(--fp-amber)]/40 transition-all text-left group"
        >
          <span class="text-[10px] uppercase font-semibold text-[var(--fp-dim)] tracking-wider">Default Model</span>
          <span class="font-mono text-sm text-[var(--fp-amber)] group-hover:text-[var(--fp-amber-hover)] transition-colors truncate w-full">{data.model}</span>
          <span class="text-[10px] text-[var(--fp-dim)] flex items-center gap-1 mt-1">
            {#if copiedIdx === 'model'}
              <Check size={10} class="text-[var(--fp-teal)]" /> <span class="text-[var(--fp-teal)]">Copied</span>
            {:else}
              <Copy size={10} /> Click to copy
            {/if}
          </span>
        </button>
      </div>
    </div>
  {/if}

  <!-- API Key Customizer Bar -->
  <div class="fp-card p-5 space-y-3">
    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
      <div>
        <h2 class="text-base font-semibold text-white">API Key Customizer</h2>
        <p class="text-xs text-[var(--fp-muted)] mt-0.5">Customize or generate client API keys to automatically update all integration snippets below</p>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onclick={() => {
            const key = generateRandomApiKey();
            customApiKey = key;
            copyToClipboard(key);
          }}
          class="fp-btn-primary text-xs py-1.5 px-3 flex items-center gap-1.5"
        >
          <Zap size={13} />
          <span>⚡ Generate Key</span>
        </button>
        <button
          type="button"
          onclick={() => customApiKey = 'not-needed'}
          disabled={customApiKey === 'not-needed'}
          class="fp-btn-secondary text-xs py-1.5 px-3"
        >
          Reset to "not-needed"
        </button>
      </div>
    </div>
    <div class="relative">
      <input
        type="text"
        bind:value={customApiKey}
        placeholder="Enter client API key or leave as not-needed..."
        spellcheck="false"
        class="fp-input fp-input-mono text-xs py-2 px-3 focus-visible:ring-2 focus-visible:ring-[var(--fp-amber)]"
      />
    </div>
  </div>

  <!-- Tool-Specific Snippets -->
  {#if data}
    <div class="fp-card p-5 space-y-4">
      <div class="flex items-center justify-between">
        <div>
          <h2 class="text-base font-semibold text-white">Tool-Specific Snippets</h2>
          <p class="text-xs text-[var(--fp-muted)] mt-0.5">Copy the config block for your AI coding extension</p>
        </div>
      </div>

      <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
        <!-- OpenCode -->
        <div class="fp-inset p-4 flex flex-col justify-between">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">OpenCode</h3>
            <CopyButton text={`"freebuff": {"type": "openai", "options": {"baseURL": "${data.base_url}", "apiKey": "${customApiKey}"}}`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">"freebuff": &#123;"type": "openai", "options": &#123;"baseURL": "{data.base_url}", "apiKey": "{customApiKey}"&#125;&#125;</pre>
        </div>

        <!-- Claude Code / Anthropic -->
        <div class="fp-inset p-4 flex flex-col justify-between">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">Claude Code / Anthropic</h3>
            <CopyButton text={`export ANTHROPIC_BASE_URL="${data.base_url}"\nexport ANTHROPIC_API_KEY="${customApiKey}"`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">export ANTHROPIC_BASE_URL="{data.base_url}"
export ANTHROPIC_API_KEY="{customApiKey}"</pre>
        </div>

        <!-- omp -->
        <div class="fp-inset p-4 flex flex-col justify-between">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">omp</h3>
            <CopyButton text={`"freebuff": {"baseUrl": "${data.base_url}", "api": "openai-completions", "apiKey": "${customApiKey}"}`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">"freebuff": &#123;"baseUrl": "{data.base_url}", "api": "openai-completions", "apiKey": "{customApiKey}"&#125;</pre>
        </div>

        <!-- Continue / Cline -->
        <div class="fp-inset p-4 flex flex-col justify-between">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">Continue / Cline</h3>
            <CopyButton text={`models:\n  - title: "FreeBuff"\n    provider: "openai"\n    model: "${data.model}"\n    apiBase: "${data.base_url}"\n    apiKey: "${customApiKey}"`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">models:
  - title: "FreeBuff"
    provider: "openai"
    model: "{data.model}"
    apiBase: "{data.base_url}"
    apiKey: "{customApiKey}"</pre>
        </div>

        <!-- aider -->
        <div class="fp-inset p-4 flex flex-col justify-between">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">aider</h3>
            <CopyButton text={`openai-api-base: ${data.base_url}\nopenai-api-key: ${customApiKey}\nmodel: ${data.model}`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">openai-api-base: {data.base_url}
openai-api-key: {customApiKey}
model: {data.model}</pre>
        </div>

        <!-- 9router -->
        <div class="fp-inset p-4 flex flex-col justify-between">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">9router</h3>
            <CopyButton text={`Name: freebuff\nPrefix: freebuff\nAPI type: Chat Completions\nBase URL: ${data.base_url}\nAPI Key: ${customApiKey}\nModel ID: ${data.model}`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">Name: freebuff
Prefix: freebuff
API type: Chat Completions
Base URL: {data.base_url}
API Key: {customApiKey}
Model ID: {data.model}</pre>
        </div>

        <!-- cURL -->
        <div class="fp-inset p-4 flex flex-col justify-between md:col-span-2 lg:col-span-3">
          <div class="flex items-center justify-between mb-2">
            <h3 class="text-sm font-bold text-white">cURL Command</h3>
            <CopyButton text={`curl -N ${data.base_url}/chat/completions -H "Authorization: Bearer ${customApiKey}" -H "Content-Type: application/json" -d '{"model":"${data.model}","messages":[{"role":"user","content":"hi"}],"stream":true}'`} variant="labeled" />
          </div>
          <pre class="bg-[var(--fp-bg)] p-2.5 rounded-lg text-xs font-mono text-[var(--fp-muted)] overflow-x-auto whitespace-pre-wrap border border-[var(--fp-border)]">curl -N {data.base_url}/chat/completions \
  -H "Authorization: Bearer {customApiKey}" \
  -H "Content-Type: application/json" \
  -d '&#123;"model":"{data.model}","messages":[&#123;"role":"user","content":"hi"&#125;],"stream":true&#125;'</pre>
        </div>
      </div>
    </div>
  {/if}

  <!-- Diagnostics Suite -->
  <div class="fp-card p-5 space-y-3">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2">
        <Activity size={18} class="text-[var(--fp-teal)]" />
        <h2 class="text-base font-semibold text-white">Full Proxy Diagnostics</h2>
      </div>
      <button
        onclick={runDiag}
        disabled={diagRunning}
        class="fp-btn-secondary"
      >
        <Activity size={14} class={diagRunning ? 'animate-spin' : ''} />
        <span>{diagRunning ? 'Running...' : 'Run Diagnostics'}</span>
      </button>
    </div>

    {#if diagChecks}
      <div class="space-y-2 pt-2">
        {#each diagChecks as c}
          <div class="p-3 rounded-lg border text-xs font-mono flex items-center gap-2
            {c.warn ? 'bg-[var(--fp-amber)]/10 border-[var(--fp-amber)]/30 text-[var(--fp-amber)]' : c.ok ? 'bg-[var(--fp-teal)]/10 border-[var(--fp-teal)]/30 text-[var(--fp-teal)]' : 'bg-[var(--fp-red)]/10 border-[var(--fp-red)]/30 text-[var(--fp-red)]'}">
            <span>{c.ok && !c.warn ? '✓' : c.warn ? '△' : '✗'}</span>
            <span>{c.message}</span>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <!-- Model Catalog Chips -->
  {#if data?.models && data.models.length > 0}
    <div class="fp-card p-5 space-y-3">
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-2">
          <Zap size={18} class="text-[var(--fp-amber)]" />
          <h2 class="text-base font-semibold text-white">Active Model Catalog ({data.models.length} Models)</h2>
        </div>
        <span class="text-xs text-[var(--fp-dim)]">Click any model chip to copy ID</span>
      </div>
      <div class="flex flex-wrap gap-2 pt-1">
        {#each data.models as m}
          <button
            type="button"
            onclick={() => {
              copyToClipboard(m);
              copiedModel = m;
              setTimeout(() => { if (copiedModel === m) copiedModel = ''; }, 1800);
            }}
            class="px-2.5 py-1 rounded-lg text-xs font-mono transition-all flex items-center gap-1.5 {copiedModel === m ? 'bg-[var(--fp-teal)]/20 text-[var(--fp-teal)] border border-[var(--fp-teal)]/50 shadow-sm' : m.includes('deepseek-v4-flash') ? 'bg-[var(--fp-amber)]/10 hover:bg-[var(--fp-amber)]/20 text-[var(--fp-amber)] border border-[var(--fp-amber)]/30' : 'bg-[var(--fp-input-bg)] hover:bg-[var(--fp-surface-3)] text-[var(--fp-muted)] hover:text-white border border-[var(--fp-border)]'}"
            title="Click to copy model ID"
          >
            {#if copiedModel === m}
              <Check size={12} class="text-[var(--fp-teal)]" />
              <span>Copied!</span>
            {:else}
              <Copy size={12} class="opacity-60" />
              <span>{m}</span>
              {#if m.includes('deepseek-v4-flash')}
                <span class="text-[9px] uppercase px-1 py-0.5 rounded bg-[var(--fp-amber)]/20 text-[var(--fp-amber)] font-sans font-bold">default</span>
              {/if}
            {/if}
          </button>
        {/each}
      </div>
    </div>
  {/if}
</div>
