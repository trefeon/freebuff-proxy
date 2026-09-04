<script>
  import { onMount } from "svelte";
  import {
    Megaphone,
    Clock,
    X,
    ExternalLink,
    ChevronDown,
    ChevronUp,
    Sparkles,
  } from "@lucide/svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  /**
   * AnnouncementsBanner — Upstream announcements, notices, peak hour alerts,
   * and live session server broadcasts.
   */
  let notices = $state([]);
  let peakHours = $state(null);
  let dismissed = $state(new Set());
  let collapsed = $state(false);
  let loaded = $state(false);

  const STORAGE_KEY = "freebuff_dismissed_notices";

  onMount(async () => {
    try {
      const stored = localStorage.getItem(STORAGE_KEY);
      if (stored) {
        dismissed = new Set(JSON.parse(stored));
      }
    } catch {
      // Ignore localStorage read errors
    }

    try {
      const res = await fetchAPI(adminApi.notices);
      if (res) {
        notices = res.notices || [];
        peakHours = res.peak_hours || null;
      }
    } catch {
      // Non-blocking degradation
    } finally {
      loaded = true;
    }
  });

  function dismissNotice(id) {
    const next = new Set(dismissed);
    next.add(id);
    dismissed = next;
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify([...next]));
    } catch {
      // Ignore localStorage write errors
    }
  }

  let activeNotices = $derived(notices.filter((n) => !dismissed.has(n.id)));
</script>

{#if loaded && activeNotices.length > 0}
  <div
    class="mb-4 rounded-xl border border-[var(--fp-border)] bg-[var(--fp-surface)] shadow-sm overflow-hidden"
    role="region"
    aria-label={$tr("Upstream Announcements")}
  >
    <!-- Header bar -->
    <div
      class="flex items-center justify-between px-3.5 py-2.5 bg-[var(--fp-surface-2)]/60 border-b border-[var(--fp-border)]/50 gap-2"
    >
      <div class="flex items-center gap-2 min-w-0">
        <span
          class="flex items-center justify-center w-5 h-5 rounded-md bg-[var(--fp-accent)]/15 text-[var(--fp-accent)] shrink-0"
        >
          <Megaphone size={13} />
        </span>
        <span
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
        >
          {$tr("Upstream Notices & Broadcasts")}
        </span>
        <span
          class="text-[10px] px-1.5 py-0.2 rounded-full font-mono font-medium bg-[var(--fp-accent)]/10 text-[var(--fp-accent)]"
        >
          {activeNotices.length}
        </span>
      </div>

      <div class="flex items-center gap-1 shrink-0">
        {#if peakHours}
          <div
            class="hidden sm:flex items-center gap-1.5 px-2 py-0.5 rounded text-[11px] font-mono {peakHours.is_peak
              ? 'bg-[var(--fp-warning)]/15 text-[var(--fp-warning)] border border-[var(--fp-warning)]/30'
              : 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20'}"
            title={$tr(
              "DeepSeek peak pricing window runs Mon-Fri 00:00-10:00 UTC",
            )}
          >
            <Clock size={11} />
            <span>
              {peakHours.is_peak
                ? $tr("Peak Window ({in} left)", {
                    in: peakHours.next_window_in,
                  })
                : $tr("Off-Peak Window")}
            </span>
          </div>
        {/if}

        <button
          type="button"
          class="fp-btn fp-btn-ghost !p-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)]"
          onclick={() => (collapsed = !collapsed)}
          aria-label={collapsed ? $tr("Expand") : $tr("Collapse")}
        >
          {#if collapsed}
            <ChevronDown size={14} />
          {:else}
            <ChevronUp size={14} />
          {/if}
        </button>
      </div>
    </div>

    <!-- Active notice items -->
    {#if !collapsed}
      <div class="p-3 flex flex-col gap-2.5">
        {#each activeNotices as notice (notice.id)}
          <div
            class="p-3 rounded-lg border text-xs flex flex-col sm:flex-row sm:items-center justify-between gap-3 {notice.tone ===
            'accent'
              ? 'bg-[var(--fp-accent)]/10 border-[var(--fp-accent)]/25'
              : notice.tone === 'warning'
                ? 'bg-[var(--fp-warning)]/10 border-[var(--fp-warning)]/25'
                : 'bg-[var(--fp-surface-2)]/70 border-[var(--fp-border)]'}"
          >
            <div class="flex items-start gap-2.5 min-w-0">
              <div class="shrink-0 mt-0.5">
                {#if notice.type === "peak_hours"}
                  <Clock size={15} class="text-[var(--fp-warning)]" />
                {:else if notice.type === "announcement"}
                  <Sparkles size={15} class="text-[var(--fp-accent)]" />
                {:else}
                  <Megaphone size={15} class="text-[var(--fp-muted)]" />
                {/if}
              </div>

              <div class="flex flex-col gap-0.5 min-w-0">
                <div class="flex items-center gap-2 flex-wrap">
                  <span class="font-semibold text-[var(--fp-text)]">
                    {notice.title}
                  </span>
                  {#if notice.badge}
                    <span
                      class="text-[9px] uppercase tracking-wider font-semibold px-1.5 py-0.2 rounded font-mono {notice.tone ===
                      'accent'
                        ? 'bg-[var(--fp-accent)]/20 text-[var(--fp-accent)]'
                        : notice.tone === 'warning'
                          ? 'bg-[var(--fp-warning)]/20 text-[var(--fp-warning)]'
                          : 'bg-[var(--fp-surface)] text-[var(--fp-muted)]'}"
                    >
                      {notice.badge}
                    </span>
                  {/if}
                </div>
                <p
                  class="text-[var(--fp-muted)] leading-relaxed break-words pr-2"
                >
                  {notice.message}
                </p>
              </div>
            </div>

            <div
              class="flex items-center gap-1.5 shrink-0 self-end sm:self-center"
            >
              {#if notice.url}
                <a
                  href={notice.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="fp-btn fp-btn-ghost !text-xs !py-1 !px-2 flex items-center gap-1 text-[var(--fp-accent)] hover:underline"
                >
                  <span>{$tr("Learn More")}</span>
                  <ExternalLink size={11} />
                </a>
              {/if}
              <button
                type="button"
                class="fp-btn fp-btn-ghost !p-1 text-[var(--fp-dim)] hover:text-[var(--fp-text)]"
                onclick={() => dismissNotice(notice.id)}
                title={$tr("Dismiss notice")}
                aria-label={$tr("Dismiss {title}", { title: notice.title })}
              >
                <X size={14} />
              </button>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </div>
{/if}
