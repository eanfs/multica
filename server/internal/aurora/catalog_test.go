package aurora_test

import (
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/aurora"
)

func TestCatalogHasSixteenEntriesAndUniqueIDs(t *testing.T) {
	cat := aurora.Catalog()
	wantIDs := []string{"poster", "xhs-image", "product-image", "text-image", "image-edit", "id-photo", "image-video", "text-video", "video-captions", "avatar-video", "xhs-copy", "resume", "document-summary", "transcription", "ppt", "excel"}
	if len(cat) != len(wantIDs) {
		t.Fatalf("expected 16 skills, got %d", len(cat))
	}
	seen := map[string]bool{}
	for i, e := range cat {
		if e.ID == "" || seen[e.ID] {
			t.Fatalf("empty or duplicate skill id %q", e.ID)
		}
		seen[e.ID] = true
		if e.ID != wantIDs[i] {
			t.Errorf("catalog[%d].ID = %q, want %q", i, e.ID, wantIDs[i])
		}
		if e.Credits <= 0 {
			t.Errorf("skill %q has non-positive credits %d", e.ID, e.Credits)
		}
		if e.Name == "" || e.NameEn == "" {
			t.Errorf("skill %q missing bilingual name", e.ID)
		}
	}
}

func TestCatalogAvailability(t *testing.T) {
	available := 0
	for _, e := range aurora.Catalog() {
		if e.Available {
			available++
		}
	}
	if available != 13 {
		t.Fatalf("expected 13 available skills, got %d", available)
	}
	for _, id := range []string{"avatar-video", "ppt", "excel"} {
		e, ok := aurora.Lookup(id)
		if !ok {
			t.Fatalf("Lookup(%q) = not found", id)
		}
		if e.Available {
			t.Errorf("phase-2 skill %q should be unavailable", id)
		}
	}
}

func TestExists(t *testing.T) {
	for _, e := range aurora.Catalog() {
		if !aurora.Exists(e.ID) {
			t.Errorf("Exists(%q) = false, want true", e.ID)
		}
		got, ok := aurora.Lookup(e.ID)
		if !ok || !reflect.DeepEqual(got, e) {
			t.Errorf("Lookup(%q) = %#v, %v; want %#v, true", e.ID, got, ok, e)
		}
	}
	for _, id := range []string{"nope", "", "XHS-IMAGE"} {
		if aurora.Exists(id) {
			t.Errorf("Exists(%q) = true, want false", id)
		}
		if got, ok := aurora.Lookup(id); ok || !reflect.DeepEqual(got, aurora.SkillCatalogEntry{}) {
			t.Errorf("Lookup(%q) = %#v, %v; want zero entry, false", id, got, ok)
		}
	}
}

func TestCatalogCopiesCannotChangeOtherCallers(t *testing.T) {
	entries := aurora.Catalog()
	entries[0].ID = "changed"
	entries[0].Input[0] = "changed"
	entries[0].Output[0] = "changed"
	poster, ok := aurora.Lookup("poster")
	if !ok || poster.Input[0] != "text" || poster.Output[0] != "image" {
		t.Fatalf("mutating Catalog changed Lookup: %#v, %v", poster, ok)
	}
	poster.Input[0] = "changed"
	poster.Output[0] = "changed"
	fresh := aurora.Catalog()[0]
	if fresh.ID != "poster" || fresh.Input[0] != "text" || fresh.Output[0] != "image" {
		t.Errorf("mutating Lookup changed Catalog: %#v", fresh)
	}
}
