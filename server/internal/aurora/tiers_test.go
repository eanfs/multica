package aurora_test

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/aurora"
)

func TestTierCatalog(t *testing.T) {
	tc := aurora.NewTierCatalog("", "", "", "", "", "") // price ids empty in unit tests
	if len(tc.Tiers) != 3 {
		t.Fatalf("expected 3 tiers, got %d", len(tc.Tiers))
	}
	creator, ok := tc.Lookup("creator")
	if !ok {
		t.Fatal("creator tier missing")
	}
	if creator.MonthlyCredits != 3000 {
		t.Fatalf("creator monthly credits = %d, want 3000", creator.MonthlyCredits)
	}
	if creator.MonthlyCreditsMicro() != 3000*1_000_000 {
		t.Fatal("creator micro conversion wrong")
	}
	if creator.GenerationsPerMonth != 30 {
		t.Fatalf("creator generations/month = %d, want 30", creator.GenerationsPerMonth)
	}
	pro, _ := tc.Lookup("pro")
	if pro.Concurrency != 5 {
		t.Fatalf("pro concurrency = %d, want 5", pro.Concurrency)
	}
	free, _ := tc.Lookup("free")
	if free.MonthlyCredits != 200 || free.Concurrency != 1 || free.GenerationsPerMonth != 10 {
		t.Fatalf("free tier wrong: %+v", free)
	}
	if len(tc.Topups) != 2 {
		t.Fatalf("expected 2 topup tiers, got %d", len(tc.Topups))
	}
	if _, ok := tc.LookupTopup("t5"); !ok {
		t.Fatal("topup t5 missing")
	}
	if _, ok := tc.LookupTopup("t20"); !ok {
		t.Fatal("topup t20 missing")
	}
}

// The paid tiers must be strictly ordered, or a tier comparison anywhere in the
// entitlement path would be meaningless. The free tier must be the cheapest of
// all three on every axis, since it is the fallback LimitsForUser hands a user
// with no subscription.
func TestTierCatalogIsMonotonic(t *testing.T) {
	tc := aurora.NewTierCatalog("cm", "cy", "pm", "py", "t5", "t20")
	prev, ok := tc.Lookup("free")
	if !ok {
		t.Fatal("free tier missing")
	}
	for _, name := range []string{"creator", "pro"} {
		cur, ok := tc.Lookup(name)
		if !ok {
			t.Fatalf("%s tier missing", name)
		}
		if cur.MonthlyCredits <= prev.MonthlyCredits {
			t.Errorf("%s monthly credits %d not above %s's %d", name, cur.MonthlyCredits, prev.Tier, prev.MonthlyCredits)
		}
		if cur.GenerationsPerMonth <= prev.GenerationsPerMonth {
			t.Errorf("%s generations/month %d not above %s's %d", name, cur.GenerationsPerMonth, prev.Tier, prev.GenerationsPerMonth)
		}
		if cur.Concurrency <= prev.Concurrency {
			t.Errorf("%s concurrency %d not above %s's %d", name, cur.Concurrency, prev.Tier, prev.Concurrency)
		}
		prev = cur
	}
	// The paid tiers carry the price ids the catalog was built with; the free
	// tier must not, because a free checkout is a bug rather than a purchase.
	if free, _ := tc.Lookup("free"); free.StripePriceMonthly != "" || free.StripePriceYearly != "" {
		t.Errorf("free tier has a price id: %+v", free)
	}
	for _, name := range []string{"creator", "pro"} {
		tier, _ := tc.Lookup(name)
		if tier.StripePriceMonthly == "" || tier.StripePriceYearly == "" {
			t.Errorf("%s tier lost its price ids: %+v", name, tier)
		}
	}
}

func TestTierCatalogUnknownLookups(t *testing.T) {
	tc := aurora.NewTierCatalog("", "", "", "", "", "")
	if _, ok := tc.Lookup("enterprise"); ok {
		t.Error("Lookup returned an unknown tier")
	}
	if _, ok := tc.Lookup(""); ok {
		t.Error("Lookup returned the empty tier")
	}
	if _, ok := tc.LookupTopup("t50"); ok {
		t.Error("LookupTopup returned an unknown topup")
	}
}
