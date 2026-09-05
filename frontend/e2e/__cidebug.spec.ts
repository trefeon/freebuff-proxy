import { test } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
test("debug countdown ci2", async ({ page }) => {
  const f = loadFixtures("e2e/fixtures-realworld");
  const tokens = JSON.parse(JSON.stringify(f.tokens));
  const list = tokens.tokens ?? tokens;
  const banned = (Array.isArray(list) ? list : []).find(
    (t) => t.cooldown_active,
  );
  if (banned)
    banned.cooldown_until = new Date(Date.now() + 30 * 864e5).toISOString();
  await mockDashboard(page, { ...f, tokens });
  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  await page.getByText("BANNED (TEMPORARY)").first().waitFor();
  console.log("ROWS:", await page.locator("table tbody tr").count());
  const row = page
    .locator("table tbody tr", { hasText: "BANNED (TEMPORARY)" })
    .first();
  console.log("ROW-VIS:", await row.isVisible().catch(() => "err"));
  console.log(
    "ROW-HTML:",
    await row
      .evaluate((e) => e.outerHTML.slice(0, 900))
      .catch((e) => String(e).slice(0, 120)),
  );
});
