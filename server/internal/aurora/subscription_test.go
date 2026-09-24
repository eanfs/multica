package aurora_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// subscriptionTestDB is the per-test handle: the queries under test, the
// fixture that owns every row written, and a user of its own.
//
// The user is per-test rather than the suite fixture because
// aurora_subscription is unique per user: two cases sharing one user would
// depend on each other's execution order.
type subscriptionTestDB struct {
	q      *db.Queries
	fx     *testutil.Fixture
	userID pgtype.UUID
}

func newSubscriptionTestDB(t *testing.T, email string) subscriptionTestDB {
	t.Helper()
	if agentsTestPool == nil {
		t.Skip("database not available")
	}
	fx := testutil.New(agentsTestPool, agentsTestWorkspaceID, agentsTestUserID)
	userID := util.MustParseUUID(fx.User(t, "Aurora Subscription Test User", email))
	// The subscription row is written through the query under test, not
	// through fx.Insert, so its teardown has to be registered by hand —
	// otherwise the row outlives the test and the next run's
	// GetAuroraSubscriptionByStripeID finds two rows for one Stripe id.
	fx.Cleanup(t, "DELETE FROM aurora_subscription WHERE user_id = $1", userID)
	return subscriptionTestDB{q: db.New(agentsTestPool), fx: fx, userID: userID}
}

// tsAt is the pgtype wrapper for a timestamptz column. Stripe's event
// `created` is whole seconds, which is why every timestamp here is truncated.
func tsAt(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at.UTC().Truncate(time.Second), Valid: true}
}

// eventTime is the base for a test's Stripe events. It is truncated up front so
// expectations built by adding to it stay whole-second and compare equal to
// what the column stores.
func eventTime() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

// subscriptionEvent is the shape the Task 3 webhook upserts: one Stripe event's
// view of the subscription. Tests override only the fields they are about.
func subscriptionEvent(userID pgtype.UUID, eventCreated time.Time) db.UpsertAuroraSubscriptionParams {
	return db.UpsertAuroraSubscriptionParams{
		UserID:             userID,
		Tier:               "creator",
		Status:             "active",
		StripeEventCreated: tsAt(eventCreated),
	}
}

func applySubscriptionEvent(t *testing.T, q *db.Queries, arg db.UpsertAuroraSubscriptionParams) db.UpsertAuroraSubscriptionRow {
	t.Helper()
	row, err := q.UpsertAuroraSubscription(context.Background(), arg)
	if err != nil {
		t.Fatalf("UpsertAuroraSubscription(%s/%s): %v", arg.Tier, arg.Status, err)
	}
	return row
}

// TestUpsertAuroraSubscriptionIgnoresStaleEvent pins the ordering guard.
//
// Stripe does not guarantee delivery order. The failure it prevents is a
// checkout.session.completed delayed behind a customer.subscription.deleted:
// without the guard the stale checkout rewrites status back to 'active' with a
// fresh period, and the monthly grant scan starts handing credits to a
// subscription nobody is being charged for.
func TestUpsertAuroraSubscriptionIgnoresStaleEvent(t *testing.T) {
	s := newSubscriptionTestDB(t, "aurora-subscription-stale@multica.ai")
	cancelAt := eventTime().Add(-1 * time.Hour)
	checkoutAt := cancelAt.Add(-2 * time.Minute)

	// The cancelation lands first.
	canceled := subscriptionEvent(s.userID, cancelAt)
	canceled.Status = "canceled"
	canceled.CurrentPeriodEnd = tsAt(cancelAt.Add(24 * time.Hour))
	canceled.CancelAtPeriodEnd = true
	if row := applySubscriptionEvent(t, s.q, canceled); row.Status != "canceled" {
		t.Fatalf("cancelation upsert status = %q, want canceled", row.Status)
	}

	// The older checkout event arrives late. It must not win.
	stale := subscriptionEvent(s.userID, checkoutAt)
	stale.CurrentPeriodEnd = tsAt(checkoutAt.Add(30 * 24 * time.Hour))
	row := applySubscriptionEvent(t, s.q, stale)

	if row.Status != "canceled" {
		t.Errorf("stale event regressed status to %q, want canceled", row.Status)
	}
	if !row.CurrentPeriodEnd.Time.Equal(cancelAt.Add(24 * time.Hour)) {
		t.Errorf("stale event regressed current_period_end to %v, want %v",
			row.CurrentPeriodEnd.Time, cancelAt.Add(24*time.Hour))
	}
	if !row.CancelAtPeriodEnd {
		t.Error("stale event cleared cancel_at_period_end")
	}
	if !row.StripeEventCreated.Time.Equal(cancelAt) {
		t.Errorf("stale event overwrote stripe_event_created with %v, want %v",
			row.StripeEventCreated.Time, cancelAt)
	}

	// The refused event must still return the authoritative row, not
	// pgx.ErrNoRows: Task 3 acks on this value.
	stored, err := s.q.GetAuroraSubscriptionByUser(context.Background(), s.userID)
	if err != nil {
		t.Fatalf("GetAuroraSubscriptionByUser: %v", err)
	}
	if stored.Status != "canceled" || !stored.CurrentPeriodEnd.Time.Equal(cancelAt.Add(24*time.Hour)) {
		t.Errorf("persisted row = %s/%v, want canceled/%v",
			stored.Status, stored.CurrentPeriodEnd.Time, cancelAt.Add(24*time.Hour))
	}

	// And a genuinely newer event still applies — the guard is an ordering
	// rule, not a write-once latch.
	resumed := subscriptionEvent(s.userID, cancelAt.Add(time.Minute))
	resumed.CurrentPeriodEnd = tsAt(cancelAt.Add(31 * 24 * time.Hour))
	after := applySubscriptionEvent(t, s.q, resumed)
	if after.Status != "active" || !after.CurrentPeriodEnd.Time.Equal(cancelAt.Add(31*24*time.Hour)) {
		t.Errorf("newer event did not apply: status=%s period=%v", after.Status, after.CurrentPeriodEnd.Time)
	}
}

