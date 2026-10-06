package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func fakeEngine(t *testing.T, f roundTrip, negotiation bool) Engine {
	t.Helper()
	opts := []client.Opt{client.WithHost("tcp://docker.invalid:2375"), client.WithHTTPClient(&http.Client{Transport: f, CheckRedirect: client.CheckRedirect})}
	if negotiation {
		opts = append(opts, client.WithAPIVersionNegotiation())
	} else {
		opts = append(opts, client.WithVersion("1.51"))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return NewEngine(c)
}
func fixtureNode() model.Node {
	return model.Node{ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, OwnerID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true}, Namespace: "ns", ContainerID: "cid", DaemonID: "04000000-0000-0000-0000-000000000000", DataVolume: "data-vol", SecretsVolume: "secrets-vol", Image: "node-snapshot", Generation: 3, Maintenance: true, Status: "stopped", Resources: model.Spec{CPUs: 2, MemoryBytes: 4 << 30, Pids: 256, MaxRuns: 1}}
}
func fixtureConfig() model.Config {
	return model.Config{Namespace: "ns", FleetID: "fleet", Image: "example/node@sha256:" + strings.Repeat("a", 64), APIURL: "http://host.docker.internal:18476"}
}
func fixtureRef() model.OperationRef {
	n := fixtureNode()
	return model.OperationRef{Namespace: n.Namespace, NodeID: n.ID, OperationID: pgtype.UUID{Bytes: [16]byte{3}, Valid: true}, Generation: n.Generation, Action: model.Delete}
}
func fixtureLabels(role string) map[string]string {
	return map[string]string{"multica.fleet.namespace": "ns", "multica.fleet.fleet_id": "fleet", "multica.fleet.node": "01000000-0000-0000-0000-000000000000", "multica.fleet.role": role}
}

// The original HTTP transport serves ordinary SDK calls and the fixed upgrade via net.Pipe.
func pipeEngine(t *testing.T, f func(*http.Request) (*http.Response, string)) Engine {
	t.Helper()
	var mu sync.Mutex
	var peers []net.Conn
	var done []chan struct{}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" || addr != "docker.invalid:2375" {
			t.Errorf("unexpected dial %s %s", network, addr)
			return nil, fmt.Errorf("unexpected dial")
		}
		a, b := net.Pipe()
		a.SetDeadline(time.Now().Add(5 * time.Second))
		b.SetDeadline(time.Now().Add(5 * time.Second))
		closed := make(chan struct{})
		mu.Lock()
		peers = append(peers, a, b)
		done = append(done, closed)
		mu.Unlock()
		go func() {
			defer close(closed)
			defer b.Close()
			reader := bufio.NewReader(b)
			for {
				req, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				res, stream := f(req)
				io.Copy(io.Discard, req.Body)
				req.Body.Close()
				if res == nil {
					return
				}
				res.ProtoMajor = 1
				res.ProtoMinor = 1
				if stream != "" {
					fmt.Fprint(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
					io.Copy(b, strings.NewReader(stream))
					return
				}
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				res.Body = io.NopCloser(bytes.NewReader(body))
				res.ContentLength = int64(len(body))
				if err = res.Write(b); err != nil {
					return
				}
			}
		}()
		return a, nil
	}}
	c, err := client.NewClientWithOpts(client.WithHost("tcp://docker.invalid:2375"), client.WithHTTPClient(&http.Client{Transport: tr, CheckRedirect: client.CheckRedirect}), client.WithVersion("1.51"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		tr.CloseIdleConnections()
		mu.Lock()
		for _, p := range peers {
			p.Close()
		}
		wait := append([]chan struct{}{}, done...)
		mu.Unlock()
		for _, d := range wait {
			select {
			case <-d:
			case <-time.After(time.Second):
				t.Error("fake pipe peer leaked")
			}
		}
	})
	return NewEngine(c)
}
func TestEngineFixedHealthExecIsSynchronousBoundedAndFixed(t *testing.T) {
	for _, kind := range []string{"success", "nonzero", "running", "stderr", "oversized", "truncated", "hijack-error"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			exec := false
			e := pipeEngine(t, func(r *http.Request) (*http.Response, string) {
				calls++
				switch r.URL.Path {
				case "/v1.51/containers/cid/exec":
					var options container.ExecOptions
					if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
						t.Error(err)
					}
					if r.Method != "POST" || strings.Join(options.Cmd, " ") != "/usr/local/bin/fleet-node health" || options.User != "10001:10001" || options.Privileged || options.Tty || len(options.Env) != 0 || options.AttachStdin || !options.AttachStdout || !options.AttachStderr {
						t.Error("unsafe fixed exec")
					}
					exec = true
					return response(201, `{"Id":"exec-id"}`), ""
				case "/v1.51/exec/exec-id/start":
					if r.Method != "POST" {
						t.Error("wrong hijack method")
					}
					if kind == "hijack-error" {
						return response(500, "failure"), ""
					}
					stream := framed(goodHealth)
					if kind == "stderr" {
						stream = "\x02\x00\x00\x00\x00\x00\x00\x01x"
					}
					if kind == "oversized" {
						stream = framed(strings.Repeat("x", 65537))
					}
					if kind == "truncated" {
						stream = framed(goodHealth) + "\x01"
					}
					return response(101, ""), stream
				case "/v1.51/exec/exec-id/json":
					exit, running := 0, false
					if kind == "nonzero" {
						exit = 2
					}
					if kind == "running" {
						running = true
					}
					raw, _ := json.Marshal(map[string]any{"ID": "exec-id", "ContainerID": "cid", "ExitCode": exit, "Running": running})
					return response(200, string(raw)), ""
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					return nil, ""
				}
			})
			raw, err := e.FixedHealth(context.Background(), "cid")
			if kind == "success" {
				if err != nil || string(raw) != goodHealth || !exec || calls != 3 {
					t.Fatalf("fresh fixed exec raw=%s err=%v calls=%d", raw, err, calls)
				}
			} else if err == nil {
				t.Fatalf("unsafe exec %s accepted", kind)
			}
		})
	}
}

