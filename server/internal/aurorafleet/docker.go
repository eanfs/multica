package aurorafleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// fleetLabel marks containers the fleet owns so host-level filtering can
// distinguish them from unrelated containers.
const fleetLabel = "multica-aurora-node=1"

// dockerPSFormat is the docker ps format used by WorkspaceNodeStatus. It
// emits one tab-separated row per container with the machine-readable State
// (rather than the localized human text) so state mapping stays a fixed
// enum, plus the container's creation timestamp.
const dockerPSFormat = "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}"

// errProvisioningNotImplemented fails closed until the hardened workspace
// Docker policy lands: without the digest-pinned image, per-workspace
// networks, and immutable security flags there is no safe way to provision a
// workspace node from this backend.
var errProvisioningNotImplemented = errors.New("workspace node provisioning requires the hardened Docker policy (not yet implemented)")

// DockerBackend manages workspace nodes as Docker containers via the docker
// CLI. It shells out to docker rather than depending on the Docker SDK so the
// fleet controller stays a single self-contained binary. Container ids are
// the node ids.
type DockerBackend struct {
	dockerPath string
}

// NewDockerBackend returns a DockerBackend that talks to the docker binary.
func NewDockerBackend() *DockerBackend {
	return &DockerBackend{dockerPath: "docker"}
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

// EnsureWorkspaceNode provisions the workspace node. It fails closed until
// the hardened Docker policy (per-workspace internal network, egress sidecar,
// digest-pinned image, immutable security flags) is implemented.
func (d *DockerBackend) EnsureWorkspaceNode(context.Context, WorkspaceNodeSpec) (Node, error) {
	return Node{}, errProvisioningNotImplemented
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

// Reconcile is not implemented yet; the label-based startup reconciliation
// arrives with the hardened Docker policy.
func (d *DockerBackend) Reconcile(context.Context) error {
	return errProvisioningNotImplemented
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
