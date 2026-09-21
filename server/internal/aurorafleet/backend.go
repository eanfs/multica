package aurorafleet

import (
	"context"
	"errors"
)

// CreateRequest describes a sandbox node to provision. Image is the container
// image; Env is the environment injected into the node. The controller, not the
// caller, controls Env — it carries the server URL and managed-registration
// secret that must never round-trip through a client.
type CreateRequest struct {
	Name   string
	Image  string
	Env    map[string]string
	Labels map[string]string
}

// ExecResult is the output of running a command inside a node.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

var (
	// ErrNotFound is returned when an operation names a node the backend does
	// not know.
	ErrNotFound = errors.New("node not found")
	// ErrUnavailable is returned when the backend cannot reach its underlying
	// runtime (for example the Docker daemon is down).
	ErrUnavailable = errors.New("node backend unavailable")
)

// Backend provisions and manages sandbox nodes. It is the seam that lets the
// controller run against real Docker containers in production and an in-memory
// registry in tests without the HTTP layer knowing which is in use.
type Backend interface {
	// Create provisions a node and returns it. A freshly created node is not
	// necessarily running; implementations that need an async bring-up return a
	// provisioning node and let Status report progress.
	Create(ctx context.Context, req CreateRequest) (Node, error)
	// Terminate destroys a node.
	Terminate(ctx context.Context, id string) error
	// Start brings a stopped node back up.
	Start(ctx context.Context, id string) error
	// Stop halts a running node without destroying it.
	Stop(ctx context.Context, id string) error
	// Reboot restarts a node.
	Reboot(ctx context.Context, id string) error
	// Status returns a node's current state.
	Status(ctx context.Context, id string) (Node, error)
	// Exec runs a command inside a node.
	Exec(ctx context.Context, id string, command []string) (ExecResult, error)
	// List returns every node the backend knows about.
	List(ctx context.Context) ([]Node, error)
	// Ping reports whether the backend's underlying runtime is reachable. It is
	// the health/ready signal the controller surfaces.
	Ping(ctx context.Context) error
}
