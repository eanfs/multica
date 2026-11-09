package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func serviceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

type prepareCall struct {
	dataVolume    string
	secretsVolume string
	image         string
}

// fakeDocker records provider calls so tests can assert both what reached
// Docker and in which order. It never talks to a daemon.
type fakeDocker struct {
	mu sync.Mutex

	pingErr         error
	imageErr        error
	networkErr      error
	volumeErr       error
	prepareErr      error
	createErr       error
	startErr        error
	stopErr         error
	restartErr      error
	removeErr       error
	removeVolumeErr error
	inspectErr      error
	healthErr       error

	containerID string
	health      []byte
	inspected   map[string]docker.ObservedContainer

	calls          []string
	created        []docker.NodeSpec
	prepareCalls   []prepareCall
	prepared       map[string][]byte
	started        []string
	stopped        []string
	restarted      []string
	removed        []string
	removedVolumes []string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containerID: "container-1",
		health:      []byte("{\"status\":\"running\",\"agents\":[\"claude\"]}"),
		inspected:   map[string]docker.ObservedContainer{},
		prepared:    map[string][]byte{},
	}
}

func (f *fakeDocker) recordLocked(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeDocker) callIndex(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, got := range f.calls {
		if got == call {
			return i
		}
	}
	return -1
}

func (f *fakeDocker) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("ping")
	return f.pingErr
}

func (f *fakeDocker) InspectImage(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("inspect-image")
	return f.imageErr
}

func (f *fakeDocker) EnsureNetwork(context.Context, string, map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("ensure-network")
	return f.networkErr
}

func (f *fakeDocker) EnsureVolume(context.Context, string, map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("ensure-volume")
	return f.volumeErr
}

func (f *fakeDocker) PrepareVolumes(_ context.Context, dataVolume, secretsVolume, image string, files map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("prepare")
	f.prepareCalls = append(f.prepareCalls, prepareCall{dataVolume: dataVolume, secretsVolume: secretsVolume, image: image})
	copied := make(map[string][]byte, len(files))
	for name, body := range files {
		copied[name] = append([]byte(nil), body...)
	}
	f.prepared = copied
	return f.prepareErr
}

func (f *fakeDocker) CreateContainer(_ context.Context, spec docker.NodeSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("create")
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created = append(f.created, spec)
	return f.containerID, nil
}

func (f *fakeDocker) Start(_ context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("start")
	if f.startErr != nil {
		return f.startErr
	}
	f.started = append(f.started, containerID)
	return nil
}

func (f *fakeDocker) Stop(_ context.Context, containerID string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("stop")
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, containerID)
	return nil
}

func (f *fakeDocker) Restart(_ context.Context, containerID string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("restart")
	if f.restartErr != nil {
		return f.restartErr
	}
	f.restarted = append(f.restarted, containerID)
	return nil
}

func (f *fakeDocker) Remove(_ context.Context, containerID string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("remove")
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, containerID)
	return nil
}

func (f *fakeDocker) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("remove-volume")
	if f.removeVolumeErr != nil {
		return f.removeVolumeErr
	}
	f.removedVolumes = append(f.removedVolumes, name)
	return nil
}

func (f *fakeDocker) Inspect(_ context.Context, containerID string) (docker.ObservedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("inspect")
	if f.inspectErr != nil {
		return docker.ObservedContainer{}, f.inspectErr
	}
	observed, ok := f.inspected[containerID]
	if !ok {
		return docker.ObservedContainer{}, errors.New("container not found")
	}
	return observed, nil
}

func (f *fakeDocker) ListByLabels(context.Context, map[string]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("list")
	return nil, nil
}

func (f *fakeDocker) ExecHealth(_ context.Context, containerID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordLocked("exec-health")
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return f.health, nil
}

func (f *fakeDocker) Close() error { return nil }

func serviceTestConfig() Config {
	return Config{
		Namespace:        "wt-svc",
		RuntimeImage:     "multica/runtime-claude:test",
		APIURL:           "http://api.test:8080",
		NodeNetwork:      "wt-svc-fleet",
		MaxNodesPerOwner: 2,
		Specs: []Spec{{
			ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256, MaxConcurrent: 1,
		}},
	}
}

func serviceTestCredentials(owner string) *Credentials {
	return &Credentials{
		users:    map[string]string{owner: "default"},
		profiles: map[string]Profile{"default": {Env: map[string]string{"ANTHROPIC_API_KEY": "sk-test"}}},
	}
}

func testOwner(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{b}, Valid: true}
}

func testUUIDString(id pgtype.UUID) string { return store.UUIDText(id) }

