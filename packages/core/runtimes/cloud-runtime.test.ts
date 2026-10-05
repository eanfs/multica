// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MutationObserver, QueryClient, useMutation } from "@tanstack/react-query";
import { ApiClient } from "../api/client";
import { setApiInstance } from "../api";
import {
  cloudRuntimeNodeListOptions,
  cloudRuntimeCapabilityOptions,
  useCreateCloudRuntimeNode,
  useDeleteCloudRuntimeNode,
  useStartCloudRuntimeNode,
  useStopCloudRuntimeNode,
  useRebootCloudRuntimeNode,
} from "./cloud-runtime";

let qc: QueryClient;
vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>();
  return { ...actual, useQueryClient: () => qc, useMutation: vi.fn((options) => options) };
});
beforeEach(() => {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false, gcTime: 0 } },
  });
  vi.stubGlobal("fetch", () => {
    throw new Error("Unstubbed network request in runtime test");
  });
  setApiInstance(new ApiClient("https://api.example.test"));
});
afterEach(() => {
  qc.clear();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});
const node = {
  id: "node",
  owner_id: "owner",
  instance_id: "container",
  region: "local",
  instance_type: "small",
  image_id: "image",
  subnet_id: "",
  name: "worker",
  status: "stopped",
  tags: {},
  metadata: {},
  created_at: "",
  updated_at: "",
};

