<script>
  import {
    FlaskConical,
    Send,
    RefreshCw,
    Zap,
    Check,
    Cpu,
    Sliders,
    Terminal,
    Copy,
    Activity,
    Lock,
    Unlock,
    Trash2,
  } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Card from '../components/Card.svelte';
  import Button from '../components/Button.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import { fetchAPI, postAPI } from '../api/client.js';
  import { usePolling } from '../utils/polling.js';
  import { formatLocalDate } from '../utils/format.js';
  import { tr } from '../i18n.js';

  // --- State for Chat Playground ---
  let selectedModel = $state('stealth/ox-alpha');
  let protocol = $state('openai'); // 'openai' | 'anthropic'
  let streamMode = $state(true);
  let promptText = $state('Hello! Count from 1 to 5 and briefly describe yourself.');
  let reasoningEffort = $state('medium');
  let sendingChat = $state(false);
  let chatOutput = $state('');
  let chatStatus = $state(null); // { latencyMs, statusCode, tokens: { prompt, completion, total }, model }
  let chatError = $state('');

  // --- State for Session Spawner & Tokens ---
  let tokensData = $state(null);
  let loadingTokens = $state(true);
  let actionMessage = $state('');
  let actionOK = $state(true);
  let actionPending = $state(false);
  let spawnModels = $state({});

  // --- State for Batch Traffic Generator ---
  let batchCount = $state(5);
  let batchRunning = $state(false);
  let batchLogs = $state([]);

  const modelsList = [
    { id: 'stealth/ox-alpha', label: 'stealth/ox-alpha (1M ctx · unmetered)', tag: '1M' },
    { id: 'openai/gpt-5.6-luna', label: 'openai/gpt-5.6-luna (5/day shared)', tag: '5/d' },
    { id: 'mimo/mimo-v2.5', label: 'mimo/mimo-v2.5 (unmetered entry)', tag: 'unmetered' },
    { id: 'z-ai/glm-5.3-flash', label: 'z-ai/glm-5.3-flash (2/day cap)', tag: '2/d' },
    { id: 'deepseek/deepseek-v4-flash', label: 'deepseek/deepseek-v4-flash (unmetered)', tag: 'unmetered' },
    { id: 'deepseek/deepseek-v4-pro', label: 'deepseek/deepseek-v4-pro (5/day shared)', tag: '5/d' },
    { id: 'z-ai/glm-5.2', label: 'z-ai/glm-5.2 (referral promo)', tag: 'referral' },
  ];

  async function fetchTokens() {
    try {
      tokensData = await fetchAPI('/admin/api/tokens');
    } catch (e) {
      console.warn('Failed to fetch tokens in DevTools', e);
    } finally {
      loadingTokens = false;
    }
  }

  usePolling(fetchTokens, 8000);

  async function sendPlaygroundChat() {
    if (sendingChat || !promptText.trim()) return;
    sendingChat = true;
    chatOutput = '';
    chatError = '';
    chatStatus = null;
    const start = performance.now();

    try {
      if (protocol === 'openai') {
        const payload = {
          model: selectedModel,
          messages: [{ role: 'user', content: promptText }],
          stream: streamMode,
        };
        if (reasoningEffort && reasoningEffort !== 'default') {
          payload.reasoning_effort = reasoningEffort;
        }

        const res = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });

        const latency = Math.round(performance.now() - start);

        if (!res.ok) {
          const errText = await res.text();
          chatError = `HTTP ${res.status}: ${errText}`;
          chatStatus = { latencyMs: latency, statusCode: res.status, model: selectedModel };
          return;
        }

        if (streamMode && res.body) {
          chatStatus = { latencyMs: latency, statusCode: res.status, model: selectedModel };
          const reader = res.body.getReader();
          const decoder = new TextDecoder();
          let buf = '';

          while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += decoder.decode(value, { stream: true });
            const lines = buf.split('\n');
            buf = lines.pop() || '';

            for (const line of lines) {
              const trimmed = line.trim();
              if (trimmed.startsWith('data: ')) {
                const dataStr = trimmed.slice(6);
                if (dataStr === '[DONE]') continue;
                try {
                  const chunk = JSON.parse(dataStr);
                  const delta = chunk.choices?.[0]?.delta?.content || '';
                  chatOutput += delta;
                } catch {}
              }
            }
          }
        } else {
          const json = await res.json();
          chatOutput = json.choices?.[0]?.message?.content || JSON.stringify(json, null, 2);
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            tokens: json.usage,
            model: selectedModel,
          };
        }
      } else {
        // Anthropic protocol (/v1/messages)
        const payload = {
          model: selectedModel,
          messages: [{ role: 'user', content: promptText }],
          max_tokens: 1024,
          stream: streamMode,
        };

        const res = await fetch('/v1/messages', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'anthropic-version': '2023-06-01' },
          body: JSON.stringify(payload),
        });

        const latency = Math.round(performance.now() - start);

        if (!res.ok) {
          const errText = await res.text();
          chatError = `HTTP ${res.status}: ${errText}`;
          chatStatus = { latencyMs: latency, statusCode: res.status, model: selectedModel };
          return;
        }

        if (streamMode && res.body) {
          chatStatus = { latencyMs: latency, statusCode: res.status, model: selectedModel };
          const reader = res.body.getReader();
          const decoder = new TextDecoder();
          let buf = '';

          while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += decoder.decode(value, { stream: true });
            const lines = buf.split('\n');
            buf = lines.pop() || '';

            for (const line of lines) {
              const trimmed = line.trim();
              if (trimmed.startsWith('data: ')) {
                try {
                  const evt = JSON.parse(trimmed.slice(6));
                  if (evt.type === 'content_block_delta' && evt.delta?.type === 'text_delta') {
                    chatOutput += evt.delta.text || '';
                  }
                } catch {}
              }
            }
          }
        } else {
          const json = await res.json();
          const textBlock = json.content?.find((c) => c.type === 'text');
          chatOutput = textBlock?.text || JSON.stringify(json, null, 2);
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            tokens: json.usage,
            model: selectedModel,
          };
        }
      }
    } catch (e) {
      chatError = e.message || 'Request failed';
      chatStatus = { latencyMs: Math.round(performance.now() - start), statusCode: 0, model: selectedModel };
    } finally {
      sendingChat = false;
      fetchTokens();
    }
  }

  async function triggerTokenAction(url, body, confirmMsg) {
    if (confirmMsg && !confirm(confirmMsg)) return;
    actionPending = true;
    actionMessage = '';
    try {
      const res = await postAPI(url, body);
      actionOK = res.ok;
      actionMessage = res.message || (res.ok ? $tr('Action completed') : $tr('Action failed'));
      await fetchTokens();
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || $tr('Action failed');
    } finally {
      actionPending = false;
    }
  }

  async function runBatchTraffic() {
    if (batchRunning) return;
    batchRunning = true;
    batchLogs = [];
    try {
      for (let i = 1; i <= batchCount; i++) {
        const start = performance.now();
        const model = selectedModel;
        try {
          const res = await fetch('/v1/chat/completions', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              model,
              messages: [{ role: 'user', content: `Ping test #${i}` }],
              max_tokens: 16,
              stream: false,
            }),
          });
          const ms = Math.round(performance.now() - start);
          batchLogs = [
            { reqNum: i, model, status: res.status, ok: res.ok, ms, time: new Date().toLocaleTimeString() },
            ...batchLogs,
          ];
        } catch (err) {
          const ms = Math.round(performance.now() - start);
          batchLogs = [
            { reqNum: i, model, status: 0, ok: false, error: err.message, ms, time: new Date().toLocaleTimeString() },
            ...batchLogs,
          ];
        }
        await new Promise((r) => setTimeout(r, 200));
      }
      await fetchTokens();
    } finally {
      batchRunning = false;
    }
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr('Dev Tools')}
    description={$tr('Interactive model playground, session spawner, and gateway load simulation.')}
  >
    {#snippet actions()}
      <Button
        variant="ghost"
        size="sm"
        disabled={actionPending || !tokensData?.token_count}
        onclick={() => triggerTokenAction('/admin/tokens/test-all', {}, $tr('Probe all pool tokens against upstream?'))}
      >
        <Activity size={14} />
        <span>{$tr('Probe All Tokens')}</span>
      </Button>
    {/snippet}
  </PageHeader>

  {#if actionMessage}
    <Alert tone={actionOK ? 'success' : 'error'} title={actionMessage} />
  {/if}

  <!-- Section 1: Live Chat Playground -->
  <section aria-label="Model Playground">
    <Card title={$tr('Model Chat & Stream Playground')} description={$tr('Send live requests directly to the proxy to test streaming, reasoning, and latency.')}>
      {#snippet actions()}
        <span class="text-xs font-mono text-[var(--fp-muted)] flex items-center gap-2">
          <span>Protocol:</span>
          <span class="uppercase tracking-wider font-semibold text-[var(--fp-accent)]">{protocol}</span>
        </span>
      {/snippet}

      <div class="space-y-4">
        <!-- Controls Bar -->
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3">
          <!-- Model Picker -->
          <div class="flex flex-col gap-1">
            <label for="dev-model" class="text-xs text-[var(--fp-muted)]">{$tr('Model')}</label>
            <select id="dev-model" bind:value={selectedModel} class="fp-input text-xs w-full py-1.5">
              {#each modelsList as m (m.id)}
                <option value={m.id}>{m.label}</option>
              {/each}
            </select>
          </div>

          <!-- Protocol Switch -->
          <div class="flex flex-col gap-1">
            <label for="dev-proto" class="text-xs text-[var(--fp-muted)]">{$tr('API Surface')}</label>
            <select id="dev-proto" bind:value={protocol} class="fp-input text-xs w-full py-1.5">
              <option value="openai">OpenAI (/v1/chat/completions)</option>
              <option value="anthropic">Anthropic (/v1/messages)</option>
            </select>
          </div>

          <!-- Stream Mode -->
          <div class="flex flex-col gap-1">
            <label for="dev-stream" class="text-xs text-[var(--fp-muted)]">{$tr('Stream Mode')}</label>
            <select id="dev-stream" bind:value={streamMode} class="fp-input text-xs w-full py-1.5">
              <option value={true}>{$tr('SSE Streaming (Real-time)')}</option>
              <option value={false}>{$tr('JSON Response (Non-stream)')}</option>
            </select>
          </div>

          <!-- Reasoning Effort -->
          <div class="flex flex-col gap-1">
            <label for="dev-reasoning" class="text-xs text-[var(--fp-muted)]">{$tr('Reasoning Effort')}</label>
            <select id="dev-reasoning" bind:value={reasoningEffort} class="fp-input text-xs w-full py-1.5">
              <option value="default">Default</option>
              <option value="minimal">minimal</option>
              <option value="low">low</option>
              <option value="medium">medium</option>
              <option value="high">high</option>
              <option value="xhigh">xhigh (max)</option>
            </select>
          </div>
        </div>

        <!-- Prompt Textarea -->
        <div class="flex flex-col gap-1.5">
          <label for="dev-prompt" class="text-xs text-[var(--fp-muted)]">{$tr('Prompt')}</label>
          <textarea
            id="dev-prompt"
            bind:value={promptText}
            rows="3"
            class="fp-input fp-mono text-xs w-full p-2.5 resize-y"
            placeholder="Type a test prompt…"
          ></textarea>
        </div>

        <div class="flex items-center justify-between gap-3">
          <div class="flex items-center gap-3 text-xs font-mono text-[var(--fp-muted)]">
            {#if chatStatus}
              <span class="inline-flex items-center gap-1.5">
                <span class="led {chatStatus.statusCode === 200 ? 'led-good' : 'led-idle'}"></span>
                <span>{chatStatus.statusCode === 200 ? 'OK 200' : `Status ${chatStatus.statusCode}`}</span>
              </span>
              <span class="text-[var(--fp-border-bright)]">|</span>
              <span>{chatStatus.latencyMs}ms</span>
              {#if chatStatus.tokens}
                <span class="text-[var(--fp-border-bright)]">|</span>
                <span>{chatStatus.tokens.total_tokens || chatStatus.tokens.output_tokens || 0} tokens</span>
              {/if}
            {/if}
          </div>

          <Button
            variant="primary"
            size="md"
            loading={sendingChat}
            disabled={sendingChat || !promptText.trim()}
            onclick={sendPlaygroundChat}
          >
            <Send size={14} />
            <span>{$tr('Send Request')}</span>
          </Button>
        </div>

        {#if chatError}
          <div role="alert" class="p-3 rounded bg-[var(--fp-error)]/10 border border-[var(--fp-error)]/30 text-xs text-[var(--fp-error)] font-mono">
            {chatError}
          </div>
        {/if}

        <!-- Output Box -->
        {#if chatOutput}
          <div class="fp-inset rounded-lg p-3.5 space-y-2 bg-[var(--fp-surface-2)]">
            <div class="flex items-center justify-between">
              <span class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider">{$tr('Output')}</span>
              <CopyButton text={chatOutput} label="Copy Output" />
            </div>
            <pre class="font-mono text-xs text-[var(--fp-text)] whitespace-pre-wrap break-all select-all leading-relaxed max-h-80 overflow-y-auto">{chatOutput}</pre>
          </div>
        {/if}
      </div>
    </Card>
  </section>

  <!-- Section 2: Per-Token Session Spawner & Lifecycle -->
  <section aria-label="Token Session Management">
    <Card title={$tr('Per-Token Session Spawner')} description={$tr('Directly admit or spawn upstream sessions for specific models on individual tokens.')}>
      {#if loadingTokens}
        <div class="p-4 space-y-3">
          <div class="skeleton skeleton-line"></div>
          <div class="skeleton skeleton-line"></div>
        </div>
      {:else if tokensData?.tokens?.length}
        <div class="overflow-x-auto">
          <table class="fp-table">
            <thead>
              <tr>
                <th scope="col">Token</th>
                <th scope="col">Status</th>
                <th scope="col">Active Session</th>
                <th scope="col">Select Model</th>
                <th scope="col" class="text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {#each tokensData.tokens as token (token.index)}
                {@const idx = token.index}
                <tr>
                  <td><span class="fp-num text-xs font-bold text-[var(--fp-text)]">#{idx}</span></td>
                  <td>
                    <span class="inline-flex items-center gap-1.5">
                      <span class="led {token.session_status === 'active' ? 'led-good' : 'led-idle'}"></span>
                      <span class="font-mono text-xs uppercase tracking-wider text-[var(--fp-muted)]">{token.session_status || 'idle'}</span>
                    </span>
                  </td>
                  <td>
                    {#if token.session_model}
                      <span class="fp-num text-xs text-[var(--fp-accent)] font-semibold">{token.session_model}</span>
                      {#if token.session_remaining_seconds > 0}
                        <span class="text-[11px] text-[var(--fp-dim)] block">{Math.floor(token.session_remaining_seconds / 60)}m left</span>
                      {/if}
                    {:else}
                      <span class="text-xs text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td>
                    <select
                      bind:value={spawnModels[idx]}
                      class="fp-input !text-xs !py-1 !px-2 !h-8 !w-48"
                    >
                      <option value="stealth/ox-alpha">stealth/ox-alpha (1M)</option>
                      <option value="openai/gpt-5.6-luna">openai/gpt-5.6-luna (5/d)</option>
                      <option value="mimo/mimo-v2.5">mimo/mimo-v2.5 (unlimited)</option>
                      <option value="z-ai/glm-5.3-flash">z-ai/glm-5.3-flash (2/d)</option>
                      <option value="deepseek/deepseek-v4-flash">deepseek/deepseek-v4-flash</option>
                      <option value="deepseek/deepseek-v4-pro">deepseek/deepseek-v4-pro</option>
                      <option value="z-ai/glm-5.2">z-ai/glm-5.2 (referral)</option>
                    </select>
                  </td>
                  <td class="text-right">
                    <div class="inline-flex items-center gap-1.5 justify-end">
                      <Button
                        variant="primary"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerTokenAction(`/admin/tokens/${idx}/session`, { model: spawnModels[idx] || 'stealth/ox-alpha' }, $tr('Spawn upstream session on token #{idx} for {model}?', { idx, model: spawnModels[idx] || 'stealth/ox-alpha' }))}
                      >
                        <Zap size={13} />
                        <span>{$tr('Make Session')}</span>
                      </Button>
                      <Button
                        variant="secondary"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerTokenAction(`/admin/tokens/${idx}/test`, {}, $tr('Probe token #{idx}?', { idx }))}
                      >
                        <RefreshCw size={13} />
                        <span>{$tr('Probe')}</span>
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={actionPending}
                        onclick={() => triggerTokenAction(`/admin/tokens/${idx}/finish`, {}, $tr('Finish runs on token #{idx}?', { idx }))}
                      >
                        <Check size={13} />
                        <span>{$tr('Finish')}</span>
                      </Button>
                    </div>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card>
  </section>

  <!-- Section 3: Batch Traffic & Rotation Simulator -->
  <section aria-label="Batch Traffic Generator">
    <Card title={$tr('Traffic Generator & Rotation Benchmark')} description={$tr('Send simulated request bursts to observe live token pool rotation and failover in action.')}>
      {#snippet actions()}
        <span class="text-xs font-mono text-[var(--fp-muted)]">Burst: {batchCount} reqs</span>
      {/snippet}

      <div class="space-y-3">
        <div class="flex flex-wrap items-center gap-3">
          <div class="flex items-center gap-2">
            <label for="dev-burst" class="text-xs text-[var(--fp-muted)]">{$tr('Requests:')}</label>
            <select id="dev-burst" bind:value={batchCount} class="fp-input text-xs w-28 py-1.5">
              <option value={3}>3 requests</option>
              <option value={5}>5 requests</option>
              <option value={10}>10 requests</option>
              <option value={20}>20 requests</option>
            </select>
          </div>

          <Button
            variant="secondary"
            size="md"
            loading={batchRunning}
            disabled={batchRunning}
            onclick={runBatchTraffic}
          >
            <Activity size={14} />
            <span>{$tr('Fire Burst Traffic')}</span>
          </Button>
        </div>

        {#if batchLogs.length > 0}
          <div class="fp-inset rounded-lg p-3 space-y-1.5 max-h-48 overflow-y-auto font-mono text-xs">
            {#each batchLogs as log (log.reqNum)}
              <div class="flex items-center justify-between gap-2">
                <span class="text-[var(--fp-muted)]">Req #{log.reqNum} · {log.model}</span>
                <div class="flex items-center gap-2">
                  <span class="px-1.5 py-0.5 rounded text-[10px] {log.ok ? 'bg-[var(--fp-success)]/10 text-[var(--fp-success)]' : 'bg-[var(--fp-error)]/10 text-[var(--fp-error)]'}">
                    {log.ok ? `HTTP ${log.status}` : (log.error || `Status ${log.status}`)}
                  </span>
                  <span class="text-[var(--fp-dim)]">{log.ms}ms</span>
                </div>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    </Card>
  </section>
</div>
