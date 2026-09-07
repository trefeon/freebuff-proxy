<script>
  import { onMount } from "svelte";
  import {
    loadPageState,
    recordPageVisit,
    savePageState,
  } from "../stores/pageState.js";
  import { SvelteSet } from "svelte/reactivity";
  import { ChevronDown, ChevronRight } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import FieldBox from "../components/FieldBox.svelte";
  import Stepper from "../components/Stepper.svelte";
  import Pips from "../components/Pips.svelte";
  import Card from "../components/Card.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import ToggleSwitch from "../components/ToggleSwitch.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi, tokenActions } from "../api/paths.js";
  import { fetchMaturityHistory, historyKindTone } from "../utils/history.js";
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

  // Touch-model candidates: served models the gateway can admit (same
  // usable filter as Quota Tracker: a live agent binding, no withdrawn
  // rows, no referral-grant row). Server order is cheapest-Freebucks-cost
  // first, so unmetered-capable rows lead with the premium pool last;
  // each option is labeled with its server-reported cost class
  // (price_label/quota/pool) — never an invented price.
  let modelRows = $state([]);

  // Folded cards by default: only expanded cards render controls +
  // history. Expanded ids persist in pages_state (maturity scope,
  // server-wins: the snapshot restores, local toggles merge back).
  let expandedIds = new SvelteSet();

  // Per-token draft controls + busy flags, keyed by token index.
  let drafts = $state({});
  let saving = $state({});
  let touching = $state({});
  let actionMessage = $state("");
  let actionOK = $state(true);

  // Restart-surviving event timelines (ADR-0016): loaded once per token
  // the first time its EXPANDED maturity block appears, never on the 10s
  // poll. Folded cards fetch nothing.
  let histByIdx = $state({});
  let histPending = new SvelteSet();

  $effect(() => {
    for (const t of tokens) {
      const idx = t.index ?? 0;
      if (
        !t.maturity ||
        !expandedIds.has(idx) ||
        idx in histByIdx ||
        histPending.has(idx)
      )
        continue;
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
          mode: t.maturity?.mode ?? "unmetered",
          touchModel: t.maturity?.touch_model ?? "",
        };
      }
    }
    clampExpanded();
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

  // Served touch candidates, cheapest-Freebucks-cost first: the rows the
  // gateway can admit (live agent binding, served, never the referral
  // grant). Server order already sorts cheapest-first, so partition
  // unmetered-capable rows ahead of the premium pool without re-sorting.
  function touchCandidates() {
    const rows = (modelRows ?? []).filter(
      (m) => m?.agent && m?.served !== false && m?.pool !== "referral",
    );
    return [
      ...rows.filter((m) => m.pool !== "premium"),
      ...rows.filter((m) => m.pool === "premium"),
    ];
  }

  // Server-reported cost class for one candidate row (never invented:
  // price_label/quota/pool straight from /admin/api/models, premium pool
  // named as the pool it spends).
  function touchCostClass(m) {
    if (!m) return "";
    if (m.pool === "premium") return "premium pool";
    return m.price_label || m.quota || m.pool || "";
  }

  function touchLabel(m) {
    const cls = touchCostClass(m);
    return cls ? `${m.id} (${cls})` : m.id;
  }

  // Fail-open options for one card: live candidates when the catalog
  // loaded, else the drafted value alone so the select never empties.
  function touchOptions(d) {
    const cands = touchCandidates();
    if (cands.length > 0) return cands;
    if (d?.touchModel) {
      return [
        { id: d.touchModel, price_label: "", quota: "", pool: "unlimited" },
      ];
    }
    return [];
  }

  function toggleExpand(idx) {
    if (expandedIds.has(idx)) expandedIds.delete(idx);
    else expandedIds.add(idx);
    savePageState("maturity", { expanded: [...expandedIds] });
  }

  // A restored expanded id may point past the live list (the pool shrank
  // while the snapshot sat in pages_state). Drop out-of-range ids
  // instead of tracking ghosts — and never re-persist the stale value
  // back over the snapshot.
  function clampExpanded() {
    const live = new Set((data?.tokens ?? []).map((t, i) => t?.index ?? i));
    for (const id of [...expandedIds]) {
      if (!live.has(id)) expandedIds.delete(id);
    }
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

  onMount(() => {
    recordPageVisit("maturity");
    // Server-wins restore: folded by default, the snapshot re-opens what
    // the operator left expanded (stale ids clamp on first tokens push).
    loadPageState("maturity").then((d) => {
      const arr = d?.expanded;
      if (Array.isArray(arr)) {
        for (const i of arr) {
          if (Number.isInteger(i) && i >= 0) expandedIds.add(i);
        }
      }
    });
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
      release();
      unsubStore?.();
      unsubErr?.();
      window.removeEventListener("fp-config-saved", onConfigSaved);
    };
  });

  const tokens = $derived(data?.tokens ?? []);
