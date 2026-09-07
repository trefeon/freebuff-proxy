import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

// ---------------------------------------------------------------------------
// Quota visit auto-probe (ADR-0025): the Quota Tracker page fires one silent
// POST /admin/tokens/test-all?auto=1 after its first tokens load. Hermetic
// mocks; the endpoint is stubbed and call counts pin fire/skip behavior.
// ---------------------------------------------------------------------------

// Minimal token row mirroring the real /admin/api/tokens row shape.
function tokenRow(
  idx: number,
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    index: idx,
    email: `acct${idx}@example.com`,
    session_status: "idle",
    queue_position: 0,
    queue_depth: 0,
    active_runs: 0,
    requests: 0,
    messages_24h: 0,
    daily_limit: 0,
    usage_pct: 0,
    risk_level: "low",
    cooldown_active: false,
    cooldown_until: "",
    locked: false,
    transient_retries: 1,
    has_standing: false,
    session_instance: "",
    session_model: "",
    session_remaining_seconds: 0,
    has_quota: false,
    ...over,
  };
}

function tokensPayload(tokens: Array<Record<string, unknown>>) {
  return {
    mode: "pooled",
    in_bridge: false,
    bridge_tokens: 0,
    token_count: tokens.length,
    has_tokens: true,
    tokens,
    bridge_token_cards: [],
  };
}

// Route the tokens poll with a counting stub and the bulk-probe endpoint
// with a counting stub. Returns the recorded probe request URLs.
async function mockQuotaVisit(page: import("@playwright/test").Page) {
  const f = loadFixtures();
  await mockDashboard(page, f, {}, { loginPage: true });
  const state = { tokens: [tokenRow(0), tokenRow(1)] };
  await page.unroute("**/admin/api/tokens*");
  await page.route("**/admin/api/tokens*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(tokensPayload(state.tokens)),
    });
  });
  const probeUrls: Array<string> = [];
  await page.route("**/admin/tokens/test-all*", async (route) => {
    probeUrls.push(route.request().url());
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: `{"token":0,"ok":true,"message":"ok"}{"token":1,"ok":true,"message":"ok"}`,
    });
  });
  return probeUrls;
}

test("quota visit: mount fires one silent auto probe", async ({ page }) => {
  const probeUrls = await mockQuotaVisit(page);
  // Register before navigation: the mount fires right after the first
  // tokens load, which can precede the heading paint.
  const autoReq = page.waitForRequest(
    (r) =>
      r.method() === "POST" &&
      r.url().includes("/admin/tokens/test-all?auto=1"),
  );
  await page.goto("http://127.0.0.1:4173/admin/#quota");
  await expect(
    page.getByRole("heading", { name: "Quota Tracker", exact: true }),
  ).toBeVisible();
  // The mount fires after the first tokens load: exactly one POST carrying
  // ?auto=1 (stale server state probes; the mock stands in for stale).
  await autoReq;
  // Settle past any double-mount artifact (HMR / StrictMode-style remount
  // would fire synchronously with the first).
  await page.waitForTimeout(500);
  expect(probeUrls).toHaveLength(1);
  expect(probeUrls[0]).toContain("auto=1");
  // Silent: no success banner, no error text.
  await expect(page.getByText("Quotas refreshed from upstream.")).toHaveCount(
    0,
  );
  await expect(page.getByText("Quota refresh failed.")).toHaveCount(0);
});

test("quota visit: remount skips the probe when already fired", async ({
  page,
}) => {
  const probeUrls = await mockQuotaVisit(page);
  // Register before navigation (same race as above).
  const autoReq = page.waitForRequest(
    (r) =>
      r.method() === "POST" &&
      r.url().includes("/admin/tokens/test-all?auto=1"),
  );
  await page.goto("http://127.0.0.1:4173/admin/#quota");
  await expect(
    page.getByRole("heading", { name: "Quota Tracker", exact: true }),
  ).toBeVisible();
  await autoReq;
  await page.waitForTimeout(500);
  expect(probeUrls).toHaveLength(1);
  // Client-side remount (hash nav, no reload): the module-scope once-guard
  // holds, so the second mount issues no further probe.
  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  await expect(
    page.getByRole("heading", { name: "Tokens", exact: true }),
  ).toBeVisible();
  await page.goto("http://127.0.0.1:4173/admin/#quota");
  await expect(
    page.getByRole("heading", { name: "Quota Tracker", exact: true }),
  ).toBeVisible();
  await page.waitForTimeout(500);
  expect(probeUrls).toHaveLength(1);
});

test("quota visit: failed auto probe surfaces the error path", async ({
  page,
}) => {
  const f = loadFixtures();
  await mockDashboard(page, f, {}, { loginPage: true });
  const state = { tokens: [tokenRow(0), tokenRow(1)] };
  await page.unroute("**/admin/api/tokens*");
  await page.route("**/admin/api/tokens*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(tokensPayload(state.tokens)),
    });
  });
  await page.route("**/admin/tokens/test-all*", async (route) => {
    await route.fulfill({
      status: 500,
      contentType: "application/json",
      body: `{"ok":false,"message":"boom"}`,
    });
  });
  await page.goto("http://127.0.0.1:4173/admin/#quota");
  await expect(
    page.getByRole("heading", { name: "Quota Tracker", exact: true }),
  ).toBeVisible();
  // Failure sets the existing probeMsg error path (HTTP status), with no
  // success banner.
  await expect(page.getByText("HTTP 500")).toBeVisible();
  await expect(page.getByText("Quotas refreshed from upstream.")).toHaveCount(
    0,
  );
});
