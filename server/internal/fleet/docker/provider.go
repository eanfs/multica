// Package docker manages only explicitly owned fleet resources through the Engine seam.
package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
)

type Provider struct {
	engine Engine
	cfg    model.Config
}

func New(e Engine, cfg model.Config) *Provider {
	cfg.Specs = nil // Node snapshots, not mutable administrator specs, drive existing resources.
	if sdk, ok := e.(*sdkEngine); ok {
		copy := *sdk
		copy.cfg = cfg
		e = &copy
	}
	return &Provider{engine: e, cfg: cfg}
}

var approvedImage = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)

func nodeID(n model.Node) string { return util.UUIDToString(n.ID) }
func labels(namespace, fleetID, node, role string) map[string]string {
	return map[string]string{"multica.fleet.namespace": namespace, "multica.fleet.fleet_id": fleetID, "multica.fleet.node": node, "multica.fleet.role": role}
}
func (p *Provider) validNode(n model.Node) error {
	if p.engine == nil {
		return model.ErrUnavailable
	}
	if p.cfg.Namespace == "" || p.cfg.FleetID == "" || n.Namespace != p.cfg.Namespace || !n.ID.Valid || n.ID.Bytes == [16]byte{} || !n.OwnerID.Valid || n.OwnerID.Bytes == [16]byte{} {
		return model.ErrForbidden
	}
	return nil
}
func safeError(e error) error {
	for _, known := range []error{model.ErrForbidden, model.ErrBusy, model.ErrConflict, model.ErrUnknownHealth, model.ErrInvalidRequest} {
		if errors.Is(e, known) {
			return known
		}
	}
	return model.ErrUnavailable
}
func (p *Provider) networkName() string {
	sum := sha256.Sum256([]byte(p.cfg.Namespace + "\x00" + p.cfg.FleetID))
	return "multica-fleet-" + hex.EncodeToString(sum[:12])
}
func (p *Provider) containerName(n model.Node) string { return p.networkName() + "-" + nodeID(n) }
func (p *Provider) volume(n model.Node, role string) Resource {
	name := n.DataVolume
	if role == "secrets" {
		name = n.SecretsVolume
	}
	return Resource{ID: name, Name: name, Role: role, Labels: labels(n.Namespace, p.cfg.FleetID, nodeID(n), role)}
}
func (p *Provider) CheckAvailability(ctx context.Context) error {
	if p.engine == nil || p.cfg.Namespace == "" || p.cfg.FleetID == "" {
		return model.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, e := p.engine.Find(ctx, map[string]string{"multica.fleet.namespace": p.cfg.Namespace, "multica.fleet.fleet_id": p.cfg.FleetID})
	if e != nil {
		return model.ErrUnavailable
	}
	return nil
}
func (p *Provider) Ensure(ctx context.Context, n model.Node, b model.Bootstrap) (model.Observation, error) {
	if n.Revoked || n.Maintenance || n.Desired == "terminating" || n.Desired == "terminated" {
		return model.Observation{}, model.ErrConflict
	}
	if e := p.validNode(n); e != nil {
		return model.Observation{}, e
	}
	if !approvedImage.MatchString(p.cfg.Image) || n.Image == "" || !volumeName.MatchString(n.DataVolume) || !volumeName.MatchString(n.SecretsVolume) || n.DataVolume == n.SecretsVolume || n.Resources.CPUs <= 0 || n.Resources.MemoryBytes <= 0 || n.Resources.Pids <= 0 || n.Resources.MaxRuns <= 0 {
		return model.Observation{}, model.ErrInvalidRequest
	}
	id := n.ContainerID
	if id == "" {
		id = p.containerName(n)
	}
	i, e := p.inspectOwned(ctx, n, id)
	if e == nil {
		return p.ensureStarted(ctx, n, i)
	}
	if !errdefs.IsNotFound(e) {
		return model.Observation{}, safeError(e)
	}
	// A persisted container that disappeared is never replaced with a new identity.
	if n.ContainerID != "" {
		return model.Observation{Status: "missing", ObservedAt: time.Now()}, model.ErrConflict
	}
	if b.DaemonID != n.DaemonID || b.NodeToken == "" || b.APIKey == "" || b.ServerURL != p.cfg.APIURL {
		return model.Observation{}, model.ErrInvalidRequest
	}
	network := Resource{Name: p.networkName(), Role: "network", Labels: labels(n.Namespace, p.cfg.FleetID, "namespace", "network")}
	if e = bounded(ctx, func(c context.Context) error { return p.engine.EnsureNetwork(c, network) }); e != nil {
		return model.Observation{}, safeError(e)
	}
	vols := []Resource{p.volume(n, "data"), p.volume(n, "secrets")}
	for _, r := range vols {
		if e = bounded(ctx, func(c context.Context) error { return p.engine.EnsureVolume(c, r) }); e != nil {
			return model.Observation{}, safeError(e)
		}
	}
	tar, e := bootstrapTar(n, p.cfg, b)
	if e != nil {
		return model.Observation{}, e
	}
	if e = bounded(ctx, func(c context.Context) error { return p.engine.InstallBootstrap(c, vols, tar) }); e != nil {
		return model.Observation{}, safeError(e)
	}
	h := NodeHostConfig(n.Resources, true)
	h.NetworkMode = container.NetworkMode(network.Name)
	h.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: n.DataVolume, Target: model.DataMount}, {Type: mount.TypeVolume, Source: n.SecretsVolume, Target: "/secrets", ReadOnly: true}}
	c := &container.Config{Image: n.Image, User: "10001:10001", Labels: labels(n.Namespace, p.cfg.FleetID, nodeID(n), "node"), Env: []string{"HOME=" + model.NodeHome, "FLEET_NODE_MAX_RUNS=" + strconv.Itoa(n.Resources.MaxRuns)}, Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"run"}, Healthcheck: &container.HealthConfig{Test: []string{"CMD", "/usr/local/bin/fleet-node", "health"}, Interval: 5 * time.Second, Timeout: 5 * time.Second, Retries: 3}}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	created, createErr := p.engine.Create(callCtx, c, &h, network.Name, p.containerName(n))
	cancel()
	// Even an uncertain create is followed by a new independent inspect before adoption/retry.
	lookup := created
	if lookup == "" {
		lookup = p.containerName(n)
	}
	i, e = p.inspectOwned(ctx, n, lookup)
	if e != nil {
		if createErr != nil {
			return model.Observation{}, safeError(createErr)
		}
		return model.Observation{}, safeError(e)
	}
	return p.ensureStarted(ctx, n, i)
}
func bounded(ctx context.Context, f func(context.Context) error) error {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return f(c)
}
func (p *Provider) ensureStarted(ctx context.Context, n model.Node, i Inspection) (model.Observation, error) {
	if e := p.validSDKSnapshot(n, i); e != nil {
		return model.Observation{}, e
	}
	if i.State != "running" {
		if e := bounded(ctx, func(c context.Context) error { return p.engine.Start(c, i.ID) }); e != nil {
			return model.Observation{}, safeError(e)
		}
	}
	n.ContainerID = i.ID
	o := p.observation(ctx, n, i)
	if i.State != "running" {
		o.Status = "starting"
	}
	return o, nil
}
func (p *Provider) Apply(ctx context.Context, n model.Node, action model.Action) (model.Observation, error) {
	if e := p.validNode(n); e != nil {
		return model.Observation{}, e
	}
	if n.Revoked || n.Desired == "terminating" || n.Desired == "terminated" {
		return model.Observation{}, model.ErrConflict
	}
	if action != model.Start && action != model.Stop && action != model.Reboot {
		return model.Observation{}, model.ErrInvalidRequest
	}
	if (action == model.Start && n.Maintenance) || (action != model.Start && !n.Maintenance) {
		return model.Observation{}, model.ErrConflict
	}
	if n.ContainerID == "" {
		return model.Observation{Status: "missing", ObservedAt: time.Now()}, model.ErrConflict
	}
	i, e := p.inspectOwned(ctx, n, n.ContainerID)
	if e != nil {
		if errdefs.IsNotFound(e) && action == model.Stop {
			return model.Observation{Status: "missing", ObservedAt: time.Now()}, nil
		}
		return model.Observation{}, safeError(e)
	}
	if e = p.validSDKSnapshot(n, i); e != nil {
		return model.Observation{}, e
	}
	if action == model.Stop || action == model.Reboot {
		if e = bounded(ctx, func(c context.Context) error { return p.engine.Stop(c, n.ContainerID) }); e != nil {
			return model.Observation{}, safeError(e)
		}
	}
	if action == model.Start || action == model.Reboot {
		if e = bounded(ctx, func(c context.Context) error { return p.engine.Start(c, n.ContainerID) }); e != nil {
			return model.Observation{}, safeError(e)
		}
		n.StartEpoch = ""
	}
	return p.Inspect(ctx, n)
}
func NodeHostConfig(spec model.Spec, linuxHostGateway bool) container.HostConfig {
	h := container.HostConfig{Resources: container.Resources{NanoCPUs: int64(spec.CPUs) * 1e9, Memory: spec.MemoryBytes, PidsLimit: &spec.Pids}, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}}
	if linuxHostGateway {
		h.ExtraHosts = []string{"host.docker.internal:host-gateway"}
	}
	return h
}
func Owns(labels map[string]string, namespace, fleetID, nodeID, role string) bool {
	return namespace != "" && fleetID != "" && nodeID != "" && role != "" && labels["multica.fleet.namespace"] == namespace && labels["multica.fleet.fleet_id"] == fleetID && labels["multica.fleet.node"] == nodeID && labels["multica.fleet.role"] == role
}
