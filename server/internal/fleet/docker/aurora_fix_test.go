package docker

// Regression coverage for the Task 5 fix round: the egress sidecar must be
// started before the node, repeated network attaches must be idempotent, and
// Aurora teardown must order and ownership-check every mutation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// auroraDeleteEngine scripts the Aurora teardown path: the node is already
// stopped, the sidecar is discoverable through Find, and every mutation is
// recorded in order. Embedding Engine satisfies the unused methods.
type auroraDeleteEngine struct {
	Engine
	node         model.Node
	fleetID      string
	sidecarID    string
	sidecarLabel map[string]string
	order        []string
}

func (d *auroraDeleteEngine) Inspect(_ context.Context, id string) (Inspection, error) {
	return Inspection{ID: id, State: "exited", Labels: labels(d.node.Namespace, d.fleetID, nodeID(d.node), "node")}, nil
}
func (d *auroraDeleteEngine) Find(context.Context, map[string]string) ([]Resource, error) {
	if d.sidecarID == "" {
		return nil, nil
	}
	return []Resource{{ID: d.sidecarID, Role: egressProxyRole, Labels: d.sidecarLabel}}, nil
}
func (d *auroraDeleteEngine) Remove(_ context.Context, id string) error {
	d.order = append(d.order, "remove:"+id)
	return nil
}
func (d *auroraDeleteEngine) RemoveVolume(_ context.Context, r Resource) error {
	d.order = append(d.order, "volume:"+r.Role)
	return nil
}
func (d *auroraDeleteEngine) RemoveNetwork(_ context.Context, r Resource) error {
	d.order = append(d.order, "network:"+r.Name)
	return nil
}
func (d *auroraDeleteEngine) FixedOfflineReports(context.Context, model.Node, model.OperationRef) ([]byte, error) {
	return []byte(goodOffline), nil
}

// TestProviderEnsureAuroraEgressOrdering asserts the deterministic admission
// order: sidecar create, sidecar start, connect with the fixed alias, then the
// node create and start. The node proxy target is dead unless the sidecar is
// started first.
func TestProviderEnsureAuroraEgressOrdering(t *testing.T) {
	cfg := auroraConfig()
	n := auroraNode()
	base := New(fakeCalls{}, cfg)
	egressName := base.egressName(n)
	nodeName := base.containerName(n)
	workspace := base.workspaceNetwork(n).Name

	var order []string
	inspect := func(_ context.Context, id string) (Inspection, error) {
		switch id {
		case "egress-id":
			return Inspection{ID: "egress-id", State: "created", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), egressProxyRole)}, nil
		case "node-id":
			return Inspection{ID: "node-id", State: "created", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), "node")}, nil
		default:
			return Inspection{}, errdefs.ErrNotFound
		}
	}
	e := fakeCalls{
		inspect:       inspect,
		ensureNetwork: func(context.Context, Resource) error { return nil },
		ensureVolume:  func(context.Context, Resource) error { return nil },
		bootstrap:     func(context.Context, []Resource, []byte) error { return nil },
		create: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ string, name string) (string, error) {
			if name == egressName {
				order = append(order, "create:egress")
				return "egress-id", nil
			}
			if name != nodeName {
				t.Fatalf("unexpected create %q", name)
			}
			order = append(order, "create:node")
			return "node-id", nil
		},
		start: func(_ context.Context, id string) error {
			order = append(order, "start:"+id)
			return nil
		},
		connectNetwork: func(_ context.Context, network, id string, aliases []string) error {
			order = append(order, "connect:"+network+":"+id+":"+strings.Join(aliases, ","))
			return nil
		},
	}
	p := New(e, cfg)
	if _, err := p.Ensure(context.Background(), n, auroraBootstrap(n)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	want := []string{
		"create:egress",
		"start:egress-id",
		"connect:" + workspace + ":egress-id:" + model.AuroraEgressAlias,
		"create:node",
		"start:node-id",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("egress ordering = %v, want %v", order, want)
	}
}

