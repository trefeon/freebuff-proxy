<script>
  import { Menu, X } from '@lucide/svelte';

  /**
   * @prop {string} activeTab
   * @prop {(tab: string) => void} onTabChange
   * @prop {{ current_version: string, has_update: boolean, latest_version: string, update_url: string }} [versionInfo]
   */
  let { activeTab = $bindable(), versionInfo } = $props();
  import { tr } from './i18n.js';
  import { isDevToolsEnabled } from './utils/devtools.js';
  import { NAV_ITEMS } from './nav.js';
  import { onMount } from 'svelte';
  import { fetchAPI } from './api/client.js';
  import { adminApi, adminRoot } from './api/paths.js';

  let mobileOpen = $state(false);
  let drawerEl = $state(null);
  let hamburgerEl = $state(null);
  // Dev Tools is a manual testing surface (batch chat, session spawn); it is
  // hidden unless the operator explicitly enables DEVTOOLS_ENABLED=true.
  let devToolsEnabled = $state(false);

  const tabs = $derived(
    NAV_ITEMS.filter((item) => {
      if (item.inSidebar === false) return false;
      if (item.gate === 'devtools') return devToolsEnabled;
      return true;
    })
  );
  onMount(async () => {
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || '';
      devToolsEnabled = isDevToolsEnabled(envContent);
    } catch {
      devToolsEnabled = false;
    }
  });

  function switchTab(id) {
    activeTab = id;
    window.location.hash = id;
    closeDrawer();
  }

  function openDrawer() {
    mobileOpen = true;
  }

  function closeDrawer() {
    if (!mobileOpen) return;
    mobileOpen = false;
  }

  function getFocusable(container) {
    if (!container) return [];
    const selector =
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
    return Array.from(container.querySelectorAll(selector)).filter((el) => {
      if (el.hasAttribute('hidden')) return false;
      try {
        const style = getComputedStyle(el);
        if (style.visibility === 'hidden' || style.display === 'none') return false;
      } catch {}
      return true;
    });
  }

  function trapFocus(e, container) {
    if (e.key !== 'Tab' || !container) return;
    const focusables = getFocusable(container);
    if (focusables.length === 0) {
      e.preventDefault();
      return;
    }
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;
    if (e.shiftKey) {
      if (active === first) {
        e.preventDefault();
        last.focus();
      }
    } else {
      if (active === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  function setDrawerBackgroundInert(enabled) {
    try {
      const main = document.getElementById('main-content');
      const desktopAside = document.querySelector('aside[aria-label="Sidebar"]');
      // We inert main-content and the desktop sidebar (hidden on mobile but inert is harmless).
      // Never inert the mobile header itself while drawer is open via inert attribute on the header
      // element that would contain the focused drawer? The drawer is outside header, so safe to inert header's brand? Instead only inert main to avoid trapping the hamburger.
      // For strict modal behavior, inert main is sufficient; header's interactive hamburger is not reachable via Tab due to trap.
      const targets = [main, desktopAside];
      for (const el of targets) {
        if (!el) continue;
        if (enabled) {
          if ('inert' in el) el.inert = true;
          else el.setAttribute('inert', '');
          el.setAttribute('aria-hidden', 'true');
        } else {
          if ('inert' in el) el.inert = false;
          el.removeAttribute('inert');
          el.removeAttribute('aria-hidden');
        }
      }
    } catch {}
  }

  // Manage focus trap, inert, Escape, focus return, and scroll lock for mobile drawer
  $effect(() => {
    if (!mobileOpen) return;
    // Focus will move into drawer; capture previous focus after render queue
    let prevOverflow = '';
    queueMicrotask(() => {
      try {
        const focusables = getFocusable(drawerEl);
        const target = focusables[0] ?? drawerEl;
        target?.focus();
        setDrawerBackgroundInert(true);
      } catch {}
    });

    const handleKeydown = (e) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        closeDrawer();
        return;
      }
      if (e.key === 'Tab') trapFocus(e, drawerEl);
    };

    document.addEventListener('keydown', handleKeydown);
    try {
      prevOverflow = document.body.style.overflow;
      document.body.style.overflow = 'hidden';
    } catch {}

    return () => {
      document.removeEventListener('keydown', handleKeydown);
      try {
        document.body.style.overflow = prevOverflow;
      } catch {}
      try {
        setDrawerBackgroundInert(false);
      } catch {}
      queueMicrotask(() => {
        try {
          hamburgerEl?.focus();
        } catch {}
      });
    };
  });
</script>

<!-- Desktop fixed sidebar (224px, hidden below md) -->
<aside
  class="hidden md:flex fixed inset-y-0 left-0 z-40 w-56 flex-col border-r border-[var(--fp-border)] bg-[var(--fp-bg)]"
  aria-label="Sidebar"
>
  <nav class="flex-1 flex flex-col px-3 pt-5 pb-3" aria-label="Main navigation">
    <!-- Brand mark: amber bolt matching the favicon -->
    <a href={adminRoot} class="flex items-center gap-3 px-2 group" aria-label="freebuff-proxy dashboard home">
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
>
  <div class="flex items-center gap-2 h-14 px-4">
    <button
      bind:this={hamburgerEl}
      class="p-2.5 min-w-11 min-h-11 rounded-lg text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface)] transition-colors flex items-center justify-center shrink-0"
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

    <a href={adminRoot} class="flex items-center gap-2.5 group" aria-label="freebuff-proxy dashboard home">
      <svg viewBox="0 0 32 32" class="w-7 h-7 shrink-0" aria-hidden="true">
        <rect width="32" height="32" rx="7" fill="var(--fp-accent)" />
        <path d="M18 6 9 18h5l-1 8 9-12h-5z" fill="var(--fp-bg)" />
      </svg>
      <span class="flex flex-col leading-tight">
        <span class="text-sm font-semibold text-[var(--fp-text)] tracking-tight">freebuff-proxy</span>
        <span class="text-[10px] font-mono uppercase tracking-[0.14em] text-[var(--fp-dim)]">Admin</span>
      </span>
    </a>
  </div>
</header>

<!-- Mobile overlay drawer -->
{#if mobileOpen}
  <div class="md:hidden fixed inset-0 z-40">
    <button
      type="button"
      class="absolute inset-0 bg-black/60 border-0 p-0 m-0 w-full h-full cursor-default"
      onclick={closeDrawer}
      aria-label={$tr('Close menu')}
      tabindex="-1"
    ></button>
    <nav
      id="mobile-nav"
      bind:this={drawerEl}
      tabindex="-1"
      aria-label="Mobile navigation"
      class="absolute inset-y-0 left-0 w-64 flex flex-col border-r border-[var(--fp-border)] bg-[var(--fp-bg)] px-3 pt-5 pb-3 focus:outline-none"
    >
      <a href={adminRoot} class="flex items-center gap-3 px-2 mb-8" aria-label="freebuff-proxy dashboard home">
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
              <span class="font-mono text-xs">{$tr(tab.label)}</span>
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
