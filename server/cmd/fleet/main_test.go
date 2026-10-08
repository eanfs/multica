package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeServer struct {
	start     chan struct{}
	stop      chan struct{}
	closed    atomic.Bool
	listenErr error
}

func (s *fakeServer) ListenAndServe() error {
	close(s.start)
	if s.listenErr != nil {
		return s.listenErr
	}
	<-s.stop
	return http.ErrServerClosed
}
func (s *fakeServer) Shutdown(context.Context) error { s.closed.Store(true); close(s.stop); return nil }
func (s *fakeServer) Close() error                   { return nil }

type fakeRunner struct {
	start chan struct{}
	done  chan struct{}
	err   error
}

func (w *fakeRunner) Run(ctx context.Context) error {
	close(w.start)
	if w.err != nil {
		close(w.done)
		return w.err
	}
	<-ctx.Done()
	close(w.done)
	return nil
}
func TestFleetMainCancellationStopsWorkerAndHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeServer{start: make(chan struct{}), stop: make(chan struct{})}
	w := &fakeRunner{start: make(chan struct{}), done: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- serveFleet(ctx, s, w) }()
	select {
	case <-s.start:
	case <-time.After(time.Second):
		t.Fatal("HTTP never started")
	}
	select {
	case <-w.start:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}
	cancel()
	select {
	case e := <-result:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("unbounded shutdown")
	}
	if !s.closed.Load() {
		t.Fatal("HTTP left active")
	}
	select {
	case <-w.done:
	default:
		t.Fatal("worker left active")
	}
}
func TestFleetMainListenFailureCancelsWorker(t *testing.T) {
	s := &fakeServer{start: make(chan struct{}), stop: make(chan struct{}), listenErr: errors.New("owned bind failure")}
	w := &fakeRunner{start: make(chan struct{}), done: make(chan struct{})}
	if e := serveFleet(context.Background(), s, w); e == nil {
		t.Fatal("listen failure hidden")
	}
	select {
	case <-w.done:
	default:
		t.Fatal("worker leaked")
	}
}
func TestFleetMainPrivateServiceKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service-key")
	want := "owned-test-key-012345678901234567890123456789"
	if e := os.WriteFile(path, []byte(want+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	got, e := privateServiceKey(path)
	if e != nil || string(got) != want {
		t.Fatal("explicit private key rejected")
	}
	for _, mode := range []os.FileMode{0644, 0000} {
		if e := os.Chmod(path, mode); e != nil {
			t.Fatal(e)
		}
		if _, e := privateServiceKey(path); e == nil {
			t.Fatal("unsafe mode accepted")
		}
	}
	if _, e := privateServiceKey("relative"); e == nil {
		t.Fatal("relative default lookup accepted")
	}
	if e := os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "symlink")
	if e := os.Symlink(path, link); e != nil {
		t.Fatal(e)
	}
	if _, e := privateServiceKey(link); e == nil {
		t.Fatal("symlink accepted")
	}
}

type ownedTransport func(*http.Request) (*http.Response, error)

