//go:build fleetintegration

// Package fleet_test holds the opt-in real-Docker acceptance test for the local
// Fleet node lifecycle. It is excluded from ordinary builds and skips itself
// unless MULTICA_FLEET_INTEGRATION=1, DATABASE_URL points at a disposable test
// server, and the Docker daemon plus both runtime images are available.
package fleet_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/fleet"
	fleetdocker "github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const (
	integrationEnv = "MULTICA_FLEET_INTEGRATION"
	// defaultIntegrationImage is the fake-Claude test image built from
	// docker/runtime/Dockerfile.test. It is never a production image.
	defaultIntegrationImage = "multica/runtime-claude:test"
	// The integration test only creates resources carrying this namespace label.
	integrationNamespacePrefix = "fleet-it-"

	// The node image contract: the data volume is mounted at HOME and the
	// read-only secrets volume at the fixed bootstrap path.
	nodeDataMount    = "/home/multica"
	nodeSecretsMount = "/run/secrets/runtime"

	daemonReadyTimeout = 3 * time.Minute
	lifecycleTimeout   = 3 * time.Minute
)

// TestFleetNodeLifecycleRealDocker is the opt-in acceptance test described in
// the design's "explicit Docker integration test" section. It creates one
// namespaced node through the Fleet HTTP contract, verifies the container and
// volume guarantees, exercises stop/start/reboot/delete, and removes only the
// resources labelled with this test's namespace.
//
// It needs:
//   - MULTICA_FLEET_INTEGRATION=1
//   - DATABASE_URL pointing at a test PostgreSQL server that allows CREATEDB
//   - a reachable Docker daemon (MULTICA_FLEET_DOCKER_HOST, default platform
//     socket)
//   - multica/runtime-claude:dev and multica/runtime-claude:test built locally
//   - on a non-Docker-Desktop Linux host, MULTICA_FLEET_TEST_API_HOST set to an
//     address the node network's containers can use to reach this test process
//     (the default uses host.docker.internal everywhere else).
func TestFleetNodeLifecycleRealDocker(t *testing.T) {
	if os.Getenv(integrationEnv) != "1" {
		t.Skip("set MULTICA_FLEET_INTEGRATION=1 with a running Docker daemon to run the Fleet integration test")
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("DATABASE_URL must point at a disposable test PostgreSQL server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dockerHost := envOr("MULTICA_FLEET_DOCKER_HOST", defaultDockerHost())
	sdk, err := fleetdocker.NewSDK(dockerHost)
	if err != nil {
		t.Fatalf("create Docker provider for %q: %v", dockerHost, err)
	}
	t.Cleanup(func() { _ = sdk.Close() })
	if err := sdk.Ping(ctx); err != nil {
		t.Skipf("Docker daemon at %q is not reachable: %v", dockerHost, err)
	}
	image := envOr("MULTICA_FLEET_RUNTIME_IMAGE", defaultIntegrationImage)
	if err := sdk.InspectImage(ctx, image); err != nil {
		t.Skipf("runtime image %q is not built; run: docker build -f docker/runtime/Dockerfile -t multica/runtime-claude:dev . && docker build -f docker/runtime/Dockerfile.test -t %s .", image, image)
	}

	raw, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(dockerHost), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("create raw Docker client: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	// The isolated database applies the normal migration runner, so Fleet never
	// starts a second schema or reuses another checkout's data.
	pool := testutil.IsolatedDB(t, dsn)
	fleetStore := store.New(pool)

	namespace := fmt.Sprintf("%s%d", integrationNamespacePrefix, time.Now().UnixNano())
	networkName := "multica-fleet-" + namespace
	ownerID := uuid.NewString()
	serviceSecret := uuid.NewString() + uuid.NewString()

	// Create the namespace network before choosing the daemon's API URL: on a
	// plain Linux daemon the network gateway is the only host address a node
	// can reach without an extra_hosts entry.
	networkLabels := map[string]string{
		fleetdocker.LabelNamespace: namespace,
		fleetdocker.LabelRole:      fleetdocker.RoleNode,
	}
	if err := sdk.EnsureNetwork(ctx, networkName, networkLabels); err != nil {
		t.Fatalf("create namespace network %q: %v", networkName, err)
	}
	// Registered after the DB/raw-client cleanups so it runs before them (Go
	// runs cleanups in reverse registration order).
	t.Cleanup(func() { removeNamespaceResources(t, raw, namespace, networkName) })

	apiHost := integrationAPIHost(ctx, t, raw, networkName)
	apiBase := newDaemonAPIStub(t, apiHost)

	credentialsPath := writeIntegrationCredentials(t, ownerID)
	credentials, err := fleet.LoadCredentials(credentialsPath)
	if err != nil {
		t.Fatalf("load integration credentials: %v", err)
	}
	secretPath := filepath.Join(t.TempDir(), "fleet-secret")
	if err := os.WriteFile(secretPath, []byte(serviceSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write integration service secret: %v", err)
	}

	cfg := fleet.Config{
		Listen:           "127.0.0.1:0",
		Namespace:        namespace,
		SecretFile:       secretPath,
		DockerHost:       dockerHost,
		NodeNetwork:      networkName,
		RuntimeImage:     image,
		APIURL:           apiBase,
		DatabaseURL:      dsn,
		CredentialsFile:  credentialsPath,
		MaxNodesPerOwner: 1,
		Specs: []fleet.Spec{{
			ID:            "it-standard",
			CPU:           1,
			MemoryMB:      1024,
			PidsLimit:     256,
			MaxConcurrent: 1,
		}},
	}
	service := fleet.NewService(cfg, fleetStore, sdk, credentials, serviceSecret)
	handler := httptest.NewServer(fleet.NewHTTP(service))
	t.Cleanup(handler.Close)
	api := &fleetHTTPTestClient{t: t, base: handler.URL, secret: serviceSecret, ownerID: ownerID}

	// A ready Fleet handler proves the isolated database and Docker daemon are
	// both usable before any resource is created.
	if status, body := httpGet(t, ctx, handler.URL+"/readyz"); status != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200: %s", status, body)
	}
	if status, _ := httpGet(t, ctx, handler.URL+"/api/v1/"); status != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/ without service auth = %d, want 401", status)
	}

	created := api.createNode(ctx, "integration-node", "it-standard")
	if created.Status != fleet.StateLaunching {
		t.Fatalf("new node status = %q, want %q", created.Status, fleet.StateLaunching)
	}
	if created.Region != "local" || created.Provider != "docker" {
		t.Fatalf("new node region/provider = %q/%q, want local/docker", created.Region, created.Provider)
	}
	if created.InstanceID == "" {
		t.Fatalf("new node has no container id: %+v", created)
	}
	daemonID := metadataString(created, "daemon_id")
	if daemonID == "" {
		t.Fatalf("new node has no daemon id: %+v", created.Metadata)
	}
	containerID := created.InstanceID
	containerName := nodeContainerName(t, ctx, raw, containerID)
	if want := "multica-fleet-" + namespace + "-" + created.ID; containerName != want {
		t.Fatalf("container name = %q, want %q", containerName, want)
	}

	assertNodeContainer(t, ctx, raw, image, namespace, daemonID, networkName, containerID, containerName)
	dataVolume, secretsVolume := assertNodeVolumes(t, ctx, raw, namespace, daemonID, containerName)

	// The daemon must become genuinely ready before Fleet can report running.
	// A reachable health endpoint that still says "starting" is not readiness.
	// Wait first: entrypoint.sh writes the CLI config before it starts the
	// daemon, so reading the token before readiness races cold container start.
	waitForDaemonReady(t, ctx, raw, sdk, containerID)
	status := waitForNodeStatus(ctx, t, service, api, created.ID, fleet.StateRunning, 60*time.Second)
	if status != fleet.StateRunning {
		// Regression gate: a launched node that never leaves launching cannot
		// be stopped, rebooted, or deleted through the API.
		t.Fatalf("node stayed %q after the daemon was ready; launching-to-running promotion regressed (coordinator nextTargetState)", status)
	}

	token := readNodeToken(t, ctx, raw, containerID)
	if !strings.HasPrefix(token, "mcn_") {
		t.Fatalf("node token prefix = %q, want mcn_", token)
	}
	identity := api.verifyPAT(ctx, token)
	if !identity.Valid || identity.OwnerID != ownerID || identity.InstanceID != containerID || identity.InstanceRecordID != created.ID {
		t.Fatalf("PAT verification = %+v, want owner %s node %s container %s", identity, ownerID, created.ID, containerID)
	}

	// stop: the container is preserved, the volumes and identity survive.
	api.operate(ctx, "stop", created.ID)
	if got := waitForNodeStatus(ctx, t, service, api, created.ID, fleet.StateStopped, lifecycleTimeout); got != fleet.StateStopped {
		t.Fatalf("node status after stop = %q, want %q", got, fleet.StateStopped)
	}
	assertContainerRunning(t, ctx, raw, containerID, false)

	// start: the same container and volumes return, now running.
	api.operate(ctx, "start", created.ID)
	if got := waitForNodeStatus(ctx, t, service, api, created.ID, fleet.StateRunning, lifecycleTimeout); got != fleet.StateRunning {
		t.Fatalf("node status after start = %q, want %q", got, fleet.StateRunning)
	}
	assertContainerRunning(t, ctx, raw, containerID, true)
	if after := nodeContainerName(t, ctx, raw, containerID); after != containerName {
		t.Fatalf("start changed the container: %q -> %q", containerName, after)
	}

	// reboot: identity and persistent content stay with the original container.
	api.operate(ctx, "reboot", created.ID)
	if got := waitForNodeStatus(ctx, t, service, api, created.ID, fleet.StateRunning, lifecycleTimeout); got != fleet.StateRunning {
		t.Fatalf("node status after reboot = %q, want %q", got, fleet.StateRunning)
	}
	if after := nodeContainerName(t, ctx, raw, containerID); after != containerName {
		t.Fatalf("reboot changed the container: %q -> %q", containerName, after)
	}

	// Delete requires a stopped container: the direct Fleet delete path removes
	// without force, and the API's queued-task barrier is exercised separately
	// by the DB-backed coordinator tests.
	api.operate(ctx, "stop", created.ID)
	if got := waitForNodeStatus(ctx, t, service, api, created.ID, fleet.StateStopped, lifecycleTimeout); got != fleet.StateStopped {
		t.Fatalf("node status before delete = %q, want %q", got, fleet.StateStopped)
	}
	deleted := api.deleteNode(ctx, created.ID)
	if deleted.Status != fleet.StateTerminated {
		t.Fatalf("node status after delete = %q, want %q", deleted.Status, fleet.StateTerminated)
	}
	if _, err := raw.ContainerInspect(ctx, containerID); !errdefs.IsNotFound(err) {
		t.Fatalf("container %s still exists after delete: %v", containerID, err)
	}
	for _, name := range []string{dataVolume, secretsVolume} {
		if _, err := raw.VolumeInspect(ctx, name); !errdefs.IsNotFound(err) {
			t.Fatalf("volume %s still exists after delete: %v", name, err)
		}
	}
	if got := api.verifyPAT(ctx, token); got.Valid {
		t.Fatalf("revoked node token still verifies: %+v", got)
	}

	assertNoNamespaceResources(t, ctx, raw, namespace)
}

type nodeResponse struct {
	ID           string         `json:"id"`
	OwnerID      string         `json:"owner_id"`
	InstanceID   string         `json:"instance_id"`
	Region       string         `json:"region"`
	InstanceType string         `json:"instance_type"`
	ImageID      string         `json:"image_id"`
	Name         string         `json:"name"`
	Status       string         `json:"status"`
	Provider     string         `json:"provider"`
	Error        string         `json:"error"`
	Metadata     map[string]any `json:"metadata"`
}

type patIdentity struct {
	Valid            bool   `json:"valid"`
	OwnerID          string `json:"owner_id"`
	InstanceID       string `json:"instance_id"`
	InstanceRecordID string `json:"instance_record_id"`
}

type fleetHTTPTestClient struct {
	t       *testing.T
	base    string
	secret  string
	ownerID string
}

func (c *fleetHTTPTestClient) createNode(ctx context.Context, name, specID string) nodeResponse {
	c.t.Helper()
	key := uuid.NewString()
	data := c.request(ctx, http.MethodPost, "/api/v1/nodes", map[string]any{
		"name":            name,
		"spec_id":         specID,
		"idempotency_key": key,
	}, http.StatusOK, map[string]string{"Idempotency-Key": key})
	var node nodeResponse
	if err := json.Unmarshal(data, &node); err != nil {
		c.t.Fatalf("decode created node: %v: %s", err, data)
	}
	return node
}

func (c *fleetHTTPTestClient) operate(ctx context.Context, operation, nodeID string) nodeResponse {
	c.t.Helper()
	data := c.request(ctx, http.MethodPost, "/api/v1/nodes/"+operation, map[string]any{
		"node_id":         nodeID,
		"idempotency_key": uuid.NewString(),
	}, http.StatusOK, nil)
	var node nodeResponse
	if err := json.Unmarshal(data, &node); err != nil {
		c.t.Fatalf("decode %s response: %v: %s", operation, err, data)
	}
	return node
}

func (c *fleetHTTPTestClient) deleteNode(ctx context.Context, nodeID string) nodeResponse {
	c.t.Helper()
	data := c.request(ctx, http.MethodDelete, "/api/v1/nodes", map[string]any{"node_id": nodeID}, http.StatusOK, nil)
	var node nodeResponse
	if err := json.Unmarshal(data, &node); err != nil {
		c.t.Fatalf("decode delete response: %v: %s", err, data)
	}
	return node
}

func (c *fleetHTTPTestClient) listNodes(ctx context.Context) []nodeResponse {
	c.t.Helper()
	data := c.request(ctx, http.MethodGet, "/api/v1/nodes", nil, http.StatusOK, nil)
	var nodes []nodeResponse
	if err := json.Unmarshal(data, &nodes); err != nil {
		c.t.Fatalf("decode node list: %v: %s", err, data)
	}
	return nodes
}

func (c *fleetHTTPTestClient) nodeByID(ctx context.Context, nodeID string) (nodeResponse, bool) {
	c.t.Helper()
	for _, node := range c.listNodes(ctx) {
		if node.ID == nodeID {
			return node, true
		}
	}
	return nodeResponse{}, false
}

func (c *fleetHTTPTestClient) verifyPAT(ctx context.Context, token string) patIdentity {
	c.t.Helper()
	body := fmt.Sprintf("{\"token\":%q}", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/pat/verify", strings.NewReader(body))
	if err != nil {
		c.t.Fatalf("build PAT verify request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("verify PAT: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("verify PAT = %d: %s", resp.StatusCode, data)
	}
	var identity patIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		c.t.Fatalf("decode PAT verify: %v: %s", err, data)
	}
	return identity
}

func (c *fleetHTTPTestClient) request(ctx context.Context, method, requestPath string, body any, wantStatus int, extraHeaders map[string]string) []byte {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encode %s %s: %v", method, requestPath, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+requestPath, reader)
	if err != nil {
		c.t.Fatalf("build %s %s: %v", method, requestPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	req.Header.Set("X-User-ID", c.ownerID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, requestPath, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != wantStatus {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, requestPath, resp.StatusCode, wantStatus, data)
	}
	return data
}

func assertNodeContainer(t *testing.T, ctx context.Context, raw *dockerclient.Client, image, namespace, daemonID, networkName, containerID, containerName string) {
	t.Helper()
	info, err := raw.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("inspect node container %s: %v", containerID, err)
	}
	if info.Config == nil || info.HostConfig == nil || info.State == nil {
		t.Fatalf("node container %s has an incomplete inspect payload", containerID)
	}
	if info.Config.Image != image {
		t.Fatalf("container image = %q, want %q", info.Config.Image, image)
	}
	if info.Config.User != "1000:1000" {
		t.Fatalf("container user = %q, want 1000:1000", info.Config.User)
	}
	for key, want := range map[string]string{
		fleetdocker.LabelNamespace: namespace,
		fleetdocker.LabelNode:      daemonID,
		fleetdocker.LabelRole:      fleetdocker.RoleNode,
	} {
		if got := info.Config.Labels[key]; got != want {
			t.Fatalf("container label %s = %q, want %q", key, got, want)
		}
	}
	if info.HostConfig.Privileged {
		t.Fatal("node container must not be privileged")
	}
	if info.HostConfig.PidMode != "" || info.HostConfig.IpcMode == "host" || info.HostConfig.NetworkMode == "host" {
		t.Fatalf("node container shares a host namespace: pid=%q ipc=%q net=%q", info.HostConfig.PidMode, info.HostConfig.IpcMode, info.HostConfig.NetworkMode)
	}
	if string(info.HostConfig.NetworkMode) != networkName {
		t.Fatalf("container network = %q, want %q", info.HostConfig.NetworkMode, networkName)
	}
	if info.HostConfig.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Fatalf("container restart policy = %q, want disabled (explicit stop must stick)", info.HostConfig.RestartPolicy.Name)
	}
	if info.Config.Healthcheck == nil || len(info.Config.Healthcheck.Test) == 0 {
		t.Fatal("node container has no healthcheck")
	}
	if len(info.HostConfig.PortBindings) != 0 || len(info.Config.ExposedPorts) != 0 {
		t.Fatalf("node container publishes ports: bindings=%v exposed=%v", info.HostConfig.PortBindings, info.Config.ExposedPorts)
	}
	if len(info.HostConfig.Binds) != 0 {
		t.Fatalf("node container has host binds: %v", info.HostConfig.Binds)
	}

	env := strings.Join(info.Config.Env, "\n")
	for _, forbidden := range []string{"ANTHROPIC_", "CLAUDE_CODE_", "sk-ant-fleet-it", "mcn_"} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("node environment contains %q; model and node credentials belong in the secrets volume", forbidden)
		}
	}

	wantDataVolume := containerName + "-data"
	wantSecretsVolume := containerName + "-secrets"
	var data, secrets bool
	for _, m := range info.Mounts {
		if m.Type == mount.TypeBind {
			t.Fatalf("node container mounts a host path: %s -> %s", m.Source, m.Destination)
		}
		if strings.Contains(m.Source, "docker.sock") {
			t.Fatalf("node container mounts the Docker socket: %s", m.Source)
		}
		switch m.Destination {
		case nodeDataMount:
			data = true
			if !m.RW {
				t.Fatalf("data volume %s is mounted read-only", m.Name)
			}
			if m.Name != wantDataVolume {
				t.Fatalf("data volume = %q, want %q", m.Name, wantDataVolume)
			}
		case nodeSecretsMount:
			secrets = true
			if m.RW {
				t.Fatalf("secrets volume %s is mounted read-write", m.Name)
			}
			if m.Name != wantSecretsVolume {
				t.Fatalf("secrets volume = %q, want %q", m.Name, wantSecretsVolume)
			}
		}
	}
	if !data || !secrets {
		t.Fatalf("node volumes not mounted at the image contract: data=%v secrets=%v mounts=%v", data, secrets, info.Mounts)
	}
	if info.Name != "/"+containerName {
		t.Fatalf("container name = %q, want %q", info.Name, "/"+containerName)
	}
}

func assertNodeVolumes(t *testing.T, ctx context.Context, raw *dockerclient.Client, namespace, daemonID, containerName string) (dataVolume, secretsVolume string) {
	t.Helper()
	for _, suffix := range []string{"-data", "-secrets"} {
		name := containerName + suffix
		vol, err := raw.VolumeInspect(ctx, name)
		if err != nil {
			t.Fatalf("inspect node volume %s: %v", name, err)
		}
		for key, want := range map[string]string{
			fleetdocker.LabelNamespace: namespace,
			fleetdocker.LabelNode:      daemonID,
			fleetdocker.LabelRole:      fleetdocker.RoleNode,
		} {
			if got := vol.Labels[key]; got != want {
				t.Fatalf("volume %s label %s = %q, want %q", name, key, got, want)
			}
		}
	}
	return containerName + "-data", containerName + "-secrets"
}

func nodeContainerName(t *testing.T, ctx context.Context, raw *dockerclient.Client, containerID string) string {
	t.Helper()
	info, err := raw.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("inspect container %s for its name: %v", containerID, err)
	}
	return strings.TrimPrefix(info.Name, "/")
}

