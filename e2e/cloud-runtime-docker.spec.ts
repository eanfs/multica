import { test, expect } from "@playwright/test";

import { loginAsDefault } from "./helpers";

// The local Docker Fleet is an optional environment component. This suite is
// opt-in because it needs a running Fleet service (make up C=api,web,fleet) and
// a signed-in worktree, neither of which the default E2E run provides.
const enabled = process.env.MULTICA_RUN_FLEET_E2E === "1";

test.describe("cloud runtime with local docker fleet", () => {
  test.skip(!enabled, "set MULTICA_RUN_FLEET_E2E=1 with a running fleet component");

  test("opens the capability-driven docker entry and shows its spec", async ({ page }) => {
    const workspaceSlug = await loginAsDefault(page);

    const capabilityResponse = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        response.request().method() === "GET" &&
        url.pathname.endsWith("/api/cloud-runtime/")
      );
    });

    await page.goto("/" + workspaceSlug + "/runtimes", { waitUntil: "domcontentloaded" });

    const capability = await capabilityResponse;
    expect(capability.status()).toBe(200);
    const service = await capability.json();
    expect(service.provider).toBe("docker");
    expect(Array.isArray(service.specs)).toBe(true);
    expect(service.specs.length).toBeGreaterThan(0);

    // The entry appears only because the capability response advertises it.
    await page
      .getByRole("button", { name: "Add runtime" })
      .filter({ hasText: "Add runtime" })
      .click();
    await page.getByRole("menuitem", { name: /Cloud Runtime/ }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText("Cloud Runtime", { exact: true }).first()).toBeVisible();
    await expect(dialog.getByText(/^Provider:/)).toContainText("docker");
    await expect(dialog.getByText("Persistent volume")).toBeVisible();

    // The provider's allowed spec is the only selectable size.
    const spec = dialog.getByLabel("Spec");
    await expect(spec).toBeVisible();
    await expect(spec).toContainText(service.specs[0].id);
    await expect(dialog.getByRole("button", { name: "Create node" })).toBeEnabled();
  });
});
