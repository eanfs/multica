/**
 * Aurora-on-Fleet end-to-end (Task 7, browser half).
 *
 * Gated on MULTICA_RUN_DOCKER_INTEGRATION=1 before any API or Docker access and
 * excluded from the default Playwright project, so a normal browser run never
 * talks to a Fleet host. Physical execution requires the owned managed
 * environment; a skip is not a pass.
 *
 * The generation is created through the same API the Aurora composer calls, so
 * the assertion sequence (create -> provision -> execute -> aurora_asset ->
 * completed with credits_charged) is the real path. The Cloud Runtime page is
 * the browser-visible execution plane; the in-app Aurora composer interaction
 * belongs to Task 8 and is not faked here.
 */
import { expect, test, type Page } from "@playwright/test";
import { TestApiClient } from "./fixtures";
import { createTestApi } from "./helpers";

const GENERATION_TIMEOUT_MS = 900000;
const NODE_READY_TIMEOUT_MS = 300000;
const ROUND_TRIP_PROMPT = "aurora e2e fleet round trip poster";

let api: TestApiClient | null = null;

async function loginFleetBrowser(page: Page, client: TestApiClient): Promise<void> {
  const token = client.getToken();
  if (!token) throw new Error("aurora E2E client is not logged in");
  await page.addInitScript((value) => {
    localStorage.setItem("multica_token", value);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, token);
}

test.afterEach(async ({}, testInfo) => {
  // Deleting the managed node is owned by the server reaper; the client
  // cleanup removes only the SQL-owner fixtures this spec created.
  testInfo.setTimeout(180000);
  const client = api;
  api = null;
  await client?.cleanup().catch(() => {});
});

test("Aurora generation completes on a Fleet node and settles credits", async ({ page }) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  await loginFleetBrowser(page, client);

  // The create call provisions the sandbox and enqueues the skill. It must not
  // be a 503: a missing Aurora runtime is a hard failure, never a skip.
  const generation = await client.createAuroraGeneration("xhs-image", ROUND_TRIP_PROMPT);
  expect(generation.id).not.toBe("");

  // Poll the authoritative detail endpoint. expect.poll drives the clock; a
  // timeout fails the test instead of being read as a pass.
  await expect
    .poll(async () => (await client.getAuroraGeneration(generation.id)).status, {
      timeout: GENERATION_TIMEOUT_MS,
      message: `aurora generation ${generation.id} did not settle`,
    })
    .toMatch(/completed|failed/);

  const settled = await client.getAuroraGeneration(generation.id);
  expect(settled.status).toBe("completed");
  expect(settled.creditsCharged).toBeGreaterThan(0);
  expect(settled.assets.length).toBeGreaterThan(0);

  // The browser sees the execution plane: the workspace's managed Fleet node is
  // listed on the Cloud Runtime page and reports ready.
  await page.goto(`/${client.getFleetWorkspaceSlug()}/runtimes`, { waitUntil: "domcontentloaded" });
  await expect
    .poll(async () => (await client.listFleetNodes()).some((node) => node.ready), {
      timeout: NODE_READY_TIMEOUT_MS,
    })
    .toBe(true);
  await expect(page.getByText(/aurora-sandbox|aurora/i).first()).toBeVisible({
    timeout: NODE_READY_TIMEOUT_MS,
  });
});
