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

type fakeCalls struct {
	Engine
	inspect        func(context.Context, string) (Inspection, error)
	find           func(context.Context, map[string]string) ([]Resource, error)
	create         func(context.Context, *container.Config, *container.HostConfig, string, string) (string, error)
	ensureVolume   func(context.Context, Resource) error
	ensureNetwork  func(context.Context, Resource) error
	connectNetwork func(context.Context, string, string, []string) error
	removeNetwork  func(context.Context, Resource) error
	bootstrap      func(context.Context, []Resource, []byte) error
	start          func(context.Context, string) error
	stop           func(context.Context, string) error
	health         func(context.Context, string) ([]byte, error)
}

func (f fakeCalls) Inspect(c context.Context, id string) (Inspection, error) { return f.inspect(c, id) }
func (f fakeCalls) Find(c context.Context, l map[string]string) ([]Resource, error) {
	return f.find(c, l)
}
func (f fakeCalls) Create(c context.Context, a *container.Config, h *container.HostConfig, net, name string) (string, error) {
	return f.create(c, a, h, net, name)
}
func (f fakeCalls) EnsureVolume(c context.Context, r Resource) error  { return f.ensureVolume(c, r) }
func (f fakeCalls) EnsureNetwork(c context.Context, r Resource) error { return f.ensureNetwork(c, r) }
func (f fakeCalls) ConnectNetwork(c context.Context, net, id string, aliases []string) error {
	return f.connectNetwork(c, net, id, aliases)
}
func (f fakeCalls) RemoveNetwork(c context.Context, r Resource) error { return f.removeNetwork(c, r) }
func (f fakeCalls) InstallBootstrap(c context.Context, r []Resource, b []byte) error {
	return f.bootstrap(c, r, b)
}
func (f fakeCalls) Start(c context.Context, id string) error                 { return f.start(c, id) }
func (f fakeCalls) Stop(c context.Context, id string) error                  { return f.stop(c, id) }
func (f fakeCalls) FixedHealth(c context.Context, id string) ([]byte, error) { return f.health(c, id) }

