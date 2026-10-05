// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { AgentRuntime } from "../types";
import { getManagedFleetNodeID } from "./managed-fleet-runtime";

const runtime = (metadata: Record<string, unknown>): AgentRuntime => ({
  id: "runtime",
  workspace_id: "workspace",
  daemon_id: null,
  name: "worker",
  runtime_mode: "local",
  provider: "claude",
  launch_header: "",
  status: "online",
  device_info: "",
  metadata,
  owner_id: "owner",
  visibility: "private",
  last_seen_at: null,
  created_at: "",
  updated_at: "",
});
const nodeID = "22222222-2222-4222-8222-222222222222";
describe("validated managed fleet runtime metadata", () => {
  it("returns only a validated managed node id without requiring a new runtime mode", () => {
    expect(
      getManagedFleetNodeID(
        runtime({ managed_by: "local_fleet", fleet_node_id: nodeID, extra: "allowed" }),
      ),
    ).toBe(nodeID);
  });
  it.each([
    {},
    { managed_by: "local_fleet" },
    { fleet_node_id: nodeID },
    { managed_by: "cloud", fleet_node_id: nodeID },
    { managed_by: "local_fleet", fleet_node_id: 7 },
    { managed_by: "local_fleet", fleet_node_id: "" },
    { managed_by: "local_fleet", fleet_node_id: "not-a-uuid" },
  ])("returns null for missing or malformed metadata %j", (metadata) => {
    expect(getManagedFleetNodeID(runtime(metadata))).toBeNull();
  });
});
