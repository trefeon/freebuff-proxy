<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import Alert from "./Alert.svelte";
  import Button from "./Button.svelte";
  import EmptyState from "./EmptyState.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { formatTime } from "../utils/format.js";

  let {
    cursor = 0,
    focusReqId = "",
    onOpenToken = null,
    onOpenLogs = null,
  } = $props();

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  // Local focus dismissal: the parent sets focusReqId (log→trace link); the
  // "Clear" chip below resets to the full list without round-tripping.
  let clearedFocus = $state(false);
  let effectiveFocus = $derived(focusReqId && !clearedFocus ? focusReqId : "");

  // A newly arrived focus always re-arms, even after a previous Clear.
  $effect(() => {
    void focusReqId;
    clearedFocus = false;
  });

  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.traces);
      error = "";
    } catch (e) {
      error = e.message || $tr("Failed to load traces");
    } finally {
      loading = false;
    }
  }

  onMount(fetchData);

  // Shared time cursor from the Activity page ("Refresh all"): refetch when
  // it advances. fetchData reads no reactive state, so cursor is the only
  // dependency.
  $effect(() => {
    if (cursor) fetchData();
  });

  const rowReqId = (t) => t.req_id ?? t.reqId ?? "";
  let rowsHaveReqId = $derived((data?.traces || []).some((t) => rowReqId(t)));
  let visibleTraces = $derived.by(() => {
    const all = data?.traces || [];
    if (!effectiveFocus || !rowsHaveReqId) return all;
    return all.filter((t) => String(rowReqId(t)) === String(effectiveFocus));
  });
  // Fallback highlight when rows carry no req_id: match token/time instead.
  function highlightRow(t) {
    if (!effectiveFocus || rowsHaveReqId) return false;
    return (
      String(t.token ?? "") === String(effectiveFocus) ||
      String(t.time ?? "").includes(String(effectiveFocus))
    );
  }

  function accIndex(tok) {
    const t = String(tok ?? "").trim();
    return /^\d+$/.test(t) ? Number(t) : null;
  }
</script>

