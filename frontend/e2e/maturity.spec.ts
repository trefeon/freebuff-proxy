import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

function maintenanceTokens() {
  return {
    mode: "pooled",
    in_bridge: false,
    show_bridge: false,
    bridge_tokens: 0,
    token_count: 2,
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
          last_touch: "2026-09-04T07:31:00Z",
          touch_day: "2026-09-04",
          last_action: "probe",
          last_result: "ok",
          last_advanced: "yes",
        },
      },
      {
        index: 1,
        email: "fresh@example.com",
        session_status: "active",
        locked: false,
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
  test("global section renders switch, touch model, window, and countdown", async ({
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
    // Master switch (global kill-switch) is a real control.
    await expect(
      page.getByRole("switch", { name: "Streak maintenance" }),
    ).toBeVisible();
    // Global touch model select defaults to Auto.
    const picker = page.getByLabel("Global touch model");
    await expect(picker).toBeVisible();
    await expect(picker).toHaveValue("auto");
    await expect(picker.locator("option").first()).toHaveText(
      "Auto (cheapest unmetered)",
    );
    const options = await picker.locator("option").allTextContents();
    expect(options).toContain("mimo/mimo-v2.5 (10 Freebucks/hr)");
    // Fixed pre-reset window copy + dry-run badge + countdown.
    await expect(
      page.getByText("Nightly window 23:00–00:00 Pacific"),
    ).toBeVisible();
    await expect(page.getByText("Dry run")).toBeVisible();
    await expect(page.getByLabel("Next maintenance run")).toBeVisible();
    await expect(page.getByText(/Next run|In window/)).toBeVisible();
  });

  test("enrolled toggle enrolls with the global target and touch now fires", async ({
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

    await gotoWarming(page);
    // Per-account rows are read-only status: enrolled toggle only.
    await expect(page.getByText("Warming").first()).toBeVisible();
    await expect(page.getByText("Not enrolled").first()).toBeVisible();
    await expect(page.getByText("Locked").first()).toBeVisible();

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
    await expect(page.getByText("Touch fired for Account #1")).toBeVisible();
  });

  test("master switch writes the global kill-switch", async ({ page }) => {
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

    const posts: Array<{ url: string; body: string }> = [];
    await page.route("**/admin/api/settings", async (route) => {
      if (route.request().method() === "POST") {
        posts.push({
          url: route.request().url(),
          body: route.request().postData() ?? "",
        });
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "saved." }),
      });
    });

    await gotoWarming(page);
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
    );
    await page.getByRole("switch", { name: "Streak maintenance" }).click();
    await saveReq;
    expect(posts[0].body).toContain("MATURITY_ENABLED");
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

  test("account row renders the restart-surviving event timeline", async ({
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
    await page.route("**/admin/api/maturity/history*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          enabled: true,
          token: 0,
          events: [
            { ts: 1785900000000, kind: "touch", detail: "admit ok" },
            { ts: 1785903600000, kind: "config", detail: "enabled target=7" },
          ],
        }),
      });
    });
    await gotoWarming(page);
    // Timeline folds by default: latest event visible, older events behind
    // the expander (capped at 5 recent).
    const timeline = page.getByRole("list", {
      name: "Maturity history for Account #1",
    });
    await expect(timeline).toBeVisible();
    await expect(timeline.getByText("enabled target=7")).toBeVisible();
    await expect(timeline.getByText("admit ok")).toBeHidden();
    await page.getByRole("button", { name: "Show 1 more" }).click();
    await expect(timeline.getByText("admit ok")).toBeVisible();
  });

  test("account row shows the last-run ledger with spend", async ({ page }) => {
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
    // Last-run ledger: touched time / skip reason plus week economics.
    await expect(page.getByText("Warming spend (7d)").first()).toBeVisible();
    await expect(page.getByText("Projected/mo").first()).toBeVisible();
    await expect(page.getByText("day 3/7").first()).toBeVisible();
  });

  test("shared harness renders the seeded maturity timeline without clipping", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await gotoWarming(page);
    // Token #1 carries a maturity object in the shared tokens fixture, so
    // the restart-surviving timeline renders with no bespoke mocks.
    const timeline = page.getByRole("list", {
      name: "Maturity history for Account #1",
    });
    await expect(timeline).toBeVisible();
    await page.getByRole("button", { name: "Show 1 more" }).click();
    await expect(timeline.getByText("admit ok")).toBeVisible();
    // Long descriptions must wrap instead of clipping header actions
    // (Pips 0/7 case): no card header may overflow horizontally.
    const overflow = await page.evaluate(
      () =>
        Array.from(document.querySelectorAll("section.fp-card header")).filter(
          (el) => el.scrollWidth > el.clientWidth + 1,
        ).length,
    );
    expect(overflow).toBe(0);
  });
});
