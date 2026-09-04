<script>
  import { tick } from "svelte";
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
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi, adminRoot } from "../api/paths.js";
  import { usePolling } from "../utils/polling.js";
  import { formatTime, parseLogFields } from "../utils/format.js";
  import { SvelteSet, SvelteMap } from "svelte/reactivity";
  import { copyToClipboard } from "../utils/clipboard.js";
  import { confirmAction } from "../stores/confirm.js";
  import { tr } from "../i18n.js";
  /** @type {any} */
  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let manualRefresh = $state(false);
  let viewMode = $state("console"); // 'table' | 'console' — console (inference /v1 traffic) is the default (issue #322 follow-up)
  let clearedBefore = $state(0);
  let copiedConsole = $state(false);
  let consoleEl = $state(null);
  let filterLevel = $state("");
  let filterMsg = $state("");
  let hideAdmin = $state(true);
  let autoPoll = $state(true);
  let page = $state(0);
  let pageSize = $state(10);
  // Follow mode: stick the console to the newest entry at the bottom.
  // Any manual scroll-up pauses it so reading history never gets yanked;
  // scrolling back to the bottom (or the Follow toggle) resumes it.
  let autoScroll = $state(true);
  const CIRCLE_EMOJIS = ["🟣", "🔵", "🟢", "🟡", "🟠", "🔴", "🟤", "⚪"];
  function circleFor(str) {
    if (!str) return "🟢";
    let h = 0;
    for (let i = 0; i < str.length; i++) h = (h * 31 + str.charCodeAt(i)) >>> 0;
    return CIRCLE_EMOJIS[h % CIRCLE_EMOJIS.length];
  }

  // Console is the specialized inference-traffic view: ONLY /v1 lines.
  // Console request groups: one card per /v1 request (keyed by req_id),
  // merging the backend's per-request cluster (request → routing → access →
  // trace → done, or request failed / refused / throttled). The old flat
  // line list rendered up to THREE near-identical REQ rows per request and a
  // context-free DONE row, and TTFT never appeared (only "chat trace"
  // carries upstream_ttfb_ms). Only access lines carry path, so pipeline
  // messages match exactly and count as inherently /v1 — the access line is
  // written AFTER the handler returns, so mid-stream an anchor lookup would
  // hide live lines. Playground traffic (req_id of an admin access in the
  // same batch) stays hidden: console is /v1 only.
  const CONSOLE_REQ = new Set([
    "chat request",
    "messages request",
    "responses request",
    "chat routing",
    "messages routing",
    "responses routing",
    "chat request refused",
    "messages request refused",
    "responses request refused",
  ]);
  const CONSOLE_DONE = new Set([
    "chat done",
    "messages done",
    "responses done",
    "chat trace",
  ]);
  const endpointOfKind = (msg) =>
    msg.startsWith("messages")
      ? "messages"
      : msg.startsWith("responses")
        ? "responses"
        : "chat";
  const endpointOfPath = (path) => {
    if (path.includes("/messages/count_tokens")) return "count_tokens";
    const seg = String(path).split("/")[2] || "";
    if (seg === "messages") return "messages";
    if (seg === "responses") return "responses";
    if (seg === "chat") return "chat";
    return seg || "unknown";
  };
  const parseStream = (v) =>
    v === "true" || v === "1" || v === true
      ? true
      : v === "false" || v === "0" || v === false
        ? false
        : null;
  let requestGroups = $derived.by(() => {
    const raw = data?.entries || [];
    // req_ids minted for admin-path accesses (playground, login, …) in this
    // batch — pipeline lines carrying one are admin traffic, not /v1.
    const adminReqIds = new SvelteSet();
    for (let i = 0; i < raw.length; i++) {
      const e = raw[i];
      if ((e.message || "") !== "access") continue;
      const parsed = parseLogFields(e.fields);
      let path = "";
      let reqId = "";
      for (let j = 0; j < parsed.length; j++) {
        if (parsed[j].key === "path") path = String(parsed[j].value || "");
        if (parsed[j].key === "req_id") reqId = parsed[j].value;
      }
      if (path.includes(adminRoot) && reqId) adminReqIds.add(reqId);
    }
    const chrono = [...raw].reverse();
    const byKey = new SvelteMap();
    const order = [];
    const get = (key, timeStr, eTime, circleKey) => {
      let g = byKey.get(key);
      if (!g) {
        g = {
          key,
          time: timeStr,
          eTime,
          circle: circleFor(circleKey),
          endpoint: "unknown",
          model: "",
          agent: "",
          servedModel: "",
          fallback: "",
          stream: null,
          msgs: 0,
          tools: 0,
          effort: "",
          token: "",
          instanceId: "",
          refused: false,
          reason: "",
          until: "",
          failed: false,
          status: "",
          code: "",
          errText: "",
          retryAfter: "",
          throttled: false,
          done: false,
          ms: "",
          bytes: "",
          chunks: "",
          traceStatus: "",
          ttft: "",
          attempts: 0,
          retried: false,
          statusesSeen: "",
          accessStatus: "",
          accessMs: "",
        };
        byKey.set(key, g);
        order.push(g);
      }
      return g;
    };
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
      const reqId = fields.req_id || "";
      const pathStr = String(fields.path || "");
      const msg = e.message || "";
      // Playground/admin pipeline traffic stays out of the /v1 console.
      if (reqId && adminReqIds.has(reqId)) continue;
      // Stable group key without tracking the window index (an index-based
      // key reshuffles every poll as the ring slides).
      const gkey = reqId
        ? "id-" + reqId
        : "solo-" + (fields.model || msg) + "-" + (e.time || i);
      if (CONSOLE_REQ.has(msg)) {
        const g = get(
          gkey,
          timeStr,
          e.time,
          reqId || fields.token || fields.model,
        );
        if (g.endpoint === "unknown") g.endpoint = endpointOfKind(msg);
        if (!g.model && fields.model) g.model = fields.model;
        if (!g.agent && fields.agent) g.agent = fields.agent;
        if (!g.servedModel && fields.served_model)
          g.servedModel = fields.served_model;
        if (!g.fallback && fields.fallback) g.fallback = fields.fallback;
        if (g.stream === null) g.stream = parseStream(fields.stream);
        if (!g.msgs && Number(fields.msgs)) g.msgs = Number(fields.msgs);
        if (!g.tools && Number(fields.tools)) g.tools = Number(fields.tools);
        if (!g.effort && fields.reasoning_effort)
          g.effort = fields.reasoning_effort;
        if (!g.token && fields.token) g.token = fields.token;
        if (!g.instanceId && fields.instance_id)
          g.instanceId = fields.instance_id;
        if (msg.endsWith("request refused")) {
          g.refused = true;
          g.reason = fields.reason || "refused";
          g.until = fields.until || "";
        }
      } else if (msg === "access" && pathStr.includes("/v1/")) {
        const status = Number(fields.status) || 0;
        if (fields.method === "POST" || status >= 400) {
          const g = get(
            gkey,
            timeStr,
            e.time,
            reqId || fields.token || fields.model,
          );
          if (g.endpoint === "unknown") g.endpoint = endpointOfPath(pathStr);
          if (!g.accessStatus && status) {
            g.accessStatus = String(status);
            g.accessMs = fields.ms || "";
          }
        }
      } else if (CONSOLE_DONE.has(msg)) {
        const g = get(
          gkey,
          timeStr,
          e.time,
          reqId || fields.token || fields.model,
        );
        if (g.endpoint === "unknown" && msg !== "chat trace")
          g.endpoint = endpointOfKind(msg);
        if (!g.model && fields.model) g.model = fields.model;
        if (!g.agent && fields.agent) g.agent = fields.agent;
        if (!g.token && fields.token) g.token = fields.token;
        if (msg === "chat trace") {
          g.traceStatus = fields.status || g.traceStatus;
          if (!g.ttft && fields.upstream_ttfb_ms)
            g.ttft = fields.upstream_ttfb_ms;
          if (!g.attempts && Number(fields.attempts))
            g.attempts = Number(fields.attempts);
          if (fields.retried === "true" || fields.retried === "1")
            g.retried = true;
          if (!g.statusesSeen && fields.statuses_seen)
            g.statusesSeen = fields.statuses_seen;
          if (!g.ms && (fields.total_ms || fields.ms))
            g.ms = fields.total_ms || fields.ms;
          if (g.traceStatus === "error" && !g.errText && fields.error)
            g.errText = fields.error;
        } else {
          g.done = true;
          if (!g.ms && fields.ms) g.ms = fields.ms;
          if (!g.bytes && fields.bytes) g.bytes = fields.bytes;
          if (!g.chunks && fields.chunks) g.chunks = fields.chunks;
          if (g.stream === null) g.stream = parseStream(fields.stream);
          if (!g.effort && fields.reasoning_effort)
            g.effort = fields.reasoning_effort;
        }
      } else if (msg === "request failed") {
        const g = get(
          gkey,
          timeStr,
          e.time,
          reqId || fields.token || fields.model,
        );
        g.failed = true;
        if (!g.status && fields.status) g.status = String(fields.status);
        if (!g.code && fields.code) g.code = fields.code;
        if (!g.errText)
          g.errText =
            fields.err ||
            fields.reason ||
            fields.error ||
            fields.message ||
            (fields.code ? `[${fields.code}]` : "") ||
            "request failed";
        if (!g.retryAfter && fields.retry_after)
          g.retryAfter = String(fields.retry_after);
        if (!g.model && fields.model) g.model = fields.model;
        if (!g.token && fields.token) g.token = fields.token;
      } else if (msg === "rate limit exceeded") {
        const g = get(
          gkey,
          timeStr,
          e.time,
          reqId || fields.token || fields.model,
        );
        // Per-IP throttle: rejected before access, so no access line exists.
        g.throttled = true;
        if (!g.status) g.status = "429";
        if (!g.retryAfter && fields.retry_after_sec)
          g.retryAfter = String(fields.retry_after_sec);
        if (!g.errText) g.errText = "client rate limit exceeded";
      }
    }
    // Finalize: outcome, stable id, copy text, chips.
    const usedIds = new SvelteSet();
    for (const g of order) {
      const accessFail = g.accessStatus && Number(g.accessStatus) >= 400;
      const traceFail = g.traceStatus === "error";
      g.outcome =
        g.failed || accessFail || traceFail
          ? "error"
          : g.refused
            ? "refused"
            : g.throttled
              ? "throttled"
              : g.done || g.traceStatus === "ok" || g.accessStatus
                ? "ok"
                : "live";
      if (!g.ms) g.ms = g.accessMs || "";
      if (!g.status)
        g.status = g.accessStatus || (g.outcome === "ok" ? "200" : "");
      const base =
        "g-" +
        (g.key.startsWith("id-") ? g.key.slice(3) : g.model + "-" + g.eTime);
      let id = base;
      let n = 1;
      while (usedIds.has(id)) {
        n += 1;
        id = base + "~" + n;
      }
      usedIds.add(id);
      g.id = id;
      const who = g.servedModel || g.agent;
      const head =
        g.outcome === "ok"
          ? `POST ${g.model || "chat"}${who ? ` → ${who}` : ""} · ${g.status} · ${g.ms}ms`
          : g.outcome === "live"
            ? `POST ${g.model || "chat"}${who ? ` → ${who}` : ""} · LIVE`
            : g.outcome === "refused"
              ? `REFUSED ${g.model || ""} · ${g.reason}`.trim()
              : g.outcome === "throttled"
                ? `THROTTLED · ${g.errText}${g.retryAfter ? ` · RETRY ${g.retryAfter}s` : ""}`
                : `ERROR ${g.status}${g.code ? ` ${g.code}` : ""} · ${g.errText}${g.retryAfter ? ` · RETRY ${g.retryAfter}s` : ""}`;
      g.text = `[${g.time}] ${head}`;
      g.chips = [
        ...(g.endpoint && g.endpoint !== "unknown" ? [g.endpoint] : []),
        ...(g.stream === true
          ? ["STREAM"]
          : g.stream === false
            ? ["SYNC"]
            : []),
        ...(g.msgs > 0 ? [`${g.msgs} MSG`] : []),
        ...(g.tools > 0 ? [`${g.tools} TOOL`] : []),
        ...(g.effort ? [`THINK ${g.effort}`] : []),
        ...(g.token ? [`ACC ${g.token}`] : []),
        ...(g.fallback ? [`FALLBACK ${g.fallback}`] : []),
        ...(g.ttft ? [`TTFT ${g.ttft}ms`] : []),
        ...(g.bytes ? [`${g.bytes}B`] : []),
        ...(g.chunks ? [`${g.chunks} CHUNKS`] : []),
        ...(g.attempts > 1 ? [`×${g.attempts}`] : []),
        ...(g.retried ? ["RETRIED"] : []),
      ];
    }
    return order;
  });

  // Stick to the tail only while new lines arrive AND the user was already
  // near the bottom — otherwise every 1s poll yanked the scroll away from
  // the history being read (felt like the console "updated itself").
  let prevConsoleCount = $state(0);
  $effect(() => {
    const n = requestGroups.length;
    if (consoleEl && n > prevConsoleCount) {
      const el = consoleEl;
      const gap = el.scrollHeight - el.scrollTop - el.clientHeight;
      if (gap < 120) el.scrollTop = el.scrollHeight;
    }
    prevConsoleCount = n;
  });
  function groupChipClass(chip) {
    if (chip === "STREAM")
      return "border-green-500/40 bg-green-500/10 text-green-300";
    if (chip === "SYNC") return "border-zinc-600 bg-zinc-800/80 text-zinc-300";
    if (chip === "RETRIED" || chip.startsWith("×") || chip.startsWith("TTFT"))
      return "border-amber-500/30 bg-amber-500/10 text-amber-200";
    return "border-zinc-700/80 bg-zinc-900 text-zinc-300";
  }
  async function copyConsoleLogs() {
    const text = requestGroups.map((g) => g.text).join("\n");
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
  // Content-stable row keys: index keys made every 1s poll swap row contents
  // in place (the table looked like it "updated itself"); identical rows in
  // one page get a collision suffix, same as the console uid scheme.
  let keyedPagedEntries = $derived.by(() => {
    const seen = new SvelteSet();
    return pagedEntries.map((e) => {
      const base =
        (e.time || "") +
        "|" +
        (e.level || "") +
        "|" +
        (e.message || "") +
        "|" +
        (e.fields || "");
      let k = base;
      let n = 1;
      while (seen.has(k)) {
        n += 1;
        k = base + "~" + n;
      }
      seen.add(k);
      return { k, e };
    });
  });
  let totalPages = $derived.by(() =>
    Math.max(1, Math.ceil(filteredEntries.length / pageSize)),
  );
  let hasActiveFilter = $derived.by(
    () => filterLevel !== "" || filterMsg.trim() !== "" || !hideAdmin,
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
      // Table-only filters: the console is an unfiltered /v1 view — sending
      // stale table filters while it is visible emptied it with no visible
      // cause ("no request log" although traffic existed).
      if (viewMode === "table") {
        if (filterLevel) query.set("level", filterLevel);
        if (filterMsg.trim()) query.set("msg", filterMsg.trim());
      }
      const res = await fetchAPI(`${adminApi.logs}?${query.toString()}`);
      data = res;
      error = "";
      // No page reset here: the clamp effect above keeps the pager in range
      // without yanking the table back to page 0 on every 1s poll.
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
    filterLevel = "";
    filterMsg = "";
    hideAdmin = true;
    handleFilterChange();
  }

  // Auto-poll every 1s while enabled; manual refresh / filter changes always fetch.
  usePolling(async () => {
    if (autoPoll) await fetchLogs();
  }, 1000);

  function isNearBottom() {
    if (!consoleEl) return true;
    return (
      consoleEl.scrollHeight - consoleEl.scrollTop - consoleEl.clientHeight < 64
    );
  }

  function handleConsoleScroll() {
    autoScroll = isNearBottom();
  }

  function toggleAutoScroll() {
    autoScroll = !autoScroll;
    if (autoScroll && consoleEl) consoleEl.scrollTop = consoleEl.scrollHeight;
  }

  // Stick to the newest entry whenever the group list grows while
  // follow mode is on. tick() waits for Svelte to flush the new rows so
  // scrollHeight already includes them.
  $effect(() => {
    const n = requestGroups.length;
    const last = n ? requestGroups[n - 1].id : "";
    if (viewMode !== "console" || !autoScroll || !consoleEl) return;
    void last;
    tick().then(() => {
      if (consoleEl && autoScroll) consoleEl.scrollTop = consoleEl.scrollHeight;
    });
  });
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
      <SegmentedControl
        bind:value={viewMode}
        options={[
          { id: "console", label: $tr("Console") },
          { id: "table", label: $tr("Table") },
        ]}
        onchange={() => fetchLogs()}
      />
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
    {#if viewMode === "console"}
      <Card pad="none">
        <!-- Console View Top Bar: stacks on mobile so 4 actions never overflow -->
        <div
          class="p-2.5 sm:p-3 bg-[var(--fp-surface)] border-b border-[var(--fp-border)] flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between"
        >
          <div class="flex items-center gap-2 min-w-0">
            <span
              class="led {requestGroups.length > 0
                ? 'led-good'
                : 'led-idle'} shrink-0"
            ></span>
            <span class="font-mono text-xs text-[var(--fp-muted)] truncate"
              >{requestGroups.length}
              {requestGroups.length === 1 ? "request" : "requests"} · /v1 only</span
            >
          </div>
          <div
            class="flex flex-wrap items-center gap-1.5 sm:gap-2 sm:justify-end"
          >
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
              class="!h-8 !text-xs !px-2 sm:!px-2.5 text-[var(--fp-dim)] hover:text-[var(--fp-error)]"
            >
              <Trash2 size={13} />
              <span class="hidden min-[480px]:inline">{$tr("Clear")}</span>
            </Button>
            <Button
              variant="ghost"
              size="sm"
              aria-pressed={autoScroll}
              onclick={toggleAutoScroll}
              class="!h-8 !text-xs !px-2 sm:!px-2.5"
              title={autoScroll
                ? $tr("Following the newest logs")
                : $tr(
                    "Follow paused — scroll to the bottom or toggle to resume",
                  )}
            >
              {$tr("Follow {state}", {
                state: autoScroll ? $tr("on") : $tr("off"),
              })}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onclick={copyConsoleLogs}
              class="!h-8 !text-xs !px-2 sm:!px-2.5"
            >
              {#if copiedConsole}
                <Check size={13} class="text-[var(--fp-success)]" />
                <span class="text-[var(--fp-success)]">{$tr("Copied")}</span>
              {:else}
                <Copy size={13} />
                <span class="hidden min-[480px]:inline">{$tr("Copy")}</span>
              {/if}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              aria-pressed={autoPoll}
              onclick={() => (autoPoll = !autoPoll)}
              class="!h-8 !text-xs !px-2 sm:!px-2.5"
              title={autoPoll
                ? $tr("Auto-refreshing every 1s")
                : $tr("Auto-refresh paused")}
            >
              {$tr("Auto {state}", {
                state: autoPoll ? "1s" : $tr("off"),
              })}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              loading={manualRefresh}
              onclick={refresh}
              disabled={loading && !data}
              class="!h-8 !text-xs !px-2 sm:!px-2.5"
            >
              <RefreshCw size={13} />
              <span class="hidden min-[480px]:inline">{$tr("Refresh")}</span>
            </Button>
          </div>
        </div>
        <!-- Terminal Console View: structured rows wrap (never truncate, never break-all) -->
        <div
          bind:this={consoleEl}
          onscroll={handleConsoleScroll}
          class="bg-black rounded-b-lg p-2 sm:p-4 font-mono text-[11px] sm:text-xs h-[60vh] min-h-[320px] sm:h-[calc(100vh-280px)] sm:min-h-[420px] overflow-x-hidden overflow-y-auto space-y-1.5 select-text border-t border-[var(--fp-border)] max-w-full"
        >
          {#if requestGroups.length === 0}
            <div
              class="h-full flex flex-col items-center justify-center text-zinc-500 italic py-16 px-4 text-center"
            >
              <p>{$tr("No request activity recorded yet.")}</p>
              <p class="text-[11px] mt-1 text-zinc-600">
                {$tr(
                  "Live incoming chat and messages requests will stream here.",
                )}
              </p>
            </div>
          {:else}
            {#each requestGroups as g (g.id)}
              <div
                class="hover:bg-zinc-900/70 px-1.5 py-1 rounded transition-colors leading-relaxed min-w-0 overflow-hidden"
              >
                <div class="break-words">
                  <span class="text-zinc-500">[{g.time}]</span>
                  <span class="text-zinc-300">{g.circle} </span>
                  {#if g.outcome === "ok"}
                    <span class="text-green-400 font-medium"
                      >POST {g.model || g.endpoint}</span
                    >
                    {#if g.servedModel || g.agent}<span class="text-zinc-400">
                        → {g.servedModel || g.agent}</span
                      >{/if}
                    <span class="text-zinc-400">
                      · {g.status}{#if g.ms}
                        · {g.ms}ms{/if}</span
                    >
                  {:else if g.outcome === "live"}
                    <span class="text-sky-300 font-medium animate-pulse"
                      >POST {g.model || g.endpoint} · LIVE</span
                    >
                    {#if g.servedModel || g.agent}<span class="text-zinc-400">
                        → {g.servedModel || g.agent}</span
                      >{/if}
                  {:else if g.outcome === "refused"}
                    <span class="text-amber-300 font-medium"
                      >REFUSED {g.model}</span
                    >
                    <span class="text-amber-200/80"> · {g.reason}</span>
                  {:else if g.outcome === "throttled"}
                    <span class="text-red-400 font-medium">THROTTLED</span>
                    <span class="text-red-300/90">
                      · {g.errText}{#if g.retryAfter}
                        · RETRY {g.retryAfter}s{/if}</span
                    >
                  {:else}
                    <span class="text-red-400 font-medium"
                      >ERROR{#if g.status}
                        {g.status}{/if}{#if g.code}
                        {g.code}{/if}</span
                    >
                    <span class="text-red-300/90"> · {g.errText}</span>
                  {/if}
                </div>
                {#if g.outcome === "error" && (g.retryAfter || g.statusesSeen)}
                  <div class="break-words text-red-300/70">
                    {#if g.retryAfter}<span>RETRY {g.retryAfter}s</span>{/if}
                    {#if g.statusesSeen}<span>
                        · tried {g.statusesSeen}</span
                      >{/if}
                  </div>
                {/if}
                {#if g.chips.length > 0}
                  <div class="flex flex-wrap gap-1 mt-1">
                    {#each g.chips as chip, j (j)}
                      <span
                        class="inline-flex items-center rounded border px-1.5 py-px text-[10px] leading-4 whitespace-nowrap {groupChipClass(
                          chip,
                        )}">{chip}</span
                      >
                    {/each}
                  </div>
                {/if}
              </div>
            {/each}
          {/if}
        </div>
      </Card>
    {:else}
      <Card pad="none">
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
              title={autoPoll
                ? $tr("Auto-refreshing every 1s")
                : $tr("Auto-refresh paused")}
            >
              {$tr("Auto {state}", {
                state: autoPoll ? "1s" : $tr("off"),
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
              {#each keyedPagedEntries as row (row.k)}
                {@const e = row.e}
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
                      {#each fields as f, j (j)}
                        <span
                          class="font-mono text-[11px] text-[var(--fp-muted)] min-w-0 break-words"
                        >
                          <span class="text-[var(--fp-dim)]">{f.key}</span
                          >=<span class="break-words">{f.value}</span>
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
  {/if}
</div>
