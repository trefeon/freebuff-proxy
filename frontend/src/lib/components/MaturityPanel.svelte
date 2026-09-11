<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import Card from "./Card.svelte";
  import Alert from "./Alert.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import {
    tokensData as tokensStore,
    tokensError as tokensErrorStore,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import { tr } from "../i18n.js";

  // Streak Maintenance status board (read-only): the nightly run state,
  // one row per account, and the last-run ledger summary. Every control
  // lives elsewhere — the enrolled toggle + Touch-now on the Accounts
  // rows, the dry-run toggle + global touch model in Settings → Advanced.

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let unsubStore = null;
  let unsubErr = null;

  // Global kill-switch state (MATURITY_ENABLED, default true) and dry-run
  // flag (MATURITY_DRY_RUN, default true): display only, wired in
  // Settings → Advanced.
  let globalEnabled = $state(true);
  let globalLoaded = $state(false);
  let dryRun = $state(true);
  // Tonight's maintenance window (RFC3339 absolute instants from the
  // payload): the next-run countdown formats these, so the window math
  // lives in one DST-safe place server-side.
  let windowStart = $state("");
  let windowEnd = $state("");
  // Wall clock for the next-run countdown (30s tick; the 10s poll also
  // refreshes it). One interval for the whole panel, cleared on unmount.
  let nowMs = $state(Date.now());
  let countdownTimer = null;

  function applyTokens(v) {
    if (!v) return;
    data = v;
    if (v.maturity_enabled !== undefined) {
      globalEnabled = Boolean(v.maturity_enabled);
    }
    if (typeof v.maturity_dry_run === "boolean") {
      dryRun = v.maturity_dry_run;
    }
    if (typeof v.maturity_window_start === "string") {
      windowStart = v.maturity_window_start;
    }
    if (typeof v.maturity_window_end === "string") {
      windowEnd = v.maturity_window_end;
    }
    globalLoaded = true;
    error = "";
    loading = false;
  }

  function badgeTone(badge) {
    if (badge === "Mature") return "good";
    if (badge === "Warming") return "warn";
    if (badge === "Cold") return "info";
    return "idle";
  }

  function fmtTime(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    return isNaN(d) ? "—" : d.toLocaleString();
  }

  function fmtCountdown(ms) {
    if (!isFinite(ms) || ms <= 0) return "due now";
    const s = Math.floor(ms / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    if (h >= 24) {
      const d = Math.floor(h / 24);
      const hr = h % 24;
      return hr > 0 ? `in ${d}d ${hr}h` : `in ${d}d`;
    }
    if (h > 0) return `in ${h}h ${m}m`;
    if (m > 0) return `in ${m}m`;
    return `in ${s}s`;
  }

  // --- Pacific-midnight fallback (old servers without the window keys) ---
  // Next Pacific midnight via Intl wall-clock math (DST-safe: the offset is
  // re-resolved for the target date, never a fixed hour).
  function laOffsetMinutes(ts) {
    const dtf = new Intl.DateTimeFormat("en-US", {
      timeZone: "America/Los_Angeles",
      hour12: false,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
    const parts = {};
    for (const p of dtf.formatToParts(new Date(ts))) parts[p.type] = p.value;
    const asUTC = Date.UTC(
      Number(parts.year),
      Number(parts.month) - 1,
      Number(parts.day),
      Number(parts.hour) % 24,
      Number(parts.minute),
      Number(parts.second),
    );
    return Math.round((asUTC - ts) / 60000);
  }

  function pacificMidnight(ts, addDays) {
    const dtf = new Intl.DateTimeFormat("en-CA", {
      timeZone: "America/Los_Angeles",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    });
    const [y, m, d] = dtf.format(new Date(ts)).split("-").map(Number);
    const base = Date.UTC(y, m - 1, d) + addDays * 86400000;
    const bd = new Date(base);
    const off = laOffsetMinutes(base + 8 * 3600000);
    return (
      Date.UTC(bd.getUTCFullYear(), bd.getUTCMonth(), bd.getUTCDate()) -
      off * 60000
    );
  }

  function fallbackWindow(ts) {
    let end = pacificMidnight(ts, 0);
    if (end <= ts) end = pacificMidnight(ts, 1);
    return { start: end - 60 * 60000, end };
  }

  function runWindow() {
    const s = Date.parse(windowStart);
    const e = Date.parse(windowEnd);
    if (isFinite(s) && isFinite(e) && e > s) return { start: s, end: e };
    return fallbackWindow(nowMs);
  }

  function enrolledTokens() {
    return tokens.filter((t) => t.maturity?.enabled);
  }

  function touchedToday(t) {
    const m = t?.maturity;
    if (m?.touch_day && m?.slot_day) return m.touch_day === m.slot_day;
    return !!t?.today_used;
  }

  function slotPast(t) {
    const slot = Date.parse(t?.maturity?.slot ?? "");
    return isFinite(slot) && slot <= nowMs;
  }

  // Per-account tonight status for the board rows: skipped carries the
  // exact ledger reason, touched carries the touch time, eligible means
  // due now inside the window, pending means waiting for the slot/window.
  function rowStatus(t) {
    const m = t?.maturity;
    if (!m?.enabled) return { kind: "unenrolled", text: $tr("Not enrolled") };
    const result = m.last_result ?? "";
    if (result.startsWith("skip:")) {
      return { kind: "skipped", text: `${$tr("Skipped")} · ${result}` };
    }
    if (touchedToday(t)) {
      return {
        kind: "touched",
        text: `${$tr("Touched")} ${fmtTime(m.last_touch)}`,
      };
    }
    const w = runWindow();
    if (nowMs >= w.start && nowMs < w.end && slotPast(t)) {
      return { kind: "eligible", text: $tr("Eligible tonight") };
    }
    return { kind: "pending", text: $tr("Pending") };
  }

  // Exact model id tonight's touch will admit, resolved server-side from
  // the live meter (manual override → premium-short pool head → auto
  // pick → global fallback). Read path only: no probing here.
  function resolvedModel(t) {
    const m = t?.maturity;
    return m?.effective_touch_model || m?.auto_touch_model || "";
  }

  // Last-run ledger summary across enrolled accounts: latest touch time,
  // touch count, and skip counts grouped by exact reason.
  function ledgerSummary(list) {
    let touched = 0;
    let latest = "";
    const skips = {};
    for (const t of list) {
      const m = t.maturity;
      if (m.last_result === "ok") touched += 1;
      else if ((m.last_result ?? "").startsWith("skip:")) {
        skips[m.last_result] = (skips[m.last_result] ?? 0) + 1;
      }
      if (m.last_touch && (!latest || m.last_touch > latest)) {
        latest = m.last_touch;
      }
    }
    const skipped = Object.values(skips).reduce((a, b) => a + b, 0);
    const reasons = Object.entries(skips)
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(([reason, n]) => (n > 1 ? `${reason} ×${n}` : reason));
    return { touched, skipped, reasons, latest };
  }

  function countdownText() {
    const w = runWindow();
    const enrolled = enrolledTokens();
    const skipped = enrolled.filter((t) => touchedToday(t)).length;
    const eligible = enrolled.length - skipped;
    const counts = `${eligible} eligible · ${skipped} skipped`;
    if (nowMs >= w.start && nowMs < w.end) {
      return `In window · ends ${fmtCountdown(w.end - nowMs)} · ${counts}`;
    }
    return `Next run ${fmtCountdown(w.start - nowMs)} · ${counts}`;
  }

  onMount(() => {
    recordPageVisit("maturity");
    countdownTimer = setInterval(() => {
      nowMs = Date.now();
    }, 30000);
    const release = ensureTokensStore();
    unsubStore = tokensStore.subscribe(applyTokens);
    unsubErr = tokensErrorStore.subscribe((err) => {
      if (err) {
        error = err;
        loading = false;
      }
    });
    function onConfigSaved() {
      refreshTokens();
    }
    window.addEventListener("fp-config-saved", onConfigSaved);
    return () => {
      if (countdownTimer) clearInterval(countdownTimer);
      countdownTimer = null;
      release();
      unsubStore?.();
      unsubErr?.();
      window.removeEventListener("fp-config-saved", onConfigSaved);
    };
  });

  const tokens = $derived(data?.tokens ?? []);
  const summary = $derived(ledgerSummary(enrolledTokens()));
</script>

{#if loading}
  <div class="flex flex-col gap-3" aria-hidden="true">
    <div class="skeleton skeleton-text w-1/3"></div>
    <div class="skeleton skeleton-line"></div>
    <div class="skeleton skeleton-line"></div>
  </div>
{:else if error}
  <div class="flex flex-col items-start gap-2">
    <Alert tone="error" title={error} />
  </div>
{:else}
  <Card
    title={$tr("Streak Maintenance")}
    description={$tr(
      "Read-only status for the nightly run. Enroll accounts and fire manual touches from the Accounts tab; dry-run and touch-model knobs live in Settings.",
    )}
  >
    {#snippet actions()}
      <span class="flex shrink-0 flex-nowrap items-center gap-1.5">
        {#if dryRun}
          <StatusBadge tone="warn" status={$tr("Dry run")} />
        {/if}
        {#if globalLoaded && !globalEnabled}
          <StatusBadge tone="bad" status={$tr("Off")} />
        {/if}
      </span>
    {/snippet}
    <div class="flex flex-col gap-2.5">
      <p class="fp-num text-[11px] leading-relaxed text-[var(--fp-dim)]">
        {$tr("Nightly window 23:00–00:00 Pacific")}
        ·
        {$tr("client request activity since the last Pacific reset skips")}
      </p>
      <p
        class="fp-num text-xs text-[var(--fp-muted)]"
        aria-label={$tr("Next maintenance run")}
      >
        {countdownText()}
      </p>
      {#if globalLoaded && !globalEnabled}
        <Alert
          tone="warning"
          title={$tr("Maturity automation is globally off")}
        >
          {$tr(
            "Turn Streak maintenance on in Settings — the rows below stay put while the kill-switch is off. Dry-run probes stay on until the schedule is proven.",
          )}
        </Alert>
      {/if}
      <div
        class="flex flex-col gap-1 border-t border-[var(--fp-border)]/60 pt-2.5"
        aria-label={$tr("Last maintenance run")}
      >
        <p class="fp-num text-[11px] text-[var(--fp-dim)]">
          {$tr("Last run")}
          {fmtTime(summary.latest)} · {$tr("touched")}
          {summary.touched}
          · {$tr("skipped")}
          {summary.skipped}
        </p>
        {#if summary.reasons.length > 0}
          <p class="fp-num text-[11px] text-[var(--fp-dim)]">
            {summary.reasons.join(" · ")}
          </p>
        {/if}
      </div>
      {#if tokens.length === 0}
        <p class="text-sm text-[var(--fp-dim)]">{$tr("No pooled tokens")}</p>
      {:else}
        <div class="flex flex-col gap-1.5">
          {#each tokens as t (t.index ?? t.email)}
            {@const idx = t.index ?? 0}
            {@const m = t.maturity}
            {@const st = rowStatus(t)}
            {@const model = resolvedModel(t)}
            <div
              class="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-[var(--fp-border)]/60 pt-2"
            >
              <span class="min-w-0">
                <span class="fp-num text-xs font-semibold text-[var(--fp-text)]"
                  >{$tr("Account #{idx}", { idx: idx + 1 })}</span
                >
                {#if t.email}
                  <span
                    class="ml-1.5 text-[11px] text-[var(--fp-muted)] truncate"
                    title={t.email}>{t.email}</span
                  >
                {/if}
              </span>
              {#if m?.badge}
                <StatusBadge tone={badgeTone(m.badge)} status={m.badge} />
              {:else}
                <StatusBadge tone="idle" status={$tr("Not enrolled")} />
              {/if}
              {#if t.locked}
                <StatusBadge tone="warn" status={$tr("Locked")} />
              {/if}
              <span class="fp-num text-[11px] text-[var(--fp-dim)]"
                >{st.text}</span
              >
              {#if model}
                <code
                  class="fp-num ml-auto text-[11px] text-[var(--fp-muted)]"
                  title={$tr("Touch model for tonight")}>{model}</code
                >
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </Card>
{/if}
