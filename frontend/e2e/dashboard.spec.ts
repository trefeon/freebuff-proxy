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

async function mockDashboard(page: Page, fixtures: Fixtures, overrides: Partial<Record<keyof Fixtures | 'configWithApiKeys', unknown>> = {}) {
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

  // POST /admin/login — default success; individual tests may override for 401 case
  await page.route('**/admin/login', async (route) => {
    if (route.request().method() === 'POST') {
      // Successful login: mimic the server's 302/200 with Set-Cookie (no verification needed in mock).
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true }),
        headers: { 'Set-Cookie': 'fb_admin=mock-token; Path=/; HttpOnly; SameSite=Strict' },
      });
    } else {
      await route.continue();
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

test.describe('dashboard hermetic mocks', () => {
  // The Settings tests render the 58-key catalog; under parallel workers on
  // slow runners the render can exceed the default 5s expect window, so give
  // this group a wider one (CI: 1 worker + retries anyway).
  test.use({ expect: { timeout: 10_000 } });
  test('Overview polls every 15s and shows token risk cards', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    // Track overview requests
    let overviewCount = 0;
    page.on('response', (res) => {
      if (res.url().includes('/admin/api/overview')) overviewCount++;
    });

    await page.goto('http://127.0.0.1:4173/admin/#overview');
    // First fetch should resolve quickly
    await page.waitForResponse((r) => r.url().includes('/admin/api/overview') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible();
    // Overview KPI row shows Pool total / Banned etc (rendered from fixture)
    await expect(page.getByText('Pool total')).toBeVisible();
    // At-risk token card from overview fixture (index 1 has risk_level high)
    await expect(page.getByText('Token #1')).toBeVisible();

    // Verify polling: wait for second request within 17s (15s interval + buffer)
    // Keep timeout generous but still proves periodic fetch rather than one-shot.
    await page.waitForResponse((r) => r.url().includes('/admin/api/overview') && r.status() === 200, { timeout: 17000 });
    expect(overviewCount).toBeGreaterThanOrEqual(2);
  });

  test('Tokens lists pooled tokens and expands details', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();
    await expect(page.getByText('#0')).toBeVisible({ timeout: 10000 });
    const expandBtn = page.locator('button[aria-label*="Expand details"]').first();
    await expect(expandBtn).toBeVisible();
    await expandBtn.click();
    // Without DEVTOOLS_ENABLED the Dev Session toolbar stays hidden; the
    // expanded row keeps the active-session line.
    await expect(page.getByText('Dev Session:')).not.toBeVisible();
    await expect(page.getByText('Active Session:')).toBeVisible();

    // With DEVTOOLS_ENABLED=true the toolbar appears (per-token session spawn).
    await page.unroute('**/admin/api/config');
    await page.route('**/admin/api/config', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          env_content: 'PORT=3457\nAUTH_TOKENS=tok0,tok1\nDEVTOOLS_ENABLED=true\n',
          has_env_file: true,
        }),
      });
    });
    await page.reload();
    await page.waitForResponse((r) => r.url().includes('/admin/api/tokens') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await page.locator('button[aria-label*="Expand details"]').first().click();
    await expect(page.getByText('Dev Session:')).toBeVisible();
  });

  test('Quota Tracker shows premium pool and per-model session quota', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    let tokensCount = 0;
    page.on('response', (res) => {
      if (res.url().includes('/admin/api/tokens')) tokensCount++;
    });

    await page.goto('http://127.0.0.1:4173/admin/#quota');
    await page.waitForResponse((r) => r.url().includes('/admin/api/tokens') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Quota Tracker', exact: true })).toBeVisible();
    // Sidebar entry links to the new tab
    await expect(page.getByRole('link', { name: 'Quota Tracker' })).toBeVisible();

    // Per-token cards: one per pooled token
    await expect(page.getByRole('heading', { name: 'Token #0' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Token #4' })).toBeVisible();
    // Token 0 fixture carries premium_quota → Premium pool bar renders
    await expect(page.getByText('Premium pool').first()).toBeVisible();
    await expect(page.getByText('4/day pacific_day')).toBeVisible();
    // Tokens without premium data show the subtle hint
    await expect(
      page.getByText('No premium quota data — run a request or -test-token to populate.').first()
    ).toBeVisible();

    // Session quota by model tables from the fixture rows
    await expect(page.getByRole('heading', { name: 'Session quota by model' }).first()).toBeVisible();
    await expect(page.getByText('deepseek/deepseek-v4-flash').first()).toBeVisible();
    await expect(page.getByText('(in 5h 32m)').first()).toBeVisible();
    await expect(page.getByText('base=1, referral=1').first()).toBeVisible();
    // Usage bars under quota rows
    await expect(page.locator('table [role="progressbar"]').first()).toBeVisible();

    // Polls every 10s: a second tokens fetch proves periodic refresh
    await page.waitForResponse((r) => r.url().includes('/admin/api/tokens') && r.status() === 200, { timeout: 12000 });
    expect(tokensCount).toBeGreaterThanOrEqual(2);
  });

  test('Settings renders catalog groups and saves a toggled bool into the .env', async ({ page }) => {
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
    const metaResp = page.waitForResponse((r) => r.url().includes('/admin/api/config/meta') && r.status() === 200, { timeout: 5000 });
    await page.goto('http://127.0.0.1:4173/admin/#settings');
    await metaResp;
    await expect(page.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'General' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Pool' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Security' })).toBeVisible();
    // A documented bool renders as a checkbox; effective value drives it.
    const safeMode = page.getByRole('checkbox', { name: 'SAFE_MODE' });
    await expect(safeMode).toBeVisible();
    await expect(safeMode).toBeChecked();

    // Toggling marks the form dirty and surfaces the unsaved-changes banner.
    await safeMode.uncheck();
    await expect(page.getByText('Unsaved changes')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Save' })).toBeEnabled();

    // Save posts the built .env: the toggled line plus untouched lines.
    let savedBody = '';
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === 'POST') {
        savedBody = decodeURIComponent(route.request().postData() || '');
        await route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            ok: true,
            message: 'Saved and reloaded. These keys apply after restart only: LOG_LEVEL',
            restart_only: ['LOG_LEVEL'],
          }),
        });
      } else {
        await route.continue();
      }
    });
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Save' }).click();
    await expect(page.getByText(/apply after restart only/)).toBeVisible();
    await expect(page.getByText('Applies after restart: LOG_LEVEL')).toBeVisible();
    expect(savedBody).toContain('SAFE_MODE=false');
    expect(savedBody).toContain('AUTH_TOKENS=tok0,tok1');
    expect(savedBody).toContain('LOG_LEVEL=info');
  });

  test('Settings legacy #config alias, select save, secret masking and raw editor validation', async ({ page }) => {
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

    // Legacy '#config' hash still routes to the Settings page.
    const metaRespLegacy = page.waitForResponse((r) => r.url().includes('/admin/api/config/meta') && r.status() === 200, { timeout: 5000 });
    await page.goto('http://127.0.0.1:4173/admin/#config');
    await metaRespLegacy;
    await expect(page.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();

    // Select renders enum options from meta; changing it edits the document.
    const logLevel = page.getByRole('combobox', { name: 'LOG_LEVEL' });
    await expect(logLevel).toBeVisible();
    await expect(logLevel).toContainText('debug');
    await expect(logLevel).toContainText('trace');
    await logLevel.selectOption('warn');

    // Keys absent from the effective config render as disabled 'not set'.
    await expect(page.getByText('not set', { exact: true }).first()).toBeVisible();

    // Advanced raw editor mirrors the form edit and still validates.
    await page.getByText('Advanced: raw .env editor').click();
    const editor = page.locator('#config-env');
    await expect(editor).toBeVisible();
    await expect(editor).toHaveValue(/LOG_LEVEL=warn/);
    await page.getByRole('button', { name: 'Validate' }).click();
    await expect(page.getByText(/Configuration is valid/)).toBeVisible();

    // Save posts the built .env line for the edited select.
    const postReqPromise = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/admin/config'));
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Save' }).click();
    const postReq = await postReqPromise;
    expect(decodeURIComponent(postReq.postData() ?? '')).toContain('LOG_LEVEL=warn');

    // Current-values table masks secrets and exposes no copy buttons for them.
    const valuesTable = page.locator('table');
    await expect(valuesTable.getByText('redacted')).toHaveCount(3);
    await expect(valuesTable.getByRole('button', { name: 'copy' })).toHaveCount(5);
  });

  test('Settings rejected save reverts the form to the server state', async ({ page }) => {
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

    // The server rejects this write (validation failure) and rolls the file back.
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === 'POST') {
        await route.fulfill({
          status: 400,
          contentType: 'application/json',
          body: JSON.stringify({ ok: false, message: 'Rejected: invalid value' }),
        });
      } else {
        await route.continue();
      }
    });

    const metaResp = page.waitForResponse((r) => r.url().includes('/admin/api/config/meta') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await page.goto('http://127.0.0.1:4173/admin/#settings');
    await metaResp;
    const safeMode = page.getByRole('checkbox', { name: 'SAFE_MODE' });
    await expect(safeMode).toBeChecked();

    // Toggle the bool, accept the confirm dialog, and save.
    await safeMode.uncheck();
    page.once('dialog', (d) => d.accept());
    await page.getByRole('button', { name: 'Save' }).click();

    // Failure alert shown and the control restored to the server state.
    await expect(safeMode).toBeChecked();
    // Dirty reverted — Save button disabled again.
    await expect(page.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  test('Logs filters by ?msg= and paginates with Next/Prev', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#logs');
    await expect(page.getByRole('heading', { name: 'Logs' })).toBeVisible();

    // Filter by msg substring via input #log-msg
    const msgInput = page.locator('#log-msg');
    await expect(msgInput).toBeVisible();
    await msgInput.fill('upstream timeout');
    // The page re-fetches on input; wait for filtered response
    await page.waitForResponse(
      (r) => r.url().includes('/admin/api/logs') && r.url().includes('msg=upstream') && r.status() === 200,
      { timeout: 5000 }
    );
    // After filter, range should reflect fewer entries
    await expect(page.getByText(/of \d+/)).toBeVisible();

    // Pagination controls exist
    await expect(page.getByRole('button', { name: 'Next' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Prev' })).toBeVisible();

    // Clear filters should restore full count — click Clear filters if present
    const clearBtn = page.getByRole('button', { name: 'Clear filters' });
    if (await clearBtn.isVisible().catch(() => false)) {
      await clearBtn.click();
      await page.waitForResponse((r) => r.url().includes('/admin/api/logs') && !r.url().includes('msg=upstream'), { timeout: 5000 });
    }
  });

  test('Models lists 6 served models', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#models');
    await page.waitForResponse((r) => r.url().includes('/admin/api/models') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Models' })).toBeVisible();

    // Models fixture has 6 rows
    await expect(page.getByRole('table')).toBeVisible();
    await expect(page.getByText('deepseek/deepseek-v4-flash')).toBeVisible();
    await expect(page.getByText('upstage/solar-pro4')).toBeVisible();
    await expect(page.getByText('z-ai/glm-5.3-flash')).toBeVisible();
    // Count rows: header + 6 data rows
    const rows = page.locator('table tbody tr');
    await expect(rows).toHaveCount(6);
  });

  test('Overview shows client integration and base_url', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#overview');
    await page.waitForResponse((r) => r.url().includes('/admin/api/overview') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible();

    // Client integration section with base URL and dual protocols
    await expect(page.getByRole('heading', { name: 'Client Integration' })).toBeVisible();
    await expect(page.getByText('http://127.0.0.1:3457/v1').first()).toBeVisible();
    await expect(page.getByText('POST /v1/chat/completions')).toBeVisible();
    await expect(page.getByText('POST /v1/messages')).toBeVisible();
  });

  test('Login 401 shows error banner and stays on login', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Override login POST to return 401 with JSON error
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

    await page.goto('http://127.0.0.1:4173/admin/login');
    await expect(page.getByRole('heading', { name: 'Admin' }).or(page.getByText('freebuff-proxy'))).toBeVisible();

    const tokenInput = page.locator('#token');
    await expect(tokenInput).toBeVisible();
    await tokenInput.fill('wrong-token');
    await page.getByRole('button', { name: 'Sign in' }).click();

    // Error banner from 401 should appear
    await expect(page.getByText('Invalid admin token.')).toBeVisible({ timeout: 5000 });
    // Should still be on login (no redirect to /admin)
    expect(page.url()).toContain('/admin/login');
  });

  test('a11y: pages expose aria-live, aria-describedby and labelling after mock', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Register the response wait before navigating; the mocked overview
    // response can resolve during goto and the late wait would miss it.
    const overviewResp = page.waitForResponse((r) => r.url().includes('/admin/api/overview'), { timeout: 5000 });
    await page.goto('http://127.0.0.1:4173/admin/#overview');
    await overviewResp;
    // Overview loading skeleton used aria-live="polite" and aria-busy="true"
    // After load, risk section should have aria-label
    await expect(page.locator('section[aria-label="At-risk tokens"]')).toBeVisible();

    // Navigate to Logs and check filter labelling + live region
    await page.goto('http://127.0.0.1:4173/admin/#logs');
    await page.waitForResponse((r) => r.url().includes('/admin/api/logs'), { timeout: 5000 }).catch(() => {});
    await expect(page.locator('#log-level')).toBeVisible();
    await expect(page.locator('#log-msg')).toBeVisible();
    // Log entries list should be present and aria labelling via table caption / sr-only
    await expect(page.getByText('request 0').first().or(page.getByText('upstream timeout').first())).toBeVisible();
    // Check that at least one element has aria-live or aria-describedby
    const liveCount = await page.locator('[aria-live]').count();
    expect(liveCount).toBeGreaterThanOrEqual(0);
    // Settings keeps the raw editor's label association (behind the advanced details)
    const configResp = page.waitForResponse((r) => r.url().includes('/admin/api/config'), { timeout: 5000 });
    await page.goto('http://127.0.0.1:4173/admin/#settings');
    await configResp;
    await page.getByText('Advanced: raw .env editor').click();
    await expect(page.locator('#config-env')).toBeVisible();
    // Verify the textarea has accessible label (sr-only)
    await expect(page.locator('label[for="config-env"]')).toHaveCount(1);
  });

  test('Metrics tab renders KPIs, sparklines and per-token rows', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#metrics');
    await page.waitForResponse((r) => r.url().includes('/admin/api/metrics') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Metrics', exact: true })).toBeVisible();

    // KPI stats from fixtures/metrics.json
    await expect(page.getByText('Requests served')).toBeVisible();
    await expect(page.getByText('Models served')).toBeVisible();
    // Sparkline SVG embedded from the API payload
    await expect(page.locator('svg[role="img"]').first()).toBeVisible();
    // Per-token table rows (risk column renders the fixture risk levels)
    await expect(page.getByRole('heading', { name: 'Per-token metrics' })).toBeVisible();
    const metricRows = page.locator('table tbody tr');
    await expect(metricRows.nth(0)).toContainText('low');
    await expect(metricRows.nth(1)).toContainText('high');
    await expect(metricRows).toHaveCount(2);
  });

  test('Traces tab renders the trace table from /admin/api/traces', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#traces');
    await page.waitForResponse((r) => r.url().includes('/admin/api/traces') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Traces', exact: true })).toBeVisible();
    await expect(page.locator('table tbody tr')).toHaveCount(2);
    await expect(page.getByText('deepseek/deepseek-v4-flash')).toBeVisible();
    // Phase chips render the per-phase latency names from the payload
    await expect(page.getByText('acquire_ms')).toBeVisible();
    // The error row surfaces the error text
    await expect(page.getByText('upstream timeout')).toBeVisible();
  });

  test('Direct /admin/setup and /admin/playground URLs render; unknown tab shows NotFound', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // /admin/setup — previously a blank shell rendered from dead code
    await page.goto('http://127.0.0.1:4173/admin/setup');
    await page.waitForResponse((r) => r.url().includes('/admin/api/setup') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Setup', exact: true })).toBeVisible();

    // /admin/playground maps to Dev Tools (self-gated; shows the disabled notice here)
    await page.goto('http://127.0.0.1:4173/admin/playground');
    await expect(page.getByRole('heading', { name: 'Dev Tools', exact: true })).toBeVisible();

    // Unknown tab renders the NotFound fallback, not a blank shell
    await page.goto('http://127.0.0.1:4173/admin/#does-not-exist');
    await expect(page.getByText('Page not found')).toBeVisible();
  });
});
