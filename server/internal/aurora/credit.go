package aurora

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrInsufficientCredits = errors.New("insufficient credits")

// Ledger kinds follow the cloud wallet contract
// (packages/core/types/billing.ts): topup | deduction | refund | adjustment.
// "expire" is added by Plan 5 (LedgerKindExpire + Expire, monthly
// settlement).
const (
	LedgerKindTopup      = "topup"
	LedgerKindDeduction  = "deduction"
	LedgerKindRefund     = "refund"
	LedgerKindAdjustment = "adjustment"
)

// TxBeginner is the narrow transaction-starting surface CreditService needs.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CreditService owns Aurora credit accounting. Every write is idempotent via a
// derived idempotency_key: the pre-check short-circuits retries before any
// balance change, and a concurrent duplicate that loses the ledger-insert race
// rolls back and returns nil (the committed transaction already applied it).
type CreditService struct {
	queries *db.Queries
	tx      TxBeginner
}

func NewCreditService(q *db.Queries, tx TxBeginner) *CreditService {
	return &CreditService{queries: q, tx: tx}
}

func (s *CreditService) Balance(ctx context.Context, userID pgtype.UUID) (int64, error) {
	bal, err := s.queries.GetCreditBalance(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return bal, err
}

// Reserve deducts amountMicro and records a "deduction" ledger row, keyed by
// "reserve:"+reference (the generation id) so retries are safe.
func (s *CreditService) Reserve(ctx context.Context, userID, workspaceID pgtype.UUID, amountMicro int64, reference string) error {
	if amountMicro <= 0 {
		return fmt.Errorf("reserve amount must be positive, got %d", amountMicro)
	}
	return s.adjust(ctx, userID, workspaceID, -amountMicro, LedgerKindDeduction, "reserve:"+reference, reference)
}

// Refund credits amountMicro back and records a "refund" ledger row, keyed by
// "refund:"+reference.
func (s *CreditService) Refund(ctx context.Context, userID, workspaceID pgtype.UUID, amountMicro int64, reference string) error {
	if amountMicro <= 0 {
		return fmt.Errorf("refund amount must be positive, got %d", amountMicro)
	}
	return s.adjust(ctx, userID, workspaceID, amountMicro, LedgerKindRefund, "refund:"+reference, reference)
}

// Grant adds amountMicro and records a ledger row of the given kind. Monthly
// quota grants use LedgerKindAdjustment; Stripe purchases use LedgerKindTopup
// (the Plan 5 webhook calls this). Keyed by "grant:"+reference.
func (s *CreditService) Grant(ctx context.Context, userID, workspaceID pgtype.UUID, amountMicro int64, kind, reference string) error {
	if kind != LedgerKindTopup && kind != LedgerKindAdjustment {
		return fmt.Errorf("invalid grant kind %q", kind)
	}
	// The direction of a grant is fixed by the caller's sign convention, so a
	// non-positive amount would silently invert it: Reserve's negation is not
	// applied here, and a negative "grant" would debit the wallet instead.
	if amountMicro <= 0 {
		return fmt.Errorf("grant amount must be positive, got %d", amountMicro)
	}
	return s.adjust(ctx, userID, workspaceID, amountMicro, kind, "grant:"+reference, reference)
}

// adjust applies a signed delta: negative delta = deduct (must have balance),
// positive delta = credit. Idempotent: the fast-path pre-check makes retries
// no-ops; a concurrent duplicate loses the ledger-insert race, rolls back its
// redundant balance change and returns nil.
func (s *CreditService) adjust(ctx context.Context, userID, workspaceID pgtype.UUID, delta int64, kind, idempotencyKey, reference string) error {
	// Fast path: if the ledger row already exists, the operation was applied.
	if _, err := s.queries.GetCreditLedgerByIdempotencyKey(ctx, idempotencyKey); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	qtx := s.queries.WithTx(tx)
	if err := qtx.EnsureCreditBalance(ctx, userID); err != nil {
		return err
	}

	var balanceAfter int64
	if delta < 0 {
		balanceAfter, err = qtx.DeductCreditBalance(ctx, db.DeductCreditBalanceParams{
			UserID: userID, AmountMicro: -delta,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// A conditional deduct matches no row for two different reasons,
			// and they need opposite answers. Either the balance is genuinely
			// short, or a concurrent duplicate holding the same idempotency
			// key committed while we were blocked on the credit_balance row
			// lock — its deduction is what left too little for ours. The
			// second is a success we must not report as a failure: Plan 3
			// would reject the generation as unaffordable even though the
			// credits are already spent. Re-read the key to tell them apart.
			// Read it through qtx, not the pool: the extra connection would
			// be a second one held while this transaction is still open.
			// Under READ COMMITTED each statement takes a fresh snapshot, so
			// the winner's committed row is visible from in here too.
			if _, lookupErr := qtx.GetCreditLedgerByIdempotencyKey(ctx, idempotencyKey); lookupErr == nil {
				return nil
			} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
				return lookupErr
			}
			return ErrInsufficientCredits
		}
	} else {
		balanceAfter, err = qtx.CreditCreditBalance(ctx, db.CreditCreditBalanceParams{
			UserID: userID, AmountMicro: delta,
		})
	}
	if err != nil {
		return err
	}

	if _, err := qtx.InsertCreditLedger(ctx, db.InsertCreditLedgerParams{
		UserID: userID, WorkspaceID: workspaceID, Kind: kind,
		AmountMicro: delta, BalanceAfterMicro: balanceAfter,
		Reference: reference, IdempotencyKey: idempotencyKey,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Concurrent duplicate: the other transaction already committed
			// this operation; our rolled-back balance change was redundant.
			return nil
		}
		return err
	}
	return tx.Commit(ctx)
}
