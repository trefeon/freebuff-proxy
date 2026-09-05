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

  // Row state: grant-gated referral rows carry an agent binding but
  // served=false — they render "referral", not "served". Rows without a
  // binding (and legacy payloads without the served flag) stay "unbound".
  function modelState(m) {
    if (!m.agent) return "unbound";
    if (m.served === false) return "referral";
    return "served";
  }
  function modelTone(state) {
    return state === "served" ? "good" : state === "referral" ? "info" : "idle";
  }
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
    data ? data.models.filter((m) => modelState(m) === "served").length : 0,
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
        <!-- Desktop: table (md+) -->
        <div class="hidden md:block overflow-x-auto">
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
                {@const st = modelState(m)}
                <tr>
                  <td>
                    <div class="flex items-center gap-1.5">
                      <span
                        class="fp-num flex-1 min-w-0 truncate"
                        title={$tr("Click copy button to copy model ID")}
                        >{m.id}</span
                      >
                      <CopyButton
                        text={m.id}
                        label={$tr("Copy model ID")}
                        iconOnly
                      />
                    </div>
                  </td>
                  <td>
                    <StatusBadge status={$tr(st)} tone={modelTone(st)} />
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
        <!-- Mobile: stacked cards (< md) — no horizontal scrolling -->
        <ul
          class="md:hidden flex flex-col gap-2.5 p-3.5"
          aria-label={$tr("Model Catalog")}
        >
          {#each data.models as m (m.id)}
            {@const bound = Boolean(m.agent)}
            {@const st = modelState(m)}
            <li class="fp-inset rounded-lg p-3 flex flex-col gap-2 min-w-0">
              <div class="flex items-start justify-between gap-2 min-w-0">
                <code
                  class="fp-num text-[13px] break-all min-w-0 text-[var(--fp-text)]"
                  >{m.id}</code
                >
                <span class="shrink-0 -mr-1 -mt-1">
                  <CopyButton
                    text={m.id}
                    label={$tr("Copy model ID")}
                    iconOnly
                  />
                </span>
              </div>
              <div
                class="flex items-center justify-between gap-2 border-t border-[var(--fp-border)] pt-2"
              >
                <StatusBadge status={$tr(st)} tone={modelTone(st)} />
                <span
                  class="fp-num text-xs text-[var(--fp-muted)] text-right break-words min-w-0"
                  >{m.quota || "unlimited session"}</span
                >
              </div>
              <div
                class="flex items-center justify-between gap-2 text-xs min-w-0"
              >
                <span class="text-[var(--fp-dim)] shrink-0">{$tr("Agent")}</span
                >
                {#if bound}
                  <span
                    class="fp-mono text-[var(--fp-muted)] text-right break-all min-w-0"
                    >{m.agent}</span
                  >
                {:else}
                  <span class="text-[var(--fp-dim)]">—</span>
                {/if}
              </div>
            </li>
          {/each}
        </ul>
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