func seedServiceNode(t *testing.T, st *store.Store, owner pgtype.UUID, name, containerID string) db.FleetNode {
	t.Helper()
	node, err := st.Queries().CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID:       owner,
		Namespace:     "wt-svc",
		Name:          name,
		ContainerID:   containerID,
		ContainerName: name + "-container",
		DaemonID:      name + "-daemon",
		Image:         "multica/runtime-claude:test",
		Spec:          []byte("{\"id\":\"standard\"}"),
		TargetState:   StateRunning,
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return node
}

func TestCreateNodeRejectsUnknownSpecBeforeSideEffects(t *testing.T) {
	svc := NewService(serviceTestConfig(), nil, nil, nil, "secret")
	_, err := svc.CreateNode(context.Background(), testUUIDString(testOwner(1)), CreateNodeRequest{Name: "n", SpecID: "nope"})
	if !errors.Is(err, ErrSpecNotAllowed) {
		t.Fatalf("err = %v, want ErrSpecNotAllowed", err)
	}
}

func TestCreateNodeRejectsOwnerWithoutCredentialProfile(t *testing.T) {
	creds := &Credentials{users: map[string]string{}, profiles: map[string]Profile{}}
	svc := NewService(serviceTestConfig(), nil, nil, creds, "secret")
	_, err := svc.CreateNode(context.Background(), testUUIDString(testOwner(2)), CreateNodeRequest{Name: "n", SpecID: "standard"})
	if !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("err = %v, want ErrProfileMissing", err)
	}
}

