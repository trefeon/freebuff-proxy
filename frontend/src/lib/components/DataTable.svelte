<script>
  import { ChevronDown } from "@lucide/svelte";
  import { tr } from "../i18n.js";

  /**
   * DataTable — hairline table with right-aligned mono numerics (DESIGN.md
   * table grammar) and an optional per-row expand slot.
   *
   * @prop {Array<{ key: string, label: string, numeric?: boolean }>} [columns=[]]
   * @prop {Array<Record<string, any>>} [rows=[]]
   * @prop {string} [rowKey='id'] — row field used as key + expand identity
   * @prop {(row: Record<string, any>) => any} [expanded] — snippet rendering the expand detail for one row
   */
  let { columns = [], rows = [], rowKey = "id", expanded } = $props();

  let expandedKey = $state(null);
  const spanAll = $derived(columns.length + (expanded ? 1 : 0));

  function toggle(key) {
    expandedKey = expandedKey === key ? null : key;
  }
</script>

<div class="fp-card overflow-x-auto">
  <table class="w-full text-sm">
    <thead>
      <tr class="border-b border-[var(--fp-border)]">
        {#if expanded}
          <th scope="col" class="w-10 px-2 py-2.5">
            <span class="sr-only">{$tr("Details")}</span>
          </th>
        {/if}
        {#each columns as col (col.key)}
          <th
            scope="col"
            class="px-4 py-2.5 font-mono text-xs font-medium uppercase tracking-wide text-[var(--fp-muted)] {col.numeric
              ? 'text-right'
              : 'text-left'}"
          >
            {col.label}
          </th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#each rows as row (row[rowKey])}
        {@const key = row[rowKey]}
        {@const open = expandedKey === key}
        <tr
          class="border-b border-[var(--fp-border)] last:border-0 hover:bg-[var(--fp-surface-2)]"
        >
          {#if expanded}
            <td class="px-2 py-3">
              <button
                type="button"
                aria-expanded={open}
                aria-label={$tr("Toggle details")}
                onclick={() => toggle(key)}
                class="rounded-[var(--fp-radius-sm)] p-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)] focus-visible:outline-2 focus-visible:outline-[var(--fp-accent)]"
              >
                <ChevronDown
                  size={16}
                  class="transition-transform {open ? 'rotate-180' : ''}"
                />
              </button>
            </td>
          {/if}
          {#each columns as col (col.key)}
            <td class="px-4 py-3 {col.numeric ? 'text-right fp-num' : ''}">
              {row[col.key] ?? "—"}
            </td>
          {/each}
        </tr>
        {#if expanded && open}
          <tr class="border-b border-[var(--fp-border)] bg-[var(--fp-inset)]">
            <td colspan={spanAll} class="px-4 py-3">
              {@render expanded(row)}
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
</div>
