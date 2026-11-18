/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { setApiInstance } from "../api";
import { ApiClient } from "../api/client";
import type { ApiClient as ApiClientType } from "../api/client";
import { CloudRuntimeNodeSchema, CloudRuntimeServiceSchema } from "../api/schemas";
import {
  EMPTY_CLOUD_RUNTIME_SERVICE,
  cloudRuntimeKeys,
  cloudRuntimeServiceKeys,
  cloudRuntimeServiceOptions,
  useDeleteCloudRuntimeNode,
  useRebootCloudRuntimeNode,
  useStartCloudRuntimeNode,
  useStopCloudRuntimeNode,
} from "./cloud-runtime";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const dockerServicePayload = {
  provider: "docker",
  operations: ["create", "start", "stop", "reboot", "delete"],
  specs: [{ id: "standard", cpu: 2, memory_mb: 4096, max_concurrent: 1 }],
  persistent_storage: true,
  disk_quota: false,
};

describe("CloudRuntimeServiceSchema", () => {
  it("parses a docker capability response into camelCase", () => {
    const parsed = CloudRuntimeServiceSchema.parse(dockerServicePayload);
    expect(parsed.provider).toBe("docker");
    expect(parsed.operations).toEqual([
      "create",
      "start",
      "stop",
      "reboot",
      "delete",
    ]);
    expect(parsed.specs[0]).toEqual({
      id: "standard",
      cpu: 2,
      memoryMb: 4096,
      maxConcurrent: 1,
    });
    expect(parsed.persistentStorage).toBe(true);
    expect(parsed.diskQuota).toBe(false);
  });

  it("falls back safely for unknown providers and missing fields", () => {
    const parsed = CloudRuntimeServiceSchema.parse({ provider: "unknown" });
    expect(parsed.provider).toBe("unknown");
    expect(parsed.operations).toEqual([]);
    expect(parsed.specs).toEqual([]);
    expect(parsed.persistentStorage).toBe(false);
    expect(parsed.diskQuota).toBe(false);
  });

  it("defaults missing spec fields without dropping the spec", () => {
    const parsed = CloudRuntimeServiceSchema.parse({
      provider: "docker",
      specs: [{ id: "custom" }],
    });
    expect(parsed.specs).toEqual([
      { id: "custom", cpu: 0, memoryMb: 0, maxConcurrent: 1 },
    ]);
  });

  it("keeps unknown capability fields loose", () => {
    const parsed = CloudRuntimeServiceSchema.parse({
      ...dockerServicePayload,
      future_flag: "ignored",
    });
    expect(parsed.provider).toBe("docker");
  });
});

describe("CloudRuntimeNodeSchema", () => {
  const nodePayload = {
    id: "node-1",
    owner_id: "user-1",
    instance_id: "container-1",
    region: "local",
    instance_type: "standard",
    image_id: "multica-runtime",
    subnet_id: "",
    name: "dev",
    status: "running",
    tags: {},
    metadata: {},
    created_at: "2026-11-09T00:00:00Z",
    updated_at: "2026-11-09T00:00:00Z",
    provider: "docker",
    daemon_id: "daemon-1",
    status_detail: { cli_installed: true, model_configured: true },
    operation: { name: "stop", phase: "prepared" },
    error: "",
  };

  it("renames the new optional fields and keeps existing snake_case fields", () => {
    const parsed = CloudRuntimeNodeSchema.parse(nodePayload);
    expect(parsed.provider).toBe("docker");
    expect(parsed.daemonId).toBe("daemon-1");
    expect(parsed.statusDetail).toEqual({
      cli_installed: true,
      model_configured: true,
    });
    expect(parsed.error).toBe("");
    expect(parsed.operation).toEqual({ name: "stop", phase: "prepared" });
    expect(parsed.instance_id).toBe("container-1");
    expect(parsed.owner_id).toBe("user-1");
    expect("daemon_id" in parsed).toBe(false);
    expect("status_detail" in parsed).toBe(false);
  });

  it("parses nodes from backends that predate the new optional fields", () => {
    const { provider, daemon_id, status_detail, operation, error, ...legacy } =
      nodePayload;
    const parsed = CloudRuntimeNodeSchema.parse(legacy);
    expect(parsed.daemonId).toBeUndefined();
    expect(parsed.statusDetail).toBeUndefined();
    expect(parsed.operation).toBeUndefined();
    expect(parsed.status).toBe("running");
  });
});

describe("ApiClient cloud runtime service", () => {
  it("reads the capability document from /api/cloud-runtime/", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse(dockerServicePayload));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    const service = await client.getCloudRuntimeService();

    expect(fetchMock.mock.calls[0]![0]).toBe(
      "https://api.example.test/api/cloud-runtime/",
    );
    expect(service.provider).toBe("docker");
    expect(service.specs[0]?.memoryMb).toBe(4096);
    expect(service.persistentStorage).toBe(true);
  });

  it("falls back to the empty service when the response drifts", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ provider: 42, specs: "invalid" }));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    await expect(client.getCloudRuntimeService()).resolves.toEqual(
      EMPTY_CLOUD_RUNTIME_SERVICE,
    );
  });
});

