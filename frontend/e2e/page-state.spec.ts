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
    // A legacy hash is an explicit route too: it redirects to its target
    // (and the normalized hash wins over the stored lastHash).
    await page.goto(admin("models"));
    await expect(
      page.getByRole("heading", { name: "Catalog", exact: true }),
    ).toBeVisible();
    expect(new URL(page.url()).hash).toBe("#catalog");
  });

  test("logs filter text round-trips across reload", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("activity"));
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

  test("tokens out-of-range index drops the drawer instead of opening the wrong row", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    // Five pooled accounts in fixtures; index 99 matches none.
    await mockPageState(page, { tokens: { expandedToken: 99 } });
    await page.goto(admin("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10_000,
    });
    // No drawer opened (and the stale index is dropped, never re-persisted).
    await expect(table.getByText("Active Session:")).toHaveCount(0);
  });

  test("logs full filter set round-trips across reload", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("activity"));
    // Filters live in the Table view (Console is the default).
    await page.getByRole("button", { name: "Table" }).click();
    await page.locator("#log-level").selectOption("info");
    await page.locator("#log-msg").fill("request");
    // Hide-admin defaults on; flipping it off is part of the persisted set.
    await page.getByRole("button", { name: "Hide admin" }).click();
    // 13 info+request fixture entries → page 2 exists at 10 rows/page.
    await page.getByRole("button", { name: "Next" }).click();
    // The debounced save (~1s) PUTs the snapshot; poll until the full
    // filter payload arrives.
    await expect
      .poll(
        () => {
          const snapshot = state.get("logs");
          if (snapshot && typeof snapshot === "object") {
            const s = snapshot as Record<string, unknown>;
            if (
              s.filterMsg === "request" &&
              s.filterLevel === "info" &&
              s.hideAdmin === false &&
              s.viewMode === "table" &&
              s.page === 1
            )
              return "ready";
          }
          return undefined;
        },
        { timeout: 10_000 },
      )
      .toBe("ready");
    await page.reload();
    // Table view restored without clicking: the stored viewMode drove it.
    await expect(page.locator("#log-level")).toBeVisible({ timeout: 10_000 });
    await expect(page.locator("#log-level")).toHaveValue("info");
    await expect(page.locator("#log-msg")).toHaveValue("request");
    await expect(
      page.getByRole("button", { name: "Hide admin" }),
    ).toHaveAttribute("aria-pressed", "false");
    await expect(page.getByText("Page 2 / 2")).toBeVisible();
  });

  test("an unknown hash is never persisted to shell.lastHash", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("no-such-page"));
    // Past the debounce window there must still be no shell snapshot: the
    // App shell only remembers known page ids.
    await page.waitForTimeout(1500);
    expect(state.get("shell")).toBeUndefined();
  });
});