func TestEngineFixedHealthHandshakeAndStreamCancellationClosesPeers(t *testing.T) {
	for _, kind := range []string{"handshake-write", "handshake-read", "stream"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			closed := make(chan struct{})
			var peer net.Conn
			tr := &http.Transport{Proxy: nil, DialContext: func(context.Context, string, string) (net.Conn, error) {
				a, b := net.Pipe()
				peer = b
				a.SetDeadline(time.Now().Add(time.Second))
				b.SetDeadline(time.Now().Add(time.Second))
				go func() {
					defer close(closed)
					defer b.Close()
					if kind == "handshake-write" {
						<-ctx.Done()
						return
					}
					req, e := http.ReadRequest(bufio.NewReader(b))
					if e != nil {
						return
					}
					io.Copy(io.Discard, req.Body)
					req.Body.Close()
					if kind == "stream" {
						fmt.Fprint(b, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					}
					io.Copy(io.Discard, b)
				}()
				return a, nil
			}}
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1.51/containers/cid/exec":
					return response(201, `{"Id":"exec-id"}`), nil
				case "/v1.51/exec/exec-id/start":
					return tr.RoundTrip(r)
				default:
					t.Errorf("unexpected stalled exec request %s", r.URL)
					return nil, fmt.Errorf("unexpected request")
				}
			}, false)
			start := time.Now()
			raw, err := e.FixedHealth(ctx, "cid")
			if err == nil || len(raw) != 0 || time.Since(start) > 250*time.Millisecond {
				t.Fatalf("unbounded canceled %s health e=%v elapsed=%v", kind, err, time.Since(start))
			}
			select {
			case <-closed:
			case <-time.After(250 * time.Millisecond):
				if peer != nil {
					peer.Close()
				}
				t.Fatal("owned fake peer leaked after cancellation")
			}
			tr.CloseIdleConnections()
		})
	}
}
func TestEngineFixedHealthRejectsForeignRedirectAndUnknownUpgrade(t *testing.T) {
	for _, kind := range []string{"redirect", "upgrade", "connection", "empty", "unknown-exec", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				calls++
				switch r.URL.Path {
				case "/v1.51/containers/cid/exec":
					return response(201, `{"Id":"exec-id"}`), nil
				case "/v1.51/exec/exec-id/start":
					if kind == "redirect" {
						res := response(302, "")
						res.Header.Set("Location", "http://foreign.invalid/steal")
						return res, nil
					}
					out := response(101, framed(goodHealth))
					out.Header.Set("Upgrade", "tcp")
					out.Header.Set("Connection", "Upgrade")
					if kind == "upgrade" {
						out.Header.Set("Upgrade", "other")
					}
					if kind == "connection" {
						out.Header.Set("Connection", "close")
					}
					if kind == "empty" {
						out.Body = io.NopCloser(strings.NewReader(""))
					}
					if kind == "malformed" {
						out.Body = io.NopCloser(strings.NewReader(framed("private malformed payload")))
					}
					return out, nil
				case "/v1.51/exec/exec-id/json":
					return response(200, `{"ID":"foreign-exec","ContainerID":"cid","ExitCode":0}`), nil
				default:
					t.Errorf("followed foreign request %s", r.URL)
					return nil, fmt.Errorf("unexpected request")
				}
			}, false)
			if _, err := e.FixedHealth(context.Background(), "cid"); err == nil || calls > 3 {
				t.Fatal("untrusted upgraded exec granted health")
			}
		})
	}
}
func TestEngineOwnershipVolumesAndSharedNetwork(t *testing.T) {
	for _, kind := range []string{"owned-volume", "foreign-volume", "missing-label", "missing-volume", "owned-network", "foreign-network", "already-removed-volume"} {
		t.Run(kind, func(t *testing.T) {
			creates, deletes := 0, 0
			missing := kind == "missing-volume" || kind == "already-removed-volume"
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.51")
				switch {
				case r.Method == "GET" && p == "/containers/json":
					return response(200, "[]"), nil
				case r.Method == "GET" && p == "/volumes/data-vol":
					if missing {
						return response(404, `{"message":"missing"}`), nil
					}
					l := fixtureLabels("data")
					if kind == "foreign-volume" {
						l["multica.fleet.namespace"] = "other"
					}
					if kind == "missing-label" {
						delete(l, "multica.fleet.fleet_id")
					}
					raw, _ := json.Marshal(map[string]any{"Name": "data-vol", "Driver": "local", "Labels": l})
					return response(200, string(raw)), nil
				case r.Method == "POST" && p == "/volumes/create":
					creates++
					missing = false
					return response(201, `{"Name":"data-vol","Driver":"local","Labels":{"multica.fleet.namespace":"ns","multica.fleet.fleet_id":"fleet","multica.fleet.node":"01000000-0000-0000-0000-000000000000","multica.fleet.role":"data"}}`), nil
				case r.Method == "GET" && p == "/networks/shared-net":
					l := fixtureLabels("network")
					l["multica.fleet.node"] = "namespace"
					if kind == "foreign-network" {
						l["multica.fleet.fleet_id"] = "foreign"
					}
					raw, _ := json.Marshal(map[string]any{"Id": "net-id", "Name": "shared-net", "Driver": "bridge", "Labels": l})
					return response(200, string(raw)), nil
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					return nil, fmt.Errorf("unexpected request")
				}
			}, false)
			p := New(e, fixtureConfig())
			r := Resource{ID: "data-vol", Name: "data-vol", Role: "data", Labels: fixtureLabels("data")}
			var err error
			if strings.Contains(kind, "network") {
				r = Resource{Name: "shared-net", Role: "network", Labels: fixtureLabels("network")}
				r.Labels["multica.fleet.node"] = "namespace"
				err = p.engine.EnsureNetwork(context.Background(), r)
			} else if kind == "already-removed-volume" {
				err = p.engine.RemoveVolume(context.Background(), r)
			} else {
				err = p.engine.EnsureVolume(context.Background(), r)
			}
			forbidden := kind == "foreign-volume" || kind == "missing-label" || kind == "foreign-network"
			if forbidden {
				if !errors.Is(err, model.ErrForbidden) {
					t.Fatalf("foreign ownership error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if kind == "missing-volume" && creates != 1 {
				t.Fatal("missing owned volume not created")
			}
			if kind != "missing-volume" && creates != 0 {
				t.Fatal("duplicate create")
			}
			if deletes != 0 {
				t.Fatal("unexpected removal")
			}
		})
	}
}
func TestEngineStopAlreadyStoppedIsIdempotent(t *testing.T) {
	calls := 0
	e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1.51/containers/cid/stop" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		return response(304, ""), nil
	}, false)
	if err := e.Stop(context.Background(), "cid"); err != nil || calls != 1 {
		t.Fatalf("stopped error=%v calls=%d", err, calls)
	}
}
func TestEngineCreateWireAndNegotiation(t *testing.T) {
	ping, create := false, false
	e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "HEAD" && r.URL.Path == "/_ping" {
			ping = true
			out := response(200, "")
			out.Header.Set("API-Version", "1.51")
			return out, nil
		}
		if r.Method != "POST" || r.URL.Path != "/v1.51/containers/create" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL)
			return nil, nil
		}
		if !ping || r.URL.Query().Get("name") != "fixed-node" {
			t.Fatal("negotiation/name missing")
		}
		var req struct {
			*container.Config
			HostConfig       container.HostConfig
			NetworkingConfig struct{ EndpointsConfig map[string]any }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.User != "10001:10001" || req.Image != "snapshot" || len(req.Env) != 0 || req.HostConfig.Memory != 4<<30 || req.HostConfig.NetworkMode != "node-net" || len(req.NetworkingConfig.EndpointsConfig) != 1 {
			t.Fatal("unsafe or changed create wire")
		}
		create = true
		return response(201, "{\"Id\":\"created\"}"), nil
	}, true)
	h := NodeHostConfig(fixtureNode().Resources, true, nil)
	h.NetworkMode = "node-net"
	id, err := e.Create(context.Background(), &container.Config{User: "10001:10001", Image: "snapshot"}, &h, "node-net", "fixed-node")
	if err != nil || id != "created" || !create || !ping {
		t.Fatalf("create id=%s error=%v ping=%v", id, err, ping)
	}
}
func TestEngineMissingRemovalIsIdempotent(t *testing.T) {
	calls := 0
	e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "DELETE" {
			t.Fatal("unexpected request")
		}
		return response(404, "{\"message\":\"No such container\"}"), nil
	}, false)
	if err := e.Remove(context.Background(), "gone"); err != nil || calls != 1 {
		t.Fatalf("already removed error=%v calls=%d", err, calls)
	}
}
func TestEngineCallsAreIndependentlyBounded(t *testing.T) {
	e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("unbounded Engine call")
		}
		return response(200, "[]"), nil
	}, false)
	if _, err := e.Find(context.Background(), map[string]string{"multica.fleet.namespace": "ns"}); err != nil {
		t.Fatal(err)
	}
}
