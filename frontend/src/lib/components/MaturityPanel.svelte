<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { SvelteDate, SvelteSet } from "svelte/reactivity";
  import Card from "./Card.svelte";
  import Alert from "./Alert.svelte";
  import Button from "./Button.svelte";
  import ToggleSwitch from "./ToggleSwitch.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import Pips from "./Pips.svelte";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi, tokenActions } from "../api/paths.js";
  import { fetchMaturityHistory, historyKindTone } from "../utils/history.js";
  import {
    touchOptions as sharedTouchOptions,
    touchLabel,
    touchPriceFor,
  } from "../utils/touchModels.js";
  import {
    tokensData as tokensStore,
    tokensError as tokensErrorStore,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import { getEnvValue } from "../utils/env.js";
  import { tr } from "../i18n.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let unsubStore = null;
  let unsubErr = null;

  // Global kill-switch state (MATURITY_ENABLED, default true). The master
  // switch below writes it through the settings overlay; Settings renders
  // the same knob from the catalog (both stay identical).
  let globalEnabled = $state(true);
  let globalLoaded = $state(false);
  let savingGlobal = $state(false);
  // Dry-run flag (MATURITY_DRY_RUN) for the badge: tokens payload first,
  // config effective fallback, default true (probe-only until proven).
  let dryRun = $state(true);
  // Resolved global MATURITY_TOUCH_MODEL: effective snapshot first, raw .env
  // fallback, "" = unknown. "auto"/"" is the sentinel for automatic
  // resolution (cheapest served unmetered row); an explicit id overrides.
  let globalTouchModel = $state("");
  let savingTouchModel = $state(false);
  // Tonight's maintenance window (RFC3339 absolute instants from the
  // payload): the next-run countdown formats these, so the window math
  // lives in one DST-safe place server-side.
  let windowStart = $state("");
  let windowEnd = $state("");
  // Wall clock for the next-run countdown (30s tick; the 10s poll also
  // refreshes it). One interval for the whole panel, cleared on unmount.
  let nowMs = $state(Date.now());
  let countdownTimer = null;

  function isAutoSentinel(v) {
    const s = String(v ?? "")
      .trim()
      .toLowerCase();
    return s === "" || s === "auto";
  }

  // Touch-model candidates: served models the gateway can admit (same
  // usable filter as Quota Tracker). Server order is cheapest-Freebucks-cost
  // first; each option is labeled with its server-reported cost class —
  // never an invented price.
  let modelRows = $state([]);

  // Per-token busy flags, keyed by token index.
  let enrolling = $state({});
  let touching = $state({});
  let actionMessage = $state("");
  let actionOK = $state(true);

  // Restart-surviving event timelines (ADR-0016): loaded once per token
  // when its row renders, never on the 10s poll.
  let histByIdx = $state({});
  // History fold state per row (default folded, latest event visible).
  let histOpen = $state({});
  let histPending = new SvelteSet();

  $effect(() => {
    for (const t of tokens) {
      const idx = t.index ?? 0;
      if (!t.maturity || idx in histByIdx || histPending.has(idx)) continue;
      histPending.add(idx);
      fetchMaturityHistory(idx)
        .then((h) => {
          histByIdx[idx] = h.events;
        })
        .catch(() => {
          histByIdx[idx] = [];
        })
        .finally(() => {
          histPending.delete(idx);
        });
    }
  });

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

  // Global touch options live in utils/touchModels.js (shared with the
  // Settings → Advanced global MATURITY_TOUCH_MODEL select so both
  // dropdowns stay identical).
  function globalTouchOpts() {
    return sharedTouchOptions(modelRows, globalTouchModel);
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

  function dayKey(ts) {
    const d = new Date(Number(ts));
    if (isNaN(d)) return "";
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  }

  // Done/pending day-strip from the existing maturity history records:
  // the last 7 calendar days, done when a touch event landed that day
  // (or today already counts via today_used), pending otherwise.
  function weekStrip(t, idx) {
    const days = [];
    const base = new SvelteDate(nowMs);
    base.setHours(0, 0, 0, 0);
    const touched = new SvelteSet();
    for (const ev of histByIdx[idx] ?? []) {
      if (ev?.kind === "touch") touched.add(dayKey(ev.ts));
    }
    for (let back = 6; back >= 0; back--) {
      const d = new Date(base.getTime() - back * 86400000);
      const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
      const isToday = back === 0;
      const done = touched.has(key) || (isToday && !!t?.today_used);
      days.push({ key, isToday, done });
    }
    return days;
  }

  function parseTouchModel(detail) {
    const s = String(detail ?? "");
    const i = s.indexOf("model=");
    if (i < 0) return "";
    return s.slice(i + 6).split(/[\s,;]+/)[0] ?? "";
  }

  function fmtSpend(n) {
    const v = Math.round(Number(n) * 10) / 10;
    if (!isFinite(v)) return "—";
    return `${Number.isInteger(v) ? v : v.toFixed(1)} Freebucks`;
  }

  // Warming ledger from the existing history records: touches today, the
  // week's warming Freebucks spend (0 by design for unmetered touches —
  // only admitted touches on priced rows add up), and the projected
  // monthly burn at the current cadence.
  function ledgerFor(t, idx) {
    const evs = histByIdx[idx] ?? [];
    const today = dayKey(nowMs);
    let touchesToday = 0;
    let spendWeek = 0;
    const weekAgo = Number(nowMs) - 7 * 86400000;
    for (const ev of evs) {
      if (ev?.kind !== "touch") continue;
      const ts = Number(ev?.ts);
      if (!isFinite(ts) || ts < weekAgo) continue;
      const detail = String(ev?.detail ?? "");
      const isAdmitOk =
        detail.includes("admit") && /(^|\s)ok(\s|$)/.test(detail);
      if (dayKey(ts) === today) touchesToday += 1;
      if (!isAdmitOk) continue;
      const price = touchPriceFor(parseTouchModel(detail), modelRows);
      if (isFinite(price) && price > 0) spendWeek += price;
    }
    if (touchesToday === 0 && touchedToday(t)) touchesToday = 1;
    const projected = Math.round(spendWeek * (30 / 7) * 10) / 10;
    return { touchesToday, spendWeek, projected };
  }

  function touchedToday(t) {
    const m = t?.maturity;
    if (m?.touch_day && m?.slot_day) return m.touch_day === m.slot_day;
    return !!t?.today_used;
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

  function skippedToday(list) {
    return list.filter((t) => touchedToday(t));
  }

  function countdownText() {
    const w = runWindow();
    const enrolled = enrolledTokens();
    const skipped = skippedToday(enrolled).length;
    const eligible = enrolled.length - skipped;
    const counts = `${eligible} eligible · ${skipped} skipped`;
    if (nowMs >= w.start && nowMs < w.end) {
      return `In window · ends ${fmtCountdown(w.end - nowMs)} · ${counts}`;
    }
    return `Next run ${fmtCountdown(w.start - nowMs)} · ${counts}`;
  }

  // Enroll/dis-enroll one account (the only per-account control left):
  // target 0 selects the global MATURITY_TARGET_DAYS default.
  async function setEnrolled(idx, next) {
    if (enrolling[idx]) return;
    enrolling[idx] = true;
    actionMessage = "";
    try {
      const res = await postAPI(tokenActions.maturity(idx), {
        enabled: next,
        target: 0,
      });
      if (res && res.ok === false)
        throw new Error(res.message || "Save rejected");
      actionOK = true;
      actionMessage = $tr("Maturity saved for Account #{idx}", {
        idx: idx + 1,
      });
      await refreshTokens();
    } catch (e) {
      actionOK = false;
      actionMessage = e?.message || String(e);
    } finally {
      enrolling[idx] = false;
    }
  }

  async function touchNow(idx) {
    if (touching[idx]) return;
    touching[idx] = true;
    actionMessage = "";
    try {
      const res = await postAPI(tokenActions.maturityTouch(idx), {});
      if (res && res.ok === false)
        throw new Error(res.message || "Touch rejected");
      actionOK = true;
      actionMessage = $tr("Touch fired for Account #{idx}", { idx: idx + 1 });
      await refreshTokens();
    } catch (e) {
      actionOK = false;
      actionMessage = e?.message || String(e);
    } finally {
      touching[idx] = false;
    }
  }

  // Global kill-switch + touch model write through the settings overlay
  // (same path as Settings → Advanced, hot-applied on save).
  async function setGlobalEnabled(next) {
    if (savingGlobal) return;
    savingGlobal = true;
    actionMessage = "";
    try {
      const res = await postAPI(adminApi.settingsSave, {
        key: "MATURITY_ENABLED",
        value: next ? "true" : "false",
      });
      if (res && res.ok === false)
        throw new Error(res.message || "Save rejected");
      globalEnabled = next;
      actionOK = true;
      actionMessage = next
        ? $tr("Streak maintenance enabled")
        : $tr("Streak maintenance disabled");
      await Promise.all([refreshTokens(), loadConfigFlags()]);
    } catch (e) {
      actionOK = false;
      actionMessage = e?.message || String(e);
    } finally {
      savingGlobal = false;
    }
  }

  async function setGlobalTouchModel(id) {
    if (savingTouchModel) return;
    savingTouchModel = true;
    actionMessage = "";
    try {
      const res = await postAPI(adminApi.settingsSave, {
        key: "MATURITY_TOUCH_MODEL",
        value: id,
      });
      if (res && res.ok === false)
        throw new Error(res.message || "Save rejected");
      globalTouchModel = id;
      actionOK = true;
      actionMessage = $tr("Touch model saved");
      await Promise.all([refreshTokens(), loadConfigFlags()]);
    } catch (e) {
      actionOK = false;
      actionMessage = e?.message || String(e);
    } finally {
      savingTouchModel = false;
    }
  }

  async function loadConfigFlags() {
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const content = cfgRes?.env_content || "";
      const eff = (cfgRes?.effective || []).find(
        (e) => e.key === "MATURITY_ENABLED",
      );
      if (eff) {
        const v = String(eff.value).trim().toLowerCase();
        globalEnabled = v === "true" || v === "1" || v === "on" || v === "yes";
      } else {
        const raw = getEnvValue(content, "MATURITY_ENABLED");
        if (raw !== null && raw !== undefined && raw !== "") {
          const v = String(raw).trim().toLowerCase();
          globalEnabled =
            v === "true" || v === "1" || v === "on" || v === "yes";
        } else {
          globalEnabled = true;
        }
      }
      // Effective snapshot wins (it reflects the live value incl. any DB
      // overlay); fall back to the raw .env line. Masked secret-style
      // display values ("N token(s)") never name a model, so ignore them.
      const effTouch = (cfgRes?.effective || []).find(
        (e) => e.key === "MATURITY_TOUCH_MODEL",
      );
      const effRaw =
        effTouch?.value !== undefined && effTouch?.value !== null
          ? String(effTouch.value).trim()
          : "";
      const envRaw = (
        getEnvValue(content, "MATURITY_TOUCH_MODEL") ?? ""
      ).trim();
      const resolved = effRaw && !effRaw.includes("(") ? effRaw : envRaw;
      globalTouchModel = resolved.includes("(") ? "" : resolved;
      if (typeof data?.maturity_dry_run !== "boolean") {
        const effDry = (cfgRes?.effective || []).find(
          (e) => e.key === "MATURITY_DRY_RUN",
        );
        if (effDry) {
          const v = String(effDry.value).trim().toLowerCase();
          dryRun = v === "true" || v === "1" || v === "on" || v === "yes";
        } else {
          const rawDry = getEnvValue(content, "MATURITY_DRY_RUN");
          dryRun =
            rawDry === null || rawDry === undefined || rawDry === ""
              ? true
              : ["true", "1", "on", "yes"].includes(
                  String(rawDry).trim().toLowerCase(),
                );
        }
      }
    } catch {
      globalEnabled = false;
    } finally {
      globalLoaded = true;
    }
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
    loadConfigFlags();
    (async () => {
      try {
        const res = await fetchAPI(adminApi.models);
        modelRows = res?.models ?? [];
      } catch {
        modelRows = [];
      }
    })();
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
    <Button
      variant="secondary"
      onclick={() => {
        error = "";
        refreshTokens();
      }}
    >
      {$tr("Retry")}
    </Button>
  </div>
{:else}
  {#if actionMessage}
    <Alert tone={actionOK ? "success" : "error"} title={actionMessage} />
  {/if}

  <Card
    title={$tr("Streak Maintenance")}
    description={$tr(
      "One nightly run touches every enrolled account right before the Pacific-midnight reset.",
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
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
        <ToggleSwitch
          checked={globalEnabled}
          disabled={!!savingGlobal}
          saving={!!savingGlobal}
          ariaLabel={$tr("Streak maintenance")}
          onchange={(next) => setGlobalEnabled(next)}
        />
        <label class="flex min-w-0 flex-1 flex-col gap-1 sm:max-w-xs">
          <span class="fp-num text-[11px] text-[var(--fp-dim)]"
            >{$tr("Touch model")}</span
          >
          <select
            class="fp-select !h-8 !py-1 !text-xs font-mono w-full min-w-0"
            value={isAutoSentinel(globalTouchModel) ? "auto" : globalTouchModel}
            disabled={!!savingTouchModel}
            aria-label={$tr("Global touch model")}
            title={$tr(
              "Global touch model for the nightly run. Auto admits the cheapest served unmetered row per account.",
            )}
            onchange={(e) => setGlobalTouchModel(e.currentTarget.value)}
          >
            <option value="auto">Auto (cheapest unmetered)</option>
            {#each globalTouchOpts() as o (o.id)}
              <option value={o.id}>{touchLabel(o)}</option>
            {/each}
          </select>
        </label>
      </div>
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
            "Turn Streak maintenance on above — enrolled toggles below do nothing while the kill-switch is off. Dry-run probes stay on until the schedule is proven.",
          )}
        </Alert>
      {/if}
    </div>
  </Card>

  {#if tokens.length === 0}
    <p class="text-sm text-[var(--fp-dim)]">{$tr("No pooled tokens")}</p>
  {:else}
    <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
      {#each tokens as t (t.index ?? t.email)}
        {@const idx = t.index ?? 0}
        {@const m = t.maturity}
        <Card
          title={$tr("Account #{idx}", { idx: idx + 1 })}
          description={t.email || $tr("unknown account")}
        >
          {#snippet actions()}
            {@const streakTarget = m?.target ?? 7}
            <span class="flex shrink-0 flex-nowrap items-center gap-1.5">
              {#if m?.badge}
                <StatusBadge tone={badgeTone(m.badge)} status={m.badge} />
              {:else}
                <StatusBadge tone="idle" status={$tr("Not enrolled")} />
              {/if}
              {#if t.locked}
                <StatusBadge tone="warn" status={$tr("Locked")} />
              {/if}
              {#if m?.warn}
                <StatusBadge tone="bad" status={$tr("Touch not advancing")} />
              {/if}
              <Pips
                value={t.streak ?? 0}
                total={streakTarget}
                label={m
                  ? $tr(
                      "Daily touches banked toward the target (streak/target)",
                    )
                  : $tr(
                      "Streak/target counts daily touches once enrolled — nothing banked yet",
                    )}
              />
            </span>
          {/snippet}
          <div class="flex flex-col gap-2">
            <p
              class="fp-num inline-flex items-center gap-1 text-[11px] text-[var(--fp-dim)]"
              title={$tr(
                "Daily touches banked toward the target (streak/target)",
              )}
            >
              {$tr("day {n}/{target}", {
                n: t.streak ?? 0,
                target: m?.target ?? 7,
              })}
              {#if t.today_used}
                <span class="text-emerald-400">· {$tr("Active today")}</span>
              {:else if m?.enabled && !m.warn}
                <span>· {$tr("Needs activity today")}</span>
              {/if}
            </p>
            {#if m}
              <p
                class="fp-num text-[11px] leading-relaxed text-[var(--fp-dim)]"
              >
                {m.last_action
                  ? `${m.last_action} → ${m.last_result ?? "?"}`
                  : $tr("no touch yet")}{m.last_touch
                  ? ` · ${fmtTime(m.last_touch)}`
                  : ""}{m.last_advanced
                  ? ` · ${$tr("advanced")} ${m.last_advanced}`
                  : ""}
              </p>
              {@const strip = weekStrip(t, idx)}
              {@const ledger = ledgerFor(t, idx)}
              <div
                class="flex items-center gap-1"
                role="img"
                aria-label={$tr("Touches last 7 days for Account #{idx}", {
                  idx: idx + 1,
                })}
              >
                {#each strip as day (day.key)}
                  <span
                    title={day.key +
                      (day.done
                        ? " · touched"
                        : day.isToday
                          ? " · pending today"
                          : "")}
                    class={`h-1.5 w-5 rounded-full ${day.done ? "bg-emerald-400/80" : day.isToday ? "bg-amber-400/70" : "bg-[var(--fp-border)]"}`}
                  ></span>
                {/each}
              </div>
              <p class="fp-num text-[11px] text-[var(--fp-dim)]">
                Touches today {ledger.touchesToday} · Warming spend (7d) {fmtSpend(
                  ledger.spendWeek,
                )} · Projected/mo {fmtSpend(ledger.projected)}
              </p>
            {/if}
            <div
              class="flex flex-wrap items-center justify-between gap-2 border-t border-[var(--fp-border)]/60 pt-2.5"
            >
              <ToggleSwitch
                checked={!!m?.enabled}
                disabled={!!enrolling[idx]}
                saving={!!enrolling[idx]}
                ariaLabel={$tr("Maturity for Account #{idx}", {
                  idx: idx + 1,
                })}
                onchange={(next) => setEnrolled(idx, next)}
              />
              <span class="flex flex-wrap gap-1.5">
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={!!touching[idx] || !m?.enabled}
                  loading={!!touching[idx]}
                  onclick={() => touchNow(idx)}
                  title={$tr("Fire one touch now (manual override)")}
                >
                  {$tr("Touch now")}
                </Button>
              </span>
            </div>
            {#if (histByIdx[idx] ?? []).length > 0}
              {@const evs = (histByIdx[idx] ?? []).slice(-5)}
              {@const open = !!histOpen[idx]}
              <ul
                class="flex flex-col gap-1.5 border-t border-[var(--fp-border)]/60 pt-2.5"
                aria-label={$tr("Maturity history for Account #{idx}", {
                  idx: idx + 1,
                })}
              >
                {#each open ? evs : evs.slice(-1) as ev (ev.ts + ev.kind + ev.detail)}
                  <li
                    class="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs"
                  >
                    <StatusBadge
                      tone={historyKindTone(ev.kind)}
                      status={ev.kind}
                    />
                    <span class="text-[var(--fp-muted)] break-words min-w-0"
                      >{ev.detail}</span
                    >
                    <span class="fp-num text-[var(--fp-dim)] ml-auto shrink-0"
                      >{fmtTime(new Date(ev.ts).toISOString())}</span
                    >
                  </li>
                {/each}
              </ul>
              {#if evs.length > 1}
                <button
                  type="button"
                  class="self-start text-xs font-mono text-[var(--fp-dim)] hover:text-[var(--fp-fg)]"
                  onclick={() => (histOpen[idx] = !open)}
                  aria-expanded={open}
                >
                  {open
                    ? $tr("Show less")
                    : $tr("Show {n} more", { n: evs.length - 1 })}
                </button>
              {/if}
            {/if}
          </div>
        </Card>
      {/each}
    </div>
  {/if}
{/if}
