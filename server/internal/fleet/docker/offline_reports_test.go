package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
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
	recoveryPhase                                                                    string
	accumulateBootstrap                                                              bool
	helperID                                                                         string
	recoveryInspects                                                                 int
	nodeInspectCount                                                                 int
	nodeMutationAt                                                                   int
	daemonTime                                                                       string
	waitEntered                                                                      chan struct{}
	leftovers                                                                        map[string]container.InspectResponse
	recovered                                                                        []string
	helperName                                                                       string
	crashStage                                                                       string
	crashCleanup                                                                     bool
	cleanupPhase                                                                     string
	attemptDeadline                                                                  time.Time
	cleanupExited                                                                    chan struct{}
	waitExited                                                                       chan struct{}
	cleanupBudgetBad                                                                 bool
	nodeMutation                                                                     func(*container.InspectResponse)
	afterLogs                                                                        func()
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
	for id, snapshot := range s.leftovers {
		if r.Method == "GET" && path == "/containers/"+id+"/json" {
			if s.recoveryPhase == "inspect" {
				return s.stalledCleanup(r)
			}
			s.recoveryInspects++
			raw, _ := json.Marshal(snapshot)
			return response(200, string(raw)), nil
		}
		if r.Method == "DELETE" && path == "/containers/"+id {
			if s.recoveryPhase == "remove" {
				return s.stalledCleanup(r)
			}
			delete(s.leftovers, id)
			s.recovered = append(s.recovered, id)
			return response(204, ""), nil
		}
	}
	if s.accumulateBootstrap {
		if s.helperID != "" {
			path = strings.Replace(path, "/containers/"+s.helperID, "/containers/helper", 1)
		}
		if r.Method == "GET" && strings.HasPrefix(path, "/networks/") {
			raw, _ := json.Marshal(map[string]any{"Name": (&Provider{cfg: fixtureConfig()}).networkName(), "Id": "network-id", "Driver": "bridge", "Labels": labels("ns", "fleet", "namespace", "network")})
			return response(200, string(raw)), nil
		}
		if r.Method == "GET" && strings.HasPrefix(path, "/containers/multica-fleet-") {
			return response(404, `{ "message":"missing" }`), nil
		}
		if r.Method == "DELETE" && path == "/containers/helper" {
			c := s.leftoverSnapshot()
			c.ID = s.helperID
			c.State = &container.State{Status: "exited"}
			s.leftovers[c.ID] = c
			return nil, context.DeadlineExceeded
		}
	}
	if s.crashCleanup && ((r.Method == "DELETE" && path == "/containers/helper") || (s.crashStage == "create" && r.Method == "GET" && strings.HasSuffix(path, "/json") && path != "/containers/cid/json")) {
		return nil, context.DeadlineExceeded
	}
	switch {
	case r.Method == "GET" && path == "/info":
		if s.recoveryPhase == "info" {
			return s.stalledCleanup(r)
		}
		if s.recoveryPhase == "record" {
			s.attemptDeadline, _ = r.Context().Deadline()
		}
		now := s.daemonTime
		if now == "" {
			now = time.Now().Format(time.RFC3339Nano)
		}
		raw, _ := json.Marshal(map[string]any{"SystemTime": now})
		return response(200, string(raw)), nil
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
		n := fixtureNode()
		net := (&Provider{cfg: fixtureConfig()}).networkName()
		h := NodeHostConfig(n.Resources, true)
		h.NetworkMode = container.NetworkMode(net)
		snapshot := container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "cid", State: &container.State{Status: "exited"}, HostConfig: &h}, Config: &container.Config{Image: n.Image, Labels: l, User: "10001:10001", Env: []string{"HOME=/data/home", "FLEET_NODE_MAX_RUNS=1"}, Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"run"}}, NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{net: {}}}, Mounts: []container.MountPoint{{Type: mount.TypeVolume, Name: name, Destination: "/data", RW: true}, {Type: mount.TypeVolume, Name: n.SecretsVolume, Destination: "/secrets"}}}
		s.nodeInspectCount++
		if s.nodeMutation != nil && s.nodeInspectCount >= s.nodeMutationAt {
			s.nodeMutation(&snapshot)
		}
		raw, _ := json.Marshal(snapshot)
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/containers/json":
		if r.URL.Query().Get("all") != "1" {
			s.t.Fatal("writer inventory must include all containers")
		}
		if s.writer {
			return response(200, `[{"Id":"foreign-writer","State":"running","Mounts":[{"Type":"volume","Name":"data-vol","Destination":"/elsewhere","RW":true}]}]`), nil
		}
		items := []map[string]any{}
		for id, c := range s.leftovers {
			items = append(items, map[string]any{"Id": id, "Labels": c.Config.Labels, "State": c.State.Status})
		}
		raw, _ := json.Marshal(items)
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/containers/foreign-writer/json":
		return response(200, `{"Id":"foreign-writer","State":{"Running":true,"Status":"running"},"Config":{"Labels":{}},"Mounts":[{"Type":"volume","Name":"data-vol","Destination":"/elsewhere","RW":true}]}`), nil
	case r.Method == "POST" && path == "/containers/create":
		var req container.CreateRequest
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			s.t.Fatal(e)
		}
		s.helperName = r.URL.Query().Get("name")
		s.helperConfig = req.Config
		s.helperHost = req.HostConfig
		s.created = true
		s.helperCreates++
		if s.crashStage == "create" {
			s.crashCleanup = true
			return nil, context.DeadlineExceeded
		}
		if s.createTimeout {
			return nil, context.DeadlineExceeded
		}
		if s.accumulateBootstrap {
			s.helperID = fmt.Sprintf("adapter-%04d", s.helperCreates)
			return response(201, fmt.Sprintf(`{"Id":%q}`, s.helperID)), nil
		}
		return response(201, `{"Id":"helper"}`), nil
	case r.Method == "GET" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
		if !s.created || s.helperMissing {
			return response(404, `{"message":"no such helper"}`), nil
		}
		if s.started && s.cleanupPhase == "inspect" {
			return s.stalledCleanup(r)
		}
		c := *s.helperConfig
		if s.cleanupForeign && s.started {
			c.Labels = fixtureLabels("other")
		}
		mounts := []map[string]any{}
		for _, m := range s.helperHost.Mounts {
			mounts = append(mounts, map[string]any{"Type": m.Type, "Name": m.Source, "Destination": m.Target, "RW": !m.ReadOnly})
		}
		raw, _ := json.Marshal(map[string]any{"Id": "helper", "State": map[string]any{"Status": "exited", "Running": false}, "Config": c, "HostConfig": s.helperHost, "NetworkSettings": map[string]any{"Networks": map[string]any{}}, "Mounts": mounts})
		if s.accumulateBootstrap {
			raw = bytes.Replace(raw, []byte(`"Id":"helper"`), []byte(fmt.Sprintf(`"Id":%q`, s.helperID)), 1)
		}
		return response(200, string(raw)), nil
	case r.Method == "POST" && path == "/containers/helper/start":
		s.started = true
		if s.crashStage == "start" {
			s.crashCleanup = true
			return nil, context.DeadlineExceeded
		}
		return response(204, ""), nil
	case r.Method == "POST" && path == "/containers/helper/wait":
		if r.URL.Query().Get("condition") != "not-running" {
			s.t.Fatal("wrong wait condition")
		}
		s.waitDeadline, _ = r.Context().Deadline()
		if s.crashStage == "wait" {
			s.crashCleanup = true
			return nil, context.DeadlineExceeded
		}
		if s.waitEntered != nil {
			close(s.waitEntered)
		}
		if s.waitBlocked {
			<-r.Context().Done()
			if s.waitExited != nil {
				close(s.waitExited)
			}
			return nil, r.Context().Err()
		}
		if s.crashStage == "cleanup" {
			s.crashCleanup = true
		}
		raw, _ := json.Marshal(map[string]any{"StatusCode": s.exit})
		return response(200, string(raw)), nil
	case r.Method == "GET" && path == "/containers/helper/logs":
		if r.URL.Query().Get("stdout") != "1" || r.URL.Query().Get("stderr") != "1" {
			s.t.Fatal("must account for both streams")
		}
		if s.crashStage == "cleanup" {
			s.crashCleanup = true
		}
		if s.afterLogs != nil {
			s.afterLogs()
		}
		return response(200, framed(s.output)), nil
	case r.Method == "DELETE" && path == "/containers/cid":
		s.nodeRemoved = true
		s.removalOrder = append(s.removalOrder, "container")
		return response(204, ""), nil
	case r.Method == "DELETE" && path == "/containers/helper":
		if s.cleanupPhase == "remove" {
			return s.stalledCleanup(r)
		}
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
func (s *offlineHTTP) stalledCleanup(r *http.Request) (*http.Response, error) {
	defer close(s.cleanupExited)
	d, ok := r.Context().Deadline()
	if !ok || d.After(s.attemptDeadline) {
		s.cleanupBudgetBad = true
		return nil, context.DeadlineExceeded
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestOfflinePrimaryDeleteRecoverySharesProofBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, 100 * time.Millisecond} {
		for _, phase := range []string{"info", "inspect", "remove"} {
			t.Run(fmt.Sprintf("%v/%s", budget, phase), func(t *testing.T) {
				s := &offlineHTTP{}
				p := offlineProvider(t, s)
				if _, err := p.Diagnose(context.Background(), fixtureNode(), fixtureRef()); err != nil {
					t.Fatal(err)
				}
				c := s.leftoverSnapshot()
				s.leftovers = map[string]container.InspectResponse{c.ID: c}
				q := offlineProvider(t, s)
				clock := time.Now()
				q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
				n := fixtureNode()
				n.Revoked = true
				n.Desired = "terminating"
				if err := q.Delete(context.Background(), n, fixtureRef()); err == nil || len(s.recovered) != 0 {
					t.Fatal("warm observation granted proof")
				}
				clock = clock.Add(helperQuiescence)
				s.recoveryPhase = phase
				s.cleanupExited = make(chan struct{})
				ctx := context.Background()
				cancel := func() {}
				start := time.Now()
				s.attemptDeadline = start.Add(5100 * time.Millisecond)
				if budget != 0 {
					ctx, cancel = context.WithTimeout(ctx, budget)
					s.attemptDeadline, _ = ctx.Deadline()
				}
				defer cancel()
				creates := s.helperCreates
				err := q.Delete(ctx, n, fixtureRef())
				limit := 5500 * time.Millisecond
				if budget != 0 {
					limit = 300 * time.Millisecond
				}
				if err == nil || s.cleanupBudgetBad || time.Since(start) > limit || s.volumeDeletes != 0 || s.nodeRemoved || len(s.recovered) != 0 || s.helperCreates != creates {
					t.Fatalf("primary recovery escaped original proof budget: err=%v bad=%v elapsed=%v", err, s.cleanupBudgetBad, time.Since(start))
				}
				select {
				case <-s.cleanupExited:
				default:
					t.Fatal("recovery transport peer remains blocked")
				}
			})
		}
	}
}