// TestProviderEnsureEgressDoesNotStartRunningSidecar proves a re-adopted,
// already-running sidecar is not restarted while the node is still started.
func TestProviderEnsureEgressDoesNotStartRunningSidecar(t *testing.T) {
	cfg := auroraConfig()
	n := auroraNode()
	base := New(fakeCalls{}, cfg)
	egressName := base.egressName(n)

	var started []string
	e := fakeCalls{
		inspect: func(_ context.Context, id string) (Inspection, error) {
			switch id {
			case "egress-id":
				return Inspection{ID: "egress-id", State: "running", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), egressProxyRole)}, nil
			case "node-id":
				return Inspection{ID: "node-id", State: "created", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), "node")}, nil
			default:
				return Inspection{}, errdefs.ErrNotFound
			}
		},
		ensureNetwork:  func(context.Context, Resource) error { return nil },
		ensureVolume:   func(context.Context, Resource) error { return nil },
		bootstrap:      func(context.Context, []Resource, []byte) error { return nil },
		connectNetwork: func(context.Context, string, string, []string) error { return nil },
		create: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ string, name string) (string, error) {
			if name == egressName {
				return "egress-id", nil
			}
			return "node-id", nil
		},
		start: func(_ context.Context, id string) error {
			started = append(started, id)
			return nil
		},
	}
	if _, err := New(e, cfg).Ensure(context.Background(), n, auroraBootstrap(n)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reflect.DeepEqual(started, []string{"node-id"}) {
		t.Fatalf("started = %v, want only the node", started)
	}
}

// TestProviderEnsureEgressConnectIdempotency proves a duplicate attach reported
// by the engine is success while any other attach failure fails closed.
func TestProviderEnsureEgressConnectIdempotency(t *testing.T) {
	for _, tc := range []struct {
		name    string
		connect error
		wantErr error
	}{
		{name: "duplicate attach is success", connect: errors.New("Error response from daemon: endpoint with name egress already exists in network multica-fleet-ws-1234")},
		{name: "other failure fails closed", connect: errors.New("Error response from daemon: network multica-fleet-ws-1234 not found"), wantErr: model.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := auroraConfig()
			n := auroraNode()
			base := New(fakeCalls{}, cfg)
			egressName := base.egressName(n)
			e := fakeCalls{
				inspect: func(_ context.Context, id string) (Inspection, error) {
					switch id {
					case "egress-id":
						return Inspection{ID: "egress-id", State: "running", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), egressProxyRole)}, nil
					case "node-id":
						return Inspection{ID: "node-id", State: "created", Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), "node")}, nil
					default:
						return Inspection{}, errdefs.ErrNotFound
					}
				},
				ensureNetwork:  func(context.Context, Resource) error { return nil },
				ensureVolume:   func(context.Context, Resource) error { return nil },
				bootstrap:      func(context.Context, []Resource, []byte) error { return nil },
				connectNetwork: func(context.Context, string, string, []string) error { return tc.connect },
				create: func(_ context.Context, _ *container.Config, _ *container.HostConfig, _ string, name string) (string, error) {
					if name == egressName {
						return "egress-id", nil
					}
					return "node-id", nil
				},
				start: func(context.Context, string) error { return nil },
			}
			_, err := New(e, cfg).Ensure(context.Background(), n, auroraBootstrap(n))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("connect failure = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("duplicate attach rejected: %v", err)
			}
		})
	}
}

// TestAuroraDeleteOrdersWorkspaceNetworkLast proves the teardown removes the
// node, then the sidecar, then the volumes, and finally the per-node workspace
// network, and never touches the shared uplink network.
func TestAuroraDeleteOrdersWorkspaceNetworkLast(t *testing.T) {
	cfg := auroraConfig()
	n := deletingRunningNode()
	e := &auroraDeleteEngine{node: n, fleetID: cfg.FleetID, sidecarID: "egress-id", sidecarLabel: labels(n.Namespace, cfg.FleetID, nodeID(n), egressProxyRole)}
	p := New(e, cfg)
	if err := p.Delete(context.Background(), n, fixtureRef()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	workspace := p.workspaceNetwork(n).Name
	want := []string{"remove:cid", "remove:egress-id", "volume:secrets", "volume:data", "network:" + workspace}
	if !reflect.DeepEqual(e.order, want) {
		t.Fatalf("aurora delete order = %v, want %v", e.order, want)
	}
	if strings.Contains(strings.Join(e.order, ","), cfg.Aurora.UplinkNetwork) {
		t.Fatalf("shared uplink network removed: %v", e.order)
	}
}

// TestAuroraDeleteRefusesForeignEgressSidecar proves a sidecar missing this
// node's ownership labels is never removed.
func TestAuroraDeleteRefusesForeignEgressSidecar(t *testing.T) {
	cfg := auroraConfig()
	n := auroraNode()
	e := &auroraDeleteEngine{node: n, fleetID: cfg.FleetID, sidecarID: "egress-id", sidecarLabel: labels("foreign", "foreign", nodeID(n), egressProxyRole)}
	if err := New(e, cfg).removeEgress(context.Background(), n); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("foreign sidecar error = %v", err)
	}
	if len(e.order) != 0 {
		t.Fatalf("foreign sidecar mutated: %v", e.order)
	}
}

