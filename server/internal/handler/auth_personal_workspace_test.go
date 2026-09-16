package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
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

// assertPersonalWorkspaceName pins the name of the single workspace a user owns
// — the auto-provisioned personal space — to want. Callers deriving the name
// from a display name spell that expectation as personalWorkspaceName(...), so
// the naming rule lives in one place.
func assertPersonalWorkspaceName(t *testing.T, userID, want string) {
	t.Helper()

	var got string
	dbfx.QueryRow(t, `
		SELECT w.name FROM workspace w
		JOIN member m ON m.workspace_id = w.id
		WHERE m.user_id = $1
	`, userID).Scan(&got)
	if got != want {
		t.Fatalf("personal workspace name = %q, want %q", got, want)
	}
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

// TestFindOrCreateUserNamesNewUserAndWorkspace covers the displayName contract
// behind #13: a caller that already knows the account's real name (GoogleLogin)
// gets both the user row and its personal workspace named accordingly, while a
// caller that has no name to offer (VerifyCode) still falls back to the email
// prefix.
func TestFindOrCreateUserNamesNewUserAndWorkspace(t *testing.T) {
	tests := []struct {
		name        string
		email       string
		displayName string
		wantName    string
	}{
		{
			name:        "provider display name wins",
			email:       "provider-display-name@multica.ai",
			displayName: "  Ada Lovelace  ",
			wantName:    "Ada Lovelace",
		},
		{
			name:     "empty display name falls back to the email prefix",
			email:    "email-code-signup@multica.ai",
			wantName: "email-code-signup",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanupPersonalWorkspaceByEmail(t, tt.email)

			user, isNew, err := testHandler.findOrCreateUser(context.Background(), tt.email, tt.displayName)
			if err != nil {
				t.Fatalf("findOrCreateUser: %v", err)
			}
			if !isNew {
				t.Fatal("expected a newly created user")
			}
			if user.Name != tt.wantName {
				t.Fatalf("user name = %q, want %q", user.Name, tt.wantName)
			}
			assertPersonalWorkspaceName(t, uuidToString(user.ID), personalWorkspaceName(tt.wantName))
		})
	}
}

// googleLoginTestHandler wires a handler against the shared test database and a
// stubbed Google profile, so a test can drive the real signup/provisioning path
// without touching the network.
func googleLoginTestHandler(t *testing.T, email, displayName string) *Handler {
	t.Helper()
	t.Setenv("GOOGLE_CLIENT_ID", "test-client")
	t.Setenv("GOOGLE_CLIENT_SECRET", "test-secret")

	h := newTestHandler(Config{AllowSignup: true})
	h.Queries = testHandler.Queries
	h.TxStarter = testPool
	h.googleOAuthHTTPClient = &http.Client{Transport: googleRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "oauth2.googleapis.com":
			return googleResponse(req, http.StatusOK, `{"access_token":"test-token"}`), nil
		case "www.googleapis.com":
			return googleResponse(req, http.StatusOK, `{"email":"`+email+`","name":"`+displayName+`"}`), nil
		default:
			t.Fatalf("unexpected Google OAuth request: %s", req.URL)
			return nil, nil
		}
	})}
	return h
}

func googleLogin(t *testing.T, h *Handler) LoginResponse {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/auth/google", strings.NewReader(`{"code":"test-code"}`))
	var got LoginResponse
	testutil.Call(t, h.GoogleLogin, req).Want(http.StatusOK).JSON(&got)
	return got
}

// TestGoogleLoginNamesPersonalWorkspaceAfterGoogleProfile is the #13
// regression: findOrCreateUser creates the account — and provisions its
// personal workspace — before GoogleLogin has seen the Google profile, so a
// Google signup used to land with "<email prefix> 的个人空间" as the workspace
// name forever. The Google display name has to reach provisioning.
func TestGoogleLoginNamesPersonalWorkspaceAfterGoogleProfile(t *testing.T) {
	const email = "google-workspace-name@example.com"
	cleanupPersonalWorkspaceByEmail(t, email)

	got := googleLogin(t, googleLoginTestHandler(t, email, "Jane Q. Doe"))
	if got.User.Name != "Jane Q. Doe" {
		t.Fatalf("Google display name not applied to the new user: got %q", got.User.Name)
	}
	assertPersonalWorkspaceName(t, got.User.ID, personalWorkspaceName("Jane Q. Doe"))
}

// TestGoogleLoginRenamesPersonalWorkspaceOnDisplayNameUpgrade covers the other
// reachable shape of the same defect: a user who signed up with an email code
// owns a personal workspace named after the email prefix, and signing in with
// Google is what upgrades that display name. The space has to follow, or the
// two disagree from then on.
func TestGoogleLoginRenamesPersonalWorkspaceOnDisplayNameUpgrade(t *testing.T) {
	const email = "google-workspace-upgrade@example.com"
	cleanupPersonalWorkspaceByEmail(t, email)

	user, isNew, err := testHandler.findOrCreateUser(context.Background(), email, "")
	if err != nil {
		t.Fatalf("findOrCreateUser: %v", err)
	}
	if !isNew {
		t.Fatal("expected a newly created user")
	}
	userID := uuidToString(user.ID)
	assertPersonalWorkspaceName(t, userID, personalWorkspaceName("google-workspace-upgrade"))

	got := googleLogin(t, googleLoginTestHandler(t, email, "Grace Hopper"))
	if got.User.Name != "Grace Hopper" {
		t.Fatalf("Google display name not applied: got %q", got.User.Name)
	}
	assertPersonalWorkspaceName(t, userID, personalWorkspaceName("Grace Hopper"))
}

// TestGoogleLoginKeepsRenamedPersonalWorkspace pins that the display-name
// upgrade only follows the name the space was derived from: a rename the user
// chose themselves is theirs to keep.
func TestGoogleLoginKeepsRenamedPersonalWorkspace(t *testing.T) {
	const email = "google-workspace-user-rename@example.com"
	cleanupPersonalWorkspaceByEmail(t, email)

	user, _, err := testHandler.findOrCreateUser(context.Background(), email, "")
	if err != nil {
		t.Fatalf("findOrCreateUser: %v", err)
	}
	userID := uuidToString(user.ID)
	dbfx.Exec(t,
		`UPDATE workspace SET name = $1 WHERE slug = $2`,
		"My Studio", personalWorkspaceSlug(user.ID),
	)

	got := googleLogin(t, googleLoginTestHandler(t, email, "Grace Hopper"))
	if got.User.Name != "Grace Hopper" {
		t.Fatalf("Google display name not applied: got %q", got.User.Name)
	}
	assertPersonalWorkspaceName(t, userID, "My Studio")
}

// TestFindOrCreateUserProvisionsPersonalWorkspace covers the login path:
// a new email through findOrCreateUser (the shared funnel behind both
// VerifyCode and GoogleLogin) lands with a personal workspace, and a second
// login (isNew=false) does not provision again.
func TestFindOrCreateUserProvisionsPersonalWorkspace(t *testing.T) {
	email := "findorcreate-" + t.Name() + "@multica.ai"

	user, isNew, err := testHandler.findOrCreateUser(context.Background(), email, "")
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

	_, isNew2, err := testHandler.findOrCreateUser(context.Background(), email, "")
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
