<script>
  import { Activity } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import Button from "./Button.svelte";
  import { fetchAPI } from "../api/client.js";
  import { fetchModelOptions, fallbackModelOptions } from "../modelOptions.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  let { onLog } = $props();

  let batchCount = $state(5);
  let batchRunning = $state(false);
  let batchLogs = $state([]);
  let batchSeq = 0;
  let selectedModel = $state("mimo/mimo-v2.5");

  let modelsList = $state(fallbackModelOptions);

  fetchModelOptions().then((rows) => (modelsList = rows));
  async function runBatchTraffic() {
    if (batchRunning) return;
    batchRunning = true;
    batchLogs = [];
    try {
      for (let i = 1; i <= batchCount; i++) {
        const start = performance.now();
        const model = selectedModel;
        try {
          const res = await fetch("/v1/chat/completions", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              model,
              messages: [{ role: "user", content: `Ping test #${i}` }],
              max_tokens: 16,
              stream: false,
            }),
          });
          const ms = Math.round(performance.now() - start);
          const entry = {
            id: ++batchSeq,
            reqNum: i,
            model,
            status: res.status,
            ok: res.ok,
            ms,
            time: new Date().toLocaleTimeString(),
          };
          batchLogs = [entry, ...batchLogs];
          onLog?.(entry);
        } catch (err) {
          const ms = Math.round(performance.now() - start);
          const entry = {
            id: ++batchSeq,
            reqNum: i,
            model,
            status: 0,
            ok: false,
            error: err.message,
            ms,
            time: new Date().toLocaleTimeString(),
          };
          batchLogs = [entry, ...batchLogs];
          onLog?.(entry);
        }
        await new Promise((r) => setTimeout(r, 200));
      }
      try {
        await fetchAPI(adminApi.tokens);
      } catch {}
    } finally {
      batchRunning = false;
    }
  }
</script>

<section aria-label="Batch Traffic Generator">
  <Card
    title={$tr("Traffic Generator & Rotation Benchmark")}
    description={$tr(
      "Send simulated request bursts to observe live token pool rotation and failover in action.",
    )}
  >
    {#snippet actions()}
      <span class="text-xs font-mono text-[var(--fp-muted)]"
        >Burst: {batchCount} reqs</span
      >
    {/snippet}

    <div class="space-y-3">
      <div class="flex flex-wrap items-center gap-3">
        <div class="flex items-center gap-2">
          <label for="dev-burst-model" class="text-xs text-[var(--fp-muted)]"
            >{$tr("Model:")}</label
          >
          <select
            id="dev-burst-model"
            bind:value={selectedModel}
            class="fp-input text-xs w-48 py-1.5"
          >
            {#each modelsList as m (m.id)}
              <option value={m.id}>{m.label}</option>
            {/each}
          </select>
        </div>
        <div class="flex items-center gap-2">
          <label for="dev-burst" class="text-xs text-[var(--fp-muted)]"
            >{$tr("Requests:")}</label
          >
          <select
            id="dev-burst"
            bind:value={batchCount}
            class="fp-input text-xs w-28 py-1.5"
          >
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
          <span>{$tr("Fire Burst Traffic")}</span>
        </Button>
      </div>

      {#if batchLogs.length > 0}
        <div
          class="fp-inset rounded-lg p-3 space-y-1.5 max-h-48 overflow-y-auto font-mono text-xs"
        >
          {#each batchLogs as log (log.id)}
            <div class="flex items-center justify-between gap-2">
              <span class="text-[var(--fp-muted)]"
                >Req #{log.reqNum} · {log.model}</span
              >
              <div class="flex items-center gap-2">
                <span
                  class="px-1.5 py-0.5 rounded text-[10px] {log.ok
                    ? 'bg-[var(--fp-success)]/10 text-[var(--fp-success)]'
                    : 'bg-[var(--fp-error)]/10 text-[var(--fp-error)]'}"
                >
                  {log.ok
                    ? `HTTP ${log.status}`
                    : log.error || `Status ${log.status}`}
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
