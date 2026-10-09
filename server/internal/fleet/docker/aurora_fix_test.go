package docker

// Aurora teardown preserves resource ordering and ownership.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// auroraDeleteEngine scripts the Aurora teardown path: the node is already
// stopped and every mutation is
// recorded in order. Embedding Engine satisfies the unused methods.
type auroraDeleteEngine struct {
	Engine
	node    model.Node
	fleetID string
	order   []string
}

func (d *auroraDeleteEngine) Inspect(_ context.Context, id string) (Inspection, error) {
	return Inspection{ID: id, State: "exited", Labels: labels(d.node.Namespace, d.fleetID, nodeID(d.node), "node")}, nil
}
func (d *auroraDeleteEngine) Find(context.Context, map[string]string) ([]Resource, error) {
	return nil, nil
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

// TestAuroraDeleteOrdersWorkspaceNetworkLast proves the teardown removes the
// node, then the volumes, and finally the per-node workspace network.
func TestAuroraDeleteOrdersWorkspaceNetworkLast(t *testing.T) {
	cfg := auroraConfig()
	n := deletingRunningNode()
	e := &auroraDeleteEngine{node: n, fleetID: cfg.FleetID}
	p := New(e, cfg)
	if err := p.Delete(context.Background(), n, fixtureRef()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	workspace := p.workspaceNetwork(n).Name
	want := []string{"remove:cid", "volume:secrets", "volume:data", "network:" + workspace}
	if !reflect.DeepEqual(e.order, want) {
		t.Fatalf("aurora delete order = %v, want %v", e.order, want)
	}
}

// TestEngineWorkspaceNetworkOwnershipRefusal proves a foreign-labelled workspace
// network is neither adopted nor removed, while an owned one is removed.
func TestEngineWorkspaceNetworkOwnershipRefusal(t *testing.T) {
	for _, kind := range []string{"ensure-owned", "ensure-internal", "ensure-foreign", "remove-foreign", "remove-owned"} {
		t.Run(kind, func(t *testing.T) {
			creates, deletes := 0, 0
			e := fakeEngine(t, func(r *http.Request) (*http.Response, error) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.51")
				switch {
				case r.Method == "GET" && p == "/networks/ws-net":
					l := fixtureLabels("workspace-network")
					if kind == "ensure-foreign" || kind == "remove-foreign" {
						l["multica.fleet.fleet_id"] = "foreign"
					}
					raw, _ := json.Marshal(map[string]any{"Id": "ws-id", "Name": "ws-net", "Driver": "bridge", "Internal": kind == "ensure-internal", "Labels": l})
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
			e = New(e, fixtureConfig()).engine
			r := Resource{Name: "ws-net", Role: "workspace-network", Internal: false, Labels: fixtureLabels("workspace-network")}
			var err error
			if strings.HasPrefix(kind, "ensure-") {
				err = e.EnsureNetwork(context.Background(), r)
			} else {
				err = e.RemoveNetwork(context.Background(), r)
			}
			if kind == "ensure-foreign" || kind == "remove-foreign" || kind == "ensure-internal" {
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
