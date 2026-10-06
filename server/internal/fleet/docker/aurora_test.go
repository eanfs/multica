package docker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// auroraEnrollmentToken is a well-formed server-issued enrollment secret.
const auroraEnrollmentToken = "mse_0123456789abcdef0123456789abcdef01234567"

func auroraConfig() model.Config {
	cfg := fixtureConfig()
	cfg.Aurora = &model.AuroraConfig{ServerURL: "http://api.internal:8080"}
	return cfg
}

func auroraNode() model.Node {
	n := fixtureNode()
	n.Maintenance = false
	n.ContainerID = ""
	return n
}

func auroraBootstrap(n model.Node) model.Bootstrap {
	return model.Bootstrap{EnrollmentToken: auroraEnrollmentToken, ServerURL: "http://api.internal:8080", DaemonID: n.DaemonID}
}

func TestProviderEnsureAuroraProfile(t *testing.T) {
	n := auroraNode()
	created := false
	var installed []byte
	inspect := func(context.Context, string) (Inspection, error) {
		if !created {
			return Inspection{}, errdefs.ErrNotFound
		}
		return Inspection{ID: "cid", State: "created", Labels: fixtureLabels("node")}, nil
	}
	e := fakeCalls{inspect: inspect,
		ensureNetwork: func(context.Context, Resource) error { return nil },
		ensureVolume:  func(context.Context, Resource) error { return nil },
		bootstrap:     func(_ context.Context, _ []Resource, raw []byte) error { installed = raw; return nil },
		create: func(_ context.Context, c *container.Config, h *container.HostConfig, _, _ string) (string, error) {
			created = true
			if c.Image != "node-snapshot" || !reflect.DeepEqual([]string(c.Entrypoint), []string{"/usr/local/bin/fleet-node"}) || !reflect.DeepEqual([]string(c.Cmd), []string{"run"}) {
				t.Fatalf("aurora node must keep the fixed fleet-node entrypoint: %+v %v", c.Entrypoint, c.Cmd)
			}
			wantEnv := []string{"HOME=" + model.NodeHome, "FLEET_NODE_MAX_RUNS=1", "MULTICA_MANAGED=1", "MULTICA_SERVER_URL=http://api.internal:8080", "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=" + model.AuroraEnrollmentFile}
			if !reflect.DeepEqual(c.Env, wantEnv) {
				t.Fatalf("aurora env = %v", c.Env)
			}
			if len(h.Mounts) != 2 || h.Mounts[1].Target != model.AuroraEnrollmentDir || !h.Mounts[1].ReadOnly {
				t.Fatalf("aurora mounts = %+v", h.Mounts)
			}
			return "", nil
		},
		start:  func(context.Context, string) error { return nil },
		health: func(context.Context, string) ([]byte, error) { return []byte(goodHealth), nil },
	}
	p := New(e, auroraConfig())
	if _, err := p.Ensure(context.Background(), n, auroraBootstrap(n)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !strings.Contains(string(installed), "secrets/aurora-enrollment") || strings.Contains(string(installed), "bootstrap.json") {
		t.Fatal("aurora installer must carry only the managed enrollment secret")
	}
}

func TestProviderRejectsCrossProfileBootstrap(t *testing.T) {
	n := auroraNode()
	notFound := fakeCalls{inspect: func(context.Context, string) (Inspection, error) { return Inspection{}, errdefs.ErrNotFound }}
	aurora := New(notFound, auroraConfig())
	if _, err := aurora.Ensure(context.Background(), n, model.Bootstrap{NodeToken: "mcn_fake", APIKey: "fake", ServerURL: "http://api.internal:8080", DaemonID: n.DaemonID}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("claude bootstrap accepted under the aurora profile: %v", err)
	}
	claude := New(notFound, fixtureConfig())
	if _, err := claude.Ensure(context.Background(), n, model.Bootstrap{EnrollmentToken: auroraEnrollmentToken, ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("enrollment secret accepted under the claude profile: %v", err)
	}
	if _, err := claude.Ensure(context.Background(), n, auroraBootstrap(n)); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("aurora payload accepted under the claude profile: %v", err)
	}
}

func TestInstallerTarSelectsProfilePayload(t *testing.T) {
	n := auroraNode()
	aurora, err := installerTar(n, auroraConfig(), auroraBootstrap(n))
	if err != nil || !strings.Contains(string(aurora), "secrets/aurora-enrollment") || strings.Contains(string(aurora), "bootstrap.json") {
		t.Fatalf("aurora installer err=%v", err)
	}
	claude, err := installerTar(n, fixtureConfig(), model.Bootstrap{NodeToken: "mcn_fake", APIKey: "fake", ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID})
	if err != nil || !strings.Contains(string(claude), "secrets/bootstrap.json") || strings.Contains(string(claude), "aurora-enrollment") {
		t.Fatalf("claude installer err=%v", err)
	}
	if _, err := installerTar(n, auroraConfig(), model.Bootstrap{EnrollmentToken: "nope", DaemonID: n.DaemonID}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("invalid enrollment token accepted: %v", err)
	}
}

func TestAuroraBootstrapValidationRejectsForeignPayload(t *testing.T) {
	n := auroraNode()
	raw, err := installerTar(n, auroraConfig(), auroraBootstrap(n))
	if err != nil {
		t.Fatal(err)
	}
	data := Resource{Role: "secrets", Labels: map[string]string{"multica.fleet.node": fixtureLabels("node")["multica.fleet.node"]}}
	engine := &sdkEngine{cfg: auroraConfig()}
	if err := engine.validateBootstrap(raw, data); err != nil {
		t.Fatalf("canonical aurora installer rejected: %v", err)
	}
	claudeRaw, err := installerTar(n, fixtureConfig(), model.Bootstrap{NodeToken: "mcn_fake", APIKey: "fake", ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateBootstrap(claudeRaw, data); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("claude installer accepted by the aurora profile: %v", err)
	}
}

func TestInspectAuroraEnvironment(t *testing.T) {
	base := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + model.NodeHome, "FLEET_NODE_MAX_RUNS=1"}
	full := append(append([]string{}, base...), "MULTICA_MANAGED=1", "MULTICA_SERVER_URL=http://api.internal:8080", "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE="+model.AuroraEnrollmentFile)
	if !inspectEnvironment(full, 1, true) {
		t.Fatal("valid aurora environment rejected")
	}
	if inspectEnvironment(full, 1, false) {
		t.Fatal("aurora environment accepted without the profile")
	}
	if inspectEnvironment(base, 1, true) {
		t.Fatal("missing managed enrollment environment accepted")
	}
	credentialed := append(append([]string{}, base...), "MULTICA_MANAGED=1", "MULTICA_SERVER_URL=http://user:pass@api.internal:8080", "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE="+model.AuroraEnrollmentFile)
	if inspectEnvironment(credentialed, 1, true) {
		t.Fatal("credentialed server url accepted")
	}
}
