//go:build dockerintegration

package docker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// TestAuroraBootstrapHelperLifecycleRealEngine drives the exact installer the
// Aurora provider uses (sdkEngine.InstallBootstrap -> runHelper ->
// cleanupHelper) against the locally available unified runtime image. That image declares
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
	// Require an explicitly selected, locally present unified runtime image. Never pull.
	digestRef := os.Getenv("AURORA_RUNTIME_IMAGE")
	if digestRef == "" {
		t.Skip("digest-pinned AURORA_RUNTIME_IMAGE required")
	}
	if !approvedImage.MatchString(digestRef) {
		t.Fatal("AURORA_RUNTIME_IMAGE must be an approved digest-pinned runtime image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("Docker engine unavailable: %v", err)
	}
	insp, _, err := cli.ImageInspectWithRaw(ctx, digestRef)
	if err != nil {
		t.Fatalf("inspect local runtime image: %v", err)
	}
	if insp.Config == nil || insp.Config.Labels["org.opencontainers.image.title"] == "" {
		t.Fatalf("runtime image %s declares no OCI labels; the regression would be vacuous", digestRef)
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
