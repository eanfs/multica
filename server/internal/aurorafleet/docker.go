package aurorafleet

import (
	"bytes"
	"context"
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

// WorkspaceNodeStatus reports the sandbox container's state for the node.
func (d *DockerBackend) WorkspaceNodeStatus(ctx context.Context, nodeID string) (Node, error) {
	out, err := d.run(ctx, "ps", "-a", "--filter", "id="+nodeID, "--format", dockerPSFormat)
	if err != nil {
		return Node{}, err
	}
	state, ok := parsePSLine(string(out))
	if !ok {
		return Node{}, ErrNotFound
	}
	return Node{ID: nodeID, State: state}, nil
}

// DeleteWorkspaceNode destroys the node's containers.
func (d *DockerBackend) DeleteWorkspaceNode(ctx context.Context, nodeID string) error {
	_, err := d.run(ctx, "rm", "-f", nodeID)
	return err
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
