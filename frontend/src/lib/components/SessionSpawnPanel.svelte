<script>
  import { Zap, RefreshCw, Check } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import { postAPI } from "../api/client.js";
  import { tokenActions } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { confirmAction } from "../stores/confirm.js";
  import { fallbackModelOptions, fetchModelOptions } from "../modelOptions.js";
  import { onMount } from "svelte";
  import { spawnIntent, intentAskLine } from "../utils/freebucks.js";

  let { idx, token = null, onSpawn } = $props();

  let spawnModel = $state("mimo/mimo-v2.5");

  let intent = $derived(spawnIntent(token, spawnModel));

  let modelOptions = $state(fallbackModelOptions);
  onMount(() => {
    fetchModelOptions().then((rows) => (modelOptions = rows));
  });
  let actionPending = $state(false);

  async function triggerAction(action, body, confirmMsg) {
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
    try {
      const res = await postAPI(tokenActions[action](idx), body);
      onSpawn?.({
        ok: res.ok,
        message:
          res.message ||
          (res.ok ? $tr("Action completed") : $tr("Action failed")),
      });
    } catch (e) {
      onSpawn?.({ ok: false, message: e.message || $tr("Action failed") });
    } finally {
      actionPending = false;
    }
  }
</script>

<td>
  <select
    bind:value={spawnModel}
    class="fp-input !text-xs !py-1 !pl-2.5 !h-8 !w-48"
  >
    {#each modelOptions as m (m.id)}
      {@const opt = spawnIntent(token, m.id)}
      <option
        value={m.id}
        disabled={opt.kind === "paywall"}
        title={opt.kind === "paywall" ? "Not enough Freebucks" : m.label}
        >{m.label}{opt.kind === "paywall" ? " — paywalled" : ""}</option
      >
    {/each}
  </select>
  {#if intent.kind === "paywall"}
    <p class="text-[11px] text-[var(--fp-warning)] mt-1">
      {$tr("Not enough Freebucks (price {price}, balance {balance})", {
        price: intent.price,
        balance: token?.freebucks?.balance ?? 0,
      })}
    </p>
  {/if}
  {#if intent.kind === "confirm" && intentAskLine(intent, token?.session_model)}
    <p class="text-[11px] text-[var(--fp-muted)] mt-1">
      {intentAskLine(intent, token?.session_model)}
    </p>
  {/if}
</td>
<td class="text-right">
  <div class="inline-flex items-center gap-1.5 justify-end">
    <Button
      variant="primary"
      size="sm"
      disabled={actionPending || intent.kind === "paywall"}
      onclick={() =>
        triggerAction(
          "session",
          { model: spawnModel },
          $tr("Spawn upstream session on account #{idx} for {model}?", {
            idx: idx + 1,
            model: spawnModel,
          }) +
            (intent.kind === "confirm" &&
            intentAskLine(intent, token?.session_model)
              ? " " + intentAskLine(intent, token?.session_model)
              : ""),
        )}
    >
      <Zap size={13} />
      <span>{$tr("Make Session")}</span>
    </Button>
    <Button
      variant="secondary"
      size="sm"
      disabled={actionPending}
      onclick={() =>
        triggerAction(
          "test",
          {},
          $tr("Probe account #{idx}?", { idx: idx + 1 }),
        )}
    >
      <RefreshCw size={13} />
      <span>{$tr("Probe")}</span>
    </Button>
    <Button
      variant="ghost"
      size="sm"
      disabled={actionPending}
      onclick={() =>
        triggerAction(
          "finish",
          {},
          $tr("Finish runs on account #{idx}?", { idx: idx + 1 }),
        )}
    >
      <Check size={13} />
      <span>{$tr("Finish")}</span>
    </Button>
  </div>
</td>
