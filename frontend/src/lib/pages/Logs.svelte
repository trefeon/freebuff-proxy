<script>
  import {
    RefreshCw,
    ChevronLeft,
    ChevronRight,
    Search,
    EyeOff,
    Trash2,
    Copy,
    Check,
  } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Card from "../components/Card.svelte";
  import Button from "../components/Button.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import CopyButton from "../components/CopyButton.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi, adminRoot } from "../api/paths.js";
  import { usePolling } from "../utils/polling.js";
  import { formatTime, parseLogFields } from "../utils/format.js";
  import { copyToClipboard } from "../utils/clipboard.js";
  import { confirmAction } from "../stores/confirm.js";
  import { tr } from "../i18n.js";
  /** @type {any} */
  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let manualRefresh = $state(false);
  let viewMode = $state("table"); // 'table' | 'console'
  let clearedBefore = $state(0);
  let copiedConsole = $state(false);
  let consoleEl = $state(null);
  let filterLevel = $state("");
  let filterMsg = $state("");
  let hideAdmin = $state(true);
  let autoPoll = $state(true);
  let page = $state(0);
  let pageSize = $state(10);

  const CIRCLE_EMOJIS = ["🟣", "🔵", "🟢", "🟡", "🟠", "🔴", "🟤", "⚪"];
  function circleFor(str) {
    if (!str) return "🟢";
    let h = 0;
    for (let i = 0; i < str.length; i++) h = (h * 31 + str.charCodeAt(i)) >>> 0;
    return CIRCLE_EMOJIS[h % CIRCLE_EMOJIS.length];
  }

  let requestLogs = $derived.by(() => {
    const raw = data?.entries || [];
    const chrono = [...raw].reverse();
    const lines = [];

    for (let i = 0; i < chrono.length; i++) {
      const e = chrono[i];
      const entryTime = new Date(e.time).getTime();
      if (clearedBefore && entryTime <= clearedBefore) continue;

      const timeStr = formatTime(e.time);
      const fields = {};
      const parsed = parseLogFields(e.fields);
      for (let j = 0; j < parsed.length; j++) {
        fields[parsed[j].key] = parsed[j].value;
      }

      const reqId = fields.req_id || fields.client_request_id || "";
      const circle = circleFor(reqId || fields.token || fields.model);
      const msg = e.message || "";

      if (
        msg.includes("request") ||
        msg.includes("routing") ||
        (msg === "access" &&
          fields.method === "POST" &&
          String(fields.path || "").includes("/v1/"))
      ) {
        const model = fields.model || "chat";
        const agent = fields.served_model
          ? `→ ${fields.served_model}`
          : fields.agent
            ? `→ ${fields.agent}`
            : "";
        const pathStr = String(fields.path || "");
        const fmt = pathStr.includes("/messages")
          ? "anthropic→openai"
          : "openai→openai";
        const stream =
          fields.stream === "true" || fields.stream === "1" ? "STREAM" : "SYNC";
        const effort = fields.reasoning_effort
          ? ` · THINK:${fields.reasoning_effort}`
          : "";
        const acc = fields.token ? ` · ACC:${fields.token}` : "";
        const lineText =
          `[${timeStr}] ${circle} ▶ POST ${model} ${agent} · FMT: ${fmt} · ${stream}${effort}${acc}`.replace(
            /\s+/g,
            " ",
          );
        lines.push({
          id: "req-" + (reqId || i) + "-" + e.time,
          text: lineText,
          type: "req",
        });
      } else if (msg.includes("done") || msg === "chat trace") {
        const ms = fields.ms || fields.total_ms || "0";
        const ttft = fields.upstream_ttfb_ms || fields.ttft_ms || "";
        const ttftPart = ttft ? ` · TTFT ${ttft}ms` : "";
        const inTok =
          fields.prompt_tokens ||
          fields.input_tokens ||
          fields.tokens ||
          fields.usage ||
          "";
        const inPart = inTok ? ` · IN ${inTok}` : "";
        const cacheTok = fields.cache_read_tokens || "";
        const cachePart = cacheTok ? ` (CACHE ↻${cacheTok})` : "";
        const outTok =
          fields.completion_tokens ||
          fields.output_tokens ||
          fields.chunks ||
          "";
        const outPart = outTok ? ` · OUT ${outTok}` : "";
        lines.push({
          id: "done-" + (reqId || i) + "-" + e.time,
          text: `[${timeStr}] ${circle} 📊 DONE ${ms}ms${ttftPart}${inPart}${cachePart}${outPart}`,
          type: "done",
        });
      } else if (
        msg.includes("failed") ||
        (msg === "access" && Number(fields.status) >= 400)
      ) {
        const status = fields.status || "";
        const err =
          fields.reason || fields.error || fields.message || "request failed";
        const ms = fields.ms ? ` · ${fields.ms}ms` : "";
        lines.push({
          id: "err-" + (reqId || i) + "-" + e.time,
          text: `[${timeStr}] ${circle} ✗ ERROR ${status} · ${err}${ms}`,
          type: "err",
        });
      }
    }
    return lines;
  });

  $effect(() => {
    if (consoleEl && requestLogs.length > 0) {
      consoleEl.scrollTop = consoleEl.scrollHeight;
    }
  });

  async function copyConsoleLogs() {
    const text = requestLogs.map((l) => l.text).join("\n");
    if (!text) return;
    const ok = await copyToClipboard(text);
    if (ok) {
      copiedConsole = true;
      setTimeout(() => (copiedConsole = false), 2000);
    }
  }
  let entries = $derived.by(() => data?.entries || []);
  let filteredEntries = $derived.by(() => {
    if (!hideAdmin) return entries;
    return entries.filter((e) => {
      const fields = parseLogFields(e.fields);
      return !fields.some(
        (f) => f.key === "path" && String(f.value).includes(adminRoot),
      );
    });
  });
  let pagedEntries = $derived.by(() => {
    const start = page * pageSize;
    return filteredEntries.slice(start, start + pageSize);
  });
  let totalPages = $derived.by(() =>
    Math.max(1, Math.ceil(filteredEntries.length / pageSize)),
  );
  let hasActiveFilter = $derived.by(
    () => filterLevel !== "info" || filterMsg.trim() !== "" || !hideAdmin,
  );
  let rangeStart = $derived.by(() =>
    filteredEntries.length === 0 ? 0 : page * pageSize + 1,
  );
  let rangeEnd = $derived.by(() =>
    Math.min((page + 1) * pageSize, filteredEntries.length),
  );
  $effect(() => {
    if (page >= totalPages) page = Math.max(0, totalPages - 1);
  });
  async function fetchLogs() {
    try {
      // eslint-disable-next-line svelte/prefer-svelte-reactivity -- transient local query builder, not reactive state
      const query = new URLSearchParams();
      if (filterLevel) query.set("level", filterLevel);
      if (filterMsg.trim()) query.set("msg", filterMsg.trim());
      const res = await fetchAPI(`${adminApi.logs}?${query.toString()}`);
      data = res;
      error = "";
      const tp = Math.ceil((res?.entries?.length || 0) / pageSize);
      if (page > tp - 1) page = 0;
    } catch (e) {
      error = e.message
        ? $tr("Could not load log entries: {reason}", { reason: e.message })
        : $tr("Could not load log entries");
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
    filterLevel = "info";
    filterMsg = "";
    hideAdmin = true;
    handleFilterChange();
  }

  // Auto-poll every 5s while enabled; manual refresh / filter changes always fetch.
  usePolling(async () => {
    if (autoPoll) await fetchLogs();
  }, 5000);

  function levelTone(level) {
    switch (level) {
      case "error":
        return "bad";
      case "warn":
        return "warn";
      case "info":
        return "info";
      default:
        return "idle";
    }
  }
</script>

<div class="space-y-4 page-enter">
  <PageHeader
    title={$tr("Logs")}
    description={$tr(
      "Live proxy and request logs from the in-memory ring buffer (200 max, newest first).",
    )}
  >
    {#snippet actions()}
      <div
        class="flex items-center gap-1 bg-[var(--fp-surface-2)] p-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)]"
      >
        <button
          type="button"
          class="px-2.5 py-1 text-xs font-mono rounded transition-colors {viewMode ===
          'console'
            ? 'bg-[var(--fp-surface)] text-[var(--fp-accent)] font-semibold shadow-sm'
            : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
          onclick={() => (viewMode = "console")}
        >
          {$tr("Console")}
        </button>
        <button
          type="button"
          class="px-2.5 py-1 text-xs font-mono rounded transition-colors {viewMode ===
          'table'
            ? 'bg-[var(--fp-surface)] text-[var(--fp-accent)] font-semibold shadow-sm'
            : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)]'}"
          onclick={() => (viewMode = "table")}
        >
          {$tr("Table")}
        </button>
      </div>
    {/snippet}
  </PageHeader>

  {#if error}
    <Alert tone="error" title={$tr("Could not load log entries")}>
      <p class="text-sm">{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={refresh}
          >{$tr("Retry")}</Button
        >
      </div>
    </Alert>
  {/if}

  {#if loading && !data}
    <div aria-live="polite" aria-busy="true">
      <span class="sr-only">{$tr("Loading logs…")}</span>
      <Card pad="none">
        <div class="p-4 space-y-3" aria-hidden="true">
          {#each [0, 1, 2, 3, 4, 5, 6] as i (i)}
            <div class="flex items-center gap-3">
              <span class="skeleton rounded-full size-2 shrink-0"></span>
              <span
                class="skeleton skeleton-line"
                style="width:{45 + (i % 4) * 12}%"
              ></span>
              <span class="skeleton skeleton-line ml-auto" style="width:15%"
              ></span>
            </div>
          {/each}
        </div>
      </Card>
    </div>
  {:else if data && !data.enabled}
    <EmptyState
      title={$tr("Log ring disabled")}
      description={$tr(
        "The server was started without an active logring handler, so no log entries are available.",
      )}
    />
  {:else if data}
    <Card pad="none">
      {#if viewMode === "console"}
        <!-- Console View Top Bar -->
        <div
          class="p-3 bg-[var(--fp-surface)] border-b border-[var(--fp-border)] flex items-center justify-between gap-2.5"
        >
          <div class="flex items-center gap-2">
            <span class="led {requestLogs.length > 0 ? 'led-good' : 'led-idle'}"
            ></span>
            <span class="font-mono text-xs text-[var(--fp-muted)]"
              >{requestLogs.length} {$tr("request events")}</span
            >
          </div>
          <div class="flex items-center gap-2">
            <Button
              variant="ghost"
              size="sm"
              onclick={async () => {
                const ok = await confirmAction({
                  title: $tr("Clear Request Console"),
                  message: $tr(
                    "Are you sure you want to clear the request console logs? This will clear all current events from your view.",
                  ),
                  confirmText: $tr("Clear"),
                  tone: "warn",
                });
                if (ok) {
                  clearedBefore = Date.now();
                }
              }}
              class="!h-8 !text-xs !px-2.5 text-[var(--fp-dim)] hover:text-[var(--fp-error)]"
            >
              <Trash2 size={13} />
              <span>{$tr("Clear")}</span>
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onclick={copyConsoleLogs}
              class="!h-8 !text-xs !px-2.5"
            >
              {#if copiedConsole}
                <Check size={13} class="text-[var(--fp-success)]" />
                <span class="text-[var(--fp-success)]">{$tr("Copied")}</span>
              {:else}
                <Copy size={13} />
                <span>{$tr("Copy")}</span>
              {/if}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              aria-pressed={autoPoll}
              onclick={() => (autoPoll = !autoPoll)}
              class="!h-8 !text-xs !px-2.5"
            >
              {$tr("Auto {state}", {
                state: autoPoll ? $tr("on") : $tr("off"),
              })}
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
              <span>{$tr("Refresh")}</span>
            </Button>
          </div>
        </div>
        <!-- Terminal Console View -->
        <div
          bind:this={consoleEl}
          class="bg-black rounded-b-lg p-4 font-mono text-xs h-[calc(100vh-280px)] min-h-[420px] overflow-y-auto space-y-1 select-text border-t border-[var(--fp-border)]"
        >
          {#if requestLogs.length === 0}
            <div
              class="h-full flex flex-col items-center justify-center text-zinc-500 italic py-16"
            >
              <p>{$tr("No request activity recorded yet.")}</p>
              <p class="text-[11px] mt-1 text-zinc-600">
                {$tr(
                  "Live incoming chat and messages requests will stream here.",
                )}
              </p>
            </div>
          {:else}
            {#each requestLogs as line (line.id)}
              <div
                class="hover:bg-zinc-900/70 px-1 py-0.5 rounded transition-colors leading-relaxed break-all"
              >
                <span
                  class={line.type === "err"
                    ? "text-red-400 font-medium"
                    : line.type === "done"
                      ? "text-amber-300"
                      : "text-green-400"}
                >
                  {line.text}
                </span>
              </div>
            {/each}
          {/if}
        </div>
      {:else}
        <!-- Integrated Top Toolbar Header for Table -->
        <div
          class="p-3 bg-[var(--fp-surface)] border-b border-[var(--fp-border)] flex flex-col sm:flex-row sm:items-center justify-between gap-2.5"
        >
          <!-- Filter Controls (Left Group) -->
          <div class="flex flex-wrap items-center gap-2 flex-1">
            <!-- Level Select -->
            <label for="log-level" class="sr-only">{$tr("Log level")}</label>
            <select
              id="log-level"
              class="fp-input !text-xs !py-1 !pl-2.5 !h-8 !w-auto !inline-block"
              bind:value={filterLevel}
              onchange={handleFilterChange}
            >
              <option value="">{$tr("All levels")}</option>
              <option value="debug">{$tr("Debug")}</option>
              <option value="info">{$tr("Info")}</option>
              <option value="warn">{$tr("Warn")}</option>
              <option value="error">{$tr("Error")}</option>
            </select>

            <!-- Search Input with Search Icon -->
            <div class="relative flex-1 min-w-[180px] max-w-xs">
              <Search
                size={13}
                class="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] pointer-events-none"
              />
              <label for="log-msg" class="sr-only"
                >{$tr("Filter by message")}</label
              >
              <input
                id="log-msg"
                type="text"
                class="fp-input !text-xs !pl-8 !pr-2.5 !py-1 !h-8 !w-full"
                bind:value={filterMsg}
                oninput={handleFilterChange}
                placeholder={$tr("Filter message…")}
              />
            </div>

            <!-- Hide admin toggle -->
            <Button
              variant={hideAdmin ? "secondary" : "ghost"}
              size="sm"
              aria-pressed={hideAdmin}
              onclick={() => {
                hideAdmin = !hideAdmin;
                page = 0;
              }}
              class="!h-8 !text-xs !px-2.5 shrink-0"
            >
              <EyeOff size={13} />
              <span>{$tr("Hide admin")}</span>
            </Button>

            {#if hasActiveFilter}
              <Button
                variant="ghost"
                size="sm"
                onclick={clearFilters}
                class="!h-8 !text-xs !px-2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] shrink-0"
              >
                {$tr("Clear filters")}
              </Button>
            {/if}
          </div>

          <!-- Live Controls (Right Group) -->
          <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
            <span
              class="inline-flex items-center gap-1.5 text-xs font-mono text-[var(--fp-muted)] mr-1"
            >
              <span
                class="led {filteredEntries.length > 0
                  ? 'led-good'
                  : 'led-idle'}"
              ></span>
              <span>{filteredEntries.length} {$tr("entries")}</span>
            </span>

            <Button
              variant="ghost"
              size="sm"
              aria-pressed={autoPoll}
              onclick={() => (autoPoll = !autoPoll)}
              class="!h-8 !text-xs !px-2.5"
            >
              {$tr("Auto {state}", {
                state: autoPoll ? $tr("on") : $tr("off"),
              })}
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
              <span>{$tr("Refresh")}</span>
            </Button>
          </div>
        </div>

        {#if filteredEntries.length === 0}
          <div class="p-8">
            <EmptyState
              title={$tr("No matching log entries")}
              description={hasActiveFilter
                ? $tr("No log entries matched your level or message filter.")
                : $tr(
                    "The log ring is empty — entries will appear here as the proxy logs activity.",
                  )}
            >
              {#if hasActiveFilter}
                <!-- eslint-disable-next-line no-unused-vars -- snippet passed implicitly as EmptyState's action prop (rendered via {@render action()}) -->
                {#snippet action()}
                  <Button variant="secondary" size="sm" onclick={clearFilters}
                    >{$tr("Clear filters")}</Button
                  >
                {/snippet}
              {/if}
            </EmptyState>
          </div>
        {:else}
          <div class="fp-inset m-3.5 overflow-x-auto">
            <ul class="divide-y divide-[var(--fp-border)]">
              {#each pagedEntries as e, i (i)}
                {@const fields = parseLogFields(e.fields)}
                {@const entryJson = JSON.stringify(
                  {
                    time: e.time,
                    level: e.level,
                    message: e.message,
                    fields: e.fields || "",
                  },
                  null,
                  2,
                )}
                <li
                  class="px-4 py-2.5 hover:bg-[var(--fp-surface-2)] transition-colors"
                >
                  <div class="flex items-center gap-3">
                    <StatusBadge status={e.level} tone={levelTone(e.level)} />
                    <span class="fp-num text-xs text-[var(--fp-dim)] shrink-0"
                      >{formatTime(e.time)}</span
                    >
                    <span
                      class="font-mono text-sm text-[var(--fp-text)] min-w-0 flex-1 truncate"
                      >{e.message}</span
                    >
                    <span class="shrink-0">
                      <CopyButton text={entryJson} label="Copy" />
                    </span>
                  </div>
                  {#if fields.length > 0}
                    <div class="mt-1.5 flex flex-wrap gap-x-4 gap-y-1 pl-1">
                      {#each fields as f (f.key)}
                        <span
                          class="font-mono text-[11px] text-[var(--fp-muted)]"
                        >
                          <span class="text-[var(--fp-dim)]">{f.key}</span
                          >=<span>{f.value}</span>
                        </span>
                      {/each}
                    </div>
                  {/if}
                </li>
              {/each}
            </ul>
          </div>
        {/if}
      {/if}
      {#snippet footer()}
        {#if viewMode === "table"}
          <div
            class="flex flex-col sm:flex-row sm:items-center justify-between gap-2.5 px-4 py-3"
          >
            <div class="flex items-center gap-3">
              <span class="fp-num text-xs text-[var(--fp-muted)]">
                {rangeStart}–{rangeEnd} of {filteredEntries.length}
              </span>
              <label
                class="inline-flex items-center gap-1.5 text-xs text-[var(--fp-muted)]"
                for="logs-page-size"
              >
                {$tr("Rows per page")}
                <select
                  id="logs-page-size"
                  class="fp-input !h-8 !w-auto min-w-[4.25rem] !py-1 !pl-2.5 text-xs"
                  value={pageSize}
                  onchange={(e) => {
                    pageSize = Number(e.currentTarget.value);
                    page = 0;
                  }}
                >
                  <option value={10}>10</option>
                  <option value={50}>50</option>
                  <option value={100}>100</option>
                </select>
              </label>
            </div>
            <div class="flex items-center gap-2">
              <Button
                variant="secondary"
                size="sm"
                class="fp-num"
                disabled={page === 0}
                onclick={() => page--}
              >
                <ChevronLeft size={14} />
                {$tr("Prev")}
              </Button>
              <span
                class="fp-num text-xs text-[var(--fp-muted)] whitespace-nowrap"
              >
                {$tr("Page {current} / {total}", {
                  current: page + 1,
                  total: totalPages,
                })}
              </span>
              <Button
                variant="secondary"
                size="sm"
                class="fp-num"
                disabled={page >= totalPages - 1}
                onclick={() => page++}
              >
                <ChevronRight size={14} />
                {$tr("Next")}
              </Button>
            </div>
          </div>
        {/if}
      {/snippet}
    </Card>
  {/if}
</div>
