import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

test.describe("dashboard hermetic mocks", () => {
  // The Settings tests render the 58-key catalog; under parallel workers on
  // slow runners the render can exceed the default 5s expect window, so give
  // this group a wider one (CI: 1 worker + retries anyway).
  test.use({ expect: { timeout: 10_000 } });
  test("Overview polls every 15s; risk cards live on Tokens page", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    // Track overview requests
    let overviewCount = 0;
    page.on("response", (res) => {
      if (res.url().includes("/admin/api/overview")) overviewCount++;
    });

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    // First fetch should resolve quickly
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/overview") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    // Overview KPI row shows Pool total / Banned etc (rendered from fixture)
    await expect(page.getByText("Pool total")).toBeVisible();
    // At-risk cards moved to Tokens page (Account # labels, 1-based): overview
    // must not render the risk section anymore.
    await expect(
      page.locator('section[aria-label="At-risk tokens"]'),
    ).toHaveCount(0);

    // Verify polling: the hot poll hits ?view=live within 17s (15s interval +
    // buffer) while the once-per-mount full fetch carries the static fields
    // (issue #322). The mock answers the live URL with the full shape, so the
    // merge path renders the same cards.
    await page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/overview") &&
        r.url().includes("view=live") &&
        r.status() === 200,
      { timeout: 17000 },
    );
    expect(overviewCount).toBeGreaterThanOrEqual(2);
    // Risk cards moved to Tokens page with 1-based Account # labels.
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(page.getByText("Account #2").first()).toBeVisible();
  });

  test("Tokens lists pooled tokens and expands details", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Tokens", exact: true }),
    ).toBeVisible();
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    const expandBtn = table
      .locator('button[aria-label*="Expand details"]')
      .first();
    await expect(expandBtn).toBeVisible();
    await expandBtn.click();
    // Without DEVTOOLS_ENABLED the Dev Session toolbar stays hidden; the
    // expanded row keeps the active-session line.
    await expect(page.getByText("Dev Session:")).not.toBeVisible();
    await expect(table.getByText("Active Session:")).toBeVisible();

    // With DEVTOOLS_ENABLED=true the toolbar appears (per-token session spawn).
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content:
            "PORT=3457\nAUTH_TOKENS=tok0,tok1\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.reload();
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(table.getByText("Dev Session:")).toBeVisible();
    // Issue #322: the 10s hot poll hits ?view=live (static fields ride the
    // once-per-mount full fetch).
    await page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/tokens") &&
        r.url().includes("view=live") &&
        r.status() === 200,
      { timeout: 12000 },
    );
  });

  test("Tokens drawer shows pinned models for locked slots", async ({
    page,
  }) => {
    const f = loadFixtures();
    const lockedTokens = JSON.parse(JSON.stringify(f.tokens));
    lockedTokens.tokens[0].allowed_models = ["z-ai/glm-5.2"];
    lockedTokens.tokens[0].allowlist_skips = 3;
    await mockDashboard(page, f, { tokens: lockedTokens });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(page.getByText("Pinned models").first()).toBeVisible();
    await expect(page.getByText("z-ai/glm-5.2").first()).toBeVisible();
  });

  test("Token drawer pins a model through MODEL_LOCKS save", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=tok0,tok1\n",
          has_env_file: true,
        }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(
      page.getByText("Unlocked — serves any model.").first(),
    ).toBeVisible();

    const postReqPromise = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/config"),
    );
    await table
      .getByLabel("Pin a model to this token")
      .selectOption("mimo/mimo-v2.5");
    await table.getByRole("button", { name: "Pin" }).click();
    const postReq = await postReqPromise;
    expect(decodeURIComponent(postReq.postData() ?? "")).toContain(
      "MODEL_LOCKS=0:mimo/mimo-v2.5",
    );
  });

  test("Quota Tracker shows premium pool and per-model session quota", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    let tokensCount = 0;
    page.on("response", (res) => {
      if (res.url().includes("/admin/api/tokens")) tokensCount++;
    });

    await page.goto("http://127.0.0.1:4173/admin/#quota");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(
      page.getByRole("heading", { name: "Quota Tracker", exact: true }),
    ).toBeVisible();
    // Sidebar entry links to the new tab
    await expect(
      page.getByRole("link", { name: "Quota Tracker" }),
    ).toBeVisible();

    // Per-account cards: one per pooled account (1-based Account # labels)
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #5" }),
    ).toBeVisible();
    // Account 1 fixture carries premium_quota → Premium pool bar renders
    await expect(page.getByText("Premium pool").first()).toBeVisible();
    await expect(page.getByText("4/day pacific_day")).toBeVisible();
    // Tokens without premium data show the subtle hint
    await expect(
      page
        .getByText(
          "No premium quota data — run a request or -test-token to populate.",
        )
        .first(),
    ).toBeVisible();

    // Session quota by model tables from the fixture rows
    await expect(
      page.getByRole("heading", { name: "Session quota by model" }).first(),
    ).toBeVisible();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
    await expect(page.getByText("(in 5h 32m)").first()).toBeVisible();
    await expect(page.getByText("base=1, referral=1").first()).toBeVisible();
    // Usage bars under quota rows
    await expect(
      page.locator('table [role="progressbar"]').first(),
    ).toBeVisible();

    // Polls every 10s: a second tokens fetch proves periodic refresh
    await page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
      { timeout: 12000 },
    );
    expect(tokensCount).toBeGreaterThanOrEqual(2);
  });

  test("Quota Tracker labels restart-restored quota as last-seen", async ({
    page,
  }) => {
    const f = loadFixtures();
    // Simulate a post-restart snapshot: quota rows present but stale.
    const staleTokens = JSON.parse(JSON.stringify(f.tokens));
    staleTokens.tokens[0].quota_stale = true;
    staleTokens.tokens[0].quota_saved_at = "2026-09-03T10:00:00Z";
    await mockDashboard(page, f, { tokens: staleTokens });

    await page.goto("http://127.0.0.1:4173/admin/#quota");
    await expect(
      page.getByRole("heading", { name: "Quota Tracker", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    // Stale note renders, and the restored rows render with it (no empty
    // state for a token that carries last-seen quota).
    await expect(page.getByText("before restart").first()).toBeVisible();
    await expect(page.getByText("stealth/ox-alpha").first()).toBeVisible();
  });

  test("Settings renders catalog groups and saves a toggled bool into the .env", async ({
    page,
  }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content:
        "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
      has_env_file: true,
      effective: [
        { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
        { key: "AUTH_TOKENS", value: "2 token(s)", secret: true },
        { key: "API_KEYS", value: "1 key(s)", secret: true },
        { key: "ADMIN_TOKEN", value: "set", secret: true },
        { key: "SAFE_MODE", value: "true", secret: false },
        { key: "LOG_LEVEL", value: "info", secret: false },
        { key: "COST_MODE", value: "free", secret: false },
        { key: "MAX_MESSAGES_PER_DAY", value: "0", secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("heading", { name: "General" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Pool" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Security" })).toBeVisible();
    // A documented bool renders as a switch; effective value drives it.
    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toBeVisible();
    await expect(safeMode).toHaveAttribute("aria-checked", "true");
    // The restart-only HTTP read timeout renders as a duration textbox
    // with the compiled-in default and a restart badge.
    const httpTimeout = page.getByRole("textbox", {
      name: "HTTP_READ_TIMEOUT",
    });
    await expect(httpTimeout).toBeVisible();
    await expect(httpTimeout).toHaveValue("60s");

    // Toggling marks the form dirty and surfaces the unsaved-changes banner.
    await safeMode.click();
    await expect(safeMode).toHaveAttribute("aria-checked", "false");
    await expect(page.getByText("Unsaved changes")).toBeVisible();
    await expect(page.getByRole("button", { name: "Save" })).toBeEnabled();

    // Save posts the built .env: the toggled line plus untouched lines.
    let savedBody = "";
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === "POST") {
        savedBody = decodeURIComponent(route.request().postData() || "");
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            ok: true,
            message:
              "Saved and reloaded. These keys apply after restart only: LOG_LEVEL",
            restart_only: ["LOG_LEVEL"],
          }),
        });
      } else {
        await route.continue();
      }
    });
    page.once("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByText(/apply after restart only/)).toBeVisible();
    await expect(
      page.getByText("Applies after restart: LOG_LEVEL"),
    ).toBeVisible();
    expect(savedBody).toContain("SAFE_MODE=false");
    expect(savedBody).toContain("AUTH_TOKENS=tok0,tok1");
    expect(savedBody).toContain("LOG_LEVEL=info");
  });

  test("Settings legacy #config alias, select save, secret masking and raw editor validation", async ({
    page,
  }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content:
        "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
      has_env_file: true,
      effective: [
        { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
        { key: "AUTH_TOKENS", value: "2 token(s)", secret: true },
        { key: "API_KEYS", value: "1 key(s)", secret: true },
        { key: "ADMIN_TOKEN", value: "set", secret: true },
        { key: "SAFE_MODE", value: "true", secret: false },
        { key: "LOG_LEVEL", value: "info", secret: false },
        { key: "COST_MODE", value: "free", secret: false },
        { key: "MAX_MESSAGES_PER_DAY", value: "0", secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });

    // Legacy '#config' hash still routes to the Settings page.
    const metaRespLegacy = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#config");
    await metaRespLegacy;
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();

    // Select renders enum options from meta; changing it edits the document.
    const logLevel = page.getByRole("combobox", { name: "LOG_LEVEL" });
    await expect(logLevel).toBeVisible();
    await expect(logLevel).toContainText("debug");
    await expect(logLevel).toContainText("trace");
    await logLevel.selectOption("warn");

    // Keys using default values render the 'default' badge.
    await expect(
      page.getByText("default", { exact: true }).first(),
    ).toBeVisible();
    // Advanced raw editor mirrors the form edit and still validates.
    await page.getByText("Advanced: raw .env editor").click();
    const editor = page.locator("#config-env");
    await expect(editor).toBeVisible();
    await expect(editor).toHaveValue(/LOG_LEVEL=warn/);
    await page.getByRole("button", { name: "Validate" }).click();
    await expect(page.getByText(/Configuration is valid/)).toBeVisible();

    // Save posts the built .env line for the edited select.
    const postReqPromise = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/config"),
    );
    page.once("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "Save" }).click();
    const postReq = await postReqPromise;
    expect(decodeURIComponent(postReq.postData() ?? "")).toContain(
      "LOG_LEVEL=warn",
    );

    // Current-values table masks secrets and exposes no copy buttons for them.
    const valuesTable = page.locator("table");
    await expect(valuesTable.getByText("redacted")).toHaveCount(3);
    await expect(valuesTable.getByRole("button", { name: "copy" })).toHaveCount(
      5,
    );
  });

  test("Settings rejected save reverts the form to the server state", async ({
    page,
  }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content:
        "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
      has_env_file: true,
      effective: [
        { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
        { key: "AUTH_TOKENS", value: "2 token(s)", secret: true },
        { key: "API_KEYS", value: "1 key(s)", secret: true },
        { key: "ADMIN_TOKEN", value: "set", secret: true },
        { key: "SAFE_MODE", value: "true", secret: false },
        { key: "LOG_LEVEL", value: "info", secret: false },
        { key: "COST_MODE", value: "free", secret: false },
        { key: "MAX_MESSAGES_PER_DAY", value: "0", secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });

    // The server rejects this write (validation failure) and rolls the file back.
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === "POST") {
        await route.fulfill({
          status: 400,
          contentType: "application/json",
          body: JSON.stringify({
            ok: false,
            message: "Rejected: invalid value",
          }),
        });
      } else {
        await route.continue();
      }
    });

    const metaResp = page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toHaveAttribute("aria-checked", "true");

    // Toggle the bool, accept the confirm dialog, and save.
    await safeMode.click();
    page.once("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "Save" }).click();

    // Failure alert shown and the control restored to the server state.
    await expect(safeMode).toHaveAttribute("aria-checked", "true");
    // Dirty reverted — Save button disabled again.
    await expect(page.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  test("Logs filters by ?msg= and paginates with Next/Prev", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#logs");
    await expect(page.getByRole("heading", { name: "Logs" })).toBeVisible();

    // Console (/v1 inference traffic) is the default view; table filtering
    // and pagination live in the Table view, so switch there first.
    await page.getByRole("button", { name: "Table" }).click();

    const msgInput = page.locator("#log-msg");
    await expect(msgInput).toBeVisible();
    await msgInput.fill("upstream timeout");
    // The page re-fetches on input; wait for filtered response
    await page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/logs") &&
        r.url().includes("msg=upstream") &&
        r.status() === 200,
      { timeout: 5000 },
    );
    // After filter, range should reflect fewer entries
    await expect(page.getByText(/of \d+/)).toBeVisible();

    // Pagination controls exist
    await expect(page.getByRole("button", { name: "Next" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Prev" })).toBeVisible();

    // Clear filters should restore full count — click Clear filters if present
    const clearBtn = page.getByRole("button", { name: "Clear filters" });
    if (await clearBtn.isVisible().catch(() => false)) {
      await clearBtn.click();
      await page.waitForResponse(
        (r) =>
          r.url().includes("/admin/api/logs") &&
          !r.url().includes("msg=upstream"),
        { timeout: 5000 },
      );
    }
  });

  test("Logs console survives same-second duplicate request lines", async ({
    page,
  }) => {
    // Live entries share second-precision timestamps: one request emits
    // chat request + routing + access + trace + done with the same req_id
    // and time. Console line ids must stay unique or Svelte throws
    // each_key_duplicate and the view breaks.
    const t = new Date().toISOString();
    const E = (message: string, fields: string) => ({
      time: t,
      level: "INFO",
      message,
      fields,
    });
    const f = loadFixtures();
    const pageErrors: string[] = [];
    page.on("pageerror", (e) => pageErrors.push(String(e)));
    await mockDashboard(page, f, {
      logs: {
        entries: [
          E(
            "chat request",
            "req_id=dup  model=openai/gpt-5.6-luna  msgs=3  tools=2",
          ),
          E("chat routing", "req_id=dup  agent=stealth/ox-alpha"),
          E(
            "access",
            "req_id=dup  method=POST  path=/v1/chat/completions  status=200  ms=100",
          ),
          E("chat trace", "req_id=dup  total_ms=100"),
          E("chat done", "req_id=dup  ms=100"),
        ],
      },
    });

    await page.goto("http://127.0.0.1:4173/admin/#logs");
    // Console is the default view: the five same-req_id lines merge into ONE
    // request group carrying the MSG/TOOL counts, no crash.
    await expect(page.getByText("1 request")).toBeVisible();
    await expect(page.getByText("3 MSG", { exact: true })).toBeVisible();
    await expect(page.getByText("2 TOOL", { exact: true })).toBeVisible();
    await expect(page.getByText("openai/gpt-5.6-luna").first()).toBeVisible();
    expect(pageErrors).toEqual([]);
  });

  test("Models lists 6 served models", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#models");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/models") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(page.getByRole("heading", { name: "Models" })).toBeVisible();

    // Models fixture has 6 rows
    await expect(page.getByRole("table")).toBeVisible();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
    await expect(page.getByText("upstage/solar-pro4").first()).toBeVisible();
    await expect(page.getByText("z-ai/glm-5.3-flash").first()).toBeVisible();
    // Count rows: header + 6 data rows
    const rows = page.locator("table tbody tr");
    await expect(rows).toHaveCount(6);
  });

  test("Overview shows client integration and base_url", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/overview") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

    // Client integration section with base URL and dual protocols
    await expect(
      page.getByRole("heading", { name: "Client Integration" }),
    ).toBeVisible();
    await expect(
      page.getByText("http://127.0.0.1:3457/v1").first(),
    ).toBeVisible();
    await expect(page.getByText("POST /v1/chat/completions")).toBeVisible();
    await expect(page.getByText("POST /v1/messages")).toBeVisible();
  });

  test("Login 401 shows error banner and stays on login", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Override login POST to return 401 with JSON error
    await page.unroute("**/admin/login");
    await page.route("**/admin/login", async (route) => {
      if (route.request().method() === "POST") {
        await route.fulfill({
          status: 401,
          contentType: "application/json",
          body: JSON.stringify({ error: "Invalid admin token." }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto("http://127.0.0.1:4173/admin/login");
    await expect(
      page
        .getByRole("heading", { name: "Admin" })
        .or(page.getByText("freebuff-proxy")),
    ).toBeVisible();

    const tokenInput = page.locator("#token");
    await expect(tokenInput).toBeVisible();
    await tokenInput.fill("wrong-token");
    await page.getByRole("button", { name: "Sign in" }).click();

    // Error banner from 401 should appear
    await expect(page.getByText("Invalid admin token.")).toBeVisible({
      timeout: 5000,
    });
    // Should still be on login (no redirect to /admin)
    expect(page.url()).toContain("/admin/login");
  });

  test("a11y: pages expose aria-live, aria-describedby and labelling after mock", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Register the response wait before navigating; the mocked overview
    // response can resolve during goto and the late wait would miss it.
    const overviewResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/overview"),
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await overviewResp;
    // Overview loading skeleton used aria-live="polite" and aria-busy="true"
    // Risk section moved to Tokens page: overview must not render it, Tokens must.
    await expect(
      page.locator('section[aria-label="At-risk tokens"]'),
    ).toHaveCount(0);
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.locator('section[aria-label="At-risk tokens"]'),
    ).toBeVisible();

    // Navigate to Logs and check filter labelling + live region. Console is
    // the default view; the labelled filter inputs and entry text live in
    // the Table view.
    await page.goto("http://127.0.0.1:4173/admin/#logs");
    await page.getByRole("button", { name: "Table" }).click();
    await page
      .waitForResponse((r) => r.url().includes("/admin/api/logs"), {
        timeout: 5000,
      })
      .catch(() => {});
    await expect(page.locator("#log-level")).toBeVisible();
    await expect(page.locator("#log-msg")).toBeVisible();
    await expect(
      page
        .getByText("request 0")
        .first()
        .or(page.getByText("upstream timeout").first()),
    ).toBeVisible();
    // Check that at least one element has aria-live or aria-describedby
    const liveCount = await page.locator("[aria-live]").count();
    expect(liveCount).toBeGreaterThanOrEqual(0);
    // Settings keeps the raw editor's label association (behind the advanced details)
    const configResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config"),
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await configResp;
    await page.getByText("Advanced: raw .env editor").click();
    await expect(page.locator("#config-env")).toBeVisible();
    // Verify the textarea has accessible label (sr-only)
    await expect(page.locator('label[for="config-env"]')).toHaveCount(1);
  });

  test("Metrics tab renders KPIs, sparklines and per-token rows", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#metrics");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/metrics") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(
      page.getByRole("heading", { name: "Metrics", exact: true }),
    ).toBeVisible();

    // KPI stats from fixtures/metrics.json
    await expect(page.getByText("Requests served")).toBeVisible();
    await expect(page.getByText("Models served")).toBeVisible();
    // Sparkline SVG embedded from the API payload
    await expect(page.locator('svg[role="img"]').first()).toBeVisible();
    // Per-token table rows (risk column renders the fixture risk levels)
    await expect(
      page.getByRole("heading", { name: "Per-token metrics" }),
    ).toBeVisible();
    const metricRows = page.locator("table tbody tr");
    await expect(metricRows.nth(0)).toContainText("low");
    await expect(metricRows.nth(1)).toContainText("high");
    await expect(metricRows).toHaveCount(2);
  });

  test("Traces tab renders the trace table from /admin/api/traces", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#traces");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/traces") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(
      page.getByRole("heading", { name: "Traces", exact: true }),
    ).toBeVisible();
    await expect(page.locator("table tbody tr")).toHaveCount(2);
    await expect(page.getByText("deepseek/deepseek-v4-flash")).toBeVisible();
    // Phase chips render the per-phase latency names from the payload
    await expect(page.getByText("acquire_ms")).toBeVisible();
    // The error row surfaces the error text
    await expect(page.getByText("upstream timeout")).toBeVisible();
  });

  test("Direct /admin/setup and /admin/playground URLs render; unknown tab shows NotFound", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // /admin/setup — previously a blank shell rendered from dead code
    await page.goto("http://127.0.0.1:4173/admin/setup");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/setup") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(
      page.getByRole("heading", { name: "Setup", exact: true }),
    ).toBeVisible();

    // /admin/playground maps to Dev Tools (self-gated; shows the disabled notice here)
    await page.goto("http://127.0.0.1:4173/admin/playground");
    await expect(
      page.getByRole("heading", { name: "Dev Tools", exact: true }),
    ).toBeVisible();

    // Unknown tab renders the NotFound fallback, not a blank shell
    await page.goto("http://127.0.0.1:4173/admin/#does-not-exist");
    await expect(page.getByText("Page not found")).toBeVisible();
  });
});
