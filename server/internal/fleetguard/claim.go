// Package fleetguard provides same-transaction admission for managed runtimes.
package fleetguard

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrBindingChanged requires rollback and retry of the WHOLE transaction.
var ErrBindingChanged = errors.New("fleet runtime binding changed")

func Managed(rt db.AgentRuntime) bool {
	var meta map[string]json.RawMessage
	if json.Unmarshal(rt.Metadata, &meta) != nil {
		return false
	}
	_, hasNode := meta["fleet_node_id"]
	var marker string
	_ = json.Unmarshal(meta["managed_by"], &marker)
	return hasNode || marker == "local_fleet"
}

func ResolveNamespace(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) (string, error) {
	rt, err := q.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return "", err
	}
	if !Managed(rt) {
		return "", nil
	}
	node, err := runtimeNode(ctx, q, "", rt)
	if err != nil {
		return "", err
	}
	return node.Namespace, nil
}

func runtimeNode(ctx context.Context, q *db.Queries, namespace string, rt db.AgentRuntime) (db.FleetNode, error) {
	var meta struct {
		ManagedBy string `json:"managed_by"`
		NodeID    string `json:"fleet_node_id"`
	}
	if json.Unmarshal(rt.Metadata, &meta) != nil || meta.ManagedBy != "local_fleet" {
		return db.FleetNode{}, model.ErrForbidden
	}
	id, err := util.ParseUUID(meta.NodeID)
	if err != nil || !id.Valid || !rt.OwnerID.Valid {
		return db.FleetNode{}, model.ErrForbidden
	}
	locator, err := q.GetFleetNodeIdentity(ctx, db.GetFleetNodeIdentityParams{NodeID: id, OwnerID: rt.OwnerID})
	if err != nil {
		return db.FleetNode{}, model.ErrForbidden
	}
	if namespace != "" && namespace != locator.Namespace {
		return db.FleetNode{}, model.ErrForbidden
	}
	return q.GetFleetNodeForRuntime(ctx, db.GetFleetNodeForRuntimeParams{Namespace: locator.Namespace, OwnerID: rt.OwnerID, RuntimeID: rt.ID})
}

// lockBindings discovers without row locks, acquires ALL sorted node locks,
// then ALL capacity locks, then workspace/runtime locks. Rechecks follow locks.
// q MUST be backed by the insertion/claim transaction, never an autocommit pool.
func lockBindings(ctx context.Context, q *db.Queries, namespace string, ids []pgtype.UUID, capacity bool, owners ...db.FleetLockTaskWorkspacesParams) ([]db.AgentRuntime, map[pgtype.UUID]db.FleetNode, error) {
	candidates, err := q.GetAgentRuntimes(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	nodes := map[pgtype.UUID]db.FleetNode{}
	byNode := map[pgtype.UUID]db.FleetNode{}
	for _, rt := range candidates {
		if Managed(rt) {
			n, e := runtimeNode(ctx, q, namespace, rt)
			if e != nil {
				return nil, nil, e
			}
			nodes[rt.ID] = n
			byNode[n.ID] = n
		}
	}
	ordered := make([]db.FleetNode, 0, len(byNode))
	for _, n := range byNode {
		ordered = append(ordered, n)
	}
	sort.Slice(ordered, func(i, j int) bool { return util.UUIDToString(ordered[i].ID) < util.UUIDToString(ordered[j].ID) })
	for _, n := range ordered {
		if err := q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: n.Namespace, NodeID: n.ID}); err != nil {
			return nil, nil, err
		}
	}
	if capacity {
		for _, n := range ordered {
			if err := q.FleetNodeCapacityLock(ctx, db.FleetNodeCapacityLockParams{Namespace: n.Namespace, NodeID: n.ID}); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, rt := range candidates {
		if n, ok := nodes[rt.ID]; ok {
			current, e := q.GetAgentRuntime(ctx, rt.ID)
			if e != nil || !sameBinding(rt, current) {
				return nil, nil, ErrBindingChanged
			}
			fresh, e := runtimeNode(ctx, q, n.Namespace, current)
			if e != nil {
				return nil, nil, e
			}
			if fresh.ID != n.ID {
				return nil, nil, ErrBindingChanged
			}
			nodes[rt.ID] = fresh
		}
	}
	// Ordinary admission keeps its existing workspace/agent lock behavior.
	if len(nodes) == 0 {
		return candidates, nodes, nil
	}
	for _, refs := range owners {
		if err := q.FleetLockTaskWorkspaces(ctx, refs); err != nil {
			return nil, nil, err
		}
	}
	managedIDs := make([]pgtype.UUID, 0, len(nodes))
	for id := range nodes {
		managedIDs = append(managedIDs, id)
	}
	if err := q.LockWorkspaceForRuntimeMerge(ctx, managedIDs); err != nil {
		return nil, nil, err
	}
	locked, err := q.LockRuntimesForMerge(ctx, managedIDs)
	if err != nil {
		return nil, nil, err
	}
	if len(locked) != len(nodes) {
		return nil, nil, ErrBindingChanged
	}
	old := map[pgtype.UUID]db.AgentRuntime{}
	for _, rt := range candidates {
		old[rt.ID] = rt
	}
	for _, rt := range locked {
		before, ok := old[rt.ID]
		if !ok || !sameBinding(before, rt) {
			return nil, nil, ErrBindingChanged
		}
	}
	return candidates, nodes, nil
}

func sameBinding(a, b db.AgentRuntime) bool {
	return a.ID == b.ID && a.OwnerID == b.OwnerID && a.WorkspaceID == b.WorkspaceID && string(a.Metadata) == string(b.Metadata)
}

func CheckClaim(ctx context.Context, q *db.Queries, namespace string, runtimeID pgtype.UUID, now time.Time) error {
	return checkClaims(ctx, q, namespace, []pgtype.UUID{runtimeID}, now, false)
}

// CheckReclaim gates existing dispatched slots without consuming capacity twice.
func CheckReclaim(ctx context.Context, q *db.Queries, namespace string, ids []pgtype.UUID, now time.Time) error {
	return checkClaims(ctx, q, namespace, ids, now, true)
}
func checkClaims(ctx context.Context, q *db.Queries, namespace string, ids []pgtype.UUID, now time.Time, reclaim bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var bindings []db.FleetReclaimBindingsRow
	if reclaim {
		var err error
		bindings, err = q.FleetReclaimBindings(ctx, ids)
		if err != nil {
			return err
		}
	}
	_, nodes, err := lockBindings(ctx, q, namespace, ids, true)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if err := checkNodeClaim(ctx, q, n, now, reclaim); err != nil {
			return err
		}
	}
	if reclaim {
		return lockReclaimAgents(ctx, q, bindings, nodes)
	}
	return nil
}