func TestOfflinePrimaryDeleteRecoveryExecutionCleanupOneDeadline(t *testing.T) {
	for _, budget := range []time.Duration{0, 100 * time.Millisecond} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			s := &offlineHTTP{}
			p := offlineProvider(t, s)
			if _, err := p.Diagnose(context.Background(), fixtureNode(), fixtureRef()); err != nil {
				t.Fatal(err)
			}
			c := s.leftoverSnapshot()
			s.leftovers = map[string]container.InspectResponse{c.ID: c}
			q := offlineProvider(t, s)
			clock := time.Now()
			q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
			n := fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			_ = q.Delete(context.Background(), n, fixtureRef())
			clock = clock.Add(helperQuiescence)
			s.recoveryPhase = "record"
			s.cleanupPhase = "remove"
			s.cleanupExited = make(chan struct{})
			s.waitBlocked = true
			s.waitExited = make(chan struct{})
			ctx := context.Background()
			cancel := func() {}
			if budget != 0 {
				ctx, cancel = context.WithTimeout(ctx, budget)
			}
			defer cancel()
			start := time.Now()
			err := q.Delete(ctx, n, fixtureRef())
			if err == nil || s.cleanupBudgetBad || s.attemptDeadline.IsZero() || s.waitDeadline.After(s.attemptDeadline) || time.Since(start) > 5500*time.Millisecond || s.volumeDeletes != 0 || s.nodeRemoved {
				t.Fatalf("recovery and execution/cleanup received different proof budgets: %v", err)
			}
			select {
			case <-s.cleanupExited:
			default:
				t.Fatal("cleanup peer remains blocked")
			}
			select {
			case <-s.waitExited:
			default:
				t.Fatal("execution peer remains blocked")
			}
		})
	}
}

