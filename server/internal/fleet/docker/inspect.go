package docker

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
)

type reportStats struct {
	Known   bool `json:"known"`
	Pending int  `json:"pending"`
	Failed  int  `json:"failed"`
}
type healthWire struct {
	DaemonID     string          `json:"daemon_id"`
	Ready        bool            `json:"ready"`
	RuntimeCount int             `json:"runtime_count"`
	ActiveRuns   int             `json:"active_runs"`
	Agents       []string        `json:"agents"`
	Reports      json.RawMessage `json:"report_queue_stats"`
}

func decodeReports(raw []byte) (reportStats, error) {
	var s reportStats
	f, e := model.DecodeStrictObject(raw, &s)
	if e != nil || len(f) != 3 || s.Pending < 0 || s.Failed < 0 {
		return s, model.ErrUnknownHealth
	}
	return s, nil
}

// Image defaults are inspected against one fixed whitelist, never administrator or caller env.
func inspectEnvironment(env []string, maxRuns int) bool {
	seen := map[string]bool{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		switch key {
		case "PATH":
			if value != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
				return false
			}
		case "HOME":
			if value != model.NodeHome {
				return false
			}
		case "FLEET_NODE_MAX_RUNS":
			if maxRuns <= 0 || value != strconv.Itoa(maxRuns) {
				return false
			}
		default:
			return false
		}
	}
	return maxRuns == 0 || (seen["HOME"] && seen["FLEET_NODE_MAX_RUNS"])
}
func validateNodeInspection(r container.InspectResponse, n model.Node, networkName string) error {
	if r.ContainerJSONBase == nil || r.Config == nil || r.HostConfig == nil || r.NetworkSettings == nil {
		return model.ErrForbidden
	}
	c, h := r.Config, r.HostConfig
	want := NodeHostConfig(n.Resources, true)
	if c.Image != n.Image || c.User != "10001:10001" || c.Tty || c.OpenStdin || len(c.ExposedPorts) != 0 || !reflect.DeepEqual([]string(c.Entrypoint), []string{"/usr/local/bin/fleet-node"}) || !reflect.DeepEqual([]string(c.Cmd), []string{"run"}) {
		return model.ErrForbidden
	}
	if !inspectEnvironment(c.Env, n.Resources.MaxRuns) {
		return model.ErrForbidden
	}
	if h.NanoCPUs != want.NanoCPUs || h.Memory != want.Memory || h.PidsLimit == nil || *h.PidsLimit != *want.PidsLimit || h.Privileged || h.PidMode != "" || len(h.Binds) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.VolumesFrom) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts || h.NetworkMode != container.NetworkMode(networkName) || h.RestartPolicy.Name != container.RestartPolicyDisabled || len(h.CapAdd) != 0 || !reflect.DeepEqual(h.CapDrop, want.CapDrop) || !reflect.DeepEqual(h.SecurityOpt, want.SecurityOpt) || !reflect.DeepEqual(h.ExtraHosts, want.ExtraHosts) {
		return model.ErrForbidden
	}
	if len(r.NetworkSettings.Networks) != 1 || r.NetworkSettings.Networks[networkName] == nil || len(r.Mounts) != 2 {
		return model.ErrForbidden
	}
	data, secrets := false, false
	for _, m := range r.Mounts {
		if m.Type != mount.TypeVolume {
			return model.ErrForbidden
		}
		switch m.Destination {
		case model.DataMount:
			if m.Name != n.DataVolume || !m.RW {
				return model.ErrForbidden
			}
			data = true
		case "/secrets":
			if m.Name != n.SecretsVolume || m.RW {
				return model.ErrForbidden
			}
			secrets = true
		default:
			return model.ErrForbidden
		}
	}
	if !data || !secrets {
		return model.ErrForbidden
	}
	return nil
}
func (p *Provider) validSDKSnapshot(n model.Node, i Inspection) error {
	if _, ok := p.engine.(*sdkEngine); ok {
		if i.sdk == nil {
			return model.ErrForbidden
		}
		return validateNodeInspection(*i.sdk, n, p.networkName())
	}
	return nil
}
func parseHealth(raw []byte, n model.Node, epoch string) (model.Observation, error) {
	if id, e := util.ParseUUID(n.DaemonID); e != nil || id.Bytes == [16]byte{} {
		return model.Observation{}, model.ErrUnknownHealth
	}
	var h healthWire
	f, e := model.DecodeStrictObject(raw, &h)
	if e != nil || len(f) != 6 || n.DaemonID == "" || epoch == "" || h.DaemonID != n.DaemonID || h.RuntimeCount < 0 || h.ActiveRuns < 0 {
		return model.Observation{}, model.ErrUnknownHealth
	}
	// Persisted epochs must agree during diagnosis; a newly started node may not yet have one.
	if n.StartEpoch != "" && n.StartEpoch != epoch {
		return model.Observation{}, model.ErrUnknownHealth
	}
	s, e := decodeReports(h.Reports)
	if e != nil {
		return model.Observation{}, e
	}
	return model.Observation{ContainerID: n.ContainerID, Status: "running", DaemonID: h.DaemonID, StartEpoch: epoch, Ready: h.Ready, Agents: h.Agents, RuntimeCount: h.RuntimeCount, ActiveRuns: h.ActiveRuns, ReportStatsKnown: s.Known, PendingReports: s.Pending, FailedReports: s.Failed}, nil
}
func (p *Provider) inspectOwned(ctx context.Context, n model.Node, id string) (Inspection, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	i, e := p.engine.Inspect(ctx, id)
	if e != nil {
		return i, e
	}
	if i.ID == "" || (n.ContainerID != "" && i.ID != n.ContainerID) || !Owns(i.Labels, n.Namespace, p.cfg.FleetID, nodeID(n), "node") {
		return Inspection{}, model.ErrForbidden
	}
	return i, nil
}
func (p *Provider) Inspect(ctx context.Context, n model.Node) (model.Observation, error) {
	if e := p.validNode(n); e != nil {
		return model.Observation{}, e
	}
	if n.ContainerID == "" {
		return model.Observation{Status: "missing", ObservedAt: time.Now()}, nil
	}
	i, e := p.inspectOwned(ctx, n, n.ContainerID)
	if errdefs.IsNotFound(e) {
		return model.Observation{Status: "missing", ObservedAt: time.Now()}, nil
	}
	if e != nil {
		return model.Observation{}, safeError(e)
	}
	return p.observation(ctx, n, i), nil
}
func (p *Provider) observation(ctx context.Context, n model.Node, i Inspection) model.Observation {
	o := model.Observation{ContainerID: i.ID, Status: "unknown", ObservedAt: time.Now()}
	if i.State == "exited" || i.State == "created" {
		o.Status = "stopped"
	}
	if i.State == "running" {
		o.Status = "running"
		n.ContainerID = i.ID
		if p.validSDKSnapshot(n, i) != nil {
			return o
		}
		if i.StartedAt == "" || (n.StartEpoch != "" && n.StartEpoch != i.StartedAt) {
			return o
		}
		healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		raw, e := p.engine.FixedHealth(healthCtx, i.ID)
		if e != nil {
			return o
		}
		after, e := p.inspectOwned(healthCtx, n, i.ID)
		if e != nil || after.ID != i.ID || after.State != "running" || after.StartedAt != i.StartedAt {
			return o
		}
		if h, e := parseHealth(raw, n, i.StartedAt); e == nil {
			o = h
			o.ObservedAt = time.Now()
		}
	}
	return o
}
