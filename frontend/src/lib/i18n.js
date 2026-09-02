/**
 * Dependency-free English-only translator for the dashboard (issue #156).
 *
 * Keys ARE the strings: the dashboard renders readable English directly and
 * no dictionary can drift out of sync. (Simplified-Chinese support was
 * removed — see git history for the old zh dictionary.)
 *
 * Usage in components:
 *   import { tr } from '../lib/i18n.js';
 *   {$tr('Overview')}                    // reactive store value = the function
 *   {$tr('banned until {time}', { time })}
 */

import { derived, writable } from "svelte/store";

// Fixed English locale. Kept as a writable so $tr stays a store (the
// auto-subscription syntax in components is unchanged) — there is no
// language switching anymore.
const locale = writable("en");

function interpolate(template, params) {
  if (!params) return template;
  return template.replace(/\{(\w+)\}/g, (match, name) =>
    params[name] !== undefined ? String(params[name]) : match,
  );
}

/** English-only translator store: keys resolve to themselves. */
export const tr = derived(
  locale,
  () => (key, params) => interpolate(key, params),
);