func TestOfflineWholeAttemptIncludesStalledCleanup(t *testing.T) {
	for _, phase := range []string{"inspect", "remove"} {
		t.Run(phase, func(t *testing.T) {
			s := &offlineHTTP{cleanupPhase: phase, waitBlocked: true, cleanupExited: make(chan struct{}), waitExited: make(chan struct{})}
			p := offlineProvider(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			s.attemptDeadline, _ = ctx.Deadline()
			start := time.Now()
			o, e := p.Diagnose(ctx, fixtureNode(), fixtureRef())
			if e == nil || o.ReportStatsKnown || s.cleanupBudgetBad || time.Since(start) > 300*time.Millisecond {
				t.Fatalf("whole cleanup deadline escaped: err=%v bad=%v elapsed=%v", e, s.cleanupBudgetBad, time.Since(start))
			}
			select {
			case <-s.cleanupExited:
			default:
				t.Fatal("owned cleanup peer remains blocked")
			}
			select {
			case <-s.waitExited:
			default:
				t.Fatal("owned wait peer remains blocked")
			}
			if s.volumeDeletes != 0 || s.nodeRemoved {
				t.Fatal("unknown cleanup mutated data/node")
			}
		})
	}
}
func (s *offlineHTTP) leftoverSnapshot() container.InspectResponse {
	h := *s.helperHost
	c := *s.helperConfig
	r := container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "crashed-helper", Name: "/" + s.helperName, Created: time.Now().Add(-2 * time.Minute).Format(time.RFC3339Nano), State: &container.State{Status: "running", Running: true, StartedAt: "2026-01-01T00:00:00Z"}, HostConfig: &h}, Config: &c, NetworkSettings: &container.NetworkSettings{}}
	for _, m := range h.Mounts {
		r.Mounts = append(r.Mounts, container.MountPoint{Type: m.Type, Name: m.Source, Destination: m.Target, RW: !m.ReadOnly})
	}
	return r
}

