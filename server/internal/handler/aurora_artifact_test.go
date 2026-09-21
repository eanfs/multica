package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestReportTaskArtifactsWritesAssetRows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	issueID := dbfx.Issue(t, "aurora artifact issue")
	agentID := dbfx.Agent(t, "AuroraArtifactAgent", handlerTestRuntimeID(t))
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t),
		"status":     "running",
		"issue_id":   issueID,
	})
	dbfx.Insert(t, "aurora_generation", testutil.Cols{
		"workspace_id":     testWorkspaceID,
		"user_id":          testUserID,
		"skill_id":         "xhs-image",
		"prompt":           "artifact test",
		"status":           "running",
		"task_id":          taskID,
		"credits_reserved": int64(620_000_000),
	})

	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/artifacts",
		map[string]any{"artifacts": []map[string]string{
			{"name": "poster.png", "media_url": "https://cdn.example/obj/1", "format": "png"},
			{"name": "poster@2x.png", "media_url": "https://cdn.example/obj/2", "format": "png"},
		}}, testWorkspaceID, "aurora-artifact-daemon")
	req = withURLParam(req, "taskId", taskID)

	testutil.Call(t, testHandler.ReportTaskArtifacts, req).Want(http.StatusOK)

	rows, err := testHandler.Queries.ListAuroraAssets(ctx, db.ListAuroraAssetsParams{
		GenerationID: parseUUID(genIDOfTask(t, taskID)),
		WorkspaceID:  parseUUID(testWorkspaceID),
		Limit:        10,
		Offset:       0,
	})
	if err != nil {
		t.Fatalf("ListAuroraAssets: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("asset count = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Kind != "image" {
			t.Errorf("asset kind = %q, want image", r.Kind)
		}
		if !r.MediaUrl.Valid || r.MediaUrl.String == "" {
			t.Errorf("asset media_url = %q, want non-empty", r.MediaUrl.String)
		}
		if r.Format.String != "png" {
			t.Errorf("asset format = %q, want png", r.Format.String)
		}
	}
}

func TestReportTaskArtifactsRejectsNonAuroraTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	issueID := dbfx.Issue(t, "non-aurora artifact issue")
	agentID := dbfx.Agent(t, "NonAuroraArtifactAgent", handlerTestRuntimeID(t))
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t),
		"status":     "running",
		"issue_id":   issueID,
	})

	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/artifacts",
		map[string]any{"artifacts": []map[string]string{
			{"name": "a.png", "media_url": "https://cdn.example/obj/1", "format": "png"},
		}}, testWorkspaceID, "non-aurora-artifact-daemon")
	req = withURLParam(req, "taskId", taskID)

	testutil.Call(t, testHandler.ReportTaskArtifacts, req).Want(http.StatusNotFound)
}

// genIDOfTask returns the generation id reverse-linked to taskID.
func genIDOfTask(t *testing.T, taskID string) string {
	t.Helper()
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`SELECT id FROM aurora_generation WHERE task_id = $1`, taskID).Scan(&id); err != nil {
		t.Fatalf("load generation by task: %v", err)
	}
	return id.String()
}
