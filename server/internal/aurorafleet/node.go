package aurorafleet

import "time"

// Status is a node lifecycle state. The vocabulary matches the cloud-runtime
// statuses the frontend already keys off, scoped to what a self-host container
// fleet can actually report (no EC2 launch/pending semantics).
type Status string

const (
	// StatusProvisioning is a node whose container has been created but is not
	// yet accepting work.
	StatusProvisioning Status = "provisioning"
	// StatusRunning is a healthy node ready to claim tasks.
	StatusRunning Status = "running"
	// StatusStopped is a node whose container exists but is not running.
	StatusStopped Status = "stopped"
	// StatusRebooting is a node mid-restart.
	StatusRebooting Status = "rebooting"
	// StatusTerminating is a node being torn down.
	StatusTerminating Status = "terminating"
	// StatusError is a node whose container failed.
	StatusError Status = "error"
)

// Node is a single sandbox node in the fleet. ID is the backend's native
// identifier (a Docker container id for the Docker backend) and is opaque to
// callers; it is what the action endpoints address.
type Node struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Status    Status            `json:"status"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}
