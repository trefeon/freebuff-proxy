import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

// Real-world pack: every fixture carries production-shaped data (bridge
// cards, bans, locks, cooldowns, freebucks, streaks, traffic counters, peak
// pricing) so each page proves it renders the full backend contract.
const RW = "e2e/fixtures-realworld";
const admin = (hash: string) => `http://127.0.0.1:4173/admin/#${hash}`;

test.describe("real-world data", () => {
  test("overview: KPIs, both notices, peak window, bridge card", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("overview"));
    await expect(page.getByText("Pool total")).toBeVisible();
    await expect(page.getByText("548")).toBeVisible();
    await expect(
      page.getByText("Official Upstream Announcement"),
    ).toBeVisible();
    await expect(page.getByText("DeepSeek peak pricing active")).toBeVisible();
    await expect(page.getByText("Peak Window (19h 0m left)")).toBeVisible();
    await expect(page.getByText("http://127.0.0.1:3457/v1")).toBeVisible();
  });

  test("tokens: every account state + bridge clients", async ({ page }) => {
    const f = loadFixtures(RW);
    // Pin the banned account's cooldown 30d out: the static fixture date
    // would otherwise age past "Nd" into "expiring" and break the countdown
    // assert on later runs.
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const banned = (Array.isArray(list) ? list : []).find(
      (t) => t.cooldown_active,
    );
    if (banned)
      banned.cooldown_until = new Date(Date.now() + 30 * 864e5).toISOString();
    await mockDashboard(page, { ...f, tokens });
    await page.goto(admin("tokens"));
    for (const n of [1, 2, 3, 4, 5]) {
      await expect(
        page.getByText(`Account #${n}`, { exact: true }).first(),
      ).toBeVisible();
    }
    await expect(page.getByText("BANNED (TEMPORARY)").first()).toBeVisible();
    await expect(
      page.getByText(/\d+d( \d+h)?\s+remaining/).first(),
    ).toBeVisible();
    await expect(page.getByText("LOCKED").first()).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Unlock" }).first(),
    ).toBeVisible();
    await expect(page.getByText("Bridge Clients")).toBeVisible();
    await expect(
      page.getByText(
        "2 active bridge client(s) relaying their own FreeBuff tokens",
      ),
    ).toBeVisible();
    await expect(page.getByText("Requests 37")).toBeVisible();
    // Referral banner: unlocked grant on account #2.
    await expect(
      page.getByText("premium session(s)/day from referrals").first(),
    ).toBeVisible();
    await expect(page.getByText("FREE-abc123").first()).toBeVisible();
    await expect(page.getByText("SPEND TODAY")).toHaveCount(2);
    await expect(page.getByText("Used 2 / Limit 4")).toBeVisible();
    await expect(page.getByText("Banned — TEMPORARY")).toBeVisible();
    // Drawer: standing + session + pinned models for the trusted account.
    await page.locator("table tbody tr button[aria-expanded]").first().click();
    await expect(page.getByText("Trusted").first()).toBeVisible();
    await expect(page.getByText("stealth/ox-alpha").first()).toBeVisible();
  });

  test("tokens: spawn posts the picked model as JSON", async ({ page }) => {
    await mockDashboard(page, loadFixtures(RW));
    let posted: unknown = null;
    await page.route("**/admin/tokens/*/session", async (route) => {
      try {
        posted = route.request().postDataJSON();
      } catch {
        posted = null;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "created" }),
      });
    });
    await page.goto(admin("tokens"));
    // The per-token Dev Session toolbar renders with DEVTOOLS_ENABLED=true.
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "PORT=3457\nAUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.reload();
    const table = page.locator("table.fp-table");
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(table.getByText("Dev Session:")).toBeVisible();
    const picker = table.locator("select").first();
    await expect(picker.locator("option")).not.toHaveCount(0);
    const options = await picker
      .locator("option")
      .evaluateAll((els) =>
        els.map((el) => (el as HTMLOptionElement).value).filter(Boolean),
      );
    await picker.selectOption(options[1]);
    page.on("dialog", (d) => d.accept());
    await table.getByRole("button", { name: "Make Session" }).first().click();
    await expect
      .poll(() => posted, { timeout: 8000 })
      .toEqual({ model: options[1] });
  });

  test("quota: streak, freebucks, traffic chips, cap banner, bridge note", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("quota"));
    await expect(
      page.getByRole("heading", { name: "Quota Tracker", exact: true }),
    ).toBeVisible();
    await expect(page.getByText("5 day streak")).toBeVisible();
    await expect(page.getByText("Active today")).toBeVisible();
    await expect(page.getByText("Balance 7.5")).toBeVisible();
    await expect(page.getByText("Used 2.5 / 10")).toBeVisible();
    await expect(page.getByText("Used 42 / 300")).toBeVisible();
    await expect(page.getByText("req/min")).toHaveCount(5);
    await expect(page.getByText("req/day")).toHaveCount(5);
    await expect(page.getByText("2/30")).toHaveCount(1);
    // Upstream header line (issue #354): daily · countdown · wallet ·
    // monthly, rendered per metered token above its quota bar.
    await expect(
      page.locator('[data-testid="freebucks-header"]').first(),
    ).toContainText(
      /7\.5\/10 Freebucks daily · resets in .* · 5 in wallet · \$258 monthly usage left/,
    );
    await expect(page.getByText("1500/1500")).toHaveCount(1);
    await expect(
      page.getByText("daily limit reached — resets 1h"),
    ).toBeVisible();
    await expect(page.getByText("cap reached").first()).toBeVisible();
    await expect(
      page.getByText("1 client(s) report premium quota"),
    ).toBeVisible();
  });

  test("models/logs/traces/metrics render production rows", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("models"));
    await expect(page.getByText("5 premium quota")).toHaveCount(4);
    await expect(page.getByText("unlimited session")).toHaveCount(8);
    await expect(
      page.getByText("meta/muse-spark-1.3-contributor").first(),
    ).toBeVisible();
    await expect(page.getByText("referral +1/day")).toHaveCount(2);
    await expect(page.getByText("referral", { exact: true })).toHaveCount(2);
    await expect(page.getByText("Premium: 4 of 5 used").first()).toBeVisible();
    await expect(page.getByText("$0.01/hr").first()).toBeVisible();
    await expect(page.getByText("low/high/max").first()).toBeVisible();
    await page.goto(admin("logs"));
    await expect(page.getByText("2 requests")).toBeVisible();
    await expect(page.getByText("502").first()).toBeVisible();
    await page.getByRole("button", { name: "Table" }).click();
    await expect(
      page.getByText("error handling request 1: upstream timeout"),
    ).toBeVisible();
    await expect(page.getByText("req_id=req-bbb2")).toHaveCount(4);
    await page.goto(admin("traces"));
    await expect(page.getByText("deepseek/deepseek-v4-flash")).toBeVisible();
    await expect(page.getByText("upstream timeout").first()).toBeVisible();
    await expect(page.getByText("acquire_ms")).toBeVisible();
    await page.goto(admin("metrics"));
    await expect(page.getByText("HIGH")).toBeVisible();
  });

  test("settings/setup render keys, traffic caps, hybrid bridge", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("settings"));
    await expect(
      page.getByText("Max Requests per Minute (per account)"),
    ).toBeVisible();
    await expect(page.getByText("MAX_REQUESTS_PER_MINUTE")).toBeVisible();
    await expect(page.getByText("MAX_REQUESTS_PER_DAY")).toBeVisible();
    await page.goto(admin("setup"));
    await expect(
      page.getByRole("heading", { name: "Setup", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "mimo/mimo-v2.5 default" }),
    ).toBeVisible();
  });
});
