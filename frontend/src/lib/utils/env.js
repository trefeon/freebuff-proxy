// Shared .env line-edit contract (issue #234): one implementation of the
// KEY=VALUE find/replace/append rules used by the Settings editor and the
// quick actions (API_KEYS, TOKEN_ROTATION, DEVTOOLS_ENABLED). Mirrors the
// Go contract in backend/internal/config (ApplyEnvUpdates): a key line is
// replaced in place, a missing key is appended after the trailing newline
// (if any), comments and blank lines are preserved.

/**
 * Parses a .env document into a KEY -> VALUE map. Blank lines and #
 * comments are skipped; the value is trimmed after the first '='.
 * @param {string} content
 * @returns {Record<string, string>}
 */
export function parseEnv(content) {
  const map = {};
  for (const line of (content || "").split("\n")) {
    const t = line.trim();
    if (!t || t.startsWith("#")) continue;
    const eq = t.indexOf("=");
    if (eq === -1) continue;
    map[t.slice(0, eq).trim()] = t.slice(eq + 1).trim();
  }
  return map;
}

/**
 * Returns the value of one .env key, or null when absent.
 * @param {string} content
 * @param {string} key
 * @returns {string | null}
 */
export function getEnvValue(content, key) {
  const map = parseEnv(content);
  return Object.prototype.hasOwnProperty.call(map, key) ? map[key] : null;
}

function escapeRegex(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Returns a new .env document with `key=value` set: replaces the existing
 * KEY= line in place, or appends the line at the end when absent.
 * @param {string} content
 * @param {string} key
 * @param {string} value
 * @returns {string}
 */
export function setEnvValue(content, key, value) {
  const re = new RegExp(`^\\s*${escapeRegex(key)}=.*$`, "m");
  const line = `${key}=${value}`;
  if (re.test(content)) return content.replace(re, line);
  if (!content) return line;
  return content.endsWith("\n") ? content + line : content + "\n" + line;
}
