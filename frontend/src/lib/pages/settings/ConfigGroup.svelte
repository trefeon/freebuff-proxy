<script>
  import { ChevronDown, ChevronUp, Eye, EyeOff } from '@lucide/svelte';
  import Card from '../../components/Card.svelte';
  import { tr } from '../../i18n.js';
  import { parseEnv } from '../../utils/env.js';

  /**
   * ConfigGroup - one catalog group rendered as a Card with a row per config
   * key (split out of Settings.svelte per issue #287). The shell owns the
   * form state; this component only renders a group's entries with the typed
   * control for each key's kind.
   *
   * @prop {Object} group - { name, entries, essential, advanced, isExpanded, displayed }
   * @prop {Object} formValues - meta key → display value
   * @prop {string} rawText - current .env document (drives the 'default' badge)
   * @prop {Object} reveal - secret key → revealed
   * @prop {(key: string, value: string) => void} onField
   * @prop {(key: string) => void} onToggleReveal
   * @prop {(name: string) => void} onToggleGroup
   */
  let {
    group,
    formValues,
    rawText,
    reveal,
    onField,
    onToggleReveal,
    onToggleGroup,
  } = $props();

  const GROUP_LABELS = {
    general: 'General',
    pool: 'Pool',
    quota: 'Quota',
    upstream: 'Upstream',
    security: 'Security',
  };
  const GROUP_DESCRIPTIONS = {
    general: 'Runtime and logging knobs.',
    pool: 'Token pool, sessions and bridge mode.',
    quota: 'Quota and rate limits.',
    upstream: 'Upstream gateway and model routing.',
    security: 'Access control and secrets.',
  };

  function groupLabel(name) {
    return GROUP_LABELS[name] || name;
  }
  function groupDescription(name) {
    return GROUP_DESCRIPTIONS[name] || '';
  }
</script>

