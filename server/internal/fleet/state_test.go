package fleet

import "testing"

func TestDeriveDisplayState(t *testing.T) {
	running := Observed{ContainerExists: true, ContainerState: "running", Healthy: true, DaemonReady: true, HealthFresh: true}
	cases := []struct {
		name   string
		target string
		obs    Observed
		want   string
	}{
		{"launching stays launching", StateLaunching, Observed{}, StateLaunching},
		{"running and ready", StateRunning, running, StateRunning},
		{"running but daemon not ready", StateRunning, Observed{ContainerExists: true, ContainerState: "running", Healthy: true}, StateStarting},
		{"running but health stale", StateRunning, Observed{ContainerExists: true, ContainerState: "running", Healthy: true, DaemonReady: true}, StateStarting},
		{"running but container missing", StateRunning, Observed{}, StateInstanceMissing},
		{"running but exited", StateRunning, Observed{ContainerExists: true, ContainerState: "exited", Healthy: false}, StateStopped},
		{"stopping while the container runs", StateStopping, running, StateStopping},
		// A stop has achieved its goal once the container is no longer running;
		// the API can then treat the node as settled for a following delete.
		{"stopping with an exited container", StateStopping, Observed{ContainerExists: true, ContainerState: "exited"}, StateStopped},
		{"stopping with a dead container", StateStopping, Observed{ContainerExists: true, ContainerState: "dead"}, StateStopped},
		{"stopping without a container", StateStopping, Observed{}, StateStopped},
		{"stopping with an unreadable container state", StateStopping, Observed{ContainerExists: true, ContainerState: "created"}, StateStopping},
		{"stopped", StateStopped, Observed{ContainerExists: true, ContainerState: "exited"}, StateStopped},
		{"rebooting", StateRebooting, running, StateRebooting},
		{"terminating", StateTerminating, running, StateTerminating},
		{"terminated", StateTerminated, Observed{}, StateTerminated},
		{"failed", StateFailed, running, StateFailed},

		// Safety matrix: a running target must never read as executable
		// unless every readiness signal agrees.
		{"running but unhealthy", StateRunning, Observed{ContainerExists: true, ContainerState: "running", DaemonReady: true, HealthFresh: true}, StateStarting},
		{"running but container dead", StateRunning, Observed{ContainerExists: true, ContainerState: "dead"}, StateStopped},
		{"running with unknown container state", StateRunning, Observed{ContainerExists: true, ContainerState: "created", Healthy: true, DaemonReady: true, HealthFresh: true}, StateStarting},
		{"starting and ready becomes running", StateStarting, running, StateRunning},
		{"starting without container is instance missing", StateStarting, Observed{}, StateInstanceMissing},
		{"unknown target is failed", "bogus", running, StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveDisplayState(tc.target, tc.obs); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
