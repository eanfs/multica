//go:build dockerintegration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// This file is the opt-in Docker/fake-Claude half of Task 14. The build tag and
// the MULTICA_RUN_DOCKER_INTEGRATION=1 environment gate are checked before any
// Docker client, Engine, executable, account, database or network lookup, so a
// default or un-gated run touches no Docker socket. The physical chain is only
// exercised inside the managed environment, which owns the Docker Engine, the
// migrated database, the already-running API/Fleet and the approved digest-pinned
// fake-Claude image.

var approvedNodeImage = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$")

// TestDockerNodeRoundTrip is the single gated entry point. The gate is the very
// first action: no Engine, CLI, account or fixture lookup can run before it.
func TestDockerNodeRoundTrip(t *testing.T) {
	if os.Getenv("MULTICA_RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("Docker opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := RoundTripFakeNode(ctx, t); err != nil {
		t.Fatal(err)
	}
}

type roundTripEnv struct {
	apiURL      string
	token       string
	workspaceID string
	image       string
	restartURL  string
}

// roundTripInputs validates every explicit input before any side effect and
// names the missing one on skip. The approved digest-pinned image is an input,
// not a guess: the managed environment supplies it and the Fleet must serve it.
func roundTripInputs(t *testing.T) roundTripEnv {
	t.Helper()
	image := os.Getenv("MULTICA_FLEET_TEST_IMAGE")
	if image == "" {
		image = os.Getenv("MULTICA_FLEET_TEST_NODE_IMAGE")
	}
	env := roundTripEnv{
		apiURL:      strings.TrimRight(os.Getenv("MULTICA_FLEET_API_URL"), "/"),
		token:       os.Getenv("MULTICA_FLEET_API_TOKEN"),
		workspaceID: os.Getenv("MULTICA_FLEET_WORKSPACE_ID"),
		image:       image,
		restartURL:  os.Getenv("MULTICA_FLEET_RESTART_URL"),
	}
	if env.apiURL == "" {
		t.Skip("MULTICA_FLEET_API_URL required for the Docker round trip")
	}
	if env.token == "" {
		t.Skip("MULTICA_FLEET_API_TOKEN required for the Docker round trip")
	}
	if env.workspaceID == "" {
		t.Skip("MULTICA_FLEET_WORKSPACE_ID required for the Docker round trip")
	}
	if env.image == "" {
		t.Skip("approved digest-pinned MULTICA_FLEET_TEST_IMAGE required for the Docker round trip")
	}
	if !approvedNodeImage.MatchString(env.image) {
		t.Skip("MULTICA_FLEET_TEST_IMAGE must be digest pinned")
	}
	if env.restartURL == "" {
		t.Skip("MULTICA_FLEET_RESTART_URL required to observe a Fleet restart")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required for the Docker round trip")
	}
	return env
}

// fleetAPI is the authenticated task HTTP client. It presents the caller's
// bearer token and workspace header exactly like the browser client; it never
// bypasses authentication.
type fleetAPI struct {
	base        string
	token       string
	workspaceID string
	http        *http.Client
}

func (c *fleetAPI) raw(ctx context.Context, method, path, key string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Workspace-ID", c.workspaceID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return payload, nil
}

func (c *fleetAPI) call(ctx context.Context, method, path, key string, body, out any) error {
	payload, err := c.raw(ctx, method, path, key, body)
	if err != nil {
		return err
	}
	if out != nil && len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

type roundTripNode struct {
	ID          string "json:\"id\""
	OperationID string "json:\"operation_id\""
	InstanceID  string "json:\"instance_id\""
	ImageID     string "json:\"image_id\""
	Name        string "json:\"name\""
	Status      string "json:\"status\""
	Ready       bool   "json:\"ready\""
	ErrorCode   string "json:\"error_code\""
}

// awaitNode polls the authenticated node list with a bounded deadline. It never
// treats wall-clock sleep as proof and never rewrites the observed state.
func (c *fleetAPI) awaitNode(ctx context.Context, nodeID string, want func(roundTripNode) bool, timeout time.Duration) (roundTripNode, error) {
	deadline := time.Now().Add(timeout)
	for {
		var nodes []roundTripNode
		if err := c.call(ctx, http.MethodGet, "/api/cloud-runtime/nodes?limit=100", "", nil, &nodes); err != nil {
			return roundTripNode{}, err
		}
		for _, n := range nodes {
			if n.ID != nodeID {
				continue
			}
			if n.ErrorCode != "" {
				return n, fmt.Errorf("node %s reported error_code %q", nodeID, n.ErrorCode)
			}
			if want(n) {
				return n, nil
			}
		}
		if time.Now().After(deadline) {
			return roundTripNode{}, fmt.Errorf("node %s did not reach the expected state before the deadline", nodeID)
		}
		select {
		case <-ctx.Done():
			return roundTripNode{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// awaitNodeGone proves the node is absent from the authenticated list.
func (c *fleetAPI) awaitNodeGone(ctx context.Context, nodeID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var nodes []roundTripNode
		if err := c.call(ctx, http.MethodGet, "/api/cloud-runtime/nodes?limit=100", "", nil, &nodes); err != nil {
			return err
		}
		found := false
		for _, n := range nodes {
			if n.ID == nodeID {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s still present after the delete deadline", nodeID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// awaitChatReply observes the fake-Claude marker through the real messages
// endpoint. API polling here is the assertion: the execution path really
// produced a completed message.
func (c *fleetAPI) awaitChatReply(ctx context.Context, sessionID, marker string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		payload, err := c.raw(ctx, http.MethodGet, "/api/chat/sessions/"+sessionID+"/messages", "", nil)
		if err != nil {
			return err
		}
		if bytes.Contains(payload, []byte(marker)) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("chat session %s never observed the fake reply marker", sessionID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func inspectNodeContainer(ctx context.Context, cli *client.Client, id string) (container.InspectResponse, error) {
	return cli.ContainerInspect(ctx, id)
}

func mountByName(info container.InspectResponse, target string) string {
	for _, m := range info.Mounts {
		if m.Destination == target {
			return m.Name
		}
	}
	return ""
}

// assertIdentityPreserved proves the stop/start pair kept the exact container
// identity and both owned volume mounts.
func assertIdentityPreserved(before, after container.InspectResponse) error {
	if before.ID == "" || before.ID != after.ID {
		return fmt.Errorf("container identity changed from %q to %q", before.ID, after.ID)
	}
	for _, target := range []string{"/data", "/secrets"} {
		if got, want := mountByName(after, target), mountByName(before, target); got == "" || got != want {
			return fmt.Errorf("mount %s changed from %q to %q", target, want, got)
		}
	}
	return nil
}

// RoundTripFakeNode drives the real sequence from plan line 819 through the
// already-running API/Fleet and the real Docker Engine:
//
//	API create -> daemon register/ready -> bind Runtime to an agent
//	-> chat session -> message -> completed reply -> stop/start identity+volumes
//	-> Fleet restart -> approved-maintenance crash boundary -> delete + token
//	invalidation.
//
// It uses testutil-owned SQL rows for verification and never executes a real
// Claude/agent CLI; the fake Claude baked into the approved image answers.
func RoundTripFakeNode(ctx context.Context, t *testing.T) error {
	t.Helper()
	env := roundTripInputs(t)

	_, fixture := testutil.NewFleetFixture(t)

	dockerCLI, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dockerCLI.Close()

	api := &fleetAPI{base: env.apiURL, token: env.token, workspaceID: env.workspaceID, http: &http.Client{Timeout: 30 * time.Second}}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	nodeName := "docker-roundtrip-" + suffix
	agentName := "docker-roundtrip-agent-" + suffix
	marker := "docker-roundtrip-marker-" + suffix

	var created roundTripNode
	if err := api.call(ctx, http.MethodPost, "/api/cloud-runtime/nodes", "it-create-"+suffix, map[string]any{
		"name":          nodeName,
		"instance_type": "local-small",
	}, &created); err != nil {
		return fmt.Errorf("API create node: %w", err)
	}
	if created.ID == "" {
		return fmt.Errorf("API create node returned no id")
	}
	// Always clean up the one owned node, even on an assertion failure.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var ignored roundTripNode
		_ = api.call(cleanup, http.MethodDelete, "/api/cloud-runtime/nodes", "it-cleanup-"+suffix, map[string]any{"instance_id": created.ID}, &ignored)
	}()

	// Daemon register/ready: the node container's real daemon connects back and
	// reports its Claude runtime. Readiness is never fabricated from SQL.
	ready, err := api.awaitNode(ctx, created.ID, func(n roundTripNode) bool { return n.Ready }, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("daemon register/ready: %w", err)
	}
	if ready.InstanceID == "" {
		return fmt.Errorf("ready node %s has no container instance id", ready.ID)
	}
	if !approvedNodeImage.MatchString(ready.ImageID) {
		return fmt.Errorf("node image %q is not digest pinned", ready.ImageID)
	}
	if ready.ImageID != env.image {
		return fmt.Errorf("node image %q is not the approved test image %q", ready.ImageID, env.image)
	}
	createdInfo, err := inspectNodeContainer(ctx, dockerCLI, ready.InstanceID)
	if err != nil {
		return fmt.Errorf("inspect created container: %w", err)
	}
	if createdInfo.ID != ready.InstanceID || createdInfo.State == nil || createdInfo.State.Status != "running" {
		return fmt.Errorf("created container %s is not the running node identity", ready.InstanceID)
	}

	// Resolve the node's registered Runtime through the test-owned database
	// boundary, then bind it to an agent through the real API.
	var daemonID string
	fixture.QueryRow(t, "SELECT COALESCE(daemon_id::text, '') FROM fleet_nodes WHERE id = $1", ready.ID).Scan(&daemonID)
	if daemonID == "" {
		return fmt.Errorf("node %s has no registered daemon id", ready.ID)
	}
	// The daemon syncs every workspace its owner can reach, so the runtime must be
	// resolved inside the workspace the API requests are scoped to.
	var runtimeID string
	fixture.QueryRow(t, "SELECT id FROM agent_runtime WHERE daemon_id = $1 AND workspace_id = $2", daemonID, env.workspaceID).Scan(&runtimeID)
	if runtimeID == "" {
		return fmt.Errorf("daemon %s registered no runtime in workspace %s", daemonID, env.workspaceID)
	}

	var agent struct {
		ID string "json:\"id\""
	}
	if err := api.call(ctx, http.MethodPost, "/api/agents", "", map[string]any{
		"name":       agentName,
		"runtime_id": runtimeID,
	}, &agent); err != nil {
		return fmt.Errorf("API bind Runtime to agent: %w", err)
	}
	if agent.ID == "" {
		return fmt.Errorf("API bind Runtime returned no agent id")
	}

	var session struct {
		ID string "json:\"id\""
	}
	if err := api.call(ctx, http.MethodPost, "/api/chat/sessions", "", map[string]any{"agent_id": agent.ID}, &session); err != nil {
		return fmt.Errorf("API create chat session: %w", err)
	}
	if session.ID == "" {
		return fmt.Errorf("API create chat session returned no id")
	}

	var sent struct {
		TaskID string "json:\"task_id\""
	}
	if err := api.call(ctx, http.MethodPost, "/api/chat/sessions/"+session.ID+"/messages", "", map[string]any{"content": marker}, &sent); err != nil {
		return fmt.Errorf("API send chat message: %w", err)
	}
	if sent.TaskID == "" {
		return fmt.Errorf("API send chat message returned no task id")
	}
	// The user's own message contains `marker`, so matching it would be a
	// vacuous assertion. Only the fake CLI result proves the task executed.
	if err := api.awaitChatReply(ctx, session.ID, "fake Claude completed", 5*time.Minute); err != nil {
		return fmt.Errorf("poll completed messages: %w", err)
	}

	// Stop and start through the local lifecycle path. The same Idempotency-Key
	// is replayed to prove the accepted maintenance operation is not re-applied.
	var stopped roundTripNode
	stopKey := "it-stop-" + suffix
	if err := api.call(ctx, http.MethodPost, "/api/cloud-runtime/nodes/stop", stopKey, map[string]any{"instance_id": ready.ID}, &stopped); err != nil {
		return fmt.Errorf("API stop node: %w", err)
	}
	var replayed roundTripNode
	if err := api.call(ctx, http.MethodPost, "/api/cloud-runtime/nodes/stop", stopKey, map[string]any{"instance_id": ready.ID}, &replayed); err != nil {
		return fmt.Errorf("API replay stop node: %w", err)
	}
	if stopped.OperationID == "" || replayed.OperationID != stopped.OperationID {
		return fmt.Errorf("stop replay operation = %q, want the original %q", replayed.OperationID, stopped.OperationID)
	}
	if _, err := api.awaitNode(ctx, ready.ID, func(n roundTripNode) bool { return n.Status == "stopped" }, 3*time.Minute); err != nil {
		return fmt.Errorf("await stopped: %w", err)
	}
	stoppedInfo, err := inspectNodeContainer(ctx, dockerCLI, ready.InstanceID)
	if err != nil {
		return fmt.Errorf("inspect stopped container: %w", err)
	}
	if stoppedInfo.State == nil || stoppedInfo.State.Status != "exited" {
		return fmt.Errorf("stopped container state = %+v, want exited", stoppedInfo.State)
	}

	var started roundTripNode
	if err := api.call(ctx, http.MethodPost, "/api/cloud-runtime/nodes/start", "it-start-"+suffix, map[string]any{"instance_id": ready.ID}, &started); err != nil {
		return fmt.Errorf("API start node: %w", err)
	}
	if _, err := api.awaitNode(ctx, ready.ID, func(n roundTripNode) bool { return n.Ready }, 3*time.Minute); err != nil {
		return fmt.Errorf("await restarted node: %w", err)
	}
	startedInfo, err := inspectNodeContainer(ctx, dockerCLI, ready.InstanceID)
	if err != nil {
		return fmt.Errorf("inspect started container: %w", err)
	}
	if err := assertIdentityPreserved(createdInfo, startedInfo); err != nil {
		return fmt.Errorf("start identity/volumes: %w", err)
	}

	// Fleet restart: trigger the managed-environment restart hook, then require
	// the same node to become ready again. This is an external state change, not
	// a sleep.
	if err := postRestart(ctx, env.restartURL); err != nil {
		return fmt.Errorf("restart Fleet: %w", err)
	}
	if _, err := api.awaitNode(ctx, ready.ID, func(n roundTripNode) bool { return n.Ready }, 5*time.Minute); err != nil {
		return fmt.Errorf("await node after Fleet restart: %w", err)
	}

	// Approved-maintenance crash boundary: a stop accepted before the restart
	// must still be at-most-once after it. Replaying the original key must not
	// mint a new operation or move the container identity.
	var crashStop roundTripNode
	crashKey := "it-crash-stop-" + suffix
	if err := api.call(ctx, http.MethodPost, "/api/cloud-runtime/nodes/stop", crashKey, map[string]any{"instance_id": ready.ID}, &crashStop); err != nil {
		return fmt.Errorf("API crash-boundary stop: %w", err)
	}
	var crashReplay roundTripNode
	if err := api.call(ctx, http.MethodPost, "/api/cloud-runtime/nodes/stop", crashKey, map[string]any{"instance_id": ready.ID}, &crashReplay); err != nil {
		return fmt.Errorf("API crash-boundary replay: %w", err)
	}
	if crashStop.OperationID == "" || crashReplay.OperationID != crashStop.OperationID {
		return fmt.Errorf("crash-boundary replay operation = %q, want %q", crashReplay.OperationID, crashStop.OperationID)
	}
	postCrashInfo, err := inspectNodeContainer(ctx, dockerCLI, ready.InstanceID)
	if err != nil {
		return fmt.Errorf("inspect after crash boundary: %w", err)
	}
	if err := assertIdentityPreserved(createdInfo, postCrashInfo); err != nil {
		return fmt.Errorf("crash-boundary identity/volumes: %w", err)
	}
	// The stop barrier stays up until the reconciler records completion; deleting
	// during that window is refused as a conflict by design.
	if _, err := api.awaitNode(ctx, ready.ID, func(n roundTripNode) bool { return n.Status == "stopped" }, 3*time.Minute); err != nil {
		return fmt.Errorf("await crash-boundary stopped: %w", err)
	}

	// Deletion refuses queued or deferred runs by design, so cancel any
	// follow-up chat work the completed turn left behind before deleting.
	if err := api.call(ctx, http.MethodDelete, "/api/chat/sessions/"+session.ID+"/queued-tasks", "", nil, nil); err != nil {
		return fmt.Errorf("API clear queued chat tasks: %w", err)
	}

	// Delete the owned node and prove the node token/credentials were revoked and
	// the Runtime is no longer online. The durable rows are the invalidation
	// proof; the deleted node is also absent from the authenticated list.
	if err := api.call(ctx, http.MethodDelete, "/api/cloud-runtime/nodes", "it-delete-"+suffix, map[string]any{"instance_id": ready.ID}, nil); err != nil {
		return fmt.Errorf("API delete node: %w", err)
	}
	if err := api.awaitNodeGone(ctx, ready.ID, 5*time.Minute); err != nil {
		return fmt.Errorf("await node deletion: %w", err)
	}
	var revoked int
	fixture.QueryRow(t, "SELECT count(*) FROM fleet_node_credentials WHERE node_id = $1 AND revoked_at IS NOT NULL", ready.ID).Scan(&revoked)
	if revoked == 0 {
		return fmt.Errorf("node %s token was not invalidated after delete", ready.ID)
	}
	var online int
	fixture.QueryRow(t, "SELECT count(*) FROM agent_runtime WHERE daemon_id = $1 AND status = 'online'", daemonID).Scan(&online)
	if online != 0 {
		return fmt.Errorf("runtime for daemon %s is still online after delete", daemonID)
	}
	return nil
}

// postRestart triggers the operator-owned Fleet restart hook. It is an explicit
// external input, never an internal server replacement.
func postRestart(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("restart hook status %d", resp.StatusCode)
	}
	return nil
}
