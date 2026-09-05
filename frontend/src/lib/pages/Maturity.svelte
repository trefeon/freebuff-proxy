<script>
  import { onMount } from "svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Card from "../components/Card.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import ToggleSwitch from "../components/ToggleSwitch.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi, tokenActions } from "../api/paths.js";
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

  // Per-token draft controls + busy flags, keyed by token index.
  let drafts = $state({});
  let saving = $state({});
  let touching = $state({});
  let actionMessage = $state("");
  let actionOK = $state(true);

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

  function progressDots(streak) {
    const filled = Math.min(streak ?? 0, 7);
    return (
      "●".repeat(filled) +
      "○".repeat(Math.max(0, 7 - filled)) +
      (streak > 7 ? "+" : "")
    );
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
        const envContent = cfgRes?.env_content || "";
        const eff = (cfgRes?.effective || []).find(
          (e) => e.key === "MATURITY_ENABLED",
        );
        if (eff) {
          const v = String(eff.value).trim().toLowerCase();
          globalEnabled =
            v === "true" || v === "1" || v === "on" || v === "yes";
        } else {
          const raw = getEnvValue(envContent, "MATURITY_ENABLED");
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
    return () => {
      release();
      unsubStore?.();
      unsubErr?.();
      window.removeEventListener("fp-config-saved", onConfigSaved);
    };
  });

  const tokens = $derived(data?.tokens ?? []);
</script>

<div class="page-enter">
  <div class="flex flex-col gap-6">
    <PageHeader
      title={$tr("Account Maturity")}
      description={$tr(
        "Lock warming accounts out of rotation while a daily low-cost touch keeps their streak alive. Reaching the target auto-releases the token.",
      )}
    />

    {#if actionMessage}
      <Alert tone={actionOK ? "success" : "error"} title={actionMessage} />
    {/if}
    {#if error}
      <Alert tone="error" title={error}>
        <Button
          variant="ghost"
          size="sm"
          onclick={() => {
            error = "";
            refreshTokens();
          }}
        >
          {$tr("Retry")}
        </Button>
      </Alert>
    {/if}

    {#if globalLoaded && !globalEnabled}
      <Alert tone="warning" title={$tr("Maturity automation is globally off")}>
        {$tr(
          "Set MATURITY_ENABLED=1 in Settings — per-token toggles below do nothing while the kill-switch is off. Dry-run probes stay on until the schedule is proven.",
        )}
      </Alert>
    {/if}

    {#if loading}
      <EmptyState title={$tr("Loading maturity…")} />
    {:else if tokens.length === 0}
      <EmptyState title={$tr("No pooled tokens")} />
    {:else}
      <div class="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {#each tokens as t (t.index ?? t.email)}
          {@const idx = t.index ?? 0}
          {@const m = t.maturity}
          {@const d = drafts[idx] ?? {
            enabled: false,
            target: 7,
            mode: "unmetered",
          }}
          <Card
            title={$tr("Account #{idx}", { idx: idx + 1 })}
            description={t.email || $tr("unknown account")}
          >
            <div class="flex flex-col gap-3">
              <div class="flex flex-wrap items-center gap-2">
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
                {#if m?.enabled}
                  <span class="text-sm text-[var(--fp-muted)]">
                    {progressDots(t.streak ?? 0)}
                    {t.streak ?? 0}/{$tr("{target} day target", {
                      target: m.target,
                    })}
                  </span>
                {/if}
              </div>

              {#if m}
                <dl class="grid grid-cols-1 gap-1 text-sm sm:grid-cols-2">
                  <div class="flex justify-between gap-2">
                    <dt class="text-[var(--fp-muted)]">
                      {$tr("Today's slot")}
                    </dt>
                    <dd class="font-mono">{fmtTime(m.slot)}</dd>
                  </div>
                  <div class="flex justify-between gap-2">
                    <dt class="text-[var(--fp-muted)]">{$tr("Last touch")}</dt>
                    <dd class="font-mono">
                      {m.last_action
                        ? `${m.last_action} → ${m.last_result ?? "?"}`
                        : "—"}
                    </dd>
                  </div>
                  <div class="flex justify-between gap-2">
                    <dt class="text-[var(--fp-muted)]">{$tr("Touched at")}</dt>
                    <dd class="font-mono">{fmtTime(m.last_touch)}</dd>
                  </div>
                  <div class="flex justify-between gap-2">
                    <dt class="text-[var(--fp-muted)]">{$tr("Advanced")}</dt>
                    <dd class="font-mono">{m.last_advanced || "—"}</dd>
                  </div>
                </dl>
              {:else}
                <p class="text-sm text-[var(--fp-muted)]">
                  {$tr(
                    "Enable maturity to lock this account for warming: it leaves serving rotation and earns its streak back one cheap touch a day.",
                  )}
                </p>
              {/if}

              <div class="flex flex-wrap items-end gap-3">
                <label class="flex flex-col gap-1 text-sm">
                  <span class="text-[var(--fp-muted)]"
                    >{$tr("Target (days)")}</span
                  >
                  <input
                    type="number"
                    min="1"
                    max="28"
                    class="fp-num w-24 rounded border px-2 py-1"
                    bind:value={d.target}
                    disabled={!!saving[idx]}
                    aria-label={$tr("Streak target for Account #{idx}", {
                      idx: idx + 1,
                    })}
                  />
                </label>
                <label class="flex flex-col gap-1 text-sm">
                  <span class="text-[var(--fp-muted)]">{$tr("Touch mode")}</span
                  >
                  <select
                    class="rounded border px-2 py-1"
                    bind:value={d.mode}
                    disabled={!!saving[idx]}
                    aria-label={$tr("Touch mode for Account #{idx}", {
                      idx: idx + 1,
                    })}
                  >
                    <option value="unmetered">{$tr("Unmetered (free)")}</option>
                    <option value="premium-short">{$tr("Premium short")}</option
                    >
                  </select>
                </label>
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
              </div>

              <div class="flex flex-wrap gap-2">
                <Button
                  variant="primary"
                  size="sm"
                  disabled={!!saving[idx]}
                  loading={!!saving[idx]}
                  onclick={() => save(idx)}
                >
                  {$tr("Save")}
                </Button>
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
              </div>
            </div>
          </Card>
        {/each}
      </div>
    {/if}
  </div>
</div>
