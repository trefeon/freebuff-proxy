<script>
  /**
   * Overview — live proxy status, KPI row, and at-risk token cards.
   * Data: GET /admin/api/overview (pooled snapshot + token cards), polled every 15s.
   * All KPIs/cards map to real response fields only.
   */
  import { RefreshCw } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Stat from '../components/Stat.svelte';
  import Card from '../components/Card.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Button from '../components/Button.svelte';
  import { fetchAPI } from '../api/client.js';
  import { formatLocalDate } from '../utils/format.js';
  import { usePolling } from '../utils/polling.js';
  import { tr } from '../i18n.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/overview');
      error = '';
    } catch (e) {
      error = e.message || $tr('Could not reach the proxy API. Check that the server is running.');
    } finally {
      loading = false;
    }
  }

  function retry() {
    error = '';
    loading = true;
    fetchData();
  }

  usePolling(fetchData, 15000);

  // --- KPIs, derived from real overview fields (no invented fields) ---
  let poolTotal = $derived(data?.tokens?.length ?? 0);
  let busyTokens = $derived(data?.tokens?.filter((t) => t.active_runs > 0).length ?? 0);
  let cooldownTokens = $derived(data?.tokens?.filter((t) => t.cooldown_active).length ?? 0);
  let bannedTokens = $derived(data?.tokens?.filter((t) => t.risk_level === 'critical').length ?? 0);
  let requestsToday = $derived(data?.tokens?.reduce((s, t) => s + (t.requests || 0), 0) ?? 0);

  let atRiskTokens = $derived((data?.tokens ?? []).filter((t) => t.risk_level && t.risk_level !== 'low'));

  function riskTone(risk) {
    switch (risk) {
      case 'low':
        return 'good';
      case 'moderate':
        return 'warn';
      case 'high':
      case 'critical':
        return 'bad';
      default:
        return 'idle';
    }
  }

  function banBadge(t) {
    if (t.ban_type === 'hard') {
      return { label: $tr('banned — appeal required'), tone: 'critical', pulse: true };
    }
    if (t.ban_type === 'temporary') {
      const until = formatLocalDate(t.banned_until);
      return { label: until ? $tr('banned until {time}', { time: until }) : $tr('banned (temporary)'), tone: 'bad' };
    }
    return null;
  }

  function formatCooldown(until) {
    if (!until) return '';
    const diff = new Date(until).getTime() - Date.now();
    if (diff <= 0) return '';
    const mins = Math.ceil(diff / 60000);
    if (mins < 60) return `${mins}m`;
    return `${Math.floor(mins / 60)}h ${mins % 60}m`;
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title={$tr('Overview')} description={$tr('Live proxy status and token pool telemetry')}>
    {#snippet actions()}
      {#if data}
        <StatusBadge
          status={data.mode}
          tone={data.in_bridge ? 'good' : 'info'}
        />
        <span class="fp-num text-xs text-[var(--fp-dim)]">up {data.uptime}</span>
      {/if}
    {/snippet}
  </PageHeader>

  <!-- Loading skeleton -->
  {#if loading}
    <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4" aria-hidden="true">
      {#each [1, 2, 3, 4, 5, 6] as _}
        <div class="skeleton skeleton-card"></div>
      {/each}
    </div>
    <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4" aria-hidden="true">
      {#each [1, 2, 3] as _}
        <div class="skeleton skeleton-card"></div>
      {/each}
    </div>
  {/if}

  <!-- Fetch error with retry -->
  {#if error}
    <Alert tone="error" title={$tr('Overview unavailable')}>
      <p>{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={retry}>
          <RefreshCw size={16} />
          {$tr('Retry')}
        </Button>
      </div>
    </Alert>
  {/if}

  {#if data && !loading}
    {#if data.in_bridge}
      <!-- Bridge mode: no pool snapshot to summarize -->
      <Card>
        <p class="text-sm text-[var(--fp-muted)]">
          {$tr('Bridge mode relays upstream tokens per client request')}
          (<span class="fp-num">{data.bridge_tokens}</span> {$tr('active bridge client(s)')}).
          {$tr('Session pools and quota tracking are client-scoped.')}
        </p>
      </Card>
    {:else if !data.has_tokens}
      <EmptyState
        title={$tr('No upstream tokens configured')}
        description={$tr('Add tokens to AUTH_TOKENS in Config to start the pooled relay.')}
      >
        {#snippet action()}
          <a href="#config" class="fp-btn fp-btn-secondary">{$tr('Go to Config')}</a>
        {/snippet}
      </EmptyState>
    {:else}
      <!-- KPI row -->
      <div class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-4">
        <Stat label={$tr('Pool total')} value={poolTotal} big />
        <Stat label={$tr('Busy')} value={busyTokens} hint={$tr('tokens with active runs')} big />
        <Stat label={$tr('Cooldown')} value={cooldownTokens} tone={cooldownTokens > 0 ? 'warn' : 'default'} big />
        <Stat label={$tr('Banned')} value={bannedTokens} hint={$tr('critical risk')} tone={bannedTokens > 0 ? 'bad' : 'default'} big />
        <Stat label={$tr('Requests today')} value={requestsToday} big />
        <Stat label={$tr('Models')} value={data.model_count ?? 0} big />
      </div>

      <!-- Token risk cards -->
      <section aria-label="At-risk tokens">
        <div class="flex items-center justify-between mb-3">
          <h2 class="text-lg font-semibold text-[var(--fp-text)]">{$tr('Token risk')}</h2>
          <span class="fp-num text-xs text-[var(--fp-dim)]">{atRiskTokens.length}/{poolTotal}</span>
        </div>

        {#if atRiskTokens.length > 0}
          <div class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-4 gap-4">
            {#each atRiskTokens as t (t.index)}
              <Card>
                <div class="space-y-3">
                  <div class="flex items-center justify-between gap-2">
                    <span class="fp-num text-sm font-semibold text-[var(--fp-text)]">Token #{t.index}</span>
                    {#if banBadge(t)}
                      <StatusBadge status={banBadge(t).label} tone={banBadge(t).tone} pulse={banBadge(t).pulse} />
                    {:else}
                      <StatusBadge
                        status={t.risk_level}
                        tone={riskTone(t.risk_level)}
                        pulse={t.risk_level === 'critical'}
                      />
                    {/if}
                  </div>

                  {#if t.cooldown_active}
                    <div class="fp-inset px-2.5 py-2 text-xs text-[var(--fp-warning)]">
                      {$tr('Cooldown')} — <span class="fp-num">{formatCooldown(t.cooldown_until)}</span> {$tr('remaining')}
                    </div>
                  {/if}

                  <div class="fp-inset px-2.5 py-2 text-xs text-[var(--fp-muted)]">
                    {#if t.daily_limit > 0}
                      <span class="fp-num text-[var(--fp-text)]">{t.messages_24h}/{t.daily_limit}</span> {$tr('msgs today')}
                      (<span class="fp-num">{t.usage_pct}%</span>)
                    {:else}
                      <span class="fp-num text-[var(--fp-text)]">{t.messages_24h}</span> {$tr('msgs 24h')}
                    {/if}
                  </div>

                  <div class="flex justify-between text-xs text-[var(--fp-dim)]">
                    <span>runs <span class="fp-num text-[var(--fp-text)]">{t.active_runs}</span></span>
                    <span>reqs <span class="fp-num text-[var(--fp-text)]">{t.requests}</span></span>
                  </div>
                </div>
              </Card>
            {/each}
          </div>
        {:else}
          <Card>
            <div class="flex items-center gap-2 text-sm text-[var(--fp-muted)]">
              <span class="led led-good" aria-hidden="true"></span>
              {$tr('All tokens healthy — no risk flags.')}
            </div>
          </Card>
        {/if}
      </section>
    {/if}
  {/if}
</div>
