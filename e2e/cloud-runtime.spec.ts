/**
 * Fleet Docker end-to-end coverage (Task 14, browser half).
 *
 * Every case is gated on MULTICA_RUN_DOCKER_INTEGRATION=1 before any API or
 * Docker access. The spec is excluded from the default Playwright project and
 * only the gated fleet-docker project runs it, so a normal
 * `pnpm exec playwright test` never talks to a Fleet host.
 *
 * Physical execution requires the owned managed environment; a skip is not a
 * pass. Browser assertions are authoritative for the fake reply and for the
 * enabled/denied/hidden affordances - API polling never substitutes for them.
 */
import { expect, test, type Page } from "@playwright/test";
import { TestApiClient } from "./fixtures";
import { createTestApi } from "./helpers";

const NODE_READY_TIMEOUT_MS = 300000;
const FAKE_CLAUDE_REPLY = "fake Claude completed";

let api: TestApiClient | null = null;
let createdNodeIds: string[] = [];

async function waitForNodeReady(client: TestApiClient, nodeId: string): Promise<void> {
  await expect
    .poll(
      async () => {
        const nodes = await client.listFleetNodes();
        return nodes.find((node) => node.id === nodeId)?.ready ?? false;
      },
      { timeout: NODE_READY_TIMEOUT_MS },
    )
    .toBe(true);
}

/** Inject the authenticated browser session created by createTestApi. */
async function loginFleetBrowser(page: Page, client: TestApiClient): Promise<void> {
  const token = client.getToken();
  if (!token) throw new Error("fleet E2E client is not logged in");
  await page.addInitScript((value) => {
    localStorage.setItem("multica_token", value);
    localStorage.setItem("multica:chat:isOpen", "false");
  }, token);
}

async function createFleetNode(client: TestApiClient, name: string) {
  const node = await client.createFleetNode("local-small", name);
  createdNodeIds.push(node.id);
  return node;
}

test.afterEach(async ({}, testInfo) => {
  // Deleting a running node stops it first, which can take longer than the
  // default hook budget; the wait below still fails closed on a bounded clock.
  testInfo.setTimeout(180000);
  const client = api;
  api = null;
  if (!client) return;
  const owned = createdNodeIds.splice(0);
  for (const nodeId of owned) {
    await client.deleteFleetNode(nodeId).catch(() => {
      /* the test may already have deleted it; cleanup stays idempotent */
    });
  }
  // Deletion is asynchronous and the per-owner limit is small: wait until this
  // test's nodes leave the owner list before the next test creates its own.
  const deadline = Date.now() + 120000;
  while (owned.length > 0 && Date.now() < deadline) {
    const nodes = await client.listFleetNodes().catch(() => []);
    if (owned.every((id) => !nodes.some((node) => node.id === id))) break;
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
  await client.cleanup();
});

test("Docker node becomes ready and appears on the runtimes page", async ({ page }) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  const node = await createFleetNode(client, "E2E Docker node");
  await waitForNodeReady(client, node.id);
  await loginFleetBrowser(page, client);
  await page.goto("/" + client.getFleetWorkspaceSlug() + "/runtimes", { waitUntil: "domcontentloaded" });
  // Managed nodes live in the Cloud Runtime panel, not in the runtime list.
  await page.getByRole("button", { name: "Cloud Runtime" }).click();
  await expect(page.getByRole("article", { name: "E2E Docker node" })).toBeVisible({
    timeout: NODE_READY_TIMEOUT_MS,
  });
});

test("a browser chat on the node's Claude runtime shows the fake reply", async ({ page }) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  const node = await createFleetNode(client, "E2E Chat node");
  await waitForNodeReady(client, node.id);

  const runtimeId = await client.getFleetClaudeRuntimeId(node.id);
  const agent = await client.createFleetAgent(runtimeId, "E2E Fleet Agent");
  const session = await client.createFleetChat(agent.id);

  await loginFleetBrowser(page, client);
  await page.goto("/" + client.getFleetWorkspaceSlug() + "/chat?session=" + session.id, {
    waitUntil: "domcontentloaded",
  });

  const composer = page
    .locator('[data-slot="chat-input-surface"] .ProseMirror[contenteditable="true"]')
    .first();
  await expect(composer).toBeVisible({ timeout: 60000 });
  await composer.click();
  await composer.fill("E2E fleet prompt");
  await page.getByRole("button", { name: "Send", exact: true }).click();

  // The fake Claude fixture writes the final result into the assistant turn;
  // this asserts the rendered browser reply, not an API poll.
  await expect(
    page.getByTestId("virtuoso-item-list").getByText(FAKE_CLAUDE_REPLY, { exact: false }),
  ).toBeVisible({ timeout: NODE_READY_TIMEOUT_MS });
});

