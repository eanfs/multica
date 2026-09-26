package aurorafleet

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

const (
	// ManagedLabel marks every Docker resource the fleet owns. Reconciliation
	// only ever lists resources carrying it, which is what keeps it away from
	// user containers and networks.
	ManagedLabel = "com.multica.aurora.managed"
	// ManagedLabelValue is the only value the fleet writes for ManagedLabel.
	ManagedLabelValue = "true"
	// NodeLabel carries the workspace node identity on a managed resource.
	NodeLabel = "com.multica.aurora.node_id"
	// KindLabel classifies a managed resource.
	KindLabel = "com.multica.aurora.kind"

	// KindSandbox is the node's execution container.
	KindSandbox = "sandbox"
	// KindProxy is the node's egress sidecar.
	KindProxy = "proxy"
	// KindNetwork is the node's workspace-internal network.
	KindNetwork = "network"
)

// reconcileGrace is how long a complete but unrecognized node survives startup
// reconciliation, so a node the server is still recording is never destroyed.
const reconcileGrace = 10 * time.Minute

// ManagedResource is one Docker resource the fleet owns.
type ManagedResource struct {
	ID        string
	Kind      string
	NodeID    string
	CreatedAt time.Time
}

// ManagedResourceStore lists and removes fleet-owned Docker resources. An
// implementation must only return resources carrying ManagedLabel: the
// label filter is the boundary that keeps reconciliation away from everything
// the fleet did not create.
type ManagedResourceStore interface {
	ListManaged(ctx context.Context) ([]ManagedResource, error)
	Remove(ctx context.Context, resource ManagedResource) error
}

// DesiredNodeState reports the node IDs the server still owns. A nil source
// means the fleet has no server view yet, in which case reconciliation only
// removes partial resources and never a complete node.
type DesiredNodeState interface {
	DesiredNodeIDs(ctx context.Context) (map[string]struct{}, error)
}

// ReconcileStats reports one reconciliation pass.
type ReconcileStats struct {
	Examined int
	Removed  int
}

// Reconciler removes fleet-owned Docker resources the server no longer expects:
// partial networks and sidecars whose sandbox is gone, and complete nodes that
// stayed unrecognized past the startup grace.
type Reconciler struct {
	store   ManagedResourceStore
	desired DesiredNodeState
	grace   time.Duration
	now     func() time.Time
	logger  *slog.Logger
}

// NewReconciler wires a reconciler. grace bounds how long a complete but
// unrecognized node survives; now defaults to time.Now.
func NewReconciler(store ManagedResourceStore, desired DesiredNodeState, grace time.Duration, now func() time.Time, logger *slog.Logger) *Reconciler {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{store: store, desired: desired, grace: grace, now: now, logger: logger}
}

