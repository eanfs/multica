package operator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestProfileMapProjectsExplicitRefsAndTombstones(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(profile, []byte(`{"api_key":"fake-map-marker"}`), 0600); err != nil {
		t.Fatal(err)
	}
	active := "00000000-0000-0000-0000-000000000001"
	disabled := "00000000-0000-0000-0000-000000000002"
	raw, err := json.Marshal(map[string]any{"version": 7, "owners": map[string]string{active: profile, disabled: ""}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "map.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	refs, version, err := LoadProfiles(path)
	if err != nil || version != 7 || len(refs) != 2 || refs[util.MustParseUUID(active)] != profile || refs[util.MustParseUUID(disabled)] != "" {
		t.Fatalf("explicit projection lost ref/version/tombstone: %v %d %v", refs, version, err)
	}
	// This tests the map boundary; credential file shape/mode matrices remain owned
	// by the accepted fleet.LoadProfile tests, not duplicated here.
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = LoadProfiles(path); err == nil {
		t.Fatal("public profile map accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "map-link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err = LoadProfiles(link); err == nil {
		t.Fatal("symlink private map accepted")
	}
}

func TestProfileMapRejectsBadOwnerAndVersion(t *testing.T) {
	for _, raw := range []string{`{"version":1,"owners":{"bad":"/approved/profile"}}`, `{"version":0,"owners":{}}`, `{"version":1.5,"owners":{}}`, `{"version":1,"owners":{},"secret":"no"}`, `{"version":1,"owners":null}`, `{"version":"1","owners":{}}`, `{"version":1,"owners":{"00000000-0000-0000-0000-000000000001":"","00000000-0000-0000-0000-000000000001":""}}`} {
		path := filepath.Join(t.TempDir(), "map.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadProfiles(path); err == nil {
			t.Fatal("invalid private owner/version map accepted")
		}
	}
}
