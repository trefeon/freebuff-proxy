<script>
  import Card from "./Card.svelte";
  import Stat from "./Stat.svelte";

  /**
   * KpiGrid — responsive instrument readout row. 2-up on mobile (DESIGN.md
   * layout grammar), 3-up on wide screens.
   *
   * @prop {Array<{ label: string, value: string|number, hint?: string, tone?: 'default'|'good'|'warn'|'bad' }>} [items=[]]
   */
  let { items = [] } = $props();
</script>

<div class="grid grid-cols-2 gap-4 xl:grid-cols-3">
  {#each items as item (item.label)}
    <Card>
      <Stat
        label={item.label}
        value={item.value}
        hint={item.hint}
        tone={item.tone ?? "default"}
      />
    </Card>
  {/each}
</div>
