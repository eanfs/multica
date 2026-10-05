import { ManagedFleetRuntimeMetadataSchema } from "../api/schemas";
import type { AgentRuntime } from "../types";

/** Only local Fleet metadata carries a UUID; hosted node IDs remain strings. */
export function getManagedFleetNodeID(runtime: AgentRuntime): string | null {
  const parsed = ManagedFleetRuntimeMetadataSchema.safeParse(runtime.metadata);
  return parsed.success ? parsed.data.fleet_node_id : null;
}
