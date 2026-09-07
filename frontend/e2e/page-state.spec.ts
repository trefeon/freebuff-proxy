import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

const root = "http://127.0.0.1:4173/admin/";
const admin = (hash: string) => `http://127.0.0.1:4173/admin/#${hash}`;

/**
 * In-memory pages_state backend for the suite: GET returns the seeded
 * snapshot ({} when absent, never 404); PUT stores {data} verbatim. The
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

test.describe("per-page persist", () => {
  test("boot with no hash restores the last-visited page", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockPageState(page, { shell: { lastHash: "tokens" } });
    await page.goto(root);
    await expect
      .poll(() => new URL(page.url()).hash, { timeout: 10_000 })
      .toBe("#tokens");
    await expect(
      page.getByRole("heading", { name: "Tokens", exact: true }),
    ).toBeVisible();
  });

  test("an explicit hash always wins over the stored lastHash", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockPageState(page, { shell: { lastHash: "tokens" } });
    await page.goto(admin("models"));
    await expect(
      page.getByRole("heading", { name: "Models", exact: true }),
    ).toBeVisible();
    expect(new URL(page.url()).hash).toBe("#models");
  });

  test("logs filter text round-trips across reload", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("logs"));
    // Filters live in the Table view (Console is the default).
    await page.getByRole("button", { name: "Table" }).click();
    const filter = page.locator("#log-msg");
    await filter.fill("zz-filter-1");
    // The debounced save (~1s) PUTs the snapshot; the mount visit PUT may
    // land first, so poll until the filter payload arrives.
    await expect
      .poll(
        () => {
          const snapshot = state.get("logs");
          if (
            snapshot &&
            typeof snapshot === "object" &&
            "filterMsg" in snapshot
          ) {
            return snapshot.filterMsg;
          }
          return undefined;
        },
        {
          timeout: 10_000,
        },
      )
      .toBe("zz-filter-1");
    await page.reload();
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-msg")).toHaveValue("zz-filter-1");
  });

  test("tokens expanded row restores from the snapshot", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockPageState(page, { tokens: { expandedToken: 0 } });
    await page.goto(admin("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10_000,
    });
    // Expanded without any click: the snapshot drove expandedToken.
    await expect(table.getByText("Active Session:")).toBeVisible();
  });
});
