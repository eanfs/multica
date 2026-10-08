//go:build dockerintegration

package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// TestAuroraRoundTripEgressReplay is the real-engine crash-replay evidence Task
// 5's review carried into Task 7. It drives the unexported ensureEgress twice
// over a sidecar left running by the first admission and proves the replay
// adopts it without a restart, with the fixed "egress" alias still attached and
// admission not wedged. It runs only under the dockerintegration tag and the
// MULTICA_RUN_DOCKER_INTEGRATION=1 gate.
func TestAuroraRoundTripEgressReplay(t *testing.T) {
	if os.Getenv("MULTICA_RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("Docker opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		t.Skipf("Docker engine unavailable: %v", err)
	}

	cfg := auroraConfig()
	proxyImage, err := localImageDigestRef(ctx, cli, firstNonEmptyEnv(os.Getenv("MULTICA_AURORA_TEST_PROXY_IMAGE"), "multica-aurora-egress-fixture:arm64"))
	if err != nil {
		cli.Close()
		t.Skipf("Aurora egress fixture image unavailable: %v", err)
	}
	cfg.Aurora.ProxyImage = proxyImage
	cfg.Aurora.EgressHosts = nil
	cfg.Aurora.ProviderSecretFiles = map[string]string{}

	n := auroraNode()
	p := New(NewEngine(cli), cfg)

	uplink := cfg.Aurora.UplinkNetwork
	if _, err := cli.NetworkCreate(ctx, uplink, network.CreateOptions{Driver: "bridge", Labels: map[string]string{"aurora.it": n.Namespace}}); err != nil && !strings.Contains(err.Error(), "already exists") {
		cli.Close()
		t.Fatalf("create uplink network: %v", err)
	}
	t.Cleanup(func() {
		cleanupAuroraEgressResources(t, cli, n.Namespace, uplink)
		cli.Close()
	})

	workspace := p.workspaceNetwork(n)
	if err := p.engine.EnsureNetwork(ctx, workspace); err != nil {
		t.Fatalf("ensure workspace network: %v", err)
	}

	if err := p.ensureEgress(ctx, n, workspace); err != nil {
		t.Fatalf("first ensureEgress: %v", err)
	}
	name := p.egressName(n)
	before, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatalf("inspect egress sidecar: %v", err)
	}
	if before.State == nil || !before.State.Running {
		t.Fatalf("egress sidecar state = %+v, want running", before.State)
	}
	if !sidecarAliasAttached(before, workspace.Name, model.AuroraEgressAlias) {
		t.Fatalf("egress alias is not attached to %s: %+v", workspace.Name, before.NetworkSettings)
	}

	if err := p.ensureEgress(ctx, n, workspace); err != nil {
		t.Fatalf("replayed ensureEgress over the running sidecar: %v", err)
	}
	after, err := cli.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatalf("re-inspect egress sidecar: %v", err)
	}
	if after.State == nil || !after.State.Running {
		t.Fatalf("egress sidecar stopped after replay: %+v", after.State)
	}
	if after.State.StartedAt != before.State.StartedAt {
		t.Fatalf("egress sidecar restarted: %q -> %q", before.State.StartedAt, after.State.StartedAt)
	}
	if !sidecarAliasAttached(after, workspace.Name, model.AuroraEgressAlias) {
		t.Fatalf("egress alias lost after replay: %+v", after.NetworkSettings)
	}
}

func firstNonEmptyEnv(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// localImageDigestRef resolves a locally present image to an immutable
// name@sha256:<id> reference. Nothing is pulled or pushed.
func localImageDigestRef(ctx context.Context, cli *client.Client, ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		if _, _, err := cli.ImageInspectWithRaw(ctx, ref); err != nil {
			return "", err
		}
		return ref, nil
	}
	insp, _, err := cli.ImageInspectWithRaw(ctx, ref)
	if err != nil {
		return "", err
	}
	name := ref
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name = ref[:i]
	}
	return name + "@" + insp.ID, nil
}

func sidecarAliasAttached(info container.InspectResponse, networkName, alias string) bool {
	if info.NetworkSettings == nil {
		return false
	}
	endpoint := info.NetworkSettings.Networks[networkName]
	if endpoint == nil {
		return false
	}
	for _, a := range endpoint.Aliases {
		if a == alias {
			return true
		}
	}
	return false
}

// cleanupAuroraEgressResources removes only the resources this test labelled.
// It never prunes.
func cleanupAuroraEgressResources(t *testing.T, cli *client.Client, namespace, uplink string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, label := range []filters.Args{
		filters.NewArgs(filters.Arg("label", "multica.fleet.namespace="+namespace)),
		filters.NewArgs(filters.Arg("label", "aurora.it="+namespace)),
	} {
		containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: label})
		if err == nil {
			for _, c := range containers {
				_ = cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
			}
		}
		vols, verr := cli.VolumeList(ctx, volume.ListOptions{Filters: label})
		if verr == nil {
			for _, v := range vols.Volumes {
				_ = cli.VolumeRemove(ctx, v.Name, true)
			}
		}
		nets, nerr := cli.NetworkList(ctx, network.ListOptions{Filters: label})
		if nerr == nil {
			for _, nw := range nets {
				_ = cli.NetworkRemove(ctx, nw.ID)
			}
		}
	}
	_ = cli.NetworkRemove(ctx, uplink)
}

