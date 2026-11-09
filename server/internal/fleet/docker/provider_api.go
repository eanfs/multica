package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// Labels identify resources owned by one checkout's Fleet namespace. Cleanup
// must match these labels together with Store ownership, never a name prefix.
const (
	LabelNamespace = "multica.fleet.namespace"
	LabelNode      = "multica.fleet.node"
	LabelRole      = "multica.fleet.role"
	RoleNode       = "node"
	// RoleSeed marks the short-lived helper container that seeds node volumes.
	// A crashed seed helper can be found and force-removed by this label.
	RoleSeed = "seed"
)

const (
	// maxNodeMemoryMB bounds the memory limit so a caller cannot overflow the
	// MB-to-bytes conversion or hand one node an implausible share of the host.
	maxNodeMemoryMB = 64 * 1024
)

const (
	// nodeUID and nodeGID are the fixed non-root identity of the node process.
	nodeUID = 1000
	nodeGID = 1000
	// nodeUser is the Docker user string that selects nodeUID/nodeGID.
	nodeUser = "1000:1000"
	// dataMountPath holds CLI configuration, the daemon identity, sessions,
	// and run working directories.
	dataMountPath = "/home/multica"
	// secretsMountPath holds bootstrap and model credentials. In the node it is
	// mounted read-only; only the fixed entrypoint reads it. The seed helper
	// mounts the same volume read-write before the node is created.
	secretsMountPath = "/run/secrets/runtime"
	// seedDataMountPath is where the seed helper mounts the data volume. It is
	// deliberately different from dataMountPath so it cannot be confused with
	// the node's own mount.
	seedDataMountPath = "/data"
	// healthURL is the fixed loopback address the node daemon health tool
	// listens on. No management port is published to the host.
	healthURL = "http://127.0.0.1:19514/health"
)

// ObservedContainer is the subset of Docker container state the Fleet
// reconciler needs. Healthy is false when the container has no healthcheck,
// is stopped, or reports "unhealthy" or "starting".
type ObservedContainer struct {
	ID      string
	State   string
	Healthy bool
}

// HealthReport is the node daemon's own health payload. Fleet treats it as an
// observation, not an authorization to claim work.
type HealthReport struct {
	Status                     string   `json:"status"`
	ActiveTaskCount            int      `json:"active_task_count"`
	RunningTaskCount           int      `json:"running_task_count"`
	ResourceWaitTaskCount      int      `json:"resource_wait_task_count"`
	PendingTerminalReportCount int      `json:"pending_terminal_report_count"`
	Agents                     []string `json:"agents"`
}

// ParseHealthReport decodes the daemon health payload. Callers must treat a
// parse error as unknown health, never as an idle node.
func ParseHealthReport(body []byte) (HealthReport, error) {
	var report HealthReport
	if err := json.Unmarshal(body, &report); err != nil {
		return HealthReport{}, fmt.Errorf("parse daemon health: %w", err)
	}
	return report, nil
}

// NodeSpec is the complete description of one node container. It has no field
// for privileged mode, host namespaces, published ports, or secrets, so callers
// cannot request them through this type.
type NodeSpec struct {
	NodeID        string
	Namespace     string
	Name          string
	Image         string
	Env           []string
	DataVolume    string
	SecretsVolume string
	Network       string
	MemoryMB      int64
	CPU           float64
	PidsLimit     int64
	Labels        map[string]string
}

// BuildNodeSpec returns the standard node spec for one Fleet node. Environment
// variables are left empty: platform variables are set by the caller and
// credentials are seeded through Provider.PrepareVolumes, never through Env.
func BuildNodeSpec(nodeID, namespace, name, image, dataVolume, secretsVolume, network string, memoryMB int64, cpu float64, pids int64) NodeSpec {
	return NodeSpec{
		NodeID:        nodeID,
		Namespace:     namespace,
		Name:          name,
		Image:         image,
		DataVolume:    dataVolume,
		SecretsVolume: secretsVolume,
		Network:       network,
		MemoryMB:      memoryMB,
		CPU:           cpu,
		PidsLimit:     pids,
		Labels: map[string]string{
			LabelNamespace: namespace,
			LabelNode:      nodeID,
			LabelRole:      RoleNode,
		},
	}
}

// allowedNodeEnv is the complete set of variables a node container may receive.
// It holds no credential: model keys are read from the read-only secrets volume
// by the entrypoint, and the daemon authenticates with its written CLI config.
var allowedNodeEnv = map[string]struct{}{
	"MULTICA_SERVER_URL":         {},
	"MULTICA_DAEMON_ID":          {},
	"MULTICA_DAEMON_DEVICE_NAME": {},
	"MULTICA_WORKSPACES_ROOT":    {},
	"MULTICA_DAEMON_AUTO_UPDATE": {},
	"MULTICA_DAEMON_AUTO_RELOAD": {},
	"HOME":                       {},
}

