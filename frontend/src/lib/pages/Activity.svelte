<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import Button from "../components/Button.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import LiveConsole from "../components/LiveConsole.svelte";
  import MetricsPanel from "../components/MetricsPanel.svelte";
  import TracesPanel from "../components/TracesPanel.svelte";
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";

  let tab = $state("live");
  // Shared time cursor: "Refresh all" stamps it; every panel refetches when
  // it advances (each panel also fetches on its own mount).
  let cursor = $state(0);
  // Log→trace link target, set by LiveConsole's Trace buttons.
  let traceFocus = $state("");
  // Trace/metric→log link text, applied as LiveConsole's one-shot
  // initialFilter through a {#key} remount.
  let liveInitial = $state("");
  let liveKey = $state(0);

  function onOpenTrace(reqId, token) {
    void token;
    traceFocus = reqId ?? "";
    tab = "traces";
  }

  function onOpenLogs(text) {
    liveInitial = text ?? "";
    liveKey += 1;
    tab = "live";
  }

  function onOpenToken(idx) {
    try {
      sessionStorage.setItem("fp-tokens-expand", String(idx));
    } catch {
      // Storage unavailable (private mode) — the Tokens page just won't
      // auto-expand; navigation still works.
    }
    location.hash = "tokens";
  }

  onMount(() => {
    recordPageVisit("logs");
    // One-shot deep-link tab (set by the shell's legacy-hash redirect);
    // consumed on mount so back-navigation keeps the operator's own tab.
    try {
      const t = sessionStorage.getItem("fp-page-tab:activity") || "";
      sessionStorage.removeItem("fp-page-tab:activity");
      if (t === "live" || t === "metrics" || t === "traces") tab = t;
    } catch {
      // Storage unavailable — stay on the default Live tab.
    }
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / activity.conf"
  title={$tr("Activity")}
  description={$tr("Live traffic, metrics, and traces.")}
>
  {#snippet actions()}
    <div class="flex flex-wrap items-center gap-2">
      <SegmentedControl
        bind:value={tab}
        options={[
          { id: "live", label: $tr("Live") },
          { id: "metrics", label: $tr("Metrics") },
          { id: "traces", label: $tr("Traces") },
        ]}
        ariaLabel={$tr("Activity view")}
      />
      <Button
        variant="secondary"
        size="sm"
        onclick={() => (cursor = Date.now())}
        class="!h-8 !text-xs !px-2.5"
      >
        <RefreshCw size={13} />
        <span class="hidden min-[480px]:inline">{$tr("Refresh all")}</span>
      </Button>
    </div>
  {/snippet}

  {#if tab === "live"}
    {#key liveKey}
      <LiveConsole
        {cursor}
        initialFilter={liveInitial}
        {onOpenTrace}
        {onOpenToken}
      />
    {/key}
  {:else if tab === "metrics"}
    <MetricsPanel {cursor} {onOpenToken} {onOpenLogs} />
  {:else}
    <TracesPanel {cursor} focusReqId={traceFocus} {onOpenToken} {onOpenLogs} />
  {/if}
</PageShell>