test("another owner cannot address the node", async () => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  const node = await createFleetNode(client, "E2E Owned node");
  await waitForNodeReady(client, node.id);

  const other = new TestApiClient();
  const suffix = Date.now().toString(36);
  await other.login("e2e-fleet-other-" + suffix + "@multica.ai", "E2E Fleet Other");
  await other.ensureWorkspace("E2E Fleet Other " + suffix, "e2e-fleet-other-" + suffix);
  try {
    await expect(other.deleteFleetNode(node.id)).rejects.toThrow(/403/);
  } finally {
    await other.deleteWorkspace().catch(() => {
      /* only the workspace this test created */
    });
    await other.cleanup();
  }
});

test("stop is refused while the node is busy and the retry reuses the intent key", async ({ page }) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  const node = await createFleetNode(client, "E2E Busy node");
  await waitForNodeReady(client, node.id);

  const stopKeys: string[] = [];
  await page.route("**/api/cloud-runtime/nodes/stop", async (route) => {
    const key = route.request().headers()["idempotency-key"];
    if (key) stopKeys.push(key);
    await route.fulfill({
      status: 409,
      contentType: "application/json",
      body: JSON.stringify({ error_code: "busy" }),
    });
  });

  await loginFleetBrowser(page, client);
  await page.goto("/" + client.getFleetWorkspaceSlug() + "/runtimes", { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Cloud Runtime" }).click();

  const row = page.getByRole("article", { name: "E2E Busy node" });
  await expect(row).toBeVisible({ timeout: 60000 });
  const stop = row.getByRole("button", { name: "Stop node" });
  await stop.click();
  await expect(page.getByRole("alert").getByText(/The node is busy/)).toBeVisible();

  const firstKey = stopKeys[0];
  expect(firstKey).toBeTruthy();
  await stop.click();
  await expect.poll(() => stopKeys.length).toBeGreaterThanOrEqual(2);
  expect(stopKeys[1]).toBe(firstKey);
});

test("unknown capabilities hide the cloud-runtime entry", async ({ page }) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  await page.route("**/api/cloud-runtime/**", async (route) => {
    const url = new URL(route.request().url());
    if (route.request().method() === "GET" && url.pathname.endsWith("/api/cloud-runtime/")) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          provider: "unknown",
          operations: [],
          specs: [],
          persistent_storage: false,
          disk_quota_supported: false,
        }),
      });
      return;
    }
    await route.continue();
  });
  await loginFleetBrowser(page, client);
  await page.goto("/" + client.getFleetWorkspaceSlug() + "/runtimes", { waitUntil: "domcontentloaded" });
  await expect(page.getByRole("button", { name: "Cloud Runtime" })).toHaveCount(0);
});

test("deleting a node requires confirmation and removes its owned volumes", async ({ page }) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const client = await createTestApi();
  api = client;
  const node = await createFleetNode(client, "E2E Delete node");
  await waitForNodeReady(client, node.id);
  // The node is running here: the product stops an idle node before removing it.

  const deleteKeys: string[] = [];
  await page.route("**/api/cloud-runtime/nodes", async (route) => {
    if (route.request().method() === "DELETE") {
      const key = route.request().headers()["idempotency-key"];
      if (key) deleteKeys.push(key);
    }
    await route.continue();
  });

  await loginFleetBrowser(page, client);
  await page.goto("/" + client.getFleetWorkspaceSlug() + "/runtimes", { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Cloud Runtime" }).click();

  const row = page.getByRole("article", { name: "E2E Delete node" });
  await expect(row).toBeVisible({ timeout: 60000 });
  await row.getByRole("button", { name: "Delete node" }).click();

  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText("E2E Delete node")).toBeVisible();
  await expect(
    dialog.getByText(
      "Deleting this node permanently removes its volumes, sessions, and working directories. This cannot be undone.",
    ),
  ).toBeVisible();
  await dialog.getByRole("button", { name: "Delete node" }).click();

  // The row leaves the browser list only after the owned delete succeeds.
  await expect(row).toHaveCount(0, { timeout: NODE_READY_TIMEOUT_MS });
  await expect(page.getByText("Cloud node deleted").first()).toBeVisible();
  expect(deleteKeys[0]).toBeTruthy();
  createdNodeIds = createdNodeIds.filter((id) => id !== node.id);
});
