package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestFleetEnqueueFinalInsertPaths(t *testing.T) {
	for _, path := range []string{"issue", "mention", "quick-create", "chat", "autopilot"} {
		for _, state := range []string{"terminating", "terminated", "stopped", "preparing-delete", "preparing-stop"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				pool, f := testutil.NewFleetFixture(t)
				ns := ownedFleetNamespace(t, pool, f, "task6-enqueue-")
				desired := state
				if strings.HasPrefix(state, "preparing-") {
					desired = "running"
				}
				node := f.FleetNode(t, ns, testutil.Cols{"desired": desired, "status": desired})
				if strings.HasPrefix(state, "preparing-") {
					if _, e := pool.Exec(context.Background(), "UPDATE fleet_nodes SET maintenance=true WHERE id=$1 AND namespace=$2", node, ns); e != nil {
						t.Fatal(e)
					}
					f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": ns, "owner_id": f.UserID, "node_id": node, "action": strings.TrimPrefix(state, "preparing-"), "phase": "preparing", "generation": 1, "idempotency_key": "fixture", "request_hash": "fixture", "prior_desired": "running"})
				}
				rt := f.Runtime(t, "enqueue", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
				ag := f.Agent(t, "enqueue", rt, testutil.Cols{"runtime_mode": "local"})
				f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
				q := db.New(pool)
				svc := NewTaskService(q, pool, nil, events.New())
				agentID, ownerID, wsID := util.MustParseUUID(ag), util.MustParseUUID(f.UserID), util.MustParseUUID(f.WorkspaceID)
				var row db.AgentTaskQueue
				var err error
				switch path {
				case "issue", "mention":
					id := f.Issue(t, "enqueue", testutil.Cols{"assignee_type": "agent", "assignee_id": ag})
					issue, e := q.GetIssue(ctx, util.MustParseUUID(id))
					if e != nil {
						t.Fatal(e)
					}
					f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
					if path == "issue" {
						row, err = svc.enqueueIssueTask(ctx, issue, pgtype.UUID{}, false, "", ownerID, pgtype.UUID{}, pgtype.Timestamptz{}, OriginNamed)
					} else {
						row, err = svc.EnqueueTaskForMention(ctx, issue, agentID, pgtype.UUID{}, OriginNamed)
					}
				case "quick-create":
					row, err = svc.EnqueueQuickCreateTask(ctx, wsID, ownerID, agentID, pgtype.UUID{}, "test prompt", "high", "", pgtype.UUID{}, pgtype.UUID{}, nil)
				case "chat":
					id := f.ChatSession(t, ag)
					session, e := q.GetChatSession(ctx, util.MustParseUUID(id))
					if e != nil {
						t.Fatal(e)
					}
					f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
					row, err = svc.EnqueueChatTask(ctx, session, ownerID, false)
				case "autopilot":
					id := f.Insert(t, "autopilot", testutil.Cols{"workspace_id": f.WorkspaceID, "title": "fleet autopilot", "assignee_type": "agent", "assignee_id": ag, "status": "active", "execution_mode": "run_only", "created_by_type": "member", "created_by_id": f.UserID})
					runID := f.Insert(t, "autopilot_run", testutil.Cols{"autopilot_id": id, "source": "manual", "status": "running"})
					f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
					ap, e := q.GetAutopilot(ctx, util.MustParseUUID(id))
					if e != nil {
						t.Fatal(e)
					}
					run, e := q.GetAutopilotRun(ctx, util.MustParseUUID(runID))
					if e != nil {
						t.Fatal(e)
					}
					auto := &AutopilotService{Queries: q, TxStarter: pool, Bus: events.New(), TaskSvc: svc}
					err = auto.dispatchRunOnly(ctx, ap, &run, ownerID)
				}
				var count int
				if e := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1", ag).Scan(&count); e != nil {
					t.Fatal(e)
				}
				if state == "stopped" || state == "preparing-stop" {
					if err != nil || count != 1 {
						t.Fatalf("stop must allow queue: err=%v count=%d", err, count)
					}
				} else if err == nil || count != 0 || row.ID.Valid {
					t.Fatalf("terminal enqueue escaped: err=%v count=%d task=%v", err, count, row.ID)
				}
			})
		}
	}
}

