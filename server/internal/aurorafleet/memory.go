package aurorafleet

import (
	"context"
	"sync"
)

// MemoryBackend is an in-process Backend used by tests and as a
// zero-dependency default. Nodes live in a map keyed by node ID and are
// created online; it only fails when a caller names an unknown node, which
// keeps controller tests deterministic without a Docker daemon.
type MemoryBackend struct {
	mu       sync.Mutex
	nodes    map[string]Node
	lastSpec WorkspaceNodeSpec
}

// NewMemoryBackend returns an empty MemoryBackend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{nodes: make(map[string]Node)}
}

// EnsureWorkspaceNode records the spec and returns the node online.
func (m *MemoryBackend) EnsureWorkspaceNode(_ context.Context, spec WorkspaceNodeSpec) (Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSpec = spec
	n := Node{ID: spec.NodeID, State: StateOnline, Health: HealthHealthy}
	m.nodes[spec.NodeID] = n
	return n, nil
}

// WorkspaceNodeStatus returns the stored node state.
func (m *MemoryBackend) WorkspaceNodeStatus(_ context.Context, nodeID string) (Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[nodeID]
	if !ok {
		return Node{}, ErrNotFound
	}
	return n, nil
}

// DeleteWorkspaceNode forgets the node.
func (m *MemoryBackend) DeleteWorkspaceNode(_ context.Context, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[nodeID]; !ok {
		return ErrNotFound
	}
	delete(m.nodes, nodeID)
	return nil
}

// Reconcile is a no-op for the in-memory backend.
func (m *MemoryBackend) Reconcile(context.Context) error { return nil }

// Ping reports the backend usable.
func (m *MemoryBackend) Ping(context.Context) error { return nil }

// LastSpec returns the most recent spec passed to EnsureWorkspaceNode. It is
// the hook tests use to assert what the controller handed the backend,
// including the staged enrollment file path.
func (m *MemoryBackend) LastSpec() WorkspaceNodeSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSpec
}
