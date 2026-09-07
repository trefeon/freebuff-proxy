<script>
  import { RefreshCw } from "@lucide/svelte";
  import PageHeader from "./PageHeader.svelte";
  import Alert from "./Alert.svelte";
  import EmptyState from "./EmptyState.svelte";
  import Button from "./Button.svelte";
  import { tr } from "../i18n.js";

  /**
   * PageShell — the single mount frame every dashboard page uses. Renders the
   * PageHeader, then exactly one state: skeleton, error + retry, empty, or
   * content. Pages own data; they never re-implement loading/error markup.
   *
   * @prop {string} title
   * @prop {string} [description]
   * @prop {boolean} [loading=false]
   * @prop {string} [error='']
   * @prop {string} [errorTitle=''] — page-specific error heading; falls
   *   back to the generic "Could not load this page".
   * @prop {{ title: string, description?: string } | null} [empty=null]
   * @prop {() => void} [onRetry]
   * @slot actions — PageHeader right-side actions
   * @slot default — page content (rendered only when loaded, no error, not empty)
   * @prop {string} [crumb=''] — mono path line above the header
   *   (decorative, aria-hidden); e.g. "freebuff-proxy / Admin / tokens.conf".
   */
  let {
    title,
    description,
    actions,
    loading = false,
    error = "",
    errorTitle = "",
    empty = null,
    onRetry,
    children,
    crumb = "",
  } = $props();
</script>

{#snippet retryAction()}
  <Button variant="secondary" onclick={onRetry}>
    <RefreshCw size={15} />
    {$tr("Retry")}
  </Button>
{/snippet}
<div class="space-y-6 page-enter">
  <div class="flex flex-col gap-1.5">
    {#if crumb}
      <p class="font-mono text-[11px] text-[var(--fp-dim)]" aria-hidden="true">
        {crumb}
      </p>
    {/if}
    <PageHeader {title} {description} {actions} />
  </div>

  {#if loading && !error}
    <div role="status" aria-label={$tr("Loading")} class="space-y-3">
      <div class="skeleton skeleton-card"></div>
      <div class="skeleton skeleton-text w-2/3"></div>
      <div class="skeleton skeleton-text w-1/2"></div>
    </div>
  {:else if error}
    <Alert tone="error" title={errorTitle || $tr("Could not load this page")}>
      {error}
      {#if onRetry}
        <div class="mt-3">
          {@render retryAction()}
        </div>
      {/if}
    </Alert>
  {:else if empty}
    {#if onRetry}
      <EmptyState
        title={empty.title}
        description={empty.description}
        action={retryAction}
      />
    {:else}
      <EmptyState title={empty.title} description={empty.description} />
    {/if}
  {:else if children}
    {@render children()}
  {/if}
</div>
