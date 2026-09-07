import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

/**
 * In-memory pages_state backend (mirrors page-state.spec.ts): GET returns
 * the seeded snapshot ({} when absent); PUT stores {data} verbatim. The
 * returned map lets tests assert what the SPA persisted.
 */
async function mockPageState(
  page: Parameters<typeof mockDashboard>[0],
  seed: Record<string, unknown> = {},
) {
  const state = new Map<string, unknown>(Object.entries(seed));
  await page.route("**/admin/api/pages/*", async (route) => {
    const id = new URL(route.request().url()).pathname.split("/").pop() ?? "";
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ data: state.get(id) ?? {} }),
      });
    } else {
      let data: unknown = {};
      try {
        data = JSON.parse(route.request().postData() ?? "{}").data ?? {};
      } catch {
        data = {};
      }
      state.set(id, data);
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ok: true,
          message: "Page state saved.",
          code: "page_saved",
        }),
      });
    }
  });
  return state;
}
function maturityTokens() {
  return {
    mode: "pooled",
    in_bridge: false,
    show_bridge: false,
    bridge_tokens: 0,
    token_count: 2,
    has_tokens: true,
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
          last_touch: "2026-09-04T07:31:00Z",
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

test.describe("account maturity", () => {
  test("maturity page renders badges, controls, and fires save + touch", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maturityTokens()),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=a,b\nMATURITY_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });

    const posts: Array<{ url: string; body: string }> = [];
    for (const suffix of ["maturity", "maturity/touch"]) {
      await page.route(`**/admin/tokens/0/${suffix}`, async (route) => {
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

    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByRole("heading", { name: "Account Maturity" }),
    ).toBeVisible();
    await expect(page.getByText("Warming").first()).toBeVisible();
    await expect(page.getByText("Not enrolled").first()).toBeVisible();
    await expect(page.getByText("Locked").first()).toBeVisible();

    // Cards render expanded: controls are interactive immediately.

    // Save posts the drafted target/mode/touch-model/enabled for Account #1.
    const saveReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/tokens/0/maturity"),
    );
    await page.getByLabel("Streak target for Account #1").fill("14");
    await page
      .getByLabel("Touch model for Account #1")
      .selectOption("mimo/mimo-v2.5");
    await page
      .getByRole("button", { name: "Save", exact: true })
      .first()
      .click();
    await saveReq;
    expect(posts[0].body).toContain("14");
    expect(posts[0].body).toContain('"touch_model":"mimo/mimo-v2.5"');

    // Touch now bypasses slot/throttle via the manual endpoint.
    const touchReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        r.url().includes("/admin/tokens/0/maturity/touch"),
    );
    await page.getByRole("button", { name: "Touch now" }).first().click();
    await touchReq;
    await expect(page.getByText("Touch fired for Account #1")).toBeVisible();
  });

  test("maturity page warns while the global kill-switch is off", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maturityTokens()),
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
        }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByText("Maturity automation is globally off"),
    ).toBeVisible();
  });
  test("maturity card renders the restart-surviving event timeline", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maturityTokens()),
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
    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByRole("heading", { name: "Account Maturity" }),
    ).toBeVisible();
    // Timelines render with the always-expanded cards.
    const timeline = page.getByRole("list", {
      name: "Maturity history for Account #1",
    });
    await expect(timeline).toBeVisible();
    await expect(timeline.getByText("admit ok")).toBeVisible();
    await expect(timeline.getByText("enabled target=7")).toBeVisible();
  });
  test("maturity card offers the per-token touch model select", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maturityTokens()),
      });
    });
    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByRole("heading", { name: "Account Maturity" }),
    ).toBeVisible();
    // The page-header touch model picker is gone (per-card selects only).
    await expect(page.getByLabel("Economy touch model")).toHaveCount(0);
    // Cards render expanded: the per-card select is visible immediately.
    const picker = page.getByLabel("Touch model for Account #1");
    await expect(picker).toBeVisible();
    // Empty value = global MATURITY_TOUCH_MODEL fallback.
    await expect(picker).toHaveValue("");
    await expect(picker.locator("option").first()).toHaveText("Global default");
    const options = await picker.locator("option").allTextContents();
    // Served models labeled with their server-reported cost class.
    expect(options).toContain("upstage/solar-pro4 (0 Freebucks/hr)");
    expect(options).toContain("openai/gpt-5.6-luna (premium pool)");
    // Cheapest-Freebucks-cost first, premium pool last.
    const solarIdx = options.findIndex((o) =>
      o.startsWith("upstage/solar-pro4"),
    );
    const lunaIdx = options.findIndex((o) =>
      o.startsWith("openai/gpt-5.6-luna"),
    );
    expect(solarIdx).toBeGreaterThan(0);
    expect(lunaIdx).toBeGreaterThan(solarIdx);
    // No mode select: the Touch box is model-select-only, mode rides the save.
    await expect(page.getByLabel("Touch mode for Account #1")).toHaveCount(0);
  });

  test("maturity cards render expanded with no toggle", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maturityTokens()),
      });
    });
    await mockPageState(page, {});
    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByRole("heading", { name: "Account Maturity" }),
    ).toBeVisible();
    // Controls render with no click; no expand toggle exists.
    await expect(page.getByLabel("Streak target for Account #1")).toBeVisible();
    await expect(
      page.getByRole("button", { name: /Expand details|Collapse details/ }),
    ).toHaveCount(0);
  });

  test("maturity ignores stale expanded snapshot", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maturityTokens()),
      });
    });
    await mockPageState(page, { maturity: { expanded: [1] } });
    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByRole("heading", { name: "Account Maturity" }),
    ).toBeVisible();
    // No fold state exists: every card renders regardless of the snapshot.
    await expect(page.getByLabel("Touch model for Account #2")).toBeVisible();
    await expect(page.getByLabel("Touch model for Account #1")).toBeVisible();
  });

  test("shared harness renders the seeded maturity timeline without clipping", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.goto("http://127.0.0.1:4173/admin/#maturity");
    await expect(
      page.getByRole("heading", { name: "Account Maturity" }),
    ).toBeVisible();
    // Token #1 carries a maturity object in the shared tokens fixture, so
    // the restart-surviving timeline renders with no bespoke mocks.
    const timeline = page.getByRole("list", {
      name: "Maturity history for Account #1",
    });
    await expect(timeline).toBeVisible();
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