// The raw reclaim SQL tests bindings but does not lock agent rows itself.
// Freeze/recheck managed agent bindings before that SQL can refresh task leases.
func lockReclaimAgents(ctx context.Context, q *db.Queries, bindings []db.FleetReclaimBindingsRow, nodes map[pgtype.UUID]db.FleetNode) error {
	expected := map[pgtype.UUID]pgtype.UUID{}
	var ids []pgtype.UUID
	for _, b := range bindings {
		if _, ok := nodes[b.TaskRuntimeID]; !ok || b.BoundRuntimeID != b.TaskRuntimeID {
			continue
		}
		if _, ok := expected[b.AgentID]; !ok {
			ids = append(ids, b.AgentID)
		}
		expected[b.AgentID] = b.BoundRuntimeID
	}
	if len(ids) == 0 {
		return nil
	}
	locked, err := q.FleetLockClaimAgents(ctx, ids)
	if err != nil {
		return err
	}
	if len(locked) != len(expected) {
		return ErrBindingChanged
	}
	for _, a := range locked {
		if a.RuntimeID != expected[a.ID] {
			return ErrBindingChanged
		}
	}
	return nil
}

func checkNodeClaim(ctx context.Context, q *db.Queries, n db.FleetNode, now time.Time, reclaim bool) error {
	node := model.Node{Desired: n.Desired, Status: n.Status, Ready: n.Ready, HealthAt: n.HealthAt.Time, Revoked: n.Revoked, Maintenance: n.Maintenance}
	if !n.HealthAt.Valid || !model.CanClaim(node, now) {
		return model.ErrBusy
	}
	var resources model.Spec
	if json.Unmarshal(n.SpecConfig, &resources) != nil || resources.MaxRuns <= 0 {
		return model.ErrUnavailable
	}
	active, err := q.CountFleetActiveRuns(ctx, db.CountFleetActiveRunsParams{Namespace: n.Namespace, OwnerID: n.OwnerID, NodeID: n.ID})
	if err != nil {
		return err
	}
	if (!reclaim && active >= int64(resources.MaxRuns)) || (reclaim && active > int64(resources.MaxRuns)) {
		return model.ErrBusy
	}
	return nil
}

