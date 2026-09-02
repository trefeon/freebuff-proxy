<script>
  /**
   * @prop {{ current_version: string, has_update: boolean, latest_version: string, update_url: string }} [versionInfo]
   */
  let { versionInfo } = $props();
</script>

<footer class="border-t border-[var(--fp-border)]">
  <div
    class="max-w-[1200px] mx-auto px-6 py-3 flex items-center justify-between gap-4"
  >
    <span class="font-mono text-[11px] text-[var(--fp-dim)]">
      freebuff-proxy <span class="fp-num text-[var(--fp-muted)]"
        >{versionInfo?.current_version ?? "dev"}</span
      >
    </span>
    <div class="flex items-center gap-4">
      {#if versionInfo?.has_update}
        <a
          href={versionInfo.update_url}
          target="_blank"
          rel="noopener noreferrer"
          class="inline-flex items-center gap-1.5 font-mono text-[11px] text-[var(--fp-accent)] hover:text-[var(--fp-accent-hover)] transition-colors"
          aria-label="Update available"
        >
          <span class="led led-accent led-pulse" aria-hidden="true"></span>
          <span>update to {versionInfo.latest_version}</span>
        </a>
      {/if}
      <a
        href={versionInfo?.update_url ??
          "https://github.com/trefeon/freebuff-proxy/releases"}
        target="_blank"
        rel="noopener noreferrer"
        class="font-mono text-[11px] text-[var(--fp-dim)] hover:text-[var(--fp-muted)] transition-colors"
      >
        github releases
      </a>
    </div>
  </div>
</footer>