func TestBuildCLIConfigWritesTokenAndURLs(t *testing.T) {
	raw := buildCLIConfig("http://127.0.0.1:8080", "http://127.0.0.1:3000", "mcn_abcdef")
	var cfg struct {
		ServerURL      string `json:"server_url"`
		AppURL         string `json:"app_url"`
		Token          string `json:"token"`
		WorkspacesRoot string `json:"workspaces_root"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v (%s)", err, raw)
	}
	if cfg.ServerURL != "http://127.0.0.1:8080" || cfg.AppURL != "http://127.0.0.1:3000" {
		t.Fatalf("urls = %q/%q", cfg.ServerURL, cfg.AppURL)
	}
	if cfg.Token != "mcn_abcdef" {
		t.Fatalf("token = %q, want the node token", cfg.Token)
	}
	if cfg.WorkspacesRoot != "/home/multica/workspaces" {
		t.Fatalf("workspaces_root = %q", cfg.WorkspacesRoot)
	}
	if strings.Contains(raw, "ANTHROPIC") {
		t.Fatalf("model credentials must not be in the CLI config: %s", raw)
	}
}

func TestBuildEnvFileSortsKeys(t *testing.T) {
	got := buildEnvFile(map[string]string{"B": "2", "A": "1"})
	if got != "A=1\nB=2\n" {
		t.Fatalf("env file = %q", got)
	}
}

func TestSanitizeBoundsError(t *testing.T) {
	if got := sanitize(nil); got != "" {
		t.Fatalf("sanitize(nil) = %q", got)
	}
	long := strings.Repeat("x", 400)
	got := sanitize(errors.New(long))
	if len(got) != 300 {
		t.Fatalf("sanitized length = %d, want 300", len(got))
	}
}

func TestDisplayStatusReportsMissingInstance(t *testing.T) {
	svc := NewService(Config{}, nil, nil, nil, "secret")
	node := db.FleetNode{
		ContainerID:   "container-1",
		TargetState:   StateRunning,
		ObservedState: StateInstanceMissing,
	}
	if got := svc.displayStatus(node); got != StateInstanceMissing {
		t.Fatalf("display status = %q, want %q", got, StateInstanceMissing)
	}
}

func TestDiagnoseParsesHealthReport(t *testing.T) {
	provider := newFakeDocker()
	provider.health = []byte("{\"status\":\"running\",\"pending_terminal_report_count\":2,\"agents\":[\"claude\"]}")
	svc := NewService(Config{}, nil, provider, nil, "secret")
	report, err := svc.Diagnose(context.Background(), db.FleetNode{ContainerID: "container-1"})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if report.Status != "running" || report.PendingTerminalReportCount != 2 || len(report.Agents) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestCreateNodePreparesVolumesBeforeContainerAndStoresToken(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(11)
	provider := newFakeDocker()
	svc := NewService(serviceTestConfig(), st, provider, serviceTestCredentials(testUUIDString(owner)), "secret")

	node, err := svc.CreateNode(context.Background(), testUUIDString(owner), CreateNodeRequest{
		Name: "node-a", SpecID: "standard", IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if node.ContainerID != provider.containerID || node.TargetState != StateLaunching {
		t.Fatalf("unexpected node: %+v", node)
	}
	if node.IdempotencyKey != "idem-1" {
		t.Fatalf("idempotency key = %q", node.IdempotencyKey)
	}

	prepareIndex, createIndex := provider.callIndex("prepare"), provider.callIndex("create")
	if prepareIndex < 0 || createIndex < 0 {
		t.Fatalf("missing provider calls: %v", provider.calls)
	}
	if prepareIndex > createIndex {
		t.Fatalf("PrepareVolumes must run before CreateContainer: %v", provider.calls)
	}

	provider.mu.Lock()
	files := make(map[string][]byte, len(provider.prepared))
	for name, body := range provider.prepared {
		files[name] = append([]byte(nil), body...)
	}
	created := append([]docker.NodeSpec(nil), provider.created...)
	provider.mu.Unlock()

	if len(created) != 1 {
		t.Fatalf("created containers = %d, want 1", len(created))
	}
	spec := created[0]
	if spec.Image != serviceTestConfig().RuntimeImage || spec.Network != serviceTestConfig().NodeNetwork {
		t.Fatalf("unexpected container spec: %+v", spec)
	}
	if !containsString(spec.Env, "MULTICA_SERVER_URL="+serviceTestConfig().APIURL) {
		t.Fatalf("missing server URL env: %v", spec.Env)
	}
	if containsSubstring(spec.Env, "sk-test") {
		t.Fatalf("model credentials leaked into container env: %v", spec.Env)
	}

	var cli struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(files["config.json"], &cli); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if !strings.HasPrefix(cli.Token, auth.CloudPATPrefix) {
		t.Fatalf("config token = %q, want mcn_ token", cli.Token)
	}
	if !strings.Contains(string(files["env"]), "ANTHROPIC_API_KEY=sk-test") {
		t.Fatalf("model env not seeded: %q", files["env"])
	}

	credential, err := st.Queries().GetFleetNodeCredentialByHash(context.Background(), auth.HashToken(cli.Token))
	if err != nil {
		t.Fatalf("node credential not stored: %v", err)
	}
	if credential.NodeID != node.ID {
		t.Fatalf("credential node = %v, want %v", credential.NodeID, node.ID)
	}
}

func TestCreateNodeCleansUpAndMarksFailed(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(12)
	provider := newFakeDocker()
	provider.createErr = errors.New("docker create failed")
	svc := NewService(serviceTestConfig(), st, provider, serviceTestCredentials(testUUIDString(owner)), "secret")

	if _, err := svc.CreateNode(context.Background(), testUUIDString(owner), CreateNodeRequest{Name: "node-b", SpecID: "standard"}); err == nil {
		t.Fatal("create node succeeded despite create error")
	}

	nodes, err := svc.ListNodes(context.Background(), testUUIDString(owner), 10, 0)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if nodes[0].TargetState != StateFailed || nodes[0].Error == "" {
		t.Fatalf("failed node not recorded: %+v", nodes[0])
	}
	if nodes[0].ContainerID != "" {
		t.Fatalf("failed node kept container id %q", nodes[0].ContainerID)
	}

	provider.mu.Lock()
	removedVolumes := append([]string(nil), provider.removedVolumes...)
	files := map[string][]byte{}
	for name, body := range provider.prepared {
		files[name] = append([]byte(nil), body...)
	}
	provider.mu.Unlock()
	if len(removedVolumes) != 2 {
		t.Fatalf("removed volumes = %v, want two node volumes", removedVolumes)
	}

	var cli struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(files["config.json"], &cli); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if cli.Token == "" {
		t.Fatal("expected a minted token before the failed start")
	}
	if _, err := st.Queries().GetFleetNodeCredentialByHash(context.Background(), auth.HashToken(cli.Token)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("credential still active after failure: %v", err)
	}
}

func TestOperateRefreshesStatusFromDocker(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(13)
	seedServiceNode(t, st, owner, "node-status", "c-status")
	provider := newFakeDocker()
	provider.inspected["c-status"] = docker.ObservedContainer{ID: "c-status", State: "running", Healthy: true}
	svc := NewService(serviceTestConfig(), st, provider, nil, "secret")

	node, err := svc.Operate(context.Background(), testUUIDString(owner), "c-status", "status", "")
	if err != nil {
		t.Fatalf("operate status: %v", err)
	}
	if node.ObservedState != StateRunning || !node.DaemonReady {
		t.Fatalf("observed state = %q ready = %v", node.ObservedState, node.DaemonReady)
	}
}

func TestOperateStartStopRebootUpdatesTargetState(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	provider := newFakeDocker()
	svc := NewService(serviceTestConfig(), st, provider, nil, "secret")

	cases := []struct {
		operation string
		want      string
	}{
		{"start", StateStarting},
		{"stop", StateStopping},
		{"reboot", StateRebooting},
	}
	for i, tc := range cases {
		owner := testOwner(byte(20 + i))
		containerID := "c-" + tc.operation
		seedServiceNode(t, st, owner, "node-"+tc.operation, containerID)
		node, err := svc.Operate(context.Background(), testUUIDString(owner), containerID, tc.operation, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.operation, err)
		}
		if node.TargetState != tc.want {
			t.Fatalf("%s target = %q, want %q", tc.operation, node.TargetState, tc.want)
		}
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.started) != 1 || len(provider.stopped) != 1 || len(provider.restarted) != 1 {
		t.Fatalf("provider calls = start %v stop %v restart %v", provider.started, provider.stopped, provider.restarted)
	}
}

func TestOperateRejectsAnotherOwner(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(30)
	seedServiceNode(t, st, owner, "node-owner", "c-owner")
	provider := newFakeDocker()
	svc := NewService(serviceTestConfig(), st, provider, nil, "secret")

	_, err := svc.Operate(context.Background(), testUUIDString(testOwner(31)), "c-owner", "start", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(provider.started) != 0 {
		t.Fatalf("cross-owner operation reached Docker: %v", provider.started)
	}
}

func TestOperateDeleteRemovesResourcesAndRevokesToken(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(35)
	node := seedServiceNode(t, st, owner, "node-delete", "c-delete")
	token := "mcn_delete_token"
	if _, err := st.Queries().CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: auth.HashToken(token),
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	provider := newFakeDocker()
	svc := NewService(serviceTestConfig(), st, provider, nil, "secret")

	deleted, err := svc.Operate(context.Background(), testUUIDString(owner), "c-delete", "delete", "")
	if err != nil {
		t.Fatalf("operate delete: %v", err)
	}
	if deleted.TargetState != StateTerminated {
		t.Fatalf("target = %q, want %q", deleted.TargetState, StateTerminated)
	}
	if len(provider.removed) != 1 || provider.removed[0] != "c-delete" {
		t.Fatalf("removed containers = %v", provider.removed)
	}
	if len(provider.removedVolumes) != 2 {
		t.Fatalf("removed volumes = %v", provider.removedVolumes)
	}
	if _, err := st.Queries().GetFleetNodeCredentialByHash(context.Background(), auth.HashToken(token)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("credential still active after delete: %v", err)
	}
}

func TestVerifyPATReturnsNodeIdentity(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(40)
	node := seedServiceNode(t, st, owner, "node-pat", "c-pat")
	token := "mcn_test_node_token"
	if _, err := st.Queries().CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: auth.HashToken(token),
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	svc := NewService(serviceTestConfig(), st, newFakeDocker(), nil, "secret")

	identity, found, err := svc.VerifyPAT(context.Background(), token)
	if err != nil || !found {
		t.Fatalf("verify = %+v found %v err %v", identity, found, err)
	}
	if !identity.Valid || identity.OwnerID != testUUIDString(owner) || identity.InstanceID != "c-pat" || identity.InstanceRecordID != testUUIDString(node.ID) {
		t.Fatalf("unexpected identity: %+v", identity)
	}

	unknown, found, err := svc.VerifyPAT(context.Background(), "mcn_unknown")
	if err != nil || found || unknown.Valid {
		t.Fatalf("unknown token = %+v found %v err %v", unknown, found, err)
	}
}

func TestListNodesIsOwnerScoped(t *testing.T) {
	pool := serviceTestPool(t)
	st := store.New(pool)
	owner := testOwner(50)
	other := testOwner(51)
	seedServiceNode(t, st, owner, "node-a", "c-a")
	seedServiceNode(t, st, owner, "node-b", "c-b")
	seedServiceNode(t, st, other, "node-other", "c-other")
	svc := NewService(serviceTestConfig(), st, newFakeDocker(), nil, "secret")

	nodes, err := svc.ListNodes(context.Background(), testUUIDString(owner), 10, 0)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	for _, node := range nodes {
		if node.OwnerID != owner {
			t.Fatalf("listed foreign node: %+v", node)
		}
	}

	page, err := svc.ListNodes(context.Background(), testUUIDString(owner), 1, 0)
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("page = %d, want 1", len(page))
	}

	if _, err := svc.ListNodes(context.Background(), "not-a-uuid", 10, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid owner err = %v, want ErrInvalidRequest", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