test.describe("settings DB overlay", () => {
  type Posted = Array<Record<string, unknown>>;
  async function mockSettings(
    page: Parameters<typeof mockDashboard>[0],
    posted: Posted,
    postStatus = 200,
    opts: { degraded?: boolean } = {},
  ) {
    // Mutable overlay: DELETE drops a key and later GETs reflect the drop,
    // so the DbOverrideBadge reset round-trip is actually observable.
    let live: Array<Record<string, unknown>> = [
      {
        key: "LOG_LEVEL",
        value: "info",
        source: "db",
        restart_only: false,
        secret: false,
      },
      {
        key: "MODEL_ALIASES",
        value: "gpt-4o:openai/gpt-5.6-luna",
        source: "db",
        restart_only: false,
        secret: false,
      },
    ];
    const deleted: string[] = [];
    await page.route("**/admin/api/settings", async (route) => {
      if (route.request().method() === "POST") {
        try {
          posted.push(JSON.parse(route.request().postData() ?? "{}"));
        } catch {
          posted.push({});
        }
        if (postStatus !== 200) {
          await route.fulfill({
            status: postStatus,
            contentType: "application/json",
            body: JSON.stringify({
              ok: false,
              message: "Setting rejected: boom",
              code: "invalid_setting",
            }),
          });
        } else {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify({
              ok: true,
              message: "X saved to the DB overlay and applied live.",
              code: "setting_saved",
              restart_only: [],
            }),
          });
        }
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            settings: live,
            degraded: opts.degraded === true,
          }),
        });
      }
    });
    // DELETE /admin/api/settings/:key needs its own glob: Playwright * does
    // not cross /, so the base pattern above never sees the keyed path and
    // the badge reset fell through unmocked before this route existed.
    await page.route("**/admin/api/settings/*", async (route) => {
      if (route.request().method() !== "DELETE") {
        await route.continue();
        return;
      }
      const key =
        new URL(route.request().url()).pathname.split("/").pop() ?? "";
      deleted.push(key);
      live = live.filter((e) => e.key !== key);
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ok: true,
          message: "DB override removed.",
          code: "setting_deleted",
        }),
      });
    });
    return deleted;
  }

  test("model routing rows mount with DB badges and per-key save posts the overlay", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: Posted = [];
    await mockSettings(page, posted);
    await mockPageState(page);
    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible({ timeout: 10_000 });
    // ModelRoutingSettings mounts (it owns these four inputs) and the
    // seeded db source renders its override badge.
    await expect(
      page.locator('input[aria-label="MODEL_ALIASES"]'),
    ).toBeVisible();
    await expect(page.getByText("DB override").first()).toBeVisible();
    // First row-level save (SAFE_MODE) posts just that key to the overlay.
    await page
      .getByRole("button", { name: "Save as override" })
      .first()
      .click();
    await expect
      .poll(() => posted.length, { timeout: 10_000 })
      .toBeGreaterThan(0);
    expect(posted[0].key).toBe("SAFE_MODE");
    expect(typeof posted[0].value).toBe("string");
    await expect(
      page.getByText(/saved to the DB overlay/i).first(),
    ).toBeVisible();
  });

  test("a rejected overlay value surfaces inline on the row", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: Posted = [];
    await mockSettings(page, posted, 400);
    await mockPageState(page);
    await page.goto(admin("settings"));
    await expect(page.locator('input[aria-label="MODEL_ALIASES"]')).toBeVisible(
      { timeout: 10_000 },
    );
    await page
      .getByRole("button", { name: "Save as override" })
      .first()
      .click();
    await expect
      .poll(() => posted.length, { timeout: 10_000 })
      .toBeGreaterThan(0);
    await expect(
      page.locator('span[role="status"]', { hasText: "Setting rejected" }),
    ).toBeVisible();
  });

  test("DB badge reset deletes the overlay key and refetches the form", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: Posted = [];
    const deleted = await mockSettings(page, posted);
    await mockPageState(page);
    await page.goto(admin("settings"));
    await expect(page.locator('input[aria-label="MODEL_ALIASES"]')).toBeVisible(
      { timeout: 10_000 },
    );
    // Two seeded db rows → two override badges + plural count copy.
    await expect(page.getByText("DB override", { exact: true })).toHaveCount(2);
    await expect(
      page.getByText("2 settings come from the DB overlay"),
    ).toBeVisible();
    const delReq = page.waitForRequest(
      (r) =>
        r.method() === "DELETE" && r.url().includes("/admin/api/settings/"),
    );
    // First Reset in DOM order drops LOG_LEVEL (Gateway card precedes
    // Model Routing); the refetch then leaves one badge + singular copy.
    await page.getByRole("button", { name: "Reset" }).first().click();
    await delReq;
    expect(deleted).toEqual(["LOG_LEVEL"]);
    await expect(page.getByText("DB override", { exact: true })).toHaveCount(1);
    await expect(
      page.getByText("1 setting comes from the DB overlay"),
    ).toBeVisible();
    await expect(page.getByText("DB override removed.")).toBeVisible();
  });

  test("degraded store banners read-only while the .env form stays usable", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettings(page, [], 200, { degraded: true });
    await mockPageState(page);
    await page.goto(admin("settings"));
    await expect(page.locator('input[aria-label="MODEL_ALIASES"]')).toBeVisible(
      { timeout: 10_000 },
    );
    await expect(page.getByText("DB overlay unavailable")).toBeVisible();
    await expect(page.getByText(/runs live-only/)).toBeVisible();
    // The .env form keeps working: editing a key enables Save Changes.
    await page
      .locator('input[aria-label="MODEL_ALIASES"]')
      .fill("gpt-4o:openai/gpt-5.6-luna,x:y");
    await expect(
      page.getByRole("button", { name: "Save Changes", exact: true }),
    ).toBeEnabled();
  });
});
