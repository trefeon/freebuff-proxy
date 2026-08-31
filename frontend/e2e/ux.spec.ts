import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';
import { readFileSync } from 'fs';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

type Fixtures = {
  overview: unknown;
  tokens: unknown;
  models: unknown;
  config: unknown;
  configMeta: unknown;
  logs: { entries: Array<{ level: string; message: string; fields?: string }> } & Record<string, unknown>;
  metrics: unknown;
  setup: unknown;
  traces: unknown;
  version: unknown;
  upstreamDrift: unknown;
  authStatus: unknown;
};

function loadFixtures(): Fixtures {
  const dir = join(__dirname, 'fixtures');
  return {
    overview: JSON.parse(readFileSync(join(dir, 'overview.json'), 'utf-8')),
    tokens: JSON.parse(readFileSync(join(dir, 'tokens.json'), 'utf-8')),
    models: JSON.parse(readFileSync(join(dir, 'models.json'), 'utf-8')),
    config: JSON.parse(readFileSync(join(dir, 'config.json'), 'utf-8')),
    configMeta: JSON.parse(readFileSync(join(dir, 'config-meta.json'), 'utf-8')),
    logs: JSON.parse(readFileSync(join(dir, 'logs.json'), 'utf-8')),
    metrics: JSON.parse(readFileSync(join(dir, 'metrics.json'), 'utf-8')),
    setup: JSON.parse(readFileSync(join(dir, 'setup.json'), 'utf-8')),
    traces: JSON.parse(readFileSync(join(dir, 'traces.json'), 'utf-8')),
    version: JSON.parse(readFileSync(join(dir, 'version.json'), 'utf-8')),
    upstreamDrift: JSON.parse(readFileSync(join(dir, 'upstream-drift.json'), 'utf-8')),
    authStatus: JSON.parse(readFileSync(join(dir, 'auth-status.json'), 'utf-8')),
  };
}
// The SPA shell served for /admin/* routes (same file serve-static.mjs
// serves); the login-page mock below fulfills with it so it can also issue
// the fb_csrf double-submit cookie like the real gateway does.
const indexPath = join(__dirname, '../../backend/internal/dashboard/dist/index.html');
function indexHtml(): string {
  return readFileSync(indexPath, 'utf-8');
}

/**
 * Same proven mock harness as dashboard.spec.ts (rebuild of the tokens/
 * login fixtures per test via overrides, never shared mutation). One
 * intentional difference: the mocked successful login also sets the
 * non-HttpOnly fb_csrf double-submit cookie, mirroring the real server
 * (backend/internal/server/admin_auth.go: successful login sets fb_admin + fb_csrf).
 */
