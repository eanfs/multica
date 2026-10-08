package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/operator"
)

// This file owns the composition-level recovery regressions named by Task 14:
// a stopped/missing container plus another namespace's failed report must
// refuse volume deletion, a wrong/unparseable mount observation must be
// unknown rather than an empty state, zero proof followed by same-operation
// cleanup, and the Linux shared-PG internal URL projection plus the dynamic
// API ExtraHosts value.
//
// The provider composition is exercised against a deterministic Engine double,
// so the default suite compiles and runs without a Docker socket. The canonical
// parsing/policy matrices already live in the docker and store packages; this
// file only asserts the named regressions and never duplicates those matrices.

const (
	recoveryNamespace = "recovery-namespace"
	recoveryFleetID   = "recovery-fleet"
	recoveryDigest    = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	recoveryImage     = "ghcr.io/eanfs/multica-runtime@" + recoveryDigest
)

func recoveryUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func recoveryUUIDString(v pgtype.UUID) string { return uuid.UUID(v.Bytes).String() }

func recoverySpec() model.Spec {
	return model.Spec{CPUs: 2, MemoryBytes: 4 << 30, Pids: 256, MaxRuns: 1}
}

func recoveryConfig() model.Config {
	return model.Config{
		Namespace: recoveryNamespace,
		FleetID:   recoveryFleetID,
		Image:     recoveryImage,
		APIURL:    "http://127.0.0.1:8090",
		Specs:     map[string]model.Spec{"local-small": recoverySpec()},
		MaxNodes:  2,
	}
}

// recoveryNode is a terminally deleting node with a durable operation ref. It
// is a valid provider input but never reaches Docker.
func recoveryNode(containerID string) model.Node {
	id := recoveryUUID()
	return model.Node{
		ID:            id,
		OwnerID:       recoveryUUID(),
		Namespace:     recoveryNamespace,
		ContainerID:   containerID,
		DaemonID:      uuid.NewString(),
		Name:          "recovery-node",
		Spec:          "local-small",
		Image:         recoveryImage,
		ProfileRef:    "/run/secrets/recovery-profile.json",
		DataVolume:    "recovery-data-" + recoveryUUIDString(id),
		SecretsVolume: "recovery-secrets-" + recoveryUUIDString(id),
		Desired:       "terminating",
		Status:        "stopped",
		Generation:    7,
		Maintenance:   true,
		Revoked:       true,
		Resources:     recoverySpec(),
	}
}

func recoveryRef(n model.Node) model.OperationRef {
	return model.OperationRef{
		Namespace:   n.Namespace,
		NodeID:      n.ID,
		OperationID: recoveryUUID(),
		Generation:  n.Generation,
		Action:      model.Delete,
	}
}

// recoveryLabels are the exact node ownership labels inspectOwned requires.
func recoveryLabels(n model.Node) map[string]string {
	return map[string]string{
		"multica.fleet.namespace": n.Namespace,
		"multica.fleet.fleet_id":  recoveryFleetID,
		"multica.fleet.node":      recoveryUUIDString(n.ID),
		"multica.fleet.role":      "node",
	}
}

// recoveryOffline builds the exact two-field offline wire the Engine helper
// emits, so parseOffline runs its real strict schema and manifest identity
// checks. A foreign namespace or non-zero report queue is therefore rejected by
// the production parser, not by the double.
func recoveryOffline(t *testing.T, n model.Node, namespace string, pending, failed int) []byte {
	t.Helper()
	manifest := model.LayoutManifestData{
		Version:        model.LayoutVersion,
		Namespace:      namespace,
		FleetID:        recoveryFleetID,
		NodeID:         recoveryUUIDString(n.ID),
		DaemonID:       n.DaemonID,
		DataMount:      model.DataMount,
		NodeHome:       model.NodeHome,
		WorkspacesRoot: model.WorkspacesRoot,
	}
	wire := struct {
		Manifest json.RawMessage `json:"manifest"`
		Reports  struct {
			Known   bool `json:"known"`
			Pending int  `json:"pending"`
			Failed  int  `json:"failed"`
		} `json:"report_queue_stats"`
	}{}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	wire.Manifest = raw
	wire.Reports.Known = true
	wire.Reports.Pending = pending
	wire.Reports.Failed = failed
	out, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal offline wire: %v", err)
	}
	return out
}

