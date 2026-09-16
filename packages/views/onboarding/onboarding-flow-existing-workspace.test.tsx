import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../locales/en/common.json";
import enOnboarding from "../locales/en/onboarding.json";
import enWorkspace from "../locales/en/workspace.json";

const TEST_RESOURCES = {
  en: { common: enCommon, onboarding: enOnboarding, workspace: enWorkspace },
};

/**
 * State the mocked store modules read. `vi.hoisted` because `vi.mock`
 * factories are hoisted above the imports and run before this file's own
 * top-level statements.
 */
const state = vi.hoisted(() => ({
  workspaces: [] as { id: string; name: string; slug: string }[],
  ready: true,
  questionnaire: {} as Record<string, unknown>,
}));

vi.mock("../auth", () => ({ useLogout: () => vi.fn() }));

vi.mock("@multica/core/config", () => ({
  useConfigStore: (
    selector: (s: {
      workspaceCreationDisabled: boolean;
      daemonAppUrl: string;
    }) => unknown,
  ) => selector({ workspaceCreationDisabled: false, daemonAppUrl: "" }),
}));

vi.mock("@multica/core/api", () => ({
  api: { getBaseUrl: () => "https://multica.ai" },
}));

vi.mock("@multica/core/workspace/mutations", () => ({
  useCreateWorkspace: () => ({ mutate: vi.fn(), isPending: false }),
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: Object.assign(
    (selector: (s: { user: unknown }) => unknown) =>
      selector({
        user: {
          id: "u-1",
          onboarding_questionnaire: state.questionnaire,
        },
      }),
    { getState: () => ({ user: { id: "u-1" } }) },
  ),
}));

vi.mock("@multica/core/workspace", () => ({
  useWorkspaceList: () => ({ workspaces: state.workspaces, ready: state.ready }),
}));

vi.mock("@multica/core/onboarding", async () => {
  const actual = await vi.importActual<Record<string, unknown>>(
    "@multica/core/onboarding",
  );
  return { ...actual, useBootstrapMika: () => ({ mutateAsync: vi.fn() }) };
});

// The same seam step-platform-fork.test.tsx uses: the runtime step's data
// source is swapped out so this file can reach it without a TanStack Query +
// WebSocket stack. This test only asks which step the run lands on.
vi.mock("./components/use-runtime-picker", () => ({
  useRuntimePicker: () => ({
    runtimes: [],
    selected: null,
    selectedId: null,
    setSelectedId: vi.fn(),
    hasRuntimes: false,
  }),
}));

import { OnboardingFlow } from "./onboarding-flow";

function renderFlow(props: Record<string, unknown> = {}) {
  // The runtime step reads the runtime list through React Query; this file
  // only cares which step the run lands on, so a bare client that never
  // resolves a query is enough.
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <OnboardingFlow
          onComplete={vi.fn()}
          // The web shell's slot: it selects the browser runtime fork, which —
          // unlike the desktop connect step — needs no daemon to render.
          runtimeInstructions={<>Install the CLI</>}
          {...props}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

/** Click past Welcome. `renderFlow` selects the web shell, whose CTA differs. */
async function leaveWelcome(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: /Continue on web/i }));
}

/** Walk from Welcome to About you, then answer it so Continue is enabled. */
async function walkToRuntime(user: ReturnType<typeof userEvent.setup>) {
  await leaveWelcome(user);
  await user.click(screen.getByRole("radio", { name: /engineer/i }));
  await user.click(screen.getByRole("button", { name: /^Continue$/i }));
}

describe("OnboardingFlow — a user the server already gave a workspace", () => {
  beforeEach(() => {
    state.workspaces = [];
    state.ready = true;
    state.questionnaire = {};
  });

  it("skips the workspace step so onboarding cannot create a second workspace", async () => {
    state.workspaces = [
      { id: "ws-personal", name: "Ada 的个人空间", slug: "user-abc" },
    ];
    const user = userEvent.setup();
    renderFlow();

    await walkToRuntime(user);

    // The regression: this step offered to create a workspace alongside the
    // server-provisioned one, leaving brand-new users with two.
    expect(
      screen.queryByRole("heading", { name: /Name your workspace/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: /Continue with/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("heading", {
        name: /Connect a computer to run your agent/i,
      }),
    ).toBeInTheDocument();
  });

  it("drops the workspace step from the progress rail", async () => {
    state.workspaces = [
      { id: "ws-personal", name: "Ada 的个人空间", slug: "user-abc" },
    ];
    const user = userEvent.setup();
    renderFlow();

    await leaveWelcome(user);

    expect(screen.getAllByText("About you").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Meet Mika").length).toBeGreaterThan(0);
    expect(screen.queryByText("Workspace")).not.toBeInTheDocument();
  });

  it("drops the workspace step even when the list only lands on About you", async () => {
    // Welcome is clickable before the workspace list resolves. Deciding the
    // run's steps at that moment would leave the create form in front of a user
    // who has a workspace — the #12 duplicate, narrowly — so the decision is
    // read live at the navigation that would enter the step.
    state.workspaces = [];
    state.ready = false;
    const user = userEvent.setup();
    renderFlow();

    await leaveWelcome(user);

    state.workspaces = [
      { id: "ws-personal", name: "Ada 的个人空间", slug: "user-abc" },
    ];
    state.ready = true;
    await user.click(screen.getByRole("radio", { name: /engineer/i }));
    await user.click(screen.getByRole("button", { name: /^Continue$/i }));

    expect(
      screen.queryByRole("heading", { name: /Name your workspace/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByRole("heading", {
        name: /Connect a computer to run your agent/i,
      }),
    ).toBeInTheDocument();
  });

  it("still asks for a workspace when the user has none", async () => {
    const user = userEvent.setup();
    renderFlow();

    await leaveWelcome(user);
    await user.click(screen.getByRole("radio", { name: /engineer/i }));
    await user.click(screen.getByRole("button", { name: /^Continue$/i }));

    expect(
      screen.getByRole("heading", { name: /Name your workspace/i }),
    ).toBeInTheDocument();
  });

  it("offers the done-this-before exit only to someone mid-onboarding", () => {
    state.workspaces = [
      { id: "ws-personal", name: "Ada 的个人空间", slug: "user-abc" },
    ];
    renderFlow();

    // A workspace is no longer evidence of a returning user — every signup gets
    // one — so the escape hatch stays hidden until they have actually started.
    expect(
      screen.queryByRole("button", { name: /I've done this before/i }),
    ).not.toBeInTheDocument();
  });

  it("keeps the exit for a user who already answered part of onboarding", () => {
    state.workspaces = [
      { id: "ws-personal", name: "Ada 的个人空间", slug: "user-abc" },
    ];
    state.questionnaire = { role: "engineer" };
    renderFlow();

    expect(
      screen.getByRole("button", { name: /I've done this before/i }),
    ).toBeInTheDocument();
  });
});
