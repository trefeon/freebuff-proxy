<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Card from "../components/Card.svelte";
  import Stat from "../components/Stat.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");

  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.metrics);
      error = "";
    } catch (e) {
      error = e.message || $tr("Failed to load metrics");
    } finally {
      loading = false;
    }
  }

  onMount(fetchData);

  // Trend shorthand: up/down/flat arrow plus the magnitude, e.g. "↑ 12.5%".
  function trendText(trend) {
    if (!trend) return "—";
    const arrow =
      trend.direction === "up" ? "↑" : trend.direction === "down" ? "↓" : "→";
    const pct = Math.abs(Number(trend.percentage) || 0).toFixed(1);
    return `${arrow} ${pct}%`;
  }

  function riskTone(level) {
    return level === "high" || level === "medium" ? "bad" : "good";
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader
    title={$tr("Metrics")}
    description={$tr(
      "Gateway request counters, transient-retry and rotation activity, and per-token breakdowns.",
    )}
  >
    {#snippet actions()}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Refresh")}
      </Button>
    {/snippet}
  </PageHeader>

  {#if loading}
    <div class="space-y-6" aria-busy="true">
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
        {#each Array(4) as _, i (i)}
          <div class="skeleton skeleton-card h-24"></div>
        {/each}
      </div>
      <div class="skeleton skeleton-card h-64"></div>
      <span class="sr-only">{$tr("Loading metrics")}</span>
    </div>
  {:else if error}
    <div class="space-y-4">
      <Alert tone="error">{error}</Alert>
      <Button variant="secondary" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Retry")}
      </Button>
    </div>
  {:else if data}
    <!-- KPI row -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <Card class="p-4">
        <Stat
          label={$tr("Requests served")}
          value={(data.requests_total ?? 0).toLocaleString()}
          hint={$tr("{count} sample(s)", { count: data.sample_count ?? 0 })}
        />
      </Card>
      <Card class="p-4">
        <Stat
          label={$tr("Transient retries")}
          value={(data.transient_retries ?? 0).toLocaleString()}
          hint={$tr("trend {trend}", { trend: trendText(data.retries_trend) })}
          tone={(data.retries_trend?.direction ?? "flat") === "up"
            ? "warn"
            : "default"}
        />
      </Card>
      <Card class="p-4">
        <Stat
          label={$tr("Fingerprint rotations")}
          value={(data.fingerprint_rotations ?? 0).toLocaleString()}
          tone={(data.fingerprint_rotations ?? 0) > 0 ? "warn" : "default"}
        />
      </Card>
      <Card class="p-4">
        <Stat
          label={$tr("Models served")}
          value={data.models ?? 0}
          hint={$tr("trend {trend}", { trend: trendText(data.requests_trend) })}
        />
      </Card>
    </div>

    <!-- Sparkline row: server-rendered SVG; content is numeric coordinates
         plus static colors/labels only (see sparklineSVG), so it carries no
         user-controlled data and is safe to embed. -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <Card
        title={$tr("Requests over time")}
        description={$tr("Samples appended per poll — rolling window of 120.")}
        pad="none"
      >
        <div class="px-5 py-4 h-16 w-full [&_svg]:w-full [&_svg]:h-full">
          {#if data.requests_spark}
            <!-- eslint-disable-next-line svelte/no-at-html-tags -- server-rendered SVG: numeric coords + static colors only (sparklineSVG), no user-controlled data -->
            {@html data.requests_spark}
          {:else}
            <p class="text-sm text-[var(--fp-muted)]">
              {$tr("Not enough samples yet.")}
            </p>
          {/if}
        </div>
      </Card>
      <Card
        title={$tr("Retries over time")}
        description={$tr("Transient retry activity across the same window.")}
        pad="none"
      >
        <div class="px-5 py-4 h-16 w-full [&_svg]:w-full [&_svg]:h-full">
          {#if data.retries_spark}
            <!-- eslint-disable-next-line svelte/no-at-html-tags -- server-rendered SVG: numeric coords + static colors only (sparklineSVG), no user-controlled data -->
            {@html data.retries_spark}
          {:else}
            <p class="text-sm text-[var(--fp-muted)]">
              {$tr("Not enough samples yet.")}
            </p>
          {/if}
        </div>
      </Card>
    </div>

    <!-- Per-token breakdown -->
    <Card
      title={$tr("Per-token metrics")}
      description={$tr(
        "24h request counts and risk posture for every pool token.",
      )}
      pad="none"
    >
      {#if data.per_tokens?.length}
        <div class="overflow-x-auto">
          <table class="fp-table">
            <caption class="sr-only"
              >{$tr(
                "Per-token metrics — requests, retries, rotations, spend and risk",
              )}</caption
            >
            <thead>
              <tr>
                <th scope="col">{$tr("Token")}</th>
                <th scope="col">{$tr("Requests (24h)")}</th>
                <th scope="col">{$tr("Transient retries")}</th>
                <th scope="col">{$tr("Fingerprint rotations")}</th>
                <th scope="col">{$tr("Spend")}</th>
                <th scope="col">{$tr("Risk")}</th>
              </tr>
            </thead>
            <tbody>
              {#each data.per_tokens as p (p.token)}
                <tr>
                  <td
                    ><span class="fp-num font-mono text-xs">#{p.token}</span
                    ></td
                  >
                  <td class="fp-num"
                    >{Number(p.requests_24h ?? 0).toLocaleString()}</td
                  >
                  <td class="fp-num"
                    >{Number(p.transient_retries ?? 0).toLocaleString()}</td
                  >
                  <td class="fp-num"
                    >{Number(p.fingerprint_rotations ?? 0).toLocaleString()}</td
                  >
                  <td class="fp-num"
                    >{Number(p.spend_day ?? 0).toLocaleString()}</td
                  >
                  <td>
                    <StatusBadge
                      status={p.risk_level || "low"}
                      tone={riskTone(p.risk_level)}
                    />
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No pool tokens yet.")}
          </p>
        </div>
      {/if}
    </Card>
  {/if}
</div>
