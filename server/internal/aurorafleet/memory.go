package aurorafleet

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"sync"
)

// MemoryBackend is an in-process Backend used by tests and as a zero-dependency
// default. Nodes live in a map; lifecycle transitions mutate a node's Status in
// place. It only fails when a caller names an unknown node, which keeps
// controller tests deterministic without a Docker daemon.
type MemoryBackend struct {
	mu    sync.Mutex
	nodes map[string]*Node
	env   map[string]map[string]string
	seq   int
}

// NewMemoryBackend returns an empty MemoryBackend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{
		nodes: make(map[string]*Node),
		env:   make(map[string]map[string]string),
	}
}

func (m *MemoryBackend) Create(_ context.Context, req CreateRequest) (Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	id := fmt.Sprintf("node-%d", m.seq)
	created := now()
	n := &Node{
		ID:        id,
		Name:      req.Name,
		Image:     req.Image,
		Status:    StatusRunning,
		Labels:    maps.Clone(req.Labels),
		CreatedAt: created,
		UpdatedAt: created,
	}
	m.nodes[id] = n
	m.env[id] = maps.Clone(req.Env)
	return *n, nil
}

func (m *MemoryBackend) Terminate(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[id]; !ok {
		return ErrNotFound
	}
	delete(m.nodes, id)
	delete(m.env, id)
	return nil
}

func (m *MemoryBackend) Start(_ context.Context, id string) error {
	return m.transition(id, StatusStopped, StatusRunning)
}

func (m *MemoryBackend) Stop(_ context.Context, id string) error {
	return m.transition(id, StatusRunning, StatusStopped)
}

func (m *MemoryBackend) Reboot(_ context.Context, id string) error {
	return m.transition(id, StatusRunning, StatusRebooting)
}

func (m *MemoryBackend) transition(id string, from, to Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return ErrNotFound
	}
	if n.Status != from {
		return fmt.Errorf("node %s: cannot move %s -> %s from %s", id, from, to, n.Status)
	}
	n.Status = to
	n.UpdatedAt = now()
	return nil
}

func (m *MemoryBackend) Status(_ context.Context, id string) (Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	return *n, nil
}

func (m *MemoryBackend) Exec(_ context.Context, id string, _ []string) (ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[id]; !ok {
		return ExecResult{}, ErrNotFound
	}
	// An in-memory node runs nothing, so exec is a no-op that proves the
	// command reached the node.
	return ExecResult{ExitCode: 0}, nil
}

func (m *MemoryBackend) List(_ context.Context) ([]Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryBackend) Ping(context.Context) error { return nil }

// NodeEnv returns the environment the controller injected into node id. It is
// the hook tests use to assert the fleet secret reached the node; production
// backends keep env out of the API surface.
func (m *MemoryBackend) NodeEnv(id string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.env[id])
}