// fakeRecoveryEngine is the deterministic Engine double. It records only the
// removal calls so the tests can prove the provider refused or ordered them.
type fakeRecoveryEngine struct {
	inspect func(string) (docker.Inspection, error)
	offline func(model.Node, model.OperationRef) ([]byte, error)
	health  []byte
	labels  map[string]string

	removed []string
	volumes []docker.Resource
	started []string
}

func (f *fakeRecoveryEngine) Find(context.Context, map[string]string) ([]docker.Resource, error) {
	return nil, nil
}

func (f *fakeRecoveryEngine) Inspect(_ context.Context, id string) (docker.Inspection, error) {
	if f.inspect != nil {
		insp, err := f.inspect(id)
		if err != nil {
			return insp, err
		}
		if insp.Labels == nil {
			insp.Labels = f.labels
		}
		return insp, nil
	}
	return docker.Inspection{}, errdefs.ErrNotFound
}

func (f *fakeRecoveryEngine) EnsureNetwork(context.Context, docker.Resource) error { return nil }
func (f *fakeRecoveryEngine) ConnectNetwork(context.Context, string, string, []string) error {
	return nil
}
func (f *fakeRecoveryEngine) RemoveNetwork(context.Context, docker.Resource) error { return nil }
func (f *fakeRecoveryEngine) EnsureVolume(context.Context, docker.Resource) error  { return nil }

func (f *fakeRecoveryEngine) Create(context.Context, *container.Config, *container.HostConfig, string, string) (string, error) {
	return "", nil
}

func (f *fakeRecoveryEngine) InstallBootstrap(context.Context, []docker.Resource, []byte) error {
	return nil
}

func (f *fakeRecoveryEngine) Start(_ context.Context, id string) error {
	f.started = append(f.started, id)
	return nil
}

func (f *fakeRecoveryEngine) Stop(context.Context, string) error { return nil }

func (f *fakeRecoveryEngine) Remove(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeRecoveryEngine) RemoveVolume(_ context.Context, r docker.Resource) error {
	f.volumes = append(f.volumes, r)
	return nil
}

func (f *fakeRecoveryEngine) FixedHealth(context.Context, string) ([]byte, error) {
	if f.health == nil {
		return []byte("{}"), nil
	}
	return f.health, nil
}

func (f *fakeRecoveryEngine) FixedOfflineReports(_ context.Context, n model.Node, ref model.OperationRef) ([]byte, error) {
	if f.offline != nil {
		return f.offline(n, ref)
	}
	return nil, errdefs.ErrNotFound
}

func recoveryProvider(engine docker.Engine) *docker.Provider {
	return docker.New(engine, recoveryConfig())
}

func stoppedInspection(id string) (docker.Inspection, error) {
	return docker.Inspection{ID: id, State: "exited"}, nil
}

func runningInspection(id string) (docker.Inspection, error) {
	return docker.Inspection{ID: id, State: "running", StartedAt: "2026-01-01T00:00:00Z"}, nil
}

// TestRecoveryDeleteRefusesWhileFailedReport proves a stopped container with a
// non-zero failed report queue is ErrBusy and never removes a volume, so a
// lost/failed report keeps the recovery inputs.
func TestRecoveryDeleteRefusesWhileFailedReport(t *testing.T) {
	n := recoveryNode("recovery-container-failed")
	engine := &fakeRecoveryEngine{
		inspect: stoppedInspection,
		labels:  recoveryLabels(n),
		offline: func(model.Node, model.OperationRef) ([]byte, error) {
			return recoveryOffline(t, n, n.Namespace, 0, 1), nil
		},
	}
	err := recoveryProvider(engine).Delete(context.Background(), n, recoveryRef(n))
	if !errors.Is(err, model.ErrBusy) {
		t.Fatalf("Delete() = %v, want ErrBusy while a failed report exists", err)
	}
	if len(engine.volumes) != 0 {
		t.Fatalf("Delete() removed %d volume(s) with a failed report, want none", len(engine.volumes))
	}
}

