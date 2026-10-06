package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type NamespaceCompletion struct {
	Node model.Node
	Ref  model.OperationRef
	Key  string
}

// CheckNamespaceCompletions is the whole-namespace linearization point. Its
// exclusive prefix prevents new maintenance intents during this SHORT SQL-only
// proof; all sorted node locks precede all capacity locks. Failure preserves data.
func (s *Store) CheckNamespaceCompletions(ctx context.Context, fence NamespaceFence, action model.Action, expected []NamespaceCompletion) error {
	if !fence.Closed || fence.Generation < 1 || fence.Namespace != s.namespace || (action != model.Stop && action != model.Delete) {
		return model.ErrConflict
	}
	for i, c := range expected {
		if !s.validCompletion(c) || c.Ref.Action != action || (i > 0 && bytes.Compare(expected[i-1].Node.ID.Bytes[:], c.Node.ID.Bytes[:]) >= 0) {
			return model.ErrConflict
		}
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return s.WithTx(ctx, func(q *db.Queries) error {
		if err := q.FleetNamespaceExclusiveLock(ctx, s.namespace); err != nil {
			return err
		}
		current, err := readNamespaceFence(ctx, q, s.namespace)
		if err != nil {
			return err
		}
		if current != fence {
			return model.ErrConflict
		}
		if fence.Finalized {
			original, originalAction, e := s.readCompletionManifest(ctx, q, fence)
			if e != nil {
				return e
			}
			if originalAction != action {
				return model.ErrConflict
			}
			expected = original
		}
		if err := s.checkNamespaceProof(ctx, q, expected); err != nil {
			return err
		}
		if !fence.Finalized {
			raw, e := encodeCompletionManifest(fence, action, expected)
			if e != nil {
				return e
			}
			changed, e := q.FinalizeFleetNamespaceFence(ctx, db.FinalizeFleetNamespaceFenceParams{CompletionManifest: raw, Namespace: s.namespace, FleetID: fence.FleetID, OperationKey: fence.OperationKey, Generation: fence.Generation})
			if e != nil {
				return e
			}
			if changed != 1 {
				return model.ErrConflict
			}
		}
		return nil
	})
}

func (s *Store) checkNamespaceProof(ctx context.Context, q *db.Queries, expected []NamespaceCompletion) error {
	after := pgtype.UUID{Valid: true}
	seen := 0
	for {
		rows, err := q.ListFleetNamespaceNodes(ctx, db.ListFleetNamespaceNodesParams{Namespace: s.namespace, AfterID: after, PageLimit: 100})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, n := range rows {
			if seen >= len(expected) || n.ID != expected[seen].Node.ID || n.OwnerID != expected[seen].Node.OwnerID {
				return model.ErrUnknownHealth
			}
			after = n.ID
			seen++
		}
	}
	if seen != len(expected) {
		return model.ErrUnknownHealth
	}
	for _, c := range expected {
		if err := q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: s.namespace, NodeID: c.Node.ID}); err != nil {
			return err
		}
	}
	for _, c := range expected {
		if err := q.FleetNodeCapacityLock(ctx, db.FleetNodeCapacityLockParams{Namespace: s.namespace, NodeID: c.Node.ID}); err != nil {
			return err
		}
	}
	for _, c := range expected {
		if err := s.checkNamespaceCompletion(ctx, q, c); err != nil {
			return err
		}
	}

	return nil
}

// The persisted schema is a whitelist: never serialize a model.Node or its observations/errors.
type completionBaseline struct {
	ID, OwnerID                                                             pgtype.UUID
	Namespace, ContainerID, DaemonID, StartEpoch, DataVolume, SecretsVolume string
	Image, ProfileRef, Spec, Name                                           string
	Resources                                                               model.Spec
	CreatedAt                                                               time.Time
	Generation                                                              int64
}
type manifestCompletion struct {
	Baseline completionBaseline
	Ref      model.OperationRef
	Key      string
}
type completionManifest struct {
	Version                          int
	Namespace, FleetID, OperationKey string
	Generation                       int64
	Action                           model.Action
	Completions                      []manifestCompletion
}

