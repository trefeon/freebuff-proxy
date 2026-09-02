import {
  LayoutDashboard,
  Key,
  Gauge,
  Cpu,
  Settings as SettingsIcon,
  FileText,
  FlaskConical,
} from "@lucide/svelte";
import Overview from "./pages/Overview.svelte";
import Tokens from "./pages/Tokens.svelte";
import QuotaTracker from "./pages/QuotaTracker.svelte";
import Models from "./pages/Models.svelte";
import Settings from "./pages/Settings.svelte";
import Logs from "./pages/Logs.svelte";
import DevTools from "./pages/DevTools.svelte";
import Setup from "./pages/Setup.svelte";
import Metrics from "./pages/Metrics.svelte";
import Traces from "./pages/Traces.svelte";

// Single source of truth for the dashboard page set (issue #290). Both the
// sidebar's tab list (lib/Sidebar.svelte) and App.svelte's page mount derive
// from this array, so a page added here appears consistently everywhere.
//
// Fields:
//   id         hash/path segment that selects the page
//   component  the page component to mount
//   label      sidebar label (optional for deep-link-only pages)
//   icon       sidebar icon (optional - sidebar items always carry one)
//   gate       optional gate key; the Sidebar filters 'devtools' behind the
//              DEVTOOLS_ENABLED env predicate (utils/devtools.js)
//   inSidebar  false for pages reachable by URL but not listed in the sidebar
export const NAV_ITEMS = [
  {
    id: "overview",
    component: Overview,
    label: "Overview",
    icon: LayoutDashboard,
  },
  { id: "tokens", component: Tokens, label: "Tokens", icon: Key },
  { id: "quota", component: QuotaTracker, label: "Quota Tracker", icon: Gauge },
  { id: "models", component: Models, label: "Models", icon: Cpu },
  {
    id: "settings",
    component: Settings,
    label: "Settings",
    icon: SettingsIcon,
  },
  { id: "logs", component: Logs, label: "Logs", icon: FileText },
  {
    id: "devtools",
    component: DevTools,
    label: "Dev Tools",
    icon: FlaskConical,
    gate: "devtools",
  },
  // Deep-link-only pages: reachable by URL but never listed in the sidebar.
  { id: "setup", component: Setup, inSidebar: false },
  { id: "metrics", component: Metrics, inSidebar: false },
  { id: "traces", component: Traces, inSidebar: false },
  // The gateway serves /admin/playground; it renders the same DevTools page
  // (which self-gates when DEVTOOLS_ENABLED is off). Kept as a separate id so
  // a deep link to /admin/playground still mounts DevTools after the registry
  // consolidation.
  { id: "playground", component: DevTools, inSidebar: false },
];

/**
 * Resolve a page id (hash or path segment) to its component, or null when the
 * id is not a known page (the App shell renders a NotFound fallback).
 * @param {string} id
 * @returns {any | null}
 */
export function pageComponentFor(id) {
  return NAV_ITEMS.find((n) => n.id === id)?.component ?? null;
}
