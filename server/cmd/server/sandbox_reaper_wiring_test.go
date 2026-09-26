package main

import (
	"context"
	"testing"
)

// TestSweepAuroraSandboxNodesSkipsWithoutReaper pins the fail-closed wiring: a
// deployment without fleet configuration exposes no reaper, so the periodic
// stage must do nothing rather than reach for a fleet that is not there.
func TestSweepAuroraSandboxNodesSkipsWithoutReaper(t *testing.T) {
	sweepAuroraSandboxNodes(context.Background(), nil)
}
