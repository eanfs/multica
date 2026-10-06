//go:build dockerintegration

package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
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
