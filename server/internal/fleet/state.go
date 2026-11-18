package fleet

const (
	StateLaunching       = "launching"
	StateStarting        = "starting"
	StateRunning         = "running"
	StateStopping        = "stopping"
	StateStopped         = "stopped"
	StateRebooting       = "rebooting"
	StateTerminating     = "terminating"
	StateTerminated      = "terminated"
	StateFailed          = "failed"
	StateInstanceMissing = "instance_missing"
)

// Observed carries the signals observed outside the desired state. They are
// persisted separately so a stale `running` target cannot imply readiness.
type Observed struct {
	ContainerExists bool
	ContainerState  string
	Healthy         bool
	DaemonReady     bool
	HealthFresh     bool
}

// DeriveDisplayState maps the persisted target state and the observed signals
// to the state shown to users. A running target only reads as executable when
// the container exists, is running, is healthy, and both the daemon and the
// health sample are fresh. Any missing or unknown signal falls back to
// starting, stopped, or instance_missing, never to running.
func DeriveDisplayState(target string, obs Observed) string {
	switch target {
	case StateLaunching:
		return StateLaunching
	case StateStarting:
		if !obs.ContainerExists {
			return StateInstanceMissing
		}
		if obs.ContainerState == "running" && obs.Healthy && obs.DaemonReady && obs.HealthFresh {
			return StateRunning
		}
		return StateStarting
	case StateRunning:
		if !obs.ContainerExists {
			return StateInstanceMissing
		}
		if obs.ContainerState == "exited" || obs.ContainerState == "dead" {
			return StateStopped
		}
		if obs.ContainerState == "running" && obs.Healthy && obs.DaemonReady && obs.HealthFresh {
			return StateRunning
		}
		return StateStarting
	case StateStopping:
		// A stop request is satisfied as soon as the container is no longer
		// running. Reporting stopping while the container is exited (or already
		// gone) kept the API's delete gate from seeing a settled node, so it
		// diagnosed an already stopped container and rejected the delete.
		if !obs.ContainerExists {
			return StateStopped
		}
		if obs.ContainerState == "exited" || obs.ContainerState == "dead" {
			return StateStopped
		}
		return StateStopping
	case StateStopped:
		return StateStopped
	case StateRebooting:
		return StateRebooting
	case StateTerminating:
		return StateTerminating
	case StateTerminated:
		return StateTerminated
	default:
		return StateFailed
	}
}