// TestAuroraBootstrapHelperLifecycleRealEngine drives the exact installer the
// Aurora provider uses (sdkEngine.InstallBootstrap -> runHelper ->
// cleanupHelper) against the real local Aurora node image. That image declares
// org.opencontainers.image.* labels, which Docker merges into every container,
// so the helper's actual label map is a strict superset of the four
// multica.fleet.* labels. Before the fix, cleanupHelper's exact DeepEqual
// rejected it, runHelper rewrote the successful bootstrap into ErrUnknownHealth,
// and the create operation failed. This asserts the helper is removed on success
// and that InstallBootstrap returns no error.
func TestAuroraBootstrapHelperLifecycleRealEngine(t *testing.T) {
	if os.Getenv("MULTICA_RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("Docker opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		t.Skipf("Docker engine unavailable: %v", err)
	}
	nodeRef := firstNonEmptyEnv(os.Getenv("MULTICA_AURORA_TEST_NODE_IMAGE"), "ghcr.io/eanfs/multica-aurora-sandbox@sha256:9e50d19d3352d5202679d35940e7dd48533f054423b9839d527c38fadaeed3da")
	digestRef, err := localImageDigestRef(ctx, cli, nodeRef)
	if err != nil {
		cli.Close()
		t.Skipf("Aurora node image unavailable: %v", err)
	}
	insp, _, err := cli.ImageInspectWithRaw(ctx, digestRef)
	if err != nil {
		cli.Close()
		t.Fatalf("inspect node image: %v", err)
	}
	if insp.Config == nil || insp.Config.Labels["org.opencontainers.image.title"] == "" {
		cli.Close()
		t.Fatalf("node image %s declares no OCI labels; the regression would be vacuous", digestRef)
	}

	cfg := auroraConfig()
	cfg.Image = digestRef
	n := auroraNode()
	n.ID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	n.DataVolume = "aurora-it-data-" + suffix
	n.SecretsVolume = "aurora-it-secrets-" + suffix
	archive, err := auroraBootstrapTar(n, cfg, auroraBootstrap(n))
	if err != nil {
		cli.Close()
		t.Fatalf("aurora bootstrap tar: %v", err)
	}
	p := New(NewEngine(cli), cfg)
	sdk := p.engine.(*sdkEngine)
	vols := []Resource{p.volume(n, "data"), p.volume(n, "secrets")}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if resources, ferr := sdk.Find(cleanupCtx, map[string]string{"multica.fleet.node": nodeID(n)}); ferr == nil {
			for _, r := range resources {
				_ = cli.ContainerRemove(cleanupCtx, r.ID, container.RemoveOptions{Force: true})
			}
		}
		_ = cli.VolumeRemove(cleanupCtx, n.DataVolume, true)
		_ = cli.VolumeRemove(cleanupCtx, n.SecretsVolume, true)
		cli.Close()
	})
	for _, r := range vols {
		if err := sdk.EnsureVolume(ctx, r); err != nil {
			t.Fatalf("ensure volume %s: %v", r.ID, err)
		}
	}
	if err := sdk.InstallBootstrap(ctx, vols, archive); err != nil {
		t.Fatalf("InstallBootstrap = %v (ErrUnknownHealth=%v)", err, errors.Is(err, model.ErrUnknownHealth))
	}
	left, err := sdk.Find(ctx, map[string]string{"multica.fleet.node": nodeID(n)})
	if err != nil {
		t.Fatalf("find helpers: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("bootstrap helper was not removed on success: %+v", left)
	}
}

