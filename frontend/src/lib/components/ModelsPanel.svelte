<script>
  import { onMount } from "svelte";
  import Stat from "./Stat.svelte";
  import Card from "./Card.svelte";
  import Button from "./Button.svelte";
  import Alert from "./Alert.svelte";
  import EmptyState from "./EmptyState.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import CopyButton from "./CopyButton.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tokensData, ensureTokensStore } from "../stores/tokens.js";
  import { sortModelsByPrice } from "../utils/freebucks.js";
  import { tr } from "../i18n.js";
  // Cheapest-first order on the meter (upstream picker revamp): merge the
  // live per-token price maps first-win, and sort only when at least one
  // price exists — unmetered accounts keep the deliberate catalog order.
  const meteredPrices = $derived(
    (() => {
      const map = {};
      for (const t of live?.tokens ?? []) {
        for (const [id, p] of Object.entries(t.freebucks?.prices ?? {})) {
          if (!(id in map)) map[id] = p;
        }
      }
      return map;
    })(),
  );
  const orderedModels = $derived(
    data == null || Object.keys(meteredPrices).length === 0
      ? (data?.models ?? [])
      : sortModelsByPrice(
          data.models.map((m) => m.id),
          { prices: meteredPrices },
        )
          .map((id) => data.models.find((m) => m.id === id))
          .filter(Boolean),
  );
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

  // Live price join: Freebucks $/hr comes from the shared tokens snapshot,
  // keyed by model id across pool tokens.
  let live = $state(null);
  function priceLabel(id) {
    for (const t of live?.tokens ?? []) {
      const p = t.freebucks?.prices?.[id];
      if (p == null) continue;
      if (p < 1) return `$${p}/hr`;
      return p === 0 ? "0 Freebucks/hr" : `${p} Freebucks/hr`;
    }
    return "";
  }
  // Price staleness: the displayed price came from the live join, but every
  // contributing token is quota_stale (pool restarted since last probe).
  function priceIsStale(id) {
    if (!priceLabel(id)) return false;
    const contributors = (live?.tokens ?? []).filter(
      (t) => t.freebucks?.prices?.[id] != null,
    );
    return contributors.length > 0 && contributors.every((t) => t.quota_stale);
  }
  const staleTitle = $derived(
    $tr("Price may be stale — pool restarted since last probe"),
  );
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