// Reconcile applies one pass. Partial nodes are removed immediately because
// nothing can be running without a sandbox; complete nodes wait out the grace
// so a node the server is still bringing up is never destroyed.
func (r *Reconciler) Reconcile(ctx context.Context) (ReconcileStats, error) {
	managed, err := r.store.ListManaged(ctx)
	if err != nil {
		return ReconcileStats{}, fmt.Errorf("list managed fleet resources: %w", err)
	}
	if len(managed) == 0 {
		return ReconcileStats{}, nil
	}

	var desired map[string]struct{}
	if r.desired != nil {
		desired, err = r.desired.DesiredNodeIDs(ctx)
		if err != nil {
			return ReconcileStats{}, fmt.Errorf("read desired node state: %w", err)
		}
	}

	stats := ReconcileStats{Examined: len(managed)}
	groups := groupManagedByNode(managed)
	for _, nodeID := range sortedNodeIDs(groups) {
		group := groups[nodeID]
		if _, ok := desired[nodeID]; ok {
			continue
		}
		if !groupHasSandbox(group) {
			// Partial failure: the sandbox never came up, so its sidecar and
			// network are leftovers regardless of age.
			if err := r.removeAll(ctx, group, &stats); err != nil {
				return stats, err
			}
			continue
		}
		// A complete node the server does not recognize survives the grace
		// window in case the server is still recording it. An unknown age is
		// never proof of staleness, so it is kept too.
		oldest := oldestCreated(group)
		if r.desired == nil || oldest.IsZero() || r.now().Sub(oldest) < r.grace {
			continue
		}
		if err := r.removeAll(ctx, group, &stats); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (r *Reconciler) removeAll(ctx context.Context, group []ManagedResource, stats *ReconcileStats) error {
	for _, resource := range group {
		if err := r.store.Remove(ctx, resource); err != nil {
			return fmt.Errorf("remove managed %s %s: %w", resource.Kind, resource.ID, err)
		}
		stats.Removed++
		r.logger.Info("reconciled managed fleet resource", "kind", resource.Kind, "node_id", resource.NodeID)
	}
	return nil
}

func groupManagedByNode(managed []ManagedResource) map[string][]ManagedResource {
	groups := make(map[string][]ManagedResource, len(managed))
	for _, resource := range managed {
		groups[resource.NodeID] = append(groups[resource.NodeID], resource)
	}
	return groups
}

func sortedNodeIDs(groups map[string][]ManagedResource) []string {
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func groupHasSandbox(group []ManagedResource) bool {
	for _, resource := range group {
		if resource.Kind == KindSandbox {
			return true
		}
	}
	return false
}

func oldestCreated(group []ManagedResource) time.Time {
	oldest := time.Time{}
	for _, resource := range group {
		if resource.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || resource.CreatedAt.Before(oldest) {
			oldest = resource.CreatedAt
		}
	}
	return oldest
}

// DockerResourceStore lists and removes the fleet's own Docker resources. Both
// listings are label-filtered, so the store can only ever observe resources the
// fleet created.
type DockerResourceStore struct {
	runner func(ctx context.Context, args ...string) (string, error)
}

// NewDockerResourceStore wires the store to a docker CLI runner.
func NewDockerResourceStore(runner func(ctx context.Context, args ...string) (string, error)) *DockerResourceStore {
	return &DockerResourceStore{runner: runner}
}

// managedListFormat asks docker for the identity, node, kind, and creation time
// of every managed resource in one tab-separated row.
const managedListFormat = "{{.ID}}\t{{.Label \"" + NodeLabel + "\"}}\t{{.Label \"" + KindLabel + "\"}}\t{{.CreatedAt}}"

// ListManaged returns the containers and networks carrying the managed label.
func (s *DockerResourceStore) ListManaged(ctx context.Context) ([]ManagedResource, error) {
	out, err := s.runner(ctx, "ps", "-a",
		"--filter", "label="+ManagedLabel+"="+ManagedLabelValue,
		"--format", managedListFormat)
	if err != nil {
		return nil, fmt.Errorf("list managed containers: %w", err)
	}
	containers, err := parseManagedList(out)
	if err != nil {
		return nil, err
	}

	netOut, err := s.runner(ctx, "network", "ls",
		"--filter", "label="+ManagedLabel+"="+ManagedLabelValue,
		"--format", managedListFormat)
	if err != nil {
		return nil, fmt.Errorf("list managed networks: %w", err)
	}
	networks, err := parseManagedList(netOut)
	if err != nil {
		return nil, err
	}
	return append(containers, networks...), nil
}

// Remove deletes one managed resource.
func (s *DockerResourceStore) Remove(ctx context.Context, resource ManagedResource) error {
	args := []string{"rm", "-f", resource.ID}
	if resource.Kind == KindNetwork {
		args = []string{"network", "rm", resource.ID}
	}
	_, err := s.runner(ctx, args...)
	return err
}

func parseManagedList(out string) ([]ManagedResource, error) {
	resources := []ManagedResource{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			return nil, fmt.Errorf("unexpected managed resource row %q", line)
		}
		resource := ManagedResource{ID: fields[0], NodeID: fields[1], Kind: fields[2]}
		if created, err := time.Parse("2006-01-02 15:04:05 -0700 MST", strings.TrimSpace(fields[3])); err == nil {
			resource.CreatedAt = created
		}
		resources = append(resources, resource)
	}
	return resources, nil
}
