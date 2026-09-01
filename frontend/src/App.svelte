<script>
  import { onMount } from 'svelte';
  import Sidebar from './lib/Sidebar.svelte';
  import Footer from './lib/Footer.svelte';
  import Overview from './lib/pages/Overview.svelte';
  import Tokens from './lib/pages/Tokens.svelte';
  import QuotaTracker from './lib/pages/QuotaTracker.svelte';
  import Models from './lib/pages/Models.svelte';
  import Settings from './lib/pages/Settings.svelte';
  import Logs from './lib/pages/Logs.svelte';
  import DevTools from './lib/pages/DevTools.svelte';
  import Login from './lib/pages/Login.svelte';
  import Setup from './lib/pages/Setup.svelte';
  import Metrics from './lib/pages/Metrics.svelte';
  import Traces from './lib/pages/Traces.svelte';
  import ChangePasswordModal from './lib/components/ChangePasswordModal.svelte';
  import Alert from './lib/components/Alert.svelte';
  import Button from './lib/components/Button.svelte';
  import EmptyState from './lib/components/EmptyState.svelte';
  import { X } from '@lucide/svelte';
  import { fetchAPI } from './lib/api/client.js';
  import { adminApi, adminActions } from './lib/api/paths.js';
  import { sessionExpired, dismissSessionExpired } from './lib/stores/session.js';
  import { tr } from './lib/i18n.js';
  function getInitialTab() {
    if (typeof window === 'undefined') return 'overview';
    const path = window.location.pathname;
    const hash = window.location.hash.replace('#', '');
    if (path === adminActions.login || hash === 'login') return 'login';
    // Legacy alias: '#config' still routes to the Settings page.
    if (hash === 'config') return 'settings';
    if (hash) return hash;
    const segments = path.split('/').filter(Boolean);
    if (segments.length >= 2 && segments[0] === 'admin' && segments[1]) {
      return segments[1];
    }
    return 'overview';
  }

  let activeTab = $state(getInitialTab());
  let versionInfo = $state(null);
  let isDefaultAdminToken = $state(false);
  let showChangePasswordModal = $state(false);

  function syncTabFromURL() {
    activeTab = getInitialTab();
  }

  $effect(() => {
    if (activeTab !== 'login' && window.location.hash.replace('#', '') !== activeTab) {
      window.location.hash = activeTab;
    }
  });

  // Explicit user action only — never invoked from background polling.
  function goToLogin() {
		// Hash-only navigation: the SPA owns the login view, so no
		// network round-trip to the gateway's login route (which on the dev
		// server is the gateway's own route). Login.svelte reads the carried hash after
		// signing in, if any was present.
		window.location.hash = 'login';
	}

  onMount(() => {
    syncTabFromURL();
    window.addEventListener('hashchange', syncTabFromURL);

    // Fetch version / update check. fetchAPI (not raw fetch): it routes the
    // admin base and surfaces the session-expired 401/redirect so the
    // login banner fires instead of swallowing an HTML error (issue #244).
    fetchAPI(adminApi.version)
      .then((data) => {
        versionInfo = {
          current_version: data.current_version || '',
          has_update: data.has_update || false,
          latest_version: data.latest_version || '',
          update_url: data.update_url || '',
        };
      })
      .catch((e) => console.warn('version check failed', e));

    // Check if using default admin token (for security banner)
    fetchAPI(adminApi.authStatus)
      .then((data) => {
        isDefaultAdminToken = data?.is_default_admin_token ?? false;
      })
      .catch(() => {});

    return () => {
      window.removeEventListener('hashchange', syncTabFromURL);
    };
  });
</script>

<div class="min-h-screen bg-[var(--fp-bg)] text-[var(--fp-text)] flex flex-col font-sans selection:bg-[var(--fp-accent)]/30 selection:text-white instrument-grid">
  <a
    href="#main-content"
    class="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus:z-[60] focus:px-4 focus:py-2 focus:rounded-lg focus:bg-[var(--fp-accent)] focus:text-[var(--fp-bg)] focus:font-semibold focus:text-sm"
  >
    Skip to content
  </a>

  {#if activeTab !== 'login'}
    <Sidebar bind:activeTab {versionInfo} />
  {/if}

  {#if isDefaultAdminToken && activeTab !== 'login'}
    <ChangePasswordModal
      bind:open={showChangePasswordModal}
      onSuccess={() => { isDefaultAdminToken = false; }}
    />
  {/if}

  <div class="flex-1 flex flex-col {activeTab !== 'login' ? 'md:pl-56' : ''}">
    <main id="main-content" tabindex="-1" class="flex-1 w-full max-w-[1200px] mx-auto px-6 py-8">
      {#if $sessionExpired && activeTab !== 'login'}
        <div class="mb-6">
          <Alert tone="error" title={$tr('Session expired')}>
            <div class="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-2">
              <span>{$tr('Your session has ended. Sign in again to continue using the dashboard.')}</span>
              <div class="flex items-center gap-2 shrink-0">
                <Button variant="secondary" size="sm" onclick={goToLogin}>{$tr('Log in')}</Button>
                <button
                  type="button"
                  class="fp-btn fp-btn-ghost fp-btn-sm"
                  aria-label={$tr('Dismiss session expired notice')}
                  onclick={dismissSessionExpired}
                >
                  <X size={15} />
                </button>
              </div>
            </div>
          </Alert>
        </div>
      {/if}
      {#if isDefaultAdminToken && activeTab !== 'login'}
        <div class="mb-6">
          <SecurityBanner onChangePassword={() => { showChangePasswordModal = true; }} />
        </div>
      {/if}

      {#key activeTab}
        <div class="page-enter">
          {#if activeTab === 'overview'}
            <Overview />
          {:else if activeTab === 'tokens'}
            <Tokens />
          {:else if activeTab === 'quota'}
            <QuotaTracker />
          {:else if activeTab === 'models'}
            <Models />
          {:else if activeTab === 'settings'}
            <Settings />
          {:else if activeTab === 'logs'}
            <Logs />
          {:else if activeTab === 'devtools'}
            <DevTools />
          {:else if activeTab === 'playground'}
            <DevTools />
          {:else if activeTab === 'setup'}
            <Setup />
          {:else if activeTab === 'metrics'}
            <Metrics />
          {:else if activeTab === 'traces'}
            <Traces />
          {:else if activeTab === 'login'}
            <Login />
          {:else}
            <EmptyState title={$tr('Page not found')} description={$tr('This tab does not exist. Pick a page from the sidebar.')}>
              {#snippet action()}
                <Button variant="secondary" onclick={() => (activeTab = 'overview')}>
                  {$tr('Back to Overview')}
                </Button>
              {/snippet}
            </EmptyState>
          {/if}
        </div>
      {/key}
    </main>

    {#if activeTab !== 'login'}
      <Footer {versionInfo} />
    {/if}
  </div>
</div>
