package aurora

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AuroraLimits are the local, self-host entitlement gates for a user.
//
// The cloud entitlement client stays out of this path: it fails open without a
// cloud URL (see internal/entitlement), so local enforcement is the only gate
// that actually limits anything in a self-host deployment. All values come
// from the TierCatalog — tiers.go is the single edit point.
type AuroraLimits struct {
	Tier                string
	GenerationsPerMonth int
	Concurrency         int
}

// LimitsForUser resolves the user's subscription tier to its limits. A user
// with no subscription row, or one that is not active, gets the free tier:
// canceled and past-due plans keep working on free limits rather than losing
// access outright, which is the documented MVP semantic.
func LimitsForUser(ctx context.Context, q *db.Queries, tiers *TierCatalog, userID pgtype.UUID) (AuroraLimits, error) {
	activeTier := "free"
	sub, err := q.GetAuroraSubscriptionByUser(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return AuroraLimits{}, err
	case sub.Status == "active":
		activeTier = sub.Tier
	}
	tier, ok := tiers.Lookup(activeTier)
	if !ok {
		// A tier the catalog no longer knows (a plan renamed or retired between
		// a row being written and this read) must not leave the user ungated:
		// fall back to the free tier rather than to no limits at all.
		tier, _ = tiers.Lookup("free")
	}
	return AuroraLimits{
		Tier:                tier.Tier,
		GenerationsPerMonth: tier.GenerationsPerMonth,
		Concurrency:         tier.Concurrency,
	}, nil
}