// TestRecoveryDeleteRefusesForeignNamespaceReport proves a report whose layout
// manifest belongs to a different namespace is unknown, not deletion proof.
func TestRecoveryDeleteRefusesForeignNamespaceReport(t *testing.T) {
	n := recoveryNode("recovery-container-foreign")
	engine := &fakeRecoveryEngine{
		inspect: stoppedInspection,
		labels:  recoveryLabels(n),
		offline: func(model.Node, model.OperationRef) ([]byte, error) {
			return recoveryOffline(t, n, "another-namespace", 0, 0), nil
		},
	}
	err := recoveryProvider(engine).Delete(context.Background(), n, recoveryRef(n))
	if !errors.Is(err, model.ErrUnknownHealth) {
		t.Fatalf("Delete() = %v, want ErrUnknownHealth for a foreign-namespace report", err)
	}
	if len(engine.volumes) != 0 {
		t.Fatalf("Delete() removed %d volume(s) from a foreign report, want none", len(engine.volumes))
	}
}

// TestRecoveryZeroProofThenSameOperationCleanup proves a zero, known report
// lets the same Delete operation remove the container and then the secrets and
// data volumes in that order.
func TestRecoveryZeroProofThenSameOperationCleanup(t *testing.T) {
	n := recoveryNode("recovery-container-zero")
	engine := &fakeRecoveryEngine{
		inspect: stoppedInspection,
		labels:  recoveryLabels(n),
		offline: func(model.Node, model.OperationRef) ([]byte, error) {
			return recoveryOffline(t, n, n.Namespace, 0, 0), nil
		},
	}
	if err := recoveryProvider(engine).Delete(context.Background(), n, recoveryRef(n)); err != nil {
		t.Fatalf("Delete() = %v, want nil with zero proof", err)
	}
	if len(engine.removed) != 1 || engine.removed[0] != n.ContainerID {
		t.Fatalf("removed containers = %v, want [%s]", engine.removed, n.ContainerID)
	}
	if len(engine.volumes) != 2 {
		t.Fatalf("removed volumes = %d, want 2 (secrets then data)", len(engine.volumes))
	}
	if engine.volumes[0].Role != "secrets" || engine.volumes[1].Role != "data" {
		t.Fatalf("removed volume order = [%s %s], want [secrets data]", engine.volumes[0].Role, engine.volumes[1].Role)
	}
	if engine.volumes[1].ID != n.DataVolume {
		t.Fatalf("data volume removed = %q, want %q", engine.volumes[1].ID, n.DataVolume)
	}
}

// TestRecoveryMissingContainerUsesReportProof proves a genuinely missing
// container is still gated by a fresh zero report before any volume removal.
func TestRecoveryMissingContainerUsesReportProof(t *testing.T) {
	n := recoveryNode("recovery-container-missing")
	engine := &fakeRecoveryEngine{
		inspect: func(string) (docker.Inspection, error) { return docker.Inspection{}, errdefs.ErrNotFound },
		offline: func(model.Node, model.OperationRef) ([]byte, error) {
			return recoveryOffline(t, n, n.Namespace, 0, 0), nil
		},
	}
	if err := recoveryProvider(engine).Delete(context.Background(), n, recoveryRef(n)); err != nil {
		t.Fatalf("Delete() = %v, want nil for a missing container with zero proof", err)
	}
	if len(engine.removed) != 0 {
		t.Fatalf("removed containers = %v, want none for an already-missing container", engine.removed)
	}
	if len(engine.volumes) != 2 {
		t.Fatalf("removed volumes = %d, want 2 (secrets then data)", len(engine.volumes))
	}
}

