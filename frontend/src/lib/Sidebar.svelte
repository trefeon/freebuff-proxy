<script>
  import {
    LayoutDashboard, Key, Cpu, Settings, FileText, Wrench, Menu, X, Languages,
  } from '@lucide/svelte';

  /**
   * @prop {string} activeTab
   * @prop {(tab: string) => void} onTabChange
   * @prop {{ current_version: string, has_update: boolean, latest_version: string, update_url: string }} [versionInfo]
   */
  let { activeTab = $bindable(), onTabChange, versionInfo } = $props();
  import { tr, locale, setLocale } from './i18n.js';

  let mobileOpen = $state(false);
  let drawerEl = $state(null);
  let hamburgerEl = $state(null);

  const tabs = [
    { id: 'overview', label: 'Overview', icon: LayoutDashboard },
    { id: 'tokens',   label: 'Tokens',   icon: Key },
    { id: 'models',   label: 'Models',   icon: Cpu },
    { id: 'config',   label: 'Config',   icon: Settings },
    { id: 'logs',     label: 'Logs',     icon: FileText },
    { id: 'setup',    label: 'Setup',    icon: Wrench },
  ];

  function switchTab(id) {
    activeTab = id;
    onTabChange?.(id);
    window.location.hash = id;
    closeDrawer();
  }

  function openDrawer() {
    mobileOpen = true;
    // Move focus into the drawer so keyboard users land on the nav.
    queueMicrotask(() => drawerEl?.focus());
  }

  function closeDrawer() {
    if (!mobileOpen) return;
    mobileOpen = false;
    // Return focus to the trigger (hamburger) after the drawer closes.
    queueMicrotask(() => hamburgerEl?.focus());
  }

  function handleDrawerKeydown(e) {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeDrawer();
    }
  }
</script>

<!-- Desktop fixed sidebar (224px, hidden below md) -->
<aside
  class="hidden md:flex fixed inset-y-0 left-0 z-40 w-56 flex-col border-r border-[var(--fp-border)] bg-[var(--fp-bg)]"
  aria-label="Sidebar"
