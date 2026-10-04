package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// A missing provisioning snapshot/credential generation or dependency must fail even with zero nodes.
func TestCheckSchemaEmptyFleetAndMissingColumns(t *testing.T) {
	pool, _ := testutil.NewFleetFixture(t)
	ctx := context.Background()
	schema := fmt.Sprintf("fleet_schema_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, e := pool.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, e := pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		if e != nil {
			t.Error(e)
		}
	})
	tables := []string{"fleet_nodes", "fleet_node_operations", "fleet_node_credentials", "fleet_credential_profiles", "user", "agent_runtime", "agent_task_queue"}
	for _, table := range tables {
		name := pgx.Identifier{table}.Sanitize()
		if _, e := pool.Exec(ctx, "CREATE TABLE "+quoted+"."+name+" (LIKE public."+name+")"); e != nil {
			t.Fatal(e)
		}
	}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	scoped, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(scoped.Close)
	s := New(scoped, "empty")
	if e = s.CheckSchema(ctx); e != nil {
		t.Fatalf("empty fully migrated schema: %v", e)
	}
	for _, tc := range []struct{ table, column string }{{"fleet_nodes", "spec_config"}, {"fleet_node_credentials", "generation"}, {"fleet_node_operations", "approved"}, {"fleet_credential_profiles", "config_version"}, {"agent_runtime", "metadata"}, {"agent_task_queue", "status"}} {
		t.Run(tc.table+"_"+tc.column, func(t *testing.T) {
			table := quoted + "." + pgx.Identifier{tc.table}.Sanitize()
			col := pgx.Identifier{tc.column}.Sanitize()
			if _, e := pool.Exec(ctx, "ALTER TABLE "+table+" RENAME COLUMN "+col+" TO missing_probe_column"); e != nil {
				t.Fatal(e)
			}
			if e := s.CheckSchema(ctx); e == nil {
				t.Fatal("missing required column reported ready")
			}
			if _, e := pool.Exec(ctx, "ALTER TABLE "+table+" RENAME COLUMN missing_probe_column TO "+col); e != nil {
				t.Fatal(e)
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if e = s.CheckSchema(cancelled); e == nil {
		t.Fatal("cancelled probe reported ready")
	}
}

// Pool acquisition is part of the probe budget, not a new unbounded wait before it.
func TestCheckSchemaPoolBudget(t *testing.T) {
	pool, _ := testutil.NewFleetFixture(t)
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	scoped, e := pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer scoped.Close()
	held, e := scoped.Acquire(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer held.Release()
	start := time.Now()
	e = New(scoped, "pool-budget").CheckSchema(context.Background())
	if e == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("unbounded schema acquisition: elapsed=%v err=%v", time.Since(start), e)
	}
}