func TestFleetRecoveryEnqueueFinalInsertAndRetainedParentTransition(t *testing.T) {
	for _, path := range []string{"optional-fail-retry", "maybe-retry", "delegated-recovery"} {
		for _, state := range []string{"terminating", "terminated", "stopped", "ordinary", "preparing-delete", "preparing-stop"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				pool, f := testutil.NewFleetFixture(t)
				ns := ownedFleetNamespace(t, pool, f, "task6-recovery-")
				desired := state
				if strings.HasPrefix(state, "preparing-") {
					desired = "running"
				}
				node := f.FleetNode(t, ns, testutil.Cols{"desired": desired, "status": desired})
				if strings.HasPrefix(state, "preparing-") {
					if _, e := pool.Exec(context.Background(), "UPDATE fleet_nodes SET maintenance=true WHERE id=$1 AND namespace=$2", node, ns); e != nil {
						t.Fatal(e)
					}
					f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": ns, "owner_id": f.UserID, "node_id": node, "action": strings.TrimPrefix(state, "preparing-"), "phase": "preparing", "generation": 1, "idempotency_key": "fixture", "request_hash": "fixture", "prior_desired": "running"})
				}
				metadata := json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))
				if state == "ordinary" {
					metadata = json.RawMessage("{}")
				}
				rt := f.Runtime(t, "recovery", testutil.Cols{"runtime_mode": "local", "metadata": metadata})
				ag := f.Agent(t, "recovery", rt, testutil.Cols{"runtime_mode": "local"})
				q := db.New(pool)
				svc := NewTaskService(q, pool, nil, events.New())
				var child *db.AgentTaskQueue
				var err error
				var parentID string
				if path == "delegated-recovery" {
					issueID := f.Issue(t, "recovery", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": ag})
					sourceID := f.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issueID, "status": "completed"})
					parentID = f.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issueID, "status": "failed", "delegated_from_task_id": sourceID})
					commentID := f.Comment(t, issueID, "test-only delegated failure", testutil.Cols{"author_type": "system", "source_task_id": parentID})
					issue, e := q.GetIssue(ctx, util.MustParseUUID(issueID))
					if e != nil {
						t.Fatal(e)
					}
					source, e := q.GetAgentTask(ctx, util.MustParseUUID(sourceID))
					if e != nil {
						t.Fatal(e)
					}
					failed, e := q.GetAgentTask(ctx, util.MustParseUUID(parentID))
					if e != nil {
						t.Fatal(e)
					}
					agent, e := q.GetAgent(ctx, util.MustParseUUID(ag))
					if e != nil {
						t.Fatal(e)
					}
					comment, e := q.GetComment(ctx, util.MustParseUUID(commentID))
					if e != nil {
						t.Fatal(e)
					}
					_, err = svc.dispatchDelegatedFailureRecovery(ctx, &delegatedFailureRecoveryTarget{failed: failed, source: source, issue: issue, agent: agent, comment: comment}, failed.ID)
				} else {
					status := "failed"
					if path == "optional-fail-retry" {
						status = "running"
					}
					issueID := f.Issue(t, "retry-report", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": ag})
					parentID = f.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issueID, "status": status, "attempt": 1, "max_attempts": 3, "failure_reason": "timeout"})
					parent, e := q.GetAgentTask(ctx, util.MustParseUUID(parentID))
					if e != nil {
						t.Fatal(e)
					}
					if path == "maybe-retry" {
						child, err = svc.MaybeRetryFailedTask(ctx, parent)
					} else {
						var changed bool
						_, changed, err = svc.FailTaskWithTransition(ctx, parent.ID, "fixture timeout", "fixture-session", "/fixture/work", "fixture-branch", "timeout", false, "", "")
						if err != nil || !changed {
							t.Fatalf("accepted terminal transition lost: changed=%v err=%v", changed, err)
						}
						after, e := q.GetAgentTask(ctx, parent.ID)
						if e != nil {
							t.Fatal(e)
						}
						if after.Status != "failed" || after.SessionID.String != "fixture-session" || after.WorkDir.String != "/fixture/work" || after.FailureReason.String != "timeout" {
							t.Fatalf("terminal report/session/classification changed: %+v", after)
						}
					}
				}
				f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
				var queued int
				if e := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND status IN ('queued','deferred')", ag).Scan(&queued); e != nil {
					t.Fatal(e)
				}
				blocked := state == "terminating" || state == "terminated" || state == "preparing-delete"
				if blocked {
					if queued != 0 || child != nil {
						t.Fatalf("%s bypassed %s: queued=%d child=%v err=%v", path, state, queued, child, err)
					}
				} else {
					if err != nil || queued != 1 {
						t.Fatalf("%s %s compatibility: queued=%d err=%v", path, state, queued, err)
					}
				}
			})
		}
	}
}

