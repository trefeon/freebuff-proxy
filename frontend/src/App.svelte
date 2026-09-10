<script>
  import { onMount } from "svelte";
  import Sidebar from "./lib/Sidebar.svelte";
  import Login from "./lib/pages/Login.svelte";
  import { pageComponentFor, resolveLegacyPage } from "./lib/nav.js";
  import ChangePasswordModal from "./lib/components/ChangePasswordModal.svelte";
  import ConfirmModal from "./lib/components/ConfirmModal.svelte";
  import SecurityBanner from "./lib/components/SecurityBanner.svelte";
  import Alert from "./lib/components/Alert.svelte";
  import Button from "./lib/components/Button.svelte";
  import EmptyState from "./lib/components/EmptyState.svelte";
  import { fetchAPI } from "./lib/api/client.js";
  import { adminApi, adminActions } from "./lib/api/paths.js";
  import {
    sessionExpired,
    authState,
    updateAuthState,
  } from "./lib/stores/session.js";
  import { tr } from "./lib/i18n.js";
  import { loadPageState, savePageState } from "./lib/stores/pageState.js";
  // Legacy page ids redirect to their IA-merge target (see
  // LEGACY_PAGE_REDIRECTS in lib/nav.js). A redirect carrying a tab plants
  // the one-shot sessionStorage key the target page consumes on mount for
  // its initial tab, then returns the target page.
  function applyLegacyRedirect(id) {
    const target = resolveLegacyPage(id);
    if (!target) return id;
    if (target.tab) {
      try {
        sessionStorage.setItem(`fp-page-tab:${target.page}`, target.tab);
      } catch {
        // Storage unavailable — the target page opens on its default tab.
      }
    }
    return target.page;
  }
  function getInitialTab() {
    if (typeof window === "undefined") return "overview";
    const path = window.location.pathname;
    const hash = window.location.hash.replace("#", "");
    if (path === adminActions.login || hash === "login") return "login";
    // Legacy hash (incl. the '#config' alias): redirect to the target page.
    if (hash) return applyLegacyRedirect(hash);
    const segments = path.split("/").filter(Boolean);
    if (segments.length >= 2 && segments[0] === "admin" && segments[1]) {
      return applyLegacyRedirect(segments[1]);
    }
    return "overview";
  }

  let activeTab = $state(getInitialTab());
  let versionInfo = $state(null);
  let isDefaultAdminToken = $derived($authState.isDefaultAdminToken);
  let showChangePasswordModal = $state(false);

  // Page mount resolved from the same nav registry the Sidebar filters
  // (issue #290): one source of truth for the page set.
  let pageComponent = $derived(pageComponentFor(activeTab));

  function syncTabFromURL() {
    activeTab = getInitialTab();
  }

  // Shell chrome state (pages_state "shell" key): the last-visited hash,
  // restored on boot when the URL carries no explicit route. Decided
  // synchronously at init (not in onMount): the tab-sync effect below can
  // run before onMount and would otherwise stamp #overview over the
  // in-flight restore.
  let restoringHash = !hasExplicitRoute();

  function hasExplicitRoute() {
    if (typeof window === "undefined") return true;
    if (window.location.hash.replace("#", "")) return true;
    const segments = window.location.pathname.split("/").filter(Boolean);
    return segments.length >= 2 && segments[0] === "admin" && !!segments[1];
  }

  function persistHash() {
    const h = window.location.hash.replace("#", "");
    // Only known pages are remembered: persisting an unknown hash would
    // reopen a NotFound view on the next boot with no explicit route.
    // Legacy ids normalize to their redirect target first (a stored legacy
    // id would otherwise reopen NotFound once the redirect is gone).
    // shell.lastHash is last-writer-wins (see lib/stores/pageState.js).
    const norm = resolveLegacyPage(h)?.page ?? h;
    if (norm && pageComponentFor(norm))
      savePageState("shell", { lastHash: norm });
  }

  $effect(() => {
    if (restoringHash) return;
    if (
      activeTab !== "login" &&
      window.location.hash.replace("#", "") !== activeTab
    ) {
      window.location.hash = activeTab;
    }
  });

  // Explicit user action only — never invoked from background polling.
  function goToLogin() {
    // Hash-only navigation: the SPA owns the login view, so no
    // network round-trip to the gateway's login route (which on the dev
    // server is the gateway's own route). Login.svelte reads the carried hash after
    // signing in, if any was present.
    window.location.hash = "login";
  }

  onMount(() => {
    syncTabFromURL();
    window.addEventListener("hashchange", syncTabFromURL);
    window.addEventListener("hashchange", persistHash);

    // Boot restore: no explicit hash/path route reopens the last-visited
    // page (shell.lastHash in pages_state). An explicit route always wins
    // and is persisted as the newest visit.
    if (restoringHash) {
      loadPageState("shell")
        .then((d) => {
          const raw = d && typeof d.lastHash === "string" ? d.lastHash : "";
          // A pre-merge snapshot may name a removed page: reopen its
          // redirect target (with the tab one-shot) instead of NotFound.
          const h = applyLegacyRedirect(raw.replace("#", ""));
          if (h && pageComponentFor(h)) window.location.hash = h;
        })
        .finally(() => {
          restoringHash = false;
          if (!window.location.hash.replace("#", ""))
            window.location.hash = activeTab;
        });
    } else {
      persistHash();
    }

    // Fetch version / update check. fetchAPI (not raw fetch): it routes the
    // admin base and surfaces the session-expired 401/redirect so the
    // login banner fires instead of swallowing an HTML error (issue #244).
    fetchAPI(adminApi.version)
      .then((data) => {
        versionInfo = {
          current_version: data.current_version || "",
          has_update: data.has_update || false,
          latest_version: data.latest_version || "",
          update_url: data.update_url || "",
        };
      })
      .catch((e) => console.warn("version check failed", e));

    // Check if using default admin token (for security banner)
    fetchAPI(adminApi.authStatus)
      .then((data) => {
        updateAuthState({
          isDefaultAdminToken: Boolean(data?.is_default_admin_token),
          requireLogin: Boolean(data?.require_login),
          hasPassword: Boolean(data?.has_password),
        });
      })
      .catch(() => {});

    return () => {
      window.removeEventListener("hashchange", syncTabFromURL);
      window.removeEventListener("hashchange", persistHash);
    };
  });
