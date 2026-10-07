package docker

// Regression coverage for the Aurora-on-Fleet 503: Docker merges the image's OCI
// labels into every container, so the helper ownership checks must compare only
// the four multica.fleet.* keys (sameLabels), never the whole label map. A
// cleanup failure after a successful helper run must also leave that primary
// result successful.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

func helperSpec() (*container.Config, *container.HostConfig) {
	h := diagnosticHost()
	c := &container.Config{
		Image:           "example/fleet@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		User:            helperUser,
		Entrypoint:      []string{"/usr/local/bin/fleet-node"},
		Cmd:             []string{"bootstrap"},
		Labels:          labels("ns", "fleet", "01000000-0000-0000-0000-000000000000", "bootstrap"),
		NetworkDisabled: true,
	}
	return c, &h
}

func helperSnapshot(c *container.Config, h *container.HostConfig, l map[string]string) container.InspectResponse {
	cc := *c
	cc.Labels = l
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: "helper-id", HostConfig: h},
		Config:            &cc,
		NetworkSettings:   &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}},
	}
}

func imageLabels() map[string]string {
	return map[string]string{
		"org.opencontainers.image.title":    "multica-aurora-sandbox",
		"org.opencontainers.image.source":   "https://github.com/eanfs/multica",
		"org.opencontainers.image.licenses": "MIT",
		"org.opencontainers.image.created":  "2026-10-06T00:00:00Z",
		"org.opencontainers.image.revision": "b8bfa9491",
	}
}

func mergedLabels(base map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func trimVersion(p string) string {
	return p[len("/v1.51"):]
}

// TestCleanupHelperAcceptsImageSuppliedLabels is the regression for the 503:
// the actual container label map is the desired fleet labels plus the image's
// OCI labels, and cleanup must still recognize and remove the helper.
func TestCleanupHelperAcceptsImageSuppliedLabels(t *testing.T) {
	c, h := helperSpec()
	actual := helperSnapshot(c, h, mergedLabels(c.Labels, imageLabels()))
	deleted := 0
	e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
		p := trimVersion(r.URL.Path)
		switch {
		case r.Method == http.MethodGet && p == "/containers/helper-id/json":
			raw, _ := json.Marshal(actual)
			return response(200, string(raw)), nil
		case r.Method == http.MethodDelete && p == "/containers/helper-id":
			deleted++
			return response(204, ""), nil
		default:
			return nil, fmt.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}, false)
	if err := e.(*sdkEngine).cleanupHelper(context.Background(), "helper-id", c, h); err != nil {
		t.Fatalf("image-supplied labels rejected by cleanup ownership check: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("owned helper not removed: deletes=%d", deleted)
	}
}

// TestCleanupHelperStillRejectsForeignFleetLabels proves the tolerant comparison
// did not weaken ownership: a missing or wrong fleet label is refused and never
// removed, even when image labels are present.
func TestCleanupHelperStillRejectsForeignFleetLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing-node", mutate: func(l map[string]string) { delete(l, "multica.fleet.node") }},
		{name: "empty-role", mutate: func(l map[string]string) { l["multica.fleet.role"] = "" }},
		{name: "wrong-fleet", mutate: func(l map[string]string) { l["multica.fleet.fleet_id"] = "foreign" }},
		{name: "wrong-role", mutate: func(l map[string]string) { l["multica.fleet.role"] = "node" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, h := helperSpec()
			l := mergedLabels(c.Labels, imageLabels())
			tc.mutate(l)
			actual := helperSnapshot(c, h, l)
			deleted := 0
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				p := trimVersion(r.URL.Path)
				switch {
				case r.Method == http.MethodGet && p == "/containers/helper-id/json":
					raw, _ := json.Marshal(actual)
					return response(200, string(raw)), nil
				case r.Method == http.MethodDelete && p == "/containers/helper-id":
					deleted++
					return response(204, ""), nil
				default:
					return nil, fmt.Errorf("unexpected %s %s", r.Method, r.URL)
				}
			}, false)
			err := e.(*sdkEngine).cleanupHelper(context.Background(), "helper-id", c, h)
			if !errors.Is(err, model.ErrForbidden) {
				t.Fatalf("foreign helper error = %v, want ErrForbidden", err)
			}
			if deleted != 0 {
				t.Fatalf("foreign helper removed: deletes=%d", deleted)
			}
		})
	}
}

// TestRunHelperCleanupFailurePreservesSuccess proves a cleanup failure after the
// helper itself succeeded does not rewrite the primary result into
// ErrUnknownHealth.
func TestRunHelperCleanupFailurePreservesSuccess(t *testing.T) {
	c, h := helperSpec()
	actual := helperSnapshot(c, h, c.Labels)
	created, started, waited, removeAttempted := false, false, false, false
	e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
		p := trimVersion(r.URL.Path)
		switch {
		case r.Method == http.MethodPost && p == "/containers/create":
			created = true
			return response(201, "{\"Id\":\"helper-id\"}"), nil
		case r.Method == http.MethodGet && p == "/containers/helper-id/json":
			raw, _ := json.Marshal(actual)
			return response(200, string(raw)), nil
		case r.Method == http.MethodPost && p == "/containers/helper-id/start":
			started = true
			return response(204, ""), nil
		case r.Method == http.MethodPost && p == "/containers/helper-id/wait":
			waited = true
			return response(200, "{\"StatusCode\":0}"), nil
		case r.Method == http.MethodGet && p == "/containers/helper-id/logs":
			return response(200, framed(goodOffline)), nil
		case r.Method == http.MethodDelete && p == "/containers/helper-id":
			removeAttempted = true
			return response(500, "{\"message\":\"device or resource busy\"}"), nil
		default:
			return nil, fmt.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := e.(*sdkEngine).runHelper(ctx, c, h, nil)
	if err != nil {
		t.Fatalf("cleanup failure overwrote a successful helper run: %v", err)
	}
	if string(raw) != goodOffline {
		t.Fatalf("raw = %q, want %q", raw, goodOffline)
	}
	if !created || !started || !waited || !removeAttempted {
		t.Fatalf("primary lifecycle incomplete: created=%v started=%v waited=%v removeAttempted=%v", created, started, waited, removeAttempted)
	}
}
