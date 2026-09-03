<script>
  import { Zap, RefreshCw, Check } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import { postAPI } from "../api/client.js";
  import { tokenActions } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { confirmAction } from "../stores/confirm.js";
  import { fallbackModelOptions, fetchModelOptions } from "../modelOptions.js";
  import { onMount } from "svelte";

  let { idx, onSpawn } = $props();

  let spawnModel = $state("mimo/mimo-v2.5");

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
      <option value={m.id}>{m.label}</option>
    {/each}
  </select>
</td>
<td class="text-right">
  <div class="inline-flex items-center gap-1.5 justify-end">
    <Button
      variant="primary"
      size="sm"
      disabled={actionPending}
      onclick={() =>
        triggerAction(
          "session",
          { model: spawnModel },
          $tr("Spawn upstream session on account #{idx} for {model}?", {
            idx: idx + 1,
            model: spawnModel,
          }),
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
        triggerAction("test", {}, $tr("Probe account #{idx}?", { idx: idx + 1 }))}
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
