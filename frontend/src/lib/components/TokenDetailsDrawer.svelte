<script>
  import {
    Zap,
    RefreshCw,
    Check,
    ExternalLink,
    X,
    Plus,
    Lock,
  } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import { fallbackModelOptions, fetchModelOptions } from "../modelOptions.js";
  import { fetchAPI, postForm } from "../api/client.js";
  import { adminApi, adminActions } from "../api/paths.js";
  import { refreshTokens } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import { onMount } from "svelte";

  /**
   * TokenDetailsDrawer — expanded details for one pooled token: the Dev Tools
   * toolbar (when enabled), the live session countdown, the account-standing
   * block, and the empty-state message. Shared by the desktop table row
   * (TokenCard) and the mobile stacked card (TokenCardMobile).
   *
   * @prop {object} token — dashboard tokenCard payload
   * @prop {string} [spawnModel] — bindable dev-spawn model selection
   * @prop {boolean} [actionPending]
   * @prop {boolean} [devToolsEnabled=false]
   * @prop {(model: string) => void} [onSpawn]
   * @prop {(action: string) => void} [onRefresh]
   * @prop {number} sessionRemaining — live seconds remaining on the session
   */
  let {
    token,
    spawnModel = $bindable(""),
    actionPending,
    devToolsEnabled = false,
    onSpawn,
    onRefresh,
    onDropSession,
    sessionRemaining,
  } = $props();
  let modelOptions = $state(fallbackModelOptions);
  onMount(() => {
    fetchModelOptions().then((rows) => (modelOptions = rows));
  });
  // --- Per-token model-lock editor (MODEL_LOCKS slot syntax) ---
  // Reads/writes the canonical .env through the existing config endpoints
  // (same round-trip as Settings save) and hot-applies via pool.SetConfig —
  // no new backend route, no restart.
  let lockSaving = $state(false);
  let lockError = $state("");
  let pinSelect = $state("");

  function parseLocks(envText) {
    const locks = {};
    const line = String(envText || "")
      .split(/\r?\n/)
      .find((l) => l.startsWith("MODEL_LOCKS="));
    if (!line) return locks;
    for (const part of line.slice("MODEL_LOCKS=".length).split(";")) {
      const i = part.indexOf(":");
      if (i < 0) continue;
      const slot = Number(part.slice(0, i).trim());
      const models = part
        .slice(i + 1)
        .split(",")
        .map((m) => m.trim())
        .filter(Boolean);
      if (Number.isInteger(slot) && models.length) locks[slot] = models;
    }
    return locks;
  }

  function serializeLocks(locks) {
    return Object.keys(locks)
      .map(Number)
      .sort((a, b) => a - b)
      .map((slot) => `${slot}:${locks[slot].join(",")}`)
      .join(";");
  }

  function patchEnvLocks(envText, slot, models) {
    const locks = parseLocks(envText);
    if (models.length) locks[slot] = models;
    else delete locks[slot];
    const value = serializeLocks(locks);
    const lines = String(envText || "").split(/\r?\n/);
    const at = lines.findIndex((l) => l.startsWith("MODEL_LOCKS="));
    if (value === "") {
      if (at >= 0) lines.splice(at, 1);
    } else if (at >= 0) {
      lines[at] = `MODEL_LOCKS=${value}`;
    } else {
      lines.push(`MODEL_LOCKS=${value}`);
    }
    return lines.join("\n");
  }

  async function saveLocks(models) {
    const slot = token.index;
    if (slot == null || lockSaving) return;
    lockSaving = true;
    lockError = "";
    try {
      const cfg = await fetchAPI(adminApi.config);
      const next = patchEnvLocks(cfg?.env_content || "", slot, models);
      const res = await postForm(adminActions.configSave, { content: next });
      const json = await res.json();
      if (!(res.ok && json?.ok)) {
        throw new Error(json?.message || "Save rejected");
      }
      pinSelect = "";
      await refreshTokens();
    } catch (e) {
      lockError = e?.message || String(e);
    } finally {
      lockSaving = false;
    }
  }

  function pinModel(id) {
    if (!id) return;
    const cur = token.allowed_models?.length ? [...token.allowed_models] : [];
    if (!cur.includes(id)) saveLocks([...cur, id]);
  }

  function unpinModel(id) {
    saveLocks((token.allowed_models || []).filter((m) => m !== id));
  }

  function fmtCountdown(totalSeconds) {
    const h = Math.floor(totalSeconds / 3600);
    const m = Math.floor((totalSeconds % 3600) / 60);
    const s = totalSeconds % 60;
    if (h > 0) return `${h}h ${m}m ${s}s remaining`;
    return `${m}m ${s}s remaining`;
  }
