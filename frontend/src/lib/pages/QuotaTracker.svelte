<script>
  import { onMount } from "svelte";
  import { RefreshCw, Info } from "@lucide/svelte";
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
    (data?.bridge_token_cards ?? []).filter(
      (c) => c.premium_quota || c.freebucks,
    ).length,
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

  <!-- Upstream Accounting Transition Notice (issue #332/#335): one subtle
       page-level card in the header zone — not repeated inside every
       account card. -->
  <div
    class="p-2.5 rounded-lg bg-[var(--fp-accent)]/10 border border-[var(--fp-accent)]/20 text-xs text-[var(--fp-muted)] flex items-start gap-2"
  >
    <Info size={14} class="shrink-0 mt-0.5 text-[var(--fp-accent)]" />
    <div>
      <strong class="text-[var(--fp-text)] font-medium block mb-0.5"
        >{$tr("Upstream Accounting Revamp (Freebucks)")}</strong
      >
      {$tr(
        "Codebuff is transitioning accounts from the legacy 4 sessions/day pool to daily Freebucks allowances (~75 Freebucks/day). Legacy accounts stay on the daily session pool until upstream finishes rollout.",
      )}
    </div>
  </div>

  {#if loading}
    <div
      class="grid grid-cols-1 lg:grid-cols-2 gap-5 items-start"
      aria-busy="true"
    >
      {#each Array(4) as _, i (i)}
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
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-5 items-start">
        {#each data.tokens as token, ti (token.index ?? ti)}
          {@const idx = token.index ?? ti}
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
                  <div
                    class="flex flex-wrap items-center justify-between gap-2"
                  >
                    <div class="flex items-center gap-2">
                      <span
                        class="font-semibold text-xs sm:text-sm text-[var(--fp-text)]"
                      >
                        {token.streak}
                        {$tr("day streak")}
                      </span>
                      <span
                        class="font-mono text-sm tracking-widest text-[var(--fp-accent)] select-none"
                        aria-label={$tr("Streak progress: {filled} of 7 days", {
                          filled: Math.min(token.streak, 7),
                        })}
                      >
                        {"●".repeat(Math.min(token.streak, 7)) +
                          "○".repeat(Math.max(0, 7 - token.streak)) +
                          (token.streak > 7 ? "+" : "")}
                      </span>
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
                  {#if token.streak < 7}
                    <p class="text-xs text-[var(--fp-muted)]">
                      🎁 {$tr(
                        "{count} more day(s) to unlock +1 bonus session every day",
                        { count: 7 - token.streak },
                      )}
                    </p>
                  {:else}
                    <p class="text-xs text-[var(--fp-accent)]">
                      🎁 {$tr("Streak perk: +1 bonus session every day")}
                    </p>
                  {/if}
                </div>
              {/if}
              {#if token.freebucks}
                <PremiumQuotaBar
                  freebucks={token.freebucks}
                  freeWindows={token.free_windows}
                  subscription={token.subscription}
                  {now}
                />
              {:else if token.premium_quota}
                <PremiumQuotaBar
                  quota={token.premium_quota}
                  freeWindows={token.free_windows}
                  subscription={token.subscription}
                  {now}
                />
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
                  <div class="hidden xl:block overflow-x-auto">
                    <table class="fp-table">
                      <caption class="sr-only"
                        >{$tr("Session quota by model for account {index}", {
                          index: idx + 1,
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
                  <!-- Narrow cards (< xl, incl. the lg 2-col grid): stacked per-model entries, no horizontal scrolling -->
                  <ul
                    class="xl:hidden flex flex-col gap-2.5"
                    aria-label={$tr(
                      "Session quota by model for account {index}",
                      {
                        index: idx + 1,
                      },
                    )}
                  >
                    {#each token.quota as q (q.model)}
                      <li
                        class="fp-inset rounded-lg p-3 flex flex-col gap-2 min-w-0"
                      >
                        <div
                          class="flex items-start justify-between gap-2 min-w-0"
                        >
                          <code
                            class="fp-num text-xs break-all min-w-0 {q.near_limit
                              ? 'text-[#f5a623] font-medium'
                              : 'text-[var(--fp-text)]'}">{q.model}</code
                          >
                          <span
                            class="fp-num text-xs shrink-0 rounded-full border border-[var(--fp-border)] px-2 py-0.5 {q.near_limit
                              ? 'text-[#f5a623]'
                              : 'text-[var(--fp-muted)]'}"
                            aria-label={$tr(
                              "{recent} of {limit} sessions used",
                              {
                                recent: q.recent,
                                limit: q.limit,
                              },
                            )}>{q.recent}/{q.limit}</span
                          >
                        </div>
                        {#if q.has_bar}
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
                        {/if}
                        <dl
                          class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs min-w-0"
                        >
                          <dt class="text-[var(--fp-dim)]">{$tr("reset")}</dt>
                          <dd
                            class="fp-num text-right break-words min-w-0 {q.near_limit
                              ? 'text-[#f5a623] font-medium'
                              : 'text-[var(--fp-dim)]'}"
                          >
                            {formatLocalDate(q.reset_at_utc) ||
                              q.reset_at}{#if q.resets_in}
                              ({q.resets_in}){/if}
                          </dd>
                          <dt class="text-[var(--fp-dim)]">{$tr("period")}</dt>
                          <dd
                            class="fp-num text-right break-words min-w-0 {q.near_limit
                              ? 'text-[#f5a623]'
                              : 'text-[var(--fp-dim)]'}"
                          >
                            {q.period}
                          </dd>
                          <dt class="text-[var(--fp-dim)]">
                            {$tr("entitlement")}
                          </dt>
                          <dd
                            class="fp-num text-right break-words min-w-0 {q.near_limit
                              ? 'text-[#f5a623]'
                              : 'text-[var(--fp-dim)]'}"
                          >
                            {q.has_entitlement ? q.entitled : "—"}
                          </dd>
                        </dl>
                      </li>
                    {/each}
                  </ul>
                {:else}
                  <p class="text-xs text-[var(--fp-dim)] italic">
                    {$tr("No quota data available for this session.")}
                  </p>
                {/if}
              </div>

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
            "Bridge: {count} client(s) report premium quota — see the Tokens page for bridge details.",
            { count: bridgeQuotaCount },
          )}
        </p>
      {/if}
    {/if}
  {/if}
</div>