func TestInspectRestartDuringFixedHealthIsUnknown(t *testing.T) {
	for _, kind := range []string{"restart", "missing-epoch", "old-sql-epoch", "wrong-daemon", "malformed", "cached-log"} {
		t.Run(kind, func(t *testing.T) {
			n := fixtureNode()
			n.StartEpoch = "epoch"
			calls := 0
			healthCalls := 0
			e := fakeCalls{inspect: func(context.Context, string) (Inspection, error) {
				calls++
				epoch := "epoch"
				if kind == "restart" && calls > 1 {
					epoch = "new-epoch"
				}
				if kind == "missing-epoch" {
					epoch = ""
				}
				return Inspection{ID: "cid", State: "running", StartedAt: epoch, Labels: fixtureLabels("node"), HealthJSON: []byte(goodHealth)}, nil
			}, health: func(context.Context, string) ([]byte, error) {
				healthCalls++
				raw := goodHealth
				if kind == "wrong-daemon" {
					raw = strings.Replace(raw, fixtureNode().DaemonID+"\"", "wrong\"", 1)
				}
				if kind == "malformed" {
					raw = "{}"
				}
				if kind == "cached-log" {
					return nil, model.ErrUnknownHealth
				}
				return []byte(raw), nil
			}}
			if kind == "old-sql-epoch" {
				n.StartEpoch = "old"
			}
			p := New(e, fixtureConfig())
			o, err := p.Inspect(context.Background(), n)
			if err != nil {
				t.Fatal(err)
			}
			if o.Ready || o.ReportStatsKnown || o.StartEpoch != "" {
				t.Fatalf("untrusted health published: %+v", o)
			}
			if kind == "restart" && calls < 2 {
				t.Fatal("no post-exec inspection")
			}
			_ = healthCalls
		})
	}
}
func TestProviderApplyKeepsIdentityAndVolumes(t *testing.T) {
	for _, action := range []model.Action{model.Start, model.Stop, model.Reboot} {
		t.Run(string(action), func(t *testing.T) {
			n := fixtureNode()
			n.Desired = "running"
			state := "exited"
			stops, starts := 0, 0
			if action == model.Start {
				n.Maintenance = false
			}
			if action == model.Stop {
				n.Desired = "stopped"
				state = "running"
			}
			e := fakeCalls{inspect: func(context.Context, string) (Inspection, error) {
				return Inspection{ID: "cid", State: state, StartedAt: "epoch", Labels: fixtureLabels("node")}, nil
			}, start: func(_ context.Context, id string) error {
				if id != "cid" {
					t.Fatal("changed ID")
				}
				starts++
				state = "running"
				return nil
			}, stop: func(_ context.Context, id string) error {
				if id != "cid" {
					t.Fatal("changed ID")
				}
				stops++
				state = "exited"
				return nil
			}, health: func(context.Context, string) ([]byte, error) { return []byte(goodHealth), nil }}
			p := New(e, fixtureConfig())
			o, err := p.Apply(context.Background(), n, action)
			if err != nil || o.ContainerID != "cid" {
				t.Fatalf("apply e=%v o=%+v", err, o)
			}
			if action == model.Stop {
				if stops != 1 || starts != 0 || o.Status != "stopped" {
					t.Fatal("stop changed lifecycle")
				}
			} else if starts != 1 || o.Status != "running" {
				t.Fatal("start/reboot failed")
			}
			if n.DataVolume != "data-vol" || n.SecretsVolume != "secrets-vol" {
				t.Fatal("snapshot changed")
			}
		})
	}
}
func TestProviderEnsureRevokedOrMaintenanceNeverMutates(t *testing.T) {
	for _, kind := range []string{"revoked", "maintenance", "terminating"} {
		t.Run(kind, func(t *testing.T) {
			n := fixtureNode()
			n.Maintenance = false
			n.Desired = "running"
			switch kind {
			case "revoked":
				n.Revoked = true
			case "maintenance":
				n.Maintenance = true
			case "terminating":
				n.Desired = "terminating"
			}
			calls := 0
			p := New(fakeCalls{inspect: func(context.Context, string) (Inspection, error) {
				return Inspection{ID: "cid", State: "exited", Labels: fixtureLabels("node")}, nil
			}, start: func(context.Context, string) error { calls++; return nil }}, fixtureConfig())
			if _, e := p.Ensure(context.Background(), n, model.Bootstrap{}); e == nil || calls != 0 {
				t.Fatal("controlled node resurrected")
			}
		})
	}
}
func TestProviderAvailabilityUsesOnlyReadonlyInventory(t *testing.T) {
	calls := 0
	p := New(fakeCalls{find: func(c context.Context, l map[string]string) ([]Resource, error) {
		calls++
		if l["multica.fleet.namespace"] != "ns" || l["multica.fleet.fleet_id"] != "fleet" {
			t.Fatal("wrong inventory boundary")
		}
		return nil, nil
	}}, fixtureConfig())
	if e := p.CheckAvailability(context.Background()); e != nil || calls != 1 {
		t.Fatalf("availability error=%v calls=%d", e, calls)
	}
}
func TestOwnershipProviderRejectsForeignOrIncompleteContainer(t *testing.T) {
	for _, kind := range []string{"foreign", "missing", "wrong-id"} {
		t.Run(kind, func(t *testing.T) {
			l := fixtureLabels("node")
			id := "cid"
			if kind == "foreign" {
				l["multica.fleet.namespace"] = "other"
			}
			if kind == "missing" {
				delete(l, "multica.fleet.fleet_id")
			}
			if kind == "wrong-id" {
				id = "different"
			}
			p := New(fakeCalls{inspect: func(context.Context, string) (Inspection, error) {
				return Inspection{ID: id, State: "running", StartedAt: "epoch", Labels: l}, nil
			}}, fixtureConfig())
			if _, e := p.Inspect(context.Background(), fixtureNode()); !errors.Is(e, model.ErrForbidden) {
				t.Fatalf("error=%v", e)
			}
		})
	}
}
func TestProviderEnsureTimeoutAfterCreateAdoptsWithoutDuplicate(t *testing.T) {
	n := fixtureNode()
	n.Maintenance = false
	n.ContainerID = ""
	created := false
	creates, starts := 0, 0
	inspect := func(context.Context, string) (Inspection, error) {
		if !created {
			return Inspection{}, errdefs.ErrNotFound
		}
		return Inspection{ID: "new-cid", State: "created", Labels: fixtureLabels("node")}, nil
	}
	e := fakeCalls{inspect: inspect, ensureNetwork: func(context.Context, Resource) error { return nil }, ensureVolume: func(_ context.Context, r Resource) error {
		if r.ID != r.Name || r.ID == "" {
			t.Fatal("persisted volume identity lost")
		}
		return nil
	}, bootstrap: func(context.Context, []Resource, []byte) error { return nil }, create: func(_ context.Context, c *container.Config, h *container.HostConfig, net, name string) (string, error) {
		creates++
		created = true
		if c.Image != "node-snapshot" || c.User != "10001:10001" || len(c.Env) != 2 || h.Memory != 4<<30 || len(h.Mounts) != 2 || !h.Mounts[1].ReadOnly {
			t.Fatal("snapshot/security lost")
		}
		return "", context.DeadlineExceeded
	}, start: func(context.Context, string) error { starts++; return nil }, health: func(context.Context, string) ([]byte, error) { return nil, model.ErrUnknownHealth }}
	p := New(e, fixtureConfig())
	o, err := p.Ensure(context.Background(), n, model.Bootstrap{NodeToken: "fake-token", APIKey: "fake-key", ServerURL: fixtureConfig().APIURL, DaemonID: n.DaemonID})
	if err != nil || o.ContainerID != "new-cid" || creates != 1 || starts != 1 {
		t.Fatalf("ensure o=%+v err=%v creates=%d starts=%d", o, err, creates, starts)
	}
	if _, err = p.Ensure(context.Background(), n, model.Bootstrap{}); err != nil || creates != 1 {
		t.Fatalf("retry duplicated create err=%v creates=%d", err, creates)
	}
}

