<script>
  import { Send, Activity } from "@lucide/svelte";
  import PageHeader from "../components/PageHeader.svelte";
  import Card from "../components/Card.svelte";
  import Button from "../components/Button.svelte";
  import Alert from "../components/Alert.svelte";
  import CopyButton from "../components/CopyButton.svelte";
  import SessionSpawnPanel from "../components/SessionSpawnPanel.svelte";
  import BatchTestPanel from "../components/BatchTestPanel.svelte";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi, adminActions, tokenActions } from "../api/paths.js";
  import { fallbackModelOptions, fetchModelOptions } from "../modelOptions.js";
  import { spawnIntent } from "../utils/freebucks.js";
  import { isDevToolsEnabled } from "../utils/devtools.js";
  import {
    tokensData as tokensStore,
    tokensError as tokensErrorStore,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import { confirmAction } from "../stores/confirm.js";
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";

  // Dev Tools is an operator-only manual testing surface (issue: dev testing
  // removed from public). Hidden unless DEVTOOLS_ENABLED=true in the proxy
  // config; the page self-checks so a direct #devtools hash can't bypass it.
  let devToolsEnabled = $state(false);

  // --- State for Chat Playground ---
  let selectedModel = $state("mimo/mimo-v2.5");
  let protocol = $state("openai"); // 'openai' | 'anthropic'
  let streamMode = $state(true);
  let promptText = $state(
    "Hello! Count from 1 to 5 and briefly describe yourself.",
  );
  let reasoningEffort = $state("medium");
  // Client API key for the /v1 playground + burst fetches below. Those hit
  // the public protocol surface (requireAuth), not the session-cookie admin
  // API, so a gateway with API_KEYS set answers 401 without one. Kept in
  // sessionStorage ("fp-devtools-client-key") — survives reloads within the
  // tab session, cleared with the tab; never localStorage, never the server.
  let clientKey = $state("");
  try {
    clientKey = sessionStorage.getItem("fp-devtools-client-key") ?? "";
  } catch {
    // Storage unavailable (private mode) — stay memory-only.
  }
  $effect(() => {
    try {
      sessionStorage.setItem("fp-devtools-client-key", clientKey);
    } catch {
      // Storage unavailable — the key simply won't survive reload.
    }
  });
  const authHeaders = () =>
    clientKey.trim() ? { Authorization: `Bearer ${clientKey.trim()}` } : {};
  // "auto" = pure pool rotation; otherwise admit the session on the chosen
  // pooled account first (same endpoint as the spawner below), then send.
  let playAccount = $state("auto");
  // Per-account spawner model picks for the mobile card list.
  let spawnModelByIdx = $state({});
  let sendingChat = $state(false);
  let chatOutput = $state("");
  let chatStatus = $state(null); // { latencyMs, statusCode, tokens: { prompt, completion, total }, model }
  let chatError = $state("");

  // --- State for Session Spawner & Tokens ---
  let tokensData = $state(null);
  let loadingTokens = $state(true);
  let actionMessage = $state("");
  let actionOK = $state(true);
  let actionPending = $state(false);

  let modelsList = $state(fallbackModelOptions);
  onMount(() => {
    // The visit is recorded only once the DEVTOOLS gate passes: a direct
    // #devtools hash with the gate off must not plant a lastHash/visit that
    // reopens this page on the next boot.
    // One shared tokens store owns the /admin/api/tokens poll + SSE (issue
    // #292); this page just renders the cached snapshot.
    const release = ensureTokensStore();
    const unsubStore = tokensStore.subscribe((v) => {
      if (v) {
        tokensData = v;
        loadingTokens = false;
      }
    });
    const unsubErr = tokensErrorStore.subscribe((err) => {
      if (err) loadingTokens = false;
    });
    fetchModelOptions().then((rows) => (modelsList = rows));
    (async () => {
      try {
        const cfgRes = await fetchAPI(adminApi.config);
        const envContent = cfgRes?.env_content || "";
        devToolsEnabled = isDevToolsEnabled(envContent);
        if (devToolsEnabled) recordPageVisit("devtools");
      } catch {
        devToolsEnabled = false;
      }
    })();
    return () => {
      release();
      unsubStore();
      unsubErr();
    };
  });

  async function sendPlaygroundChat() {
    if (sendingChat || !promptText.trim()) return;
    sendingChat = true;
    chatOutput = "";
    chatError = "";
    chatStatus = null;
    const start = performance.now();
    // Pinned account: admit the session there first via the spawner
    // endpoint, then send through normal rotation.
    if (playAccount !== "auto") {
      try {
        const adm = await postAPI(tokenActions.session(Number(playAccount)), {
          model: selectedModel,
        });
        if (adm && adm.ok === false)
          throw new Error(adm.message || "Admit rejected");
      } catch (e) {
        chatError = `Admit on account ${Number(playAccount) + 1} failed: ${e.message || e}`;
        sendingChat = false;
        await refreshTokens();
        return;
      }
    }

    try {
      if (protocol === "openai") {
        const payload = {
          model: selectedModel,
          messages: [{ role: "user", content: promptText }],
          stream: streamMode,
        };
        if (reasoningEffort && reasoningEffort !== "default") {
          payload.reasoning_effort = reasoningEffort;
        }

        const res = await fetch("/v1/chat/completions", {
          method: "POST",
          headers: { "Content-Type": "application/json", ...authHeaders() },
          body: JSON.stringify(payload),
        });

        const latency = Math.round(performance.now() - start);

        if (!res.ok) {
          const errText = await res.text();
          chatError = `HTTP ${res.status}: ${errText}`;
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            model: selectedModel,
          };
          return;
        }

        if (streamMode && res.body) {
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            model: selectedModel,
          };
          const reader = res.body.getReader();
          const decoder = new TextDecoder();
          let buf = "";

          while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += decoder.decode(value, { stream: true });
            const lines = buf.split("\n");
            buf = lines.pop() || "";

            for (const line of lines) {
              const trimmed = line.trim();
              if (trimmed.startsWith("data: ")) {
                const dataStr = trimmed.slice(6);
                if (dataStr === "[DONE]") continue;
                try {
                  const chunk = JSON.parse(dataStr);
                  const delta = chunk.choices?.[0]?.delta?.content || "";
                  chatOutput += delta;
                } catch {}
              }
            }
          }
        } else {
          const json = await res.json();
          chatOutput =
            json.choices?.[0]?.message?.content ||
            JSON.stringify(json, null, 2);
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            tokens: json.usage,
            model: selectedModel,
          };
        }
      } else {
        // Anthropic protocol (/v1/messages)
        const payload = {
          model: selectedModel,
          messages: [{ role: "user", content: promptText }],
          max_tokens: 1024,
          stream: streamMode,
        };

        const res = await fetch("/v1/messages", {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "anthropic-version": "2023-06-01",
            ...authHeaders(),
          },
          body: JSON.stringify(payload),
        });

        const latency = Math.round(performance.now() - start);

        if (!res.ok) {
          const errText = await res.text();
          chatError = `HTTP ${res.status}: ${errText}`;
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            model: selectedModel,
          };
          return;
        }

        if (streamMode && res.body) {
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            model: selectedModel,
          };
          const reader = res.body.getReader();
          const decoder = new TextDecoder();
          let buf = "";

          while (true) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += decoder.decode(value, { stream: true });
            const lines = buf.split("\n");
            buf = lines.pop() || "";

            for (const line of lines) {
              const trimmed = line.trim();
              if (trimmed.startsWith("data: ")) {
                try {
                  const evt = JSON.parse(trimmed.slice(6));
                  if (
                    evt.type === "content_block_delta" &&
                    evt.delta?.type === "text_delta"
                  ) {
                    chatOutput += evt.delta.text || "";
                  }
                } catch {}
              }
            }
          }
        } else {
          const json = await res.json();
          const textBlock = json.content?.find((c) => c.type === "text");
          chatOutput = textBlock?.text || JSON.stringify(json, null, 2);
          chatStatus = {
            latencyMs: latency,
            statusCode: res.status,
            tokens: json.usage,
            model: selectedModel,
          };
        }
      }
    } catch (e) {
      chatError = e.message || "Request failed";
      chatStatus = {
        latencyMs: Math.round(performance.now() - start),
        statusCode: 0,
        model: selectedModel,
      };
    } finally {
      sendingChat = false;
      refreshTokens();
    }
  }

  // Deep-link the latest playground run into the Activity Live console.
  // The one-shot filter carries the run's most selective captured text (no
  // request id is captured in this flow, so the run's model id); the hash
  // routes to the Activity page, whose LiveConsole consumes the key once.
  function viewRunInActivity() {
    try {
      sessionStorage.setItem(
        "fp-activity-log-filter",
        chatStatus?.model ?? selectedModel,
      );
    } catch {
      // Storage unavailable — navigation still lands on Activity unfiltered.
    }
    window.location.hash = "activity";
  }

  async function triggerTokenAction(url, body, confirmMsg) {
    if (confirmMsg) {
      const ok = await confirmAction({
        title: $tr("Confirm Action"),
        message: confirmMsg,
        confirmText: $tr("Confirm"),
        tone: "warn",
      });
      if (!ok) return;
    }
    actionPending = true;
    actionMessage = "";
    try {
      const res = await postAPI(url, body);
      if (Array.isArray(res)) {
        // Probe-all answers one JSON array with a per-token outcome each
        // (backend/internal/server/admin_tokens.go): summarize it into the
        // single alert line instead of showing raw JSON.
        const okCount = res.filter((r) => r && r.ok).length;
        actionOK = res.length > 0 && okCount === res.length;
        const firstBad = res.find((r) => r && !r.ok);
        actionMessage =
          $tr("Probe complete: {ok}/{n} tokens OK", {
            ok: okCount,
            n: res.length,
          }) + (firstBad?.message ? ` — ${firstBad.message}` : "");
      } else {
        actionOK = res.ok;
        actionMessage =
          res.message ||
          (res.ok ? $tr("Action completed") : $tr("Action failed"));
      }
      await refreshTokens();
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || $tr("Action failed");
    } finally {
      actionPending = false;
    }
  }

  function handleSpawn({ ok, message }) {
    actionOK = ok;
    actionMessage = message;
    refreshTokens();
  }
