import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

function maintenanceTokens() {
  return {
    mode: "pooled",
    in_bridge: false,
    show_bridge: false,
    bridge_tokens: 0,
    token_count: 3,
    has_tokens: true,
    maturity_enabled: true,
    maturity_dry_run: true,
    maturity_window_start: "2026-09-12T06:00:00Z",
    maturity_window_end: "2026-09-12T07:00:00Z",
    tokens: [
      {
        index: 0,
        email: "warm@example.com",
        session_status: "active",
        locked: true,
        streak: 3,
        today_used: false,
        maturity: {
          enabled: true,
          target: 7,
          mode: "unmetered",
          badge: "Warming",
          slot: "2026-09-05T07:30:00Z",
          slot_day: "2026-09-05",
          last_touch: "2026-09-05T07:31:00Z",
          touch_day: "2026-09-05",
          last_action: "probe",
          last_result: "ok",
          last_advanced: "yes",
          effective_touch_model: "mimo/mimo-v2.5",
          auto_touch_model: "mimo/mimo-v2.5",
          auto_touch_reason: "auto:unmetered",
        },
      },
      {
        index: 1,
        email: "fresh@example.com",
        session_status: "active",
        locked: false,
      },
      {
        index: 2,
        email: "cool@example.com",
        session_status: "active",
        locked: false,
        streak: 1,
        today_used: false,
        maturity: {
          enabled: true,
          target: 7,
          mode: "unmetered",
          badge: "Warming",
          slot: "2026-09-05T07:30:00Z",
          slot_day: "2026-09-05",
          last_touch: "2026-09-06T07:31:00Z",
          touch_day: "2026-09-04",
          last_action: "",
          last_result: "skip:cooling",
          effective_touch_model: "mimo/mimo-v2.5",
          auto_touch_model: "mimo/mimo-v2.5",
          auto_touch_reason: "auto:unmetered",
        },
      },
    ],
  };
}

function maintenanceConfig() {
  return {
    env_content: "AUTH_TOKENS=a,b\nMATURITY_ENABLED=true\n",
    has_env_file: true,
    effective: [
      { key: "MATURITY_ENABLED", value: "true", secret: false },
      { key: "MATURITY_TOUCH_MODEL", value: "auto", secret: false },
      { key: "MATURITY_DRY_RUN", value: "true", secret: false },
    ],
  };
}

