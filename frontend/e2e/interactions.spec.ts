import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import type { Fixtures } from "./mocks.js";

// ---------------------------------------------------------------------------
// Clickable / interactable coverage (hermetic mocks).
//
// dashboard.spec.ts and ux.spec.ts pin data rendering and the main mutation
// flows; this suite pins every remaining button, toggle, select, radio and
// dialog on the operator path: token reorder/clear/probe/finish/drop-session,
// dialog dismiss, rotation radios, failover switch, log view/filter/paging
// controls, settings discard/bridge/rate-limit/password, setup key buttons,
// sidebar navigation and the overview error-retry path.
// ---------------------------------------------------------------------------

// Minimal token row mirroring the real /admin/api/tokens row shape.
function tokenRow(
  idx: number,
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    index: idx,
    email: `acct${idx}@example.com`,
    session_status: "idle",
    queue_position: 0,
    queue_depth: 0,
    active_runs: 0,
    requests: 0,
    messages_24h: 0,
    daily_limit: 0,
    usage_pct: 0,
    risk_level: "low",
    cooldown_active: false,
    cooldown_until: "",
    locked: false,
    transient_retries: 1,
    has_standing: false,
    session_instance: "",
    session_model: "",
    session_remaining_seconds: 0,
    has_quota: false,
    ...over,
  };
}

function tokensPayload(tokens: Array<Record<string, unknown>>) {
  return {
    mode: "pooled",
    in_bridge: false,
    bridge_tokens: 0,
    token_count: tokens.length,
    has_tokens: true,
    tokens,
    bridge_token_cards: [],
  };
}

// Settings fixtures carry a live .env so the form cards render with values.
function settingsConfig(f: Fixtures) {
  return {
    ...f.config,
    env_content:
      "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
    has_env_file: true,
  };
}