func TestNodeHostConfigIsRestricted(t *testing.T) {
	h := NodeHostConfig(model.Spec{CPUs: 2, MemoryBytes: 4 << 30, Pids: 256, MaxRuns: 1}, true, nil)
	if h.Privileged || h.NetworkMode == "host" || h.PidMode == "host" || len(h.PortBindings) != 0 || h.PublishAllPorts {
		t.Fatal("unsafe isolation")
	}
	if h.Memory != 4<<30 || h.NanoCPUs != 2000000000 || h.PidsLimit == nil || *h.PidsLimit != 256 {
		t.Fatal("resource snapshot not enforced")
	}
	if !reflect.DeepEqual([]string(h.CapDrop), []string{"ALL"}) || !reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges:true"}) || h.RestartPolicy.Name != "no" {
		t.Fatal("missing restrictions")
	}
	if !reflect.DeepEqual(h.ExtraHosts, []string{"host.docker.internal:host-gateway"}) {
		t.Fatal("Linux gateway missing")
	}
	if len(NodeHostConfig(model.Spec{}, false, nil).ExtraHosts) != 0 {
		t.Fatal("unrequested gateway")
	}
}

func TestOwnershipRequiresEveryNonemptyLabel(t *testing.T) {
	labels := map[string]string{"multica.fleet.fleet_id": "fleet", "multica.fleet.namespace": "ns", "multica.fleet.node": "node", "multica.fleet.role": "data"}
	if !Owns(labels, "ns", "fleet", "node", "data") {
		t.Fatal("owned resource rejected")
	}
	for k := range labels {
		copy := map[string]string{}
		for a, b := range labels {
			copy[a] = b
		}
		delete(copy, k)
		if Owns(copy, "ns", "fleet", "node", "data") {
			t.Fatalf("missing %s accepted", k)
		}
	}
	if Owns(nil, "", "", "", "") || Owns(labels, "other", "fleet", "node", "data") {
		t.Fatal("foreign or blank ownership accepted")
	}
}