func baseline(n model.Node) completionBaseline {
	return completionBaseline{ID: n.ID, OwnerID: n.OwnerID, Namespace: n.Namespace, ContainerID: n.ContainerID, DaemonID: n.DaemonID, StartEpoch: n.StartEpoch, DataVolume: n.DataVolume, SecretsVolume: n.SecretsVolume, Image: n.Image, ProfileRef: n.ProfileRef, Spec: n.Spec, Name: n.Name, Resources: n.Resources, CreatedAt: n.CreatedAt, Generation: n.Generation}
}
func (b completionBaseline) node() model.Node {
	return model.Node{ID: b.ID, OwnerID: b.OwnerID, Namespace: b.Namespace, ContainerID: b.ContainerID, DaemonID: b.DaemonID, StartEpoch: b.StartEpoch, DataVolume: b.DataVolume, SecretsVolume: b.SecretsVolume, Image: b.Image, ProfileRef: b.ProfileRef, Spec: b.Spec, Name: b.Name, Resources: b.Resources, CreatedAt: b.CreatedAt, Generation: b.Generation}
}
func encodeCompletionManifest(f NamespaceFence, action model.Action, cs []NamespaceCompletion) ([]byte, error) {
	m := completionManifest{Version: 1, Namespace: f.Namespace, FleetID: f.FleetID, OperationKey: f.OperationKey, Generation: f.Generation, Action: action, Completions: make([]manifestCompletion, 0, len(cs))}
	for _, c := range cs {
		m.Completions = append(m.Completions, manifestCompletion{Baseline: baseline(c.Node), Ref: c.Ref, Key: c.Key})
	}
	return json.Marshal(m)
}
func (s *Store) readCompletionManifest(ctx context.Context, q *db.Queries, f NamespaceFence) ([]NamespaceCompletion, model.Action, error) {
	row, e := q.GetFleetNamespaceFence(ctx, s.namespace)
	if e != nil {
		return nil, "", e
	}
	if fenceFromRow(row) != f || !f.Finalized {
		return nil, "", model.ErrConflict
	}
	if len(row.CompletionManifest) == 0 {
		return nil, "", model.ErrUnknownHealth
	}
	var m completionManifest
	dec := json.NewDecoder(bytes.NewReader(row.CompletionManifest))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil || dec.Decode(new(any)) != io.EOF || m.Version != 1 || m.Namespace != f.Namespace || m.FleetID != f.FleetID || m.OperationKey != f.OperationKey || m.Generation != f.Generation || m.Completions == nil || (m.Action != model.Stop && m.Action != model.Delete) {
		return nil, "", model.ErrUnknownHealth
	}
	cs := make([]NamespaceCompletion, 0, len(m.Completions))
	for i, c := range m.Completions {
		item := NamespaceCompletion{Node: c.Baseline.node(), Ref: c.Ref, Key: c.Key}
		if !s.validCompletion(item) || item.Ref.Action != m.Action || (i > 0 && bytes.Compare(cs[i-1].Node.ID.Bytes[:], item.Node.ID.Bytes[:]) >= 0) {
			return nil, "", model.ErrUnknownHealth
		}
		cs = append(cs, item)
	}
	return cs, m.Action, nil
}