</script>

<div class="fp-inset rounded p-3">
  <!-- Dev Tools: Session Generator & Diagnostics Toolbar (hidden unless DEVTOOLS_ENABLED) -->
  {#if devToolsEnabled}
    <div
      class="mb-3 p-2.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] flex flex-wrap items-center justify-between gap-2.5"
    >
      <div class="flex flex-wrap items-center gap-2">
        <span
          class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider"
          >{$tr("Dev Session:")}</span
        >
        <select
          bind:value={spawnModel}
          class="fp-input !text-xs !py-1 !pl-2.5 !h-7 !w-44 !inline-block"
        >
          {#each modelOptions as m (m.id)}
            <option value={m.id}>{m.label}</option>
          {/each}
        </select>
        <Button
          variant="secondary"
          size="sm"
          class="!h-7 !text-xs !px-2.5"
          disabled={actionPending}
          onclick={() => onSpawn?.(spawnModel || "mimo/mimo-v2.5")}
        >
          <Zap size={12} />
          <span>{$tr("Make Session")}</span>
        </Button>
      </div>

      <div class="flex items-center gap-1.5">
        <Button
          variant="ghost"
          size="sm"
          class="!h-7 !text-xs !px-2"
          disabled={actionPending}
          onclick={() => onRefresh?.("probe")}
        >
          <RefreshCw size={12} />
          <span>{$tr("Probe")}</span>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          class="!h-7 !text-xs !px-2"
          disabled={actionPending}
          onclick={() => onRefresh?.("finish")}
        >
          <Check size={12} />
          <span>{$tr("Finish Runs")}</span>
        </Button>
      </div>
    </div>
  {/if}
  {#if token.session_remaining_seconds > 0 && token.session_model}
    <div
      class="mb-2 px-2 py-1 rounded bg-[var(--fp-accent)]/10 text-xs text-[var(--fp-accent)] flex items-center justify-between gap-2 flex-wrap"
    >
      <span
        >{$tr("Active Session:")}
        <code class="fp-num">{token.session_model}</code></span
      >
      <span class="fp-num">{fmtCountdown(sessionRemaining)}</span>
    </div>
  {/if}
  {#if token.session_remaining_seconds > 0}
    <div class="mb-2 flex justify-end">
      <Button
        variant="danger"
        size="sm"
        class="!h-7 !text-xs !px-2"
        disabled={actionPending}
        onclick={() => onDropSession?.()}
      >
        <span>{$tr("Drop Session")}</span>
      </Button>
    </div>
  {/if}
  {#if token.has_standing}
    <!-- Standing / trust block (issue #140): level,
         score progress toward the next level, the cap
         holding the account (capped_by), and upstream's
         suggested earn-back actions. -->
    <div class="mb-2 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
      <div class="flex items-center justify-between gap-2 mb-1">
        <p
          class="text-xs text-[var(--fp-muted)] uppercase tracking-wider font-semibold"
        >
          {$tr("Account standing")}
        </p>
        <span class="text-xs text-[var(--fp-text)] font-semibold"
          >{token.standing_label || token.standing_level}</span
        >
      </div>
      {#if token.standing_score != null && token.standing_next_level}
        <div class="flex items-center gap-2">
          <div class="h-1.5 flex-1 rounded bg-[var(--fp-bg)] overflow-hidden">
            <div
              class="h-full rounded bg-[var(--fp-accent)]"
              style={`width: ${Math.min(100, Math.max(0, token.standing_score))}%`}
            ></div>
          </div>
          <span class="fp-num text-xs text-[var(--fp-muted)] shrink-0">
            {$tr("score {score} → next: {level}", {
              score: token.standing_score,
              level: token.standing_next_level,
            })}
          </span>
        </div>
      {:else if token.standing_score != null}
        <span class="fp-num text-xs text-[var(--fp-muted)]"
          >{$tr("score {score}", { score: token.standing_score })}</span
        >
      {/if}
      {#if token.standing_blurb}
        <p class="text-xs text-[var(--fp-dim)] mt-1">{token.standing_blurb}</p>
      {/if}
      {#if token.standing_capped_by}
        <p class="text-xs mt-1 text-[var(--fp-warning)]">
          {$tr("Capped by")}
          <code class="fp-num">{token.standing_capped_by}</code
          >{#if token.standing_capped_reason}: {token.standing_capped_reason}{/if}
        </p>
      {/if}
      {#if token.standing_next_steps?.length > 0}
        <ul class="mt-1.5 flex flex-col gap-1">
          {#each token.standing_next_steps as step (step.label)}
            <li class="text-xs text-[var(--fp-text)] flex items-start gap-1.5">
              <span class="fp-num text-[var(--fp-accent)] shrink-0"
                >+{step.points}</span
              >
              <span>
                {step.label}{#if step.detail}
                  — <span class="text-[var(--fp-dim)]">{step.detail}</span>{/if}
                {#if step.href}
                  <a
                    href={step.href}
                    target="_blank"
                    rel="noopener noreferrer"
                    class="ml-1 text-[var(--fp-accent)] hover:underline inline-flex items-center gap-0.5"
                  >
                    <ExternalLink size={10} />
                  </a>
                {/if}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
  <div class="mb-2 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
    <div
      class="flex items-center justify-between gap-2 text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider mb-1"
    >
      <span class="inline-flex items-center gap-1.5"
        ><Lock size={12} />{$tr("Pinned models")}</span
      >
      {#if token.allowed_models?.length}
        <button
          type="button"
          class="normal-case font-medium text-[var(--fp-accent)] hover:underline disabled:opacity-50"
          disabled={lockSaving}
          onclick={() => saveLocks([])}
        >
          {$tr("Clear pins")}
        </button>
      {/if}
    </div>
    {#if token.allowed_models?.length}
      <div class="flex flex-wrap gap-1.5">
        {#each token.allowed_models as m (m)}
          <span
            class="inline-flex items-center gap-1 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] px-1.5 py-0.5"
          >
            <code class="fp-num text-xs text-[var(--fp-text)]">{m}</code>
            <button
              type="button"
              class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] disabled:opacity-50"
              aria-label={$tr("Unpin {model}", { model: m })}
              disabled={lockSaving}
              onclick={() => unpinModel(m)}
            >
              <X size={12} />
            </button>
          </span>
        {/each}
      </div>
      {#if token.allowlist_skips > 0}
        <p class="mt-1 text-xs text-[var(--fp-dim)]">
          {$tr("{count} request(s) routed elsewhere by this pin", {
            count: token.allowlist_skips,
          })}
        </p>
      {/if}
    {:else}
      <p class="text-xs text-[var(--fp-dim)]">
        {$tr(
          "Unlocked — serves any model. Pin models to dedicate this account.",
        )}
      </p>
    {/if}
    <div class="mt-1.5 flex items-center gap-1.5">
      <select
        bind:value={pinSelect}
        class="fp-input !text-xs !py-1 !pl-2 !h-7 flex-1 min-w-0"
        aria-label={$tr("Pin a model to this token")}
        disabled={lockSaving}
      >
        <option value="">{$tr("Pin a model…")}</option>
        {#each modelOptions.filter((o) => !(token.allowed_models || []).includes(o.id)) as o (o.id)}
          <option value={o.id}>{o.label}</option>
        {/each}
      </select>
      <Button
        variant="secondary"
        size="sm"
        class="!h-7 !text-xs !px-2.5"
        disabled={lockSaving || !pinSelect}
        onclick={() => pinModel(pinSelect)}
      >
        <Plus size={12} />
        <span>{lockSaving ? $tr("Saving…") : $tr("Pin")}</span>
      </Button>
    </div>
    {#if lockError}
      <p class="mt-1 text-xs text-red-400">{lockError}</p>
    {/if}
  </div>
  {#if !devToolsEnabled && !(token.session_remaining_seconds > 0 && token.session_model) && !token.has_standing}
    <p class="text-xs text-[var(--fp-dim)] italic">
      {$tr("No active session or run for this auth token.")}
    </p>
  {/if}
</div>
