package aurorafleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// fleetLabel marks containers and networks the fleet owns so host-level
// filtering can distinguish them from unrelated resources.
const fleetLabel = "multica-aurora-node=1"

// dockerPSFormat is the docker ps format used by WorkspaceNodeStatus. It
// emits one tab-separated row per container with the machine-readable State
// (rather than the localized human text) so state mapping stays a fixed
// enum, plus the container's creation timestamp.
const dockerPSFormat = "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}"

// nodeProvisionTimeout bounds one EnsureWorkspaceNode call's docker work.
const nodeProvisionTimeout = 90 * time.Second

// sandboxStartPollInterval and sandboxStartTimeout bound the wait for the
// sandbox container to reach the running state after creation.
const (
	sandboxStartPollInterval = 500 * time.Millisecond
	sandboxStartTimeout      = 30 * time.Second
)

// DockerBackend manages workspace nodes as Docker containers via the docker
// CLI. It shells out to docker rather than depending on the Docker SDK so the
// fleet controller stays a single self-contained binary. Every container and
// network is created from the backend's immutable Policy; a backend without a
// valid policy fails closed.
type DockerBackend struct {
	dockerPath string
	policy     Policy
	// desired is the server's node view for reconciliation. It is optional: a
	// fleet without it removes only partial nodes.
	desired DesiredNodeState
}

// NewDockerBackend returns a DockerBackend with no policy configured. It
// fails closed: EnsureWorkspaceNode returns an error until a hardened Policy
// is supplied via NewDockerBackendWithPolicy.
func NewDockerBackend() *DockerBackend {
	return &DockerBackend{dockerPath: "docker"}
}

// NewDockerBackendWithPolicy returns a DockerBackend that provisions
// workspace nodes from the given immutable policy.
func NewDockerBackendWithPolicy(policy Policy) *DockerBackend {
	return &DockerBackend{dockerPath: "docker", policy: policy}
}

// run runs docker and returns its stdout, folding stderr into the error on
// failure.
func (d *DockerBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.dockerPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isDockerUnavailable(err) {
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, strings.TrimSpace(errBuf.String()))
		}
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errBuf.String()))
	}
	return outBuf.Bytes(), nil
}

// EnsureWorkspaceNode provisions the workspace node: a workspace-private
// internal network, the egress sidecar, and the hardened sandbox container,
// all constructed from the backend's immutable Policy. On any failure the
// created resources are rolled back so no half-provisioned node survives.
func (d *DockerBackend) EnsureWorkspaceNode(ctx context.Context, spec WorkspaceNodeSpec) (Node, error) {
	if err := d.policy.Validate(); err != nil {
		return Node{}, fmt.Errorf("docker backend policy: %w", err)
	}
	network, proxyName, sandboxName, err := d.policy.NodeNames(spec)
	if err != nil {
		return Node{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, nodeProvisionTimeout)
	defer cancel()

	if err := d.ensureNetwork(ctx, network); err != nil {
		return Node{}, err
	}

	// Confirm an already-running node instead of recreating it. The
	// policy-derived names are deterministic, so a second ensure would
	// otherwise collide on the container names and roll the node back.
	existing, exists, err := d.existingSandboxState(ctx, sandboxName)
	if err != nil {
		return Node{}, err
	}
	if exists && existing == StateOnline {
		return Node{ID: sandboxName, ProxyID: proxyName, NetworkID: network, State: StateOnline, Health: HealthHealthy}, nil
	}
	if exists {
		// A non-running leftover cannot serve; remove it before the
		// deterministic names are recreated.
		_, _ = d.run(ctx, "rm", "-f", sandboxName)
		_, _ = d.run(ctx, "rm", "-f", proxyName)
	}

	node := Node{ID: sandboxName, ProxyID: proxyName, NetworkID: network, State: StateStarting}
	if err := d.createEgress(ctx, proxyName, network); err != nil {
		d.rollback(ctx, node, spec.EnrollmentFile)
		return Node{}, err
	}
	if err := d.createSandbox(ctx, spec); err != nil {
		d.rollback(ctx, node, spec.EnrollmentFile)
		return Node{}, err
	}
	if err := d.waitRunning(ctx, sandboxName); err != nil {
		d.rollback(ctx, node, spec.EnrollmentFile)
		return Node{}, err
	}
	return node, nil
}

// ensureNetwork creates the workspace network if it does not already exist.
func (d *DockerBackend) ensureNetwork(ctx context.Context, network string) error {
	if _, err := d.run(ctx, d.policy.WorkspaceNetworkCreateArgs(network)...); err != nil {
		// Another ensure for the same node may have created it first; any
		// existing network must be one of ours.
		if _, inspectErr := d.run(ctx, "network", "inspect", network); inspectErr != nil {
			return err
		}
	}
	return nil
}

// createEgress runs the egress sidecar on the uplink network and attaches it
// to the workspace-internal network under the "egress" alias.
func (d *DockerBackend) createEgress(ctx context.Context, proxyName, network string) error {
	args, err := d.policy.ProxyArgs(proxyName)
	if err != nil {
		return err
	}
	if _, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("create egress sidecar: %w", err)
	}
	if _, err := d.run(ctx, d.policy.EgressNetworkConnect(proxyName, network)...); err != nil {
		return fmt.Errorf("attach egress sidecar to workspace network: %w", err)
	}
	return nil
}

