<script>
  import { Search, X, Sparkles, SlidersHorizontal, RefreshCw } from '@lucide/svelte';
  import Button from '../../components/Button.svelte';
  import Card from '../../components/Card.svelte';
  import EmptyState from '../../components/EmptyState.svelte';
  import ConfigGroup from './ConfigGroup.svelte';
  import { tr } from '../../i18n.js';

  /**
   * ConfigEditor - the interactive catalog form: mode toggle, search box and
   * the per-group cards (via ConfigGroup). Split out of Settings.svelte (issue
   * #287) so the shell only orchestrates state/save while this owns the
   * editable-form rendering.
   *
   * @prop {Array} meta
   * @prop {Array} groups
   * @prop {Object} formValues
   * @prop {string} rawText
   * @prop {Object} reveal
   * @prop {string} searchQuery
   * @prop {string} viewMode
   * @prop {(value: string) => void} onSearch
   * @prop {(mode: string) => void} onViewMode
   * @prop {(key: string, value: string) => void} onField
   * @prop {(key: string) => void} onToggleReveal
   * @prop {(name: string) => void} onToggleGroup
   * @prop {() => void} onResetFilters
   */
  let {
    meta,
    groups,
    formValues,
    rawText,
    reveal,
    searchQuery,
    viewMode,
    onSearch,
    onViewMode,
    onField,
    onToggleReveal,
    onToggleGroup,
    onResetFilters,
  } = $props();

  function isKeyEssential(entry) {
    return Boolean(entry?.essential);
  }
</script>

<!-- Filter and Search Toolbar -->
<div class="flex flex-col sm:flex-row gap-3 items-stretch sm:items-center justify-between pb-1">
  <!-- Mode Toggle -->
  <div class="flex items-center gap-1 p-1 rounded-lg bg-[var(--fp-surface)] border border-[var(--fp-border)]">
    <button
      type="button"
      class="px-3 py-1 rounded-md text-xs font-medium transition-colors flex items-center gap-1.5 {viewMode === 'essential' ? 'bg-[var(--fp-accent)] text-[var(--fp-accent-fg,white)] font-semibold shadow-sm' : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
      onclick={() => onViewMode('essential')}
    >
      <Sparkles size={13} />
      <span>{$tr('Essential ({count})', { count: meta.filter(e => !e.hidden && isKeyEssential(e)).length })}</span>
    </button>
    <button
      type="button"
      class="px-3 py-1 rounded-md text-xs font-medium transition-colors flex items-center gap-1.5 {viewMode === 'all' ? 'bg-[var(--fp-accent)] text-[var(--fp-accent-fg,white)] font-semibold shadow-sm' : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
      onclick={() => onViewMode('all')}
    >
      <SlidersHorizontal size={13} />
      <span>{$tr('All Settings ({count})', { count: meta.filter(e => !e.hidden).length })}</span>
    </button>
  </div>

  <!-- Search Box -->
  <div class="relative min-w-[220px] sm:w-72">
    <input
      type="text"
      value={searchQuery}
      oninput={(e) => onSearch(e.currentTarget.value)}
      placeholder={$tr('Search settings by key or description…')}
      class="fp-input !pl-8 !pr-8 !py-1.5 !text-xs !w-full"
    />
    <Search size={14} class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] pointer-events-none" />
    {#if searchQuery}
      <button
        type="button"
        onclick={() => onSearch('')}
        class="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-0.5"
        aria-label={$tr('Clear search')}
      >
        <X size={13} />
      </button>
    {/if}
  </div>
</div>

<!-- Key catalog groups: one Card per group, one typed control per key -->
<div class="space-y-6">
  {#each groups as group}
    <ConfigGroup
      {group}
      {formValues}
      {rawText}
      {reveal}
      onField={onField}
      onToggleReveal={onToggleReveal}
      onToggleGroup={onToggleGroup}
    />
  {/each}

  {#if !groups.length}
    <Card>
      <EmptyState title={$tr('No matching configuration keys')} description={$tr('No settings match the active search or category filter.')}>
        {#snippet action()}
          <Button variant="secondary" onclick={onResetFilters}>
            <RefreshCw size={14} />
            {$tr('Reset filters')}
          </Button>
        {/snippet}
      </EmptyState>
    </Card>
  {/if}
</div>
