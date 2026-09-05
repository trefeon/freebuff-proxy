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
  import { tokensData, ensureTokensStore } from "../stores/tokens.js";
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

  // Live pool/price join: quota chips (N of M) and Freebucks $/hr come from
  // the shared tokens snapshot, keyed by model id across pool tokens.
  let live = $state(null);
  function poolChip(id) {
    for (const t of live?.tokens ?? []) {
      const q = (t.quota ?? []).find((r) => r.model === id);
      if (q && q.pool && q.pool !== "unlimited" && q.pool_label) {
        return `${q.pool_label}: ${q.recent ?? "?"} of ${q.limit} used`;
      }
    }
    return "";
  }
  function priceLabel(id) {
    for (const t of live?.tokens ?? []) {
      const p = t.freebucks?.prices?.[id];
      if (p == null) continue;
      if (p < 1) return `$${p}/hr`;
      return p === 0 ? "0 Freebucks/hr" : `${p} Freebucks/hr`;
    }
    return "";
  }
  onMount(() => {
    const release = ensureTokensStore();
    const unsub = tokensData.subscribe((v) => {
      if (v) live = v;
    });
    return () => {
      release();
      unsub();
    };
  });
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
                <th scope="col">{$tr("Pool")}</th>
                <th scope="col">{$tr("Price")}</th>
              </tr>
            </thead>
            <tbody>
              {#each data.models as m (m.id)}
                {@const bound = Boolean(m.agent)}
                {@const st = modelState(m)}
                {@const effectivePrice =
                  priceLabel(m.id) || m.price_label || "—"}
                {@const effectivePool = poolChip(m.id) || m.pool || "—"}
                <tr>
                  <td>
                    <div class="flex flex-col gap-0.5 min-w-0">
                      <div class="flex items-center gap-1.5 flex-wrap">
                        <strong
                          class="text-xs font-semibold text-[var(--fp-text)] truncate"
                        >
                          {m.display_name || m.id}
                        </strong>
                        {#if m.badges?.length}
                          {#each m.badges as badge (badge)}
                            <span
                              class="px-1 py-0.2 rounded text-[9px] uppercase tracking-wider border {badge ===
                              'NEW'
                                ? 'text-emerald-400 bg-emerald-500/10 border-emerald-500/30 font-semibold'
                                : 'text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]'}"
                            >
                              {badge}
                            </span>
                          {/each}
                        {/if}
                      </div>
                      <div class="flex items-center gap-1 text-[11px]">
                        <span
                          class="fp-num text-[11px] text-[var(--fp-dim)] truncate max-w-[220px]"
                          title={$tr("Click copy button to copy model ID")}
                        >
                          {m.id}
                        </span>
                        <CopyButton
                          text={m.id}
                          label={$tr("Copy model ID")}
                          iconOnly
                        />
                      </div>
                      {#if m.tagline || m.notice}
                        <div
                          class="text-[11px] text-[var(--fp-muted)] flex items-center gap-1.5 pt-0.5"
                        >
                          {#if m.tagline}
                            <span>{m.tagline}</span>
                          {/if}
                          {#if m.tagline && m.notice}
                            <span class="text-[var(--fp-dim)]">·</span>
                          {/if}
                          {#if m.notice}
                            <span class="italic text-[var(--fp-dim)]"
                              >{m.notice}</span
                            >
                          {/if}
                        </div>
                      {/if}
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
                      >{m.quota || "unlimited session"}{#if m.efforts?.length}
                        <span class="text-[var(--fp-dim)]">
                          · {$tr("Reasoning")}: {m.efforts.join("/")}</span
                        >{/if}</span
                    ></td
                  >
                  <td
                    ><span
                      class="fp-num text-xs font-medium {m.pool === 'premium'
                        ? 'text-[var(--fp-accent)]'
                        : m.pool === 'referral'
                          ? 'text-amber-400'
                          : 'text-[var(--fp-muted)]'}">{effectivePool}</span
                    >
                  </td>
                  <td>
                    <span
                      class="fp-num text-xs font-semibold {effectivePrice ===
                      '0 Freebucks/hr'
                        ? 'text-emerald-400'
                        : 'text-[var(--fp-accent)]'}">{effectivePrice}</span
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
            {@const effectivePrice =
              priceLabel(m.id) ||
              m.price_label ||
              (m.price != null ? `${m.price} Freebucks/hr` : "—")}
            {@const effectivePool = poolChip(m.id) || m.pool || "—"}
            <li class="fp-inset rounded-lg p-3 flex flex-col gap-2 min-w-0">
              <div class="flex items-start justify-between gap-2 min-w-0">
                <div class="min-w-0">
                  <strong
                    class="text-sm font-bold text-[var(--fp-text)] block truncate"
                  >
                    {m.display_name || m.id}
                  </strong>
                  <div class="flex items-center gap-1.5 pt-0.5">
                    <code
                      class="fp-num text-xs text-[var(--fp-dim)] truncate max-w-[200px]"
                      >{m.id}</code
                    >
                    <span class="shrink-0 -mr-1">
                      <CopyButton
                        text={m.id}
                        label={$tr("Copy model ID")}
                        iconOnly
                      />
                    </span>
                  </div>
                </div>
                <StatusBadge status={$tr(st)} tone={modelTone(st)} />
              </div>
              {#if m.tagline || m.notice || m.badges?.length}
                <div
                  class="flex flex-wrap items-center gap-1.5 text-xs text-[var(--fp-muted)]"
                >
                  {#if m.tagline}
                    <span>{m.tagline}</span>
                  {/if}
                  {#each m.badges ?? [] as badge (badge)}
                    <span class="text-[var(--fp-dim)]">·</span>
                    <span
                      class="px-1.5 py-0.2 rounded text-[10px] uppercase tracking-wider border {badge ===
                      'NEW'
                        ? 'text-emerald-400 bg-emerald-500/10 border-emerald-500/30 font-semibold'
                        : 'text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]'}"
                    >
                      {badge}
                    </span>
                  {/each}
                  {#if m.notice}
                    <span class="text-[var(--fp-dim)]">·</span>
                    <span class="italic text-[var(--fp-dim)]">{m.notice}</span>
                  {/if}
                </div>
              {/if}
              <div
                class="flex items-center justify-between gap-2 text-xs pt-1 border-t border-[var(--fp-border)]/60"
              >
                <span
                  class="font-semibold {effectivePrice === '0 Freebucks/hr'
                    ? 'text-emerald-400'
                    : 'text-[var(--fp-accent)]'}"
                >
                  {effectivePrice}
                </span>
                <span class="text-[var(--fp-dim)]">
                  {effectivePool}
                </span>
              </div>
              <div
                class="flex items-center justify-between gap-2 border-t border-[var(--fp-border)]/60 pt-1.5 text-xs text-[var(--fp-muted)]"
              >
                <span class="text-[var(--fp-dim)] shrink-0">{$tr("Quota")}</span
                >
                <span class="fp-num text-xs text-right break-words min-w-0"
                  >{m.quota || "unlimited session"}{#if m.efforts?.length}
                    <span class="text-[var(--fp-dim)]">
                      · {m.efforts.join("/")}</span
                    >{/if}</span
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
