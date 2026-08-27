<script>
  import { RefreshCw, ChevronLeft, ChevronRight, Search, EyeOff } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Card from '../components/Card.svelte';
  import Button from '../components/Button.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import { fetchAPI } from '../api/client.js';
  import { usePolling } from '../utils/polling.js';
  import { formatTime, parseLogFields } from '../utils/format.js';
  import { tr } from '../i18n.js';

  /** @type {any} */
  let data = $state(null);
  let loading = $state(true);
  let error = $state('');
  let manualRefresh = $state(false);
  let filterLevel = $state('info');
  let filterMsg = $state('');
  let hideAdmin = $state(true);
  let autoPoll = $state(true);
  let page = $state(0);
  const PAGE_SIZE = 50;

  let entries = $derived.by(() => data?.entries || []);
  let filteredEntries = $derived.by(() => {
    if (!hideAdmin) return entries;
    return entries.filter((e) => {
      const fields = parseLogFields(e.fields);
      return !fields.some((f) => f.key === 'path' && String(f.value).includes('/admin'));
    });
  });
  let pagedEntries = $derived.by(() => {
    const start = page * PAGE_SIZE;
    return filteredEntries.slice(start, start + PAGE_SIZE);
  });
  let totalPages = $derived.by(() => Math.max(1, Math.ceil(filteredEntries.length / PAGE_SIZE)));
  let hasActiveFilter = $derived.by(
    () => filterLevel !== 'info' || filterMsg.trim() !== '' || !hideAdmin
  );
  let rangeStart = $derived.by(() => (filteredEntries.length === 0 ? 0 : page * PAGE_SIZE + 1));
  let rangeEnd = $derived.by(() => Math.min((page + 1) * PAGE_SIZE, filteredEntries.length));
  async function fetchLogs() {
    try {
      const query = new URLSearchParams();
      if (filterLevel) query.set('level', filterLevel);
      if (filterMsg.trim()) query.set('msg', filterMsg.trim());
      const res = await fetchAPI(`/admin/api/logs?${query.toString()}`);
      data = res;
      error = '';
      const tp = Math.ceil((res?.entries?.length || 0) / PAGE_SIZE);
      if (page > tp - 1) page = 0;
    } catch (e) {
      error = e.message
        ? $tr('Could not load log entries: {reason}', { reason: e.message })
        : $tr('Could not load log entries');
    } finally {
      loading = false;
      manualRefresh = false;
    }
  }

  async function refresh() {
    manualRefresh = true;
    await fetchLogs();
  }

  function handleFilterChange() {
    page = 0;
    fetchLogs();
  }

  function clearFilters() {
    filterLevel = 'info';
    filterMsg = '';
    hideAdmin = true;
    handleFilterChange();
  }

  // Auto-poll every 5s while enabled; manual refresh / filter changes always fetch.
  usePolling(async () => {
    if (autoPoll) await fetchLogs();
  }, 5000);

  function levelTone(level) {
    switch (level) {
      case 'error':
        return 'bad';
      case 'warn':
        return 'warn';
      case 'info':
        return 'info';
      default:
        return 'idle';
    }
  }
</script>