// TestAuroraNodeAdmissionRealEngineDesktopBindSources is the real-Docker proof
// for the Docker Desktop bind-source translation. It creates the exact Aurora
// node container the provider builds, with four read-only provider credential
// bind mounts, inspects it through the same Engine path admission uses, and
// requires validateNodeInspection to accept it. On Docker Desktop the daemon
// reports every bind source as "/host_mnt" + the configured host path, which the
// pre-fix exact comparison rejected. It never pulls or pushes an image.
func TestAuroraNodeAdmissionRealEngineDesktopBindSources(t *testing.T) {
	if os.Getenv("MULTICA_RUN_DOCKER_INTEGRATION") != "1" {
		t.Skip("Docker opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	if _, err := cli.Ping(ctx); err != nil {
		cli.Close()
		t.Skipf("Docker engine unavailable: %v", err)
	}
	nodeRef := firstNonEmptyEnv(os.Getenv("MULTICA_AURORA_TEST_NODE_IMAGE"), "ghcr.io/eanfs/multica-aurora-sandbox@sha256:9e50d19d3352d5202679d35940e7dd48533f054423b9839d527c38fadaeed3da")
	digestRef, err := localImageDigestRef(ctx, cli, nodeRef)
	if err != nil {
		cli.Close()
		t.Skipf("Aurora node image unavailable: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		cli.Close()
		t.Fatalf("get working directory: %v", err)
	}
	// Docker Desktop resolves symlinks before translating a source, so the
	// fixture lives under the non-symlinked working tree rather than t.TempDir()
	// (whose /var/... path resolves to /private/var/... in the VM).
	dir, err := os.MkdirTemp(wd, ".aurora-bind-fixture-")
	if err != nil {
		cli.Close()
		t.Fatalf("create fixture dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	secretSources := map[string]string{}
	for _, key := range []string{"anthropic-api-key", "ark-api-key", "volc-asr-api-key"} {
		hostPath := filepath.Join(dir, key)
		if err := os.WriteFile(hostPath, []byte("fixture-"+key), 0o600); err != nil {
			cli.Close()
			t.Fatalf("write fixture secret: %v", err)
		}
		secretSources[key] = hostPath
	}
	cfg := auroraConfig()
	cfg.Image = digestRef
	cfg.Aurora.ProviderSecretFiles = secretSources
	seccompPath := filepath.Join(dir, "seccomp.json")
	if err := os.WriteFile(seccompPath, []byte(testSeccompProfileJSON), 0o600); err != nil {
		cli.Close()
		t.Fatalf("write seccomp profile: %v", err)
	}
	cfg.Aurora.SeccompProfile = seccompPath

	n := auroraNode()
	n.Image = digestRef
	n.ID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	n.DataVolume = "aurora-it-bind-data-" + suffix
	n.SecretsVolume = "aurora-it-bind-secrets-" + suffix

	p := New(NewEngine(cli), cfg)
	sdk := p.engine.(*sdkEngine)
	network := p.workspaceNetwork(n)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = cli.ContainerRemove(cleanupCtx, p.containerName(n), container.RemoveOptions{Force: true})
		_ = cli.VolumeRemove(cleanupCtx, n.DataVolume, true)
		_ = cli.VolumeRemove(cleanupCtx, n.SecretsVolume, true)
		_ = cli.NetworkRemove(cleanupCtx, network.Name)
		cli.Close()
	})
	if err := sdk.EnsureNetwork(ctx, network); err != nil {
		t.Fatalf("ensure workspace network: %v", err)
	}
	for _, role := range []string{"data", "secrets"} {
		if err := sdk.EnsureVolume(ctx, p.volume(n, role)); err != nil {
			t.Fatalf("ensure %s volume: %v", role, err)
		}
	}

	seccompJSON, err := resolveAuroraSeccomp(cfg.Aurora)
	if err != nil {
		t.Fatalf("resolve seccomp: %v", err)
	}
	h := NodeHostConfig(n.Resources, true, cfg.Aurora, seccompJSON)
	h.NetworkMode = container.NetworkMode(network.Name)
	h.Mounts = []mount.Mount{
		{Type: mount.TypeVolume, Source: n.DataVolume, Target: model.DataMount},
		{Type: mount.TypeVolume, Source: n.SecretsVolume, Target: model.AuroraEnrollmentDir, ReadOnly: true},
	}
	h.Mounts = append(h.Mounts, providerSecretMounts(cfg.Aurora)...)
	c := &container.Config{Image: n.Image, User: "10001:10001", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), "node"), Env: p.nodeEnv(n), Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"run"}}
	id, err := sdk.Create(ctx, c, &h, network.Name, p.containerName(n))
	if err != nil {
		t.Fatalf("create real node container: %v", err)
	}
	i, err := sdk.Inspect(ctx, id)
	if err != nil || i.sdk == nil {
		t.Fatalf("inspect real node container: err=%v snapshot=%v", err, i.sdk != nil)
	}
	translated := 0
	for _, m := range i.sdk.Mounts {
		if m.Type == mount.TypeBind && strings.HasPrefix(m.Source, "/host_mnt/") {
			translated++
		}
	}
	t.Logf("real node: %d inspected mounts, %d provider bind sources translated to /host_mnt", len(i.sdk.Mounts), translated)
	if err := p.validSDKSnapshot(n, i); err != nil {
		t.Fatalf("validateNodeInspection rejected the real node: %v", err)
	}
	for _, m := range i.sdk.Mounts {
		if m.Type != mount.TypeBind {
			continue
		}
		accepted := false
		for _, want := range providerSecretMounts(cfg.Aurora) {
			if m.Destination == want.Target && sameBindSource(m.Source, want.Source) {
				accepted = true
				break
			}
		}
		if !accepted {
			t.Fatalf("inspected bind not accepted: source=%s destination=%s", m.Source, m.Destination)
		}
	}
	if translated == 0 {
		t.Log("no /host_mnt translation observed; the daemon reported verbatim sources (Linux-style)")
	}
}
