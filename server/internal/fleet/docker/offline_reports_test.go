package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

const goodOffline = `{"manifest":{"version":1,"namespace":"ns","fleet_id":"fleet","node_id":"01000000-0000-0000-0000-000000000000","daemon_id":"04000000-0000-0000-0000-000000000000","data_mount":"/data","node_home":"/data/home","workspaces_root":"/data/workspaces"},"report_queue_stats":{"known":true,"pending":0,"failed":0}}`

func framed(raw string) string {
	h := make([]byte, 8)
	h[0] = 1
	binary.BigEndian.PutUint32(h[4:], uint32(len(raw)))
	return string(h) + raw
}

// This stateful transport models Docker HTTP, not the provider or future CLI.
type offlineHTTP struct {
	afterSecrets                                                                     func()
	t                                                                                *testing.T
	nodeMount                                                                        string
	nodeLabels                                                                       map[string]string
	volumeMissing, writer, createTimeout, helperMissing, cleanupForeign, waitBlocked bool
	output                                                                           string
	exit                                                                             int
	created, started, removed                                                        bool
	helperConfig                                                                     *container.Config
	helperHost                                                                       *container.HostConfig
	waitDeadline                                                                     time.Time
	nodeRemoved, secretsRemoved, failSecretsDrop                                     bool
	removalOrder                                                                     []string
	helperCreates                                                                    int
	volumeDeletes                                                                    int
	copied                                                                           bool
	archive                                                                          []byte
}

