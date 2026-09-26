package aurorafleet

import (
	"context"
	"testing"
	"time"
)

// fakeResourceStore models Docker: it holds every resource with its labels and
// only ever hands the reconciler the ones carrying the managed label.
type fakeResourceStore struct {
	all     []labeledResource
	removed []string
}

type labeledResource struct {
	resource ManagedResource
	labels   map[string]string
}

func (s *fakeResourceStore) ListManaged(_ context.Context) ([]ManagedResource, error) {
	managed := make([]ManagedResource, 0, len(s.all))
	for _, item := range s.all {
		if item.labels[ManagedLabel] == ManagedLabelValue {
			managed = append(managed, item.resource)
		}
	}
	return managed, nil
}

func (s *fakeResourceStore) Remove(_ context.Context, resource ManagedResource) error {
	s.removed = append(s.removed, resource.ID)
	return nil
}

func (s *fakeResourceStore) contains(id string) bool {
	for _, item := range s.all {
		if item.resource.ID == id {
			return true
		}
	}
	return false
}

func (s *fakeResourceStore) wasRemoved(id string) bool {
	for _, removed := range s.removed {
		if removed == id {
			return true
		}
	}
	return false
}

type staticDesired map[string]struct{}

func (d staticDesired) DesiredNodeIDs(context.Context) (map[string]struct{}, error) {
	return d, nil
}

func managedLabeled(id, kind, nodeID string, age time.Duration, now time.Time) labeledResource {
	return labeledResource{
		resource: ManagedResource{ID: id, Kind: kind, NodeID: nodeID, CreatedAt: now.Add(-age)},
		labels:   map[string]string{ManagedLabel: ManagedLabelValue, NodeLabel: nodeID, KindLabel: kind},
	}
}

// TestFleetReconcileDeletesOrphanedLabeledContainers pins both removal paths:
// a complete node the server does not recognize goes away after the grace, and
// a partial node whose sandbox is missing goes away immediately.
func TestFleetReconcileDeletesOrphanedLabeledContainers(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeResourceStore{all: []labeledResource{
		managedLabeled("sbx-orphan", KindSandbox, "node-orphan", time.Hour, now),
		managedLabeled("egr-orphan", KindProxy, "node-orphan", time.Hour, now),
		managedLabeled("net-orphan", KindNetwork, "node-orphan", time.Hour, now),
		managedLabeled("egr-partial", KindProxy, "node-partial", time.Minute, now),
		managedLabeled("net-partial", KindNetwork, "node-partial", time.Minute, now),
		managedLabeled("sbx-kept", KindSandbox, "node-kept", time.Hour, now),
		managedLabeled("egr-kept", KindProxy, "node-kept", time.Hour, now),
		managedLabeled("sbx-fresh", KindSandbox, "node-fresh", time.Second, now),
		managedLabeled("egr-fresh", KindProxy, "node-fresh", time.Second, now),
	}}
	reconciler := NewReconciler(store, staticDesired{"node-kept": {}}, time.Minute, func() time.Time { return now }, nil)

	stats, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Examined != 9 {
		t.Fatalf("examined = %d, want 9", stats.Examined)
	}
	for _, id := range []string{"sbx-orphan", "egr-orphan", "net-orphan", "egr-partial", "net-partial"} {
		if !store.wasRemoved(id) {
			t.Errorf("%s was not removed: %v", id, store.removed)
		}
	}
	for _, id := range []string{"sbx-kept", "egr-kept", "sbx-fresh", "egr-fresh"} {
		if store.wasRemoved(id) {
			t.Errorf("%s was removed but must survive: %v", id, store.removed)
		}
	}
	if stats.Removed != 5 {
		t.Fatalf("removed = %d, want 5", stats.Removed)
	}
}

// TestFleetReconcileNeverTouchesUnlabeledContainers pins the label boundary:
// resources without the managed label are invisible to reconciliation even
// when their sandbox-shaped node is unrecognized.
func TestFleetReconcileNeverTouchesUnlabeledContainers(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeResourceStore{all: []labeledResource{
		{resource: ManagedResource{ID: "user-container", Kind: KindSandbox, NodeID: "node-user", CreatedAt: now.Add(-24 * time.Hour)}, labels: map[string]string{}},
		{resource: ManagedResource{ID: "user-network", Kind: KindNetwork, NodeID: "node-user", CreatedAt: now.Add(-24 * time.Hour)}, labels: map[string]string{"com.docker.compose.project": "user"}},
		managedLabeled("sbx-managed", KindSandbox, "node-managed", 24*time.Hour, now),
	}}
	reconciler := NewReconciler(store, staticDesired{}, time.Minute, func() time.Time { return now }, nil)

	stats, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Examined != 1 {
		t.Fatalf("examined = %d, want 1 (only the labeled resource)", stats.Examined)
	}
	for _, id := range []string{"user-container", "user-network"} {
		if store.wasRemoved(id) {
			t.Fatalf("unlabeled %s was removed", id)
		}
		if !store.contains(id) {
			t.Fatalf("unlabeled %s disappeared", id)
		}
	}
	if !store.wasRemoved("sbx-managed") {
		t.Fatalf("managed orphan was not removed: %v", store.removed)
	}
}
