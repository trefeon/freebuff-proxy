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
  import Button from "./Button.svelte";
  import Alert from "./Alert.svelte";
  import EmptyState from "./EmptyState.svelte";
  import {
    tokensData,
    tokensError,
    ensureTokensStore,
    refreshTokens,
    probeAllQuotas,
  } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import {
    formatFreebucks,
    formatAllowanceUsd,
    freebucksResetCountdown,
  } from "../utils/freebucks.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  // Countdown tick: the global reset strip re-renders "resets in" against
  // this clock every second. Refetches nothing on its own.
  let now = $state(Date.now());

  // Auto-probe failure line only: the probe itself is silent (no success
  // banner), and manual probing lives under Dev Tools.
  let probeMsg = $state("");

  // Global reset strip: the first account carrying a daily reset time sets
  // the shared Pacific-midnight countdown for every account on the page.
  const resetSource = $derived(
    (data?.tokens ?? []).find((t) => t.freebucks?.daily?.reset_at),
  );
  const resetAt = $derived(resetSource?.freebucks?.daily?.reset_at ?? "");
  const resetCountdown = $derived(
    resetAt ? freebucksResetCountdown(resetAt, now) : "",
  );

  // Daily window math mirrors FreebucksQuotaBar: spent defaults to
  // limit − remaining when the server only sends the remainder.
  function dailyWin(token) {
    const d = token.freebucks?.daily;
    if (d == null) return null;
    const limit = Number(d.limit ?? 0);
    const remaining = Number(
      d.remaining ?? Math.max(0, limit - Number(d.spent ?? 0)),
    );
    const spent = Number(d.spent ?? Math.max(0, limit - remaining));
    let pct = Number(d.percent_used ?? (limit > 0 ? (spent / limit) * 100 : 0));
    if (!Number.isFinite(pct)) pct = 0;
    pct = Math.min(100, Math.max(0, pct));
    return { limit, remaining, spent, pct };
  }

  function monthlyWin(token) {
    const m = token.freebucks?.monthly;
    if (m == null) return null;
    const limit = Number(m.limit ?? 0);
    const remaining = Number(
      m.remaining ?? Math.max(0, limit - Number(m.spent ?? 0)),
    );
    const spent = Number(m.spent ?? Math.max(0, limit - remaining));
    return { limit, remaining, spent };
  }

  // One-line account summary: daily fraction, wallet, monthly remainder.
  // The "resets in" countdown is intentionally absent here — it renders once
  // in the global strip above, shared for all accounts.
  function accountHeaderLine(token) {
    const fb = token.freebucks;
    if (!fb?.daily) return "";
    const parts = [
      `${formatFreebucks(fb.daily.remaining)}/${formatFreebucks(fb.daily.limit)} ${$tr("Freebucks daily")}`,
    ];
    const walletBalance = fb.wallet?.balance ?? 0;
    if (walletBalance > 0) {
      parts.push(`${formatFreebucks(walletBalance)} ${$tr("in wallet")}`);
    }
    if (fb.monthly != null && fb.monthly.remaining != null) {
      parts.push(
        `${formatAllowanceUsd(fb.monthly.remaining)} ${$tr("monthly usage left")}`,
      );
    }
    return parts.join(" · ");
  }

  function dayCapped(token) {
    return (
      token.requests_per_day_limit > 0 &&
      token.requests_per_day >= token.requests_per_day_limit
    );
  }

  function quotaExempt(token) {
    return Boolean(
      token.freebucks?.quota_exempt ?? token.freebucks?.quotaExempt,
    );
  }

  let unsubStore = null;
  let unsubErr = null;
  let tick = null;
  onMount(() => {
    recordPageVisit("quota");
    const release = ensureTokensStore();
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
</script>

{#if probeMsg}
  <p class="text-xs font-mono text-red-400" role="status">
    {probeMsg}
  </p>
{/if}

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
  {#if resetAt}
    <p
      class="text-xs text-[var(--fp-muted)] font-mono"
      data-testid="reset-strip"
    >
      {$tr("Daily pools reset at")}
      {resetAt} · {$tr("resets in")}
      {resetCountdown} · {$tr("shared for all accounts")}
    </p>
  {/if}
  <ul class="flex flex-col gap-2.5" aria-label={$tr("Accounts")}>
    {#each data.tokens as token, ti (token.index ?? ti)}
      {@const idx = token.index ?? ti}
      {@const daily = dailyWin(token)}
      {@const monthly = monthlyWin(token)}
      {@const balance = token.freebucks?.balance ?? token.freebucks?.Balance}
      <li
        class="rounded border border-[var(--fp-border)] bg-[var(--fp-surface-2)]/30 px-3 py-2.5 flex flex-col gap-1.5 min-w-0"
        data-testid="account-row"
      >
        <div class="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 min-w-0">
          <h3 class="text-sm font-semibold text-[var(--fp-text)]">
            {$tr("Account #{index}", { index: idx + 1 })}
          </h3>
          {#if token.email}
            <span class="text-xs text-[var(--fp-dim)] font-mono truncate"
              >{token.email}</span
            >
          {/if}
          {#if quotaExempt(token)}
            <span
              class="text-[10px] font-mono px-1.5 py-0.5 rounded border text-emerald-400 bg-emerald-500/10 border-emerald-500/20"
              title={$tr(
                "Server-authorized: new sessions stay usable at zero balance",
              )}>{$tr("quota exempt")}</span
            >
          {/if}
          {#if dayCapped(token)}
            <span class="text-[11px] text-[#f5a623] font-medium">
              {$tr("daily limit reached")}
            </span>
          {/if}
        </div>
        {#if token.freebucks}
          <p
            class="text-xs text-[var(--fp-muted)] font-mono"
            data-testid="freebucks-header"
          >
            {accountHeaderLine(token)}
          </p>
          {#if balance != null}
            <p class="fp-num text-xs text-[var(--fp-text)] tabular-nums">
              {$tr("Balance")}
              <span class="text-[var(--fp-accent)]"
                >{formatFreebucks(balance)}</span
              >
            </p>
          {/if}
          {#if daily}
            <div class="flex flex-col gap-1">
              <div
                class="h-[5px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
                role="progressbar"
                aria-valuenow={Math.round(daily.pct)}
                aria-valuemin="0"
                aria-valuemax="100"
                aria-label={$tr("Daily usage {pct}%", {
                  pct: Math.round(daily.pct),
                })}
              >
                <div
                  class="h-full rounded-full transition-all duration-300 bg-emerald-500"
                  style="width: {daily.pct}%"
                ></div>
              </div>
              <p class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
                {$tr("Daily")}
                {$tr("Used")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(daily.spent)}</span
                >
                / {formatFreebucks(daily.limit)}
                • {$tr("Remaining")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(daily.remaining)}</span
                >
              </p>
            </div>
          {/if}
          {#if monthly}
            <p class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
              {$tr("Monthly")}
              {$tr("Used")}
              <span class="text-[var(--fp-text)] font-medium"
                >{formatFreebucks(monthly.spent)}</span
              >
              / {formatFreebucks(monthly.limit)}
            </p>
          {/if}
        {:else}
          <p class="text-xs text-[var(--fp-dim)] italic">
            {$tr("No Freebucks data — run a request or Probe all to populate.")}
          </p>
        {/if}
      </li>
    {/each}
  </ul>
{/if}
