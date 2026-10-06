package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

const deleteEpoch = "2026-01-01T00:00:00Z"

// deleteEngine scripts one idle running node: the online proof is readable, the
// container reports running until Stop is requested, and every removal is
// recorded in order. Embedding Engine satisfies the unused methods.
type deleteEngine struct {
	Engine
	online  []byte
	stopped bool
	order   []string
}

func (d *deleteEngine) Inspect(_ context.Context, id string) (Inspection, error) {
	state := "running"
	if d.stopped {
		state = "exited"
	}
	return Inspection{ID: id, State: state, StartedAt: deleteEpoch, Labels: fixtureLabels("node"), HealthJSON: d.online}, nil
}
func (d *deleteEngine) Stop(context.Context, string) error {
	d.stopped = true
	d.order = append(d.order, "stop")
	return nil
}
func (d *deleteEngine) FixedHealth(context.Context, string) ([]byte, error) { return d.online, nil }
func (d *deleteEngine) FixedOfflineReports(context.Context, model.Node, model.OperationRef) ([]byte, error) {
	return []byte(goodOffline), nil
}
func (d *deleteEngine) Remove(context.Context, string) error {
	d.order = append(d.order, "container")
	return nil
}
func (d *deleteEngine) RemoveVolume(_ context.Context, r Resource) error {
	d.order = append(d.order, r.Role)
	return nil
}

func deletingRunningNode() model.Node {
	n := fixtureNode()
	n.Revoked = true
	n.Desired = "terminating"
	n.StartEpoch = deleteEpoch
	return n
}

// Deleting an idle running node stops it before the stopped/missing volume
// proof; otherwise the approved delete retries forever with unknown health.
func TestDeleteStopsIdleRunningNodeBeforeRemoval(t *testing.T) {
	e := &deleteEngine{online: []byte(goodHealth)}
	p := New(e, fixtureConfig())
	if err := p.Delete(context.Background(), deletingRunningNode(), fixtureRef()); err != nil {
		t.Fatalf("delete idle running node: %v", err)
	}
	if !e.stopped || strings.Join(e.order, ",") != "stop,container,secrets,data" {
		t.Fatalf("stop/removal order = %v (stopped=%v)", e.order, e.stopped)
	}
}

// A running node without its own online proof must not be stopped or removed.
func TestDeleteRefusesRunningNodeWithoutOnlineProof(t *testing.T) {
	unknown := []byte(strings.Replace(goodHealth, "ready\":true", "ready\":false", 1))
	e := &deleteEngine{online: unknown}
	p := New(e, fixtureConfig())
	if err := p.Delete(context.Background(), deletingRunningNode(), fixtureRef()); !errors.Is(err, model.ErrUnknownHealth) {
		t.Fatalf("delete without online proof = %v", err)
	}
	if e.stopped || len(e.order) != 0 {
		t.Fatalf("unproven running node mutated: stopped=%v order=%v", e.stopped, e.order)
	}
}
