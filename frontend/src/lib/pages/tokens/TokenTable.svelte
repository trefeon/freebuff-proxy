<script>
  import Card from "../../components/Card.svelte";
  import Button from "../../components/Button.svelte";
  import EmptyState from "../../components/EmptyState.svelte";
  import TokenCard from "../../components/TokenCard.svelte";
  import TokenCardMobile from "../../components/TokenCardMobile.svelte";
  import { tr } from "../../i18n.js";

  /**
   * TokenTable - the "Pool Tokens" card: desktop table + mobile stacked card
   * grid, one row/card per pooled token. Split out of Tokens.svelte (issue
   * #287) so the page owns add-token/rotation/device-login/page state while
   * this component owns the table rendering.
   *
   * @prop {Array} tokens
   * @prop {number} [tokenCount=0]
   * @prop {boolean} [loading=false]
   * @prop {string} [error='']
   * @prop {number|null} expandedToken
   * @prop {boolean} [actionPending=false]
   * @prop {number} now
   * @prop {boolean} [devToolsEnabled=false]
   * @prop {Object} [spawnModels={}] - per-token selected spawn model (bindable)
   * @prop {(idx: number) => void} onToggle
   * @prop {(token: object, idx: number, action: string) => void} onAction
   * @prop {(idx: number, model: string) => void} onSpawn
   * @prop {(idx: number, action: string) => void} onRefresh
   * @prop {(idx: number) => void} onDropSession
   * @prop {(from: number, to: number) => void} [onSwap]
   * @prop {(from: number, to: number) => void} [onMove]
   * @prop {() => void} onRetry
   */
  let {
    tokens = [],
    tokenCount = 0,
    loading = false,
    error = "",
    expandedToken = null,
    actionPending = false,
    now,
    devToolsEnabled = false,
    spawnModels = $bindable({}),
    onToggle,
    onAction,
    onSpawn,
    onRefresh,
    onDropSession,
    onSwap,
    onMove,
    onRetry,
  } = $props();

  let draggingIndex = $state(null);
  let dragOverIndex = $state(null);

  function handleDragStart(e, idx) {
    if (actionPending) return;
    draggingIndex = idx;
    dragOverIndex = null;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", String(idx));
    }
  }

  function handleDragOver(e, idx) {
    if (draggingIndex === null || draggingIndex === idx) return;
    e.preventDefault();
    if (e.dataTransfer) {
      e.dataTransfer.dropEffect = "move";
    }
    dragOverIndex = idx;
  }

  function handleDragLeave(e, idx) {
    if (dragOverIndex === idx) {
      dragOverIndex = null;
    }
  }

  function handleDrop(e, targetIdx) {
    e.preventDefault();
    const sourceIdx =
      draggingIndex !== null
        ? draggingIndex
        : Number(e.dataTransfer?.getData("text/plain"));
    draggingIndex = null;
    dragOverIndex = null;
    if (
      !isNaN(sourceIdx) &&
      sourceIdx !== targetIdx &&
      sourceIdx >= 0 &&
      targetIdx >= 0
    ) {
      if (onMove) {
        onMove(sourceIdx, targetIdx);
      } else if (onSwap) {
        onSwap(sourceIdx, targetIdx);
      }
    }
  }

  function handleDragEnd() {
    draggingIndex = null;
    dragOverIndex = null;
  }
</script>

<Card
  title={$tr("Pool Tokens")}
  description={tokenCount
    ? $tr(
        "{count} pooled token(s) · Drag handle to reorder priority · Tap for details",
        { count: tokenCount },
      )
    : $tr("Tap a card to see session & quota details")}
>
  {#if loading}
    <div class="flex flex-col gap-3">
      <div class="skeleton skeleton-text w-1/3"></div>
      <div class="skeleton skeleton-line"></div>
      <div class="skeleton skeleton-line"></div>
      <div class="skeleton skeleton-line"></div>
      <div class="skeleton skeleton-line"></div>
    </div>
  {:else if error}
    <EmptyState title={$tr("Could not load tokens")} description={error}>
      {#snippet action()}
        <Button variant="secondary" onclick={onRetry}>
          {$tr("Retry")}
        </Button>
      {/snippet}
    </EmptyState>
  {:else if !tokens || tokens.length === 0}
    <EmptyState
      title={$tr("No tokens in pool")}
      description={$tr(
        "Add one above or use Device Login to generate credentials via browser.",
      )}
    />
  {:else}
    <!-- Desktop: table (md+) -->
    <div class="hidden md:block overflow-x-auto">
      <table class="fp-table w-full min-w-[760px]">
        <thead>
          <tr>
            <th class="w-8"></th>
            <th>{$tr("Account")}</th>
            <th>{$tr("Status")}</th>
            <th>{$tr("Instance")}</th>
            <th class="num">{$tr("Cooldown")}</th>
            <th class="num">{$tr("Usage")}</th>
            <th class="text-right">{$tr("Actions")}</th>
          </tr>
        </thead>
        <tbody>
          {#each tokens as token, i (token.index ?? i)}
            {@const idx = token.index ?? i}
            <TokenCard
              {token}
              {idx}
              totalTokens={tokens.length}
              expanded={expandedToken === idx}
              bind:spawnModel={spawnModels[idx]}
              {actionPending}
              {now}
              {devToolsEnabled}
              dragging={draggingIndex === idx}
              dragOver={dragOverIndex === idx}
              onDragStart={handleDragStart}
              onDragOver={handleDragOver}
              onDragLeave={handleDragLeave}
              onDrop={handleDrop}
              onDragEnd={handleDragEnd}
              onToggle={() => onToggle(idx)}
              onAction={(action) => onAction(token, idx, action)}
              onSpawn={(model) => onSpawn(idx, model)}
              onRefresh={(action) => onRefresh(idx, action)}
              onDropSession={() => onDropSession(idx)}
              {onSwap}
            />
          {/each}
        </tbody>
      </table>
    </div>
    <!-- Mobile: stacked cards (< md) - no horizontal scrolling -->
    <div class="md:hidden flex flex-col gap-3 p-4">
      {#each tokens as token, i (token.index ?? i)}
        {@const idx = token.index ?? i}
        <TokenCardMobile
          {token}
          {idx}
          totalTokens={tokens.length}
          expanded={expandedToken === idx}
          bind:spawnModel={spawnModels[idx]}
          {actionPending}
          {now}
          {devToolsEnabled}
          dragging={draggingIndex === idx}
          dragOver={dragOverIndex === idx}
          onDragStart={handleDragStart}
          onDragOver={handleDragOver}
          onDragLeave={handleDragLeave}
          onDrop={handleDrop}
          onDragEnd={handleDragEnd}
          onToggle={() => onToggle(idx)}
          onAction={(action) => onAction(token, idx, action)}
          onSpawn={(model) => onSpawn(idx, model)}
          onRefresh={(action) => onRefresh(idx, action)}
          onDropSession={() => onDropSession(idx)}
          {onSwap}
        />
      {/each}
    </div>
  {/if}
</Card>