async function mockDashboard(
  page: Page,
  fixtures: Fixtures,
  overrides: Partial<Record<keyof Fixtures | 'configWithApiKeys', unknown>> = {}
) {
  // Helpers to pick overridden or base fixture
  const pick = (key: keyof Fixtures) => (overrides[key] ?? (fixtures as Record<string, unknown>)[key]);

  // Overview
  await page.route('**/admin/api/overview', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('overview')),
    });
  });

  // Tokens
  await page.route('**/admin/api/tokens', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('tokens')),
    });
  });

  // Models
  await page.route('**/admin/api/models', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('models')),
    });
  });

  // Traces
  await page.route('**/admin/api/traces', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('traces')),
    });
  });

  // Setup
  await page.route('**/admin/api/setup', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('setup')),
    });
  });

  // Config — Supports override that includes API_KEYS for Tokens parsing test.
  await page.route(/\/admin\/api\/config(\?.*)?$/, async (route) => {
    const cfg = overrides['configWithApiKeys'] ?? pick('config');
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(cfg),
    });
  });

  // Config meta — the Settings page key catalog (JSON array from /admin/api/config/meta).
  await page.route(/\/admin\/api\/config\/meta(\?.*)?$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('configMeta')),
    });
  });

  // Logs — handles ?level= & ?msg= filtering like the real Go handler.
  await page.route('**/admin/api/logs**', async (route) => {
    const url = new URL(route.request().url());
    const level = (url.searchParams.get('level') || '').toLowerCase();
    const msg = (url.searchParams.get('msg') || '').trim();
    const base = pick('logs');
    const logsData = base as unknown as { entries: Array<{ level: string; message: string }> };
    let entries: Array<{ level: string; message: string }> = logsData.entries || [];
    if (level) {
      entries = entries.filter((e) => (e.level || '').toLowerCase() === level);
    }
    if (msg) {
      const low = msg.toLowerCase();
      entries = entries.filter((e) => (e.message || '').toLowerCase().includes(low));
    }
    const body = JSON.stringify({
      enabled: true,
      level: level,
      msg: msg,
      has_filter: !!(level || msg),
      entries,
    });
    await route.fulfill({ status: 200, contentType: 'application/json', body });
  });

  // Metrics
  await page.route('**/admin/api/metrics', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('metrics')),
    });
  });

  // Version
  await page.route('**/admin/api/version', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('version')),
    });
  });

  // Upstream drift
  await page.route('**/admin/api/upstream-drift', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('upstreamDrift')),
    });
  });

  // Auth status
  await page.route('**/admin/api/auth/status', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pick('authStatus')),
    });
  });

  // /admin/login — default success; individual tests may override for 401 case.
  // Login models the real gateway (backend/internal/server/admin_auth.go): the login
  // PAGE response issues the non-HttpOnly fb_csrf double-submit cookie
  // (setCSRFCookieIfAbsent runs on HTML responses), the login POST grants the
  // fb_admin session cookie. Each response carries exactly ONE Set-Cookie:
  // route.fulfill joins multiple values into a single broken cookie.
  await page.route('**/admin/login', async (route) => {
    if (route.request().method() === 'POST') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true }),
        headers: { 'Set-Cookie': 'fb_admin=mock-token; Path=/; HttpOnly; SameSite=Strict' },
      });
    } else {
      await route.fulfill({
        status: 200,
        contentType: 'text/html',
        body: indexHtml(),
        headers: { 'Set-Cookie': 'fb_csrf=mocknonce123; Path=/; SameSite=Strict' },
      });
    }
  });

  // POST /admin/api/change-password
  await page.route('**/admin/api/change-password', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ok: true, message: 'Password changed' }),
    });
  });

  // Also mock POST /admin/config save (Tokens add-token flow uses POST /admin/config with form)
  await page.route(/\/admin\/config$/, async (route) => {
    if (route.request().method() === 'POST') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true, message: 'Config saved' }),
      });
    } else {
      await route.continue();
    }
  });
}

// ---------------------------------------------------------------------------
// Fixture builders (per-test copies — never mutate shared fixtures)
// ---------------------------------------------------------------------------

// Minimal token row mirroring the real /admin/api/tokens row shape.
function tokenRow(idx: number, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    index: idx,
    session_status: 'idle',
    queue_position: 0,
    queue_depth: 0,
    active_runs: 0,
    requests: 0,
    messages_24h: 0,
    daily_limit: 0,
    usage_pct: 0,
    risk_level: 'low',
    cooldown_active: false,
    cooldown_until: '',
    locked: false,
    transient_retries: 1,
    has_standing: false,
    session_instance: '',
    session_model: '',
    session_remaining_seconds: 0,
    has_quota: false,
    ...over,
  };
}

function tokensPayload(tokens: Array<Record<string, unknown>>) {
  return {
      mode: 'pooled',
      in_bridge: false,
      bridge_tokens: 0,
      token_count: tokens.length,
      has_tokens: true,
      tokens,
      bridge_token_cards: [],
  };
}

// Copies the fixture's token rows into fresh objects (never mutate shared
// fixtures) after a runtime shape check.
function tokenRowsOf(value: unknown): Array<Record<string, unknown>> {
  if (typeof value !== 'object' || value === null) return [];
  if (!('tokens' in value)) return [];
  const arr = value.tokens;
  if (!Array.isArray(arr)) return [];
  return arr
      .filter((t): t is Record<string, unknown> => typeof t === 'object' && t !== null)
      .map((t) => ({ ...t }));
}