// TestUpsertAuroraSubscriptionAppliesEventWithEqualTimestamp pins the tie
// breaker. Stripe's created field has one-second resolution, so the events of a
// single checkout share a timestamp; refusing ties would silently drop real
// state changes. Ties keep delivery order.
func TestUpsertAuroraSubscriptionAppliesEventWithEqualTimestamp(t *testing.T) {
	s := newSubscriptionTestDB(t, "aurora-subscription-tie@multica.ai")
	at := eventTime().Add(-1 * time.Hour)

	first := subscriptionEvent(s.userID, at)
	first.CurrentPeriodEnd = tsAt(at.Add(30 * 24 * time.Hour))
	applySubscriptionEvent(t, s.q, first)

	second := subscriptionEvent(s.userID, at)
	second.Status = "past_due"
	second.CurrentPeriodEnd = tsAt(at.Add(31 * 24 * time.Hour))
	row := applySubscriptionEvent(t, s.q, second)

	if row.Status != "past_due" {
		t.Errorf("same-second event was refused: status = %q, want past_due", row.Status)
	}
	if !row.CurrentPeriodEnd.Time.Equal(at.Add(31 * 24 * time.Hour)) {
		t.Errorf("same-second event did not apply: period = %v", row.CurrentPeriodEnd.Time)
	}
}

// TestUpsertAuroraSubscriptionPreservesStripeIDs locks in the COALESCE: a
// webhook is not a full snapshot, so an event that carries no subscription id
// must not erase the one a later GetAuroraSubscriptionByStripeID depends on.
func TestUpsertAuroraSubscriptionPreservesStripeIDs(t *testing.T) {
	s := newSubscriptionTestDB(t, "aurora-subscription-ids@multica.ai")
	at := eventTime().Add(-1 * time.Hour)

	withIDs := subscriptionEvent(s.userID, at)
	withIDs.StripeCustomerID = pgtype.Text{String: "cus_test_1", Valid: true}
	withIDs.StripeSubscriptionID = pgtype.Text{String: "sub_test_1", Valid: true}
	applySubscriptionEvent(t, s.q, withIDs)

	withoutIDs := subscriptionEvent(s.userID, at.Add(time.Minute))
	withoutIDs.Tier = "pro"
	row := applySubscriptionEvent(t, s.q, withoutIDs)

	if row.StripeCustomerID.String != "cus_test_1" || row.StripeSubscriptionID.String != "sub_test_1" {
		t.Errorf("stripe ids lost: customer=%q subscription=%q",
			row.StripeCustomerID.String, row.StripeSubscriptionID.String)
	}

	byStripe, err := s.q.GetAuroraSubscriptionByStripeID(context.Background(), pgtype.Text{String: "sub_test_1", Valid: true})
	if err != nil {
		t.Fatalf("GetAuroraSubscriptionByStripeID: %v", err)
	}
	if byStripe.UserID != s.userID || byStripe.Tier != "pro" {
		t.Errorf("stripe lookup = user %v tier %q, want %v/pro", byStripe.UserID, byStripe.Tier, s.userID)
	}
}

// TestCountActiveGenerationsIncludesUnlinkedQueuedGenerations pins the
// concurrency gate's blind spot. CreateAuroraGeneration inserts the generation
// before it has a task_id, so an inner join reports zero active work in that
// window and two simultaneous requests both clear a concurrency of one.
func TestCountActiveGenerationsIncludesUnlinkedQueuedGenerations(t *testing.T) {
	s := newSubscriptionTestDB(t, "aurora-subscription-active@multica.ai")
	ctx := context.Background()

	rt := s.fx.Runtime(t, "aurora-subscription-test-runtime")
	agentID := s.fx.Agent(t, "Aurora Subscription Test Agent", rt)
	// Both tasks carry a runtime_id: agent_task_queue_active_requires_runtime
	// requires a non-terminal task to have one, and it is expressed as
	// "runtime_id IS NOT NULL OR completed_at IS NOT NULL" rather than as a
	// status test, so a 'completed' row with neither is rejected too.
	runningTask := s.fx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": rt})
	completedTask := s.fx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": rt})

	generation := func(status string, over ...testutil.Cols) {
		t.Helper()
		cols := testutil.Cols{
			"workspace_id": agentsTestWorkspaceID,
			"user_id":      s.userID,
			"skill_id":     "aurora:image",
			"prompt":       "count active generations",
			"status":       status,
		}
		for _, override := range over {
			for name, value := range override {
				cols[name] = value
			}
		}
		s.fx.Insert(t, "aurora_generation", cols)
	}

	// Counted: the pre-link window this test exists for, and live work.
	generation("queued")
	generation("queued", testutil.Cols{"task_id": runningTask})
	// Not counted: a task that reached a terminal state, and a generation that
	// died before it was ever linked — counting the latter would occupy a
	// concurrency slot forever.
	generation("succeeded", testutil.Cols{"task_id": completedTask})
	generation("failed")

	got, err := s.q.CountActiveGenerations(ctx, s.userID)
	if err != nil {
		t.Fatalf("CountActiveGenerations: %v", err)
	}
	if got != 2 {
		t.Errorf("CountActiveGenerations = %d, want 2 (unlinked queued + running task)", got)
	}
}
