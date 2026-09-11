<script>
  import { onMount } from "svelte";
  import PageShell from "../components/PageShell.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import ModelsPanel from "../components/ModelsPanel.svelte";
  import AllowancesPanel from "../components/AllowancesPanel.svelte";
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";

  let tab = $state("accounts");

  onMount(() => {
    recordPageVisit("models");
    try {
      const pending = sessionStorage.getItem("fp-page-tab:catalog");
      sessionStorage.removeItem("fp-page-tab:catalog");
      if (pending === "models" || pending === "accounts") tab = pending;
    } catch {
      // storage unavailable — stay on the default tab
    }
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / catalog.conf"
  title={$tr("Catalog")}
  description={$tr("Serving accounts and served models.")}
>
  <div class="flex flex-wrap items-center gap-2">
    <SegmentedControl
      bind:value={tab}
      options={[
        { id: "accounts", label: $tr("Accounts") },
        { id: "models", label: $tr("Models") },
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
