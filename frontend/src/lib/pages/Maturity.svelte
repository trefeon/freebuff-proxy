<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { SvelteSet } from "svelte/reactivity";
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
  // Resolved global MATURITY_TOUCH_MODEL for the per-card "Global default"
  // option label: effective snapshot first, raw .env fallback, "" = unknown.
  let globalTouchModel = $state("");

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

  // Served touch candidates, cheapest-Freebucks-cost first: the rows the
  // gateway can admit (live agent binding, served, never the referral
  // grant). Server order already sorts cheapest-first, so priced rows
  // stay ahead without re-sorting.
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
  // price_label/quota straight from /admin/api/models). The legacy pool
  // tag renders only when the row carries no price.
  function touchCostClass(m) {
    if (!m) return "";
    return m.price_label || m.quota || "";
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
            <!-- Dots stay for never-enrolled accounts (consistent geometry,
              0/7 reads honestly as nothing banked); the tooltip explains
              what the count measures in each case. -->
            <Pips
              value={t.streak ?? 0}
              total={streakTarget}
              label={m
                ? $tr("Daily touches banked toward the target (streak/target)")
                : $tr(
                    "Streak/target counts daily touches once enrolled — nothing banked yet",
                  )}
            />
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
                  "Per-token touch model (cheapest first, priced rows last). Empty uses the global default from Settings → Advanced → Maturity Touch Model.",
                )}
              >
                <option value="">
                  {globalTouchModel
                    ? $tr("Global default ({model})", {
                        model: globalTouchModel,
                      })
                    : $tr("Global default")}
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
</PageShell>