</script>

<div
  class="min-h-screen bg-[var(--fp-bg)] text-[var(--fp-text)] flex flex-col font-sans selection:bg-[var(--fp-accent)]/30 selection:text-white instrument-grid"
>
  <a
    href="#main-content"
    class="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus:z-[60] focus:px-4 focus:py-2 focus:rounded focus:bg-[var(--fp-accent)] focus:text-[var(--fp-bg)] focus:font-semibold focus:text-sm"
  >
    Skip to content
  </a>

  {#if activeTab !== "login"}
    <Sidebar bind:activeTab {versionInfo} />
  {/if}

  {#if isDefaultAdminToken && activeTab !== "login"}
    <ChangePasswordModal
      bind:open={showChangePasswordModal}
      onSuccess={() => {
        updateAuthState({
          isDefaultAdminToken: false,
          hasPassword: true,
          requireLogin: true,
        });
      }}
    />
  {/if}

  <ConfirmModal />

  <div class="flex-1 min-w-0 {activeTab !== 'login' ? 'md:pl-56' : ''}">
    <main
      id="main-content"
      tabindex="-1"
      class="w-full max-w-[1200px] mx-auto px-4 py-6 sm:px-6 sm:py-8"
    >
      {#if $sessionExpired && activeTab !== "login"}
        <div class="space-y-6 page-enter">
          <Alert tone="error" title={$tr("Session expired")}>
            <div
              class="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-2"
            >
              <span
                >{$tr(
                  "Your session has ended. Sign in again to continue using the dashboard.",
                )}</span
              >
              <div class="flex items-center gap-2 shrink-0">
                <Button variant="secondary" size="sm" onclick={goToLogin}
                  >{$tr("Log in")}</Button
                >
              </div>
            </div>
          </Alert>

          <EmptyState
            title={$tr("Dashboard Locked")}
            description={$tr(
              "Your session has ended. All dashboard operations are locked until you sign in again.",
            )}
          >
            {#snippet action()}
              <Button variant="primary" onclick={goToLogin}>
                {$tr("Sign in again")}
              </Button>
            {/snippet}
          </EmptyState>
        </div>
      {:else}
        {#if isDefaultAdminToken && activeTab !== "login"}
          <div class="mb-6">
            <SecurityBanner
              onChangePassword={() => {
                showChangePasswordModal = true;
              }}
            />
          </div>
        {/if}

        {#key activeTab}
          <div class="page-enter">
            {#if pageComponent}
              {@const ActivePage = pageComponent}
              <ActivePage />
            {:else if activeTab === "login"}
              <Login />
            {:else}
              <EmptyState
                title={$tr("Page not found")}
                description={$tr(
                  "This tab does not exist. Pick a page from the sidebar.",
                )}
              >
                {#snippet action()}
                  <Button
                    variant="secondary"
                    onclick={() => (activeTab = "overview")}
                  >
                    {$tr("Back to Overview")}
                  </Button>
                {/snippet}
              </EmptyState>
            {/if}
          </div>
        {/key}
      {/if}
    </main>
  </div>
</div>
