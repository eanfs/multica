package main

import "testing"

func TestNamespaceIndexCleanupRegistered(t *testing.T) {
	const version = "580_fleet_namespace_fences_namespace_index"
	if concurrentIndexCleanups[version] != "fleet_namespace_fences_namespace_uidx" || preMigrationHooks[version] == nil {
		t.Fatal("namespace concurrent index retry cleanup missing")
	}
	// DROP INDEX CONCURRENTLY has no invalid rebuild in the down direction.
	if concurrentDownIndexCleanups[version] != "" {
		t.Fatal("drop-only rollback registered as an index build")
	}
}
