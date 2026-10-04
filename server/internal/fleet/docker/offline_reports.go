package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

const diagnosticLimit = 64 << 10

type offlineWire struct {
	Manifest json.RawMessage `json:"manifest"`
	Reports  json.RawMessage `json:"report_queue_stats"`
}

func parseOffline(raw []byte, n model.Node, cfg model.Config) (model.Observation, error) {
	var wire offlineWire
	f, e := model.DecodeStrictObject(raw, &wire)
	if e != nil || len(f) != 2 || model.ValidateLayoutManifest(wire.Manifest, model.LayoutIdentity{Namespace: n.Namespace, FleetID: cfg.FleetID, NodeID: nodeID(n), DaemonID: n.DaemonID}) != nil {
		return model.Observation{}, model.ErrUnknownHealth
	}
	s, e := decodeReports(wire.Reports)
	if e != nil {
		return model.Observation{}, e
	}
	return model.Observation{Status: n.Status, Offline: true, DataVolume: n.DataVolume, LayoutVersion: "1", ReportStatsKnown: s.Known, PendingReports: s.Pending, FailedReports: s.Failed, ObservedAt: time.Now()}, nil
}
func validRef(n model.Node, ref model.OperationRef) bool {
	return n.Maintenance && ref.Namespace == n.Namespace && ref.NodeID == n.ID && ref.OperationID.Valid && ref.OperationID.Bytes != [16]byte{} && ref.Generation > 0 && ref.Generation == n.Generation && (ref.Action == model.Delete || ref.Action == model.Stop || ref.Action == model.Reboot)
}
func (p *Provider) Diagnose(ctx context.Context, n model.Node, ref model.OperationRef) (model.Observation, error) {
	if e := p.validNode(n); e != nil {
		return model.Observation{}, e
	}
	if !validRef(n, ref) {
		return model.Observation{}, model.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	o, e := p.Inspect(ctx, n)
	if e != nil {
		return model.Observation{}, e
	}
	if o.Status == "running" {
		if !o.Ready || !o.ReportStatsKnown || o.StartEpoch != n.StartEpoch || o.DaemonID != n.DaemonID {
			return o, model.ErrUnknownHealth
		}
	} else {
		if ref.Action != model.Delete || (o.Status != "stopped" && o.Status != "missing") {
			return o, model.ErrUnknownHealth
		}
		// Actual stopped/missing state, not stale SQL status, determines the offline observation.
		copy := n
		copy.Status = o.Status
		raw, err := p.engine.FixedOfflineReports(ctx, copy, ref)
		if err != nil {
			return model.Observation{}, safeError(err)
		}
		o, e = parseOffline(raw, copy, p.cfg)
		if e != nil {
			return model.Observation{}, e
		}
		if !o.ReportStatsKnown {
			return o, model.ErrUnknownHealth
		}
	}
	return o, nil
}

// Delete receives the original durable SQL operation, never an in-memory diagnostic permit.
func (p *Provider) Delete(ctx context.Context, n model.Node, ref model.OperationRef) error {
	if e := p.validNode(n); e != nil {
		return e
	}
	if !validRef(n, ref) || ref.Action != model.Delete || !n.Revoked || n.Desired != "terminating" || !volumeName.MatchString(n.DataVolume) || !volumeName.MatchString(n.SecretsVolume) || n.DataVolume == n.SecretsVolume {
		return model.ErrForbidden
	}
	if sdk, ok := p.engine.(*sdkEngine); ok {
		absent, e := sdk.nodeResourcesAbsent(ctx, n)
		if e != nil {
			return safeError(e)
		}
		if absent {
			return nil
		}
	}
	o, e := p.Inspect(ctx, n)
	if e != nil {
		return e
	}
	if o.Status != "stopped" && o.Status != "missing" {
		return model.ErrUnknownHealth
	}
	copy := n
	copy.Status = o.Status
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	raw, e := p.engine.FixedOfflineReports(c, copy, ref)
	cancel()
	if e != nil {
		return safeError(e)
	}
	fresh, e := parseOffline(raw, copy, p.cfg)
	if e != nil || !fresh.ReportStatsKnown {
		return model.ErrUnknownHealth
	}
	if fresh.PendingReports != 0 || fresh.FailedReports != 0 {
		return model.ErrBusy
	}
	if n.ContainerID != "" {
		i, e := p.inspectOwned(ctx, n, n.ContainerID)
		if e == nil {
			if i.State == "running" || i.State == "paused" || i.State == "restarting" {
				return model.ErrUnknownHealth
			}
			if e = bounded(ctx, func(c context.Context) error { return p.engine.Remove(c, n.ContainerID) }); e != nil {
				return safeError(e)
			}
		} else if !errdefs.IsNotFound(e) {
			return safeError(e)
		}
	}
	// Data is last: a crash or uncertain earlier deletion always leaves data for fresh proof/retry.
	for _, r := range []Resource{p.volume(n, "secrets"), p.volume(n, "data")} {
		if r.Role == "data" {
			r.deletion = &deletionContext{node: n, ref: ref}
		}
		if e = bounded(ctx, func(c context.Context) error { return p.engine.RemoveVolume(c, r) }); e != nil {
			return safeError(e)
		}
	}
	return nil
}
func (e *sdkEngine) nodeResourcesAbsent(ctx context.Context, n model.Node) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e.client == nil {
		return false, model.ErrUnavailable
	}
	absent := true
	id := n.ContainerID
	if id == "" {
		p := Provider{cfg: e.cfg}
		id = p.containerName(n)
	}
	c, err := e.client.ContainerInspect(ctx, id)
	if err == nil {
		absent = false
		if c.ContainerJSONBase == nil || c.Config == nil || !Owns(c.Config.Labels, n.Namespace, e.cfg.FleetID, nodeID(n), "node") || (n.ContainerID != "" && c.ID != n.ContainerID) {
			return false, model.ErrForbidden
		}
		if n.ContainerID == "" {
			return false, model.ErrUnknownHealth
		}
	} else if !errdefs.IsNotFound(err) {
		return false, model.ErrUnknownHealth
	}
	p := Provider{cfg: e.cfg}
	for _, r := range []Resource{p.volume(n, "data"), p.volume(n, "secrets")} {
		v, err := e.client.VolumeInspect(ctx, r.ID)
		if err == nil {
			absent = false
			if err = validateVolume(v, r); err != nil {
				return false, err
			}
		} else if !errdefs.IsNotFound(err) {
			return false, model.ErrUnknownHealth
		}
	}
	// Completion needs actual all-absence and no node-owned orphan/helper, not report-zero inference.
	resources, err := e.Find(ctx, map[string]string{"multica.fleet.node": nodeID(n)})
	if err != nil {
		return false, model.ErrUnknownHealth
	}
	for _, r := range resources {
		if r.Labels["multica.fleet.node"] != nodeID(n) {
			continue
		}
		if !Owns(r.Labels, n.Namespace, e.cfg.FleetID, nodeID(n), r.Role) {
			return false, model.ErrForbidden
		}
		if r.ID != n.ContainerID {
			return false, model.ErrUnknownHealth
		}
		absent = false
	}
	return absent, nil
}
func (e *sdkEngine) FixedOfflineReports(ctx context.Context, n model.Node, ref model.OperationRef) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if e.client == nil || !approvedImage.MatchString(e.cfg.Image) || e.cfg.FleetID == "" || n.Namespace != e.cfg.Namespace || !validRef(n, ref) || ref.Action != model.Delete {
		return nil, model.ErrUnknownHealth
	}
	if err := e.offlineRoot(ctx, n); err != nil {
		return nil, err
	}
	h := diagnosticHost()
	h.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: n.DataVolume, Target: model.DataMount, ReadOnly: true}}
	c := &container.Config{Image: e.cfg.Image, User: "10001:10001", Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"report-stats"}, Labels: labels(n.Namespace, e.cfg.FleetID, nodeID(n), "diagnostic"), NetworkDisabled: true}
	raw, err := e.runHelper(ctx, c, &h, nil)
	if err != nil {
		return nil, model.ErrUnknownHealth
	}
	// A helper cannot supply expected identity or conceal a writer that appeared while scanning.
	if err = e.offlineRoot(ctx, n); err != nil {
		return nil, err
	}
	if _, err = parseOffline(raw, n, e.cfg); err != nil {
		return nil, err
	}
	return raw, nil
}
func diagnosticHost() container.HostConfig {
	pids := int64(16)
	return container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}, Resources: container.Resources{NanoCPUs: 250000000, Memory: 64 << 20, PidsLimit: &pids}}
}
func (e *sdkEngine) offlineRoot(ctx context.Context, n model.Node) error {
	if !volumeName.MatchString(n.DataVolume) {
		return model.ErrUnknownHealth
	}
	r, err := e.client.VolumeInspect(ctx, n.DataVolume)
	if err != nil {
		return model.ErrUnknownHealth
	}
	if r.Name != n.DataVolume || r.Driver != "local" || len(r.Options) != 0 || !Owns(r.Labels, n.Namespace, e.cfg.FleetID, nodeID(n), "data") {
		return model.ErrForbidden
	}
	if n.ContainerID != "" {
		c, err := e.client.ContainerInspect(ctx, n.ContainerID)
		if err == nil {
			if c.ContainerJSONBase == nil || c.State == nil || c.Config == nil || c.ID != n.ContainerID || !Owns(c.Config.Labels, n.Namespace, e.cfg.FleetID, nodeID(n), "node") {
				return model.ErrForbidden
			}
			if c.State.Running || c.State.Paused || c.State.Restarting || c.Config.Image != n.Image {
				return model.ErrUnknownHealth
			}
			count := 0
			for _, m := range c.Mounts {
				if m.Destination == model.DataMount {
					count++
					if m.Type != mount.TypeVolume || m.Name != n.DataVolume {
						return model.ErrUnknownHealth
					}
				}
			}
			if count != 1 {
				return model.ErrUnknownHealth
			}
		} else if !errdefs.IsNotFound(err) {
			return model.ErrUnknownHealth
		}
	}
	return e.noWriter(ctx, n.DataVolume)
}
func (e *sdkEngine) noWriter(ctx context.Context, name string) error {
	// No label filter: a foreign container may be writing this volume.
	list, err := e.client.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return model.ErrUnknownHealth
	}
	for _, item := range list {
		c, err := e.client.ContainerInspect(ctx, item.ID)
		if err != nil {
			return model.ErrUnknownHealth
		}
		if c.ContainerJSONBase == nil || c.State == nil {
			return model.ErrUnknownHealth
		}
		if c.State.Running || c.State.Paused || c.State.Restarting {
			for _, m := range c.Mounts {
				if m.Type == mount.TypeVolume && m.Name == name && m.RW {
					return model.ErrUnknownHealth
				}
			}
		}
	}
	return nil
}
func (e *sdkEngine) runHelper(ctx context.Context, c *container.Config, h *container.HostConfig, archive []byte) (raw []byte, retErr error) {
	name := "multica-fleet-helper-" + uuid.NewString()
	id, createErr := e.Create(ctx, c, h, "", name)
	lookup := id
	if lookup == "" {
		lookup = name
	}
	actual, err := e.client.ContainerInspect(ctx, lookup)
	if err != nil {
		if createErr != nil {
			return nil, createErr
		}
		return nil, err
	}
	if actual.ContainerJSONBase == nil || actual.ID == "" || actual.Config == nil || !sameLabels(actual.Config.Labels, c.Labels) || (id != "" && actual.ID != id) {
		return nil, model.ErrForbidden
	}
	id = actual.ID
	defer func() {
		if err := e.cleanupHelper(context.WithoutCancel(ctx), id, c.Labels); err != nil {
			raw = nil
			retErr = model.ErrUnknownHealth
		}
	}()
	if err = validateHelper(actual, c, h); err != nil {
		return nil, err
	}
	if archive != nil {
		if err = e.client.CopyToContainer(ctx, id, "/", bytes.NewReader(archive), container.CopyToContainerOptions{CopyUIDGID: true}); err != nil {
			return nil, err
		}
	}
	if err = e.Start(ctx, id); err != nil {
		return nil, err
	}
	status, failed := e.client.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-failed:
		return nil, err
	case result := <-status:
		if result.StatusCode != 0 || result.Error != nil {
			return nil, model.ErrUnknownHealth
		}
	}
	if archive != nil {
		return nil, nil
	}
	logs, err := e.client.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return nil, err
	}
	defer logs.Close()
	stop := context.AfterFunc(ctx, func() { logs.Close() })
	defer stop()
	return boundedStdout(logs)
}
func sameLabels(a, b map[string]string) bool {
	for _, k := range []string{"multica.fleet.namespace", "multica.fleet.fleet_id", "multica.fleet.node", "multica.fleet.role"} {
		if b[k] == "" || a[k] != b[k] {
			return false
		}
	}
	return true
}
func validateHelper(actual container.InspectResponse, c *container.Config, h *container.HostConfig) error {
	if actual.Config == nil || actual.HostConfig == nil || actual.Config.Image != c.Image || actual.Config.User != c.User || actual.Config.Tty || !inspectEnvironment(actual.Config.Env, 0) || !reflect.DeepEqual(actual.Config.Entrypoint, c.Entrypoint) || !reflect.DeepEqual(actual.Config.Cmd, c.Cmd) {
		return model.ErrForbidden
	}
	ah := actual.HostConfig
	if ah.NetworkMode != "none" || !ah.ReadonlyRootfs || ah.Privileged || ah.PidMode != "" || len(ah.CapAdd) != 0 || len(ah.ExtraHosts) != 0 || len(ah.Devices) != 0 || len(ah.DeviceRequests) != 0 || ah.RestartPolicy.Name != container.RestartPolicyDisabled || len(ah.Binds) != 0 || len(ah.PortBindings) != 0 || len(ah.VolumesFrom) != 0 || ah.PublishAllPorts || !reflect.DeepEqual(ah.CapDrop, h.CapDrop) || !reflect.DeepEqual(ah.SecurityOpt, h.SecurityOpt) || ah.NanoCPUs != h.NanoCPUs || ah.Memory != h.Memory || ah.PidsLimit == nil || *ah.PidsLimit != *h.PidsLimit || len(actual.Mounts) != len(h.Mounts) {
		return model.ErrForbidden
	}
	for _, want := range h.Mounts {
		found := false
		for _, m := range actual.Mounts {
			if m.Type == mount.TypeVolume && m.Name == want.Source && m.Destination == want.Target && m.RW == !want.ReadOnly {
				found = true
			}
		}
		if !found {
			return model.ErrForbidden
		}
	}
	return nil
}
func (e *sdkEngine) cleanupHelper(ctx context.Context, id string, want map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := e.client.ContainerInspect(ctx, id)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.ContainerJSONBase == nil || r.ID != id || r.Config == nil || !sameLabels(r.Config.Labels, want) {
		return model.ErrForbidden
	}
	return e.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

// Strict framing avoids SDK StdCopy's unbounded frame allocation and truncated-frame EOF acceptance.
func boundedStdout(input io.Reader) ([]byte, error) {
	r := io.LimitReader(input, 2*diagnosticLimit+1)
	var out []byte
	total := 0
	for {
		var header [8]byte
		n, err := io.ReadFull(r, header[:])
		if err == io.EOF && n == 0 {
			return out, nil
		}
		if err != nil {
			return nil, model.ErrUnknownHealth
		}
		size := int(binary.BigEndian.Uint32(header[4:]))
		total += size + 8
		if total > 2*diagnosticLimit || size > diagnosticLimit || header[1] != 0 || header[2] != 0 || header[3] != 0 || (header[0] != 1 && header[0] != 2) {
			return nil, model.ErrUnknownHealth
		}
		data := make([]byte, size)
		if _, err = io.ReadFull(r, data); err != nil {
			return nil, model.ErrUnknownHealth
		}
		if header[0] == 2 && size != 0 {
			return nil, model.ErrUnknownHealth
		}
		if header[0] == 1 {
			if len(out)+size > diagnosticLimit {
				return nil, model.ErrUnknownHealth
			}
			out = append(out, data...)
		}
	}
}
