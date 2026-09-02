<script>
  import { onMount } from "svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Stat from "../components/Stat.svelte";
  import Card from "../components/Card.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import Button from "../components/Button.svelte";
  import CopyButton from "../components/CopyButton.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";
  let data = $state(null);
  let loading = $state(true);
  let error = $state("");

  async function load() {
    loading = true;
    error = "";
    try {
      data = await fetchAPI(adminApi.models);
    } catch (e) {
      error = e.message || $tr("Failed to load models");
    } finally {
      loading = false;
    }
  }

  onMount(load);

  const servedCount = $derived(
    data ? data.models.filter((m) => m.agent).length : 0,
  );
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Models")}
    description={$tr(
      "Served model catalog with upstream agent bindings and session quotas.",
    )}
  />

  {#if error}
    <Alert tone="error" title={$tr("Failed to load models")}>
      <p>{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={load}
          >{$tr("Retry")}</Button
        >
      </div>
    </Alert>
  {:else if loading}
    <div role="status" aria-label="Loading model catalog" class="space-y-6">
      <div class="skeleton skeleton-text w-56"></div>
      <div class="fp-card">
        <div class="p-4 space-y-3">
          <div class="skeleton skeleton-line w-40"></div>
          {#each Array(6) as _, i (i)}
            <div class="skeleton skeleton-line w-full"></div>
          {/each}
        </div>
      </div>
    </div>
  {:else if data}
    <Stat
      label={$tr("Served Models")}
      value={servedCount}
      hint={$tr("{count} registered · {agents} agents", {
        count: data.count,
        agents: data.agents,
      })}
      tone={servedCount > 0 ? "good" : "idle"}
      big
    />

    {#if data.models.length > 0}
      <Card title={$tr("Model Catalog")} pad="none">
        <div class="overflow-x-auto">
          <table class="fp-table">
            <thead>
              <tr>
                <th scope="col">{$tr("Model ID")}</th>
                <th scope="col">{$tr("Served")}</th>
                <th scope="col">{$tr("Agent")}</th>
                <th scope="col">{$tr("Premium Quota")}</th>
              </tr>
            </thead>
            <tbody>
              {#each data.models as m (m.id)}
                {@const bound = Boolean(m.agent)}
                <tr>
                  <td>
                    <div class="flex items-center gap-1.5">
                      <span
                        class="fp-num flex-1 min-w-0 truncate"
                        title={$tr("Click copy button to copy model ID")}
                        >{m.id}</span
                      >
                      <CopyButton text={m.id} label={$tr("Copy model ID")} />
                    </div>
                  </td>
                  <td>
                    <StatusBadge
                      status={bound ? $tr("served") : $tr("unbound")}
                      tone={bound ? "good" : "idle"}
                    />
                  </td>
                  <td>
                    {#if bound}
                      <span class="fp-mono text-[var(--fp-muted)]"
                        >{m.agent}</span
                      >
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td
                    ><span class="fp-num text-xs text-[var(--fp-muted)]"
                      >{m.quota || "unlimited session"}</span
                    ></td
                  >
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </Card>
    {:else}
      <EmptyState
        title={$tr("No models registered")}
        description={$tr(
          "The model registry is empty. Add model-to-agent mappings in the gateway config and reload.",
        )}
      />
    {/if}
  {/if}
</div>