<div class="space-y-6">
  {#if loading}
    <div class="space-y-6" aria-busy="true">
      <div class="skeleton skeleton-card h-64"></div>
      <span class="sr-only">{$tr("Loading traces")}</span>
    </div>
  {:else if error}
    <Alert tone="error" title={$tr("Could not load this page")}>
      {error}
      <div class="mt-3">
        <Button variant="secondary" onclick={fetchData}>
          <RefreshCw size={15} />
          {$tr("Retry")}
        </Button>
      </div>
    </Alert>
  {:else if data?.enabled}
    {#if effectiveFocus}
      <div class="flex flex-wrap items-center gap-2">
        <span
          class="inline-flex items-center rounded border border-[var(--fp-accent)]/30 bg-[var(--fp-accent-dim)] px-2 py-1 font-mono text-xs text-[var(--fp-accent)]"
        >
          {$tr("Filtered to {id}", { id: effectiveFocus })}
        </span>
        <Button
          variant="ghost"
          size="sm"
          onclick={() => (clearedFocus = true)}
          class="!h-7 !text-xs"
        >
          {$tr("Clear")}
        </Button>
      </div>
    {/if}
    <Card
      title={$tr("Trace log")}
      description={$tr("Last 200 chat traces from the in-memory log ring.")}
      pad="none"
    >
      {#if visibleTraces?.length}
        <div class="overflow-x-auto hidden md:block">
          <table class="fp-table w-full min-w-[640px]">
            <caption class="sr-only"
              >{$tr(
                "Chat traces — time, token, model, status, latency and phases",
              )}</caption
            >
            <thead>
              <tr>
                <th scope="col">{$tr("Time")}</th>
                <th scope="col">{$tr("Token")}</th>
                <th scope="col">{$tr("Model")}</th>
                <th scope="col">{$tr("Status")}</th>
                <th scope="col" class="num">{$tr("Latency")}</th>
                <th scope="col">{$tr("Phases")}</th>
                <th scope="col">{$tr("Error")}</th>
                <th scope="col"><span class="sr-only">{$tr("Links")}</span></th>
              </tr>
            </thead>
            <tbody>
              {#each visibleTraces as t (t.time)}
                {@const tidx = accIndex(t.token)}
                {@const reqId = rowReqId(t)}
                <tr class={highlightRow(t) ? "bg-amber-500/5" : ""}>
                  <td
                    class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                    >{formatTime(t.time)}</td
                  >
                  <td>
                    {#if tidx !== null}
                      <button
                        type="button"
                        onclick={() => onOpenToken?.(tidx)}
                        title={$tr("Open token {idx}", { idx: tidx })}
                        class="fp-num font-mono text-xs text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0"
                        >#{t.token}</button
                      >
                    {:else}
                      <span class="fp-num font-mono text-xs">#{t.token}</span>
                    {/if}
                  </td>
                  <td class="font-mono text-[11px]">{t.model || "—"}</td>
                  <td>
                    <span
                      class={t.status === "error"
                        ? "text-[var(--fp-error)] font-semibold"
                        : "text-[var(--fp-success)]"}
                    >
                      {t.status || "ok"}
                    </span>
                  </td>
                  <td class="num">{t.ms ? t.ms : "—"}</td>
                  <td>
                    {#if t.phases?.length}
                      <div class="flex flex-wrap gap-1">
                        {#each t.phases as ph (ph.name)}
                          <span
                            class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]"
                          >
                            {ph.name}
                            {ph.ms}ms
                          </span>
                        {/each}
                      </div>
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td
                    class="text-[var(--fp-error)] text-[11px] max-w-[200px] truncate"
                    >{t.error || ""}</td
                  >
                  <td>
                    <button
                      type="button"
                      onclick={() => onOpenLogs?.(reqId ? String(reqId) : "")}
                      class="font-mono text-[11px] text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 whitespace-nowrap"
                      >{$tr("Logs")}</button
                    >
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        <ul
          class="md:hidden flex flex-col gap-2.5 p-3.5"
          aria-label={$tr("Chat traces")}
        >
          {#each visibleTraces as t (t.time)}
            {@const tidx = accIndex(t.token)}
            {@const reqId = rowReqId(t)}
            <li class="fp-inset rounded p-3 flex flex-col gap-2 min-w-0">
              <div class="flex items-center justify-between gap-2 min-w-0">
                <span
                  class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                  >{formatTime(t.time)}</span
                >
                <span
                  class={t.status === "error"
                    ? "text-[var(--fp-error)] font-semibold text-xs"
                    : "text-[var(--fp-success)] text-xs"}
                >
                  {t.status || "ok"}
                </span>
              </div>
              <div class="flex items-center gap-1.5 min-w-0 text-xs">
                {#if tidx !== null}
                  <button
                    type="button"
                    onclick={() => onOpenToken?.(tidx)}
                    title={$tr("Open token {idx}", { idx: tidx })}
                    class="fp-num font-mono text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 shrink-0"
                    >#{t.token}</button
                  >
                {:else}
                  <span class="fp-num font-mono shrink-0">#{t.token}</span>
                {/if}
                <code
                  class="fp-num truncate min-w-0 text-[11px] text-[var(--fp-muted)]"
                  >{t.model || "—"}</code
                >
              </div>
              <div class="flex items-center justify-between gap-2 text-xs">
                <span class="fp-num text-[var(--fp-muted)]"
                  >{t.ms ? t.ms : "—"}</span
                >
                <button
                  type="button"
                  onclick={() => onOpenLogs?.(reqId ? String(reqId) : "")}
                  class="font-mono text-[11px] text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 whitespace-nowrap"
                  >{$tr("Logs")}</button
                >
              </div>
              {#if t.phases?.length}
                <div class="flex flex-wrap gap-1">
                  {#each t.phases as ph (ph.name)}
                    <span
                      class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]"
                    >
                      {ph.name}
                      {ph.ms}ms
                    </span>
                  {/each}
                </div>
              {/if}
              {#if t.error}
                <p class="text-[var(--fp-error)] text-[11px] break-words">
                  {t.error}
                </p>
              {/if}
            </li>
          {/each}
        </ul>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No traces recorded yet.")}
          </p>
        </div>
      {/if}
    </Card>
  {:else if data}
    <EmptyState
      title={$tr("Traces disabled")}
      description={$tr(
        "Trace collection is off — no log ring is wired to this dashboard.",
      )}
    />
  {/if}
</div>
