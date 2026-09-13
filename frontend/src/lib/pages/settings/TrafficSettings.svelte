<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbBadge from "../../components/DbOverrideBadge.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import Stepper from "../../components/Stepper.svelte";
  import FieldBox from "../../components/FieldBox.svelte";
  import { Activity } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";
  import { fetchAPI } from "../../api/client.js";
  import { adminApi } from "../../api/paths.js";

  /**
   * Traffic & Rate Limiting settings card (Pool group).
   * Built using the SettingsCard and SettingsRow template components.
   * Leads with a bespoke Rotation section (relocated from the
   * Tokens page): the TOKEN_ROTATION radiogroup + RATE_LIMIT_FAILOVER
   * toggle persist through the whole-file .env flow (onField, batched
   * into the page Save).
   * All keys apply live on reload (none is restart-only).
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - DB override reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key DB-overlay save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   */
  let {
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
  } = $props();

  let env = $derived(parseEnv(rawText));
  let rateLimitPerIp = $derived(formValues.RATE_LIMIT_PER_IP ?? "0");

  // ---------------------------------------------------------------------------
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  // ---------------------------------------------------------------------------
  const RL_IP_LABEL = "Rate Limit per Client IP";
  const RL_IP_DESC =
    "Maximum requests per second allowed from any single client IP address. Prevents rapid agent loops from depleting the pool. Set to 0 for no cap.";
  const RL_IP_HINT = "0 = no cap (recommended for a single-user gateway)";
  const MAX_MIN_LABEL = "Max Requests per Minute (per account)";
  const MAX_MIN_DESC =
    "Per-token cap on admitted chat requests in a rolling 60s window. Throttles the request rate upstream actually observes — a runaway loop locks within a minute and the pool rolls to the next account. 0 = no cap, empty = default (30).";
  const MAX_MIN_HINT = "0 = no cap · recommended 30";
  const MAX_DAY_LABEL = "Max Requests per Day (per account)";
  const MAX_DAY_DESC =
    "Per-token cap on successful chat requests per Pacific day. A capped account is skipped and the pool rolls to the next one; all tokens unlock at Pacific midnight — the same instant upstream resets its daily quota windows. 0 = no cap, empty = default (1500).";
  const MAX_DAY_HINT = "0 = no cap · recommended 1500";

  // Rotation copy (relocated verbatim from Tokens.svelte).
  const ROT_SECTION = "Rotation";
  const ROT_POLICY_LABEL = "Token Rotation Policy";
  const ROT_DRAIN_BTN = "Drain (Safest)";
  const ROT_RR_BTN = "Round Robin (1:1)";
  const ROT_LU_BTN = "Least Used (Max Quota)";
  const ROT_RANDOM_BTN = "Random (Stochastic)";
  const ROT_DRAIN_TITLE = "Drain Mode (Default & Recommended):";
  const ROT_DRAIN_BODY =
    "Sticks to one account until it is unfit (cooldown, quota, or ban) before rotating to the next token. Mimics authentic single-user behavior and provides the strongest anti-ban protection.";
  const ROT_RR_TITLE = "Round-Robin Mode:";
  const ROT_RR_BODY =
    "Rotates to the next token on every request (1:1). Note: rapid alternating requests across healthy accounts may raise upstream anomaly-detection signals.";
  const ROT_LU_TITLE = "Least-Used Mode:";
  const ROT_LU_BODY =
    "Routes requests to the token with the lowest daily usage or active run count. Maximizes concurrency and distributes quota consumption evenly.";
  const ROT_RANDOM_TITLE = "Random Mode:";
  const ROT_RANDOM_BODY =
    "Selects an available healthy token at random per request. Provides stochastic load balancing.";
  const FAILOVER_LABEL = "Auto Failover on Rate Limit (429)";
  const FAILOVER_DESC =
    "When enabled, an in-flight request encountering a 429 rate limit or account throttle immediately leases another healthy pool token and retries seamlessly without failing the request.";


  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }

  let showRotation = $derived(
    hit(
      "TOKEN_ROTATION",
      "RATE_LIMIT_FAILOVER",
      ROT_SECTION,
      ROT_POLICY_LABEL,
      ROT_DRAIN_BTN,
      ROT_RR_BTN,
      ROT_LU_BTN,
      ROT_RANDOM_BTN,
      ROT_DRAIN_TITLE,
      ROT_DRAIN_BODY,
      ROT_RR_TITLE,
      ROT_RR_BODY,
      ROT_LU_TITLE,
      ROT_LU_BODY,
      ROT_RANDOM_TITLE,
      ROT_RANDOM_BODY,
      FAILOVER_LABEL,
      FAILOVER_DESC,
      "Token Rotation & Handling Policy",
      "Strategy used by the gateway to select upstream accounts for model requests.",
    ),
  );
  let showIp = $derived(
    hit("RATE_LIMIT_PER_IP", RL_IP_LABEL, RL_IP_DESC, RL_IP_HINT),
  );
  let showMin = $derived(
    hit("MAX_REQUESTS_PER_MINUTE", MAX_MIN_LABEL, MAX_MIN_DESC, MAX_MIN_HINT),
  );
  let showDay = $derived(
    hit("MAX_REQUESTS_PER_DAY", MAX_DAY_LABEL, MAX_DAY_DESC, MAX_DAY_HINT),
  );
  let visibleRateKeys = $derived(
    [
      showIp ? "RATE_LIMIT_PER_IP" : null,
      showMin ? "MAX_REQUESTS_PER_MINUTE" : null,
      showDay ? "MAX_REQUESTS_PER_DAY" : null,
    ].filter((k) => k !== null),
  );
  // The bespoke section counts as one row for the "N of M" search count.
  let visible = $derived((showRotation ? 1 : 0) + visibleRateKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });

  // ---------------------------------------------------------------------------
  // Rotation & failover: .env whole-file flow. Unlike Tokens.svelte (which
  // saved immediately via its own fetch + postForm), this page batches every
  // .env edit through onField into the shared Save/Discard flow — the
  // persistence target (.env document → configSave) is identical.
  // ---------------------------------------------------------------------------
  const ROT_MODES = ["drain", "round_robin", "least_used", "random"];
  let tokenRotation = $derived.by(() => {
    const raw = String(
      formValues.TOKEN_ROTATION ?? env.TOKEN_ROTATION ?? "drain",
    ).toLowerCase();
    return ROT_MODES.includes(raw) ? raw : "drain";
  });
  let rateLimitFailover = $derived(
    String(
      formValues.RATE_LIMIT_FAILOVER ?? env.RATE_LIMIT_FAILOVER ?? "true",
    ).toLowerCase() !== "false",
  );

  function setTokenRotation(mode) {
    if (tokenRotation === mode) return;
    onField("TOKEN_ROTATION", mode);
  }
  function toggleRateLimitFailover(next) {
    const v = typeof next === "boolean" ? next : !rateLimitFailover;
    onField("RATE_LIMIT_FAILOVER", v ? "true" : "false");
  }