func TestOfflineDeleteAccumulatedBootstrapHelpersProgressInBoundedBatches(t *testing.T) {
	for _, count := range []int{6, 129} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := &offlineHTTP{accumulateBootstrap: true, leftovers: map[string]container.InspectResponse{}}
			p := offlineProvider(t, s)
			n := fixtureNode()
			n.ContainerID = ""
			n.Maintenance = false
			n.Desired = "running"
			b := model.Bootstrap{NodeToken: "fake-token", APIKey: "fake-key", DaemonID: n.DaemonID, ServerURL: fixtureConfig().APIURL}
			s.archive, _ = bootstrapTar(n, fixtureConfig(), b)
			for i := 0; i < count; i++ {
				if _, err := p.Ensure(context.Background(), n, b); err == nil {
					t.Fatal("cleanup failure unexpectedly succeeded")
				}
			}
			if len(s.leftovers) != count || !s.copied || s.helperCreates != count {
				t.Fatalf("adapter accumulation failed: %d", len(s.leftovers))
			}
			s.accumulateBootstrap = false
			s.created = false
			s.started = false
			s.helperID = ""
			n = fixtureNode()
			n.Maintenance = true
			n.Revoked = true
			n.Desired = "terminating"
			q := offlineProvider(t, s)
			clock := time.Now()
			q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
			if err := q.Delete(context.Background(), n, fixtureRef()); err == nil || len(s.recovered) != 0 || s.volumeDeletes != 0 {
				t.Fatal("first fresh Engine observation must remain unknown")
			}
			done := false
			for attempt := 0; attempt < count*3; attempt++ {
				clock = clock.Add(helperQuiescence)
				before := len(s.recovered)
				inspections := s.recoveryInspects
				creates := s.helperCreates
				err := New(q.engine, fixtureConfig()).Delete(context.Background(), n, fixtureRef())
				if len(q.engine.(*sdkEngine).helpers.seen) > 128 {
					t.Fatal("lifecycle registry exceeded bound")
				}
				if len(s.recovered)-before > 5 || s.recoveryInspects-inspections > 10 {
					t.Fatal("per-attempt recovery work exceeded bounded batch")
				}
				if len(s.leftovers) > 0 && (err == nil || s.volumeDeletes != 0 || s.nodeRemoved || s.helperCreates != creates) {
					t.Fatal("partial cleanup fabricated completion/proof")
				}
				if err == nil {
					done = true
					break
				}
			}
			if !done || len(s.recovered) != count || len(s.leftovers) != 0 || strings.Join(s.removalOrder, ",") != "container,secrets,data" || s.helperCreates != count+2 {
				t.Fatalf("bounded eventual fresh-proof recovery failed: recovered=%d remaining=%d order=%v", len(s.recovered), len(s.leftovers), s.removalOrder)
			}
		})
	}
}