func assertContainerRunning(t *testing.T, ctx context.Context, raw *dockerclient.Client, containerID string, want bool) {
	t.Helper()
	info, err := raw.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("inspect container %s: %v", containerID, err)
	}
	if info.State == nil || info.State.Running != want {
		state := "missing"
		if info.State != nil {
			state = info.State.Status
		}
		t.Fatalf("container %s state = %q, want running=%v", containerID, state, want)
	}
}

func readNodeToken(t *testing.T, ctx context.Context, raw *dockerclient.Client, containerID string) string {
	t.Helper()
	rc, _, err := raw.CopyFromContainer(ctx, containerID, "/home/multica/.multica/config.json")
	if err != nil {
		t.Fatalf("copy the node CLI config: %v", err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read the node CLI config archive: %v", err)
		}
		if path.Base(strings.TrimSuffix(header.Name, "/")) != "config.json" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			t.Fatalf("read the node CLI config: %v", err)
		}
		var cfg struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("decode the node CLI config: %v", err)
		}
		return cfg.Token
	}
	t.Fatal("node CLI config.json was not found in the data volume")
	return ""
}

func waitForDaemonReady(t *testing.T, ctx context.Context, raw *dockerclient.Client, sdk *fleetdocker.SDK, containerID string) fleetdocker.HealthReport {
	t.Helper()
	deadline := time.Now().Add(daemonReadyTimeout)
	var last string
	for {
		info, err := raw.ContainerInspect(ctx, containerID)
		switch {
		case err != nil:
			last = fmt.Sprintf("inspect: %v", err)
		case info.State != nil && info.State.Running:
			body, execErr := sdk.ExecHealth(ctx, containerID)
			if execErr != nil {
				last = fmt.Sprintf("health exec: %v", execErr)
				break
			}
			report, parseErr := fleetdocker.ParseHealthReport(body)
			if parseErr != nil {
				last = fmt.Sprintf("health payload: %v", parseErr)
				break
			}
			if report.Status == "running" {
				return report
			}
			last = fmt.Sprintf("daemon status %q", report.Status)
		case info.State != nil:
			last = fmt.Sprintf("container state %q", info.State.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready within %s (last observation: %s)\ncontainer logs:\n%s", daemonReadyTimeout, last, containerLogs(ctx, raw, containerID))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("daemon readiness wait canceled: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func waitForNodeStatus(ctx context.Context, t *testing.T, service *fleet.Service, api *fleetHTTPTestClient, nodeID, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		if err := service.Tick(ctx); err != nil {
			t.Logf("coordinator tick failed while waiting for %q: %v", want, err)
		}
		node, ok := api.nodeByID(ctx, nodeID)
		if !ok {
			t.Fatalf("node %s disappeared while waiting for %q", nodeID, want)
		}
		last = node.Status
		if last == want {
			return last
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(2 * time.Second):
		}
	}
}

func metadataString(node nodeResponse, key string) string {
	value, _ := node.Metadata[key].(string)
	return value
}

func containerLogs(ctx context.Context, raw *dockerclient.Client, containerID string) string {
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	rc, err := raw.ContainerLogs(logCtx, containerID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "100"})
	if err != nil {
		return fmt.Sprintf("(container logs unavailable: %v)", err)
	}
	defer rc.Close()
	var stdout, stderr bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdout, &stderr, rc)
	return stdout.String() + stderr.String()
}

func assertNoNamespaceResources(t *testing.T, ctx context.Context, raw *dockerclient.Client, namespace string) {
	t.Helper()
	containers, err := raw.ContainerList(ctx, container.ListOptions{All: true, Filters: namespaceFilter(namespace)})
	if err != nil {
		t.Fatalf("list namespace containers: %v", err)
	}
	if len(containers) != 0 {
		ids := make([]string, 0, len(containers))
		for _, item := range containers {
			ids = append(ids, item.ID)
		}
		t.Fatalf("namespace %s still has containers after delete: %v", namespace, ids)
	}
	vols, err := raw.VolumeList(ctx, volume.ListOptions{Filters: namespaceFilter(namespace)})
	if err != nil {
		t.Fatalf("list namespace volumes: %v", err)
	}
	if len(vols.Volumes) != 0 {
		names := make([]string, 0, len(vols.Volumes))
		for _, item := range vols.Volumes {
			names = append(names, item.Name)
		}
		t.Fatalf("namespace %s still has volumes after delete: %v", namespace, names)
	}
}

// removeNamespaceResources is the test's cleanup. It deletes only resources
// carrying this test's namespace label, never a name prefix and never a global
// prune, so a shared Docker daemon is safe.
func removeNamespaceResources(t *testing.T, raw *dockerclient.Client, namespace, networkName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	containers, err := raw.ContainerList(ctx, container.ListOptions{All: true, Filters: namespaceFilter(namespace)})
	if err != nil {
		t.Errorf("cleanup: list namespace containers: %v", err)
	}
	for _, item := range containers {
		if item.Labels[fleetdocker.LabelNamespace] != namespace {
			continue
		}
		if err := raw.ContainerRemove(ctx, item.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("cleanup: remove container %s: %v", item.ID, err)
		}
	}
	vols, err := raw.VolumeList(ctx, volume.ListOptions{Filters: namespaceFilter(namespace)})
	if err != nil {
		t.Errorf("cleanup: list namespace volumes: %v", err)
	}
	for _, item := range vols.Volumes {
		if item.Labels[fleetdocker.LabelNamespace] != namespace {
			continue
		}
		if err := raw.VolumeRemove(ctx, item.Name, true); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("cleanup: remove volume %s: %v", item.Name, err)
		}
	}
	info, err := raw.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err != nil {
		if !errdefs.IsNotFound(err) {
			t.Errorf("cleanup: inspect network %s: %v", networkName, err)
		}
		return
	}
	if info.Labels[fleetdocker.LabelNamespace] != namespace {
		t.Errorf("cleanup: refusing to remove network %s with foreign labels %v", networkName, info.Labels)
		return
	}
	if err := raw.NetworkRemove(ctx, networkName); err != nil && !errdefs.IsNotFound(err) {
		t.Errorf("cleanup: remove network %s: %v", networkName, err)
	}
}

