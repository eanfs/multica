package aurora

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunMonthlySettlement performs the natural-month credit settlement. Call it on
// every tick and once at startup — not just on the 1st: every ledger write is
// idempotent via its month-scoped reference, so reruns are no-ops and a missed
// 1st-of-month tick self-heals on the next one. Per-user failures are logged
// and skipped, so one broken user cannot block the rest.
//
// Phase 1 (expire): for every user who received a "sub:" grant in the window,
// the unused remainder expires: expire = max(0, granted + netSpend), where
// netSpend is the signed sum of that window's deduction (negative) and refund
// (positive) rows. Signup bonuses and topups never expire and are excluded —
// SumMonthlyGrantInWindow only sums reference LIKE 'sub:%', and the topup kind
// is not in the spend kinds.
//
// Phase 2 (grant): every active subscription receives its tier's monthly
// credits as an "adjustment" with reference "sub:<userID>:<YYYY-MM>". Free-tier
// users get the same reference lazily on their first generation of the month
// (entitlement.go), so their grant expires the same way.
func RunMonthlySettlement(ctx context.Context, q *db.Queries, credit *CreditService, tiers *TierCatalog, now time.Time) error {
	now = now.UTC()
	windowStart := now.AddDate(0, -1, 0)
	windowMonth := windowStart.Format("2006-01")

	// Phase 1: expire unused monthly grants.
	recipients, err := q.ListMonthlyGrantRecipients(ctx, db.ListMonthlyGrantRecipientsParams{
		FromTs: pgtype.Timestamptz{Time: windowStart, Valid: true},
		ToTs:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return err
	}
	for _, userID := range recipients {
		if err := expireUserMonth(ctx, q, credit, userID, windowStart, now, windowMonth); err != nil {
			// Known deviation: if the tick was missed and the user already spent
			// below the grant amount, Expire returns ErrInsufficientCredits and
			// this user is skipped — the leftover stays. Never over-deduct, never
			// block the other users.
			slog.Warn("aurora monthly expiry skipped for user", "user_id", util.UUIDToString(userID), "error", err)
		}
	}

	// Phase 2: grant the current month to active subscribers.
	subs, err := q.ListActiveSubscriptionsForGrant(ctx)
	if err != nil {
		return err
	}
	thisMonth := now.Format("2006-01")
	for _, sub := range subs {
		tier, ok := tiers.Lookup(sub.Tier)
		if !ok || tier.MonthlyCreditsMicro() <= 0 {
			slog.Warn("aurora monthly grant skipped for unknown tier", "user_id", util.UUIDToString(sub.UserID), "tier", sub.Tier)
			continue
		}
		wsID, err := q.GetPersonalWorkspaceForUser(ctx, sub.UserID)
		if err != nil {
			slog.Warn("aurora monthly grant skipped for user", "user_id", util.UUIDToString(sub.UserID), "error", err)
			continue
		}
		if err := credit.Grant(ctx, sub.UserID, wsID, tier.MonthlyCreditsMicro(), LedgerKindAdjustment,
			fmt.Sprintf("sub:%s:%s", util.UUIDToString(sub.UserID), thisMonth)); err != nil {
			slog.Warn("aurora monthly grant skipped for user", "user_id", util.UUIDToString(sub.UserID), "error", err)
		}
	}
	return nil
}

// expireUserMonth expires one user's unused monthly grant for the window
// [from, to). It returns the write error, which the caller logs and skips: the
// expiry is best-effort per user.
func expireUserMonth(ctx context.Context, q *db.Queries, credit *CreditService, userID pgtype.UUID, from, to time.Time, month string) error {
	wsID, err := q.GetPersonalWorkspaceForUser(ctx, userID)
	if err != nil {
		return err
	}
	granted, err := q.SumMonthlyGrantInWindow(ctx, db.SumMonthlyGrantInWindowParams{
		UserID: userID,
		FromTs: pgtype.Timestamptz{Time: from, Valid: true},
		ToTs:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return err
	}
	netSpend, err := q.SumCreditLedgerInWindow(ctx, db.SumCreditLedgerInWindowParams{
		UserID: userID,
		Kinds:  []string{LedgerKindDeduction, LedgerKindRefund},
		FromTs: pgtype.Timestamptz{Time: from, Valid: true},
		ToTs:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return err
	}
	expireAmount := granted + netSpend
	if expireAmount <= 0 {
		// Nothing left of the grant: the user spent at least all of it, so
		// there is no remainder to take back. Not an error.
		return nil
	}
	return credit.Expire(ctx, userID, wsID, expireAmount, fmt.Sprintf("%s:%s", util.UUIDToString(userID), month))
}
