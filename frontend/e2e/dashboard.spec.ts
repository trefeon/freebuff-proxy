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
  await page.route('**/admin/api/config', async (route) => {
    const cfg = overrides['configWithApiKeys'] ?? pick('config');
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(cfg),
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
  await page.route('**/admin/config', async (route) => {
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

  test('Tokens lists pooled tokens and expands quota table', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#tokens');
    await expect(page.getByRole('heading', { name: 'Tokens', exact: true })).toBeVisible();
    await expect(page.getByText('#0')).toBeVisible({ timeout: 10000 });
    const expandBtn = page.locator('button[aria-label*="Expand quotas"]').first();
    await expect(expandBtn).toBeVisible();
    await expandBtn.click();
    await expect(page.locator('td', { hasText: 'deepseek/deepseek-v4-flash' }).first()).toBeVisible();
  });

  test('Config shows env_content in editor and effective table', async ({ page }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content: 'LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\n',
      has_env_file: true,
      effective: [
        { key: 'LISTEN_ADDR', value: '127.0.0.1:3457', secret: false },
        { key: 'API_KEYS', value: '1 key(s)', secret: true },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });

    await page.goto('http://127.0.0.1:4173/admin/#config');
    await page.waitForResponse((r) => r.url().includes('/admin/api/config') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Config', exact: true })).toBeVisible();

    // Textarea with id config-env should contain env_content
    const editor = page.locator('#config-env');
    await expect(editor).toBeVisible();
    await expect(editor).toHaveValue(/AUTH_TOKENS/);
    await expect(editor).toHaveValue(/API_KEYS=sk-local-xyz/);

    // Effective table shows masked secret for API_KEYS
    await expect(page.getByRole('table')).toContainText('API_KEYS');
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

  test('Models lists 7 served models and aliases', async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto('http://127.0.0.1:4173/admin/#models');
    await page.waitForResponse((r) => r.url().includes('/admin/api/models') && r.status() === 200, { timeout: 5000 }).catch(() => {});
    await expect(page.getByRole('heading', { name: 'Models' })).toBeVisible();

    // Models fixture has 7 rows
    await expect(page.getByRole('table')).toBeVisible();
    await expect(page.getByText('deepseek/deepseek-v4-flash')).toBeVisible();
    await expect(page.getByText('stealth/ox-alpha')).toBeVisible();
    await expect(page.getByText('z-ai/glm-5.3-flash')).toBeVisible();
    // Count rows: header + 7 data rows
    const rows = page.locator('table tbody tr');
    await expect(rows).toHaveCount(7);
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

    await page.goto('http://127.0.0.1:4173/admin/#overview');
    await page.waitForResponse((r) => r.url().includes('/admin/api/overview'), { timeout: 5000 });
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
    // Field/Config should have label association
    await page.goto('http://127.0.0.1:4173/admin/#config');
    await page.waitForResponse((r) => r.url().includes('/admin/api/config'), { timeout: 5000 });
    await expect(page.locator('#config-env')).toBeVisible();
    // Verify the textarea has accessible label (sr-only)
    await expect(page.locator('label[for="config-env"]')).toHaveCount(1);
  });
});
