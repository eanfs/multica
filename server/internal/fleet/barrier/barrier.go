package barrier

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Key derives the 64-bit advisory-lock key every API/Fleet process must share
// for a node: the namespace keeps worktrees isolated, the node id keeps nodes
// independent.
func Key(namespace, nodeID string) int64 {
	sum := sha256.Sum256([]byte("fleet:" + namespace + ":" + nodeID))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// Keys derives the lock keys for nodeIDs, dropping duplicates and sorting them
// so callers can acquire a batch in one deterministic order. Batch callers must
// acquire keys from this function, not in node-id order, or concurrent claims
// can take the same keys in different orders and deadlock.
func Keys(namespace string, nodeIDs []string) []int64 {
	seen := make(map[int64]struct{}, len(nodeIDs))
	out := make([]int64, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		k := Key(namespace, id)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AcquireShared locks every key for read-only coordination. Pass keys in the
// order Keys returns (sorted) so concurrent batch claims cannot deadlock.
func AcquireShared(ctx context.Context, tx pgx.Tx, keys ...int64) error {
	for _, k := range keys {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1)", k); err != nil {
			return fmt.Errorf("acquire shared fleet lock %d: %w", k, err)
		}
	}
	return nil
}

// AcquireExclusive locks every key for maintenance coordination. It sorts a
// copy of keys before acquiring so arbitrary caller order cannot deadlock;
// pgx.Tx values are not safe for concurrent use.
func AcquireExclusive(ctx context.Context, tx pgx.Tx, keys ...int64) error {
	sorted := append([]int64(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, k := range sorted {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", k); err != nil {
			return fmt.Errorf("acquire exclusive fleet lock %d: %w", k, err)
		}
	}
	return nil
}