func (f ownedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFleetMainReadyProbeUsesOnlyReadyz(t *testing.T) {
	for _, status := range []int{200, 503} {
		calls := 0
		c := &http.Client{Transport: ownedTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path != "/readyz" || r.Method != "GET" {
				t.Fatal("not readiness-only")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("owned")), Header: http.Header{}}, nil
		})}
		e := probeReady(context.Background(), "http://owned.invalid", c)
		if (e == nil) != (status == 200) || calls != 1 {
			t.Fatal("readiness failure hidden")
		}
	}
	if base, e := selfURL("0.0.0.0:8090"); e != nil || base != "http://127.0.0.1:8090" {
		t.Fatal("wildcard self-route wrong")
	}
}
func TestFleetMainHTTPCompositionFailClosed(t *testing.T) {
	h := fleet.NewService(nil, model.Config{}, nil).Handler([]byte("owned-key"))
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/healthz", 200}, {"GET", "/readyz", 503}, {"POST", "/api/v1/nodes", 401}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s status=%d", tc.path, w.Code)
		}
	}
}
func TestFleetMainReviewClientOriginalRefNoNetwork(t *testing.T) {
	id := func(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }
	ref := model.OperationRef{Namespace: "owned", NodeID: id(1), OperationID: id(3), Generation: 7, Action: model.Delete}
	owner := util.UUIDToString(id(2))
	calls := 0
	c := cloudruntime.NewClient(cloudruntime.Config{BaseURL: "http://owned.invalid", ServiceSecret: []byte("owned-key"), HTTPClient: &http.Client{Transport: ownedTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var got fleet.OperationRequestDTO
		if e := json.NewDecoder(r.Body).Decode(&got); e != nil {
			t.Fatal(e)
		}
		parsed, e := got.OperationRef()
		if e != nil || parsed != ref || r.URL.Path != "/internal/local-fleet/operations/review" || r.Header.Get("X-User-ID") != owner || r.Header.Get("X-Fleet-Service-Key") != "owned-key" {
			t.Fatal("original private review binding changed")
		}
		return &http.Response{StatusCode: 409, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error_code":"unknown_health"}`))}, nil
	})}})
	_, e := c.ReviewOperation(context.Background(), owner, ref)
	if !errors.Is(e, model.ErrUnknownHealth) || calls != 1 {
		t.Fatalf("unknown review=%v calls=%d", e, calls)
	}
}

// A self-origin review request is a regression even though the private client itself is correct.
func TestFleetMainReviewCompositionUsesAPIOrigin(t *testing.T) {
	id := func(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }
	ref := model.OperationRef{Namespace: "owned", NodeID: id(1), OperationID: id(3), Generation: 7, Action: model.Delete}
	owner := util.UUIDToString(id(2))
	cfg := model.Config{APIURL: "http://api.owned.invalid:8000"}
	reviews, probes := 0, 0
	transport := &http.Client{Transport: ownedTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/readyz" {
			probes++
			if req.URL.Host != "127.0.0.1:8090" {
				t.Error("readyz lost Fleet origin")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ready"))}, nil
		}
		reviews++
		var dto fleet.OperationRequestDTO
		if e := json.NewDecoder(req.Body).Decode(&dto); e != nil {
			t.Fatal(e)
		}
		got, e := dto.OperationRef()
		if req.URL.Host != "api.owned.invalid:8000" || req.URL.Path != "/internal/local-fleet/operations/review" || req.Method != "POST" || e != nil || got != ref || req.Header.Get("X-User-ID") != owner || req.Header.Get("X-Fleet-Service-Key") != "owned-key" {
			t.Errorf("composition review misbound: url=%s ref=%+v", req.URL, got)
		}
		return &http.Response{StatusCode: 409, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error_code":"unknown_health"}`))}, nil
	})}
	client := reviewClient(cfg, []byte("owned-key"), transport)
	_, e := client.ReviewOperation(context.Background(), owner, ref)
	if !errors.Is(e, model.ErrUnknownHealth) {
		t.Fatal(e)
	}
	base, e := selfURL("127.0.0.1:8090")
	if e != nil {
		t.Fatal(e)
	}
	if e = probeReady(context.Background(), base, transport); e != nil {
		t.Fatal(e)
	}
	if reviews != 1 || probes != 1 {
		t.Fatal("composition not exercised")
	}
}

func TestFleetMainMissingConfigStopsBeforeAnyDefaultLookup(t *testing.T) {
	t.Setenv("FLEET_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("FLEET_SERVICE_KEY_FILE", filepath.Join(t.TempDir(), "missing-key"))
	t.Setenv("DATABASE_URL", "")
	if e := run(context.Background()); e == nil {
		t.Fatal("missing explicit inputs accepted")
	}
}
