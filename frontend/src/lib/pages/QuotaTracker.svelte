<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Card from "../components/Card.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import PremiumQuotaBar from "../components/PremiumQuotaBar.svelte";
  import {
    tokensData,
    tokensError,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import { formatLocalDate } from "../utils/format.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  // Countdown tick: refetches nothing on its own; PremiumQuotaBar and the
  // reset cells re-render "resets in" against this clock every second.
  let now = $state(Date.now());

  let unsubStore = null;
  let unsubErr = null;
  let tick = null;
  onMount(() => {
    // One shared tokens store: the page renders from the cached snapshot and
    // the store owns the poll + SSE subscription (issue #292).
    const release = ensureTokensStore();
    unsubStore = tokensData.subscribe((v) => {
      if (v) {
        data = v;
        loading = false;
        error = "";
      }
    });
    unsubErr = tokensError.subscribe((err) => {
      if (err) {
        error = err;
        loading = false;
      }
    });
    tick = setInterval(() => {
      now = Date.now();
    }, 1000);
    return () => {
      release();
      unsubStore?.();
      unsubErr?.();
      clearInterval(tick);
    };
  });

  // Bridge entries are owned by the Tokens page; this page only reports the
  // count that currently report premium quota or freebucks (no bridge cards here).
  const bridgeQuotaCount = $derived(
    (data?.bridge_token_cards ?? []).filter((c) => c.premium_quota || c.freebucks).length,
  );
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Quota Tracker")}
    description={$tr(
      "Live per-model session quota and premium pool usage across pooled tokens",
    )}
  >
    {#snippet actions()}
      <Button variant="ghost" onclick={refreshTokens}>
        <RefreshCw size={15} />
        {$tr("Refresh")}
      </Button>
    {/snippet}
  </PageHeader>

  {#if loading}
    <div class="space-y-6" aria-busy="true">
      {#each Array(3) as _, i (i)}
        <div class="skeleton skeleton-card h-44"></div>
      {/each}
      <span class="sr-only">{$tr("Loading quota tracker")}</span>
    </div>
  {:else if error}
    <div class="space-y-4">
      <Alert tone="error">{error}</Alert>
      <Button variant="secondary" onclick={refreshTokens}>
        <RefreshCw size={15} />
        {$tr("Retry")}
      </Button>
    </div>
  {:else if data}
    {#if !data.has_tokens || !data.tokens?.length}
      <EmptyState
        title={$tr("No tokens in pool")}
        description={$tr(
          "Add a token to the pool to see per-model session quota and premium pool usage.",
        )}
      />
    {:else}
      <div class="grid grid-cols-1 gap-4">
        {#each data.tokens as token, ti (token.index ?? ti)}
          {@const idx = token.index ?? ti}
          <Card
            title={$tr("Token #{index}", { index: idx })}
            description={token.session_model
              ? $tr("Session: {model}", { model: token.session_model })
              : ""}
          >
            <div class="flex flex-col gap-4">
              {#if token.freebucks}
                <PremiumQuotaBar freebucks={token.freebucks} {now} />
              {:else if token.premium_quota}
                <PremiumQuotaBar quota={token.premium_quota} {now} />
              {:else}
                <p class="text-xs text-[var(--fp-dim)] italic">
                  {$tr(
                    "No premium quota data — run a request or -test-token to populate.",
                  )}
                </p>
              {/if}

              <div>
                <h3
                  class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] mb-2"
                >
                  {$tr("Session quota by model")}
                </h3>
                {#if token.quota?.length}
                  <div class="overflow-x-auto">
                    <table class="fp-table">
                      <caption class="sr-only"
                        >{$tr("Session quota by model for token {index}", {
                          index: idx,
                        })}</caption
                      >
                      <thead>
                        <tr>
                          <th scope="col">{$tr("model")}</th>
                          <th scope="col" class="num">{$tr("recent")}</th>
                          <th scope="col" class="num">{$tr("limit")}</th>
                          <th scope="col">{$tr("period")}</th>
                          <th scope="col">{$tr("reset")}</th>
                          <th scope="col">{$tr("entitlement")}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {#each token.quota as q (q.model)}
                          <tr>
                            <td
                              ><code
                                class="fp-num text-xs {q.near_limit
                                  ? 'text-[#f5a623] font-medium'
                                  : 'text-[var(--fp-text)]'}">{q.model}</code
                              ></td
                            >
                            <td
                              class="num text-xs {q.near_limit
                                ? 'text-[#f5a623]'
                                : 'text-[var(--fp-muted)]'}">{q.recent}</td
                            >
                            <td
                              class="num text-xs {q.near_limit
                                ? 'text-[#f5a623]'
                                : 'text-[var(--fp-muted)]'}">{q.limit}</td
                            >
                            <td
                              class="fp-num text-xs {q.near_limit
                                ? 'text-[#f5a623]'
                                : 'text-[var(--fp-dim)]'}">{q.period}</td
                            >
                            <td class="fp-num text-xs">
                              <span
                                class={q.near_limit
                                  ? "text-[#f5a623] font-medium"
                                  : "text-[var(--fp-dim)]"}
                              >
                                {formatLocalDate(q.reset_at_utc) ||
                                  q.reset_at}{#if q.resets_in}
                                  ({q.resets_in}){/if}
                              </span>
                            </td>
                            <td
                              class="fp-num text-xs {q.near_limit
                                ? 'text-[#f5a623]'
                                : 'text-[var(--fp-dim)]'}"
                              >{q.has_entitlement ? q.entitled : "—"}</td
                            >
                          </tr>
                          {#if q.has_bar}
                            <tr>
                              <td colspan="6" class="!border-0 !pt-0 !pb-2">
                                <div
                                  class="h-1.5 w-full rounded-full overflow-hidden {q.near_limit
                                    ? 'bg-[#f5a623]/20'
                                    : 'bg-[var(--fp-inset)]'}"
                                  role="progressbar"
                                  aria-valuenow={q.usage_pct}
                                  aria-valuemin="0"
                                  aria-valuemax="100"
                                  aria-label={$tr(
                                    "{model} session quota {pct}% used",
                                    { model: q.model, pct: q.usage_pct },
                                  )}
                                >
                                  <div
                                    class="h-full rounded-full transition-all duration-300"
                                    style={`width: ${Math.min(100, Math.max(0, q.usage_pct))}%; background: ${q.near_limit ? "#ef4444" : "var(--fp-accent)"}`}
                                  ></div>
                                </div>
                              </td>
                            </tr>
                          {/if}
                        {/each}
                      </tbody>
                    </table>
                  </div>
                {:else}
                  <p class="text-xs text-[var(--fp-dim)] italic">
                    {$tr("No quota data available for this session.")}
                  </p>
                {/if}
              </div>
            </div>
          </Card>
        {/each}
      </div>

      {#if bridgeQuotaCount > 0}
        <p class="text-xs text-[var(--fp-dim)]">
          {$tr(
            "Bridge: {count} client(s) report premium quota — see the Tokens page for bridge details.",
            { count: bridgeQuotaCount },
          )}
        </p>
      {/if}
    {/if}
  {/if}
</div>
