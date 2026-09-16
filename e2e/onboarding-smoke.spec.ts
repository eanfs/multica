import { test, expect } from "@playwright/test";
import { TestApiClient } from "./fixtures";
import { waitForPageText } from "./helpers";

// Smoke test for the onboarding flow: welcome → About you (role +
// use case on ONE screen) → runtime. The source question is
// intentionally absent — it moved to the workspace source-backfill
// prompt (MUL-5159). Captures screenshots for review. Uses a unique
// email per run so the user is always a fresh, un-onboarded user
// landing on /onboarding.
//
// There is no workspace step: signup provisions the user a personal
// workspace, so the run has nothing to name and must not offer to create a
// second one (#12). The step still exists for `new_workspace` mode.

const EMAIL = `onboarding-v3-${Date.now()}@localhost`;
const SHOTS_DIR = "../shots-rail";

test.use({ viewport: { width: 1440, height: 900 } });

test("onboarding — welcome → about you → runtime, in one workspace", async ({ page }) => {
  const api = new TestApiClient();
  await api.login(EMAIL, "OBv3 Tester");
  const token = api.getToken();

  await page.addInitScript((t) => {
    localStorage.setItem("multica_token", t);
  }, token);
  await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
  await waitForPageText(page, "Continue on web");

  // 1. Welcome screen
  await expect(page.getByRole("button", { name: "Continue on web" })).toBeVisible({ timeout: 15000 });
  await page.screenshot({ path: `${SHOTS_DIR}/01-welcome.png`, fullPage: false });

  // Click Continue on web to advance to About you
  await page.getByRole("button", { name: "Continue on web" }).click();

  // 2. About you step — both questions live on this one screen and the
  //    source question must NOT exist anywhere in the flow.
  await expect(page.getByText("Tell us a bit about you.")).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("Which best describes you?")).toBeVisible();
  await expect(page.getByText("What do you want to use Multica for?")).toBeVisible();
  // The rail names every step and marks the current one; the ordinal
  // counter it replaced is gone. Two steps, not three: the provisioned
  // workspace drops the workspace step (#12), and nothing on this screen
  // offers to create another one.
  await expect(page.locator('[data-slot="stepper-title"]')).toHaveText([
    "About you",
    "Meet Mika",
  ]);
  await expect(
    page.locator('[aria-current="step"]').filter({ hasText: "About you" }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: /Name your workspace/i })).toHaveCount(0);
  await expect(page.getByText("How did you hear about Multica?")).toHaveCount(0);
  await page.waitForTimeout(500);
  await page.screenshot({ path: `${SHOTS_DIR}/02-about-you.png` });

  // Answer both groups, then Continue → the runtime step directly.
  await page.getByRole("radio", { name: /Engineer \/ developer/i }).click();
  await page.getByRole("checkbox", { name: /Ship code with AI agents/i }).click();
  await page.getByRole("button", { name: "Continue" }).click();

  // 3. Runtime step — "About you" is behind us and the rail marks
  //    "Meet Mika" current.
  await expect(
    page.getByRole("heading", { name: /Connect a computer/i }),
  ).toBeVisible({ timeout: 10000 });
  await expect(
    page.locator('[aria-current="step"]').filter({ hasText: "Meet Mika" }),
  ).toBeVisible();
  await page.waitForTimeout(800);
  await page.screenshot({ path: `${SHOTS_DIR}/03-runtime.png` });

  // #12's acceptance: walking onboarding added no second workspace to the one
  // signup provisioned.
  await expect.poll(async () => (await api.getWorkspaces()).length).toBe(1);
});

test("onboarding — one skip clears the whole questionnaire step", async ({ page }) => {
  const api = new TestApiClient();
  await api.login(`skip-${Date.now()}@localhost`, "Skipper");
  const token = api.getToken();

  await page.addInitScript((t) => localStorage.setItem("multica_token", t), token);
  await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
  await waitForPageText(page, "Continue on web");

  await page.getByRole("button", { name: "Continue on web" }).click();
  await expect(page.getByText("Tell us a bit about you.")).toBeVisible({ timeout: 10000 });

  // A single Skip covers role + use case. The next stop is the runtime step,
  // not a workspace step: this user already has the workspace signup
  // provisioned (#12).
  await page.getByRole("button", { name: "Skip" }).click();
  await expect(
    page.getByRole("heading", { name: /Connect a computer/i }),
  ).toBeVisible({ timeout: 10000 });
  await page.waitForTimeout(600);
  await page.screenshot({ path: `${SHOTS_DIR}/04-after-skip.png` });

  await expect.poll(async () => (await api.getWorkspaces()).length).toBe(1);
});

test("onboarding — zh-Hans renders Chinese labels", async ({ page, context, baseURL }) => {
  await context.addCookies([
    {
      name: "multica-locale",
      value: "zh-Hans",
      url: baseURL ?? "http://localhost:3000",
    },
  ]);
  const api = new TestApiClient();
  await api.login(`zh-${Date.now()}@localhost`, "中文用户");
  const token = api.getToken();

  await page.addInitScript((t) => localStorage.setItem("multica_token", t), token);
  await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
  await waitForPageText(page, "在 web 端继续");

  // Click the CTA by name. `getByRole("button").first()` used to stand in for
  // it, but the welcome screen renders the pinned Log out button first in DOM
  // order — so this step was signing the user out and the assertions below
  // were waiting on a page that had already redirected to login.
  await page.getByRole("button", { name: "在 web 端继续" }).click();

  // About-you screen — Chinese headline + both sub-questions.
  await expect(page.getByText("简单介绍一下你自己。")).toBeVisible({ timeout: 10000 });
  await expect(page.getByText("哪一项最符合你？")).toBeVisible();
  await page.waitForTimeout(500);
  await page.screenshot({ path: `${SHOTS_DIR}/05-about-you-zh.png` });
});