{#if loading}
  <p class="text-xs text-[var(--fp-dim)] font-mono">{$tr("Loading…")}</p>
{:else if error}
  <div class="flex flex-col gap-3">
    <Alert tone="error" title={$tr("Failed to load models")}>{error}</Alert>
    <div>
      <Button variant="secondary" onclick={load}>{$tr("Retry")}</Button>
    </div>
  </div>
{:else if data && data.models.length === 0}
  <EmptyState
    title={$tr("No models registered")}
    description={$tr(
      "The model registry is empty. Add model-to-agent mappings in the gateway config and reload.",
    )}
  />
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
  <p class="text-xs text-[var(--fp-muted)]" data-testid="models-note">
    {$tr("Live upstream values — identical for every account in the region.")}
  </p>

  <Card title={$tr("Models")} pad="none">
    <!-- Desktop: table (md+) -->
    <div class="hidden md:block overflow-x-auto">
      <table class="fp-table">
        <thead>
          <tr>
            <th scope="col">{$tr("Model ID")}</th>
            <th scope="col">{$tr("Served")}</th>
            <th scope="col">{$tr("Agent")}</th>
            <th scope="col">{$tr("Price")}</th>
          </tr>
        </thead><tbody>
          {#each orderedModels as m (m.id)}
            {@const bound = Boolean(m.agent)}
            {@const st = modelState(m)}
            {@const effectivePrice = priceLabel(m.id) || m.price_label || "—"}
            {@const stale = priceIsStale(m.id)}
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
                        {#if badge !== "NEW"}
                          <span
                            class="px-1 py-0.2 rounded text-[9px] uppercase tracking-wider border text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]"
                          >
                            {badge}
                          </span>
                        {/if}
                      {/each}
                    {/if}
                  </div>
                  <div
                    class="flex items-center gap-1.5 flex-wrap text-[11px] text-[var(--fp-dim)] min-w-0"
                  >
                    <code
                      class="fp-num truncate max-w-[220px] text-[var(--fp-muted)]"
                      title={m.id}
                    >
                      {m.id}
                    </code>
                    <CopyButton
                      text={m.id}
                      label={$tr("Copy model ID")}
                      iconOnly
                    />
                    {#if m.tagline}
                      <span class="text-[var(--fp-dim)]">·</span>
                      <span class="text-[var(--fp-muted)]">{m.tagline}</span>
                    {/if}
                    {#if m.notice}
                      <span class="text-[var(--fp-dim)]">·</span>
                      <span class="italic">{m.notice}</span>
                    {/if}
                    {#if m.efforts?.length}
                      <span class="text-[var(--fp-dim)]">·</span>
                      <span>{$tr("Reasoning")}: {m.efforts.join("/")}</span>
                    {/if}
                  </div>
                </div>
              </td>
              <td>
                <StatusBadge status={$tr(st)} tone={modelTone(st)} />
              </td>
              <td>
                {#if bound}
                  <span class="fp-mono text-[var(--fp-muted)]">{m.agent}</span>
                {:else}
                  <span class="text-[var(--fp-dim)]">—</span>
                {/if}
              </td>
              <td>
                <span class="inline-flex items-center gap-1.5">
                  <span
                    class="fp-num text-xs font-semibold {effectivePrice ===
                    '0 Freebucks/hr'
                      ? 'text-emerald-400'
                      : 'text-[var(--fp-accent)]'}">{effectivePrice}</span
                  >
                  {#if stale}
                    <span
                      class="led led-warn shrink-0"
                      title={staleTitle}
                      aria-label={staleTitle}
                      role="img"
                    ></span>
                  {/if}
                </span></td
              >
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <!-- Mobile: stacked cards (< md) — no horizontal scrolling -->
    <ul
      class="md:hidden flex flex-col gap-2.5 p-3.5"
      aria-label={$tr("Models")}
    >
      {#each orderedModels as m (m.id)}
        {@const bound = Boolean(m.agent)}
        {@const st = modelState(m)}
        {@const effectivePrice = priceLabel(m.id) || m.price_label || "—"}
        {@const stale = priceIsStale(m.id)}
        <li class="fp-inset rounded p-3 flex flex-col gap-2 min-w-0">
          <div class="flex items-start justify-between gap-2 min-w-0">
            <div class="min-w-0">
              <strong
                class="text-sm font-bold text-[var(--fp-text)] block truncate"
              >
                {m.display_name || m.id}
              </strong>
              <div
                class="flex items-center gap-1.5 flex-wrap text-xs text-[var(--fp-dim)] pt-0.5 min-w-0"
              >
                <code class="fp-num truncate max-w-[200px]">{m.id}</code>
                <span class="shrink-0 -mr-1">
                  <CopyButton
                    text={m.id}
                    label={$tr("Copy model ID")}
                    iconOnly
                  />
                </span>
                {#if m.tagline}
                  <span>·</span>
                  <span class="text-[var(--fp-muted)]">{m.tagline}</span>
                {/if}
                {#each m.badges ?? [] as badge (badge)}
                  {#if badge !== "NEW"}
                    <span>·</span>
                    <span
                      class="px-1.5 py-0.2 rounded text-[10px] uppercase tracking-wider border bg-[var(--fp-surface)] border-[var(--fp-border)]"
                    >
                      {badge}
                    </span>
                  {/if}
                {/each}
                {#if m.notice}
                  <span>·</span>
                  <span class="italic">{m.notice}</span>
                {/if}
                {#if m.efforts?.length}
                  <span>·</span>
                  <span>{$tr("Reasoning")}: {m.efforts.join("/")}</span>
                {/if}
              </div>
            </div>
            <StatusBadge status={$tr(st)} tone={modelTone(st)} />
          </div>
          <div
            class="flex items-center justify-between gap-2 text-xs pt-1 border-t border-[var(--fp-border)]/60"
          >
            <span class="inline-flex items-center gap-1.5">
              <span
                class="font-semibold {effectivePrice === '0 Freebucks/hr'
                  ? 'text-emerald-400'
                  : 'text-[var(--fp-accent)]'}"
              >
                {effectivePrice}
              </span>
              {#if stale}
                <span
                  class="led led-warn shrink-0"
                  title={staleTitle}
                  aria-label={staleTitle}
                  role="img"
                ></span>
              {/if}
            </span>
          </div>
          <div class="flex items-center justify-between gap-2 text-xs min-w-0">
            <span class="text-[var(--fp-dim)] shrink-0">{$tr("Agent")}</span>
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
{/if}