func (s *offlineHTTP) roundTrip(r *http.Request) (*http.Response, error) {
	s.t.Helper()
	path := strings.TrimPrefix(r.URL.Path, "/v1.51")
	switch {
	case r.Method == "PUT" && path == "/containers/helper/archive":
		if r.URL.Query().Get("path") != "/" || r.URL.Query().Get("copyUIDGID") != "true" {
			s.t.Fatal("unsafe copy destination/ownership")
		}
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Equal(raw, s.archive) {
			s.t.Fatal("bootstrap archive changed")
		}
		s.copied = true
		return response(200, ""), nil
	case r.Method == "GET" && path == "/volumes/secrets-vol":
		if s.secretsRemoved {
			return response(404, `{"message":"missing"}`), nil
		}
		raw, _ := json.Marshal(map[string]any{"Name": "secrets-vol", "Driver": "local", "Labels": fixtureLabels("secrets")})
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/volumes/foreign":
		return response(200, `{"Name":"foreign","Driver":"local","Labels":{}}`), nil
	case r.Method == "GET" && path == "/volumes/data-vol":
		if s.volumeMissing {
			return response(404, `{"message":"no such volume"}`), nil
		}
		raw, _ := json.Marshal(map[string]any{"Name": "data-vol", "Driver": "local", "Labels": fixtureLabels("data")})
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/containers/cid/json":
		if s.nodeRemoved {
			return response(404, `{"message":"missing"}`), nil
		}
		l := s.nodeLabels
		if l == nil {
			l = fixtureLabels("node")
		}
		name := s.nodeMount
		if name == "" {
			name = "data-vol"
		}
		raw, _ := json.Marshal(map[string]any{"Id": "cid", "State": map[string]any{"Status": "exited", "Running": false}, "Config": map[string]any{"Image": "node-snapshot", "Labels": l}, "Mounts": []map[string]any{{"Type": "volume", "Name": name, "Destination": "/data", "RW": true}}})
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/containers/json":
		if r.URL.Query().Get("all") != "1" {
			s.t.Fatal("writer inventory must include all containers")
		}
		if s.writer {
			return response(200, `[{"Id":"foreign-writer","State":"running","Mounts":[{"Type":"volume","Name":"data-vol","Destination":"/elsewhere","RW":true}]}]`), nil
		}
		return response(200, "[]"), nil
	case r.Method == "GET" && path == "/containers/foreign-writer/json":
		return response(200, `{"Id":"foreign-writer","State":{"Running":true,"Status":"running"},"Config":{"Labels":{}},"Mounts":[{"Type":"volume","Name":"data-vol","Destination":"/elsewhere","RW":true}]}`), nil
	case r.Method == "POST" && path == "/containers/create":
		var req container.CreateRequest
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			s.t.Fatal(e)
		}
		s.helperConfig = req.Config
		s.helperHost = req.HostConfig
		s.created = true
		s.helperCreates++
		if s.createTimeout {
			return nil, context.DeadlineExceeded
		}
		return response(201, `{"Id":"helper"}`), nil
	case r.Method == "GET" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		if !s.created || s.helperMissing {
			return response(404, `{"message":"no such helper"}`), nil
		}
		c := *s.helperConfig
		if s.cleanupForeign && s.started {
			c.Labels = fixtureLabels("other")
		}
		mounts := []map[string]any{}
		for _, m := range s.helperHost.Mounts {
			mounts = append(mounts, map[string]any{"Type": m.Type, "Name": m.Source, "Destination": m.Target, "RW": !m.ReadOnly})
		}
		raw, _ := json.Marshal(map[string]any{"Id": "helper", "State": map[string]any{"Status": "exited", "Running": false}, "Config": c, "HostConfig": s.helperHost, "Mounts": mounts})
		return response(200, string(raw)), nil
	case r.Method == "POST" && path == "/containers/helper/start":
		s.started = true
		return response(204, ""), nil
	case r.Method == "POST" && path == "/containers/helper/wait":
		if r.URL.Query().Get("condition") != "not-running" {
			s.t.Fatal("wrong wait condition")
		}
		s.waitDeadline, _ = r.Context().Deadline()
		if s.waitBlocked {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		raw, _ := json.Marshal(map[string]any{"StatusCode": s.exit})
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/containers/helper/logs":
		if r.URL.Query().Get("stdout") != "1" || r.URL.Query().Get("stderr") != "1" {
			s.t.Fatal("must account for both streams")
		}
		return response(200, framed(s.output)), nil
	case r.Method == "DELETE" && path == "/containers/cid":
		s.nodeRemoved = true
		s.removalOrder = append(s.removalOrder, "container")
		return response(204, ""), nil
	case r.Method == "DELETE" && path == "/containers/helper":
		if r.URL.Query().Get("v") != "" {
			s.t.Fatal("cleanup must never remove volumes")
		}
		s.removed = true
		return response(204, ""), nil
	case r.Method == "DELETE" && strings.HasPrefix(path, "/volumes/"):
		if path == "/volumes/secrets-vol" && s.failSecretsDrop {
			return response(500, `{"message":"failed"}`), nil
		}
		s.volumeDeletes++
		if path == "/volumes/data-vol" {
			s.volumeMissing = true
			s.removalOrder = append(s.removalOrder, "data")
		} else {
			s.secretsRemoved = true
			s.removalOrder = append(s.removalOrder, "secrets")
			if s.afterSecrets != nil {
				s.afterSecrets()
			}
		}
		return response(204, ""), nil
	default:
		s.t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		return nil, errors.New("unexpected fake request")
	}
}
func offlineProvider(t *testing.T, s *offlineHTTP) *Provider {
	s.t = t
	if s.output == "" {
		s.output = goodOffline
	}
	return New(fakeEngine(t, s.roundTrip, false), fixtureConfig())
}
func TestOfflineHelperHasNoCredentialsOrNetwork(t *testing.T) {
	s := &offlineHTTP{}
	p := offlineProvider(t, s)
	before := time.Now()
	o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
	if e != nil || !o.ReportStatsKnown || !o.Offline || o.Ready || o.StartEpoch != "" || o.DaemonID != "" || o.ContainerID != "" || o.DataVolume != "data-vol" || o.LayoutVersion != "1" || o.ObservedAt.Before(before) {
		t.Fatalf("offline proof o=%+v err=%v", o, e)
	}
	if !s.created || !s.started || !s.removed {
		t.Fatal("helper did not complete and clean up")
	}
	c, h := s.helperConfig, s.helperHost
	if c.Image != fixtureConfig().Image || c.User != "10001:10001" || len(c.Env) != 0 || c.Tty || strings.Join(c.Entrypoint, " ") != "/usr/local/bin/fleet-node" || strings.Join(c.Cmd, " ") != "report-stats" {
		t.Fatal("helper credential/command boundary violated")
	}
	if h.NetworkMode != "none" || !h.ReadonlyRootfs || h.Privileged || h.PidMode == "host" || len(h.Binds) != 0 || len(h.PortBindings) != 0 || len(h.Mounts) != 1 || h.Mounts[0].Type != mount.TypeVolume || h.Mounts[0].Source != "data-vol" || h.Mounts[0].Target != "/data" || !h.Mounts[0].ReadOnly || h.NanoCPUs != 250000000 || h.Memory != 64<<20 || h.PidsLimit == nil || *h.PidsLimit != 16 || strings.Join(h.CapDrop, ",") != "ALL" || strings.Join(h.SecurityOpt, ",") != "no-new-privileges:true" {
		t.Fatal("unsafe offline helper")
	}
	if !Owns(c.Labels, "ns", "fleet", "01000000-0000-0000-0000-000000000000", "diagnostic") || s.waitDeadline.IsZero() || s.waitDeadline.Sub(before) > 5*time.Second+time.Millisecond*100 {
		t.Fatal("unowned/unbounded helper")
	}
}
func TestOfflineReportsWrongMountNeverZero(t *testing.T) {
	good := &offlineHTTP{}
	p := offlineProvider(t, good)
	if o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef()); e != nil || !o.ReportStatsKnown {
		t.Fatalf("valid baseline e=%v", e)
	}
	for _, kind := range []string{"wrong-mount", "foreign-label", "missing-label", "missing-volume", "active-writer"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{}
			switch kind {
			case "wrong-mount":
				s.nodeMount = "foreign-volume"
			case "foreign-label":
				s.nodeLabels = fixtureLabels("node")
				s.nodeLabels["multica.fleet.namespace"] = "foreign"
			case "missing-label":
				s.nodeLabels = fixtureLabels("node")
				delete(s.nodeLabels, "multica.fleet.node")
			case "missing-volume":
				s.volumeMissing = true
			case "active-writer":
				s.writer = true
			}
			p := offlineProvider(t, s)
			o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
			if e == nil || o.ReportStatsKnown || s.created || s.volumeDeletes != 0 {
				t.Fatalf("unsafe proof e=%v known=%v helper=%v", e, o.ReportStatsKnown, s.created)
			}
		})
	}
}
func TestOfflineReportsUnknownPreservesData(t *testing.T) {
	for _, kind := range []string{"malformed", "unknown", "wrong-layout", "wrong-daemon", "missing-count", "fractional", "nonzero-exit", "oversized", "pending", "failed"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{output: goodOffline}
			switch kind {
			case "malformed":
				s.output = "{}"
			case "unknown":
				s.output = strings.Replace(goodOffline, "true", "false", 1)
			case "wrong-layout":
				s.output = strings.Replace(goodOffline, "/data/workspaces", "/foreign", 1)
			case "wrong-daemon":
				s.output = strings.Replace(goodOffline, fixtureNode().DaemonID+"\"", "foreign\"", 1)
			case "missing-count":
				s.output = strings.Replace(goodOffline, "\"pending\":0,", "", 1)
			case "fractional":
				s.output = strings.Replace(goodOffline, "\"pending\":0", "\"pending\":0.5", 1)
			case "nonzero-exit":
				s.exit = 2
			case "oversized":
				s.output = strings.Repeat("x", 65537)
			case "pending":
				s.output = strings.Replace(goodOffline, "\"pending\":0", "\"pending\":1", 1)
			case "failed":
				s.output = strings.Replace(goodOffline, "\"failed\":0", "\"failed\":1", 1)
			}
			p := offlineProvider(t, s)
			o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
			if kind == "pending" || kind == "failed" {
				if e != nil || !o.ReportStatsKnown {
					t.Fatalf("known busy must remain known e=%v", e)
				}
			} else if e == nil && o.ReportStatsKnown {
				t.Fatal("unknown became known")
			}
			if e = p.Delete(context.Background(), fixtureNode(), fixtureRef()); e == nil || s.volumeDeletes != 0 {
				t.Fatalf("data deletion escaped proof e=%v deletes=%d", e, s.volumeDeletes)
			}
			if !s.created || !s.removed {
				t.Fatal("own helper cleanup missing")
			}
		})
	}
}
func TestOfflineHelperTimeoutCleansOnlyOwnResources(t *testing.T) {
	s := &offlineHTTP{waitBlocked: true}
	p := offlineProvider(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	o, e := p.Diagnose(ctx, fixtureNode(), fixtureRef())
	if e == nil || o.ReportStatsKnown || !s.removed || s.volumeDeletes != 0 || time.Since(start) > time.Second {
		t.Fatalf("timeout cleanup err=%v known=%v removed=%v", e, o.ReportStatsKnown, s.removed)
	}
}
func TestOfflineHelperForeignCleanupNeverDeletesOrProves(t *testing.T) {
	s := &offlineHTTP{cleanupForeign: true}
	p := offlineProvider(t, s)
	o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
	if e == nil || o.ReportStatsKnown || s.removed || s.volumeDeletes != 0 {
		t.Fatal("foreign helper cleanup granted proof or removed resource")
	}
}
func TestOfflineHelperUncertainCreateInspectsAndCleansOwn(t *testing.T) {
	s := &offlineHTTP{createTimeout: true}
	p := offlineProvider(t, s)
	o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
	if e != nil || !o.ReportStatsKnown || !s.removed {
		t.Fatalf("uncertain helper proof e=%v o=%+v cleanup=%v", e, o, s.removed)
	}
}
func TestOfflineDeleteDataIsReprovedAfterEarlierRemoval(t *testing.T) {
	s := &offlineHTTP{}
	p := offlineProvider(t, s)
	n := fixtureNode()
	n.Revoked = true
	n.Desired = "terminating"
	s.afterSecrets = func() { s.output = strings.Replace(goodOffline, "\"pending\":0", "\"pending\":1", 1) }
	if e := p.Delete(context.Background(), n, fixtureRef()); e == nil || s.volumeMissing || strings.Join(s.removalOrder, ",") != "container,secrets" {
		t.Fatalf("final data step ignored current proof e=%v order=%v", e, s.removalOrder)
	}
}
func TestOfflineDeleteFreshProviderAndApprovedRefRecovery(t *testing.T) {
	for _, kind := range []string{"fresh", "all-absent", "partial-missing-data", "failed-secrets"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{}
			n := fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			if kind == "all-absent" {
				s.nodeRemoved = true
				s.secretsRemoved = true
				s.volumeMissing = true
			}
			if kind == "partial-missing-data" {
				s.nodeRemoved = true
				s.volumeMissing = true
			}
			if kind == "failed-secrets" {
				s.failSecretsDrop = true
			}
			p := offlineProvider(t, s)
			e := p.Delete(context.Background(), n, fixtureRef())
			switch kind {
			case "fresh":
				if e != nil || strings.Join(s.removalOrder, ",") != "container,secrets,data" {
					t.Fatalf("fresh durable-ref deletion e=%v order=%v", e, s.removalOrder)
				}
				q := offlineProvider(t, s)
				if e = q.Delete(context.Background(), n, fixtureRef()); e != nil {
					t.Fatalf("fresh process same ref retry e=%v", e)
				}
			case "all-absent":
				if e != nil || s.created || s.volumeDeletes != 0 {
					t.Fatalf("all-absent completion e=%v helper=%v", e, s.created)
				}
			case "partial-missing-data":
				if e == nil || s.created || s.volumeDeletes != 0 {
					t.Fatal("missing data fabricated report zero")
				}
			case "failed-secrets":
				if e == nil || s.volumeMissing || strings.Join(s.removalOrder, ",") != "container" {
					t.Fatalf("data removed before uncertain secrets completion e=%v order=%v", e, s.removalOrder)
				}
			}
		})
	}
}
func TestOfflineDeleteRejectsUntrustedRefBeforeMutation(t *testing.T) {
	for _, kind := range []string{"namespace", "node", "operation", "generation", "action", "revoked", "maintenance", "desired"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{}
			p := offlineProvider(t, s)
			n := fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			ref := fixtureRef()
			switch kind {
			case "namespace":
				ref.Namespace = "other"
			case "node":
				ref.NodeID.Bytes[0] = 9
			case "operation":
				ref.OperationID.Valid = false
			case "generation":
				ref.Generation++
			case "action":
				ref.Action = model.Stop
			case "revoked":
				n.Revoked = false
			case "maintenance":
				n.Maintenance = false
			case "desired":
				n.Desired = "running"
			}
			if e := p.Delete(context.Background(), n, ref); e == nil || s.created || s.nodeRemoved || s.volumeDeletes != 0 {
				t.Fatal("untrusted deletion reached mutation")
			}
		})
	}
}
func TestOfflineDeleteRechecksCurrentOwnedStoppedData(t *testing.T) {
	for _, kind := range []string{"owned-zero", "writer-after-proof", "changed-volume", "changed-generation", "foreign-after-proof"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{}
			p := offlineProvider(t, s)
			n := fixtureNode()
			if _, e := p.Diagnose(context.Background(), n, fixtureRef()); e != nil {
				t.Fatal(e)
			}
			n.Revoked = true
			n.Desired = "terminating"
			switch kind {
			case "writer-after-proof":
				s.writer = true
			case "changed-volume":
				n.DataVolume = "foreign"
			case "changed-generation":
				n.Generation++
			case "foreign-after-proof":
				s.nodeLabels = fixtureLabels("node")
				s.nodeLabels["multica.fleet.fleet_id"] = "foreign"
			}
			e := p.Delete(context.Background(), n, fixtureRef())
			if kind == "owned-zero" {
				if e != nil || s.volumeDeletes != 2 || !s.nodeRemoved || s.helperCreates != 3 {
					t.Fatalf("physical cleanup not independently proved e=%v deletes=%d helpers=%d", e, s.volumeDeletes, s.helperCreates)
				}
			} else if e == nil || s.volumeDeletes != 0 || s.nodeRemoved {
				t.Fatalf("stale/foreign proof deleted e=%v", e)
			}
		})
	}
}
func TestOfflineBadApprovedDigestNeverCreatesHelper(t *testing.T) {
	s := &offlineHTTP{}
	s.t = t
	e := fakeEngine(t, s.roundTrip, false)
	cfg := fixtureConfig()
	cfg.Image = "example/node:latest"
	p := New(e, cfg)
	o, err := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
	if err == nil || o.ReportStatsKnown || s.created {
		t.Fatal("unapproved helper digest accepted")
	}
}
func TestEngineBootstrapCopiesOnlyFixedOwnedVolumeTar(t *testing.T) {
	n, cfg := fixtureNode(), fixtureConfig()
	b := model.Bootstrap{NodeToken: "fake-private-token", APIKey: "fake-private-api-key", BaseURL: "https://provider.invalid", Model: "fake-model", DaemonID: n.DaemonID, ServerURL: cfg.APIURL}
	raw, err := bootstrapTar(n, cfg, b)
	if err != nil {
		t.Fatal(err)
	}
	s := &offlineHTTP{archive: raw}
	p := offlineProvider(t, s)
	vols := []Resource{p.volume(n, "data"), p.volume(n, "secrets")}
	if err = p.engine.InstallBootstrap(context.Background(), vols, raw); err != nil || !s.copied || !s.removed {
		t.Fatalf("bootstrap copy error=%v copied=%v removed=%v", err, s.copied, s.removed)
	}
	if strings.Join(s.helperConfig.Cmd, " ") != "bootstrap" || len(s.helperConfig.Env) != 0 || len(s.helperHost.Mounts) != 2 || s.helperHost.Mounts[0].ReadOnly || s.helperHost.Mounts[1].ReadOnly {
		t.Fatal("wrong fixed installer")
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	names := []string{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		names = append(names, h.Name)
		if h.Uid != 10001 || h.Gid != 10001 || h.Typeflag == tar.TypeSymlink || h.Mode&0077 != 0 {
			t.Fatal("unsafe tar ownership/type/mode")
		}
	}
	if strings.Join(names, ",") != "data/,data/home/,data/workspaces/,secrets/,data/fleet-layout.json,secrets/bootstrap.json" {
		t.Fatal("unexpected paths")
	}
	invalid := &offlineHTTP{}
	q := offlineProvider(t, invalid)
	if e := q.engine.InstallBootstrap(context.Background(), vols, []byte("arbitrary tar")); e == nil || invalid.created {
		t.Fatal("arbitrary bootstrap accepted")
	}
}
func TestProviderSDKConfigCopiesAreIsolated(t *testing.T) {
	e := fakeEngine(t, func(*http.Request) (*http.Response, error) { t.Fatal("construction must not request"); return nil, nil }, false)
	a := fixtureConfig()
	b := a
	b.FleetID = "other"
	p, q := New(e, a), New(e, b)
	if p.engine == q.engine || p.engine == e || q.engine == e || p.engine.(*sdkEngine).cfg.FleetID != "fleet" || q.engine.(*sdkEngine).cfg.FleetID != "other" || e.(*sdkEngine).cfg.FleetID != "" {
		t.Fatal("shared adapter config mutated")
	}
}
