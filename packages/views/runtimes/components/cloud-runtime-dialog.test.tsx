import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { setApiInstance } from "@multica/core/api";
import { I18nProvider } from "@multica/core/i18n/react";
import type {
  CloudRuntimeNode,
  CloudRuntimeService,
} from "@multica/core/runtimes";
import enCommon from "../../locales/en/common.json";
import enLayout from "../../locales/en/layout.json";
import enRuntimes from "../../locales/en/runtimes.json";
import { CloudRuntimeDialog } from "./cloud-runtime-dialog";

const TEST_RESOURCES = {
  en: { common: enCommon, layout: enLayout, runtimes: enRuntimes },
};

const apiMocks = vi.hoisted(() => ({
  listCloudRuntimeNodes: vi.fn(),
  createCloudRuntimeNode: vi.fn(),
  deleteCloudRuntimeNode: vi.fn(),
  startCloudRuntimeNode: vi.fn(),
  stopCloudRuntimeNode: vi.fn(),
  rebootCloudRuntimeNode: vi.fn(),
  getCloudRuntimeService: vi.fn(),
}));

function installApi() {
  setApiInstance(
    apiMocks as unknown as Parameters<typeof setApiInstance>[0],
  );
}

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-test" }));

function dockerService(
  overrides: Partial<CloudRuntimeService> = {},
): CloudRuntimeService {
  return {
    provider: "docker",
    operations: ["create"],
    specs: [{ id: "standard", cpu: 2, memoryMb: 4096, maxConcurrent: 1 }],
    persistentStorage: true,
    diskQuota: false,
    ...overrides,
  };
}

function makeNode(overrides: Partial<CloudRuntimeNode> = {}): CloudRuntimeNode {
  return {
    id: "node-1",
    owner_id: "user-1",
    instance_id: "container-1",
    region: "local",
    instance_type: "standard",
    image_id: "multica/runtime:latest",
    subnet_id: "",
    name: "docker-dev",
    status: "running",
    tags: {},
    metadata: {},
    created_at: "2026-11-09T05:00:00Z",
    updated_at: "2026-11-09T05:00:00Z",
    ...overrides,
  };
}

function renderDialog(service: CloudRuntimeService) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <CloudRuntimeDialog service={service} onClose={() => {}} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  installApi();
  apiMocks.listCloudRuntimeNodes.mockResolvedValue([]);
  apiMocks.createCloudRuntimeNode.mockResolvedValue(makeNode());
  apiMocks.deleteCloudRuntimeNode.mockResolvedValue(undefined);
  apiMocks.startCloudRuntimeNode.mockResolvedValue(undefined);
  apiMocks.stopCloudRuntimeNode.mockResolvedValue(undefined);
  apiMocks.rebootCloudRuntimeNode.mockResolvedValue(undefined);
});

