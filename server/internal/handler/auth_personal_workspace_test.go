package handler

import (
	"context"
	"testing"
)

// cleanupPersonalWorkspace removes the single-member workspace that
// ensurePersonalWorkspace provisions for userID, together with its seeded
// issue-status rows and the user row, so tests do not leak rows into the
// shared test database. Deleting the workspace cascades its member rows
// (member.workspace_id → workspace ON DELETE CASCADE), and deleting the user
// cascades any remaining member rows (member.user_id → "user" ON DELETE
// CASCADE); issue_status has no FK, so it is removed explicitly.
func cleanupPersonalWorkspace(t *testing.T, userID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM issue_status WHERE workspace_id IN (
				SELECT workspace_id FROM member WHERE user_id = $1
			)`, userID)
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM workspace WHERE id IN (
				SELECT workspace_id FROM member WHERE user_id = $1
			)`, userID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
}

// cleanupPersonalWorkspaceByEmail removes the personal workspace (plus its
// seeded issue-status rows) and the user for a signup identified by email.
// Unlike cleanupPersonalWorkspace it can be registered up front, before the
// flow runs, so a failed signup still leaves no rows behind.
func cleanupPersonalWorkspaceByEmail(t *testing.T, email string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM issue_status WHERE workspace_id IN (
				SELECT workspace_id FROM member WHERE user_id = (SELECT id FROM "user" WHERE email = $1)
			)`, email)
		_, _ = testPool.Exec(context.Background(), `
			DELETE FROM workspace WHERE id IN (
				SELECT workspace_id FROM member WHERE user_id = (SELECT id FROM "user" WHERE email = $1)
			)`, email)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = $1`, email)
	})
}

// TestEnsurePersonalWorkspaceProvisionsOnce gives a brand-new user exactly one
// owner personal workspace, and is idempotent: a second call must not create a
// second workspace.
func TestEnsurePersonalWorkspaceProvisionsOnce(t *testing.T) {
	userID := dbfx.User(t, "Personal WS User", "personal-ws-"+t.Name()+"@multica.ai")
	cleanupPersonalWorkspace(t, userID)

	if err := testHandler.ensurePersonalWorkspace(context.Background(), parseUUID(userID), "Personal WS User"); err != nil {
		t.Fatalf("ensurePersonalWorkspace: %v", err)
	}

	var n int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM workspace WHERE id IN (SELECT workspace_id FROM member WHERE user_id = $1)`,
		userID,
	).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 personal workspace, got %d", n)
	}

	if err := testHandler.ensurePersonalWorkspace(context.Background(), parseUUID(userID), "Personal WS User"); err != nil {
		t.Fatalf("second ensurePersonalWorkspace: %v", err)
	}
	dbfx.QueryRow(t,
		`SELECT count(*) FROM workspace WHERE id IN (SELECT workspace_id FROM member WHERE user_id = $1)`,
		userID,
	).Scan(&n)
	if n != 1 {
		t.Fatalf("second call should not create another workspace, got %d", n)
	}
}

// TestEnsurePersonalWorkspaceSkipsExisting verifies a user who already belongs
// to a workspace (the shared handler-test user owns testWorkspaceID) gets no
// additional workspace provisioned.
func TestEnsurePersonalWorkspaceSkipsExisting(t *testing.T) {
	before := dbfx.Count(t,
		`SELECT count(*) FROM workspace WHERE id IN (SELECT workspace_id FROM member WHERE user_id = $1)`,
		testUserID,
	)
	if err := testHandler.ensurePersonalWorkspace(context.Background(), parseUUID(testUserID), "Handler Test User"); err != nil {
		t.Fatalf("ensurePersonalWorkspace: %v", err)
	}
	after := dbfx.Count(t,
		`SELECT count(*) FROM workspace WHERE id IN (SELECT workspace_id FROM member WHERE user_id = $1)`,
		testUserID,
	)
	if after != before {
		t.Fatalf("existing user workspace count changed: before=%d after=%d", before, after)
	}
}

// TestFindOrCreateUserProvisionsPersonalWorkspace covers the login path:
// a new email through findOrCreateUser (the shared funnel behind both
// VerifyCode and GoogleLogin) lands with a personal workspace, and a second
// login (isNew=false) does not provision again.
func TestFindOrCreateUserProvisionsPersonalWorkspace(t *testing.T) {
	email := "findorcreate-" + t.Name() + "@multica.ai"

	user, isNew, err := testHandler.findOrCreateUser(context.Background(), email)
	if err != nil {
		t.Fatalf("findOrCreateUser: %v", err)
	}
	if !isNew {
		t.Fatal("first call should be a new user")
	}
	userID := uuidToString(user.ID)
	cleanupPersonalWorkspace(t, userID)

	n := dbfx.Count(t,
		`SELECT count(*) FROM workspace WHERE id IN (SELECT workspace_id FROM member WHERE user_id = $1)`,
		userID,
	)
	if n != 1 {
		t.Fatalf("expected 1 personal workspace after signup, got %d", n)
	}

	_, isNew2, err := testHandler.findOrCreateUser(context.Background(), email)
	if err != nil {
		t.Fatalf("second findOrCreateUser: %v", err)
	}
	if isNew2 {
		t.Fatal("second call should not be a new user")
	}
	n2 := dbfx.Count(t,
		`SELECT count(*) FROM workspace WHERE id IN (SELECT workspace_id FROM member WHERE user_id = $1)`,
		userID,
	)
	if n2 != 1 {
		t.Fatalf("second call should not create another workspace, got %d", n2)
	}
}