test.describe('operator UX journey (hermetic mocks)', () => {
  // The Settings page catalog render is heavy; under parallel workers on slow
  // runners the default 5s expect window can be tight (same as the dashboard
  // suite), so give this whole group a wider one.
  test.use({ expect: { timeout: 10_000 } });

  // ---------------------------------------------------------------------------
  // 1. Login: wrong token surfaces the server error (no crash, stays on login)
  // ---------------------------------------------------------------------------
  test('login: wrong admin token surfaces the server error and stays on login', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // 401 with JSON error, like the real gateway.
    await page.unroute('**/admin/login');
    await page.route('**/admin/login', async (route) => {
      if (route.request().method() === 'POST') {
        await route.fulfill({
          status: 401,
          contentType: 'application/json',
          body: JSON.stringify({ error: 'Invalid admin token.' }),
        });
      } else {
        await route.continue();
      }
    });

    // Register the waits before triggering (single-fetch race: a wait
    // registered after the click can miss an already-resolved response).
    await page.goto('http://127.0.0.1:4173/admin/login');
    const tokenInput = page.locator('#token');
    await expect(tokenInput).toBeVisible();
    await tokenInput.fill('wrong-token');
    await page.getByRole('button', { name: 'Sign in' }).click();

    // Error surfaced from the 401 body; app did not crash or navigate away.
    await expect(page.getByText('Invalid admin token.')).toBeVisible();
    expect(page.url()).toContain('/admin/login');
    await expect(tokenInput).toBeVisible();
    await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible();
    // No session cookie was granted for a rejected login.
    const cookies = await page.context().cookies('http://127.0.0.1:4173');
    expect(cookies.find((c) => c.name === 'fb_admin')).toBeUndefined();
  });

  // ---------------------------------------------------------------------------
  // 2. Login: correct token signs in and the dashboard renders
  // ---------------------------------------------------------------------------
  test('login: correct admin token signs in, sets cookies and renders the dashboard', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Register before triggering: the overview fetch can resolve during the
    // post-login navigation.
    const overviewResp = page.waitForResponse(
      (r) => r.url().includes('/admin/api/overview') && r.status() === 200,
      { timeout: 10_000 }
    );
    await page.goto('http://127.0.0.1:4173/admin/login');
    await page.locator('#token').fill('correct-admin-token');
    await page.getByRole('button', { name: 'Sign in' }).click();

    // App redirects to the dashboard root and it renders from the mocks.
    await page.waitForURL('**/admin', { timeout: 10_000 });
    await overviewResp;
    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible();
    await expect(page.getByText('Pool total')).toBeVisible();

    // The login response granted the session + double-submit CSRF cookies.
    const cookies = await page.context().cookies('http://127.0.0.1:4173');
    const names = cookies.map((c) => c.name);
    expect(names).toContain('fb_admin');
    expect(names).toContain('fb_csrf');
    expect(cookies.find((c) => c.name === 'fb_csrf')?.httpOnly).toBe(false);
  });

  // ---------------------------------------------------------------------------
  // 3. Add token: invalid format is rejected client-side, no POST at all
  // ---------------------------------------------------------------------------
  test('tokens: add-token invalid format errors client-side and never POSTs', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    let addPosts = 0;
    page.on('request', (req) => {
      if (req.method() === 'POST' && req.url().includes('/admin/tokens/add')) addPosts++;
    });

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();

    const input = page.locator('#add-token-input');
    await input.fill('short');
    // Client-side validation error + disabled submit (no server round trip possible).
    await expect(page.getByText('Token must be at least 10 characters and must not contain spaces, commas, or Bearer prefix')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Add Token' })).toBeDisabled();

    // Enter in the form reaches the submit guard too — still no POST.
    await input.press('Enter');
    await expect(page.locator('#add-token-input')).toHaveValue('short');
    expect(addPosts).toBe(0);
  });

  // ---------------------------------------------------------------------------
  // 4. Add token: valid cb_… token POSTs the value and shows a success alert
  // ---------------------------------------------------------------------------
  test('tokens: add-token valid format POSTs the token value and shows success', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    let postedBody = '';
    await page.route('**/admin/tokens/add', async (route) => {
      postedBody = route.request().postData() || '';
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true, message: 'Token added to pool and saved to .env.' }),
      });
    });
    // Real-world UUID / session token format.
    const validToken = '550e8400-e29b-41d4-a716-446655440000';

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();

    const input = page.locator('#add-token-input');
    await input.fill(validToken);
    await expect(page.getByText('Valid format')).toBeVisible();

    const addReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/add'));
    await page.getByRole('button', { name: 'Add Token' }).click();
    await addReq;

    // The exact token value traveled as JSON {token: ...}.
    expect(JSON.parse(postedBody)).toEqual({ token: validToken });
    // Success alert renders and the form is cleared.
    await expect(page.getByText('Token added to pool and saved to .env.')).toBeVisible();
    await expect(input).toHaveValue('');
  });

  // ---------------------------------------------------------------------------
  // 5. REGRESSION: removing a middle (non-last) row POSTs its own index and
  //    the previously-last token remains.
  //    Pins the by-index removal fix (pool.RemoveTokenAt) against the old
  //    remove-last behavior.
  // ---------------------------------------------------------------------------
  test('tokens: remove of a middle row posts its own index and keeps the last token (regression)', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    const state = { tokens: [tokenRow(0), tokenRow(1), tokenRow(2)] };
    await page.unroute('**/admin/api/tokens');
    await page.route('**/admin/api/tokens', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(tokensPayload(state.tokens)) });
    });

    let removeBody = '';
    await page.route('**/admin/tokens/remove', async (route) => {
      removeBody = route.request().postData() || '';
      const idx = JSON.parse(removeBody || '{}').token as number;
      state.tokens = state.tokens.filter((t) => t.index !== idx);
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true, message: `Token ${idx} removed from the pool and .env.` }),
      });
    });

    // Waits before the trigger: the refetch after removal races nothing.
    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.locator('table tbody tr')).toHaveCount(3);
    await expect(page.getByText('#1', { exact: true })).toBeVisible();

    const removeReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/remove'));
    const refetch = page.waitForResponse((r) => r.url().includes('/admin/api/tokens') && r.status() === 200);
    const rowToRemove = page.locator('table tbody tr').filter({ hasText: '#1' });
    page.once('dialog', (d) => d.accept());
    await rowToRemove.getByRole('button', { name: 'Remove', exact: true }).click();
    const req = await removeReq;

    // Row index 1 (the middle row — NOT the last) travels as token=1.
    expect(JSON.parse(req.postData() || '{}')).toEqual({ token: 1 });
    await refetch;
    await expect(page.getByText('Token 1 removed from the pool and .env.')).toBeVisible();
    await expect(page.locator('table tbody tr')).toHaveCount(2);
    // The middle row is gone; the previously-last token (index 2) remains.
    await expect(page.getByText('#1', { exact: true })).toHaveCount(0);
    await expect(page.getByText('#2', { exact: true })).toBeVisible();
  });

  // ---------------------------------------------------------------------------
  // 6. Lock/unlock: per-action endpoints, per-action POST bodies, status flips
  // ---------------------------------------------------------------------------
  test('tokens: lock and unlock post per-action endpoints and flip the row state', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    const state = { tokens: [tokenRow(0), tokenRow(1)] };
    await page.unroute('**/admin/api/tokens');
    await page.route('**/admin/api/tokens', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(tokensPayload(state.tokens)) });
    });
    await page.route('**/admin/tokens/0/lock', async (route) => {
      state.tokens[0].locked = true;
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, message: 'Token 0 locked' }) });
    });
    await page.route('**/admin/tokens/0/unlock-lock', async (route) => {
      state.tokens[0].locked = false;
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, message: 'Token 0 unlocked' }) });
    });

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    const row0 = page.locator('table tbody tr').filter({ hasText: '#0' });
    await expect(page.locator('table tbody tr')).toHaveCount(2);
    await expect(row0.getByRole('button', { name: 'Lock' })).toBeVisible();

    // Lock: POST /admin/tokens/0/lock with an empty JSON body.
    const lockReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/0/lock'));
    const lockedRefetch = page.waitForResponse((r) => r.url().includes('/admin/api/tokens') && r.status() === 200);
    page.once('dialog', (d) => d.accept());
    await row0.getByRole('button', { name: 'Lock' }).click();
    const lock = await lockReq;
    expect(lock.postData()).toBe('{}');
    await lockedRefetch;

    // Row reflects the locked state: status badge + Unlock action.
    await expect(row0.getByRole('button', { name: 'Unlock' })).toBeVisible();
    await expect(row0.getByText('locked')).toBeVisible();

    // Unlock: POST /admin/tokens/0/unlock-lock (its own endpoint, not /lock).
    const unlockReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/0/unlock-lock'));
    const unlockedRefetch = page.waitForResponse((r) => r.url().includes('/admin/api/tokens') && r.status() === 200);
    page.once('dialog', (d) => d.accept());
    await row0.getByRole('button', { name: 'Unlock' }).click();
    const unlock = await unlockReq;
    expect(unlock.postData()).toBe('{}');
    await unlockedRefetch;

    await expect(row0.getByRole('button', { name: 'Lock' })).toBeVisible();
    await expect(row0.getByText('locked')).toHaveCount(0);
  });

  // ---------------------------------------------------------------------------
  // 7. Device Login: POST /admin/login/start, alert carries URL + copy button
  // ---------------------------------------------------------------------------
  test('tokens: device login starts the wizard, surfaces the login URL and copy button', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    const loginUrl = 'https://freebuff.app/device/login?code=fp-demo-0001';
    await page.route('**/admin/login/start', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ fingerprint: 'fp-demo-0001', login_url: loginUrl }),
      });
    });
    // Device wizard stays pending so the alert remains stable for assertions.
    await page.route('**/admin/login/status**', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ status: 'pending' }) });
    });

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();

    const startReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/login/start'));
    await page.getByRole('button', { name: 'Device Login' }).click();
    await startReq;

    // Status alert shows the URL, an Open link and the copy button.
    await expect(page.getByText('Open this URL in your browser to sign in:')).toBeVisible();
    await expect(page.getByText(loginUrl, { exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Copy link' })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Open' })).toHaveAttribute('href', loginUrl);
  });

  // ---------------------------------------------------------------------------
  // 8. CSRF: every admin POST carries X-CSRF-Token once fb_csrf is present
  //    (add / remove / lock / config save)
  // ---------------------------------------------------------------------------
  test('security: admin POSTs carry the X-CSRF-Token header after login', async ({ page }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content: 'LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n',
      has_env_file: true,
      effective: [
        { key: 'LISTEN_ADDR', value: '127.0.0.1:3457', secret: false },
        { key: 'AUTH_TOKENS', value: '2 token(s)', secret: true },
        { key: 'API_KEYS', value: '1 key(s)', secret: true },
        { key: 'ADMIN_TOKEN', value: 'set', secret: true },
        { key: 'SAFE_MODE', value: 'true', secret: false },
        { key: 'LOG_LEVEL', value: 'info', secret: false },
        { key: 'COST_MODE', value: 'free', secret: false },
        { key: 'MAX_MESSAGES_PER_DAY', value: '0', secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });

    // Sign in first; the login response carries fb_admin + fb_csrf.
    await page.goto('http://127.0.0.1:4173/admin/login');
    const backToDash = page.waitForResponse(
      (r) => r.url().includes('/admin/api/overview') && r.status() === 200,
      { timeout: 10_000 }
    );
    await page.locator('#token').fill('csrf-check-token');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await page.waitForURL('**/admin', { timeout: 10_000 });
    await backToDash;

    const cookies = await page.context().cookies('http://127.0.0.1:4173');
    const csrfCookie = cookies.find((c) => c.name === 'fb_csrf');
    expect(csrfCookie?.value).toBe('mocknonce123');

    // Collect every admin POST after login (login itself is excluded).
    const posts: Array<{ path: string; header: string | undefined }> = [];
    page.on('request', (req) => {
      if (req.method() !== 'POST') return;
      const p = new URL(req.url()).pathname;
      if (!p.startsWith('/admin') || p === '/admin/login') return;
      posts.push({ path: p, header: req.headers()['x-csrf-token'] });
    });

    // Stateful tokens so add/remove/lock keep the table consistent.
    const state = { tokens: tokenRowsOf(f.tokens) };
    await page.unroute('**/admin/api/tokens');
    await page.route('**/admin/api/tokens', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(tokensPayload(state.tokens)) });
    });
    await page.route('**/admin/tokens/add', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, message: 'Token added to pool and saved to .env.' }) });
    });
    await page.route('**/admin/tokens/1/lock', async (route) => {
      const t = state.tokens.find((x) => x.index === 1);
      if (t) t.locked = true;
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, message: 'Token 1 locked' }) });
    });
    await page.route('**/admin/tokens/remove', async (route) => {
      const idx = JSON.parse(route.request().postData() || '{}').token as number;
      state.tokens = state.tokens.filter((t) => t.index !== idx);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, message: `Token ${idx} removed from the pool and .env.` }) });
    });

    // --- add token (POST /admin/tokens/add) ---
    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();
    const addToken = 'cb_' + 'y'.repeat(24);
    const addReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/add'));
    await page.locator('#add-token-input').fill(addToken);
    await page.getByRole('button', { name: 'Add Token' }).click();
    await addReq;
    await expect(page.getByText('Token added to pool and saved to .env.')).toBeVisible();

    // --- lock token 1 (POST /admin/tokens/1/lock) ---
    const row1 = page.locator('table tbody tr').filter({ hasText: '#1' });
    const lockReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/1/lock'));
    page.once('dialog', (d) => d.accept());
    await row1.getByRole('button', { name: 'Lock' }).click();
    await lockReq;

    // --- remove token 0 (POST /admin/tokens/remove) ---
    const row0 = page.locator('table tbody tr').filter({ hasText: '#0' });
    const removeReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/tokens/remove'));
    page.once('dialog', (d) => d.accept());
    await row0.getByRole('button', { name: 'Remove' }).click();
    await removeReq;

    // --- config save (POST /admin/config) ---
    const metaResp = page.waitForResponse((r) => r.url().includes('/admin/api/config/meta') && r.status() === 200);
    await page.goto('http://127.0.0.1:4173/admin/#settings');
    await metaResp;
    const safeMode = page.getByRole('checkbox', { name: 'SAFE_MODE' });
    await expect(safeMode).toBeChecked();
    await safeMode.uncheck();
    const configReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/config'));
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Save' }).click();
    await configReq;

    // Every recorded admin POST carried the matching X-CSRF-Token.
    const expectedPaths = ['/admin/tokens/add', '/admin/tokens/1/lock', '/admin/tokens/remove', '/admin/config'];
    const seenPaths = posts.map((p) => p.path);
    for (const p of expectedPaths) {
      expect(seenPaths).toContain(p);
    }
    for (const post of posts) {
      if (!expectedPaths.includes(post.path)) continue;
      expect(post.header, `X-CSRF-Token missing on ${post.path}`).toBe('mocknonce123');
    }
  });

  // ---------------------------------------------------------------------------
  // 9. Logout: POST /admin/logout clears the session; the app surfaces the
  //    session-expired banner and login recovery (no page reload, issue #197)
  // ---------------------------------------------------------------------------
  test('session: logout POST clears the session and the app surfaces login recovery', async ({ page, context }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Establish a session (cookies the login flow would have granted).
    await context.addCookies([
      { name: 'fb_admin', value: 'mock-token', domain: '127.0.0.1', path: '/' },
      { name: 'fb_csrf', value: 'logoutnonce1', domain: '127.0.0.1', path: '/' },
    ]);

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();

    // Logout answers ok:true and expires the session cookie (like the Go
    // handler; the fb_csrf cookie is left intact, same as the server).
    await page.route('**/admin/logout', async (route) => {
      if (route.request().method() === 'POST') {
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ ok: true }),
          headers: { 'Set-Cookie': 'fb_admin=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0' },
        });
      } else {
        await route.continue();
      }
    });
    // After logout the sensitive API is dead: 401s.
    await page.unroute('**/admin/api/tokens');
    await page.route('**/admin/api/tokens', async (route) => {
      await route.fulfill({ status: 401, contentType: 'application/json', body: JSON.stringify({ error: { message: 'Unauthorized' } }) });
    });

    // The next background poll (10s interval) must observe the 401 without
    // navigating; register the wait before triggering the poll.
    const deadResp = page.waitForResponse(
      (r) => r.url().includes('/admin/api/tokens') && r.status() === 401,
      { timeout: 15_000 }
    );

    // POST /admin/logout with the CSRF header, exactly as the SPA's postForm
    // client would (csrfHeader('POST') for the logged-in session).
    const status = await page.evaluate(async () => {
      const m = document.cookie.match(/(?:^|;\s*)fb_csrf=([^;]*)/);
      const csrf = m ? decodeURIComponent(m[1]) : '';
      const res = await fetch('/admin/logout', {
        method: 'POST',
        headers: csrf ? { 'X-CSRF-Token': csrf } : {},
      });
      return res.status;
    });
    expect(status).toBe(200);
    // Session cookie is gone.
    const afterLogout = await context.cookies('http://127.0.0.1:4173');
    expect(afterLogout.find((c) => c.name === 'fb_admin')).toBeUndefined();

    await deadResp;
    // App surfaces the session-expired banner with a recovery action.
    await expect(page.getByText('Session expired')).toBeVisible();
    await expect(page.getByText('Your session has ended. Sign in again to continue using the dashboard.')).toBeVisible();
    await page.getByRole('button', { name: 'Log in' }).click();
    // Recovery is the login view (hash-only navigation, no reload).
    await expect(page.locator('#token')).toBeVisible();
    expect(page.url()).toContain('/admin/#login');
  });

  // ---------------------------------------------------------------------------
  // 10. Quota Tracker: pooled tokens without quota data render the hint
  // ---------------------------------------------------------------------------
  test('quota: pooled tokens without quota data render the no-premium placeholder', async ({ page }) => {
    const f = loadFixtures();
    // Two pooled tokens, neither carrying premium_quota (or session quota).
    await mockDashboard(page, f, {
      tokens: tokensPayload([tokenRow(0, { has_quota: true, quota: [] }), tokenRow(1, { has_quota: true, quota: [] })]),
    });

    await page.goto('http://127.0.0.1:4173/admin/#quota');
    await expect(page.getByRole('heading', { name: 'Quota Tracker', exact: true })).toBeVisible();

    // Tokens are pooled, so per-token cards render (not the empty pool state).
    await expect(page.getByRole('heading', { name: 'Token #0' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Token #1' })).toBeVisible();
    await expect(page.getByText('No tokens in pool')).toHaveCount(0);

    // No quota data yet: the per-token hint renders for every pooled token.
    await expect(page.getByText('No premium quota data — run a request or -test-token to populate.')).toHaveCount(2);
    await expect(page.getByText('No quota data available for this session.')).toHaveCount(2);
  });

  // ---------------------------------------------------------------------------
  // 11. Settings: the raw .env editor inside <details> still validates and
  //     save posts the edited content
  // ---------------------------------------------------------------------------
  test('settings: advanced raw .env editor validates the edits and save posts them', async ({ page }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content: 'LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n',
      has_env_file: true,
      effective: [
        { key: 'LISTEN_ADDR', value: '127.0.0.1:3457', secret: false },
        { key: 'AUTH_TOKENS', value: '2 token(s)', secret: true },
        { key: 'API_KEYS', value: '1 key(s)', secret: true },
        { key: 'ADMIN_TOKEN', value: 'set', secret: true },
        { key: 'SAFE_MODE', value: 'true', secret: false },
        { key: 'LOG_LEVEL', value: 'info', secret: false },
        { key: 'COST_MODE', value: 'free', secret: false },
        { key: 'MAX_MESSAGES_PER_DAY', value: '0', secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });

    const metaResp = page.waitForResponse((r) => r.url().includes('/admin/api/config/meta') && r.status() === 200);
    await page.goto('http://127.0.0.1:4173/admin/#settings');
    await metaResp;
    await expect(page.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();

    // Raw editor lives behind the <details> toggle; open it and edit directly.
    await page.getByText('Advanced: raw .env editor').click();
    const editor = page.locator('#config-env');
    await expect(editor).toBeVisible();
    const editedEnv =
      'LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=debug\nCOST_MODE=free\n';
    await editor.fill(editedEnv);
    await expect(editor).toHaveValue(editedEnv);

    // Client-side validation passes for the edited document.
    await page.getByRole('button', { name: 'Validate' }).click();
    await expect(page.getByText(/Configuration is valid/)).toBeVisible();

    // Save posts the edited raw content (form POST, dialog accept required).
    const saveReq = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/config'));
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Save' }).click();
    const req = await saveReq;
    const body = decodeURIComponent(req.postData() || '');
    expect(body).toContain('content=');
    expect(body).toContain('LOG_LEVEL=debug');
    expect(body).toContain('SAFE_MODE=true');
    await expect(page.getByText('Config saved')).toBeVisible();
  });
});
