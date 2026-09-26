package aurorafleet

import (
	"context"
	"errors"
)

// WorkspaceNodeSpec describes one workspace-scoped sandbox node to provision.
// The controller, not the API caller, owns everything the sandbox runs with:
// the request carries identity only, and the enrollment secret is staged by
// the controller into EnrollmentFile, which the backend mounts read-only.
type WorkspaceNodeSpec struct {
	NodeID         string
	WorkspaceID    string
	RuntimeID      string
	DaemonID       string
	EnrollmentFile string
}

// Node is the fleet's view of one workspace sandbox node.
type Node struct {
	ID        string `json:"id"`
	ProxyID   string `json:"proxy_id"`
	NetworkID string `json:"network_id"`
	State     string `json:"state"`
	Health    string `json:"health"`
}

var (
	// ErrNotFound is returned when an operation names a node the backend does
	// not know.
	ErrNotFound = errors.New("node not found")
	// ErrUnavailable is returned when the backend cannot reach its underlying
	// runtime (for example the Docker daemon is down).
	ErrUnavailable = errors.New("node backend unavailable")
)

// Backend provisions and manages workspace sandbox nodes. It is the seam that
// lets the controller run against real Docker containers in production and an
// in-memory registry in tests without the HTTP layer knowing which is in use.
type Backend interface {
	// EnsureWorkspaceNode provisions the workspace node described by spec (or
	// confirms the existing one) and returns its current state.
	EnsureWorkspaceNode(ctx context.Context, spec WorkspaceNodeSpec) (Node, error)
	// WorkspaceNodeStatus returns a node's current state.
	WorkspaceNodeStatus(ctx context.Context, nodeID string) (Node, error)
	// DeleteWorkspaceNode destroys a node and its network and proxy sidecar.
	DeleteWorkspaceNode(ctx context.Context, nodeID string) error
	// Reconcile restores label-based invariants after a fleet restart.
	Reconcile(ctx context.Context) error
	// Ping reports whether the backend's underlying runtime is reachable. It
	// is the ready signal the controller surfaces.
	Ping(ctx context.Context) error
}
