import {
  LayoutDashboard,
  Key,
  Cpu,
  Settings as SettingsIcon,
  FileText,
  FlaskConical,
  AlertTriangle,
} from "@lucide/svelte";
import Overview from "./pages/Overview.svelte";
import Tokens from "./pages/Tokens.svelte";
import Plans from "./pages/Plans.svelte";
import Activity from "./pages/Activity.svelte";
import Settings from "./pages/Settings.svelte";
import DevTools from "./pages/DevTools.svelte";
import Review from "./pages/Review.svelte";

// Single source of truth for the dashboard page set (issue #290, dashboard
// IA Phase 2: 11 page ids collapsed to 6). Both the sidebar's tab list
// (lib/Sidebar.svelte) and App.svelte's page mount derive from this array,
// so a page added here appears consistently everywhere.
//
// Fields:
//   id         hash/path segment that selects the page
//   component  the page component to mount
//   label      sidebar label (optional for deep-link-only pages)
//   icon       sidebar icon (optional - sidebar items always carry one)
//   gate       optional gate key; the Sidebar filters 'devtools' behind the
//              DEVTOOLS_ENABLED env predicate (utils/devtools.js)
//   inSidebar  false for pages reachable by URL but not listed in the sidebar
//
// Removed pages redirect via LEGACY_PAGE_REDIRECTS below (consumed by
// App.svelte): setup -> overview, maturity -> tokens/warming,
// quota+models -> plans, logs+metrics+traces -> activity,
// playground -> devtools, config -> settings.
export const NAV_ITEMS = [
  {
    id: "overview",
    component: Overview,
    label: "Overview",
    icon: LayoutDashboard,
  },
  { id: "tokens", component: Tokens, label: "Tokens", icon: Key },
  { id: "plans", component: Plans, label: "Plans", icon: Cpu },
  { id: "activity", component: Activity, label: "Activity", icon: FileText },
  {
    id: "settings",
    component: Settings,
    label: "Settings",
    icon: SettingsIcon,
  },
  {
    id: "devtools",
    component: DevTools,
    label: "Dev Tools",
    icon: FlaskConical,
    gate: "devtools",
  },
  // REVIEW TEMP - temporary show-all page, WILL BE DELETED. Always visible,
  // no gate, inSidebar true so the owner can click through everything.
  {
    id: "review",
    component: Review,
    label: "REVIEW-TEMP",
    icon: AlertTriangle,
  },
];

/**
 * Legacy page ids removed by the dashboard IA merge, mapped to their
 * redirect target. A string value redirects to that page with no tab
 * one-shot; an object also carries the tab the target page opens on
 * (delivered one-shot via sessionStorage "fp-page-tab:<page>").
 */
export const LEGACY_PAGE_REDIRECTS = {
  setup: "overview",
  maturity: { page: "tokens", tab: "warming" },
  quota: { page: "plans", tab: "accounts" },
  models: { page: "plans", tab: "models" },
  logs: "activity",
  metrics: { page: "activity", tab: "metrics" },
  traces: { page: "activity", tab: "traces" },
  playground: "devtools",
  config: "settings",
};

/**
 * Resolve a page id (hash or path segment) to its redirect target when the
 * id is a removed legacy page, or null when it is not a legacy id.
 * @param {string} id
 * @returns {{ page: string, tab: string | null } | null}
 */
export function resolveLegacyPage(id) {
  const target = LEGACY_PAGE_REDIRECTS[id];
  if (!target) return null;
  if (typeof target === "string") return { page: target, tab: null };
  return { page: target.page, tab: target.tab ?? null };
}

/**
 * Resolve a page id (hash or path segment) to its component, or null when the
 * id is not a known page (the App shell renders a NotFound fallback).
 * @param {string} id
 * @returns {any | null}
 */
export function pageComponentFor(id) {
  return NAV_ITEMS.find((n) => n.id === id)?.component ?? null;
}