func TestOfflineMixedBatchPreservesBadHelpersWithoutStarvingNeighbors(t *testing.T) {
	for _, kind := range []string{"tampered", "current", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{accumulateBootstrap: true, leftovers: map[string]container.InspectResponse{}}
			p := offlineProvider(t, s)
			n := fixtureNode()
			n.ContainerID = ""
			n.Maintenance = false
			n.Desired = "running"
			b := model.Bootstrap{NodeToken: "fake-token", APIKey: "fake-key", DaemonID: n.DaemonID, ServerURL: fixtureConfig().APIURL}
			s.archive, _ = bootstrapTar(n, fixtureConfig(), b)
			for i := 0; i < 10; i++ {
				if _, err := p.Ensure(context.Background(), n, b); err == nil {
					t.Fatal("expected cleanup failure")
				}
			}
			if len(s.leftovers) != 10 {
				t.Fatal("missing actual adapter leftovers")
			}
			s.accumulateBootstrap = false
			s.created = false
			s.started = false
			q := offlineProvider(t, s)
			clock := time.Now()
			q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
			n = fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			// Observe all members before corrupting the early-ID helper.
			for i := 0; i < 2; i++ {
				_ = q.Delete(context.Background(), n, fixtureRef())
			}
			bad := s.leftovers["adapter-0001"]
			if kind == "tampered" {
				bad.Config.User = "root"
			}
			if kind == "foreign" {
				bad.Config.Labels["multica.fleet.namespace"] = "foreign"
			}
			s.leftovers[bad.ID] = bad
			for i := 0; i < 12; i++ {
				clock = clock.Add(helperQuiescence)
				if kind == "current" {
					bad.State = &container.State{Status: "running", Running: true, StartedAt: clock.Format(time.RFC3339Nano)}
					s.daemonTime = clock.Format(time.RFC3339Nano)
					s.leftovers[bad.ID] = bad
				}
				before := len(s.recovered)
				inspects := s.recoveryInspects
				if err := q.Delete(context.Background(), n, fixtureRef()); err == nil {
					t.Fatal("bad helper fabricated all-absence")
				}
				if len(s.recovered)-before > 5 || s.recoveryInspects-inspects > 10 || s.volumeDeletes != 0 || s.nodeRemoved {
					t.Fatal("mixed batch escaped bounds/preservation")
				}
				if _, ok := s.leftovers[bad.ID]; !ok {
					t.Fatal("bad/current helper removed")
				}
			}
			if kind != "foreign" && (len(s.recovered) != 9 || len(s.leftovers) != 1) {
				t.Fatalf("early bad helper starved its verified neighbors: recovered=%d", len(s.recovered))
			}
		})
	}
}

