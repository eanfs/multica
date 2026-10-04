package testutil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fixture ignoring an explicit private reference would route to the wrong profile.
func TestFleetProfileFixture(t *testing.T) {
	pool, f := NewFleetFixture(t)
	ref := filepath.Join(t.TempDir(), "own-profile.json")
	if err := os.WriteFile(ref, []byte(`{"api_key":"fake-owned-profile-marker"}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := f.FleetProfile(t, "test-fixture-profile", Cols{"profile_ref": ref, "config_version": int64(7)})
	var owner, namespace, stored, record string
	var version int64
	if err := pool.QueryRow(context.Background(), "SELECT owner_id::text,namespace,profile_ref,config_version,to_jsonb(p)::text FROM fleet_credential_profiles p WHERE id=$1", id).Scan(&owner, &namespace, &stored, &version, &record); err != nil {
		t.Fatal(err)
	}
	if owner != f.UserID || namespace != "test-fixture-profile" || stored != ref || version != 7 {
		t.Fatalf("fixture owner=%s namespace=%s refMatch=%v version=%d", owner, namespace, stored == ref, version)
	}
	if strings.Contains(record, "fake-owned-profile-marker") {
		t.Fatal("fixture persisted a secret")
	}
	defaultID := f.FleetProfile(t, "test-fixture-default")
	if err := pool.QueryRow(context.Background(), "SELECT profile_ref FROM fleet_credential_profiles WHERE id=$1", defaultID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(stored)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !filepath.IsAbs(stored) {
		t.Fatal("default fixture profile is not a private absolute regular file")
	}
}
