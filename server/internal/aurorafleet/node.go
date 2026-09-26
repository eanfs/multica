package aurorafleet

// Node lifecycle states reported in Node.State. The vocabulary matches the
// workspace fleet lifecycle the server-side manager reconciles.
const (
	// StateStarting is a node whose containers exist but are not yet healthy.
	StateStarting = "starting"
	// StateOnline is a healthy node ready to claim tasks on its runtime.
	StateOnline = "online"
	// StateDraining is a node finishing its current task before teardown.
	StateDraining = "draining"
	// StateFailed is a node whose containers failed.
	StateFailed = "failed"
	// StateStopped is a node torn down but still known to the fleet.
	StateStopped = "stopped"
)

// Node health values reported in Node.Health.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)
