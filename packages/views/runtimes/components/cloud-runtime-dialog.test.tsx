// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { workspaceKeys } from "@multica/core/workspace/queries";
import {
  CloudRuntimeCapabilitiesSchema,
  CloudRuntimeNodeSchema,
} from "@multica/core/api/schemas";
import type { Workspace } from "@multica/core/types";
import { createAuthStore } from "@multica/core/auth";
import { WSProvider } from "@multica/core/realtime";
import { parseWithFallback } from "@multica/core/api/schema";
import {
  cloudRuntimeKeys,
  EMPTY_CLOUD_RUNTIME_CAPABILITIES,
  type CloudRuntimeCapabilities,
  type CloudRuntimeNode,
} from "@multica/core/runtimes";
import { api } from "@multica/core/api";
import { NavigationProvider } from "../../navigation";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { CloudRuntimeDialog } from "./cloud-runtime-dialog";
import { RuntimesPage } from "./runtimes-page";

vi.mock("@multica/core/api", () => ({
  api: {
    listWorkspaces: vi.fn(),
    listCloudRuntimeNodes: vi.fn(),
    getCloudRuntimeCapabilities: vi.fn(),
    createCloudRuntimeNode: vi.fn(),
    deleteCloudRuntimeNode: vi.fn(),
    startCloudRuntimeNode: vi.fn(),
    stopCloudRuntimeNode: vi.fn(),
    rebootCloudRuntimeNode: vi.fn(),
    listRuntimes: vi.fn(),
    listRuntimeProfiles: vi.fn(),
    listAgents: vi.fn(),
    listChatSessions: vi.fn(),
    getAgentTaskSnapshot: vi.fn(),
  },
  ApiError: class ApiError extends Error {},
}));
vi.mock("@multica/core/auth", async () => {
  const actual =
    await vi.importActual<typeof import("@multica/core/auth")>(
      "@multica/core/auth",
    );
  const state = { user: { id: "owner" }, isLoading: false };
  return {
    ...actual,
    useAuthStore: Object.assign(
      (select: (s: typeof state) => unknown) => select(state),
      { getState: () => state },
    ),
  };
});
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
function capabilitiesFixture(data: unknown): CloudRuntimeCapabilities {
  const parsed = parseWithFallback(
    data,
    CloudRuntimeCapabilitiesSchema,
    EMPTY_CLOUD_RUNTIME_CAPABILITIES,
    { endpoint: "fixture" },
  );
  return {
    ...parsed,
    provider:
      parsed.provider === "docker" || parsed.provider === "cloud"
        ? parsed.provider
        : "unknown",
  };
}
function nodeFixture(data: unknown): CloudRuntimeNode {
  const parsed = CloudRuntimeNodeSchema.parse(data);
  return {
    ...parsed,
    provider:
      parsed.provider === "docker" || parsed.provider === "cloud"
        ? parsed.provider
        : "unknown",
  };
}
const caps = capabilitiesFixture({
  provider: "docker",
  operations: ["create", "start", "stop", "reboot", "delete"],
  specs: [{ id: "local-small", cpus: 2, memory_bytes: 4294967296, pids: 256 }],
  persistent_storage: true,
  disk_quota_supported: false,
});
const node = nodeFixture({
  id: "a09fe54c-0740-467f-812a-114e21c3db06",
  owner_id: "owner",
  instance_id: "container-1",
  region: "local",
  instance_type: "local-small",
  image_id: "approved",
  subnet_id: "",
  name: "worker",
  status: "running",
  tags: {},
  metadata: {},
  created_at: "2026-10-04T00:00:00Z",
  updated_at: "2026-10-04T00:00:00Z",
  provider: "docker",
  ready: true,
  operation_id: "",
  error_code: "",
});
const workspace: Workspace = {
  id: "ws-test",
  slug: "test",
  name: "Test",
  description: null,
  context: null,
  settings: {},
  repos: [],
  issue_prefix: "TST",
  avatar_url: null,
  created_at: "2026-10-04T00:00:00Z",
  updated_at: "2026-10-04T00:00:00Z",
};
function mount(page = false, desktop = false, hosted = false) {
  const qc = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: Infinity },
      mutations: { retry: false },
    },
  });
  qc.setQueryData(workspaceKeys.list(), [workspace]);
  const onClose = vi.fn();
  const storage = {
    getItem: () => null,
    setItem: () => {},
    removeItem: () => {},
  };
  const authStore = createAuthStore({ api, storage });
  render(
    <I18nProvider
      locale="en"
      resources={{ en: { common: enCommon, runtimes: enRuntimes } }}
    >
      <QueryClientProvider client={qc}>
        <WSProvider
          wsUrl="ws://invalid.test"
          storage={storage}
          authStore={authStore}
        >
          <WorkspaceSlugProvider slug="test">
            <NavigationProvider
              value={{
                push: vi.fn(),
                replace: vi.fn(),
                back: vi.fn(),
                pathname: "/test/runtimes",
                searchParams: new URLSearchParams(),
                hash: "",
                getShareableUrl: (p) => p,
              }}
            >
              {page ? (
                <RuntimesPage
                  cloudRuntimeEnabled={hosted}
                  hasLocalMachine={desktop}
                />
              ) : (
                <CloudRuntimeDialog onClose={onClose} />
              )}
            </NavigationProvider>
          </WorkspaceSlugProvider>
        </WSProvider>
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { qc, onClose };
}
beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.mocked(api.listWorkspaces).mockResolvedValue([workspace]);
  vi.mocked(api.getCloudRuntimeCapabilities)
    .mockReset()
    .mockResolvedValue(caps);
  vi.mocked(api.listCloudRuntimeNodes).mockReset().mockResolvedValue([node]);
  vi.mocked(api.startCloudRuntimeNode).mockReset();
  vi.mocked(api.deleteCloudRuntimeNode).mockReset();
  vi.mocked(api.listRuntimes).mockResolvedValue([]);
  vi.mocked(api.listRuntimeProfiles).mockResolvedValue([]);
  vi.mocked(api.listAgents).mockResolvedValue([]);
  vi.mocked(api.listChatSessions).mockResolvedValue([]);
  vi.mocked(api.getAgentTaskSnapshot).mockResolvedValue([]);
  vi.mocked(api.createCloudRuntimeNode).mockReset().mockResolvedValue(node);
});
describe("cloud runtime manager", () => {
  it.each([
    { reason: "unknown", degraded: EMPTY_CLOUD_RUNTIME_CAPABILITIES },
    {
      reason: "removed create permission",
      degraded: {
        ...caps,
        operations: caps.operations.filter((action) => action !== "create"),
      },
    },
    { reason: "removed specs", degraded: { ...caps, specs: [] } },
  ])(
    "retains an open Docker manager and pending draft with $reason capabilities",
    async ({ degraded }) => {
      let reject!: (error: Error) => void;
      vi.mocked(api.createCloudRuntimeNode).mockImplementationOnce(
        () =>
          new Promise((_resolve, r) => {
            reject = r;
          }),
      );
      const { qc } = mount(true);
      const user = userEvent.setup();
      await user.click(
        await screen.findByRole("button", { name: "Cloud Runtime" }),
      );
      await user.type(await screen.findByLabelText("Name"), "retained-draft");
      await user.click(screen.getByRole("button", { name: "Create node" }));
      const key = vi.mocked(api.createCloudRuntimeNode).mock.calls[0]?.[1];
      vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(degraded);
      act(() => {
        qc.setQueryData(cloudRuntimeKeys.capabilities(), degraded);
      });
      await waitFor(() =>
        expect(
          screen.queryByRole("button", { name: "Cloud Runtime" }),
        ).not.toBeInTheDocument(),
      );
      expect(screen.getByRole("dialog")).toBeVisible();
      expect(screen.getByLabelText("Name")).toHaveValue("retained-draft");
      expect(
        screen.getByRole("button", { name: "Create node" }),
      ).toHaveAttribute("aria-busy", "true");
      expect(screen.getByRole("button", { name: "Refresh" })).toBeDisabled();
      await user.keyboard("{Escape}");
      expect(screen.getByRole("dialog")).toBeVisible();
      act(() => reject(new Error("profile_missing")));
      expect(await screen.findByRole("alert")).toHaveTextContent(
        /configure your Claude credential profile/,
      );
      expect(screen.getByLabelText("Name")).toHaveValue("retained-draft");
      expect(
        screen.getByRole("button", { name: "Create node" }),
      ).toBeDisabled();
      vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(caps);
      await user.click(screen.getByRole("button", { name: "Refresh" }));
      await waitFor(() =>
        expect(
          screen.getByRole("button", { name: "Create node" }),
        ).not.toBeDisabled(),
      );
      await user.click(screen.getByRole("button", { name: "Create node" }));
      await waitFor(() =>
        expect(api.createCloudRuntimeNode).toHaveBeenCalledTimes(2),
      );
      expect(api.createCloudRuntimeNode).toHaveBeenLastCalledWith(
        { name: "retained-draft", spec: "local-small" },
        key,
      );
    },
  );
  it("fails closed on a capability refetch error with retained old data and recovers through Refresh", async () => {
    const { qc } = mount(true);
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("button", { name: "Cloud Runtime" }),
    );
    await user.type(await screen.findByLabelText("Name"), "error-draft");
    vi.mocked(api.getCloudRuntimeCapabilities).mockRejectedValue(
      new Error("unavailable"),
    );
    await act(async () => {
      await qc.refetchQueries({ queryKey: cloudRuntimeKeys.capabilities() });
    });
    expect(qc.getQueryData(cloudRuntimeKeys.capabilities())).toEqual(caps);
    expect(
      screen.queryByRole("button", { name: "Cloud Runtime" }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeVisible();
    expect(screen.getByLabelText("Name")).toHaveValue("error-draft");
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Create node" }),
      ).toBeDisabled(),
    );
    expect(
      screen.queryByRole("button", { name: "Start node" }),
    ).not.toBeInTheDocument();
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(caps);
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Create node" }),
      ).not.toBeDisabled(),
    );
    expect(screen.getByLabelText("Name")).toHaveValue("error-draft");
  });
  it("retains a pending and then rejected delete confirmation through permission degradation and retries the same key", async () => {
    let reject!: (error: Error) => void;
    vi.mocked(api.deleteCloudRuntimeNode).mockImplementationOnce(
      () =>
        new Promise((_resolve, r) => {
          reject = r;
        }),
    );
    const { qc } = mount();
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("button", { name: "Delete node" }),
    );
    const confirmation = screen.getByRole("alertdialog");
    const confirm = within(confirmation).getByRole("button", {
      name: "Delete node",
    });
    await user.click(confirm);
    const key = vi.mocked(api.deleteCloudRuntimeNode).mock.calls[0]?.[1];
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(
      EMPTY_CLOUD_RUNTIME_CAPABILITIES,
    );
    act(() => {
      qc.setQueryData(
        cloudRuntimeKeys.capabilities(),
        EMPTY_CLOUD_RUNTIME_CAPABILITIES,
      );
    });
    expect(screen.getByRole("alertdialog")).toBe(confirmation);
    expect(confirm).toBeDisabled();
    expect(confirm).toHaveAttribute("aria-busy", "true");
    await user.keyboard("{Escape}");
    expect(screen.getByRole("alertdialog")).toBe(confirmation);
    act(() => reject(new Error("busy")));
    expect(await within(confirmation).findByRole("alert")).toHaveTextContent(
      /active runs and queued work/,
    );
    expect(confirm).toBeDisabled();
    expect(
      qc.getQueryData([...cloudRuntimeKeys.nodes(), { limit: 20, offset: 0 }]),
    ).toEqual([node]);
    const noDelete = {
      ...caps,
      operations: caps.operations.filter((action) => action !== "delete"),
    };
    act(() => {
      qc.setQueryData(cloudRuntimeKeys.capabilities(), noDelete);
    });
    expect(screen.getByRole("alertdialog")).toBe(confirmation);
    expect(confirm).toBeDisabled();
    await user.click(confirm);
    expect(api.deleteCloudRuntimeNode).toHaveBeenCalledOnce();
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(caps);
    vi.mocked(api.deleteCloudRuntimeNode).mockResolvedValueOnce(undefined);
    act(() => {
      qc.setQueryData(cloudRuntimeKeys.capabilities(), caps);
    });
    await waitFor(() => expect(confirm).not.toBeDisabled());
    await user.click(confirm);
    await waitFor(() =>
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument(),
    );
    expect(api.deleteCloudRuntimeNode).toHaveBeenLastCalledWith(node.id, key);
  });
  it("Refresh retries nodes and unknown capabilities together and prevents duplicate refresh while either is pending", async () => {
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValueOnce(
      EMPTY_CLOUD_RUNTIME_CAPABILITIES,
    );
    mount(true, false, true);
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("button", { name: "Cloud Runtime" }),
    );
    await screen.findByText("worker");
    expect(screen.getByRole("button", { name: "Create node" })).toBeDisabled();
    let resolve!: (value: CloudRuntimeCapabilities) => void;
    vi.mocked(api.getCloudRuntimeCapabilities).mockImplementationOnce(
      () =>
        new Promise((r) => {
          resolve = r;
        }),
    );
    const refresh = screen.getByRole("button", { name: "Refresh" });
    await user.click(refresh);
    await waitFor(() =>
      expect(api.listCloudRuntimeNodes).toHaveBeenCalledTimes(2),
    );
    expect(api.getCloudRuntimeCapabilities).toHaveBeenCalledTimes(2);
    expect(refresh).toBeDisabled();
    expect(refresh).toHaveAttribute("aria-busy", "true");
    await user.click(refresh);
    expect(api.getCloudRuntimeCapabilities).toHaveBeenCalledTimes(2);
    act(() => resolve(caps));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Create node" }),
      ).not.toBeDisabled(),
    );
    expect(screen.getByLabelText("Specification")).toHaveTextContent(
      "local-small",
    );
    expect(refresh).not.toBeDisabled();
  });
  it("offers only advertised Docker specifications and no hosted disk control", async () => {
    mount();
    expect(await screen.findByLabelText("Specification")).toHaveTextContent(
      "local-small",
    );
    expect(screen.queryByLabelText("Disk size GiB")).not.toBeInTheDocument();
  });
  it("preserves the hosted name/type/disk form and awaited request defaults", async () => {
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(
      capabilitiesFixture({
        provider: "cloud",
        operations: ["create", "delete"],
      }),
    );
    mount();
    const user = userEvent.setup();
    await screen.findByLabelText("Disk size GiB");
    expect(screen.getByLabelText("Instance type")).toHaveTextContent(
      "t4g.medium",
    );
    expect(screen.getByLabelText("Disk size GiB")).toHaveValue(20);
    await user.type(screen.getByLabelText("Name"), "hosted-worker");
    await user.click(screen.getByRole("button", { name: "Create node" }));
    await waitFor(() =>
      expect(api.createCloudRuntimeNode).toHaveBeenCalledWith(
        {
          name: "hosted-worker",
          instance_type: "t4g.medium",
          disk_size_gb: 20,
        },
        expect.any(String),
      ),
    );
    expect(screen.getByRole("dialog")).toBeVisible();
  });
  it("shows absent desired and observed data as unknown, not invented healthy diagnostics", async () => {
    mount();
    await screen.findByText("worker");
    expect(
      screen.getByText("Desired state").nextElementSibling,
    ).toHaveTextContent("Unknown");
    expect(
      screen.getByText("Observed state").nextElementSibling,
    ).toHaveTextContent("Unknown");
    expect(
      screen.getByText("Approved resource limits").nextElementSibling,
    ).toHaveTextContent("2 CPU · 4 GiB · 256 processes");
    expect(screen.getByText("Disk quota").nextElementSibling).toHaveTextContent(
      "Not supported",
    );
    expect(
      screen.queryByText(/authenticated|provider healthy/i),
    ).not.toBeInTheDocument();
  });
  it("does not grant a manager entry for unknown capabilities when the hosted flag is off", async () => {
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(
      capabilitiesFixture({ provider: "unknown", operations: [] }),
    );
    mount(true);
    await waitFor(() =>
      expect(api.getCloudRuntimeCapabilities).toHaveBeenCalled(),
    );
    expect(
      screen.queryByRole("button", { name: "Cloud Runtime" }),
    ).not.toBeInTheDocument();
  });
  it("retains the hosted entry but does not grant create when capabilities are unknown", async () => {
    vi.mocked(api.getCloudRuntimeCapabilities).mockResolvedValue(
      capabilitiesFixture({ provider: "unknown", operations: [] }),
    );
    mount(true, false, true);
    await userEvent
      .setup()
      .click(await screen.findByRole("button", { name: "Cloud Runtime" }));
    expect(screen.getByRole("button", { name: "Create node" })).toBeDisabled();
  });
  it("changes the create intent only when the failed draft changes", async () => {
    vi.mocked(api.createCloudRuntimeNode).mockRejectedValue(
      new Error("profile_missing"),
    );
    mount();
    const user = userEvent.setup();
    await screen.findByText("worker");
    await user.type(screen.getByLabelText("Name"), "draft-a");
    await user.click(screen.getByRole("button", { name: "Create node" }));
    await screen.findByRole("alert");
    const intent = vi.mocked(api.createCloudRuntimeNode).mock.calls[0]?.[1];
    await user.clear(screen.getByLabelText("Name"));
    await user.type(screen.getByLabelText("Name"), "draft-b");
    await user.click(screen.getByRole("button", { name: "Create node" }));
    await waitFor(() =>
      expect(api.createCloudRuntimeNode).toHaveBeenCalledTimes(2),
    );
    expect(vi.mocked(api.createCloudRuntimeNode).mock.calls[1]?.[1]).not.toBe(
      intent,
    );
  });
  it("shows initial list failure without fabricating nodes", async () => {
    vi.mocked(api.listCloudRuntimeNodes).mockRejectedValue(
      new Error("unavailable"),
    );
    mount();
    expect(
      await screen.findByText("Couldn't load cloud nodes. Try again."),
    ).toBeVisible();
    expect(screen.queryByText("worker")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Delete node" }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Refresh" })).not.toBeDisabled();
  });
  it("accepts a successful empty refresh without retaining phantom nodes", async () => {
    const { qc } = mount();
    const user = userEvent.setup();
    await screen.findByText("worker");
    vi.mocked(api.listCloudRuntimeNodes).mockResolvedValueOnce([]);
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() =>
      expect(screen.queryByText("worker")).not.toBeInTheDocument(),
    );
    expect(
      qc.getQueryData(["cloud-runtime", "nodes", { limit: 20, offset: 0 }]),
    ).toEqual([]);
  });
  it("requires confirmation before deletion", async () => {
    mount();
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole("button", { name: "Delete node" }),
    );
    const dialog = screen.getByRole("alertdialog");
    expect(
      within(dialog).getByText(/volumes, sessions, and working directories/i),
    ).toBeVisible();
    expect(api.deleteCloudRuntimeNode).not.toHaveBeenCalled();
  });
  it("awaits creation, retains failed input and intent, then resets without closing the manager", async () => {
    vi.mocked(api.createCloudRuntimeNode).mockRejectedValueOnce(
      new Error("profile_missing"),
    );
    const { onClose } = mount();
    const user = userEvent.setup();
    await screen.findByText("worker");
    await user.type(screen.getByLabelText("Name"), "retry-worker");
    await user.click(screen.getByRole("button", { name: "Create node" }));
    await screen.findByRole("alert");
    expect(screen.getByLabelText("Name")).toHaveValue("retry-worker");
    const key = vi.mocked(api.createCloudRuntimeNode).mock.calls[0]?.[1];
    expect(key).toEqual(expect.any(String));
    let resolve!: (value: typeof node) => void;
    vi.mocked(api.createCloudRuntimeNode).mockImplementationOnce(
      () =>
        new Promise((r) => {
          resolve = r;
        }),
    );
    await user.click(screen.getByRole("button", { name: "Create node" }));
    await waitFor(() =>
      expect(api.createCloudRuntimeNode).toHaveBeenCalledTimes(2),
    );
    expect(screen.getByRole("button", { name: "Create node" })).toBeDisabled();
    expect(screen.getByLabelText("Name")).toHaveValue("retry-worker");
    expect(onClose).not.toHaveBeenCalled();
    resolve(node);
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveValue(""));
    expect(api.createCloudRuntimeNode).toHaveBeenLastCalledWith(
      { name: "retry-worker", spec: "local-small" },
      key,
    );
    expect(screen.getByRole("dialog")).toBeVisible();
  });
  it("retains the visible node and failed action intent when the follow-up list refresh also fails", async () => {
    const readyNode = nodeFixture({ ...node, ready: true });
    vi.mocked(api.listCloudRuntimeNodes).mockResolvedValueOnce([readyNode]);
    const { qc } = mount();
    const user = userEvent.setup();
    await screen.findByText("worker");
    vi.mocked(api.listCloudRuntimeNodes).mockRejectedValue(
      new Error("unavailable"),
    );
    vi.mocked(api.startCloudRuntimeNode)
      .mockRejectedValueOnce(new Error("profile_missing"))
      .mockResolvedValueOnce(node);
    await user.click(screen.getByRole("button", { name: "Start node" }));
    await waitFor(() =>
      expect(api.listCloudRuntimeNodes).toHaveBeenCalledTimes(2),
    );
    expect(screen.getByText("worker")).toBeVisible();
    expect(
      screen.getByText("Couldn't load cloud nodes. Try again."),
    ).toBeVisible();
    expect(
      qc.getQueryData(["cloud-runtime", "nodes", { limit: 20, offset: 0 }]),
    ).toEqual([readyNode]);
    const readiness = screen.getByText("Readiness").nextElementSibling;
    expect(readiness).not.toHaveTextContent("Ready");
    expect(readiness).toHaveTextContent("Unknown");
    const intent = vi.mocked(api.startCloudRuntimeNode).mock.calls[0]?.[1];
    await user.click(screen.getByRole("button", { name: "Start node" }));
    await waitFor(() =>
      expect(api.startCloudRuntimeNode).toHaveBeenCalledTimes(2),
    );
    expect(api.startCloudRuntimeNode).toHaveBeenLastCalledWith(node.id, intent);
  });
  it("keeps node management open while a lifecycle action is pending", async () => {
    let resolve!: (value: typeof node) => void;
    vi.mocked(api.startCloudRuntimeNode).mockImplementationOnce(
      () =>
        new Promise((r) => {
          resolve = r;
        }),
    );
    const { onClose } = mount();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Start node" }));
    await user.keyboard("{Escape}");
    expect(onClose).not.toHaveBeenCalled();
    const dialog = screen.getByRole("dialog");
    expect(
      within(dialog).getByRole("button", { name: /^Cancel$/ }),
    ).toBeDisabled();
    expect(
      within(dialog).getByRole("button", { name: "Refresh" }),
    ).toBeDisabled();
    resolve(node);
    await waitFor(() =>
      expect(
        within(dialog).getByRole("button", { name: /^Cancel$/ }),
      ).not.toBeDisabled(),
    );
  });
  it.each([false, true])(
    "enables the manager entry with Docker capabilities and hosted flag off (desktop=%s)",
    async (desktop) => {
      mount(true, desktop);
      const entry = await screen.findByRole("button", {
        name: "Cloud Runtime",
      });
      await userEvent.setup().click(entry);
      expect(await screen.findByRole("dialog")).toBeVisible();
      await userEvent.setup().keyboard("{Escape}");
      await waitFor(() =>
        expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
      );
      await waitFor(() => expect(entry).toHaveFocus());
    },
  );
});