describe("CloudRuntimeDialog", () => {
  it("renders the advertised specs and hides disk size for Docker", () => {
    renderDialog(dockerService());

    expect(screen.getAllByText("standard").length).toBeGreaterThan(0);
    expect(screen.getByText("docker")).toBeInTheDocument();
    expect(screen.getByText("Persistent volume")).toBeInTheDocument();
    expect(screen.queryByText(/disk size/i)).not.toBeInTheDocument();
  });

  it("keeps the disk size field for non-Docker providers", () => {
    renderDialog(
      dockerService({ provider: "ec2", specs: [], persistentStorage: false }),
    );

    expect(screen.getByText("Disk size GiB")).toBeInTheDocument();
    expect(screen.queryByText("Persistent volume")).not.toBeInTheDocument();
    // No specs means no node shape: never fall back to the old EC2 names.
    expect(screen.queryByText("t4g.medium")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create node" })).toBeDisabled();
  });

  it("renders only the lifecycle operations the provider advertises", async () => {
    apiMocks.listCloudRuntimeNodes.mockResolvedValue([makeNode()]);
    renderDialog(
      dockerService({ operations: ["create", "start", "reboot"] }),
    );

    expect(
      await screen.findByRole("button", { name: "Start node" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reboot node" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Stop node" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete node" })).not.toBeInTheDocument();
  });

  it("asks for confirmation before deleting a node permanently", async () => {
    const user = userEvent.setup();
    apiMocks.listCloudRuntimeNodes.mockResolvedValue([makeNode()]);
    renderDialog(dockerService({ operations: ["create", "delete"] }));

    await user.click(await screen.findByRole("button", { name: "Delete node" }));
    expect(
      await screen.findByText(/Its container, volumes, and node token are removed/),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() =>
      expect(
        screen.queryByText(/Its container, volumes, and node token are removed/),
      ).not.toBeInTheDocument(),
    );
    expect(apiMocks.deleteCloudRuntimeNode).not.toHaveBeenCalled();
  });

  it("surfaces a Fleet failure detail nested in node metadata", async () => {
    apiMocks.listCloudRuntimeNodes.mockResolvedValue([
      makeNode({
        status: "failed",
        metadata: { status_detail: { error: "container exited with code 1" } },
      }),
    ]);
    renderDialog(dockerService());

    expect(
      await screen.findByText("container exited with code 1"),
    ).toBeInTheDocument();
  });

  it("disables creation and explains why when the provider cannot create", () => {
    renderDialog(dockerService({ operations: ["start"], diskQuota: false }));

    expect(screen.getByRole("button", { name: "Create node" })).toBeDisabled();
    expect(
      screen.getByText("This operation is not available for this provider."),
    ).toBeInTheDocument();
  });
  it("sends only the Docker create fields, with a fresh idempotency key", async () => {
    const user = userEvent.setup();
    renderDialog(dockerService());

    await user.click(screen.getByRole("button", { name: "Create node" }));
    await waitFor(() =>
      expect(apiMocks.createCloudRuntimeNode).toHaveBeenCalledTimes(1),
    );

    const payload = apiMocks.createCloudRuntimeNode.mock.calls[0]![0];
    // Local Fleet decodes with DisallowUnknownFields: no EC2 field may ride
    // along, or the create 400s.
    expect(Object.keys(payload).sort()).toEqual(["idempotency_key", "spec_id"]);
    expect(payload).toMatchObject({ spec_id: "standard" });
    expect(typeof payload.idempotency_key).toBe("string");
    expect(payload.idempotency_key).not.toBe("");
  });

  it("adds the optional node name to the Docker payload", async () => {
    const user = userEvent.setup();
    renderDialog(dockerService());

    await user.type(screen.getByLabelText("Name"), "docker-dev");
    await user.click(screen.getByRole("button", { name: "Create node" }));
    await waitFor(() =>
      expect(apiMocks.createCloudRuntimeNode).toHaveBeenCalledWith({
        name: "docker-dev",
        spec_id: "standard",
        idempotency_key: expect.any(String),
      }),
    );

    const payload = apiMocks.createCloudRuntimeNode.mock.calls[0]![0];
    expect(Object.keys(payload).sort()).toEqual([
      "idempotency_key",
      "name",
      "spec_id",
    ]);
  });

  it("keeps the EC2 create shape and never sends spec_id", async () => {
    const user = userEvent.setup();
    renderDialog(
      dockerService({
        provider: "ec2",
        specs: [{ id: "g5.xlarge", cpu: 4, memoryMb: 8192, maxConcurrent: 1 }],
        persistentStorage: false,
      }),
    );

    await user.click(screen.getByRole("button", { name: "Create node" }));
    await waitFor(() => expect(apiMocks.createCloudRuntimeNode).toHaveBeenCalled());

    const payload = apiMocks.createCloudRuntimeNode.mock.calls[0]![0];
    expect(payload).toEqual({
      instance_type: "g5.xlarge",
      disk_size_gb: 20,
    });
    expect(payload).not.toHaveProperty("spec_id");
    expect(payload).not.toHaveProperty("idempotency_key");
  });

  it("disables creation when the provider advertises no specs", () => {
    renderDialog(dockerService({ specs: [] }));

    expect(screen.getByRole("button", { name: "Create node" })).toBeDisabled();
    expect(
      screen.getByText("This operation is not available for this provider."),
    ).toBeInTheDocument();
    expect(screen.queryByText("t4g.medium")).not.toBeInTheDocument();
  });
});