// createSandbox runs the sandbox container from the immutable policy.
func (d *DockerBackend) createSandbox(ctx context.Context, spec WorkspaceNodeSpec) error {
	args, err := d.policy.SandboxArgs(spec)
	if err != nil {
		return err
	}
	if _, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	return nil
}

// waitRunning polls until the sandbox container reports running.
func (d *DockerBackend) waitRunning(ctx context.Context, container string) error {
	deadline := time.Now().Add(sandboxStartTimeout)
	for {
		out, err := d.run(ctx, "inspect", "--format", "{{.State.Running}}", container)
		if err == nil && strings.TrimSpace(string(out)) == "true" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox container %s did not reach the running state", container)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sandboxStartPollInterval):
		}
	}
}

// rollback removes any containers, network, and staged secret created for a
// failed ensure. Errors are ignored: rollback is best-effort cleanup of
// best-effort state, and the controller also removes the secret after the
// call, so the removal is idempotent.
func (d *DockerBackend) rollback(ctx context.Context, node Node, secretPath string) {
	if node.ProxyID != "" {
		_, _ = d.run(ctx, "rm", "-f", node.ProxyID)
	}
	if node.ID != "" {
		_, _ = d.run(ctx, "rm", "-f", node.ID)
	}
	if node.NetworkID != "" {
		_, _ = d.run(ctx, "network", "rm", node.NetworkID)
	}
	if secretPath != "" {
		_ = os.Remove(secretPath)
	}
}

// WorkspaceNodeStatus reports the sandbox container's state for the node. A
// node UUID is resolved to its policy-derived sandbox name through the
// controlled identity label; a name the policy would generate is used as-is.
func (d *DockerBackend) WorkspaceNodeStatus(ctx context.Context, nodeID string) (Node, error) {
	sandbox, err := d.resolveSandboxName(ctx, nodeID)
	if err != nil {
		return Node{}, err
	}
	out, err := d.run(ctx, "ps", "-a", "--filter", "name=^"+sandbox+"$", "--format", dockerPSFormat)
	if err != nil {
		return Node{}, err
	}
	state, ok := parsePSLine(string(out))
	if !ok {
		return Node{}, ErrNotFound
	}
	health := HealthUnhealthy
	if state == StateOnline {
		health = HealthHealthy
	}
	return Node{ID: sandbox, State: state, Health: health}, nil
}

// existingSandboxState reports the state of the policy-derived sandbox
// container, or exists=false when no such container exists. The name filter is
// anchored so only the exact node is considered.
func (d *DockerBackend) existingSandboxState(ctx context.Context, sandbox string) (state string, exists bool, err error) {
	out, err := d.run(ctx, "ps", "-a", "--filter", "name=^"+sandbox+"$", "--format", dockerPSFormat)
	if err != nil {
		return "", false, err
	}
	state, exists = parsePSLine(string(out))
	return state, exists, nil
}

// resolveSandboxName maps a node UUID to its policy-derived sandbox name
// through the controlled identity label. A name already carrying the sandbox
// prefix is returned unchanged.
func (d *DockerBackend) resolveSandboxName(ctx context.Context, nodeID string) (string, error) {
	if strings.HasPrefix(nodeID, sandboxNamePrefix) {
		return nodeID, nil
	}
	return d.findSandboxByNodeID(ctx, nodeID)
}

