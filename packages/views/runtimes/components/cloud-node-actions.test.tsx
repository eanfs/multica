// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import { parseWithFallback } from "@multica/core/api/schema";
import {
  EMPTY_CLOUD_RUNTIME_CAPABILITIES,
  type CloudRuntimeCapabilities,
  type CloudRuntimeNode,
} from "@multica/core/runtimes";
import { api } from "@multica/core/api";
import {
  CloudRuntimeCapabilitiesSchema,
  CloudRuntimeNodeSchema,
} from "@multica/core/api/schemas";
import { cloudRuntimeKeys } from "@multica/core/runtimes";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { CloudNodeActions } from "./cloud-node-actions";

vi.mock("@multica/core/api", async () => ({
  ...(await vi.importActual<typeof import("@multica/core/api")>(
    "@multica/core/api",
  )),
  api: {
    startCloudRuntimeNode: vi.fn(),
    stopCloudRuntimeNode: vi.fn(),
    rebootCloudRuntimeNode: vi.fn(),
    deleteCloudRuntimeNode: vi.fn(),
  },
}));
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
function mount(currentNode = node, capabilities = caps) {
  const qc = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: Infinity },
      mutations: { retry: false },
    },
  });
  const key = [...cloudRuntimeKeys.nodes(), { limit: 20, offset: 0 }];
  qc.setQueryData(key, [currentNode]);
  const onDeleted = vi.fn();
  render(
    <I18nProvider
      locale="en"
      resources={{ en: { common: enCommon, runtimes: enRuntimes } }}
    >
      <QueryClientProvider client={qc}>
        <CloudNodeActions
          node={currentNode}
          capabilities={capabilities}
          wsId="ws-test"
          onDeleted={onDeleted}
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { qc, key, onDeleted };
}
beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
});
describe("cloud node actions", () => {
  it.each(["Start node", "Stop node", "Restart node"])(
    "%s awaits the result and preserves failed intent for a pending-disabled retry",
    async (label) => {
      const mutation = vi.mocked(
        label === "Start node"
          ? api.startCloudRuntimeNode
          : label === "Stop node"
            ? api.stopCloudRuntimeNode
            : api.rebootCloudRuntimeNode,
      );
      mutation.mockRejectedValueOnce(new Error("profile_missing"));
      const { qc, key, onDeleted } = mount();
      const user = userEvent.setup();
      await user.click(screen.getByRole("button", { name: label }));
      expect(await screen.findByRole("alert")).toHaveTextContent(
        /configure your Claude credential profile/,
      );
      expect(qc.getQueryData(key)).toEqual([node]);
      expect(onDeleted).not.toHaveBeenCalled();
      const intent = mutation.mock.calls[0]?.[1];
      expect(intent).toEqual(expect.any(String));
      let resolve!: (value: typeof node) => void;
      mutation.mockImplementationOnce(
        () =>
          new Promise((r) => {
            resolve = r;
          }),
      );
      await user.click(screen.getByRole("button", { name: label }));
      const pendingButton = screen.getByRole("button", { name: label });
      expect(pendingButton).toBeDisabled();
      expect(pendingButton).toHaveAttribute("aria-busy", "true");
      await user.click(pendingButton);
      expect(mutation).toHaveBeenCalledTimes(2);
      expect(mutation).toHaveBeenLastCalledWith(node.instance_id, intent);
      expect(onDeleted).not.toHaveBeenCalled();
      resolve(node);
      await waitFor(() => expect(pendingButton).not.toBeDisabled());
      mutation.mockResolvedValueOnce(node);
      await user.click(pendingButton);
      await waitFor(() => expect(mutation).toHaveBeenCalledTimes(3));
      expect(mutation.mock.calls[2]?.[1]).not.toBe(intent);
    },
  );
  it("delete retains node, cache and confirmation on busy failure; retries the same intent and only cleans up after success", async () => {
    vi.mocked(api.deleteCloudRuntimeNode).mockRejectedValueOnce(
      new Error("busy"),
    );
    const { qc, key, onDeleted } = mount();
    const user = userEvent.setup();
    const opener = screen.getByRole("button", { name: "Delete node" });
    await user.click(opener);
    let dialog = screen.getByRole("alertdialog");
    expect(
      within(dialog).getByText(/volumes, sessions, and working directories/),
    ).toBeVisible();
    expect(api.deleteCloudRuntimeNode).not.toHaveBeenCalled();
    await user.click(
      within(dialog).getByRole("button", { name: "Delete node" }),
    );
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      /active runs and queued work/,
    );
    expect(qc.getQueryData(key)).toEqual([node]);
    expect(onDeleted).not.toHaveBeenCalled();
    const intent = vi.mocked(api.deleteCloudRuntimeNode).mock.calls[0]?.[1];
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await user.click(opener);
    dialog = screen.getByRole("alertdialog");
    let resolve!: () => void;
    vi.mocked(api.deleteCloudRuntimeNode).mockImplementationOnce(
      () =>
        new Promise((r) => {
          resolve = r;
        }),
    );
    const confirm = within(dialog).getByRole("button", { name: "Delete node" });
    await user.click(confirm);
    await user.click(confirm);
    expect(confirm).toBeDisabled();
    expect(confirm).toHaveAttribute("aria-busy", "true");
    expect(onDeleted).not.toHaveBeenCalled();
    expect(api.deleteCloudRuntimeNode).toHaveBeenCalledTimes(2);
    expect(api.deleteCloudRuntimeNode).toHaveBeenLastCalledWith(
      node.instance_id,
      intent,
    );
    expect(qc.getQueryData(key)).toEqual([node]);
    resolve();
    await waitFor(() => expect(onDeleted).toHaveBeenCalledOnce());
    await waitFor(() =>
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument(),
    );
    await waitFor(() => expect(opener).toHaveFocus());
  });
  it("restores focus after Escape without deleting", async () => {
    mount();
    const user = userEvent.setup();
    const opener = screen.getByRole("button", { name: "Delete node" });
    await user.click(opener);
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument(),
    );
    await waitFor(() => expect(opener).toHaveFocus());
    expect(api.deleteCloudRuntimeNode).not.toHaveBeenCalled();
  });
  it("does not disable a compatible hosted node for absent optional readiness diagnostics", () => {
    mount(
      nodeFixture({ ...node, provider: "cloud", ready: undefined }),
      capabilitiesFixture({ provider: "cloud", operations: ["start"] }),
    );
    expect(
      screen.getByRole("button", { name: "Start node" }),
    ).not.toBeDisabled();
  });
  it("keeps unknown capabilities read only with a visible recovery path", () => {
    mount(node, capabilitiesFixture({ provider: "unknown", operations: [] }));
    expect(
      screen.queryByRole("button", { name: "Delete node" }),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/Refresh capabilities/)).toBeVisible();
  });
  it("disables unknown node state without pretending the node is healthy", () => {
    mount(nodeFixture({ ...node, status: "future-state" }));
    expect(screen.getByRole("button", { name: "Stop node" })).toBeDisabled();
    expect(screen.getByText(/state is unconfirmed/)).toBeVisible();
  });
});
