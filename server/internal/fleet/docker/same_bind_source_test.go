package docker

// Regression coverage for Aurora node admission on Docker Desktop: the provider
// builds a bind mount from the configured host path, while Docker Desktop for
// macOS reports the same mount translated into the VM's path space as
// "/host_mnt" + the host path. The adoption authority must accept that one
// documented mapping without accepting any other source difference.

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// auroraInspection builds a complete, otherwise valid Aurora node inspection
// snapshot for cfg and n. Callers mutate exactly one field to model drift.
func auroraInspection(t *testing.T, n model.Node, cfg model.Config) container.InspectResponse {
	t.Helper()
	seccompJSON, err := resolveAuroraSeccomp(cfg.Aurora)
	if err != nil {
		t.Fatalf("resolve seccomp: %v", err)
	}
	p := &Provider{cfg: cfg}
	networkName := p.workspaceNetwork(n).Name
	h := NodeHostConfig(n.Resources, true, cfg.Aurora, seccompJSON)
	h.NetworkMode = container.NetworkMode(networkName)
	h.Mounts = []mount.Mount{
		{Type: mount.TypeVolume, Source: n.DataVolume, Target: model.DataMount},
		{Type: mount.TypeVolume, Source: n.SecretsVolume, Target: model.AuroraEnrollmentDir, ReadOnly: true},
	}
	h.Mounts = append(h.Mounts, providerSecretMounts(cfg.Aurora)...)
	r := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: "cid", HostConfig: &h},
		Config:            &container.Config{Image: n.Image, User: "10001:10001", Env: p.nodeEnv(n), Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"run"}},
		NetworkSettings:   &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{networkName: {}}},
	}
	for _, m := range h.Mounts {
		if m.Type == mount.TypeBind {
			r.Mounts = append(r.Mounts, container.MountPoint{Type: mount.TypeBind, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly})
			continue
		}
		r.Mounts = append(r.Mounts, container.MountPoint{Type: mount.TypeVolume, Name: m.Source, Destination: m.Target, RW: !m.ReadOnly})
	}
	return r
}

// configureProviderSecret returns a fresh Aurora config with exactly one
// provider credential bind mount at source.
func auroraConfigWithProviderSecret(t *testing.T, source string) model.Config {
	t.Helper()
	cfg := auroraConfigWithSeccomp(t)
	cfg.Aurora.ProviderSecretFiles = map[string]string{"anthropic-api-key": source}
	return cfg
}

// TestInspectAuroraBindSourceAcceptsDockerDesktopTranslation is the RED/GREEN
// regression: the verbatim Linux source is accepted, and so is the documented
// Docker Desktop translation where the inspected source is the configured host
// path with the fixed "/host_mnt" prefix.
func TestInspectAuroraBindSourceAcceptsDockerDesktopTranslation(t *testing.T) {
	cfg := auroraConfigWithProviderSecret(t, "/etc/multica/aurora/anthropic-api-key")
	n := auroraNode()
	networkName := (&Provider{cfg: cfg}).workspaceNetwork(n).Name

	if err := validateNodeInspection(auroraInspection(t, n, cfg), n, networkName, cfg); err != nil {
		t.Fatalf("verbatim bind source rejected: %v", err)
	}

	desktop := auroraInspection(t, n, cfg)
	translated := false
	for i := range desktop.Mounts {
		if desktop.Mounts[i].Type == mount.TypeBind {
			desktop.Mounts[i].Source = "/host_mnt" + desktop.Mounts[i].Source
			translated = true
		}
	}
	if !translated {
		t.Fatal("fixture produced no bind mount to translate")
	}
	if err := validateNodeInspection(desktop, n, networkName, cfg); err != nil {
		t.Fatalf("Docker Desktop translated bind source rejected: %v", err)
	}
}

// TestInspectAuroraBindSourceStillRejectsDrift proves the tolerant mapping did
// not weaken ownership: any other source difference, plus mount count,
// destination and read-only drift, still fails closed.
func TestInspectAuroraBindSourceStillRejectsDrift(t *testing.T) {
	cfg := auroraConfigWithProviderSecret(t, "/etc/multica/aurora/anthropic-api-key")
	n := auroraNode()
	networkName := (&Provider{cfg: cfg}).workspaceNetwork(n).Name

	for _, tc := range []struct {
		name   string
		mutate func(*container.InspectResponse)
	}{
		{name: "foreign-source", mutate: func(r *container.InspectResponse) {
			findBind(t, r).Source = "/tmp/foreign/anthropic-api-key"
		}},
		{name: "host-mnt-prefix-only", mutate: func(r *container.InspectResponse) {
			findBind(t, r).Source = "/host_mnt"
		}},
		{name: "host-mnt-other-path", mutate: func(r *container.InspectResponse) {
			findBind(t, r).Source = "/host_mnt/etc/multica/aurora/other-api-key"
		}},
		{name: "host-mnt-missing-slash", mutate: func(r *container.InspectResponse) {
			findBind(t, r).Source = "/host_mntetc/multica/aurora/anthropic-api-key"
		}},
		{name: "missing-mount", mutate: func(r *container.InspectResponse) {
			for i, m := range r.Mounts {
				if m.Type == mount.TypeBind {
					r.Mounts = append(r.Mounts[:i], r.Mounts[i+1:]...)
					return
				}
			}
		}},
		{name: "extra-mount", mutate: func(r *container.InspectResponse) {
			r.Mounts = append(r.Mounts, container.MountPoint{Type: mount.TypeBind, Source: "/etc/multica/aurora/extra", Destination: "/run/secrets/extra", RW: false})
		}},
		{name: "destination-drift", mutate: func(r *container.InspectResponse) {
			findBind(t, r).Destination = "/run/secrets/other"
		}},
		{name: "writable", mutate: func(r *container.InspectResponse) {
			findBind(t, r).RW = true
		}},
		{name: "volume-name-drift", mutate: func(r *container.InspectResponse) {
			r.Mounts[0].Name = "foreign-data"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := auroraInspection(t, n, cfg)
			tc.mutate(&r)
			if err := validateNodeInspection(r, n, networkName, cfg); err == nil {
				t.Fatalf("unsafe %s snapshot adopted", tc.name)
			}
		})
	}
}

func findBind(t *testing.T, r *container.InspectResponse) *container.MountPoint {
	t.Helper()
	for i := range r.Mounts {
		if r.Mounts[i].Type == mount.TypeBind {
			return &r.Mounts[i]
		}
	}
	t.Fatal("no bind mount in fixture")
	return nil
}

// TestSameBindSource pins the helper's single tolerated mapping directly.
func TestSameBindSource(t *testing.T) {
	const want = "/etc/multica/aurora/anthropic-api-key"
	for _, tc := range []struct {
		name      string
		inspected string
		want      bool
	}{
		{name: "verbatim", inspected: want, want: true},
		{name: "desktop", inspected: "/host_mnt" + want, want: true},
		{name: "foreign", inspected: "/tmp/foreign", want: false},
		{name: "prefix-only", inspected: "/host_mnt", want: false},
		{name: "prefix-no-slash", inspected: "/host_mnt" + want[1:], want: false},
		{name: "nested-host-mnt", inspected: "/host_mnt/host_mnt" + want, want: false},
		{name: "empty", inspected: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameBindSource(tc.inspected, want); got != tc.want {
				t.Fatalf("sameBindSource(%q, %q) = %v, want %v", tc.inspected, want, got, tc.want)
			}
		})
	}
}