// DeleteWorkspaceNode destroys a workspace node: the sandbox container, its
// egress sidecar, and the workspace-internal network. nodeID is either the
// policy-derived sandbox name the fleet reports or the node UUID the control
// API addresses; a UUID is resolved through the controlled node label, so only
// fleet-owned containers are ever considered. The sidecar and network names are
// derived from the resolved sandbox name, which Policy.NodeNames builds from the
// same hash prefix, so no caller-supplied name can reach another resource.
func (d *DockerBackend) DeleteWorkspaceNode(ctx context.Context, nodeID string) error {
	sandbox, err := d.removeSandbox(ctx, nodeID)
	if err != nil {
		return err
	}
	proxy, network, ok := siblingNodeNames(sandbox)
	if !ok {
		return nil
	}
	// Detach the sidecar before removing the network it shares with the sandbox.
	_, _ = d.run(ctx, "rm", "-f", proxy)
	_, _ = d.run(ctx, "network", "rm", network)
	return nil
}

// removeSandbox removes the sandbox container addressed by a policy-derived name
// or a node UUID and returns the container name it removed.
func (d *DockerBackend) removeSandbox(ctx context.Context, nodeID string) (string, error) {
	// Only a policy-derived sandbox name can be removed directly. Any other
	// reference (a node UUID) must resolve through the controlled label first:
	// some Docker versions report success for a missing reference, so the exit
	// code alone cannot prove a sandbox was removed.
	if _, _, isSandboxName := siblingNodeNames(nodeID); isSandboxName {
		if _, err := d.run(ctx, "rm", "-f", nodeID); err == nil {
			return nodeID, nil
		}
	}
	name, err := d.findSandboxByNodeID(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if _, err := d.run(ctx, "rm", "-f", name); err != nil {
		return "", err
	}
	return name, nil
}

// findSandboxByNodeID resolves a node UUID to a fleet-owned sandbox through the
// controlled node label. It only ever returns a policy-derived sandbox name, so
// a label collision cannot redirect the delete to an unrelated container.
func (d *DockerBackend) findSandboxByNodeID(ctx context.Context, nodeID string) (string, error) {
	out, err := d.run(ctx, "ps", "-a",
		"--filter", "label="+nodeIdentityLabel+"="+nodeID,
		"--format", "{{.Names}}")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if _, _, ok := siblingNodeNames(name); ok {
			return name, nil
		}
	}
	return "", ErrNotFound
}

// siblingNodeNames derives the egress sidecar and workspace network names from
// a policy-generated sandbox name. It reports ok=false for any name that is not
// an aurora-sbx-<hash> sandbox, so delete never touches an unrelated resource.
func siblingNodeNames(sandbox string) (proxy, network string, ok bool) {
	suffix, found := strings.CutPrefix(sandbox, sandboxNamePrefix)
	if !found || suffix == "" {
		return "", "", false
	}
	return proxyNamePrefix + suffix, networkNamePrefix + suffix, true
}

// SetDesiredNodeState supplies the server's node view to reconciliation.
func (d *DockerBackend) SetDesiredNodeState(desired DesiredNodeState) {
	d.desired = desired
}

// Reconcile removes fleet-owned Docker resources the server no longer expects.
// Only label-filtered inventory is examined, so user containers and networks
// are never candidates.
func (d *DockerBackend) Reconcile(ctx context.Context) error {
	store := NewDockerResourceStore(func(ctx context.Context, args ...string) (string, error) {
		out, err := d.run(ctx, args...)
		return string(out), err
	})
	_, err := NewReconciler(store, d.desired, reconcileGrace, nil, nil).Reconcile(ctx)
	return err
}

// Ping reports whether the Docker daemon is reachable. It is the ready
// signal the controller surfaces.
func (d *DockerBackend) Ping(ctx context.Context) error {
	_, err := d.run(ctx, "info")
	return err
}

// parsePSLine decodes one docker ps row and returns the container's state.
// It reports ok=false for a blank or malformed line.
func parsePSLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	fields := strings.Split(line, "\t")
	if len(fields) < 4 {
		return "", false
	}
	return dockerStateToNodeState(fields[3]), true
}

// dockerStateToNodeState maps a Docker container State (created/running/
// restarting/removing/exited/dead) onto the fleet's lifecycle vocabulary.
// State is the machine-readable field; it is deliberately not the localized
// human text, which would force prefix matching.
func dockerStateToNodeState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "created":
		return StateStarting
	case "running":
		return StateOnline
	case "restarting":
		return StateStarting
	case "removing":
		return StateDraining
	case "dead":
		return StateFailed
	default: // "exited", "paused", unknown
		return StateStopped
	}
}

