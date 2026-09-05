import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

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

    // Save posts the drafted target/mode/enabled for Account #1.
    const saveReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/tokens/0/maturity"),
    );
    await page
      .getByLabel("Streak target for Account #1")
      .fill("14");
    await page.getByRole("button", { name: "Save" }).first().click();
    await saveReq;
    expect(posts[0].body).toContain("14");

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
});
