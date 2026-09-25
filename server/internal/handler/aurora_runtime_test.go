package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// auroraEnrollmentImageDigest is a well-formed pinned image reference. Issue
// rejects anything else; the digest itself is not asserted by these tests.
var auroraEnrollmentImageDigest = "ghcr.io/eanfs/multica-aurora@sha256:" + strings.Repeat("a", 64)

// withSandboxEnrollment installs svc on the shared handler and restores the
// previous wiring at cleanup, so a subtest that disables the feature cannot
// leak a nil service into a sibling.
func withSandboxEnrollment(t *testing.T, svc *aurora.SandboxEnrollmentService) {
	t.Helper()
	prev := testHandler.SandboxEnrollment
	testHandler.SandboxEnrollment = svc
	t.Cleanup(func() { testHandler.SandboxEnrollment = prev })
}

// newSandboxEnrollmentService builds the production wiring on the fixture pool.
func newSandboxEnrollmentService() *aurora.SandboxEnrollmentService {
	return aurora.NewSandboxEnrollmentService(testPool, db.New(testPool), nil)
}

// seedManagedEnrollment creates a throwaway workspace with one managed runtime
// and issues a single-use mse_ secret for it. The throwaway workspace keeps the
// per-workspace node and managed-runtime unique indexes from colliding with the
// other handler tests. now is the issuance clock; nil means time.Now.
func seedManagedEnrollment(t *testing.T, now func() time.Time) (token, runtimeID, workspaceID string) {
	t.Helper()
	ctx := context.Background()
	workspaceID = dbfx.Workspace(t, "Aurora enrollment workspace", "aurora-enroll-"+uuid.NewString())
	runtimeID = dbfx.Runtime(t, "Aurora enrollment managed runtime", testutil.Cols{
		"workspace_id": workspaceID,
		"provider":     "aurora_managed",
		"status":       "offline",
		"last_seen_at": nil,
	})
	// Node, enrollment, and daemon-token rows carry no foreign key, so the
	// fixture's row cleanup cannot reach them.
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM daemon_token WHERE workspace_id = $1`, workspaceID)
		testPool.Exec(ctx, `DELETE FROM aurora_sandbox_node WHERE workspace_id = $1`, workspaceID)
	})

	svc := aurora.NewSandboxEnrollmentService(testPool, db.New(testPool), now)
	issued, err := svc.Issue(ctx, parseUUID(workspaceID), parseUUID(runtimeID), auroraEnrollmentImageDigest)
	if err != nil {
		t.Fatalf("issue managed enrollment: %v", err)
	}
	return issued.Token, runtimeID, workspaceID
}

// managedEnrollRequest builds an enroll call. An empty token sends no
// Authorization header; a non-nil body is attached so the test can prove the
// endpoint decodes no caller identity.
func managedEnrollRequest(token string, body any) *http.Request {
	req := testutil.JSONRequest(http.MethodPost, "/api/daemon/managed/enroll", body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// TestManagedRuntimeEnroll is the HTTP contract matrix for the enrollment
// exchange: the endpoint accepts no caller-selected identity and answers 401
// for every credential that is not a live, single-use mse_ secret. The success
// shape is asserted separately in TestManagedRuntimeEnrollSuccess.
func TestManagedRuntimeEnroll(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	withSandboxEnrollment(t, newSandboxEnrollmentService())

	cases := []struct {
		name  string
		setup func(t *testing.T) *http.Request
		want  int
	}{
		{
			name: "missing bearer",
			setup: func(t *testing.T) *http.Request {
				return managedEnrollRequest("", nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "malformed authorization scheme",
			setup: func(t *testing.T) *http.Request {
				req := managedEnrollRequest("", nil)
				req.Header.Set("Authorization", "Token mse_"+strings.Repeat("a", 40))
				return req
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "wrong credential prefix",
			setup: func(t *testing.T) *http.Request {
				return managedEnrollRequest("mdt_"+strings.Repeat("a", 40), nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "well-formed but unknown secret",
			setup: func(t *testing.T) *http.Request {
				return managedEnrollRequest("mse_"+strings.Repeat("a", 40), nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "expired secret",
			setup: func(t *testing.T) *http.Request {
				token, _, _ := seedManagedEnrollment(t, func() time.Time {
					return time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
				})
				return managedEnrollRequest(token, nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "replayed secret",
			setup: func(t *testing.T) *http.Request {
				token, _, _ := seedManagedEnrollment(t, nil)
				if _, _, err := newSandboxEnrollmentService().Consume(context.Background(), token); err != nil {
					t.Fatalf("spend enrollment: %v", err)
				}
				return managedEnrollRequest(token, nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "service disabled",
			setup: func(t *testing.T) *http.Request {
				withSandboxEnrollment(t, nil)
				return managedEnrollRequest("mse_"+strings.Repeat("a", 40), nil)
			},
			want: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Call(t, testHandler.ManagedRuntimeEnroll, tc.setup(t)).Want(tc.want)
		})
	}
}

// TestManagedRuntimeEnrollSuccess pins the exact response contract: the seven
// documented keys, the claude execution projection, one claim slot, the enrolled
// workspace/runtime identity, and an mdt_ daemon credential. The request body
// names a decoy workspace to prove the endpoint decodes no caller identity.
func TestManagedRuntimeEnrollSuccess(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	withSandboxEnrollment(t, newSandboxEnrollmentService())
	token, runtimeID, workspaceID := seedManagedEnrollment(t, nil)

	const decoyWorkspaceID = "00000000-0000-0000-0000-000000000000"
	w := testutil.Call(t, testHandler.ManagedRuntimeEnroll,
		managedEnrollRequest(token, map[string]string{"workspace_id": decoyWorkspaceID})).
		Want(http.StatusOK)

	wantKeys := map[string]bool{
		"workspace_id":            true,
		"daemon_id":               true,
		"runtime":                 true,
		"execution_provider":      true,
		"max_concurrency":         true,
		"daemon_token":            true,
		"daemon_token_expires_at": true,
	}
	var raw map[string]json.RawMessage
	w.JSON(&raw)
	if len(raw) != len(wantKeys) {
		t.Errorf("response has %d keys, want exactly %d: %v", len(raw), len(wantKeys), responseKeys(raw))
	}
	for key := range wantKeys {
		if _, ok := raw[key]; !ok {
			t.Errorf("response is missing %q", key)
		}
	}
	for key := range raw {
		if !wantKeys[key] {
			t.Errorf("response carries undocumented key %q", key)
		}
	}

	var got struct {
		WorkspaceID          string               `json:"workspace_id"`
		DaemonID             string               `json:"daemon_id"`
		Runtime              AgentRuntimeResponse `json:"runtime"`
		ExecutionProvider    string               `json:"execution_provider"`
		MaxConcurrency       int                  `json:"max_concurrency"`
		DaemonToken          string               `json:"daemon_token"`
		DaemonTokenExpiresAt time.Time            `json:"daemon_token_expires_at"`
	}
	w.JSON(&got)

	if got.ExecutionProvider != "claude" {
		t.Errorf("execution_provider = %q, want claude", got.ExecutionProvider)
	}
	if got.ExecutionProvider != aurora.ManagedExecutionProvider {
		t.Errorf("execution_provider = %q, want %q", got.ExecutionProvider, aurora.ManagedExecutionProvider)
	}
	if got.MaxConcurrency != 1 {
		t.Errorf("max_concurrency = %d, want 1", got.MaxConcurrency)
	}
	if got.MaxConcurrency != aurora.ManagedMaxConcurrency {
		t.Errorf("max_concurrency = %d, want %d", got.MaxConcurrency, aurora.ManagedMaxConcurrency)
	}
	if got.WorkspaceID != workspaceID {
		t.Errorf("workspace_id = %q, want the enrolled workspace %q (a body-supplied id must be ignored)", got.WorkspaceID, workspaceID)
	}
	if got.WorkspaceID == decoyWorkspaceID {
		t.Error("the endpoint honoured the caller-supplied workspace_id")
	}
	if got.Runtime.ID != runtimeID {
		t.Errorf("runtime.id = %q, want %q", got.Runtime.ID, runtimeID)
	}
	if got.Runtime.Provider != "aurora_managed" {
		t.Errorf("persisted runtime provider = %q, want aurora_managed", got.Runtime.Provider)
	}
	if got.DaemonID == "" {
		t.Error("daemon_id is empty")
	}
	if !strings.HasPrefix(got.DaemonToken, "mdt_") {
		t.Errorf("daemon_token = %q, want an mdt_ prefix", got.DaemonToken)
	}
	if got.DaemonTokenExpiresAt.Before(time.Now().Add(7 * time.Hour)) {
		t.Errorf("daemon_token_expires_at = %v, want roughly eight hours from now", got.DaemonTokenExpiresAt)
	}

	// The secret is single-use on the success path too.
	testutil.Call(t, testHandler.ManagedRuntimeEnroll, managedEnrollRequest(token, nil)).Want(http.StatusUnauthorized)
}

// TestManagedRuntimeEnrollRejectsSharedToken is the regression that keeps the
// removed global secret a non-credential: AURORA_SANDBOX_TOKEN still being set
// in the environment must not authenticate the replacement endpoint.
func TestManagedRuntimeEnrollRejectsSharedToken(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	withSandboxEnrollment(t, newSandboxEnrollmentService())
	t.Setenv("AURORA_SANDBOX_TOKEN", "legacy-shared-sandbox-token")

	testutil.Call(t, testHandler.ManagedRuntimeEnroll,
		managedEnrollRequest("legacy-shared-sandbox-token", nil)).Want(http.StatusUnauthorized)
}

func responseKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	return keys
}