func isDockerUnavailable(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// The binary itself could not be found or launched.
		return true
	}
	// docker exits 125 when the daemon is unreachable.
	return exitErr.ExitCode() == 125
}

// ContainerInspect is the machine-readable subset of `docker inspect` that
// proves the hardened container boundary. It is evidence and diagnostics only:
// nothing here feeds back into the Policy used to create a node.
type ContainerInspect struct {
	ID              string                   `json:"Id"`
	Name            string                   `json:"Name"`
	Image           string                   `json:"Image"`
	Config          ContainerInspectConfig   `json:"Config"`
	HostConfig      ContainerInspectHost     `json:"HostConfig"`
	NetworkSettings ContainerInspectNetworks `json:"NetworkSettings"`
	Mounts          []ContainerMount         `json:"Mounts"`
}

// ContainerInspectConfig is the container's immutable run configuration.
type ContainerInspectConfig struct {
	User       string            `json:"User"`
	Image      string            `json:"Image"`
	Env        []string          `json:"Env"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Labels     map[string]string `json:"Labels"`
}

// ContainerInspectHost is the subset of HostConfig the acceptance assertions
// cover. PidsLimit is a pointer because Docker reports it as null when unset.
type ContainerInspectHost struct {
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop"`
	CapAdd         []string          `json:"CapAdd"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	Memory         int64             `json:"Memory"`
	MemorySwap     int64             `json:"MemorySwap"`
	NanoCpus       int64             `json:"NanoCpus"`
	PidsLimit      *int64            `json:"PidsLimit"`
	Privileged     bool              `json:"Privileged"`
	Devices        []ContainerDevice `json:"Devices"`
	PidMode        string            `json:"PidMode"`
	IpcMode        string            `json:"IpcMode"`
	UTSMode        string            `json:"UTSMode"`
	UsernsMode     string            `json:"UsernsMode"`
	NetworkMode    string            `json:"NetworkMode"`
	Binds          []string          `json:"Binds"`
	Mounts         []ContainerMount  `json:"Mounts"`
}

// ContainerDevice is one host device passed into a container.
type ContainerDevice struct {
	PathOnHost string `json:"PathOnHost"`
}

// ContainerMount is one mount visible to the container.
type ContainerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ContainerInspectNetworks is the container's attached-network view.
type ContainerInspectNetworks struct {
	Networks map[string]ContainerEndpoint `json:"Networks"`
}

// ContainerEndpoint is one attached network endpoint.
type ContainerEndpoint struct {
	NetworkID string `json:"NetworkID"`
	IPAddress string `json:"IPAddress"`
	Gateway   string `json:"Gateway"`
}

// InspectContainer returns docker's machine-readable inspection for one
// container. The raw JSON is parsed rather than formatted, so callers get the
// same evidence the daemon returned.
func (d *DockerBackend) InspectContainer(ctx context.Context, container string) (ContainerInspect, error) {
	raw, err := d.InspectRaw(ctx, container)
	if err != nil {
		return ContainerInspect{}, err
	}
	return parseContainerInspect(raw, container)
}

// InspectRaw returns unmodified `docker inspect` JSON so a caller can scan
// the complete output, including fields the typed view omits, for secret material.
func (d *DockerBackend) InspectRaw(ctx context.Context, container string) ([]byte, error) {
	return d.run(ctx, "inspect", container)
}

// parseContainerInspect decodes the first element of a `docker inspect` array.
func parseContainerInspect(raw []byte, container string) (ContainerInspect, error) {
	var list []ContainerInspect
	if err := json.Unmarshal(raw, &list); err != nil {
		return ContainerInspect{}, fmt.Errorf("parse docker inspect for %s: %w", container, err)
	}
	if len(list) == 0 {
		return ContainerInspect{}, fmt.Errorf("docker inspect for %s returned no container", container)
	}
	return list[0], nil
}

// ImageHistory returns the full non-truncated layer command history for an
// image reference, used to prove no build layer embeds a runtime secret.
func (d *DockerBackend) ImageHistory(ctx context.Context, image string) (string, error) {
	out, err := d.run(ctx, "history", "--no-trunc", "--format", "{{.CreatedBy}}", image)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