// TestEngineConnectNetworkIsIdempotent proves the Engine neither reconnects an
// already-attached container nor turns Docker's duplicate-endpoint response into
// a failure, and still fails closed on any other error.
func TestEngineConnectNetworkIsIdempotent(t *testing.T) {
	for _, kind := range []string{"attached", "duplicate", "failure"} {
		t.Run(kind, func(t *testing.T) {
			connects := 0
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.51")
				switch {
				case r.Method == "GET" && p == "/networks/ws-net":
					containers := map[string]any{}
					if kind == "attached" {
						containers["egress-id"] = map[string]any{"Name": "egress", "EndpointID": "ep-id"}
					}
					raw, _ := json.Marshal(map[string]any{"Id": "ws-id", "Name": "ws-net", "Driver": "bridge", "Internal": true, "Labels": fixtureLabels("workspace-network"), "Containers": containers})
					return response(200, string(raw)), nil
				case r.Method == "POST" && p == "/networks/ws-net/connect":
					connects++
					if kind == "duplicate" {
						return response(500, `{"message":"endpoint with name egress already exists in network ws-net"}`), nil
					}
					if kind == "failure" {
						return response(500, `{"message":"some other attach failure"}`), nil
					}
					return response(200, ""), nil
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					return nil, fmt.Errorf("unexpected request")
				}
			}, false)
			err := e.ConnectNetwork(context.Background(), "ws-net", "egress-id", []string{model.AuroraEgressAlias})
			if kind == "failure" {
				if err == nil {
					t.Fatal("other attach failure accepted")
				}
			} else if err != nil {
				t.Fatalf("idempotent attach rejected: %v", err)
			}
			wantConnects := 1
			if kind == "attached" {
				wantConnects = 0
			}
			if connects != wantConnects {
				t.Fatalf("connects = %d, want %d", connects, wantConnects)
			}
		})
	}
}

// TestEngineWorkspaceNetworkOwnershipRefusal proves a foreign-labelled workspace
// network is neither adopted nor removed, while an owned one is removed.
func TestEngineWorkspaceNetworkOwnershipRefusal(t *testing.T) {
	for _, kind := range []string{"ensure-foreign", "remove-foreign", "remove-owned"} {
		t.Run(kind, func(t *testing.T) {
			creates, deletes := 0, 0
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.51")
				switch {
				case r.Method == "GET" && p == "/networks/ws-net":
					l := fixtureLabels("workspace-network")
					if kind != "remove-owned" {
						l["multica.fleet.fleet_id"] = "foreign"
					}
					raw, _ := json.Marshal(map[string]any{"Id": "ws-id", "Name": "ws-net", "Driver": "bridge", "Internal": true, "Labels": l})
					return response(200, string(raw)), nil
				case r.Method == "POST" && p == "/networks/create":
					creates++
					return response(201, `{"Id":"ws-id"}`), nil
				case r.Method == "DELETE" && p == "/networks/ws-net":
					deletes++
					return response(204, ""), nil
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					return nil, fmt.Errorf("unexpected request")
				}
			}, false)
			r := Resource{Name: "ws-net", Role: "workspace-network", Internal: true, Labels: fixtureLabels("workspace-network")}
			var err error
			if kind == "ensure-foreign" {
				err = e.EnsureNetwork(context.Background(), r)
			} else {
				err = e.RemoveNetwork(context.Background(), r)
			}
			if kind == "ensure-foreign" || kind == "remove-foreign" {
				if !errors.Is(err, model.ErrForbidden) {
					t.Fatalf("foreign workspace network error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if creates != 0 {
				t.Fatalf("foreign workspace network created: %d", creates)
			}
			wantDeletes := 0
			if kind == "remove-owned" {
				wantDeletes = 1
			}
			if deletes != wantDeletes {
				t.Fatalf("deletes = %d, want %d", deletes, wantDeletes)
			}
		})
	}
}