</script>

{#if devToolsEnabled}
  <div class="space-y-6 page-enter">
    <PageHeader
      title={$tr("Dev Tools")}
      description={$tr(
        "Interactive model playground, session spawner, and gateway load simulation.",
      )}
    >
      {#snippet actions()}
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending || !tokensData?.token_count}
          onclick={() =>
            triggerTokenAction(
              adminActions.tokenTestAll,
              {},
              $tr("Probe all pool tokens against upstream?"),
            )}
        >
          <Activity size={14} />
          <span>{$tr("Probe All Tokens")}</span>
        </Button>
      {/snippet}
    </PageHeader>

    {#if actionMessage}
      <Alert tone={actionOK ? "success" : "error"} title={actionMessage} />
    {/if}

    <!-- Section 1: Live Chat Playground -->
    <section aria-label="Model Playground">
      <Card
        title={$tr("Model Chat & Stream Playground")}
        description={$tr(
          "Send live requests directly to the proxy to test streaming, reasoning, and latency. Account Auto follows pool rotation; picking an account admits the session there first.",
        )}
      >
        {#snippet actions()}
          <span
            class="text-xs font-mono text-[var(--fp-muted)] flex items-center gap-2"
          >
            <span>Protocol:</span>
            <span
              class="uppercase tracking-wider font-semibold text-[var(--fp-accent)]"
              >{protocol}</span
            >
          </span>
        {/snippet}

        <div class="space-y-4">
          <!-- Controls Bar -->
          <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-5 gap-3">
            <!-- Model Picker -->
            <div class="flex flex-col gap-1">
              <label for="dev-model" class="text-xs text-[var(--fp-muted)]"
                >{$tr("Model")}</label
              >
              <select
                id="dev-model"
                bind:value={selectedModel}
                class="fp-input text-xs w-full py-1.5"
              >
                {#each modelsList as m (m.id)}
                  <option value={m.id}>{m.label}</option>
                {/each}
              </select>
            </div>
            <!-- Account Picker -->
            <div class="flex flex-col gap-1">
              <label for="dev-account" class="text-xs text-[var(--fp-muted)]"
                >{$tr("Account")}</label
              >
              <select
                id="dev-account"
                bind:value={playAccount}
                class="fp-input text-xs w-full py-1.5"
              >
                <option value="auto">{$tr("Auto (pool rotation)")}</option>
                {#each tokensData?.tokens ?? [] as t (t.index)}
                  <option value={String(t.index ?? 0)}>
                    {$tr("Account #{idx}", { idx: (t.index ?? 0) + 1 })}{t.email
                      ? ` · ${t.email}`
                      : ""}
                  </option>
                {/each}
              </select>
            </div>

            <!-- Protocol Switch -->
            <div class="flex flex-col gap-1">
              <label for="dev-proto" class="text-xs text-[var(--fp-muted)]"
                >{$tr("API Surface")}</label
              >
              <select
                id="dev-proto"
                bind:value={protocol}
                class="fp-input text-xs w-full py-1.5"
              >
                <option value="openai">OpenAI (/v1/chat/completions)</option>
                <option value="anthropic">Anthropic (/v1/messages)</option>
              </select>
            </div>

            <!-- Stream Mode -->
            <div class="flex flex-col gap-1">
              <label for="dev-stream" class="text-xs text-[var(--fp-muted)]"
                >{$tr("Stream Mode")}</label
              >
              <select
                id="dev-stream"
                bind:value={streamMode}
                class="fp-input text-xs w-full py-1.5"
              >
                <option value={true}>{$tr("SSE Streaming (Real-time)")}</option>
                <option value={false}
                  >{$tr("JSON Response (Non-stream)")}</option
                >
              </select>
            </div>

            <!-- Reasoning Effort -->
            <div class="flex flex-col gap-1">
              <label for="dev-reasoning" class="text-xs text-[var(--fp-muted)]"
                >{$tr("Reasoning Effort")}</label
              >
              <select
                id="dev-reasoning"
                bind:value={reasoningEffort}
                class="fp-input text-xs w-full py-1.5"
              >
                <option value="default">Default</option>
                <option value="minimal">minimal</option>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
                <option value="xhigh">xhigh (max)</option>
              </select>
            </div>
          </div>
          <!-- Client key: the sends below hit /v1 (requireAuth), not the
            session-cookie admin API, so a gateway with API_KEYS set answers
            401 without one. Tab-session only (sessionStorage, never the server). -->
          <div class="flex flex-col gap-1.5">
            <label for="dev-client-key" class="text-xs text-[var(--fp-muted)]"
              >{$tr("Client API key")}</label
            >
            <input
              id="dev-client-key"
              type="password"
              bind:value={clientKey}
              autocomplete="off"
              spellcheck="false"
              class="fp-input fp-mono text-xs w-full p-2.5"
              placeholder="Bearer key for /v1 sends — leave empty when the gateway sets no API_KEYS"
            />
          </div>

          <!-- Prompt Textarea -->
          <div class="flex flex-col gap-1.5">
            <label for="dev-prompt" class="text-xs text-[var(--fp-muted)]"
              >{$tr("Prompt")}</label
            >
            <textarea
              id="dev-prompt"
              bind:value={promptText}
              rows="3"
              class="fp-input fp-mono text-xs w-full p-2.5 resize-y"
              placeholder="Type a test prompt…"></textarea>
          </div>

          <div class="flex items-center justify-between gap-3">
            <div
              class="flex items-center gap-3 text-xs font-mono text-[var(--fp-muted)]"
            >
              {#if chatStatus}
                <span class="inline-flex items-center gap-1.5">
                  <span
                    class="led {chatStatus.statusCode === 200
                      ? 'led-good'
                      : 'led-idle'}"
                  ></span>
                  <span
                    >{chatStatus.statusCode === 200
                      ? "OK 200"
                      : `Status ${chatStatus.statusCode}`}</span
                  >
                </span>
                <span class="text-[var(--fp-border-bright)]">|</span>
                <span>{chatStatus.latencyMs}ms</span>
                {#if chatStatus.tokens}
                  <span class="text-[var(--fp-border-bright)]">|</span>
                  <span
                    >{chatStatus.tokens.total_tokens ||
                      chatStatus.tokens.output_tokens ||
                      0} tokens</span
                  >
                {/if}
              {/if}
            </div>

            <div class="flex items-center gap-2 shrink-0">
              {#if chatStatus}
                <Button variant="ghost" size="sm" onclick={viewRunInActivity}>
                  <span>{$tr("View in Activity")}</span>
                </Button>
              {/if}
              <Button
                variant="primary"
                size="md"
                loading={sendingChat}
                disabled={sendingChat || !promptText.trim()}
                onclick={sendPlaygroundChat}
              >
                <Send size={14} />
                <span>{$tr("Send Request")}</span>
              </Button>
            </div>
            {#if chatError}
              <div
                role="alert"
                class="p-3 rounded bg-[var(--fp-error)]/10 border border-[var(--fp-error)]/30 text-xs text-[var(--fp-error)] font-mono"
              >
                {chatError}
              </div>
            {/if}

            <!-- Output Box -->
            {#if chatOutput}
              <div
                class="fp-inset rounded p-3.5 space-y-2 bg-[var(--fp-surface-2)]"
              >
                <div class="flex items-center justify-between">
                  <span
                    class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider"
                    >{$tr("Output")}</span
                  >
                  <CopyButton text={chatOutput} label="Copy Output" />
                </div>
                <pre
                  class="font-mono text-xs text-[var(--fp-text)] whitespace-pre-wrap break-all select-all leading-relaxed max-h-80 overflow-y-auto">{chatOutput}</pre>
              </div>
            {/if}
          </div>
        </div></Card
      >
    </section>

    <!-- Section 2: Per-Token Session Spawner & Lifecycle -->
    <section aria-label="Token Session Management">
      <Card
        title={$tr("Per-Token Session Spawner")}
        description={$tr(
          "Directly admit or spawn upstream sessions for specific models on individual tokens.",
        )}
      >
        {#if loadingTokens}
          <div class="p-4 space-y-3">
            <div class="skeleton skeleton-line"></div>
            <div class="skeleton skeleton-line"></div>
          </div>
        {:else if tokensData?.tokens?.length}
          <div class="hidden md:block overflow-x-auto">
            <table class="fp-table">
              <thead>
                <tr>
                  <th scope="col">Token</th>
                  <th scope="col">Status</th>
                  <th scope="col">Active Session</th>
                  <th scope="col">Select Model</th>
                  <th scope="col" class="text-right">Actions</th>
                </tr>
              </thead>
              <tbody>
                {#each tokensData.tokens as token (token.index)}
                  {@const idx = token.index}
                  <tr>
                    <td
                      ><span
                        class="fp-num text-xs font-bold text-[var(--fp-text)]"
                        >#{idx}</span
                      ></td
                    >
                    <td>
                      <span class="inline-flex items-center gap-1.5">
                        <span
                          class="led {token.session_status === 'active'
                            ? 'led-good'
                            : 'led-idle'}"
                        ></span>
                        <span
                          class="font-mono text-xs uppercase tracking-wider text-[var(--fp-muted)]"
                          >{token.session_status || "idle"}</span
                        >
                      </span>
                    </td>
                    <td>
                      {#if token.session_model}
                        <span
                          class="fp-num text-xs text-[var(--fp-accent)] font-semibold"
                          >{token.session_model}</span
                        >
                        {#if token.session_remaining_seconds > 0}
                          <span class="text-[11px] text-[var(--fp-dim)] block"
                            >{Math.floor(token.session_remaining_seconds / 60)}m
                            left</span
                          >
                        {/if}
                      {:else}
                        <span class="text-xs text-[var(--fp-dim)]">—</span>
                      {/if}
                    </td>
                    <SessionSpawnPanel {idx} {token} onSpawn={handleSpawn} />
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
          <ul
            class="md:hidden flex flex-col gap-2.5"
            aria-label="Token sessions"
          >
            {#each tokensData.tokens as token (token.index)}
              {@const idx = token.index ?? 0}
              {@const spawnSel = spawnModelByIdx[idx] ?? selectedModel}
              {@const spawnOpt = spawnIntent(token, spawnSel)}
              <li class="fp-inset rounded p-3 flex flex-col gap-2 min-w-0">
                <div class="flex items-center justify-between gap-2">
                  <span class="fp-num text-xs font-bold text-[var(--fp-text)]"
                    >{$tr("Account #{idx}", { idx: idx + 1 })}</span
                  >
                  <span class="inline-flex items-center gap-1.5">
                    <span
                      class="led {token.session_status === 'active'
                        ? 'led-good'
                        : 'led-idle'}"
                    ></span>
                    <span
                      class="font-mono text-[11px] uppercase tracking-wider text-[var(--fp-muted)]"
                      >{token.session_status || "idle"}</span
                    >
                  </span>
                </div>
                {#if token.session_model}
                  <p
                    class="fp-num text-xs text-[var(--fp-accent)] font-semibold truncate"
                  >
                    {token.session_model}{#if token.session_remaining_seconds > 0}
                      <span
                        class="text-[11px] text-[var(--fp-dim)] font-normal"
                      >
                        · {Math.floor(token.session_remaining_seconds / 60)}m
                        {$tr("left")}</span
                      >{/if}
                  </p>
                {/if}
                <select
                  class="fp-input text-xs w-full py-1.5"
                  value={spawnSel}
                  onchange={(e) => {
                    spawnModelByIdx[idx] = e.currentTarget.value;
                  }}
                  aria-label={$tr("Model for Account #{idx}", { idx: idx + 1 })}
                >
                  {#each modelsList as m (m.id)}
                    {@const opt = spawnIntent(token, m.id)}
                    <option
                      value={m.id}
                      disabled={opt.kind === "paywall"}
                      title={opt.kind === "paywall"
                        ? "Not enough Freebucks"
                        : m.label}
                      >{m.label}{opt.kind === "paywall"
                        ? " — paywalled"
                        : ""}</option
                    >
                  {/each}
                </select>
                {#if spawnOpt.kind === "paywall"}
                  <p class="text-[11px] text-[var(--fp-warning)]">
                    {$tr(
                      "Not enough Freebucks (price {price}, balance {balance})",
                      {
                        price: spawnOpt.price,
                        balance: token?.freebucks?.balance ?? 0,
                      },
                    )}
                  </p>
                {/if}
                <div class="flex flex-wrap gap-1.5">
                  <Button
                    variant="primary"
                    size="sm"
                    disabled={actionPending || spawnOpt.kind === "paywall"}
                    onclick={() =>
                      triggerTokenAction(
                        tokenActions.session(idx),
                        { model: spawnSel },
                        $tr(
                          "Spawn upstream session on account #{idx} for {model}?",
                          {
                            idx: idx + 1,
                            model: spawnSel,
                          },
                        ),
                      )}
                  >
                    {$tr("Make Session")}
                  </Button>
                  <Button
                    variant="secondary"
                    size="sm"
                    disabled={actionPending}
                    onclick={() =>
                      triggerTokenAction(
                        tokenActions.test(idx),
                        {},
                        $tr("Probe account #{idx}?", { idx: idx + 1 }),
                      )}
                  >
                    {$tr("Probe")}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={actionPending}
                    onclick={() =>
                      triggerTokenAction(
                        tokenActions.finish(idx),
                        {},
                        $tr("Finish runs on account #{idx}?", { idx: idx + 1 }),
                      )}
                  >
                    {$tr("Finish")}
                  </Button>
                </div>
              </li>
            {/each}
          </ul>
        {/if}
      </Card>
    </section>

    <!-- Section 3: Batch Traffic & Rotation Simulator -->
    <BatchTestPanel {clientKey} />
  </div>
{:else}
  <div class="space-y-6 page-enter">
    <PageHeader
      title={$tr("Dev Tools")}
      description={$tr("Manual testing surface")}
    />
    <div
      class="rounded-sm border border-[var(--fp-border)] p-6 text-sm text-[var(--fp-muted)]"
    >
      {$tr(
        "Dev Tools is disabled. Set DEVTOOLS_ENABLED=true in the proxy configuration to enable it.",
      )}
    </div>
  </div>
{/if}