>
  <nav class="flex-1 flex flex-col px-3 pt-5 pb-3" aria-label="Main navigation">
    <!-- Brand mark: amber bolt matching the favicon -->
    <a href="/admin" class="flex items-center gap-3 px-2 group" aria-label="freebuff-proxy dashboard home">
      <svg viewBox="0 0 32 32" class="w-7 h-7 shrink-0" aria-hidden="true">
        <rect width="32" height="32" rx="7" fill="var(--fp-accent)" />
        <path d="M18 6 9 18h5l-1 8 9-12h-5z" fill="var(--fp-bg)" />
      </svg>
      <span class="flex flex-col leading-tight">
        <span class="text-sm font-semibold text-[var(--fp-text)] tracking-tight">freebuff-proxy</span>
        <span class="text-[10px] font-mono uppercase tracking-[0.14em] text-[var(--fp-dim)]">Admin</span>
      </span>
    </a>

    <ul class="mt-8 space-y-0.5">
      {#each tabs as tab}
        <li>
          <a
            href={'#' + tab.id}
            onclick={() => switchTab(tab.id)}
            aria-current={activeTab === tab.id ? 'page' : undefined}
            class="relative flex items-center gap-2.5 pl-4 pr-3 py-2 rounded-sm text-xs font-medium transition-colors duration-150
              {activeTab === tab.id
                ? 'text-[var(--fp-accent)] bg-[var(--fp-surface)]'
                : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)]'}"
          >
            {#if activeTab === tab.id}
              <span class="absolute left-0 top-1/2 -translate-y-1/2 h-5 w-[2px] bg-[var(--fp-accent)]" aria-hidden="true"></span>
              <span class="led led-accent" aria-hidden="true"></span>
            {/if}
            <tab.icon size={16} class="shrink-0" />
            <span class="font-mono text-xs">{$tr(tab.label)}</span>
          </a>
        </li>
      {/each}
    </ul>

    <!-- Version badge, pinned to the bottom of the sidebar -->
    <!-- Language toggle (issue #156): EN <-> 中文, persisted via i18n store -->
    <div class="px-2 pb-2">
      <button
        type="button"
        onclick={() => setLocale($locale === 'zh' ? 'en' : 'zh')}
        aria-label={$locale === 'zh' ? 'Switch to English' : $tr('Language')}
        class="w-full flex items-center justify-center gap-1.5 px-2 py-1.5 rounded-sm text-[10px] font-mono uppercase tracking-[0.12em] text-[var(--fp-dim)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)] transition-colors border border-[var(--fp-border)]"
      >
        <Languages size={13} />
        <span>{$locale === 'zh' ? 'EN' : '中文'}</span>
      </button>
    </div>
    <div class="mt-auto border-t border-[var(--fp-border)] px-2 pt-3 pb-1">
      {#if versionInfo?.has_update}
        <a
          href={versionInfo.update_url}
          target="_blank"
          rel="noopener noreferrer"
          class="flex items-center gap-2 mb-2.5 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--fp-accent)] hover:text-[var(--fp-accent-hover)] transition-colors"
          aria-label="Update available"
        >
          <span class="led led-accent led-pulse" aria-hidden="true"></span>
          <span>update</span>
          <span class="ml-auto text-[var(--fp-dim)] normal-case tracking-normal">v{versionInfo.latest_version}</span>
        </a>
      {/if}
      <div class="flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--fp-dim)]">
        <span class="led led-idle" aria-hidden="true"></span>
        <span>freebuff-proxy</span>
        <span class="fp-num ml-auto normal-case tracking-normal text-[var(--fp-muted)]">{versionInfo?.current_version ?? 'dev'}</span>
      </div>
    </div>
  </nav>
</aside>

<!-- Mobile top bar + overlay drawer (< md) -->
<header
  class="md:hidden sticky top-0 z-50 border-b border-[var(--fp-border)] bg-[var(--fp-bg)]"
  onkeydown={handleDrawerKeydown}
>
  <div class="flex items-center justify-between h-14 px-4">
    <a href="/admin" class="flex items-center gap-2.5 group" aria-label="freebuff-proxy dashboard home">
      <svg viewBox="0 0 32 32" class="w-7 h-7 shrink-0" aria-hidden="true">
        <rect width="32" height="32" rx="7" fill="var(--fp-accent)" />
        <path d="M18 6 9 18h5l-1 8 9-12h-5z" fill="var(--fp-bg)" />
      </svg>
      <span class="flex flex-col leading-tight">
        <span class="text-sm font-semibold text-[var(--fp-text)] tracking-tight">freebuff-proxy</span>
        <span class="text-[10px] font-mono uppercase tracking-[0.14em] text-[var(--fp-dim)]">Admin</span>
      </span>
    </a>

    <button
      bind:this={hamburgerEl}
      class="p-2.5 min-w-11 min-h-11 rounded-lg text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface)] transition-colors flex items-center justify-center"
      onclick={mobileOpen ? closeDrawer : openDrawer}
      aria-label={mobileOpen ? 'Close menu' : 'Open menu'}
      aria-expanded={mobileOpen}
      aria-controls="mobile-nav"
    >
      {#if mobileOpen}
        <X size={20} />
      {:else}
        <Menu size={20} />
      {/if}
    </button>
  </div>
</header>

<!-- Mobile overlay drawer -->
{#if mobileOpen}
  <div class="md:hidden fixed inset-0 z-40">
    <div class="absolute inset-0 bg-black/60" onclick={closeDrawer} aria-hidden="true"></div>
    <nav
      id="mobile-nav"
      bind:this={drawerEl}
      tabindex="-1"
      aria-label="Mobile navigation"
      class="absolute inset-y-0 left-0 w-64 flex flex-col border-r border-[var(--fp-border)] bg-[var(--fp-bg)] px-3 pt-5 pb-3 focus:outline-none"
      onkeydown={handleDrawerKeydown}
    >
      <a href="/admin" class="flex items-center gap-3 px-2 mb-8" aria-label="freebuff-proxy dashboard home">
        <svg viewBox="0 0 32 32" class="w-7 h-7 shrink-0" aria-hidden="true">
          <rect width="32" height="32" rx="7" fill="var(--fp-accent)" />
          <path d="M18 6 9 18h5l-1 8 9-12h-5z" fill="var(--fp-bg)" />
        </svg>
        <span class="flex flex-col leading-tight">
          <span class="text-sm font-semibold text-[var(--fp-text)] tracking-tight">freebuff-proxy</span>
          <span class="text-[10px] font-mono uppercase tracking-[0.14em] text-[var(--fp-dim)]">Admin</span>
        </span>
      </a>

      <ul class="space-y-0.5">
        {#each tabs as tab}
          <li>
            <a
              href={'#' + tab.id}
              onclick={() => switchTab(tab.id)}
              aria-current={activeTab === tab.id ? 'page' : undefined}
              class="relative flex items-center gap-2.5 pl-4 pr-3 py-2.5 min-h-11 rounded-sm text-sm font-medium transition-colors
                {activeTab === tab.id
                  ? 'text-[var(--fp-accent)] bg-[var(--fp-surface)]'
                  : 'text-[var(--fp-muted)] hover:text-[var(--fp-text)] hover:bg-[var(--fp-surface)]'}"
            >
              {#if activeTab === tab.id}
                <span class="absolute left-0 top-1/2 -translate-y-1/2 h-5 w-[2px] bg-[var(--fp-accent)]" aria-hidden="true"></span>
                <span class="led led-accent" aria-hidden="true"></span>
              {/if}
              <tab.icon size={16} class="shrink-0" />
              <span class="font-mono text-xs">{tab.label}</span>
            </a>
          </li>
        {/each}
      </ul>

      <div class="mt-auto border-t border-[var(--fp-border)] px-2 pt-3 pb-1">
        {#if versionInfo?.has_update}
          <a
            href={versionInfo.update_url}
            target="_blank"
            rel="noopener noreferrer"
            class="flex items-center gap-2 mb-2.5 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--fp-accent)] hover:text-[var(--fp-accent-hover)] transition-colors"
            aria-label="Update available"
          >
            <span class="led led-accent led-pulse" aria-hidden="true"></span>
            <span>update</span>
            <span class="ml-auto text-[var(--fp-dim)] normal-case tracking-normal">v{versionInfo.latest_version}</span>
          </a>
        {/if}
        <div class="flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--fp-dim)]">
          <span class="led led-idle" aria-hidden="true"></span>
          <span>freebuff-proxy</span>
          <span class="fp-num ml-auto normal-case tracking-normal text-[var(--fp-muted)]">{versionInfo?.current_version ?? 'dev'}</span>
        </div>
      </div>
    </nav>
  </div>
{/if}
