<script module>
  // Visit auto-probe guard (ADR-0025): the mount fires one silent ?auto=1
  // probe after the first tokens load. Module-scoped so a double-mount
  // (HMR / StrictMode-style remount) still fires once per page load; the
  // server throttles pool-wide to one upstream pass per hour regardless.
  let visitAutoProbeSent = false;
</script>

<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { RefreshCw } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import Button from "./Button.svelte";
  import Alert from "./Alert.svelte";
  import EmptyState from "./EmptyState.svelte";
  import FreebucksQuotaBar from "./FreebucksQuotaBar.svelte";
  import Sparkline from "./Sparkline.svelte";
  import Pips from "./Pips.svelte";
  import { SvelteSet } from "svelte/reactivity";
  import { fetchQuotaHistory } from "../utils/history.js";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import {
    tokensData,
    tokensError,
    ensureTokensStore,
    refreshTokens,
    probeAllQuotas,
  } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import { formatLocalDate } from "../utils/format.js";
  import {
    LIMITED_TIER_MODEL_IDS,
    freebucksHeaderLine,
    modelDisplayInfo,
    sortModelsByPrice,
  } from "../utils/freebucks.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  // Countdown tick: refetches nothing on its own; FreebucksQuotaBar and the
  // reset cells re-render "resets in" against this clock every second.
  let now = $state(Date.now());
  // IDs the gateway can actually admit (live /admin/api/models rows with
  // an agent binding). Upstream prices maps carry models this gateway has
  // no binding for — those rows are unusable and stay hidden. Null until
  // the catalog loads, in which case nothing is filtered.
  let usableIds = $state(null);
  // Modelcat display names keyed by model id (same rows, display_name).
  // Row names prefer these over the static MODEL_METADATA table.
  let modelNames = $state({});

  // Probe-all status: the button POSTs /admin/tokens/test-all (zero-cost
  // upstream GET per token, no session claimed) then refetches the store.
  let probePending = $state(false);
  let probeMsg = $state("");
  let probeOk = $state(true);

  async function handleProbeAll() {
    if (probePending) return;
    probePending = true;
    probeMsg = "";
    try {
      await probeAllQuotas();
      probeOk = true;
      probeMsg = $tr("Quotas refreshed from upstream.");
    } catch (e) {
      probeOk = false;
      probeMsg = e.message || $tr("Quota refresh failed.");
    } finally {
      probePending = false;
    }
  }

  // Per-token usage sparklines (ADR-0016): one quota/history fetch per
  // token the first time its card renders, keyed by token index. The model
  // is the live session model, falling back to the first quota row.
  let sparkByIdx = $state({});
  let sparkPending = new SvelteSet();

  function sparkModel(token) {
    return token.session_model ?? token.quota?.[0]?.model ?? null;
  }
  // Served-model scope per token: the live freebucks.prices keys are the
  // models this account can actually start. The widest scope on the page
  // marks full access; narrower scopes read Limited.
  let maxServed = $derived(
    Math.max(
      0,
      ...(data?.tokens ?? []).map(
        (t) =>
          Object.keys(t.freebucks?.prices ?? {}).filter(
            (id) =>
              (usableIds == null || usableIds.has(id)) &&
              (t.access_tier !== "limited" || LIMITED_TIER_MODEL_IDS.has(id)),
          ).length,
      ),
    ),
  );
  $effect(() => {
    for (const token of data?.tokens ?? []) {
      const idx = token.index ?? 0;
      if (idx in sparkByIdx || sparkPending.has(idx)) continue;
      const model = sparkModel(token);
      if (!model) {
        sparkByIdx[idx] = null;
        continue;
      }
      sparkPending.add(idx);
      fetchQuotaHistory(idx, model)
        .then((h) => {
          sparkByIdx[idx] = h.snapshots.length > 1 ? h : null;
        })
        .catch(() => {
          sparkByIdx[idx] = null;
        })
        .finally(() => {
          sparkPending.delete(idx);
        });
    }
  });
  let unsubStore = null;
  let unsubErr = null;
  let tick = null;
  onMount(() => {
    recordPageVisit("quota");
    const release = ensureTokensStore();
    fetchAPI(adminApi.models)
      .then((res) => {
        const rows = res?.models ?? [];
        usableIds = new Set(rows.filter((m) => m.agent).map((m) => m.id));
        const names = {};
        for (const m of rows) {
          if (m?.id && m.display_name) names[m.id] = m.display_name;
        }
        modelNames = names;
      })
      .catch(() => {
        usableIds = null;
      });
    unsubStore = tokensData.subscribe((v) => {
      if (v) {
        data = v;
        loading = false;
        error = "";
        // Visit auto-probe (ADR-0025): one silent ?auto=1 probe after the
        // first tokens load, then the store reload carries the numbers.
        // No success banner; a failure surfaces on the probeMsg error path.
        if (!visitAutoProbeSent) {
          visitAutoProbeSent = true;
          probeAllQuotas({ auto: true }).catch((e) => {
            probeOk = false;
            probeMsg = e.message || $tr("Quota refresh failed.");
          });
        }
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
  // count that currently report freebucks (no bridge cards here).
  const bridgeQuotaCount = $derived(
    (data?.bridge_token_cards ?? []).filter((c) => c.freebucks).length,
  );

  // resetInLabel renders a seconds countdown (e.g. the time until the next
  // Pacific midnight for a day-capped account) as "Xh Ym" / "Xm".
  function resetInLabel(sec) {
    if (!sec || sec <= 0) return "—";
    if (sec < 3600) return `${Math.ceil(sec / 60)}m`;
    const h = Math.floor(sec / 3600);
    const m = Math.ceil((sec % 3600) / 60);
    return m > 0 ? `${h}h ${m}m` : `${h}h`;
  }
</script>

<div class="flex flex-wrap items-center gap-2">
  <Button variant="ghost" onclick={refreshTokens}>
    <RefreshCw size={15} />
    {$tr("Refresh")}
  </Button>
  <Button
    variant="secondary"
    onclick={handleProbeAll}
    disabled={probePending}
    title={$tr(
      "Probe every pooled token against upstream (no session claimed) and reload the quotas.",
    )}
  >
    {#if probePending}
      <RefreshCw size={15} class="animate-spin" />
      <span>{$tr("Probing…")}</span>
    {:else}
      <RefreshCw size={15} />
      <span>{$tr("Probe all")}</span>
    {/if}
  </Button>
</div>
{#if probeMsg}
  <p
    class="text-xs font-mono {probeOk
      ? 'text-[var(--fp-muted)]'
      : 'text-red-400'}"
    role="status"
  >
    {probeMsg}
  </p>
{/if}

<!-- Upstream Accounting Notice (Meet Freebucks — TUI parity) -->
<div
  class="p-4 rounded bg-[var(--fp-surface-2)] border border-[var(--fp-border)] text-xs text-[var(--fp-text)] space-y-2.5"
>
  <div class="flex items-center gap-2">
    <span class="text-emerald-400 font-bold font-mono text-sm select-none"
      >★</span
    >
    <strong class="text-[var(--fp-text)] font-semibold text-sm tracking-wide"
      >{$tr("Meet Freebucks")}</strong
    >
  </div>
  <p class="text-[var(--fp-text)]/90 leading-relaxed font-normal">
    {$tr(
      "Sessions are now bought with Freebucks instead of counted against weekly and monthly limits.",
    )}
  </p>
  <ul class="list-disc list-inside space-y-1 text-[var(--fp-muted)] pl-0.5">
    <li>
      {$tr("A fresh Freebucks pool every day – spend it on any model.")}
    </li>
    <li>
      {$tr(
        "No per-model caps — every served row draws from the same daily pool.",
      )}
    </li>
    <li>
      {$tr(
        "Each model shows its price per hour; the list runs cheapest first.",
      )}
    </li>
  </ul>
  <p class="text-[11px] text-[var(--fp-muted)] font-mono pt-0.5">
    {$tr(
      "Accounts run on daily Freebucks allowances. Per-account daily pools and model pricing below are live upstream values.",
    )}
  </p>
</div>

{#if loading}
  <p class="text-xs text-[var(--fp-dim)] font-mono">{$tr("Loading…")}</p>
{:else if error}
  <div class="flex flex-col gap-3">
    <Alert tone="error" title={$tr("Could not load this page")}>{error}</Alert>
    <div>
      <Button variant="secondary" onclick={refreshTokens}>{$tr("Retry")}</Button
      >
    </div>
  </div>
{:else if !data?.has_tokens || !data?.tokens?.length}
  <EmptyState
    title={$tr("No tokens in pool")}
    description={$tr(
      "Add a token to the pool to see Freebucks allowances and model pricing.",
    )}
  />
{:else if data}
  <div class="grid grid-cols-1 lg:grid-cols-2 gap-5 items-start">
    {#each data.tokens as token, ti (token.index ?? ti)}
      {@const idx = token.index ?? ti}
      {@const streakTarget = token.maturity?.target ?? 7}
      <Card
        title={$tr("Account #{index}", { index: idx + 1 })}
        description={token.session_model
          ? $tr("Session: {model}", { model: token.session_model })
          : ""}
        class="h-full flex flex-col"
      >
        <div class="flex flex-col gap-4">
          {#if token.quota_stale}
            <p class="text-xs text-[#f5a623]">
              {$tr(
                "Last seen {when} — before restart. Refreshes on the next request.",
                {
                  when:
                    formatLocalDate(token.quota_saved_at) ||
                    token.quota_saved_at,
                },
              )}
            </p>
          {/if}
          {#if token.streak > 0}
            <div
              class="rounded border border-[var(--fp-border)] bg-[var(--fp-surface-2)]/40 p-3 flex flex-col gap-1.5"
            >
              <div class="flex flex-wrap items-center justify-between gap-2">
                <div class="flex items-center gap-2">
                  <span
                    class="font-semibold text-xs sm:text-sm text-[var(--fp-text)]"
                  >
                    {token.streak}
                    {$tr("day streak")}
                  </span>
                  <Pips
                    value={token.streak}
                    total={streakTarget}
                    label={$tr("Streak progress: {filled} of {total} days", {
                      filled: Math.min(token.streak, streakTarget),
                      total: streakTarget,
                    })}
                  />
                </div>
                <span
                  class="text-[11px] font-mono px-2 py-0.5 rounded {token.today_used
                    ? 'bg-emerald-500/15 text-emerald-400 border border-emerald-500/30'
                    : 'bg-amber-500/15 text-amber-400 border border-amber-500/30'}"
                >
                  {token.today_used
                    ? $tr("Active today")
                    : $tr("Needs activity today")}
                </span>
              </div>
              {#if token.streak < streakTarget}
                <p class="text-xs text-[var(--fp-muted)]">
                  {$tr(
                    "{count} more day(s) to complete the {total} day streak",
                    {
                      count: streakTarget - token.streak,
                      total: streakTarget,
                    },
                  )}
                </p>
              {:else}
                <p class="text-xs text-emerald-400 font-medium">
                  {$tr("{total} day streak complete", {
                    total: streakTarget,
                  })}
                </p>
              {/if}
            </div>
          {/if}
          {#if token.freebucks}
            <p
              class="text-xs text-[var(--fp-muted)] font-mono"
              data-testid="freebucks-header"
            >
              {freebucksHeaderLine(token.freebucks, now, $tr)}
            </p>
            <FreebucksQuotaBar freebucks={token.freebucks} {now} />
          {/if}
          {#if sparkByIdx[idx]?.snapshots?.length > 1}
            {@const spark = sparkByIdx[idx]}
            {@const lastSnap = spark.snapshots[spark.snapshots.length - 1]}
            <div class="flex items-center gap-3">
              <Sparkline
                values={spark.snapshots.map((s) => s.recent)}
                label={$tr("Session usage history")}
              />
              <p class="text-[11px] text-[var(--fp-dim)] font-mono">
                {$tr("{count} samples · latest {recent}/{limit}", {
                  count: spark.snapshots.length,
                  recent: lastSnap.recent,
                  limit: lastSnap.limit,
                })}
              </p>
            </div>
          {/if}
          {#if token.freebucks?.prices && Object.keys(token.freebucks.prices).length > 0}
            {@const allowedByTier =
              token.access_tier === "limited" ? LIMITED_TIER_MODEL_IDS : null}
            {@const servedModels = sortModelsByPrice(
              Object.keys(token.freebucks.prices).filter(
                (id) =>
                  (usableIds == null || usableIds.has(id)) &&
                  (allowedByTier == null || allowedByTier.has(id)),
              ),
              token.freebucks,
              modelNames,
            ).map((id) => modelDisplayInfo(id, token.freebucks, modelNames))}
            {@const fullAccess = token.access_tier
              ? token.access_tier === "full"
              : servedModels.length > 0 && servedModels.length >= maxServed}
            {#if servedModels.length > 0}
              <div class="space-y-2">
                <div class="flex items-center justify-between gap-2">
                  <h3
                    class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
                  >
                    {$tr("Served models")} ({servedModels.length})
                  </h3>
                  <span
                    class="text-[10px] font-mono px-1.5 py-0.5 rounded border shrink-0 {fullAccess
                      ? 'text-emerald-400 bg-emerald-500/10 border-emerald-500/20'
                      : 'text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]'}"
                  >
                    {fullAccess ? $tr("Full access") : $tr("Limited")}
                  </span>
                </div>
                <ul
                  class="flex flex-col rounded border border-[var(--fp-border)] bg-[var(--fp-surface-2)]/30 divide-y divide-[var(--fp-border)]/50 font-mono text-xs"
                >
                  {#each servedModels as m (m.id)}
                    {@const isActive =
                      token.session_model === m.id ||
                      (!token.session_model && m.id === "z-ai/glm-5.3-flash")}
                    <li class="px-2.5 py-1.5 min-w-0 flex flex-col gap-0.5">
                      <span class="flex items-center justify-between gap-2">
                        <span class="flex items-center gap-1.5 min-w-0">
                          {#if isActive}
                            <span class="text-emerald-400 font-bold select-none"
                              >&gt;</span
                            >
                          {/if}
                          <span class="font-bold text-[var(--fp-text)] truncate"
                            >{m.displayName}</span
                          >
                        </span>
                        <span
                          class="font-semibold shrink-0 {m.price === 0
                            ? 'text-emerald-400'
                            : 'text-[var(--fp-accent)]'}"
                        >
                          {m.price}
                          {$tr("Freebucks/hr")}
                        </span>
                      </span>
                      <span class="flex items-center gap-2 min-w-0">
                        <code class="text-[10px] text-[var(--fp-dim)] truncate"
                          >{m.id}</code
                        >
                        {#if !m.canStart}
                          <span class="text-[10px] text-amber-400 shrink-0"
                            >{$tr("Need {amount} more", {
                              amount: m.shortfall,
                            })}</span
                          >
                        {/if}
                      </span>
                    </li>
                  {/each}
                </ul>
                <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed">
                  {$tr(
                    "Live served models for this account, cheapest first, charged hourly in Freebucks.",
                  )}
                </p>
              </div>
            {/if}
          {/if}

          {#if !token.freebucks}
            <p class="text-xs text-[var(--fp-dim)] italic">
              {$tr(
                "No Freebucks data — run a request or Probe all to populate.",
              )}
            </p>
          {/if}

          <!-- Per-account request limits (MAX_REQUESTS_PER_MINUTE/_DAY):
                 live counters vs caps. The day cap unlocks at Pacific
                 midnight — the same instant upstream resets daily quota. -->
          {#if token.requests_per_day_limit > 0 || token.requests_per_minute_limit > 0}
            <div
              class="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] fp-num text-[var(--fp-dim)]"
            >
              {#if token.requests_per_minute_limit > 0}
                <span
                  title={$tr(
                    "Admitted requests in the last 60s / per-minute cap",
                  )}
                >
                  {$tr("req/min")}
                  <span class="text-[var(--fp-text)] font-medium"
                    >{token.requests_per_minute}</span
                  >/{token.requests_per_minute_limit}
                </span>
              {/if}
              {#if token.requests_per_day_limit > 0}
                <span
                  title={$tr(
                    "Successful requests today / per-day cap — unlocks at Pacific midnight",
                  )}
                >
                  {$tr("req/day")}
                  <span class="text-[var(--fp-text)] font-medium"
                    >{token.requests_per_day}</span
                  >/{token.requests_per_day_limit}
                </span>
                {#if token.requests_per_day >= token.requests_per_day_limit}
                  <span class="text-[#f5a623] font-medium">
                    {$tr("daily limit reached — resets {in}", {
                      in: resetInLabel(token.requests_per_day_reset_in),
                    })}
                  </span>
                {/if}
              {/if}
            </div>
          {/if}
        </div>
      </Card>
    {/each}
  </div>

  {#if bridgeQuotaCount > 0}
    <p class="text-xs text-[var(--fp-dim)]">
      {$tr(
        "Bridge: {count} client(s) report quota — see the Tokens page for bridge details.",
        { count: bridgeQuotaCount },
      )}
    </p>
  {/if}
{/if}
