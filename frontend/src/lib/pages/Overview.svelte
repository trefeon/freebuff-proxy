<script>
  /**
   * Overview — live proxy status, KPI row, and at-risk token cards.
   * Data: the overview endpoint (pooled snapshot + token cards), polled every 15s.
   * All KPIs/cards map to real response fields only.
   */
  import { onMount } from "svelte";
  import { RefreshCw, ExternalLink } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import ApiKeysEditor from "../components/ApiKeysEditor.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import Stat from "../components/Stat.svelte";
  import Card from "../components/Card.svelte";
  import CopyButton from "../components/CopyButton.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import PremiumQuotaBar from "../components/PremiumQuotaBar.svelte";
  import AnnouncementsBanner from "../components/AnnouncementsBanner.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { usePolling } from "../utils/polling.js";
  import { tr } from "../i18n.js";
  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let now = $state(Date.now());

  // Issue #322: restart/deploy-only fields (mode, model_count, safe_mode,
  // transient_retries, max_messages_per_day, upstream_sync) and account-stable
  // card fields (email, standing_*, referral_*) ride a once-per-mount full
  // fetch; the 15s hot poll hits ?view=live and merges over the cached static
  // snapshot. A full refresh every ~5min (or when the cache is empty) picks
  // up mid-session changes (mode switches, trust updates, registry syncs).
  const LIVE_QS = "?view=live";
  const FULL_EVERY_POLLS = 20;
  const FULL_EVERY_MS = 5 * 60 * 1000;
  const STATIC_TOP_KEYS = [
    "base_url",
    "mode",
    "in_bridge",
    "show_bridge",
    "models",
    "model_count",
    "safe_mode",
    "max_messages_per_day",
    "transient_retries",
    "fingerprint_rotations",
    "is_default_admin_token",
    "upstream_sync",
  ];
  const STATIC_TOKEN_KEYS = [
    "email",
    "account_id",
    "daily_limit",
    "has_standing",
    "standing_level",
    "standing_label",
    "standing_score",
    "standing_next_level",
    "standing_next_level_at",
    "standing_capped_by",
    "standing_capped_reason",
    "standing_blurb",
    "standing_next_steps",
    "has_referral",
    "referral_code",
    "referral_qualified_count",
    "referral_sessions_left",
    "referral_github_linked",
    "referral_reset_at",
    "allowed_models",
  ];
  let staticPart = null;
  let staticTokensByIndex = {};
  let polls = 0;
  let lastFullAt = 0;

  function pick(obj, keys) {
    const out = {};
    for (const k of keys) if (k in obj) out[k] = obj[k];
    return out;
  }

  function rememberStatic(full) {
    staticPart = pick(full, STATIC_TOP_KEYS);
    staticTokensByIndex = {};
    for (const t of full.tokens ?? []) {
      staticTokensByIndex[t.index] = pick(t, STATIC_TOKEN_KEYS);
    }
    lastFullAt = Date.now();
  }

  function mergeLive(live) {
    // Old servers and hermetic mocks answer the live URL with the full shape:
    // refresh the static cache instead of rendering stale snapshots.
    if ("mode" in live && "model_count" in live) rememberStatic(live);
    return {
      ...staticPart,
      ...live,
      tokens: (live.tokens ?? []).map((lt) => ({
        ...(staticTokensByIndex[lt.index] ?? {}),
        ...lt,
      })),
    };
  }

  async function fetchFull() {
    const full = await fetchAPI(adminApi.overview);
    rememberStatic(full);
    data = full;
  }

  async function fetchData() {
    try {
      polls += 1;
      if (
        staticPart === null ||
        polls % FULL_EVERY_POLLS === 0 ||
        Date.now() - lastFullAt > FULL_EVERY_MS
      ) {
        await fetchFull();
      } else {
        data = mergeLive(await fetchAPI(adminApi.overview + LIVE_QS));
      }
    } catch (e) {
      error =
        e.message ||
        $tr("Could not reach the proxy API. Check that the server is running.");
    } finally {
      loading = false;
    }
  }

  let tick = null;
  onMount(() => {
    fetchData();
    tick = setInterval(() => {
      now = Date.now();
    }, 1000);
    return () => clearInterval(tick);
  });

  function retry() {
    error = "";
    loading = true;
    fetchData();
  }

  usePolling(fetchData, 15000);
  let poolTotal = $derived(data?.tokens?.length ?? 0);
  let busyTokens = $derived(
    data?.tokens?.filter((t) => t.active_runs > 0).length ?? 0,
  );
  let cooldownTokens = $derived(
    data?.tokens?.filter((t) => t.cooldown_active).length ?? 0,
  );
  let bannedTokens = $derived(
    data?.tokens?.filter((t) => t.risk_level === "critical").length ?? 0,
  );
  let requestsToday = $derived(
    data?.tokens?.reduce((s, t) => s + (t.requests || 0), 0) ?? 0,
  );

  // Freebucks: per-token daily/weekly/monthly + balance + bindingWindow (issue #232)
  let hasFreebucks = $derived((data?.tokens ?? []).some((t) => t.freebucks));
  let freebucksTokens = $derived(
    (data?.tokens ?? []).filter((t) => t.freebucks),
  );
  let hasBridgeFreebucks = $derived(
    (data?.bridge_token_cards ?? []).some((c) => c.freebucks),
  );

  // Dynamic Base URL follows the browser's current host (VPS IP, domain, VPN reverse proxy)
  // as computed dynamically by the backend from the request headers (Host, X-Forwarded-Host/Proto).
  let dynamicBaseURL = $derived.by(() => {
    if (data?.base_url) {
      return data.base_url;
    }
    if (typeof window !== "undefined" && window.location.host) {
      return `${window.location.protocol}//${window.location.host}/v1`;
    }
    return "http://127.0.0.1:3457/v1";
  });
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Overview")}
    description={$tr("Live proxy status and token pool telemetry")}
  >
    {#snippet actions()}
      {#if data}
        <StatusBadge
          status={data.mode}
          tone={data.in_bridge ? "good" : "info"}
        />
        <span class="fp-num text-xs text-[var(--fp-dim)]">up {data.uptime}</span
        >
      {/if}
    {/snippet}
  </PageHeader>

  <!-- Upstream announcements and broadcasts -->
  <AnnouncementsBanner />

  <!-- Loading skeleton — live region announces loading without duplicating Alert -->
  {#if loading}
    <div aria-live="polite" aria-busy="true">
      <span class="sr-only">{$tr("Loading overview…")}</span>
      <div
        class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4"
        aria-hidden="true"
      >
        {#each [1, 2, 3, 4, 5, 6] as _, i (i)}
          <div class="skeleton skeleton-card"></div>
        {/each}
      </div>
      <div
        class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4 mt-4"
        aria-hidden="true"
      >
        {#each [1, 2, 3] as _, i (i)}
          <div class="skeleton skeleton-card"></div>
        {/each}
      </div>
    </div>
  {/if}

  <!-- Upstream sync banner: warns operators that the running build is
       behind CodebuffAI/freebuff@main. Data ships compiled into the
       binary (see backend/internal/dashboard/data/upstream_drift.json) and is
       refreshed by .github/workflows/upstream-drift.yml. -->
  {#if data?.upstream_sync}
    {@const us = data.upstream_sync}
    {#if us.has_drift}
      <Alert
        tone={us.has_registry_drift ? "error" : "warning"}
        title={$tr("Upstream has updates — your build is behind")}
      >
        <p class="mb-2">
          {$tr(
            "CodebuffAI/freebuff moved past vendor {sha} (checked {when}). This build knows about {pinned} upstream SHAs; a newer one is on main.",
            {
              sha: us.upstream_sha,
              when: us.checked_at,
              pinned: us.drifted_files?.length ?? 0,
            },
          )}
        </p>
        {#if us.drifted_files && us.drifted_files.length > 0}
          <ul class="text-xs space-y-1 mb-3">
            {#each us.drifted_files as f (f.file)}
              <li>
                <span class="fp-num text-[var(--fp-muted)]">[{f.group}]</span>
                <code class="fp-num text-xs">{f.file}</code>
                {#if f.pinned_sha}
                  <span class="fp-num text-[var(--fp-dim)]"
                    >({f.pinned_sha} -> {f.vendor_sha})</span
                  >
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
        <p class="text-xs text-[var(--fp-muted)]">
          {$tr(
            "Registry pin drift can land via the auto-synced PR; wire-shape drift needs a human to port (the upstream-drift workflow opens a needs-port issue for each).",
          )}
        </p>
        <a
          href={us.releases_url}
          target="_blank"
          rel="noopener noreferrer"
          class="mt-2 inline-flex items-center gap-1 text-xs text-[var(--fp-accent)] hover:underline"
        >
          {$tr("Open releases page")}
          <ExternalLink size={12} />
        </a>
      </Alert>
    {/if}
  {/if}

  <!-- Fetch error with retry -->
  {#if error}
    <Alert tone="error" title={$tr("Overview unavailable")}>
      <p>{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={retry}>
          <RefreshCw size={16} />
          {$tr("Retry")}
        </Button>
      </div>
    </Alert>
  {/if}

  {#if data && !loading}
    {#if data.has_tokens}
      <!-- KPI row (pooled tokens active) -->
      <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4">
        <Stat label={$tr("Pool total")} value={poolTotal} big />
        <Stat
          label={$tr("Busy")}
          value={busyTokens}
          hint={$tr("tokens with active runs")}
          big
        />
        <Stat
          label={$tr("Cooldown")}
          value={cooldownTokens}
          tone={cooldownTokens > 0 ? "warn" : "default"}
          big
        />
        <Stat
          label={$tr("Banned")}
          value={bannedTokens}
          hint={$tr("critical risk")}
          tone={bannedTokens > 0 ? "bad" : "default"}
          big
        />
        <Stat label={$tr("Requests today")} value={requestsToday} big />
        <Stat label={$tr("Models")} value={data.model_count ?? 0} big />
      </div>

      <!-- Hybrid mode: pool summary above plus a compact bridge-relay card -->
      {#if data.mode === "hybrid"}
        <Card title={$tr("Bridge relay")}>
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr(
              "{count} active bridge client(s) relaying their own FreeBuff tokens",
              { count: data.bridge_tokens ?? 0 },
            )}
          </p>
          {#if data.bridge_token_cards?.length}
            <ul class="mt-2 flex flex-col gap-1.5">
              {#each data.bridge_token_cards.slice(0, 4) as bc (bc.key)}
                <li class="flex flex-wrap items-center gap-2 text-xs">
                  <StatusBadge status={bc.status} />
                  <code class="fp-num font-mono text-[var(--fp-text)]"
                    >{bc.key}</code
                  >
                  {#if bc.model}
                    <code class="fp-num font-mono text-[var(--fp-muted)]"
                      >{bc.model}</code
                    >
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
        </Card>
      {/if}
    {:else}
      <!-- Bridge mode / empty pool summary -->
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
        <Stat
          label={$tr("Relay Mode")}
          value={data.in_bridge ? "Bridge" : "Hybrid"}
          hint={data.in_bridge
            ? $tr("client-supplied tokens")
            : $tr("shared pool + bridge")}
          big
        />
        <Stat
          label={$tr("Active Bridge Clients")}
          value={data.bridge_tokens ?? 0}
          hint={$tr("relaying upstream sessions")}
          big
        />
        <Stat
          label={$tr("Served Models")}
          value={data.model_count ?? 0}
          hint={$tr("OpenAI & Anthropic")}
          big
        />
      </div>

      <Card
        title={$tr("Gateway Ready — Bridge & Pooled Relay")}
        description={$tr(
          "The gateway is online and ready for traffic. Connect your tools directly in Bridge mode, or add FreeBuff accounts to create a shared token pool.",
        )}
      >
        {#snippet actions()}
          <a
            href="#tokens"
            class="fp-btn fp-btn-secondary fp-btn-sm inline-flex items-center gap-1.5"
          >
            <span>{$tr("Manage Tokens")}</span>
          </a>
        {/snippet}
        <div class="text-xs text-[var(--fp-muted)] space-y-2">
          <p>
            <strong class="text-[var(--fp-text)]"
              >{$tr("Bridge Mode (Active):")}</strong
            >
            {$tr(
              "Clients can send requests using their own FreeBuff token as the Bearer or x-api-key credential.",
            )}
          </p>
          <p>
            <strong class="text-[var(--fp-text)]"
              >{$tr("Pooled Mode (Ready):")}</strong
            >
            {$tr(
              "Add FreeBuff accounts in Tokens (via Device Login or pasting tokens) to enable shared pool rotation, admission coercion, and Client API Key routing.",
            )}
          </p>
        </div>
      </Card>

      {#if data.bridge_token_cards?.length}
        <Card title={$tr("Active Bridge Clients")}>
          <ul class="flex flex-col gap-1.5">
            {#each data.bridge_token_cards as bc (bc.key)}
              <li class="flex flex-wrap items-center gap-2 text-xs">
                <StatusBadge status={bc.status} />
                <code class="fp-num font-mono text-[var(--fp-text)]"
                  >{bc.key}</code
                >
                {#if bc.model}
                  <code class="fp-num font-mono text-[var(--fp-muted)]"
                    >{bc.model}</code
                  >
                {/if}
              </li>
            {/each}
          </ul>
        </Card>
      {/if}
    {/if}

    {#if hasFreebucks || hasBridgeFreebucks}
      <Card
        title={$tr("Freebucks — allowance")}
        description={$tr(
          "Daily, weekly and monthly windows with balance and binding window — per token",
        )}
      >
        <div class="grid grid-cols-1 gap-4">
          {#each freebucksTokens as tok, i (tok.index ?? tok.account_id ?? i)}
            <PremiumQuotaBar
              freebucks={tok.freebucks}
              title={$tr("Account #{index} • Freebucks", {
                index: (tok.index ?? i) + 1,
              })}
              {now}
            />
          {/each}
          {#if hasBridgeFreebucks}
            {#each (data.bridge_token_cards ?? []).filter((c) => c.freebucks) as bc (bc.key)}
              <PremiumQuotaBar
                freebucks={bc.freebucks}
                title={`${bc.key} • Freebucks`}
                {now}
              />
            {/each}
          {/if}
        </div>
      </Card>
    {/if}

    <!-- Universal Client Integration & Endpoints Card (Always Available) -->
    <section aria-label="Client integration">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold text-[var(--fp-text)]">
          {$tr("Client Integration")}
        </h2>
        <span class="text-xs font-mono text-[var(--fp-muted)]"
          >OpenAI & Anthropic Compatible</span
        >
      </div>

      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <!-- Gateway Base URL -->
        <Card
          title={$tr("Gateway Base URL")}
          description={$tr(
            "Universal base endpoint for any OpenAI or Anthropic client, SDK, or CLI tool.",
          )}
        >
          <div class="flex items-center gap-2">
            <div class="fp-inset flex-1 px-3 py-2 overflow-x-auto">
              <code
                class="fp-num text-xs text-[var(--fp-accent)] font-mono font-semibold"
                >{dynamicBaseURL}</code
              >
            </div>
            <CopyButton text={dynamicBaseURL} label={$tr("Copy URL")} />
          </div>
          <p class="mt-3 text-xs text-[var(--fp-muted)]">
            {$tr(
              "Authentication: Use any Client API Key below via Bearer token or x-api-key header.",
            )}
          </p>
        </Card>

        <!-- Supported Protocols & Routes -->
        <Card
          title={$tr("Supported Wire Protocols")}
          description={$tr(
            "Dual-protocol translation handled transparently by the gateway.",
          )}
        >
          <div class="space-y-2">
            <div
              class="fp-inset px-3 py-2 flex items-center justify-between gap-2 text-xs"
            >
              <div class="flex items-center gap-2 min-w-0">
                <span
                  class="px-1.5 py-0.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] font-mono text-[10px] text-[var(--fp-accent)]"
                  >OpenAI</span
                >
                <span class="font-mono text-[var(--fp-text)] truncate"
                  >POST /v1/chat/completions</span
                >
              </div>
              <span class="text-[var(--fp-dim)] text-[11px] shrink-0"
                >Cursor, Aider, OMP</span
              >
            </div>
            <div
              class="fp-inset px-3 py-2 flex items-center justify-between gap-2 text-xs"
            >
              <div class="flex items-center gap-2 min-w-0">
                <span
                  class="px-1.5 py-0.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] font-mono text-[10px] text-[#A78BFA]"
                  >Anthropic</span
                >
                <span class="font-mono text-[var(--fp-text)] truncate"
                  >POST /v1/messages</span
                >
              </div>
              <span class="text-[var(--fp-dim)] text-[11px] shrink-0"
                >Claude Code, Cline</span
              >
            </div>
          </div>
        </Card>
      </div>

      <div class="mt-4">
        <ApiKeysEditor />
      </div>
    </section>
  {/if}
</div>
