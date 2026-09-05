<script>
  import Card from "./Card.svelte";
  import CopyButton from "./CopyButton.svelte";
  import { tr } from "../i18n.js";

  // Referral grant banner (mirrors the CLI referral surface): unlocked when
  // any pool token carries a referral grant, otherwise the locked pitch.
  // Reads the static referral_* keys off the shared tokens snapshot —
  // the 10s live poll strips them, the store merges them back.
  let { tokens = [] } = $props();

  const referred = $derived(tokens.filter((t) => t.has_referral));
  const best = $derived(
    referred.length === 0
      ? null
      : referred.reduce((a, b) =>
          (b.referral_sessions_left ?? 0) > (a.referral_sessions_left ?? 0)
            ? b
            : a,
        ),
  );
</script>

{#if best}
  <Card>
    <div class="flex items-center gap-2 flex-wrap">
      <span class="font-semibold"
        >✦ +{best.referral_sessions_left ?? 0}
        {$tr("premium session(s)/day from referrals")}</span
      >
      {#if best.referral_code}
        <code class="fp-num rounded bg-white/5 px-1.5 py-0.5"
          >{best.referral_code}</code
        >
        <CopyButton text={best.referral_code} />
      {/if}
      <span class="text-sm opacity-70">
        {best.referral_qualified_count ?? 0}
        {$tr("qualified")} ·
        {#if best.referral_github_linked}
          {$tr("GitHub linked")}
        {:else}
          {$tr("Connect GitHub to qualify")}
        {/if}
      </span>
    </div>
  </Card>
{:else if tokens.length > 0}
  <Card>
    <div class="text-sm opacity-80">
      ✦ {$tr("Refer friends → +1 premium session/day")}
    </div>
  </Card>
{/if}
