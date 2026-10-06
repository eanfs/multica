export type NodeAction = "create" | "start" | "stop" | "reboot" | "delete";

export interface CloudRuntimeCapabilities {
  provider: "docker" | "cloud" | "unknown";
  operations: NodeAction[];
  specs: Array<{ id: string; cpus: number; memoryBytes: number; pids: number }>;
  persistentStorage: boolean;
  diskQuotaSupported: boolean;
}

export const EMPTY_CLOUD_RUNTIME_CAPABILITIES: CloudRuntimeCapabilities = {
  provider: "unknown",
  operations: [],
  specs: [],
  persistentStorage: false,
  diskQuotaSupported: false,
};

export function supportsNodeAction(caps: CloudRuntimeCapabilities, action: NodeAction): boolean {
  return caps.provider !== "unknown" && caps.operations.includes(action);
}
