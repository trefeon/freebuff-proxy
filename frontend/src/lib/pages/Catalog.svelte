<script>
  import { onMount } from "svelte";
  import PageShell from "../components/PageShell.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import ModelsPanel from "../components/ModelsPanel.svelte";
  import AllowancesPanel from "../components/AllowancesPanel.svelte";
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";

  let tab = $state("models");

  onMount(() => {
    recordPageVisit("models");
    try {
      const pending = sessionStorage.getItem("fp-page-tab:catalog");
      sessionStorage.removeItem("fp-page-tab:catalog");
      if (pending === "models" || pending === "allowances") tab = pending;
    } catch {
      // storage unavailable — stay on the default tab
    }
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / catalog.conf"
  title={$tr("Catalog")}
  description={$tr(
    "Served models, serving accounts, and Freebucks allowances.",
  )}
>
  <div class="flex flex-wrap items-center gap-2">
    <SegmentedControl
      bind:value={tab}
      options={[
        { id: "models", label: $tr("Models") },
        { id: "allowances", label: $tr("Allowances") },
      ]}
      ariaLabel={$tr("Catalog section")}
    />
  </div>

  <div class="flex flex-col gap-5">
    {#if tab === "models"}
      <ModelsPanel />
    {:else}
      <AllowancesPanel />
    {/if}
  </div>
</PageShell>
