<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Card from "../components/Card.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { formatTime } from "../utils/format.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");

  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.traces);
      error = "";
    } catch (e) {
      error = e.message || $tr("Failed to load traces");
    } finally {
      loading = false;
    }
  }

  onMount(fetchData);
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Traces")}
    description={$tr("Recent chat traces with per-phase latency breakdowns.")}
  >
    {#snippet actions()}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Refresh")}
      </Button>
    {/snippet}
  </PageHeader>

  {#if loading}
    <div class="space-y-6" aria-busy="true">
      <div class="skeleton skeleton-card h-64"></div>
      <span class="sr-only">{$tr("Loading traces")}</span>
    </div>
  {:else if error}
    <div class="space-y-4">
      <Alert tone="error">{error}</Alert>
      <Button variant="secondary" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Retry")}
      </Button>
    </div>
  {:else if data?.enabled}
    <Card
      title={$tr("Trace log")}
      description={$tr("Last 200 chat traces from the in-memory log ring.")}
      pad="none"
    >
      {#if data.traces?.length}
        <div class="overflow-x-auto">
          <table class="fp-table">
            <caption class="sr-only"
              >{$tr(
                "Chat traces — time, token, model, status, latency and phases",
              )}</caption
            >
            <thead>
              <tr>
                <th scope="col">{$tr("Time")}</th>
                <th scope="col">{$tr("Token")}</th>
                <th scope="col">{$tr("Model")}</th>
                <th scope="col">{$tr("Status")}</th>
                <th scope="col" class="num">{$tr("Latency")}</th>
                <th scope="col">{$tr("Phases")}</th>
                <th scope="col">{$tr("Error")}</th>
              </tr>
            </thead>
            <tbody>
              {#each data.traces as t (t.time)}
                <tr>
                  <td
                    class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                    >{formatTime(t.time)}</td
                  >
                  <td
                    ><span class="fp-num font-mono text-xs">#{t.token}</span
                    ></td
                  >
                  <td class="font-mono text-[11px]">{t.model || "—"}</td>
                  <td>
                    <span
                      class={t.status === "error"
                        ? "text-[var(--fp-error)] font-semibold"
                        : "text-[var(--fp-success)]"}
                    >
                      {t.status || "ok"}
                    </span>
                  </td>
                  <td class="num">{t.ms ? t.ms + "ms" : "—"}</td>
                  <td>
                    {#if t.phases?.length}
                      <div class="flex flex-wrap gap-1">
                        {#each t.phases as ph (ph.name)}
                          <span
                            class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]"
                          >
                            {ph.name}
                            {ph.ms}ms
                          </span>
                        {/each}
                      </div>
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td
                    class="text-[var(--fp-error)] text-[11px] max-w-[200px] truncate"
                    >{t.error || ""}</td
                  >
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No traces recorded yet.")}
          </p>
        </div>
      {/if}
    </Card>
  {:else if data}
    <EmptyState
      title={$tr("Traces disabled")}
      description={$tr(
        "Trace collection is off — no log ring is wired to this dashboard.",
      )}
    />
  {/if}
</div>