async function gotoWarming(page) {
  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  await page.getByRole("button", { name: "Warming" }).click();
  await expect(
    page.getByRole("heading", { name: "Tokens", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Warming" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
}

test.describe("streak maintenance", () => {
  test("global board is read-only: state, rows, ledger, no controls", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceConfig()),
      });
    });

    await gotoWarming(page);
    await expect(page.getByText("Streak Maintenance")).toBeVisible();
    // No controls on the board: master switch, touch-model select, and
    // Touch-now buttons all moved out.
    await expect(
      page.getByRole("switch", { name: "Streak maintenance" }),
    ).toHaveCount(0);
    await expect(page.getByLabel("Global touch model")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Touch now" })).toHaveCount(
      0,
    );
    // Fixed pre-reset window copy + read-only dry-run badge + countdown.
    await expect(
      page.getByText("Nightly window 23:00–00:00 Pacific"),
    ).toBeVisible();
    await expect(page.getByText("Dry run")).toBeVisible();
    await expect(page.getByLabel("Next maintenance run")).toBeVisible();
    await expect(page.getByText(/Next run|In window/)).toBeVisible();
    // One row per account: touched with the resolved model id, skipped
    // with the exact ledger reason, not-enrolled without a model.
    await expect(page.getByText("Touched").first()).toBeVisible();
    await expect(page.getByText("mimo/mimo-v2.5").first()).toBeVisible();
    await expect(page.getByText("Skipped · skip:cooling")).toBeVisible();
    await expect(page.getByText("Not enrolled").first()).toBeVisible();
    // Last-run ledger summary: time, touched, skipped with reasons.
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      /touched\s+1/,
    );
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      /skipped\s+1/,
    );
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      "skip:cooling",
    );
  });

  test("accounts rows carry the enrolled toggle and touch now", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });

    const posts: Array<{ url: string; body: string }> = [];
    for (const suffix of ["maturity", "maturity/touch"]) {
      await page.route(`**/admin/tokens/1/${suffix}`, async (route) => {
        posts.push({
          url: route.request().url(),
          body: route.request().postData() ?? "",
        });
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "done." }),
        });
      });
    }
    await page.route("**/admin/tokens/0/maturity/touch", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "done." }),
      });
    });

    // Accounts tab (default): every row shows maturity status plus the
    // relocated controls.
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Tokens", exact: true }),
    ).toBeVisible();
    await expect(page.getByText("Warming").first()).toBeVisible();
    await expect(page.getByText("Not enrolled").first()).toBeVisible();
    await expect(page.getByText("Locked").first()).toBeVisible();
    await expect(
      page.getByRole("switch", { name: "Maturity for Account #2" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Touch now" }).first(),
    ).toBeVisible();

    // Enrolling posts enabled + target 0 (global MATURITY_TARGET_DAYS).
    const saveReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/tokens/1/maturity"),
    );
    await page.getByRole("switch", { name: "Maturity for Account #2" }).click();
    await saveReq;
    expect(posts[0].body).toContain('"enabled":true');
    expect(posts[0].body).toContain('"target":0');

    // Touch now stays as the manual override.
    const touchReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        r.url().includes("/admin/tokens/0/maturity/touch"),
    );
    await page.getByRole("button", { name: "Touch now" }).first().click();
    await touchReq;
  });

  test("settings advanced wires the dry-run toggle and touch model", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    // Pool knobs live here now: MATURITY_DRY_RUN as a switch,
    // MATURITY_TOUCH_MODEL as the Auto select.
    await expect(
      page.getByRole("switch", { name: "MATURITY_DRY_RUN" }),
    ).toBeVisible();
    await expect(page.getByLabel("MATURITY_TOUCH_MODEL")).toBeVisible();
  });

  test("no per-account target stepper or model select remains", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await gotoWarming(page);
    await expect(page.getByLabel("Streak target for Account #1")).toHaveCount(
      0,
    );
    await expect(page.getByLabel("Touch model for Account #1")).toHaveCount(0);
    await expect(page.getByLabel("Touch model for Account #2")).toHaveCount(0);
  });

  test("maintenance warns while the global kill-switch is off", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      const body = maintenanceTokens();
      body.maturity_enabled = false;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(body),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=a,b\nMATURITY_ENABLED=false\n",
          has_env_file: true,
          effective: [
            { key: "MATURITY_ENABLED", value: "false", secret: false },
          ],
        }),
      });
    });

    await gotoWarming(page);
    await expect(
      page.getByText("Maturity automation is globally off"),
    ).toBeVisible();
  });

  test("board renders rows without any event timeline", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await gotoWarming(page);
    // No per-account event list anywhere on the board.
    await expect(
      page.getByRole("list", { name: "Maturity history for Account #1" }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: /Show \d+ more|Show less/ }),
    ).toHaveCount(0);
    // Rows, badges, and the operator lock stay readable.
    await expect(page.getByText("Warming").first()).toBeVisible();
    await expect(page.getByText("Locked").first()).toBeVisible();
    await expect(page.getByText("mimo/mimo-v2.5").first()).toBeVisible();
  });

  test("board shows the last-run ledger summary", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await gotoWarming(page);
    // Last-run ledger: time, touched, skipped with exact reasons.
    const ledger = page.getByLabel("Last maintenance run");
    await expect(ledger).toContainText(/touched\s+1/);
    await expect(ledger).toContainText(/skipped\s+1/);
    await expect(ledger).toContainText("skip:cooling");
  });

  test("shared harness renders the seeded board without clipping", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await gotoWarming(page);
    // Token #1 carries a maturity object in the shared tokens fixture, so
    // the board renders with no bespoke mocks — and no event timeline.
    await expect(
      page.getByRole("list", { name: "Maturity history for Account #1" }),
    ).toHaveCount(0);
    await expect(page.getByLabel("Last maintenance run")).toBeVisible();
    // Long model ids must wrap instead of clipping header actions:
    // no card header may overflow horizontally.
    const overflow = await page.evaluate(
      () =>
        Array.from(document.querySelectorAll("section.fp-card header")).filter(
          (el) => el.scrollWidth > el.clientWidth + 1,
        ).length,
    );
    expect(overflow).toBe(0);
  });
});