</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Pool")}
    description={$tr(
      "Traffic limits and rotation policy for the token pool. Changes apply live without restart.",
    )}
  >
    {#snippet icon()}
      <Activity size={20} />
    {/snippet}
    {#snippet actions()}
      <span
        class="inline-flex items-center gap-1.5 font-mono text-xs text-[var(--fp-muted)]"
      >
        <span class="led {tokenRotation === 'drain' ? 'led-good' : 'led-idle'}"
        ></span>
        <span
          class="uppercase tracking-wider font-semibold text-[var(--fp-accent)]"
          >{tokenRotation}</span
        >
      </span>
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 4 })}</span
        >
      {/if}
    {/snippet}

    {#if showRotation}
      <!-- Rotation (relocated from Tokens.svelte) -->
      <div class="space-y-3 py-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
        >
          {$tr(ROT_SECTION)}
        </p>
        <div
          class="flex flex-wrap items-center gap-2"
          role="radiogroup"
          aria-label={$tr(ROT_POLICY_LABEL)}
        >
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "drain"}
            onclick={() => setTokenRotation("drain")}
            class="fp-btn {tokenRotation === 'drain'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_DRAIN_BTN)}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "round_robin"}
            onclick={() => setTokenRotation("round_robin")}
            class="fp-btn {tokenRotation === 'round_robin'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_RR_BTN)}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "least_used"}
            onclick={() => setTokenRotation("least_used")}
            class="fp-btn {tokenRotation === 'least_used'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_LU_BTN)}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "random"}
            onclick={() => setTokenRotation("random")}
            class="fp-btn {tokenRotation === 'random'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_RANDOM_BTN)}
          </button>
        </div>

        <div
          class="fp-inset p-3 rounded text-xs text-[var(--fp-muted)] flex items-start gap-2"
        >
          {#if tokenRotation === "drain"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]"
                >{$tr(ROT_DRAIN_TITLE)}</strong
              >
              {$tr(ROT_DRAIN_BODY)}
            </p>
          {:else if tokenRotation === "round_robin"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]">{$tr(ROT_RR_TITLE)}</strong>
              {$tr(ROT_RR_BODY)}
            </p>
          {:else if tokenRotation === "least_used"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]">{$tr(ROT_LU_TITLE)}</strong>
              {$tr(ROT_LU_BODY)}
            </p>
          {:else if tokenRotation === "random"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]"
                >{$tr(ROT_RANDOM_TITLE)}</strong
              >
              {$tr(ROT_RANDOM_BODY)}
            </p>
          {/if}
        </div>
        <!-- Rate Limit Auto-Failover Toggle -->
        <div
          class="pt-3 border-t border-[var(--fp-border)] flex flex-col sm:flex-row sm:items-center justify-between gap-3"
        >
          <div class="space-y-0.5">
            <div class="flex items-center gap-2">
              <span class="text-xs font-semibold text-[var(--fp-text)]">
                {$tr(FAILOVER_LABEL)}
              </span>
              <span class="led {rateLimitFailover ? 'led-good' : 'led-dim'}"
              ></span>
            </div>
            <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
              {$tr(FAILOVER_DESC)}
            </p>
          </div>
          <ToggleSwitch
            checked={rateLimitFailover}
            ariaLabel="Auto Failover on Rate Limit (429)"
            onchange={(v) => toggleRateLimitFailover(v)}
          />
        </div>
      </div>
    {/if}

    {#if showIp}
      <!-- Client IP Rate Limit -->
      <SettingsRow
        first={visibleRateKeys[0] === "RATE_LIMIT_PER_IP"}
        last={visibleRateKeys[visibleRateKeys.length - 1] ===
          "RATE_LIMIT_PER_IP"}
        label={$tr(RL_IP_LABEL)}
        description={$tr(RL_IP_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >RATE_LIMIT_PER_IP</code
          >
          {#if !env.RATE_LIMIT_PER_IP}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          {#if sources.RATE_LIMIT_PER_IP === "db"}
            <DbBadge settingKey="RATE_LIMIT_PER_IP" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="RATE_LIMIT_PER_IP"
            value={rateLimitPerIp}
            {onSaved}
          />
        {/snippet}

        <div class="w-full sm:w-44">
          <div class="relative">
            <input
              type="number"
              min="0"
              step="1"
              aria-label="RATE_LIMIT_PER_IP"
              class="fp-input w-full !text-xs !py-1.5 !pr-14"
              placeholder="0"
              value={rateLimitPerIp}
              oninput={(e) => {
                const val = e.currentTarget.value.trim();
                onField("RATE_LIMIT_PER_IP", val === "" ? "0" : val);
              }}
            />
            <span
              class="absolute right-3 top-1/2 -translate-y-1/2 text-[10px] text-text-muted pointer-events-none"
              >req/s</span
            >
          </div>
          <p class="text-[10px] text-[var(--fp-dim)] mt-1">
            {$tr(RL_IP_HINT)}
          </p>
        </div>
      </SettingsRow>
    {/if}

    {#if showMin}
      <!-- Per-token Per-Minute Request Limit -->
      <SettingsRow
        first={visibleRateKeys[0] === "MAX_REQUESTS_PER_MINUTE"}
        last={visibleRateKeys[visibleRateKeys.length - 1] ===
          "MAX_REQUESTS_PER_MINUTE"}
        label={$tr(MAX_MIN_LABEL)}
        description={$tr(MAX_MIN_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >MAX_REQUESTS_PER_MINUTE</code
          >
          {#if !env.MAX_REQUESTS_PER_MINUTE}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          {#if sources.MAX_REQUESTS_PER_MINUTE === "db"}
            <DbBadge settingKey="MAX_REQUESTS_PER_MINUTE" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="MAX_REQUESTS_PER_MINUTE"
            value={formValues.MAX_REQUESTS_PER_MINUTE ?? "30"}
            {onSaved}
          />
        {/snippet}

        <div class="w-full sm:w-44">
          <div class="relative">
            <input
              type="number"
              min="0"
              step="1"
              aria-label="MAX_REQUESTS_PER_MINUTE"
              class="fp-input w-full !text-xs !py-1.5 !pr-14"
              placeholder="30"
              value={formValues.MAX_REQUESTS_PER_MINUTE ?? "30"}
              oninput={(e) => {
                const val = e.currentTarget.value.trim();
                onField("MAX_REQUESTS_PER_MINUTE", val);
              }}
            />
            <span
              class="absolute right-3 top-1/2 -translate-y-1/2 text-[10px] text-text-muted pointer-events-none"
              >req/min</span
            >
          </div>
          <p class="text-[10px] text-[var(--fp-dim)] mt-1">
            {$tr(MAX_MIN_HINT)}
          </p>
        </div>
      </SettingsRow>
    {/if}

    {#if showDay}
      <!-- Per-token Per-Day Request Limit -->
      <SettingsRow
        first={visibleRateKeys[0] === "MAX_REQUESTS_PER_DAY"}
        last={visibleRateKeys[visibleRateKeys.length - 1] ===
          "MAX_REQUESTS_PER_DAY"}
        label={$tr(MAX_DAY_LABEL)}
        description={$tr(MAX_DAY_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >MAX_REQUESTS_PER_DAY</code
          >
          {#if !env.MAX_REQUESTS_PER_DAY}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          {#if sources.MAX_REQUESTS_PER_DAY === "db"}
            <DbBadge settingKey="MAX_REQUESTS_PER_DAY" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="MAX_REQUESTS_PER_DAY"
            value={formValues.MAX_REQUESTS_PER_DAY ?? "1500"}
            {onSaved}
          />
        {/snippet}

        <div class="w-full sm:w-44">
          <div class="relative">
            <input
              type="number"
              min="0"
              step="1"
              aria-label="MAX_REQUESTS_PER_DAY"
              class="fp-input w-full !text-xs !py-1.5 !pr-14"
              placeholder="1500"
              value={formValues.MAX_REQUESTS_PER_DAY ?? "1500"}
              oninput={(e) => {
                const val = e.currentTarget.value.trim();
                onField("MAX_REQUESTS_PER_DAY", val);
              }}
            />
            <span
              class="absolute right-3 top-1/2 -translate-y-1/2 text-[10px] text-text-muted pointer-events-none"
              >req/day</span
            >
          </div>
          <p class="text-[10px] text-[var(--fp-dim)] mt-1">
            {$tr(MAX_DAY_HINT)}
          </p>
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