func TestFleetOptionalRetryMaintenanceRaceRetainsTerminalReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-report-race-")
	node := f.FleetNode(t, ns)
	rt := f.Runtime(t, "report-race", testutil.Cols{"runtime_mode": "local", "metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ag := f.Agent(t, "report-race", rt, testutil.Cols{"runtime_mode": "local"})
	issue := f.Issue(t, "report-race", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": ag})
	id := f.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issue, "status": "running", "attempt": 1, "max_attempts": 3})
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if err := db.New(blocker).FleetNodeExclusiveLock(ctx, db.FleetNodeExclusiveLockParams{Namespace: ns, NodeID: util.MustParseUUID(node)}); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, "UPDATE fleet_nodes SET maintenance=true WHERE id=$1 AND namespace=$2", node, ns); err != nil {
		t.Fatal(err)
	}
	f.Insert(t, "fleet_node_operations", testutil.Cols{"namespace": ns, "owner_id": f.UserID, "node_id": node, "action": "delete", "phase": "preparing", "generation": 1, "idempotency_key": "fixture", "request_hash": "fixture", "prior_desired": "running"})
	starter := &fleetClaimTxStarter{pool: pool, entered: make(chan struct{})}
	svc := NewTaskService(db.New(pool), starter, nil, events.New())
	type result struct {
		task    *db.AgentTaskQueue
		changed bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		task, changed, err := svc.FailTaskWithTransition(ctx, util.MustParseUUID(id), "fixture timeout", "fixture-session", "/fixture/work", "fixture-branch", "timeout", false, "", "")
		done <- result{task, changed, err}
	}()
	select {
	case <-starter.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var probe pgx.Tx
	probe, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ table, id string }{{"workspace", f.WorkspaceID}, {"agent_runtime", rt}, {"agent_task_queue", id}} {
		if _, err := probe.Exec(ctx, "SELECT id FROM "+item.table+" WHERE id=$1 FOR UPDATE NOWAIT", item.id); err != nil {
			t.Fatalf("%s owner row preceded node admission: %v", item.table, err)
		}
	}
	_ = probe.Rollback(ctx)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || !result.changed || result.task == nil || result.task.Status != "failed" || result.task.SessionID.String != "fixture-session" {
			t.Fatalf("terminal report lost under delete barrier: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var children int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", id).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("created %d optional retry children under pending delete", children)
	}
}

func TestFleetOptionalRetryArchivedAgentRetainsParentReport(t *testing.T) {
	ctx := context.Background()
	pool, f := testutil.NewFleetFixture(t)
	ns := ownedFleetNamespace(t, pool, f, "task6-archived-report-")
	node := f.FleetNode(t, ns)
	rt := f.Runtime(t, "archived-report", testutil.Cols{"metadata": json.RawMessage(fmt.Sprintf(`{"managed_by":"local_fleet","fleet_node_id":"%s"}`, node))})
	ag := f.Agent(t, "archived-report", rt, testutil.Cols{"archived_at": time.Now()})
	issue := f.Issue(t, "archived-report", testutil.Cols{"status": "todo", "assignee_type": "agent", "assignee_id": ag})
	id := f.Task(t, ag, testutil.Cols{"runtime_id": rt, "issue_id": issue, "status": "running", "attempt": 1, "max_attempts": 3})
	f.Cleanup(t, "DELETE FROM agent_task_queue WHERE agent_id=$1", ag)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	parent, changed, err := svc.FailTaskWithTransition(ctx, util.MustParseUUID(id), "fixture timeout", "fixture-session", "/fixture/work", "", "timeout", false, "", "")
	if err != nil || !changed || parent == nil || parent.Status != "failed" {
		t.Fatalf("logical archive enqueue refusal lost parent report: changed=%v err=%v", changed, err)
	}
	var children int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_task_queue WHERE retry_of_task_id=$1", id).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("archived agent received %d retry children", children)
	}
}