<Card title={groupLabel(group.name)} description={groupDescription(group.name)}>
  <div class="divide-y divide-[var(--fp-border)]">
    {#each group.displayed as entry (entry.key)}
      <div class="py-3.5 first:pt-0 last:pb-0 grid grid-cols-1 md:grid-cols-12 gap-3 md:gap-6 items-start">
        <div class="md:col-span-6 min-w-0">
          <div class="flex flex-wrap items-center gap-2">
            <span id={`setting-${entry.key}`} class="fp-num text-[12px] font-semibold text-[var(--fp-text)]">{entry.key}</span>
            {#if entry.restart_only}
              <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-warning)]/40 bg-[var(--fp-warning)]/15 text-[#FCD34D] font-semibold uppercase tracking-wider shrink-0">{$tr('restart')}</span>
            {/if}
            {#if entry.secret}
              <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-error)]/40 bg-[var(--fp-error)]/15 text-[#FCA5A5] font-semibold uppercase tracking-wider shrink-0">{$tr('secret')}</span>
            {/if}
            {#if !parseEnv(rawText)[entry.key]}
              <span class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0">{$tr('default')}</span>
            {/if}
          </div>
          {#if entry.description}
            <p class="mt-1 text-[11px] text-[var(--fp-dim)] leading-relaxed">{entry.description}</p>
          {/if}
        </div>

        <div class="md:col-span-6 min-w-0">
          {#if entry.kind === 'bool'}
            <label class="inline-flex items-center gap-2 cursor-pointer select-none">
              <input
                type="checkbox"
                class="fp-checkbox"
                checked={formValues[entry.key] === 'true'}
                onchange={(e) => onField(entry.key, e.currentTarget.checked ? 'true' : 'false')}
                aria-label={entry.key}
              />
              <span class="text-xs text-[var(--fp-muted)]">
                {formValues[entry.key] === 'true' ? $tr('enabled') : $tr('disabled')}
              </span>
            </label>
          {:else if entry.kind === 'select'}
            <select
              class="fp-input text-[13px] py-2"
              value={formValues[entry.key]}
              onchange={(e) => onField(entry.key, e.currentTarget.value)}
              aria-label={entry.key}
            >
              {#if entry.enum && !entry.enum.includes(formValues[entry.key])}
                <option value={formValues[entry.key]}>{formValues[entry.key]}</option>
              {/if}
              {#each entry.enum ?? [] as opt}
                <option value={opt}>{opt}</option>
              {/each}
            </select>
          {:else if entry.kind === 'int'}
            <input
              type="number"
              step="1"
              class="fp-input fp-num text-[13px] py-2"
              value={formValues[entry.key]}
              oninput={(e) => onField(entry.key, e.currentTarget.value)}
              aria-label={entry.key}
            />
          {:else if entry.kind === 'secret'}
            <div class="flex items-center gap-2">
              <input
                type={reveal[entry.key] ? 'text' : 'password'}
                class="fp-input fp-mono text-[12px] py-2"
                value={formValues[entry.key]}
                oninput={(e) => onField(entry.key, e.currentTarget.value)}
                aria-label={entry.key}
                autocomplete="off"
              />
              <button
                type="button"
                class="fp-btn fp-btn-ghost fp-btn-sm shrink-0"
                onclick={() => onToggleReveal(entry.key)}
                aria-label={reveal[entry.key] ? $tr('Hide {key}', { key: entry.key }) : $tr('Reveal {key}', { key: entry.key })}
                title={reveal[entry.key] ? $tr('Hide') : $tr('Reveal')}
              >
                {#if reveal[entry.key]}
                  <EyeOff size={15} />
                {:else}
                  <Eye size={15} />
                {/if}
              </button>
            </div>
          {:else if entry.kind === 'list'}
            <div>
              <textarea
                class="fp-input fp-mono text-[12px] py-2 min-h-[56px]"
                rows="2"
                value={formValues[entry.key]}
                oninput={(e) => onField(entry.key, e.currentTarget.value)}
                aria-label={entry.key}
                spellcheck="false"
                placeholder="val1, val2, val3"
              ></textarea>
              <p class="mt-1 text-[10px] text-[var(--fp-dim)]">{$tr('Comma-separated values.')}</p>
            </div>
          {:else}
            <input
              type="text"
              class="fp-input fp-mono text-[12px] py-2"
              value={formValues[entry.key]}
              oninput={(e) => onField(entry.key, e.currentTarget.value)}
              aria-label={entry.key}
              spellcheck="false"
              placeholder={entry.default || $tr('unset — default applies')}
            />
          {/if}
        </div>
      </div>
    {/each}
  </div>
  {#if !group.isExpanded && group.advanced.length > 0}
    <div class="pt-3 border-t border-[var(--fp-border)]/60 flex flex-col sm:flex-row sm:items-center justify-between gap-2">
      <button
        type="button"
        class="inline-flex items-center gap-1.5 text-xs text-[var(--fp-accent)] hover:underline font-medium"
        onclick={() => onToggleGroup(group.name)}
      >
        <ChevronDown size={14} />
        <span>{$tr('Show {count} more advanced settings in {group}', { count: group.advanced.length, group: groupLabel(group.name) })}</span>
      </button>
      <span class="text-[11px] text-[var(--fp-dim)] font-mono truncate max-w-xs">
        {group.advanced.slice(0, 3).map((e) => e.key).join(', ')}{group.advanced.length > 3 ? '…' : ''}
      </span>
    </div>
  {:else if group.isExpanded && group.advanced.length > 0}
    <div class="pt-3 border-t border-[var(--fp-border)]/60">
      <button
        type="button"
        class="inline-flex items-center gap-1.5 text-xs text-[var(--fp-dim)] hover:text-[var(--fp-text)] font-medium"
        onclick={() => onToggleGroup(group.name)}
      >
        <ChevronUp size={14} />
        <span>{$tr('Hide advanced settings')}</span>
      </button>
    </div>
  {/if}
</Card>