// GetNamespaceCompletions reads only the durable original cycle, never current nodes.
func (s *Store) GetNamespaceCompletions(ctx context.Context, f NamespaceFence, action model.Action) (cs []NamespaceCompletion, found bool, err error) {
	if f.Namespace != s.namespace || !f.Closed || !f.Finalized || (action != model.Stop && action != model.Delete) {
		return nil, false, model.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	err = s.WithTx(ctx, func(q *db.Queries) error {
		if e := q.FleetNamespaceSharedLock(ctx, s.namespace); e != nil {
			return e
		}
		original, a, e := s.readCompletionManifest(ctx, q, f)
		if e != nil {
			return e
		}
		if a == action {
			cs = original
			found = true
		}
		return nil
	})
	return
}

// BeginNamespaceDestroy opens only negative work in a new trusted cycle.
// It never opens positive admission or renews an existing node action.
func (s *Store) BeginNamespaceDestroy(ctx context.Context, expected NamespaceFence) (next NamespaceFence, err error) {
	if expected.Namespace != s.namespace || !expected.Closed || !expected.Finalized || expected.Generation < 1 || expected.Generation == math.MaxInt64 || !validFenceText(expected.FleetID) || !validFenceText(expected.OperationKey) {
		return next, model.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	err = s.WithTx(ctx, func(q *db.Queries) error {
		if e := q.FleetNamespaceExclusiveLock(ctx, s.namespace); e != nil {
			return e
		}
		current, e := readNamespaceFence(ctx, q, s.namespace)
		if e != nil {
			return e
		}
		if current != expected {
			return model.ErrConflict
		}
		original, _, e := s.readCompletionManifest(ctx, q, expected)
		if e != nil {
			return e
		}
		if e = s.checkNamespaceProof(ctx, q, original); e != nil {
			return e
		}
		row, e := q.BeginFleetNamespaceDestroy(ctx, db.BeginFleetNamespaceDestroyParams{Namespace: s.namespace, FleetID: expected.FleetID, OperationKey: expected.OperationKey, Generation: expected.Generation})
		if e != nil {
			return e
		}
		next = fenceFromRow(row)
		return nil
	})
	return
}

// LookupNamespaceOperation reads a same-cycle receipt without creating an intent
// or invoking diagnosis. Missing is distinct from unknown or corrupt.
func (s *Store) LookupNamespaceOperation(ctx context.Context, n model.Node, action model.Action, key string) (op model.Operation, found bool, err error) {
	if n.Namespace != s.namespace || !validOwner(n.ID) || !validOwner(n.OwnerID) || (action != model.Stop && action != model.Delete) || !validFenceText(key) {
		return op, false, model.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	err = s.WithTx(ctx, func(q *db.Queries) error {
		if e := q.FleetNamespaceSharedLock(ctx, s.namespace); e != nil {
			return e
		}
		if _, e := readNamespaceFence(ctx, q, s.namespace); e != nil {
			return e
		}
		row, e := q.GetFleetIntentByKey(ctx, db.GetFleetIntentByKeyParams{Namespace: s.namespace, OwnerID: n.OwnerID, IdempotencyKey: key})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		op = operationFromRow(row)
		if op.NodeID != n.ID || op.OwnerID != n.OwnerID || op.Generation != n.Generation || op.Action != action || op.RequestHash != lifecycleFingerprint(n.ID, action) {
			return model.ErrConflict
		}
		found = true
		return nil
	})
	return
}

type NamespaceFence struct {
	Namespace, FleetID, OperationKey string
	Generation                       int64
	Closed, Finalized                bool
}

func validFenceText(v string) bool {
	return v != "" && len(v) <= 128 && strings.TrimSpace(v) == v && strings.IndexFunc(v, unicode.IsControl) < 0
}
func fenceFromRow(r db.FleetNamespaceFence) NamespaceFence {
	return NamespaceFence{Namespace: r.Namespace, FleetID: r.FleetID, OperationKey: r.OperationKey, Generation: r.Generation, Closed: r.Closed, Finalized: r.Finalized}
}
func readNamespaceFence(ctx context.Context, q *db.Queries, namespace string) (NamespaceFence, error) {
	if !validFenceText(namespace) {
		return NamespaceFence{}, model.ErrInvalidRequest
	}
	row, err := q.GetFleetNamespaceFence(ctx, namespace)
	if errors.Is(err, pgx.ErrNoRows) {
		return NamespaceFence{Namespace: namespace}, nil
	}
	if err != nil || row.Namespace != namespace || !validFenceText(row.FleetID) || !validFenceText(row.OperationKey) || row.Generation < 1 || row.Finalized && !row.Closed {
		return NamespaceFence{}, model.ErrUnavailable
	}
	return fenceFromRow(row), nil
}

// CheckNamespaceAdmission MUST use the producer's transaction before any old
// node/capacity/workspace/runtime locks. An autocommit query cannot hold admission.
func CheckNamespaceAdmission(ctx context.Context, q *db.Queries, namespace string) error {
	if q == nil || !validFenceText(namespace) {
		return model.ErrUnavailable
	}
	if err := q.FleetNamespaceSharedLock(ctx, namespace); err != nil {
		return model.ErrUnavailable
	}
	fence, err := readNamespaceFence(ctx, q, namespace)
	if err != nil {
		return err
	}
	if fence.Closed {
		return model.ErrBusy
	}
	return nil
}
func (s *Store) GetNamespaceFence(ctx context.Context) (NamespaceFence, error) {
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return readNamespaceFence(ctx, db.New(s.pool), s.namespace)
}
func (s *Store) CloseNamespace(ctx context.Context, fleetID, key string) (NamespaceFence, error) {
	if !validFenceText(s.namespace) || !validFenceText(fleetID) || !validFenceText(key) {
		return NamespaceFence{}, model.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	var fence NamespaceFence
	err := s.WithTx(ctx, func(q *db.Queries) error {
		if err := q.FleetNamespaceExclusiveLock(ctx, s.namespace); err != nil {
			return err
		}
		old, err := readNamespaceFence(ctx, q, s.namespace)
		if err != nil {
			return err
		}
		var row db.FleetNamespaceFence
		if old.Generation == 0 {
			row, err = q.InsertFleetNamespaceFence(ctx, db.InsertFleetNamespaceFenceParams{Namespace: s.namespace, FleetID: fleetID, OperationKey: key})
		} else {
			if old.FleetID != fleetID {
				return model.ErrConflict
			}
			if old.Closed {
				if old.OperationKey != key {
					return model.ErrConflict
				}
				fence = old
				return nil
			}
			if old.OperationKey == key || old.Generation == math.MaxInt64 {
				return model.ErrConflict
			}
			row, err = q.CloseFleetNamespaceFence(ctx, db.CloseFleetNamespaceFenceParams{Namespace: s.namespace, FleetID: fleetID, OperationKey: key, Generation: old.Generation})
		}
		if err != nil {
			return err
		}
		fence = fenceFromRow(row)
		return nil
	})
	return fence, err
}
func (s *Store) OpenNamespace(ctx context.Context, expected NamespaceFence) error {
	if expected.Namespace != s.namespace || !expected.Closed || expected.Generation < 1 || !validFenceText(expected.FleetID) || !validFenceText(expected.OperationKey) {
		return model.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return s.WithTx(ctx, func(q *db.Queries) error {
		if err := q.FleetNamespaceExclusiveLock(ctx, s.namespace); err != nil {
			return err
		}
		old, err := readNamespaceFence(ctx, q, s.namespace)
		if err != nil {
			return err
		}
		if old != expected {
			return model.ErrConflict
		}
		n, err := q.OpenFleetNamespaceFence(ctx, db.OpenFleetNamespaceFenceParams{Namespace: s.namespace, FleetID: expected.FleetID, OperationKey: expected.OperationKey, Generation: expected.Generation})
		if err != nil {
			return err
		}
		if n != 1 {
			return model.ErrConflict
		}
		return nil
	})
}

// CheckNamespaceCompletion reads the ORIGINAL worker receipt under the closed
// namespace and node/capacity fences. It never remints physical authority.
func (s *Store) validCompletion(c NamespaceCompletion) bool {
	return c.Node.Namespace == s.namespace && c.Ref.Namespace == s.namespace && c.Node.ID == c.Ref.NodeID && validOwner(c.Node.OwnerID) && validOwner(c.Node.ID) && validOwner(c.Ref.OperationID) && c.Ref.Generation > 0 && c.Node.Generation > 0 && (c.Ref.Action == model.Stop || c.Ref.Action == model.Delete) && validFenceText(c.Key)
}
func (s *Store) CheckNamespaceCompletion(ctx context.Context, fence NamespaceFence, expected model.Node, ref model.OperationRef, key string) error {
	c := NamespaceCompletion{Node: expected, Ref: ref, Key: key}
	if !fence.Closed || fence.Generation < 1 || fence.Namespace != s.namespace || !s.validCompletion(c) {
		return model.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return s.WithTx(ctx, func(q *db.Queries) error {
		if err := q.FleetNamespaceSharedLock(ctx, s.namespace); err != nil {
			return err
		}
		current, err := readNamespaceFence(ctx, q, s.namespace)
		if err != nil {
			return err
		}
		if current != fence {
			return model.ErrConflict
		}
		if err = q.FleetNodeSharedLock(ctx, db.FleetNodeSharedLockParams{Namespace: s.namespace, NodeID: ref.NodeID}); err != nil {
			return err
		}
		if err = q.FleetNodeCapacityLock(ctx, db.FleetNodeCapacityLockParams{Namespace: s.namespace, NodeID: ref.NodeID}); err != nil {
			return err
		}
		return s.checkNamespaceCompletion(ctx, q, c)
	})
}
func (s *Store) checkNamespaceCompletion(ctx context.Context, q *db.Queries, c NamespaceCompletion) error {
	expected, ref, key := c.Node, c.Ref, c.Key
	owner := expected.OwnerID
	raw, err := q.GetFleetNode(ctx, db.GetFleetNodeParams{Namespace: s.namespace, OwnerID: owner, NodeID: ref.NodeID})
	if err != nil {
		return model.ErrUnknownHealth
	}
	n, err := nodeFromRow(raw)
	if err != nil {
		return err
	}
	if expected.ContainerID != n.ContainerID || expected.DaemonID != n.DaemonID || expected.StartEpoch != n.StartEpoch || expected.DataVolume != n.DataVolume || expected.SecretsVolume != n.SecretsVolume || expected.Image != n.Image || expected.ProfileRef != n.ProfileRef || expected.Resources != n.Resources || expected.Spec != n.Spec || expected.Name != n.Name || !expected.CreatedAt.Equal(n.CreatedAt) || (expected.Generation != ref.Generation && expected.Generation+1 != ref.Generation) {
		return model.ErrConflict
	}
	row, err := q.GetFleetOperation(ctx, db.GetFleetOperationParams{Namespace: s.namespace, OwnerID: owner, OperationID: ref.OperationID})
	if err != nil {
		return model.ErrUnknownHealth
	}
	op := operationFromRow(row)
	if op.NodeID != n.ID || op.OwnerID != owner || op.Generation != ref.Generation || n.Generation != ref.Generation || op.Action != ref.Action || op.IdempotencyKey != key || op.RequestHash != lifecycleFingerprint(n.ID, ref.Action) {
		return model.ErrConflict
	}
	if !op.Approved || op.NonRetryable || op.Phase == "failed" {
		return model.ErrUnknownHealth
	}
	if op.Phase != "completed" {
		return model.ErrBusy
	}
	if n.ActiveRuns != 0 || n.PendingReports != 0 || n.FailedReports != 0 {
		return model.ErrBusy
	}
	if err = s.idleSQL(ctx, q, n, ref.Action); err != nil {
		return err
	}
	if ref.Action == model.Delete {
		if !n.Revoked || !n.Maintenance || n.Status != "terminated" || n.Desired != "terminated" || n.DataVolume == "" || n.SecretsVolume == "" {
			return model.ErrUnknownHealth
		}
		return nil
	}
	now, err := q.FleetRecoveryClock(ctx)
	if err != nil || !now.Valid {
		return model.ErrUnavailable
	}
	o := n.Observation
	if n.Revoked || n.Maintenance || n.Ready || n.Desired != "stopped" || (n.Status != "stopped" && n.Status != "missing") || o.Status != n.Status || o.Ready || o.Offline || op.ActionClaimedAt.IsZero() || o.ObservedAt.Before(op.ActionClaimedAt) || o.ObservedAt.After(now.Time) || !o.ObservedAt.Equal(n.HealthAt) || o.ActiveRuns != 0 || o.PendingReports != 0 || o.FailedReports != 0 {
		return model.ErrUnknownHealth
	}
	if n.Status == "stopped" && (n.ContainerID == "" || o.ContainerID != n.ContainerID) {
		return model.ErrUnknownHealth
	}
	if n.Status == "missing" && o.ContainerID != "" {
		return model.ErrUnknownHealth
	}
	return nil
}

func (s *Store) ListNamespaceNodes(ctx context.Context, after pgtype.UUID, limit int32) ([]model.Node, error) {
	if limit < 1 || limit > 100 {
		return nil, model.ErrInvalidRequest
	}
	if !after.Valid {
		after = pgtype.UUID{Valid: true}
	}
	ctx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	rows, err := db.New(s.pool).ListFleetNamespaceNodes(ctx, db.ListFleetNamespaceNodesParams{Namespace: s.namespace, AfterID: after, PageLimit: limit})
	if err != nil {
		return nil, err
	}
	nodes := make([]model.Node, 0, len(rows))
	for _, row := range rows {
		n, err := nodeFromRow(row)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}