describe("account cloud runtime queries and mutation lifecycle", () => {
  it("shares account node and capability queries between workspace projections", () => {
    expect(cloudRuntimeNodeListOptions("ws-a").queryKey).toEqual([
      "cloud-runtime",
      "nodes",
      { limit: 20, offset: 0 },
    ]);
    expect(cloudRuntimeNodeListOptions("ws-b").queryKey).toEqual([
      "cloud-runtime",
      "nodes",
      { limit: 20, offset: 0 },
    ]);
    expect(cloudRuntimeCapabilityOptions("ws-a").queryKey).toEqual([
      "cloud-runtime",
      "capabilities",
    ]);
    expect(cloudRuntimeCapabilityOptions("ws-b").queryKey).toEqual([
      "cloud-runtime",
      "capabilities",
    ]);
  });
  it("reuses the same fetched account capabilities across workspaces", async () => {
    const fetchMock = vi.fn(
      async (_url: string, _init: RequestInit) =>
        new Response(
          JSON.stringify({
            provider: "docker",
            operations: ["create"],
            specs: [],
            persistent_storage: true,
            disk_quota_supported: false,
          }),
          { status: 200 },
        ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const first = await qc.fetchQuery(cloudRuntimeCapabilityOptions("ws-a"));
    const second = await qc.fetchQuery(cloudRuntimeCapabilityOptions("ws-b"));
    expect(second).toEqual(first);
    expect(first.provider).toBe("docker");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("https://api.example.test/api/cloud-runtime/");
  });
  it.each([
    {
      action: "create",
      hook: useCreateCloudRuntimeNode,
      variables: { spec: "small", name: "worker", idempotencyKey: "intent" },
      body: { spec: "small", name: "worker" },
    },
    {
      action: "start",
      hook: useStartCloudRuntimeNode,
      variables: { instanceId: "container", idempotencyKey: "intent" },
      body: { instance_id: "container" },
    },
    {
      action: "stop",
      hook: useStopCloudRuntimeNode,
      variables: { instanceId: "container", idempotencyKey: "intent" },
      body: { instance_id: "container" },
    },
    {
      action: "reboot",
      hook: useRebootCloudRuntimeNode,
      variables: { instanceId: "container", idempotencyKey: "intent" },
      body: { instance_id: "container" },
    },
    {
      action: "delete",
      hook: useDeleteCloudRuntimeNode,
      variables: { instanceId: "container", idempotencyKey: "intent" },
      body: { instance_id: "container" },
    },
  ])(
    "$action awaits success, reuses caller intent on Query retry, and invalidates account plus only this workspace runtime",
    async ({ action, hook, variables, body }) => {
      const calls: Array<{ key: string | null; body: unknown }> = [];
      let finish!: (response: Response) => void;
      vi.stubGlobal("fetch", async (_url: string, init: RequestInit) => {
        calls.push({
          key: new Headers(init.headers).get("Idempotency-Key"),
          body: JSON.parse(String(init.body)),
        });
        if (calls.length === 1)
          return new Response(JSON.stringify({ error: "unavailable" }), { status: 503 });
        return new Promise<Response>((resolve) => {
          finish = resolve;
        });
      });
      const nodeKey = ["cloud-runtime", "nodes", { limit: 20, offset: 0 }];
      qc.setQueryData(nodeKey, [node]);
      qc.setQueryData(["cloud-runtime", "capabilities"], { provider: "docker" });
      qc.setQueryData(["runtimes", "ws-a", "list"], [{ id: "runtime" }]);
      qc.setQueryData(["runtimes", "ws-b", "list"], [{ id: "other" }]);
      hook("ws-a");
      const options = vi.mocked(useMutation).mock.calls.at(-1)![0];
      expect(options.onMutate).toBeUndefined();
      const observer = new MutationObserver(qc, { ...options, retry: 1, retryDelay: 0 });
      const settled = observer.mutate(variables);
      await vi.waitFor(() => expect(calls).toHaveLength(2));
      expect(observer.getCurrentResult().status).toBe("pending");
      expect(qc.getQueryData(nodeKey)).toEqual([node]);
      expect(qc.getQueryState(nodeKey)?.isInvalidated).toBe(false);
      finish(
        action === "delete"
          ? new Response(null, { status: 204 })
          : new Response(JSON.stringify(node), { status: 200 }),
      );
      await settled;
      expect(observer.getCurrentResult().status).toBe("success");
      expect(calls).toEqual([
        { key: "intent", body },
        { key: "intent", body },
      ]);
      expect(qc.getQueryData(nodeKey)).toEqual([node]);
      expect(qc.getQueryState(nodeKey)?.isInvalidated).toBe(true);
      expect(qc.getQueryState(["cloud-runtime", "capabilities"])?.isInvalidated).toBe(true);
      expect(qc.getQueryState(["runtimes", "ws-a", "list"])?.isInvalidated).toBe(true);
      expect(qc.getQueryState(["runtimes", "ws-b", "list"])?.isInvalidated).toBe(false);
    },
  );
  it("malformed create is an error, retains cached nodes, and invalidates projections", async () => {
    vi.stubGlobal("fetch", async () => new Response(JSON.stringify({ id: 123 }), { status: 200 }));
    const nodeKey = ["cloud-runtime", "nodes", { limit: 20, offset: 0 }];
    qc.setQueryData(nodeKey, [node]);
    qc.setQueryData(["runtimes", "ws-a", "list"], []);
    useCreateCloudRuntimeNode("ws-a");
    const observer = new MutationObserver(qc, vi.mocked(useMutation).mock.calls.at(-1)![0]);
    await expect(observer.mutate({ spec: "small", idempotencyKey: "intent" })).rejects.toThrow(
      /invalid cloud runtime node/i,
    );
    expect(observer.getCurrentResult().status).toBe("error");
    expect(qc.getQueryData(nodeKey)).toEqual([node]);
    expect(qc.getQueryState(nodeKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(["runtimes", "ws-a", "list"])?.isInvalidated).toBe(true);
  });
  it("failed delete retains cached nodes and invalidates uncertain projections on settle", async () => {
    vi.stubGlobal(
      "fetch",
      async () => new Response(JSON.stringify({ error: "busy" }), { status: 409 }),
    );
    const nodeKey = ["cloud-runtime", "nodes", { limit: 20, offset: 0 }];
    qc.setQueryData(nodeKey, [node]);
    qc.setQueryData(["runtimes", "ws-a", "list"], []);
    useDeleteCloudRuntimeNode("ws-a");
    const observer = new MutationObserver(qc, vi.mocked(useMutation).mock.calls.at(-1)![0]);
    await expect(
      observer.mutate({ instanceId: "container", idempotencyKey: "intent" }),
    ).rejects.toThrow("busy");
    expect(observer.getCurrentResult().status).toBe("error");
    expect(qc.getQueryData(nodeKey)).toEqual([node]);
    expect(qc.getQueryState(nodeKey)?.isInvalidated).toBe(true);
    expect(qc.getQueryState(["runtimes", "ws-a", "list"])?.isInvalidated).toBe(true);
  });
});