<div class="space-y-4 page-enter">
  <PageHeader
    title={$tr('Logs')}
    description={$tr('Structured entries from the in-memory ring buffer (200 max, newest first), filtered by level and message.')}
  />

  {#if error}
    <Alert tone="error" title={$tr('Could not load log entries')}>
      <p class="text-sm">{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={refresh}>{$tr('Retry')}</Button>
      </div>
    </Alert>
  {/if}

  {#if loading && !data}
    <div aria-live="polite" aria-busy="true">
      <span class="sr-only">{$tr('Loading logs…')}</span>
      <Card pad="none">
        <div class="p-4 space-y-3" aria-hidden="true">
          {#each [0, 1, 2, 3, 4, 5, 6] as i (i)}
            <div class="flex items-center gap-3">
              <span class="skeleton rounded-full size-2 shrink-0"></span>
              <span class="skeleton skeleton-line" style="width:{45 + (i % 4) * 12}%"></span>
              <span class="skeleton skeleton-line ml-auto" style="width:15%"></span>
            </div>
          {/each}
        </div>
      </Card>
    </div>
  {:else if data && !data.enabled}
    <EmptyState
      title={$tr('Log ring disabled')}
      description={$tr('The server was started without an active logring handler, so no log entries are available.')}
    />
  {:else if data}
    <Card pad="none">
      <!-- Integrated Top Toolbar Header -->
      <div class="p-3 bg-[var(--fp-surface)] border-b border-[var(--fp-border)] flex flex-col sm:flex-row sm:items-center justify-between gap-2.5">
        <!-- Filter Controls (Left Group) -->
        <div class="flex flex-wrap items-center gap-2 flex-1">
          <!-- Level Select -->
          <label for="log-level" class="sr-only">{$tr('Log level')}</label>
          <select
            id="log-level"
            class="fp-input !text-xs !py-1 !px-2.5 !h-8 !w-auto !inline-block"
            bind:value={filterLevel}
            onchange={handleFilterChange}
          >
            <option value="">{$tr('All levels')}</option>
            <option value="debug">{$tr('Debug')}</option>
            <option value="info">{$tr('Info')}</option>
            <option value="warn">{$tr('Warn')}</option>
            <option value="error">{$tr('Error')}</option>
          </select>

          <!-- Search Input with Search Icon -->
          <div class="relative flex-1 min-w-[180px] max-w-xs">
            <Search size={13} class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] pointer-events-none" />
            <label for="log-msg" class="sr-only">{$tr('Filter by message')}</label>
            <input
              id="log-msg"
              type="text"
              class="fp-input !text-xs !pl-8 !pr-2.5 !py-1 !h-8 !w-full"
              bind:value={filterMsg}
              oninput={handleFilterChange}
              placeholder={$tr('Filter message…')}
            />
          </div>

          <!-- Hide admin toggle -->
          <Button
            variant={hideAdmin ? 'secondary' : 'ghost'}
            size="sm"
            aria-pressed={hideAdmin}
            onclick={() => { hideAdmin = !hideAdmin; page = 0; }}
            class="!h-8 !text-xs !px-2.5 shrink-0"
          >
            <EyeOff size={13} />
            <span>{$tr('Hide admin')}</span>
          </Button>

          {#if hasActiveFilter}
            <Button
              variant="ghost"
              size="sm"
              onclick={clearFilters}
              class="!h-8 !text-xs !px-2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] shrink-0"
            >
              {$tr('Clear filters')}
            </Button>
          {/if}
        </div>

        <!-- Live Controls (Right Group) -->
        <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
          <span class="inline-flex items-center gap-1.5 text-xs font-mono text-[var(--fp-muted)] mr-1">
            <span class="led {filteredEntries.length > 0 ? 'led-good' : 'led-idle'}"></span>
            <span>{filteredEntries.length} {$tr('entries')}</span>
          </span>

          <Button
            variant="ghost"
            size="sm"
            aria-pressed={autoPoll}
            onclick={() => (autoPoll = !autoPoll)}
            class="!h-8 !text-xs !px-2.5"
          >
            {$tr('Auto {state}', { state: autoPoll ? $tr('on') : $tr('off') })}
          </Button>

          <Button
            variant="secondary"
            size="sm"
            loading={manualRefresh}
            onclick={refresh}
            disabled={loading && !data}
            class="!h-8 !text-xs !px-2.5"
          >
            <RefreshCw size={13} />
            <span>{$tr('Refresh')}</span>
          </Button>
        </div>
      </div>

      {#if filteredEntries.length === 0}
        <div class="p-8">
          <EmptyState
            title={$tr('No matching log entries')}
            description={hasActiveFilter
              ? $tr('No log entries matched your level or message filter.')
              : $tr('The log ring is empty — entries will appear here as the proxy logs activity.')}
          >
            {#if hasActiveFilter}
              {#snippet action()}
                <Button variant="secondary" size="sm" onclick={clearFilters}>{$tr('Clear filters')}</Button>
              {/snippet}
            {/if}
          </EmptyState>
        </div>
      {:else}
        <div class="fp-inset m-3.5 overflow-x-auto">
          <ul class="divide-y divide-[var(--fp-border)]">
          {#each pagedEntries as e}
            {@const fields = parseLogFields(e.fields)}
            {@const entryJson = JSON.stringify(
              { time: e.time, level: e.level, message: e.message, fields: e.fields || '' },
              null,
              2
            )}
            <li class="px-4 py-2.5 hover:bg-[var(--fp-surface-2)] transition-colors">
              <div class="flex items-center gap-3">
                <StatusBadge status={e.level} tone={levelTone(e.level)} />
                <span class="fp-num text-xs text-[var(--fp-dim)] shrink-0">{formatTime(e.time)}</span>
                <span class="font-mono text-sm text-[var(--fp-text)] min-w-0 flex-1 truncate">{e.message}</span>
                <span class="shrink-0">
                  <CopyButton text={entryJson} label="Copy" />
                </span>
              </div>
              {#if fields.length > 0}
                <div class="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 pl-1">
                  {#each fields as f}
                    <span class="font-mono text-[11px] text-[var(--fp-muted)]">
                      <span class="text-[var(--fp-dim)]">{f.key}</span>=<span>{f.value}</span>
                    </span>
                  {/each}
                </div>
              {/if}
            </li>
          {/each}
        </ul>
      </div>
      {/if}
      {#snippet footer()}
        <div class="flex items-center justify-between gap-3 px-4 py-3">
          <span class="fp-num text-xs text-[var(--fp-muted)]">
            {rangeStart}–{rangeEnd} of {filteredEntries.length}
          </span>
          <div class="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              class="fp-num"
              disabled={page === 0}
              onclick={() => page--}
            >
              <ChevronLeft size={14} />
              {$tr('Prev')}
            </Button>
            <span class="fp-num text-xs text-[var(--fp-muted)]">
              {$tr('Page {current} / {total}', { current: page + 1, total: totalPages })}
            </span>
            <Button
              variant="secondary"
              size="sm"
              class="fp-num"
              disabled={page >= totalPages - 1}
              onclick={() => page++}
            >
              {$tr('Next')}
              <ChevronRight size={14} />
            </Button>
          </div>
        </div>
      {/snippet}
    </Card>
  {/if}
</div>