// validateNodeEnv rejects any variable outside the explicit allowlist so a
// secret cannot be smuggled into Config.Env or a node process cannot be
// redirected to another binary or configuration root.
func validateNodeEnv(env []string) error {
	seen := make(map[string]struct{}, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return fmt.Errorf("invalid node env entry %q", entry)
		}
		if _, allowed := allowedNodeEnv[key]; !allowed {
			return fmt.Errorf("node env %q is not allowed", key)
		}
		if strings.ContainsAny(value, "\x00\n\r") {
			return fmt.Errorf("node env %q contains control characters", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("node env %q is set more than once", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// validateNodeSpec enforces the invariants that keep a node confined to its
// namespace: a dedicated user-defined network, bounded resources, distinct
// volumes, and the labels cleanup depends on.
func validateNodeSpec(spec NodeSpec) error {
	switch {
	case spec.Image == "":
		return fmt.Errorf("node spec: image is required")
	case spec.Name == "":
		return fmt.Errorf("node spec: container name is required")
	case spec.NodeID == "":
		return fmt.Errorf("node spec: node id is required")
	case spec.Namespace == "":
		return fmt.Errorf("node spec: namespace is required")
	case spec.DataVolume == "" || spec.SecretsVolume == "":
		return fmt.Errorf("node spec: data and secrets volumes are required")
	case spec.DataVolume == spec.SecretsVolume:
		return fmt.Errorf("node spec: data and secrets volumes must differ")
	case spec.Network == "":
		return fmt.Errorf("node spec: network is required")
	case isUnsafeNetworkMode(spec.Network):
		return fmt.Errorf("node spec: network mode %q is not allowed", spec.Network)
	case spec.MemoryMB <= 0:
		return fmt.Errorf("node spec: memory limit is required")
	case spec.MemoryMB > maxNodeMemoryMB:
		return fmt.Errorf("node spec: memory limit %d MB exceeds %d MB", spec.MemoryMB, maxNodeMemoryMB)
	// NaN compares false with everything, so it must be rejected explicitly
	// before the positivity check; Inf likewise cannot become NanoCPUs.
	case math.IsNaN(spec.CPU) || math.IsInf(spec.CPU, 0):
		return fmt.Errorf("node spec: cpu limit must be finite")
	case spec.CPU <= 0:
		return fmt.Errorf("node spec: cpu limit is required")
	case spec.PidsLimit <= 0:
		return fmt.Errorf("node spec: pids limit is required")
	}
	if err := validateNodeEnv(spec.Env); err != nil {
		return err
	}
	if got := spec.Labels[LabelNamespace]; got != spec.Namespace {
		return fmt.Errorf("node spec: label %s = %q, want %q", LabelNamespace, got, spec.Namespace)
	}
	if got := spec.Labels[LabelNode]; got != spec.NodeID {
		return fmt.Errorf("node spec: label %s = %q, want %q", LabelNode, got, spec.NodeID)
	}
	if got := spec.Labels[LabelRole]; got != RoleNode {
		return fmt.Errorf("node spec: label %s = %q, want %q", LabelRole, got, RoleNode)
	}
	return nil
}

// isUnsafeNetworkMode reports whether a network mode would share the host, the
// Fleet container, or no network at all instead of a namespace bridge.
func isUnsafeNetworkMode(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "host", "none", "bridge", "default":
		return true
	}
	return strings.HasPrefix(lower, "container:")
}

// Provider owns the Docker resources of Fleet nodes. Implementations must not
// expose image pulls, command execution, or arbitrary container access.
type Provider interface {
	Ping(ctx context.Context) error
	// InspectImage reports whether an already-built image is available locally.
	// Fleet never pulls or builds an image as part of a create request.
	InspectImage(ctx context.Context, image string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	CreateContainer(ctx context.Context, spec NodeSpec) (string, error)
	// PrepareVolumes seeds a node's data and secrets volumes before the node
	// container starts. It runs a short-lived, network-less helper container as
	// root because a fresh named volume is root-owned, while the node runs as
	// 1000:1000 and mounts the secrets volume read-only. files are written to
	// the secrets volume only; the data volume is created empty and owned by
	// the node identity.
	PrepareVolumes(ctx context.Context, dataVolume, secretsVolume, image string, files map[string][]byte) error
	Start(ctx context.Context, containerID string) error
	Stop(ctx context.Context, containerID string, timeout time.Duration) error
	Restart(ctx context.Context, containerID string, timeout time.Duration) error
	Remove(ctx context.Context, containerID string, removeVolumes bool) error
	RemoveVolume(ctx context.Context, name string) error
	Inspect(ctx context.Context, containerID string) (ObservedContainer, error)
	ListByLabels(ctx context.Context, labels map[string]string) ([]string, error)
	// ExecHealth runs the fixed loopback health command and returns its stdout.
	ExecHealth(ctx context.Context, containerID string) ([]byte, error)
	// Close releases the Docker API client.
	Close() error
}
