/**
 * Regression for #57 — `ensureWorkspace` must resolve the workspace it was
 * asked for.
 *
 * Login auto-provisions a personal workspace, so the workspace list is never
 * empty. The helper used to fall back to `workspaces[0]` — that personal
 * workspace — so the named E2E workspace was never created and the suite ran
 * against a workspace no spec had named.
 *
 * Stays at the HTTP layer: the defect is in how the helper reconciles the
 * provisioned list with the requested slug, not in any rendered surface.
 */
import "./env";
import { expect, test } from "@playwright/test";
import { TestApiClient } from "./fixtures";
import {
  DEFAULT_E2E_EMAIL,
  DEFAULT_E2E_NAME,
  DEFAULT_E2E_WORKSPACE,
  DEFAULT_E2E_WORKSPACE_NAME,
} from "./helpers";

// Distinct slugs so each test owns its "does this slug exist yet" precondition,
// even when another spec file in the same run already created the shared
// workspace.
const FRESH_SLUG = `${DEFAULT_E2E_WORKSPACE}-created`;
const EXISTING_SLUG = `${DEFAULT_E2E_WORKSPACE}-selected`;

async function loginAsDefaultUser() {
  const api = new TestApiClient();
  await api.login(DEFAULT_E2E_EMAIL, DEFAULT_E2E_NAME);
  return api;
}

test("ensureWorkspace creates the requested slug instead of returning the personal workspace", async () => {
  const api = await loginAsDefaultUser();

  // Precondition: login provisioned a personal workspace and the requested
  // slug is not among the user's workspaces — the exact state the old fallback
  // collapsed into "return the personal workspace".
  const provisioned = await api.getWorkspaces();
  expect(provisioned.length).toBeGreaterThan(0);
  expect(provisioned.map((item) => item.slug)).not.toContain(FRESH_SLUG);

  const workspace = await api.ensureWorkspace(DEFAULT_E2E_WORKSPACE_NAME, FRESH_SLUG);

  expect(workspace.slug).toBe(FRESH_SLUG);
  expect(api.getWorkspace()).toEqual({ id: workspace.id, slug: FRESH_SLUG });

  const slugs = (await api.getWorkspaces()).map((item) => item.slug);
  expect(slugs).toContain(FRESH_SLUG);
  // The provisioned workspace is untouched: the helper adds, it does not swap.
  expect(slugs).toContain(provisioned[0]!.slug);
});

test("ensureWorkspace re-selects an existing slug instead of creating a duplicate", async () => {
  const api = await loginAsDefaultUser();
  const created = await api.ensureWorkspace(DEFAULT_E2E_WORKSPACE_NAME, EXISTING_SLUG);

  // Every spec after the first one hits this state: the workspace already
  // exists, so the call must select it by slug.
  const selected = await api.ensureWorkspace("Name that must be ignored", EXISTING_SLUG);

  expect(selected.id).toBe(created.id);
  expect(selected.slug).toBe(EXISTING_SLUG);
  expect((await api.getWorkspaces()).filter((item) => item.slug === EXISTING_SLUG)).toHaveLength(1);
});