</script>

<PageShell
  crumb="freebuff-proxy / Admin / maturity.conf"
  title={$tr("Account Maturity")}
  description={$tr(
    "Lock warming accounts out of rotation while a daily low-cost Freebucks touch keeps their streak alive. Reaching the target auto-releases the token.",
  )}
  {loading}
  {error}
  empty={tokens.length === 0 ? { title: $tr("No pooled tokens") } : null}
  onRetry={() => {
    error = "";
    refreshTokens();
  }}
>
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
      {@const expanded = expandedIds.has(idx)}
      <Card
        title={$tr("Account #{idx}", { idx: idx + 1 })}
        description={t.email || $tr("unknown account")}
      >
        {#snippet actions()}
          {@const streakTarget = m?.target ?? d.target ?? 7}
          <span class="flex flex-wrap items-center justify-end gap-1.5">
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
              label={$tr("Current streak / target")}
            />
            <button
              type="button"
              onclick={() => toggleExpand(idx)}
              aria-expanded={expanded}
              aria-label={expanded
                ? $tr("Collapse details for Account #{idx}", {
                    idx: idx + 1,
                  })
                : $tr("Expand details for Account #{idx}", {
                    idx: idx + 1,
                  })}
              class="inline-flex items-center justify-center w-8 h-8 shrink-0 rounded text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface-2)] transition-colors"
            >
              {#if expanded}
                <ChevronDown size={16} />
              {:else}
                <ChevronRight size={16} />
              {/if}
            </button>
          </span>
        {/snippet}
        <div class="flex flex-col gap-2">
          {#if m}
            <p class="fp-num text-[11px] leading-relaxed text-[var(--fp-dim)]">
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

          {#if expanded}
            {@const opts = touchOptions(d)}
            {@const selClass = d.touchModel
              ? touchCostClass(opts.find((o) => o.id === d.touchModel))
              : ""}
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
                unit={d.touchModel
                  ? selClass || $tr("custom")
                  : $tr("global default")}
                class="sm:col-span-6 min-w-0 h-full"
              >
                <div class="flex flex-col gap-2">
                  <select
                    class="fp-select !h-8 !py-1 !text-xs font-mono flex-1 min-w-0"
                    bind:value={d.touchModel}
                    disabled={!!saving[idx]}
                    aria-label={$tr("Touch model for Account #{idx}", {
                      idx: idx + 1,
                    })}
                    title={$tr(
                      "Per-token touch model (cheapest first, premium pool last). Empty uses the global MATURITY_TOUCH_MODEL fallback.",
                    )}
                  >
                    <option value="">{$tr("Global default")}</option>
                    {#each opts as o (o.id)}
                      <option value={o.id}>{touchLabel(o)}</option>
                    {/each}
                  </select>
                  <select
                    class="fp-select !h-8 !py-1 !text-xs w-full"
                    bind:value={d.mode}
                    disabled={!!saving[idx]}
                    aria-label={$tr("Touch mode for Account #{idx}", {
                      idx: idx + 1,
                    })}
                    title={d.mode === "premium-short"
                      ? $tr(
                          "One short premium admission per day, paid from this account's daily Freebucks pool.",
                        )
                      : $tr(
                          "Cheapest served model, minimal spend from this account's daily Freebucks pool.",
                        )}
                  >
                    <option value="unmetered">{$tr("Economy")}</option>
                    <option value="premium-short">{$tr("Premium short")}</option
                    >
                  </select>
                </div>
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
          {/if}
          {#if expanded && (histByIdx[idx] ?? []).length > 0}
            <ul
              class="flex flex-col gap-1.5 border-t border-[var(--fp-border)]/60 pt-2.5"
              aria-label={$tr("Maturity history for Account #{idx}", {
                idx: idx + 1,
              })}
            >
              {#each histByIdx[idx] as ev (ev.ts + ev.kind + ev.detail)}
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
          {/if}
        </div>
      </Card>
    {/each}
  </div>
</PageShell>
