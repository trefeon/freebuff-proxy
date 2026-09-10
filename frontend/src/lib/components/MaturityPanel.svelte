<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { SvelteDate, SvelteSet } from "svelte/reactivity";
  import FieldBox from "./FieldBox.svelte";
  import Stepper from "./Stepper.svelte";
  import Pips from "./Pips.svelte";
  import Card from "./Card.svelte";
  import Alert from "./Alert.svelte";
  import Button from "./Button.svelte";
  import ToggleSwitch from "./ToggleSwitch.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi, tokenActions } from "../api/paths.js";
  import { fetchMaturityHistory, historyKindTone } from "../utils/history.js";
  import {
    touchOptions as sharedTouchOptions,
    touchLabel,
    autoTouchPick,
    autoTouchReason,
    touchCostFor,
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

  // Global kill-switch state (MATURITY_ENABLED, default true) for the honest
  // header notice. Settings renders the toggle itself (catalog Essential).
  let globalEnabled = $state(true);
  let globalLoaded = $state(false);
  // Resolved global MATURITY_TOUCH_MODEL: effective snapshot first, raw .env
  // fallback, "" = unknown. "auto"/"" is the sentinel for automatic
  // resolution (cheapest served unmetered row); an explicit id is the
  // fallback the server uses when auto has no unmetered candidate.
  let globalTouchModel = $state("");
  // Wall clock for the next-touch countdown (30s tick; the 10s poll also
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
  // usable filter as Quota Tracker: a live agent binding, no withdrawn
  // rows, no referral-grant row). Server order is cheapest-Freebucks-cost
  // first, so unmetered-capable rows lead with the premium pool last;
  // each option is labeled with its server-reported cost class
  // (price_label/quota/pool) — never an invented price.
  let modelRows = $state([]);

  // Per-token draft controls + busy flags, keyed by token index.
  let drafts = $state({});
  let saving = $state({});
  let touching = $state({});
  let resetting = $state({});
  let actionMessage = $state("");
  let actionOK = $state(true);

  // Restart-surviving event timelines (ADR-0016): loaded once per token
  // when its card renders (cards are always expanded), never on the
  // 10s poll.
  let histByIdx = $state({});
  // History fold state per card (default folded, latest event visible).
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
    for (const t of v?.tokens ?? []) {
      const idx = t.index ?? 0;
      if (!(idx in drafts)) {
        drafts[idx] = {
          enabled: !!t.maturity?.enabled,
          target: t.maturity?.target ?? 7,
          // No UI: the Touch box is model-select-only, the server value rides
          // along on save so an enabled token never resets to unmetered.
          mode: t.maturity?.mode ?? "unmetered",
          touchModel: t.maturity?.touch_model ?? "",
        };
      }
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

  // Touch-model options live in utils/touchModels.js (shared with the
  // Settings → Advanced global MATURITY_TOUCH_MODEL select so both
  // dropdowns stay identical).
  function touchOptions(d) {
    return sharedTouchOptions(modelRows, d?.touchModel ?? "");
  }

  function exemptFor(t) {
    return !!t?.freebucks?.quota_exempt;
  }

  // Server-resolved Auto pick first (payload), local cheapest-unmetered
  // fallback for old servers that predate the new keys.
  function autoFor(t) {
    return (
      t?.maturity?.auto_touch_model ||
      autoTouchPick(modelRows, exemptFor(t)) ||
      ""
    );
  }

  function autoReasonFor(t) {
    return (
      t?.maturity?.auto_touch_reason || autoTouchReason(modelRows, exemptFor(t))
    );
  }

  // Effective model preview: the manual draft when set, else the server's
  // resolved model, else the local auto pick, else the explicit global.
  function effectiveFor(t, d) {
    if (d?.touchModel) return d.touchModel;
    return (
      t?.maturity?.effective_touch_model ||
      autoFor(t) ||
      (isAutoSentinel(globalTouchModel) ? "" : globalTouchModel)
    );
  }

  function costFor(modelId) {
    return touchCostFor(modelId, modelRows);
  }

  // First-option label: Auto pick with its reason under the new default,
  // the explicit global fallback otherwise (kept verbatim for the
  // Settings deep-link contract).
  function autoOptionLabel() {
    if (isAutoSentinel(globalTouchModel)) {
      const pick = autoTouchPick(modelRows, false);
      return pick ? `Auto (${pick})` : "Auto";
    }
    return null;
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

  function touchedToday(t) {
    const m = t?.maturity;
    if (m?.touch_day && m?.slot_day) return m.touch_day === m.slot_day;
    return !!t?.today_used;
  }

  function nextTouchText(t) {
    const m = t?.maturity;
    if (!m) return "";
    if (!m.enabled) return "Automation off";
    if (touchedToday(t)) return "Touched today · next slot tomorrow";
    if (m.slot) {
      const at = new Date(m.slot).getTime();
      if (!isNaN(at)) return fmtCountdown(at - nowMs);
    }
    return "due now";
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
  // only admitted touches on priced rows add up, which surfaces a
  // premium-short fallback the moment it occurs), and the projected
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

  async function save(idx) {
    if (saving[idx]) return;
    saving[idx] = true;
    actionMessage = "";
    try {
      const d = drafts[idx];
      const res = await postAPI(tokenActions.maturity(idx), {
        enabled: d.enabled,
        target: Number(d.target) || 7,
        mode: d.mode,
        touch_model: d.touchModel ?? "",
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
      saving[idx] = false;
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

  async function resetWarn(idx) {
    if (resetting[idx]) return;
    resetting[idx] = true;
    actionMessage = "";
    try {
      const res = await postAPI(tokenActions.maturityWarnReset(idx), {});
      if (res && res.ok === false)
        throw new Error(res.message || "Reset rejected");
      actionOK = true;
      actionMessage = $tr("Warning cleared for Account #{idx}", {
        idx: idx + 1,
      });
      await refreshTokens();
    } catch (e) {
      actionOK = false;
      actionMessage = e?.message || String(e);
    } finally {
      resetting[idx] = false;
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
    (async () => {
      try {
        const cfgRes = await fetchAPI(adminApi.config);
        const content = cfgRes?.env_content || "";
        const eff = (cfgRes?.effective || []).find(
          (e) => e.key === "MATURITY_ENABLED",
        );
        if (eff) {
          const v = String(eff.value).trim().toLowerCase();
          globalEnabled =
            v === "true" || v === "1" || v === "on" || v === "yes";
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
      } catch {
        globalEnabled = false;
      } finally {
        globalLoaded = true;
      }
    })();
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

  {#if globalLoaded && !globalEnabled}
    <Alert tone="warning" title={$tr("Maturity automation is globally off")}>
      {$tr(
        "Set MATURITY_ENABLED=1 in Settings — per-token toggles below do nothing while the kill-switch is off. Dry-run probes stay on until the schedule is proven.",
      )}
    </Alert>
  {/if}

  {#if tokens.length === 0}
    <p class="text-sm text-[var(--fp-dim)]">{$tr("No pooled tokens")}</p>
  {:else}
    <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
      {#each tokens as t (t.index ?? t.email)}
        {@const idx = t.index ?? 0}
        {@const m = t.maturity}
        {@const d = drafts[idx] ?? {
          enabled: false,
          target: 7,
          mode: "unmetered",
          touchModel: "",
        }}
        {@const autoLbl = autoOptionLabel()}
        <Card
          title={$tr("Account #{idx}", { idx: idx + 1 })}
          description={t.email || $tr("unknown account")}
        >
          {#snippet actions()}
            {@const streakTarget = m?.target ?? d.target ?? 7}
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
              <!-- Dots stay for never-enrolled accounts (consistent geometry,
              0/7 reads honestly as nothing banked); the tooltip explains
              what the count measures in each case. -->
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
            {#if m}
              <p
                class="fp-num text-[11px] leading-relaxed text-[var(--fp-dim)]"
              >
                {$tr("slot")}
                {fmtTime(m.slot)} ·
                {m.last_action
                  ? `${m.last_action} → ${m.last_result ?? "?"}`
                  : $tr("no touch yet")}{m.last_touch
                  ? ` · ${fmtTime(m.last_touch)}`
                  : ""}{m.last_advanced
                  ? ` · ${$tr("advanced")} ${m.last_advanced}`
                  : ""}
              </p>
            {/if}
            {#if m}
              {@const eff = effectiveFor(t, d)}
              {@const effCost = costFor(eff)}
              {@const strip = weekStrip(t, idx)}
              {@const ledger = ledgerFor(t, idx)}
              {@const streakTarget = m.target ?? d.target ?? 7}
              {@const streakVal = t.streak ?? 0}
              <div
                class="flex flex-col gap-1.5 rounded border border-[var(--fp-border)]/60 bg-[var(--fp-surface)]/60 px-2 py-1.5"
                aria-label={$tr("Next touch for Account #{idx}", {
                  idx: idx + 1,
                })}
              >
                <div
                  class="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs"
                >
                  <span class="fp-num font-semibold text-[var(--fp-fg)]">
                    {nextTouchText(t)}
                  </span>
                  {#if eff}
                    <code class="fp-num text-[11px] text-[var(--fp-muted)]"
                      >{eff}</code
                    >
                    {#if effCost}
                      <span class="fp-num text-[11px] text-[var(--fp-dim)]"
                        >· {effCost}</span
                      >
                    {/if}
                  {/if}
                  <span
                    class="fp-num ml-auto inline-flex items-center gap-1 text-[11px] text-[var(--fp-dim)]"
                    title={$tr(
                      "Daily touches banked toward the target (streak/target)",
                    )}
                  >
                    {$tr("day {n}/{target}", {
                      n: streakVal,
                      target: streakTarget,
                    })}
                    {#if t.today_used}
                      <span class="text-emerald-400"
                        >· {$tr("Active today")}</span
                      >
                    {:else if !m.warn}
                      <span>· {$tr("Needs activity today")}</span>
                    {/if}
                  </span>
                </div>
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
                {#if !d.touchModel}
                  <p class="fp-num text-[11px] text-[var(--fp-dim)]">
                    Auto: {autoFor(t) || "—"} ({autoReasonFor(t)})
                  </p>
                {/if}
                <p class="fp-num text-[11px] text-[var(--fp-dim)]">
                  Touches today {ledger.touchesToday} · Warming spend (7d) {fmtSpend(
                    ledger.spendWeek,
                  )} · Projected/mo {fmtSpend(ledger.projected)}
                </p>
              </div>
            {/if}

            <div class="grid grid-cols-1 sm:grid-cols-12 gap-2">
              <FieldBox
                label={$tr("Target Period")}
                unit={$tr("days")}
                class="sm:col-span-6 min-w-0 h-full"
              >
                <Stepper
                  bind:value={d.target}
                  min={1}
                  max={28}
                  disabled={!!saving[idx]}
                  ariaLabel={$tr("Streak target for Account #{idx}", {
                    idx: idx + 1,
                  })}
                  decreaseLabel={$tr("Decrease target for Account #{idx}", {
                    idx: idx + 1,
                  })}
                  increaseLabel={$tr("Increase target for Account #{idx}", {
                    idx: idx + 1,
                  })}
                />
              </FieldBox>
              <FieldBox
                label={$tr("Touch Model")}
                class="sm:col-span-6 min-w-0 h-full"
              >
                <select
                  class="fp-select !h-8 !py-1 !text-xs font-mono w-full min-w-0"
                  bind:value={d.touchModel}
                  disabled={!!saving[idx]}
                  aria-label={$tr("Touch model for Account #{idx}", {
                    idx: idx + 1,
                  })}
                  title={$tr(
                    "Per-token touch model (cheapest first, priced rows last). Empty is Auto: the cheapest served unmetered row, falling back to the global default from Settings → Advanced → Maturity Touch Model.",
                  )}
                >
                  <option value="">
                    {#if autoLbl}
                      {autoLbl}
                    {:else if globalTouchModel}
                      {$tr("Global default ({model})", {
                        model: globalTouchModel,
                      })}
                    {:else}
                      {$tr("Global default")}
                    {/if}
                  </option>
                  {#each touchOptions(d) as o (o.id)}
                    <option value={o.id}>{touchLabel(o)}</option>
                  {/each}
                </select>
                <!-- Jump link to the exact Settings row that owns the global. The
                click stashes a focus key; AdvancedSettings scrolls to the row
                and focuses its control on mount. -->
                <a
                  href="#settings"
                  class="fp-num text-[11px] text-[var(--fp-dim)] underline underline-offset-2 hover:text-[var(--fp-fg)]"
                  onclick={() => {
                    try {
                      sessionStorage.setItem(
                        "fp-settings-focus",
                        "MATURITY_TOUCH_MODEL",
                      );
                    } catch {
                      /* storage blocked: plain navigation still lands on Settings */
                    }
                  }}
                >
                  {$tr("Settings → Advanced → Maturity Touch Model")}
                </a>
              </FieldBox>
            </div>
            <div
              class="flex flex-wrap items-center justify-between gap-2 border-t border-[var(--fp-border)]/60 pt-2.5"
            >
              <ToggleSwitch
                checked={d.enabled}
                disabled={!!saving[idx]}
                ariaLabel={$tr("Maturity for Account #{idx}", {
                  idx: idx + 1,
                })}
                onchange={(next) => {
                  d.enabled = next;
                }}
              />
              <span class="flex flex-wrap gap-1.5">
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={!!touching[idx] || !m?.enabled}
                  loading={!!touching[idx]}
                  onclick={() => touchNow(idx)}
                  title={$tr("Fire one touch now (bypasses slot and throttle)")}
                >
                  {$tr("Touch now")}
                </Button>
                {#if m?.warn}
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={!!resetting[idx]}
                    loading={!!resetting[idx]}
                    onclick={() => resetWarn(idx)}
                    title={$tr(
                      "Clear the non-advance warning and re-arm the daily loop (config unchanged)",
                    )}
                  >
                    {$tr("Reset warning")}
                  </Button>
                {/if}
                <Button
                  variant="primary"
                  size="sm"
                  disabled={!!saving[idx]}
                  loading={!!saving[idx]}
                  onclick={() => save(idx)}
                >
                  {$tr("Save")}
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