// ReclaimCandidates locks all nodes first but filters only blocked managed runtimes.
// An ordinary runtime in the same machine batch must retain its old redelivery behavior.
func ReclaimCandidates(ctx context.Context, q *db.Queries, ids []pgtype.UUID, now time.Time) ([]pgtype.UUID, []pgtype.UUID, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	bindings, err := q.FleetReclaimBindings(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	runtimes, nodes, err := lockBindings(ctx, q, "", ids, true)
	if err != nil {
		return nil, nil, err
	}
	var eligible, protected []pgtype.UUID
	allowed := map[pgtype.UUID]db.FleetNode{}
	for _, rt := range runtimes {
		if n, ok := nodes[rt.ID]; ok {
			if err := checkNodeClaim(ctx, q, n, now, true); err != nil {
				if errors.Is(err, model.ErrBusy) {
					continue
				}
				return nil, nil, err
			}
		}
		eligible = append(eligible, rt.ID)
		if n, ok := nodes[rt.ID]; ok {
			allowed[rt.ID] = n
			protected = append(protected, rt.ID)
		}
	}
	if err := lockReclaimAgents(ctx, q, bindings, allowed); err != nil {
		return nil, nil, err
	}
	return eligible, protected, nil
}

func CheckEnqueue(ctx context.Context, q *db.Queries, namespace string, runtimeID pgtype.UUID, now time.Time) error {
	return CheckEnqueueForOwners(ctx, q, namespace, db.FleetLockTaskWorkspacesParams{RuntimeID: runtimeID}, now)
}

// CheckEnqueueForOwners prelocks every workspace migration 284's insertion fence
// will revisit, before acquiring runtime and agent/issue locks.
func CheckEnqueueForOwners(ctx context.Context, q *db.Queries, namespace string, refs db.FleetLockTaskWorkspacesParams, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, nodes, err := lockBindings(ctx, q, namespace, []pgtype.UUID{refs.RuntimeID}, true, refs)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if !model.CanEnqueue(model.Node{Desired: n.Desired, Revoked: n.Revoked}) || n.Status == "terminated" || n.Status == "terminating" {
			return model.ErrBusy
		}
	}
	return nil
}

// CheckCallerEnqueue never discovers/acquires a new node behind caller-owned
// workspace/chat locks. A changed managed binding retries the whole owning tx.
func CheckCallerEnqueue(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID, now time.Time) error {
	if !runtimeID.Valid {
		return nil
	}
	rt, err := q.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return err
	}
	if !Managed(rt) {
		return nil
	}
	n, err := runtimeNode(ctx, q, "", rt)
	if err != nil {
		return err
	}
	held, err := q.FleetAdmissionLocksHeld(ctx, db.FleetAdmissionLocksHeldParams{Namespace: n.Namespace, NodeID: n.ID})
	if err != nil {
		return err
	}
	if !held {
		return ErrBindingChanged
	}
	return CheckEnqueue(ctx, q, n.Namespace, runtimeID, now)
}

func CheckRegister(ctx context.Context, q *db.Queries, namespace string, nodeID pgtype.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: namespace, NodeID: nodeID}); err != nil {
		return err
	}
	n, err := q.GetFleetNodeByID(ctx, db.GetFleetNodeByIDParams{Namespace: namespace, NodeID: nodeID})
	if err != nil {
		return model.ErrForbidden
	}
	if !model.CanEnqueue(model.Node{Desired: n.Desired, Revoked: n.Revoked}) || n.Status == "terminated" || n.Status == "terminating" {
		return model.ErrBusy
	}
	return nil
}

func CheckRuntimeMerge(ctx context.Context, q *db.Queries, namespace string, sourceID, targetID pgtype.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ids := []pgtype.UUID{sourceID, targetID}
	candidates, _, err := lockBindings(ctx, q, namespace, ids, false)
	if err != nil {
		return err
	}
	// Even an ordinary candidate can be rebound while waiting for its row lock.
	if err := q.LockWorkspaceForRuntimeMerge(ctx, ids); err != nil {
		return err
	}
	locked, err := q.LockRuntimesForMerge(ctx, ids)
	if err != nil {
		return err
	}
	if len(locked) != 2 || len(candidates) != 2 {
		return ErrBindingChanged
	}
	old := map[pgtype.UUID]db.AgentRuntime{}
	for _, rt := range candidates {
		old[rt.ID] = rt
	}
	for _, rt := range locked {
		if !sameBinding(old[rt.ID], rt) {
			return ErrBindingChanged
		}
		if Managed(rt) {
			return model.ErrForbidden
		}
	}
	return nil
}
