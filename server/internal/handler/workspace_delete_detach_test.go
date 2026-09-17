package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

// workspaceDetachCase is one table the deletion manifest classifies
// workspaceDeleteDetach, plus the row this test seeds into it.
type workspaceDetachCase struct {
	table string
	// markerColumn carries a value unique to this test so the seeded row can be
	// found again, and removed again, without depending on the workspace row
	// (which teardown deletes).
	markerColumn string
	marker       string
	// insertSQL seeds one row attributed to the workspace under test. $1 is the
	// workspace id, $2 the marker, $3 the fixture user.
	insertSQL string
}

// workspaceDetachCases is the detach set from workspaceDeletionManifest, kept
// literal because seeding is per-table (each table needs its own columns and
// marker). TestWorkspaceDetachCasesCoverTheManifest keeps it in lockstep with
// the manifest, so a new workspaceDeleteDetach table fails that drift check
// until a case is written for it.
//
// Which mechanism keeps each case green, and whether removing it fails the test:
//   - credit_ledger: only the detached_credit_ledger arm in
//     DeleteWorkspaceAdministration clears workspace_id — no FK, and the final
//     DeleteWorkspace statement does not touch the table. This is the one case
//     that actually guards its arm, and the one that matters: a deleted ledger
//     row takes its idempotency key with it, and a retried payment then credits
//     the account twice.
//   - client_usage_daily: detached_client_usage clears it, but so does
//     cleared_client_usage_workspace in the final DeleteWorkspace statement
//     (workspace.sql), so removing only the arm leaves the case green.
//   - feedback: detached_feedback clears it, but the legacy FK
//     feedback_workspace_id_fkey ... ON DELETE SET NULL does too, so removing
//     only the arm leaves the case green.
//
// The two masked cases still pin the observable contract, but they are not what
// guards their arms.
var workspaceDetachCases = []workspaceDetachCase{
	{
		table:        "credit_ledger",
		markerColumn: "idempotency_key",
		marker:       "handler-teardown-detach-credit-ledger",
		insertSQL: `INSERT INTO credit_ledger
    (user_id, workspace_id, kind, amount_micro, balance_after_micro, reference, idempotency_key)
VALUES ($3, $1, 'topup', 1, 1, 'teardown-detach', $2)`,
	},
	{
		table:        "feedback",
		markerColumn: "message",
		marker:       "handler-teardown-detach-feedback",
		insertSQL:    `INSERT INTO feedback (user_id, workspace_id, message) VALUES ($3, $1, $2)`,
	},
	{
		table:        "client_usage_daily",
		markerColumn: "client_version",
		marker:       "handler-teardown-detach-client-usage",
		insertSQL: `INSERT INTO client_usage_daily
    (user_id, client_type, install_id, activity_date, workspace_id, client_version, os, first_active_at, last_active_at)
VALUES ($3, 'web', gen_random_uuid(), current_date, $1, $2, 'test', now(), now())`,
	},
}

// TestDeleteWorkspace_DetachesRatherThanDeletes is the regression the manifest
// comment promises: it drives the real teardown over HTTP and then asserts, for
// every workspaceDeleteDetach table, that the row survived with its workspace
// attribution cleared. Detaching is what keeps a deleted workspace from taking
// credit_ledger's idempotency keys with it.
//
// A row that is missing or still attributed to the deleted workspace means the
// deletion graph no longer implements the manifest's classification.
func TestDeleteWorkspace_DetachesRatherThanDeletes(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const slug = "handler-tests-teardown-detach"

	// A stale row from an interrupted earlier run would make the counts below
	// ambiguous, so start from a known-empty slate.
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)

	var victimID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug) VALUES ('Teardown Detach Check', $1) RETURNING id
`, slug).Scan(&victimID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// The seeded rows outlive teardown by design, so they are removed by
		// marker rather than through the workspace.
		for _, tc := range workspaceDetachCases {
			_, _ = testPool.Exec(bg, `DELETE FROM `+tc.table+` WHERE `+tc.markerColumn+` = $1`, tc.marker)
		}
		_, _ = testPool.Exec(bg, `DELETE FROM member WHERE workspace_id = $1`, victimID)
		_, _ = testPool.Exec(bg, `DELETE FROM workspace WHERE id = $1`, victimID)
	})

	// DeleteWorkspace re-checks ownership itself and rejects a non-owner, so the
	// requester has to be a real owner of the victim workspace.
	if _, err := testPool.Exec(ctx, `
INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
`, victimID, testUserID); err != nil {
		t.Fatalf("create owner member: %v", err)
	}

	for _, tc := range workspaceDetachCases {
		_, _ = testPool.Exec(ctx, `DELETE FROM `+tc.table+` WHERE `+tc.markerColumn+` = $1`, tc.marker)
		if _, err := testPool.Exec(ctx, tc.insertSQL, victimID, tc.marker, testUserID); err != nil {
			t.Fatalf("seed %s: %v", tc.table, err)
		}
	}

	w := httptest.NewRecorder()
	req := newRequest("DELETE", "/api/workspaces/"+victimID, nil)
	req = withURLParam(req, "id", victimID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}

	for _, tc := range workspaceDetachCases {
		var total, detached int
		if err := testPool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE workspace_id IS NULL)
FROM `+tc.table+` WHERE `+tc.markerColumn+` = $1
`, tc.marker).Scan(&total, &detached); err != nil {
			t.Fatalf("read %s row: %v", tc.table, err)
		}
		switch {
		case total != 1:
			t.Errorf("%s: teardown left %d of the seeded rows, want 1 — the manifest classifies this table workspaceDeleteDetach, so teardown must not delete it", tc.table, total)
		case detached != 1:
			t.Errorf("%s: surviving row still points at the deleted workspace, want workspace_id IS NULL — teardown must detach it", tc.table)
		}
	}
}

// TestWorkspaceDetachCasesCoverTheManifest keeps the hand-written
// workspaceDetachCases in exact lockstep with the manifest's
// workspaceDeleteDetach entries. The case table is literal because seeding is
// per-table — a purely derived set would break the moment a fourth detach table
// appeared without seed logic. This drift check instead makes adding a detach
// classification without writing its case fail here, and also catches a stale
// case left behind when a table is reclassified away from detach.
func TestWorkspaceDetachCasesCoverTheManifest(t *testing.T) {
	covered := make(map[string]struct{}, len(workspaceDetachCases))
	for _, tc := range workspaceDetachCases {
		covered[tc.table] = struct{}{}
	}

	var unguarded, extra []string
	for table, action := range workspaceDeletionManifest {
		if action != workspaceDeleteDetach {
			continue
		}
		if _, ok := covered[table]; !ok {
			unguarded = append(unguarded, table)
		}
	}
	for table := range covered {
		if workspaceDeletionManifest[table] != workspaceDeleteDetach {
			extra = append(extra, table)
		}
	}
	sort.Strings(unguarded)
	sort.Strings(extra)
	if len(unguarded) > 0 || len(extra) > 0 {
		t.Fatalf("workspaceDetachCases drift: detach tables without a case=%v, stale cases=%v", unguarded, extra)
	}
}