func TestOfflineFreshDeleteRecoversAdapterCreatedHelpers(t *testing.T) {
	for _, role := range []string{"diagnostic", "bootstrap"} {
		for _, stage := range []string{"create", "start", "wait", "cleanup"} {
			t.Run(role+"/"+stage, func(t *testing.T) {
				s := &offlineHTTP{crashStage: stage}
				p := offlineProvider(t, s)
				n := fixtureNode()
				if role == "diagnostic" {
					_, _ = p.Diagnose(context.Background(), n, fixtureRef())
				} else {
					raw, e := bootstrapTar(n, fixtureConfig(), model.Bootstrap{NodeToken: "fake-token", APIKey: "fake-key", DaemonID: n.DaemonID, ServerURL: fixtureConfig().APIURL})
					if e != nil {
						t.Fatal(e)
					}
					s.archive = raw
					_ = p.engine.InstallBootstrap(context.Background(), []Resource{p.volume(n, "data"), p.volume(n, "secrets")}, raw)
				}
				if !s.created || s.removed {
					t.Fatal("fixture did not leave adapter-created helper")
				}
				s.leftovers = map[string]container.InspectResponse{"crashed-helper": s.leftoverSnapshot()}
				s.crashStage = ""
				s.crashCleanup = false
				n.Revoked = true
				n.Desired = "terminating"
				q := offlineProvider(t, s)
				base := time.Now()
				clock := base
				q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
				if e := q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 || s.volumeDeletes != 0 {
					t.Fatalf("first observation must preserve resources: %v", e)
				}
				clock = base.Add(helperQuiescence)
				q = New(q.engine, fixtureConfig()) // Fresh Provider shares only the explicit Engine lifecycle arbitration.
				if e := q.Delete(context.Background(), n, fixtureRef()); e != nil || len(s.recovered) != 1 || !s.volumeMissing || strings.Join(s.removalOrder, ",") != "container,secrets,data" {
					t.Fatalf("fresh same-ref recovery failed: %v recovered=%v order=%v", e, s.recovered, s.removalOrder)
				}
				if e := offlineProvider(t, s).Delete(context.Background(), n, fixtureRef()); e != nil {
					t.Fatalf("same ref completed replay: %v", e)
				}
			})
		}
	}
}
func TestOfflineRecoveryWaitResetsAndNeverCachesProof(t *testing.T) {
	for _, kind := range []string{"restart", "new-engine", "wall-jump", "current-pending", "multiple-roles"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{}
			p := offlineProvider(t, s)
			_, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
			if e != nil {
				t.Fatal(e)
			}
			c := s.leftoverSnapshot()
			s.leftovers = map[string]container.InspectResponse{"crashed-helper": c}
			if kind == "multiple-roles" {
				b := s.leftoverSnapshot()
				b.ID = "bootstrap-leftover"
				b.Config = &container.Config{Image: fixtureConfig().Image, User: "10001:10001", Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"bootstrap"}, Labels: fixtureLabels("bootstrap"), NetworkDisabled: true}
				bh := diagnosticHost()
				bh.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "data-vol", Target: "/data"}, {Type: mount.TypeVolume, Source: "secrets-vol", Target: "/secrets"}}
				b.HostConfig = &bh
				b.Mounts = []container.MountPoint{{Type: mount.TypeVolume, Name: "data-vol", Destination: "/data", RW: true}, {Type: mount.TypeVolume, Name: "secrets-vol", Destination: "/secrets", RW: true}}
				s.leftovers[b.ID] = b
			}
			q := offlineProvider(t, s)
			base := time.Now()
			clock := base
			q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
			n := fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			if e = q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 {
				t.Fatal("first observation granted cleanup")
			}
			if kind == "wall-jump" {
				s.daemonTime = time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339Nano)
				if e = q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 {
					t.Fatal("daemon wallclock aged monotonic observation")
				}
			}
			clock = base.Add(helperQuiescence)
			if kind == "restart" {
				c.State.StartedAt = "2026-02-01T00:00:00Z"
				s.leftovers[c.ID] = c
			}
			if kind == "new-engine" {
				q = offlineProvider(t, s)
				q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
			}
			if kind == "restart" || kind == "new-engine" {
				if e = q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 {
					t.Fatal("changed lifecycle/process reused elapsed permission")
				}
				clock = clock.Add(helperQuiescence)
			}
			if kind == "current-pending" {
				s.output = strings.Replace(goodOffline, "\"pending\":0", "\"pending\":1", 1)
			}
			e = q.Delete(context.Background(), n, fixtureRef())
			if kind == "current-pending" {
				if e == nil || len(s.recovered) != 1 || s.volumeDeletes != 0 || s.nodeRemoved {
					t.Fatalf("recovery substituted old proof: %v", e)
				}
			} else if e != nil || !s.volumeMissing {
				t.Fatalf("eventual recovery failed: %v", e)
			}
			if len(q.engine.(*sdkEngine).helpers.seen) != 0 {
				t.Fatal("removed helper observation retained")
			}
		})
	}
}
func TestOfflineDefaultBudgetIncludesStalledCleanup(t *testing.T) {
	for _, phase := range []string{"inspect", "remove"} {
		t.Run(phase, func(t *testing.T) {
			s := &offlineHTTP{cleanupPhase: phase, waitBlocked: true, cleanupExited: make(chan struct{}), waitExited: make(chan struct{})}
			p := offlineProvider(t, s)
			start := time.Now()
			s.attemptDeadline = start.Add(5100 * time.Millisecond)
			o, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
			if e == nil || o.ReportStatsKnown || s.cleanupBudgetBad || time.Since(start) > 5500*time.Millisecond {
				t.Fatalf("fixed5s whole attempt escaped: %v bad=%v elapsed=%v", e, s.cleanupBudgetBad, time.Since(start))
			}
			select {
			case <-s.cleanupExited:
			default:
				t.Fatal("cleanup peer not terminated")
			}
			select {
			case <-s.waitExited:
			default:
				t.Fatal("wait peer not terminated")
			}
		})
	}
}
func TestOfflineCurrentCallHelperSurvivesConcurrentFreshProvider(t *testing.T) {
	s := &offlineHTTP{waitBlocked: true, waitEntered: make(chan struct{}), waitExited: make(chan struct{})}
	p := offlineProvider(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = p.Diagnose(ctx, fixtureNode(), fixtureRef()) }()
	select {
	case <-s.waitEntered:
	case <-ctx.Done():
		t.Fatal("current helper did not enter wait")
	}
	c := s.leftoverSnapshot()
	c.ID = "helper"
	c.Created = time.Now().Format(time.RFC3339Nano)
	s.leftovers = map[string]container.InspectResponse{"helper": c}
	n := fixtureNode()
	n.Revoked = true
	n.Desired = "terminating"
	q := New(p.engine, fixtureConfig())
	if e := q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 || s.removed || s.nodeRemoved || s.volumeDeletes != 0 {
		t.Fatalf("concurrent active helper removed: %v", e)
	}
	s.leftovers = nil
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("owned caller/SDK wait did not terminate")
	}
	select {
	case <-s.waitExited:
	default:
		t.Fatal("wait transport peer remains blocked")
	}
}
func TestOfflineUnknownLeftoversPreserveAllResources(t *testing.T) {
	for _, kind := range []string{"fresh-active", "future", "bad-time", "missing-time", "foreign", "missing-label", "unsafe", "wrong-mount", "wrong-image", "unknown-role", "bad-argv", "credential", "wrong-id", "arbitrary-name", "network", "stdin", "cpu", "memory", "pids", "root-write", "caps", "security", "data-rw", "user", "network-snapshot", "mount-extra", "restart", "bad-start", "missing-start", "state-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			s := &offlineHTTP{}
			p := offlineProvider(t, s)
			_, e := p.Diagnose(context.Background(), fixtureNode(), fixtureRef())
			if e != nil {
				t.Fatal(e)
			}
			c := s.leftoverSnapshot()
			s.leftovers = map[string]container.InspectResponse{"crashed-helper": c}
			q := offlineProvider(t, s)
			base := time.Now()
			clock := base
			q.engine.(*sdkEngine).helpers.now = func() time.Time { return clock }
			n := fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			if e := q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 {
				t.Fatal("initial observation removed helper")
			}
			switch kind {
			case "fresh-active":
				c.Created = time.Now().Format(time.RFC3339Nano)
			case "future":
				c.Created = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			case "bad-time":
				c.Created = "invalid"
			case "missing-time":
				c.Created = ""
			case "arbitrary-name":
				c.Name = "/not-an-adapter-helper"
			case "network":
				c.Config.NetworkDisabled = false
			case "stdin":
				c.Config.OpenStdin = true
			case "bad-start":
				c.State.StartedAt = "invalid"
			case "missing-start":
				c.State.StartedAt = ""
			case "state-mismatch":
				c.State.Running = false
			case "cpu":
				c.HostConfig.NanoCPUs++
			case "memory":
				c.HostConfig.Memory++
			case "pids":
				*c.HostConfig.PidsLimit++
			case "root-write":
				c.HostConfig.ReadonlyRootfs = false
			case "caps":
				c.HostConfig.CapDrop = nil
			case "security":
				c.HostConfig.SecurityOpt = nil
			case "data-rw":
				c.Mounts[0].RW = true
			case "user":
				c.Config.User = "root"
			case "network-snapshot":
				c.NetworkSettings = nil
			case "mount-extra":
				c.Mounts = append(c.Mounts, container.MountPoint{Type: mount.TypeBind, Destination: "/socket"})
			case "restart":
				c.HostConfig.RestartPolicy.Name = "always"
			case "foreign":
				c.Config.Labels["multica.fleet.namespace"] = "foreign"
			case "missing-label":
				delete(c.Config.Labels, "multica.fleet.fleet_id")
			case "unsafe":
				c.HostConfig.Privileged = true
			case "wrong-mount":
				c.Mounts[0].Name = "foreign"
			case "wrong-image":
				c.Config.Image = "other"
			case "unknown-role":
				c.Config.Labels["multica.fleet.role"] = "other"
			case "bad-argv":
				c.Config.Cmd = []string{"other"}
			case "credential":
				c.Config.Env = []string{"API_KEY=private"}
			case "wrong-id":
				c.ID = "replacement"
			}
			s.leftovers = map[string]container.InspectResponse{"crashed-helper": c}
			for attempt := 1; attempt <= 2; attempt++ {
				if kind != "fresh-active" {
					clock = base.Add(time.Duration(attempt) * helperQuiescence)
				}
				if e := q.Delete(context.Background(), n, fixtureRef()); e == nil || len(s.recovered) != 0 || s.nodeRemoved || s.volumeDeletes != 0 {
					t.Fatalf("unknown leftover mutated resources on expired retry%d: %v", attempt, e)
				}
			}
		})
	}
}
func offlineProvider(t *testing.T, s *offlineHTTP) *Provider {
	s.t = t
	if s.output == "" {
		s.output = goodOffline
	}
	return New(fakeEngine(t, s.roundTrip, false), fixtureConfig())
}
func TestOfflineStoppedSnapshotTamperingNeverProvesOrMutates(t *testing.T) {
	mutations := map[string]func(*container.InspectResponse){
		"user":       func(r *container.InspectResponse) { r.Config.User = "root" },
		"cpu":        func(r *container.InspectResponse) { r.HostConfig.NanoCPUs++ },
		"memory":     func(r *container.InspectResponse) { r.HostConfig.Memory++ },
		"pids":       func(r *container.InspectResponse) { *r.HostConfig.PidsLimit++ },
		"env":        func(r *container.InspectResponse) { r.Config.Env = append(r.Config.Env, "API_KEY=private") },
		"network":    func(r *container.InspectResponse) { r.HostConfig.NetworkMode = "host" },
		"secret-rw":  func(r *container.InspectResponse) { r.Mounts[1].RW = true },
		"data-ro":    func(r *container.InspectResponse) { r.Mounts[0].RW = false },
		"privileged": func(r *container.InspectResponse) { r.HostConfig.Privileged = true },
		"caps":       func(r *container.InspectResponse) { r.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
		"security":   func(r *container.InspectResponse) { r.HostConfig.SecurityOpt = nil },
		"restart":    func(r *container.InspectResponse) { r.HostConfig.RestartPolicy.Name = "always" },
		"argv":       func(r *container.InspectResponse) { r.Config.Cmd = []string{"other"} },
		"image":      func(r *container.InspectResponse) { r.Config.Image = "other" },
		"read-only":  func(r *container.InspectResponse) { r.HostConfig.ReadonlyRootfs = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := &offlineHTTP{nodeMutation: mutate}
			p := offlineProvider(t, s)
			n := fixtureNode()
			o, e := p.Diagnose(context.Background(), n, fixtureRef())
			if e == nil || o.ReportStatsKnown || s.created {
				t.Fatalf("tampered snapshot diagnosed: %v %+v", e, o)
			}
			n.Revoked = true
			n.Desired = "terminating"
			if e = p.Delete(context.Background(), n, fixtureRef()); e == nil || s.created || s.nodeRemoved || s.volumeDeletes != 0 {
				t.Fatalf("tampered node mutated: %v", e)
			}
		})
	}
}
func TestOfflineStoppedSnapshotRecheckedAfterProof(t *testing.T) {
	for _, at := range []int{4, 5} {
		t.Run(string(rune('0'+at)), func(t *testing.T) {
			s := &offlineHTTP{nodeMutationAt: at, nodeMutation: func(r *container.InspectResponse) { r.Config.User = "root" }}
			p := offlineProvider(t, s)
			n := fixtureNode()
			n.Revoked = true
			n.Desired = "terminating"
			if e := p.Delete(context.Background(), n, fixtureRef()); e == nil || !s.created || s.nodeRemoved || s.volumeDeletes != 0 || s.nodeInspectCount < at {
				t.Fatalf("postproof snapshot changed at inspect%d: %v count=%d", at, e, s.nodeInspectCount)
			}
		})
	}
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
