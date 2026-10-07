package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exercise real Fleet DDL and the runner only in a schema this test creates.
func TestFleetInvalidConcurrentIndexRetry(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("explicit DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid database configuration")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("database setup failed")
	}
	defer pool.Close()
	var database string
	if err = pool.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal("database unavailable")
	}
	t.Logf("database=%s; all DDL confined to test-owned schema", database)
	schema := fmt.Sprintf("fleet_retry_%d_%d", time.Now().UnixNano(), rand.Uint32())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatal(err)
	}
	defer func() {
		c, x := context.WithTimeout(context.Background(), 10*time.Second)
		defer x()
		if _, e := pool.Exec(c, "DROP SCHEMA "+ident+" CASCADE"); e != nil {
			t.Errorf("owned schema cleanup: %v", e)
		}
	}()
	// Never include public: retrying a partly completed down must not resolve
	// a missing owned object to the live development table of the same name.
	cfg = cfg.Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer scoped.Close()
	if _, err = scoped.Exec(ctx, "CREATE TABLE agent_runtime(id uuid, owner_id uuid, metadata jsonb)"); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../migrations/*_fleet_*.up.sql")
	if err != nil || len(files) < 13 {
		t.Fatalf("want at least 13 Fleet migrations, got %d err=%v", len(files), err)
	}
	snapshotFound := false
	for _, file := range files {
		if filepath.Base(file) == "576_fleet_provisioning_snapshot.up.sql" {
			snapshotFound = true
		}
	}
	if !snapshotFound {
		t.Fatal("Fleet provisioning snapshot migration missing from recovery discovery")
	}
	sort.Strings(files)
	opts := runOptions{Direction: "up", Files: files, SchemaMigrationsTable: schema + ".schema_migrations", AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1}
	// Install only the tables, then leave an INVALID index by a failed unique build.
	opts.Files = files[:1]
	if err = runMigrations(ctx, scoped, opts); err != nil {
		t.Fatal(err)
	}
	if _, err = scoped.Exec(ctx, "INSERT INTO fleet_nodes(id,namespace,owner_id) SELECT '11111111-1111-1111-1111-111111111111'::uuid,'retry',gen_random_uuid() FROM generate_series(1,2)"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(files[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scoped.Exec(ctx, string(body)); err == nil {
		t.Fatal("duplicate-ID unique build must fail")
	}
	assertIndexValidity(t, scoped, schema, "fleet_nodes_id_idx", false)
	if _, err = scoped.Exec(ctx, "DELETE FROM fleet_nodes WHERE ctid IN (SELECT ctid FROM fleet_nodes LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	opts.Files = files[1:]
	opts.Hooks = map[string]preMigrationHook{}
	// Index-aware: only the Fleet migrations that actually build a concurrent
	// index carry a cleanup hook. concurrentIndexCleanups is the authority for
	// which ones do, and TestEveryConcurrentUpBuildHasCleanup independently
	// proves every concurrent build (Fleet or not) is registered. A Fleet
	// migration outside the registry must not register a hook, so a stray hook
	// cannot hide the fact that a build has no cleanup.
	var fleetIndexes []string
	for _, file := range opts.Files {
		version := strings.TrimSuffix(filepath.Base(file), ".up.sql")
		index, registered := concurrentIndexCleanups[version]
		if !registered {
			if _, hooked := preMigrationHooks[version]; hooked {
				t.Fatalf("non-index Fleet migration must not register a pre-migration hook: %s", version)
			}
			continue
		}
		if preMigrationHooks[version] == nil {
			t.Fatalf("missing Fleet cleanup registration: %s", version)
		}
		index = strings.TrimPrefix(index, "public.")
		fleetIndexes = append(fleetIndexes, index)
		opts.Hooks[version] = cleanupInvalidConcurrentIndexHook(schema + "." + index)
	}
	if err = runMigrations(ctx, scoped, opts); err != nil {
		t.Fatal(err)
	}
	for _, index := range fleetIndexes {
		assertIndexValidity(t, scoped, schema, index, true)
	}
	var constraints, required, indexes int
	if err = scoped.QueryRow(ctx, "SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace JOIN pg_class r ON r.oid=c.conrelid WHERE n.nspname=$1 AND r.relname LIKE 'fleet_%' AND c.contype IN ('f','p','u')", schema).Scan(&constraints); err != nil || constraints != 0 {
		t.Fatalf("inline FK/PK/UNIQUE constraints=%d err=%v", constraints, err)
	}
	if err = scoped.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name LIKE 'fleet_%' AND column_name IN ('id','node_id','owner_id','namespace') AND is_nullable='NO'", schema).Scan(&required); err != nil || required != 15 {
		t.Fatalf("required NOT NULL fields=%d err=%v", required, err)
	}
	if err = scoped.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname LIKE 'fleet_%'", schema).Scan(&indexes); err != nil || indexes != len(fleetIndexes) {
		t.Fatalf("Fleet pg_indexes=%d want=%d err=%v", indexes, len(fleetIndexes), err)
	}
	// Interrupt a concurrent DROP while a reader retains the old index snapshot.
	blocker, err := scoped.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, "SELECT * FROM fleet_nodes"); err != nil {
		t.Fatal(err)
	}
	dropper, err := scoped.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer dropper.Release()
	if _, err = dropper.Exec(ctx, "SET statement_timeout='200ms'"); err != nil {
		t.Fatal(err)
	}
	_, dropErr := dropper.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS fleet_nodes_id_idx")
	if _, err = dropper.Exec(ctx, "SET statement_timeout=DEFAULT"); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if dropErr == nil {
		t.Fatal("DROP should have been interrupted")
	}
	// Down uses actual files, including IF EXISTS for an interrupted drop retry.
	down := make([]string, len(files))
	for i, file := range files {
		down[len(files)-1-i] = strings.TrimSuffix(file, ".up.sql") + ".down.sql"
	}
	opts.Direction = "down"
	opts.Files = down
	opts.Hooks = nil
	if err = runMigrations(ctx, scoped, opts); err != nil {
		t.Fatal(err)
	}
	opts.Direction = "up"
	opts.Files = files
	if err = runMigrations(ctx, scoped, opts); err != nil {
		t.Fatal(err)
	}
	assertIndexValidity(t, scoped, schema, "fleet_nodes_id_idx", true)
}
