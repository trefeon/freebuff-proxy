<script>
  import { tr } from "../i18n.js";
  import { formatFreebucks } from "../utils/freebucks.js";
  import { refreshRefund, autoRefreshRefund } from "../utils/refundRefresh.js";

  /**
   * RefundLines — the pending + settled refund lines for one account card.
   * Shared by the Accounts list (AllowancesPanel) and the account drawer
   * (TokenDetailsDrawer) so both surfaces copy, refresh, and auto-attempt
   * identically.
   *
   * Copy mirrors the official CLI landing screen: the pending line keeps the
   * dashboard wording, the settled line renders "{n} {Freebuck|Freebucks}
   * returned to your wallet." (zero included — a zero receipt is settled).
   *
   * @prop {object} token — dashboard tokenCard payload
   * @prop {string} pendingClass — classes for the pending <p>
   * @prop {string} settledClass — classes for the settled <p>
   */
  let { token, pendingClass = "", settledClass = "" } = $props();

  let refreshing = $state(false);
  let refreshError = $state("");

  const pending = $derived(token?.pending_refund ?? "");
  const settled = $derived(token?.last_refund ?? null);

  // One automatic attempt when the pending line first renders (per parked
  // instance id, never per render — see autoRefreshRefund).
  $effect(() => {
    if (pending) autoRefreshRefund(token);
  });

  async function onRefresh() {
    const idx = token?.index ?? -1;
    if (!Number.isInteger(idx) || idx < 0 || refreshing) return;
    refreshing = true;
    refreshError = "";
    try {
      await refreshRefund(idx);
    } catch (e) {
      refreshError = e?.message || $tr("Refresh failed.");
    } finally {
      refreshing = false;
    }
  }
</script>

{#if pending}
  <p class={pendingClass} data-testid="refund-line">
    {$tr(
      "Your refund is awaiting final usage, once settled it will appear in your wallet",
    )}
    <button
      type="button"
      class="underline underline-offset-2 hover:opacity-80 disabled:no-underline disabled:opacity-50"
      disabled={refreshing}
      onclick={onRefresh}
    >
      {refreshing ? $tr("Refreshing…") : $tr("Refresh")}
    </button>
  </p>
  {#if refreshError}
    <p class="text-xs text-[var(--fp-error)]" role="status">
      {refreshError}
    </p>
  {/if}
{/if}
{#if settled !== null && settled !== undefined}
  <p class={settledClass} data-testid="refund-settled-line">
    {$tr("{amount} {unit} returned to your wallet.", {
      amount: formatFreebucks(settled),
      unit: Number(settled) === 1 ? $tr("Freebuck") : $tr("Freebucks"),
    })}
  </p>
{/if}