describe("ApiClient cloud runtime node operations", () => {
  it("sends an Idempotency-Key with start, stop and reboot", async () => {
    const fetchMock = vi
      .fn()
      .mockImplementation(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    await client.startCloudRuntimeNode("container-1", "key-start");
    await client.stopCloudRuntimeNode("container-1", "key-stop");
    await client.rebootCloudRuntimeNode("container-1", "key-reboot");

    const expected = [
      ["/api/cloud-runtime/nodes/start", "key-start"],
      ["/api/cloud-runtime/nodes/stop", "key-stop"],
      ["/api/cloud-runtime/nodes/reboot", "key-reboot"],
    ] as const;
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expected.forEach(([path, key], index) => {
      const [url, opts] = fetchMock.mock.calls[index]!;
      expect(url).toBe(`https://api.example.test${path}`);
      expect(opts).toMatchObject({
        method: "POST",
        body: JSON.stringify({
          instance_id: "container-1",
          idempotency_key: key,
        }),
      });
      const headers = opts.headers as Record<string, string>;
      expect(headers["Idempotency-Key"]).toBe(key);
      expect(headers["Content-Type"]).toBe("application/json");
    });
  });

  it("sends an Idempotency-Key with delete", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    await client.deleteCloudRuntimeNode(
      { instanceId: "container-1" },
      "key-delete",
    );

    const [url, opts] = fetchMock.mock.calls[0]!;
    expect(url).toBe("https://api.example.test/api/cloud-runtime/nodes");
    expect(opts).toMatchObject({
      method: "DELETE",
      body: JSON.stringify({
        instance_id: "container-1",
        idempotency_key: "key-delete",
      }),
    });
    expect((opts.headers as Record<string, string>)["Idempotency-Key"]).toBe(
      "key-delete",
    );
  });

  it("addresses a failed node by node id when it has no container", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    await client.deleteCloudRuntimeNode({ nodeId: "node-1" }, "key-delete");

    const [, opts] = fetchMock.mock.calls[0]!;
    expect(opts).toMatchObject({
      method: "DELETE",
      body: JSON.stringify({
        node_id: "node-1",
        idempotency_key: "key-delete",
      }),
    });
  });
});

function wrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: qc }, children);
  };
}

describe("cloud runtime node mutations", () => {
  it("generates a fresh Idempotency-Key per operation and invalidates the cache", async () => {
    const start = vi.fn().mockResolvedValue(undefined);
    const stop = vi.fn().mockResolvedValue(undefined);
    const reboot = vi.fn().mockResolvedValue(undefined);
    setApiInstance({
      startCloudRuntimeNode: start,
      stopCloudRuntimeNode: stop,
      rebootCloudRuntimeNode: reboot,
    } as unknown as ApiClientType);

    const qc = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const invalidate = vi.spyOn(qc, "invalidateQueries");

    const startHook = renderHook(() => useStartCloudRuntimeNode("ws-1"), {
      wrapper: wrapper(qc),
    });
    await act(async () => {
      await startHook.result.current.mutateAsync({ instanceId: "container-1" });
    });
    const stopHook = renderHook(() => useStopCloudRuntimeNode("ws-1"), {
      wrapper: wrapper(qc),
    });
    await act(async () => {
      await stopHook.result.current.mutateAsync({ instanceId: "container-1" });
    });
    const rebootHook = renderHook(() => useRebootCloudRuntimeNode("ws-1"), {
      wrapper: wrapper(qc),
    });
    await act(async () => {
      await rebootHook.result.current.mutateAsync({ instanceId: "container-1" });
    });

    expect(start).toHaveBeenCalledWith("container-1", expect.any(String));
    expect(stop).toHaveBeenCalledWith("container-1", expect.any(String));
    expect(reboot).toHaveBeenCalledWith("container-1", expect.any(String));
    const keys = [
      start.mock.calls[0]![1] as string,
      stop.mock.calls[0]![1] as string,
      reboot.mock.calls[0]![1] as string,
    ];
    for (const key of keys) expect(key.length).toBeGreaterThan(0);
    expect(new Set(keys).size).toBe(keys.length);
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: cloudRuntimeKeys.all("ws-1"),
    });
  });

  it("reuses an explicit Idempotency-Key and can delete by node id", async () => {
    const del = vi.fn().mockResolvedValue(undefined);
    setApiInstance({ deleteCloudRuntimeNode: del } as unknown as ApiClientType);

    const qc = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    });
    const hook = renderHook(() => useDeleteCloudRuntimeNode("ws-1"), {
      wrapper: wrapper(qc),
    });
    await act(async () => {
      await hook.result.current.mutateAsync({
        nodeId: "node-1",
        idempotencyKey: "retry-key",
      });
    });
    expect(del).toHaveBeenCalledWith(
      { instanceId: undefined, nodeId: "node-1" },
      "retry-key",
    );
  });

  it("scopes the service query to the workspace", () => {
    expect(cloudRuntimeServiceKeys.service("ws-1")).toEqual([
      ...cloudRuntimeKeys.all("ws-1"),
      "service",
    ]);
    expect(cloudRuntimeServiceOptions("ws-1").queryKey).toEqual(
      cloudRuntimeServiceKeys.service("ws-1"),
    );
  });
});
