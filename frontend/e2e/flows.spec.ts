import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

const RW = "e2e/fixtures-realworld";
const admin = (hash) => `http://127.0.0.1:4173/admin/#${hash}`;

test.describe("user flows", () => {
  test("referral: copy invite code confirms Copied", async ({ page }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("tokens"));
    await page.getByText("FREE-abc123").first().waitFor();
    const banner = page.locator("div", { hasText: "FREE-abc123" }).last();
    await banner.getByRole("button").click();
    await expect(page.getByText("Copied").first()).toBeVisible();
  });

  test("models: copy model ID confirms Copied", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await page.goto(admin("models"));
    await page.getByText("deepseek/deepseek-v4-flash").first().waitFor();
    await page.getByRole("button", { name: "Copy model ID" }).first().click();
    await expect(page.getByText("Copied").first()).toBeVisible();
  });

  test("models: failed load shows retry and recovers", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    let calls = 0;
    await page.unroute("**/admin/api/models*");
    await page.route("**/admin/api/models*", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.models),
        });
      }
    });
    await page.goto(admin("models"));
    await expect(page.getByText("Failed to load models")).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
  });

  test("quota: refresh button refetches tokens", async ({ page }) => {
    await mockDashboard(page, loadFixtures(RW));
    let hits = 0;
    await page.route("**/admin/api/tokens*", async (route) => {
      hits += 1;
      await route.fallback();
    });
    await page.goto(admin("quota"));
    await page.getByText("Quota Tracker").first().waitFor();
    const before = hits;
    await page.getByRole("button", { name: "Refresh" }).first().click();
    await expect
      .poll(async () => hits, { timeout: 5000 })
      .toBeGreaterThan(before);
  });

  test("quota: exempt account shows quota exempt chip", async ({ page }) => {
    const f = loadFixtures(RW);
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const withFb = (Array.isArray(list) ? list : []).find((t) => t.freebucks);
    if (!withFb) throw new Error("RW tokens fixture has no freebucks row");
    withFb.freebucks.quota_exempt = true;
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokens),
      });
    });
    await page.goto(admin("quota"));
    await expect(page.getByText("quota exempt").first()).toBeVisible();
  });
});
