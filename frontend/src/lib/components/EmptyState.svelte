<script>
  import { Inbox } from "@lucide/svelte";

  /**
   * EmptyState — centered empty/fallback block with optional action.
   *
   * @prop {string} title
   * @prop {string} [description]
   * @slot action
   */
  let { title, description, action } = $props();
</script>

<div
  class="fp-card flex flex-col items-center justify-center gap-2 px-6 py-12 text-center"
>
  <Inbox
    size={32}
    strokeWidth={1.5}
    class="text-[var(--fp-dim)]"
    aria-hidden="true"
  />
  <h3 class="font-semibold text-[var(--fp-text)]">{title}</h3>
  {#if description}
    <p class="max-w-md text-sm text-[var(--fp-muted)]">{description}</p>
  {/if}
  {#if action}
    <div class="mt-2">
      {@render action()}
    </div>
  {/if}
</div>
