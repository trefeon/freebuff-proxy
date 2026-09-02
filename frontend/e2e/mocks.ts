import type { Page } from "@playwright/test";
import { readFileSync } from "fs";
import { join, dirname } from "path";
import { fileURLToPath } from "url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

export type Fixtures = {
  overview: unknown;
  tokens: unknown;
  models: unknown;
  config: unknown;
  configMeta: unknown;
  logs: {
    entries: Array<{ level: string; message: string; fields?: string }>;
  } & Record<string, unknown>;
  metrics: unknown;
  setup: unknown;
  traces: unknown;
  version: unknown;
  upstreamDrift: unknown;
  authStatus: unknown;
};

/**
 * Load the shared JSON fixtures once. `fixtureDir` overrides the default
 * `e2e/fixtures` directory (useful when a spec wants a bespoke fixture set).
 */
export function loadFixtures(fixtureDir?: string): Fixtures {
  const dir = fixtureDir ?? join(__dirname, "fixtures");
  return {
    overview: JSON.parse(readFileSync(join(dir, "overview.json"), "utf-8")),
    tokens: JSON.parse(readFileSync(join(dir, "tokens.json"), "utf-8")),
    models: JSON.parse(readFileSync(join(dir, "models.json"), "utf-8")),
    config: JSON.parse(readFileSync(join(dir, "config.json"), "utf-8")),
    configMeta: JSON.parse(
      readFileSync(join(dir, "config-meta.json"), "utf-8"),
    ),
    logs: JSON.parse(readFileSync(join(dir, "logs.json"), "utf-8")),
    metrics: JSON.parse(readFileSync(join(dir, "metrics.json"), "utf-8")),
    setup: JSON.parse(readFileSync(join(dir, "setup.json"), "utf-8")),
    traces: JSON.parse(readFileSync(join(dir, "traces.json"), "utf-8")),
    version: JSON.parse(readFileSync(join(dir, "version.json"), "utf-8")),
    upstreamDrift: JSON.parse(
      readFileSync(join(dir, "upstream-drift.json"), "utf-8"),
    ),
    authStatus: JSON.parse(
      readFileSync(join(dir, "auth-status.json"), "utf-8"),
    ),
  };
}

// The SPA shell served for /admin/* routes (same file serve-static.mjs
// serves); the login-page mock below fulfills with it so it can also issue
// the fb_csrf double-submit cookie like the real gateway does.
const indexPath = join(
  __dirname,
  "../../backend/internal/dashboard/dist/index.html",
);

export type MockOptions = {
  /**
   * Model the full login page the way the real gateway does (the ux.spec
   * journey needs it): GET /admin/login is fulfilled with the built SPA shell
   * plus the non-HttpOnly fb_csrf double-submit cookie. When unset (the
   * dashboard.spec contract), GET /admin/login is passed through so the
   * static dev server serves the SPA without a CSRF cookie.
   */
  loginPage?: boolean;
};

export type MockOverrides = Partial<
  Record<keyof Fixtures | "configWithApiKeys", unknown>
>;

/**
 * Shared hermetic route mock layer (issue #294): every /admin/* endpoint the
 * SPA talks to is fulfilled from the fixture pack. Both dashboard.spec.ts and
 * ux.spec.ts previously copy-pasted this harness; they now import it.
 *
 * `overrides` lets a test substitute one fixture (or inject API_KEYS via the
 * `configWithApiKeys` key). `opts.loginPage` switches the GET /admin/login
 * handling to the full CSRF-modeling variant used by the ux.spec journey.
 */
export async function mockDashboard(
  page: Page,
  fixtures: Fixtures,
  overrides: MockOverrides = {},
  opts: MockOptions = {},
) {
  // Helpers to pick overridden or base fixture
  const pick = (key: keyof Fixtures) =>
    overrides[key] ?? (fixtures as Record<string, unknown>)[key];

  // Overview
  await page.route("**/admin/api/overview", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("overview")),
    });
  });

  // Tokens
  await page.route("**/admin/api/tokens", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("tokens")),
    });
  });

  // Models
  await page.route("**/admin/api/models", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("models")),
    });
  });

  // Traces
  await page.route("**/admin/api/traces", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("traces")),
    });
  });

  // Setup
  await page.route("**/admin/api/setup", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("setup")),
    });
  });

  // Config - Supports override that includes API_KEYS for Tokens parsing test.
  await page.route(/\/admin\/api\/config(\?.*)?$/, async (route) => {
    const cfg = overrides["configWithApiKeys"] ?? pick("config");
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(cfg),
    });
  });

  // Config meta - the Settings page key catalog (JSON array from /admin/api/config/meta).
  await page.route(/\/admin\/api\/config\/meta(\?.*)?$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("configMeta")),
    });
  });

  // Logs - handles ?level= & ?msg= filtering like the real Go handler.
  await page.route("**/admin/api/logs**", async (route) => {
    const url = new URL(route.request().url());
    const level = (url.searchParams.get("level") || "").toLowerCase();
    const msg = (url.searchParams.get("msg") || "").trim();
    const base = pick("logs");
    const logsData = base as unknown as {
      entries: Array<{ level: string; message: string }>;
    };
    let entries: Array<{ level: string; message: string }> =
      logsData.entries || [];
    if (level) {
      entries = entries.filter((e) => (e.level || "").toLowerCase() === level);
    }
    if (msg) {
      const low = msg.toLowerCase();
      entries = entries.filter((e) =>
        (e.message || "").toLowerCase().includes(low),
      );
    }
    const body = JSON.stringify({
      enabled: true,
      level: level,
      msg: msg,
      has_filter: !!(level || msg),
      entries,
    });
    await route.fulfill({ status: 200, contentType: "application/json", body });
  });

  // Metrics
  await page.route("**/admin/api/metrics", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("metrics")),
    });
  });

  // Version
  await page.route("**/admin/api/version", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("version")),
    });
  });

  // Upstream drift
  await page.route("**/admin/api/upstream-drift", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("upstreamDrift")),
    });
  });

  // Auth status
  await page.route("**/admin/api/auth/status", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("authStatus")),
    });
  });

  // POST /admin/login - default success; individual tests may override for 401 case.
  // The real gateway (backend/internal/server/admin_auth.go) issues the
  // non-HttpOnly fb_csrf double-submit cookie on the login PAGE. Each response
  // carries exactly ONE Set-Cookie: route.fulfill joins multiple values into a
  // single broken cookie.
  await page.route("**/admin/login", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true }),
        headers: {
          "Set-Cookie":
            "fb_admin=mock-token; Path=/; HttpOnly; SameSite=Strict",
        },
      });
    } else if (opts.loginPage) {
      await route.fulfill({
        status: 200,
        contentType: "text/html",
        body: readFileSync(indexPath, "utf-8"),
        headers: {
          "Set-Cookie": "fb_csrf=mocknonce123; Path=/; SameSite=Strict",
        },
      });
    } else {
      await route.continue();
    }
  });

  // POST /admin/api/change-password
  await page.route("**/admin/api/change-password", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true, message: "Password changed" }),
    });
  });

  // Also mock POST /admin/config save (Tokens add-token flow uses POST /admin/config with form)
  await page.route(/\/admin\/config$/, async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Config saved" }),
      });
    } else {
      await route.continue();
    }
  });
}