// TestRecoveryWrongMountIsUnknownNotEmpty proves that a running container whose
// native mount/health proof cannot be read yields a non-empty unknown
// observation. An empty Observation would let callers mistake "not proven" for
// a definitive stopped/clean state.
func TestRecoveryWrongMountIsUnknownNotEmpty(t *testing.T) {
	n := recoveryNode("recovery-container-wrongmount")
	engine := &fakeRecoveryEngine{inspect: runningInspection, labels: recoveryLabels(n), health: []byte("{}")}
	o, err := recoveryProvider(engine).Inspect(context.Background(), n)
	if err != nil {
		t.Fatalf("Inspect() = %v, want a non-empty unknown observation", err)
	}
	if o.ContainerID != n.ContainerID || o.Status != "running" || o.Ready {
		t.Fatalf("Inspect() = %+v, want running non-empty unknown for the observed container", o)
	}
}

// TestRecoveryLinuxHostGatewayExtraHosts pins the dynamic API ExtraHosts value
// used so a Linux node container can reach the API through the host gateway.
func TestRecoveryLinuxHostGatewayExtraHosts(t *testing.T) {
	want := []string{"host.docker.internal:host-gateway"}
	if got := docker.NodeHostConfig(recoverySpec(), true, nil).ExtraHosts; !reflect.DeepEqual(got, want) {
		t.Fatalf("NodeHostConfig(..., true).ExtraHosts = %v, want %v", got, want)
	}
	if got := docker.NodeHostConfig(recoverySpec(), false, nil).ExtraHosts; len(got) != 0 {
		t.Fatalf("NodeHostConfig(..., false).ExtraHosts = %v, want none", got)
	}
}

// TestRecoverySharedPGInternalURLProjection proves the operator accepts a
// shared-PostgreSQL internal-network URL only in the hermetic no-HOME
// environment the managed launcher projects for the Fleet child, and rejects
// that same URL once HOME would let pgx discover a local default.
func TestRecoverySharedPGInternalURLProjection(t *testing.T) {
	// localDatabaseURI refuses any inherited PG* setting or a resolvable HOME;
	// scrub both so the assertion is about the URL projection, not the host.
	t.Setenv("HOME", "")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
		}
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "fleet-config.json")
	secretPath := filepath.Join(dir, "service-key")
	profilesPath := filepath.Join(dir, "profiles.json")
	writePrivate(t, configPath, `{"namespace":"recovery-namespace","fleet_id":"recovery-fleet","image":"`+recoveryImage+`","api_url":"http://127.0.0.1:8090","max_nodes":2,"specs":{"local-small":{"cpus":2,"memory_bytes":4294967296,"pids":256,"max_runs":1}}}`)
	writePrivate(t, secretPath, strings.Repeat("s", 32))
	writePrivate(t, profilesPath, `{"version":1,"owners":{}}`)

	cfg := operator.Config{
		Namespace:      recoveryNamespace,
		FleetID:        recoveryFleetID,
		FleetURL:       "http://127.0.0.1:8090",
		DatabaseURL:    "postgres://multica:secret@shared-postgres:5432/multica?sslmode=disable",
		ConfigFile:     configPath,
		ServiceKeyFile: secretPath,
		ProfilesFile:   profilesPath,
		OperationKey:   "recovery-operation",
		Timeout:        5 * time.Second,
	}
	if err := operator.Validate("status", cfg); err != nil {
		t.Fatalf("Validate(status) with an internal shared-PG URL = %v, want nil", err)
	}

	t.Setenv("HOME", dir)
	if err := operator.Validate("status", cfg); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("Validate(status) with HOME set = %v, want ErrInvalidRequest", err)
	}
}

func writePrivate(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