test.describe("operator interactions (hermetic mocks)", () => {
  test.use({ expect: { timeout: 10_000 } });

  // -------------------------------------------------------------------------
  // 1. Token reorder: Move Down posts from/to and reorders; Move Up is
  //    disabled on the first account and posts the reverse on the second.
  // -------------------------------------------------------------------------
  test("tokens: move down swaps pool order; move up disabled on first account", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = { tokens: [tokenRow(0), tokenRow(1), tokenRow(2)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    const swaps: Array<Record<string, unknown>> = [];
    await page.route("**/admin/tokens/swap", async (route) => {
      swaps.push(JSON.parse(route.request().postData() || "{}"));
      const { from, to } = swaps[swaps.length - 1] as {
        from: number;
        to: number;
      };
      const moved = state.tokens.splice(from, 1)[0];
      state.tokens.splice(to, 0, moved);
      state.tokens.forEach((t, i) => {
        t.index = i;
      });
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Pool order updated." }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const rows = page.locator("table tbody tr");
    await expect(rows).toHaveCount(3);
    const first = rows.filter({ hasText: "Account #1" });
    const second = rows.filter({ hasText: "Account #2" });
    const last = rows.filter({ hasText: "Account #3" });

    // Boundary states: nothing above the first account, nothing below the last.
    await expect(first.getByRole("button", { name: "Move Up" })).toBeDisabled();
    await expect(
      first.getByRole("button", { name: "Move Down" }),
    ).toBeEnabled();
    await expect(
      last.getByRole("button", { name: "Move Down" }),
    ).toBeDisabled();
    await expect(last.getByRole("button", { name: "Move Up" })).toBeEnabled();

    // Move Down on Account #1 swaps positions 0 and 1 (no confirm dialog).
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await first.getByRole("button", { name: "Move Down" }).click();
    await refetch;
    expect(swaps).toEqual([{ from: 0, to: 1 }]);
    await expect(page.getByText("Pool order updated.")).toBeVisible();
    // The second account's email now leads the table.
    await expect(rows.first().getByText("acct1@example.com")).toBeVisible();

    // Move Up on the (new) second row posts the reverse swap.
    const refetch2 = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await second.getByRole("button", { name: "Move Up" }).click();
    await refetch2;
    expect(swaps).toEqual([
      { from: 0, to: 1 },
      { from: 1, to: 0 },
    ]);
    await expect(rows.first().getByText("acct0@example.com")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 2. Clear cooldown posts the per-token unlock endpoint and clears the row.
  // -------------------------------------------------------------------------
  test("tokens: clear cooldown posts unlock and clears the warning", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = {
      tokens: [
        tokenRow(0, {
          cooldown_active: true,
          cooldown_until: new Date(Date.now() + 5 * 60_000).toISOString(),
          risk_level: "high",
        }),
        tokenRow(1),
      ],
    };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    await page.route("**/admin/tokens/0/unlock", async (route) => {
      state.tokens[0].cooldown_active = false;
      state.tokens[0].cooldown_until = "";
      state.tokens[0].risk_level = "low";
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Cooldown cleared." }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    await expect(row.getByRole("button", { name: "Clear" })).toBeVisible();

    const unlockReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/tokens/0/unlock"),
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    page.once("dialog", (d) => d.accept());
    await row.getByRole("button", { name: "Clear" }).click();
    await unlockReq;
    await refetch;
    await expect(page.getByText("Cooldown cleared.")).toBeVisible();
    await expect(row.getByRole("button", { name: "Clear" })).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 3. Drawer actions: probe, finish and drop-session post per-token endpoints.
  // -------------------------------------------------------------------------
  test("tokens: probe, finish and drop session post per-token endpoints", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = {
      tokens: [
        tokenRow(0, {
          session_instance: "inst-live-1",
          session_model: "openai/gpt-5.6-luna",
          session_remaining_seconds: 1800,
        }),
      ],
    };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    // DEVTOOLS on so the drawer Probe / Finish Runs toolbar renders.
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    const posts: string[] = [];
    for (const action of ["test", "finish", "drop-session"]) {
      await page.route(`**/admin/tokens/0/${action}`, async (route) => {
        posts.push(action);
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: `${action} done.` }),
        });
      });
    }

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    await row.locator('button[aria-label*="Expand details"]').click();
    await expect(
      page.locator("table").getByRole("button", { name: "Drop Session" }),
    ).toBeVisible();

    for (const [label, action] of [
      ["Probe", "test"],
      ["Finish Runs", "finish"],
      ["Drop Session", "drop-session"],
    ] as Array<[string, string]>) {
      const req = page.waitForRequest(
        (r) =>
          r.method() === "POST" &&
          r.url().includes(`/admin/tokens/0/${action}`),
      );
      page.once("dialog", (d) => d.accept());
      await page.locator("table").getByRole("button", { name: label }).click();
      await req;
      await expect(page.getByText(`${action} done.`)).toBeVisible();
    }
    expect(posts).toEqual(["test", "finish", "drop-session"]);
  });

  // -------------------------------------------------------------------------
  // 4. Dismissing the confirm dialog sends no request and keeps the row.
  // -------------------------------------------------------------------------
  test("tokens: dismissing the confirm dialog sends no request", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = { tokens: [tokenRow(0), tokenRow(1)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    let removePosted = false;
    await page.route("**/admin/tokens/remove", async (route) => {
      removePosted = true;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "removed" }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    page.once("dialog", (d) => d.dismiss());
    await row.getByRole("button", { name: "Remove", exact: true }).click();
    await page.waitForTimeout(800);
    expect(removePosted).toBe(false);
    await expect(
      page.locator("table tbody tr").filter({ hasText: "Account #1" }),
    ).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 5. Rotation radios and the failover switch persist via config save.
  // -------------------------------------------------------------------------
  test("tokens: rotation radio and failover switch persist via config save", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const bodies: string[] = [];
    await page.unroute(/\/admin\/config$/);
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === "POST") {
        bodies.push(decodeURIComponent(route.request().postData() || ""));
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "Config saved" }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const drain = page.getByRole("radio", { name: "Drain (Safest)" });
    const rr = page.getByRole("radio", { name: "Round Robin (1:1)" });
    await expect(drain).toHaveAttribute("aria-checked", "true");

    await rr.click();
    await expect(rr).toHaveAttribute("aria-checked", "true");
    expect(bodies[bodies.length - 1]).toContain("TOKEN_ROTATION=round_robin");

    const failover = page.getByRole("switch");
    await expect(failover).toHaveAttribute("aria-checked", "true");
    await failover.click();
    await expect(failover).toHaveAttribute("aria-checked", "false");
    expect(bodies[bodies.length - 1]).toContain("RATE_LIMIT_FAILOVER=false");
  });

  // -------------------------------------------------------------------------
  // 6. Logs: console/table toggle, auto toggle, refresh and clear console.
  // -------------------------------------------------------------------------
  test("logs: view toggle, auto toggle, refresh and clear console", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#logs");
    // Console is the default view.
    await expect(page.getByText("/v1 only")).toBeVisible();

    // Table view exposes the labelled filter controls.
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-level")).toBeVisible();
    await expect(page.locator("#log-msg")).toBeVisible();
    await page.getByRole("button", { name: "Console" }).click();
    await expect(page.getByText("/v1 only")).toBeVisible();

    // Auto toggle flips label and pauses the 1s poll.
    const auto = page.getByRole("button", { name: /^Auto / });
    await expect(auto).toContainText("Auto 1s");
    await auto.click();
    await expect(page.getByRole("button", { name: "Auto off" })).toBeVisible();

    // Manual refresh always fetches.
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/logs") && r.status() === 200,
    );
    await page.getByRole("button", { name: "Refresh" }).click();
    await refetch;

    // Clear wipes the console view behind a confirm dialog.
    page.once("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "Clear" }).click();
    await expect(
      page.getByText("No request activity recorded yet."),
    ).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 7. Logs table: level select, hide-admin toggle and clear filters.
  // -------------------------------------------------------------------------
  test("logs: level select, hide-admin toggle and clear filters", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#logs");
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-level")).toBeVisible();

    // Admin-path rows are hidden by default; an info /v1 row shows.
    await expect(page.getByText("request 0").first()).toBeVisible();
    await expect(page.getByText("request 1 completed")).toHaveCount(0);

    // Reveal admin rows.
    await page.getByRole("button", { name: "Hide admin" }).click();
    await expect(page.getByText("request 1 completed")).toBeVisible();

    // Level select filters server-side (?level=).
    const levelResp = page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/logs") &&
        r.url().includes("level=error") &&
        r.status() === 200,
    );
    await page.locator("#log-level").selectOption("error");
    await levelResp;
    await expect(page.getByText("request 2 completed")).toBeVisible();
    await expect(page.getByText("request 1 completed")).toHaveCount(0);

    // Clear filters resets to All levels and hides admin rows again (it must
    // not apply ?level=info — that would silently drop warn/error rows).
    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.locator("#log-level")).toHaveValue("");
    await expect(page.getByText("request 1 completed")).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 8. Logs table: rows-per-page select collapses pagination.
  // -------------------------------------------------------------------------
  test("logs: rows-per-page select collapses pagination", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#logs");
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#logs-page-size")).toBeVisible();
    await expect(page.getByText("Page 1 /", { exact: false })).toBeVisible();

    await page.locator("#logs-page-size").selectOption("100");
    await expect(page.getByText("Page 1 / 1")).toBeVisible();
    await expect(page.getByRole("button", { name: "Next" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Prev" })).toBeDisabled();
  });

  // -------------------------------------------------------------------------
  // 9. Settings: discard reverts a dirty form without posting.
  // -------------------------------------------------------------------------
  test("settings: discard reverts a dirty form without posting", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { configWithApiKeys: settingsConfig(f) });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;

    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toHaveAttribute("aria-checked", "true");
    await safeMode.click();
    await expect(safeMode).toHaveAttribute("aria-checked", "false");
    await expect(page.getByText("Unsaved changes")).toBeVisible();

    let posted = false;
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === "POST") {
        posted = true;
      }
      await route.continue();
    });
    await page.getByRole("button", { name: "Discard" }).first().click();
    await expect(safeMode).toHaveAttribute("aria-checked", "true");
    await expect(page.getByText("Unsaved changes")).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Save Changes", exact: true }),
    ).toBeDisabled();
    expect(posted).toBe(false);
  });

  // -------------------------------------------------------------------------
  // 10. Settings: bridge toggle and rate-limit input persist into the save.
  // -------------------------------------------------------------------------
  test("settings: bridge toggle and rate-limit input persist into the save", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { configWithApiKeys: settingsConfig(f) });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;

    // Absent from .env, the bridge switch defaults to on.
    const bridge = page.getByRole("switch", { name: "BRIDGE_ENABLED" });
    await expect(bridge).toHaveAttribute("aria-checked", "true");
    await bridge.click();
    await page.locator('input[aria-label="RATE_LIMIT_PER_IP"]').fill("25");
    // Per-account request limits (MAX_REQUESTS_PER_MINUTE / _PER_DAY) are
    // user-facing quota rows in the same Pool card.
    await page
      .locator('input[aria-label="MAX_REQUESTS_PER_MINUTE"]')
      .fill("40");
    await page.locator('input[aria-label="MAX_REQUESTS_PER_DAY"]').fill("800");

    let savedBody = "";
    await page.route(/\/admin\/config$/, async (route) => {
      if (route.request().method() === "POST") {
        savedBody = decodeURIComponent(route.request().postData() || "");
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "Saved." }),
        });
      } else {
        await route.continue();
      }
    });
    page.once("dialog", (d) => d.accept());
    await page
      .getByRole("button", { name: "Save Changes", exact: true })
      .click();
    expect(savedBody).toContain("BRIDGE_ENABLED=false");
    expect(savedBody).toContain("RATE_LIMIT_PER_IP=25");
    expect(savedBody).toContain("MAX_REQUESTS_PER_MINUTE=40");
    expect(savedBody).toContain("MAX_REQUESTS_PER_DAY=800");
    await expect(page.getByText("Saved.")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 11. Settings: password form validates, then submits successfully.
  test("settings: password form validates, then submits successfully", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { configWithApiKeys: settingsConfig(f) });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    await expect(page.getByRole("heading", { name: "Security" })).toBeVisible();

    // Eye toggles reveal the password text.
    const eyes = page.getByRole("button", { name: "Show password" });
    await expect(eyes.first()).toBeVisible();
    await eyes.first().click();
    await expect(
      page.getByRole("button", { name: "Hide password" }).first(),
    ).toBeVisible();

    // Mismatch surfaces the inline error and keeps submit disabled.
    await page.locator("#sec-current-password").fill("oldpass1");
    await page.locator("#sec-new-password").fill("newpass123");
    await page.locator("#sec-confirm-password").fill("different");
    await expect(page.getByText("Passwords do not match")).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Update Password" }),
    ).toBeDisabled();

    // Matching passwords enable submit; the mocked endpoint succeeds and the
    // success alert survives (refetches are silent once mounted).
    await page.locator("#sec-confirm-password").fill("newpass123");
    const submit = page.getByRole("button", { name: "Update Password" });
    await expect(submit).toBeEnabled();
    const changeReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/api/change-password"),
    );
    await submit.click();
    const req = await changeReq;
    expect(JSON.parse(req.postData() || "{}")).toEqual({
      current_password: "oldpass1",
      new_password: "newpass123",
    });
    await expect(page.locator("#sec-current-password")).toHaveValue("");
    await expect(page.locator("#sec-new-password")).toHaveValue("");
    await expect(page.locator("#sec-confirm-password")).toHaveValue("");
    await expect(page.getByText("Passwords do not match")).toHaveCount(0);
    await expect(page.getByText("Password changed")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 12. Setup: generate fills a key, reset restores, model buttons render.
  // -------------------------------------------------------------------------
  test("setup: generate fills a key, reset restores, model buttons render", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    const setupResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/setup") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/setup");
    await setupResp;
    await page
      .context()
      .grantPermissions(["clipboard-read", "clipboard-write"]);
    const keyInput = page.locator("#setup-api-key");
    await expect(keyInput).toHaveValue("not-needed");
    const reset = page.getByRole("button", { name: "Reset" });
    await expect(reset).toBeDisabled();

    await page.getByRole("button", { name: "Generate" }).click();
    const generated = await keyInput.inputValue();
    expect(generated).not.toBe("not-needed");
    expect(generated.length).toBeGreaterThan(8);
    await expect(reset).toBeEnabled();
    await reset.click();
    await expect(keyInput).toHaveValue("not-needed");

    const modelBtns = page.getByTitle("Copy model ID");
    await expect(modelBtns.first()).toBeVisible();
    await modelBtns.first().click();
    await expect(page.getByText("Copied").first()).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 13. Sidebar reaches every section.
  // -------------------------------------------------------------------------
  test("nav: sidebar links reach every section", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    const nav = page.locator(
      'aside[aria-label="Sidebar"] nav[aria-label="Main navigation"]',
    );

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    for (const [link, heading] of [
      ["Tokens", "Tokens"],
      ["Quota Tracker", "Quota Tracker"],
      ["Models", "Models"],
      ["Logs", "Logs"],
      ["Settings", "Settings"],
    ] as Array<[string, string]>) {
      await nav.getByRole("link", { name: link }).click();
      await expect(
        page.getByRole("heading", { name: heading, exact: true }),
      ).toBeVisible();
    }
  });

  // -------------------------------------------------------------------------
  // 14. Overview: a failed load shows Retry and recovers on click.
  // -------------------------------------------------------------------------
  test("overview: failed load shows retry and recovers on click", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    let calls = 0;
    await page.unroute("**/admin/api/overview*");
    await page.route("**/admin/api/overview*", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.overview),
        });
      }
    });

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    await expect(page.getByText("Pool total")).toBeVisible();
  });
});