func namespaceFilter(namespace string) filters.Args {
	args := filters.NewArgs()
	args.Add("label", fleetdocker.LabelNamespace+"="+namespace)
	return args
}

// newDaemonAPIStub serves the minimum daemon preflight: token renewal always
// succeeds and the user belongs to zero workspaces. A user with no workspaces
// is a supported daemon startup path, so the daemon reports "running" without
// registering any runtime. This keeps the test focused on Fleet's real Docker
// lifecycle; workspace registration and task execution are covered by the
// non-Docker tests and require a real API, not a stub.
func newDaemonAPIStub(t *testing.T, host string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/workspaces", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"renewed\":false}")
	})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen for the daemon API stub: %v", err)
	}
	server := httptest.NewUnstartedServer(mux)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	port := listener.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://%s:%d", host, port)
}

// integrationAPIHost is the address a node container uses to reach this test
// process. Docker Desktop provides host.docker.internal; a plain Linux daemon
// reaches host services through its network gateway. An explicit override wins.
func integrationAPIHost(ctx context.Context, t *testing.T, raw *dockerclient.Client, networkName string) string {
	t.Helper()
	if override := strings.TrimSpace(os.Getenv("MULTICA_FLEET_TEST_API_HOST")); override != "" {
		return override
	}
	if runtime.GOOS != "linux" {
		return "host.docker.internal"
	}
	info, err := raw.NetworkInspect(ctx, networkName, network.InspectOptions{})
	if err != nil {
		t.Fatalf("inspect the node network for its gateway: %v", err)
	}
	if len(info.IPAM.Config) == 0 || info.IPAM.Config[0].Gateway == "" {
		t.Fatalf("node network %s has no IPv4 gateway; set MULTICA_FLEET_TEST_API_HOST", networkName)
	}
	return info.IPAM.Config[0].Gateway
}

func writeIntegrationCredentials(t *testing.T, ownerID string) string {
	t.Helper()
	file := map[string]any{
		"users": map[string]string{ownerID: "integration"},
		"profiles": map[string]fleet.Profile{
			"integration": {Env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-fleet-it-fake"}},
		},
	}
	encoded, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("encode integration credentials: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fleet-credentials.json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write integration credentials: %v", err)
	}
	return path
}

func httpGet(t *testing.T, ctx context.Context, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(data)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func defaultDockerHost() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}
