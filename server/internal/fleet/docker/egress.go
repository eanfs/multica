package docker

import (
	"reflect"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/multica-ai/multica/server/internal/fleet/model"
)

// The egress sidecar is the only path off an Aurora node's workspace-internal
// network. It joins the operator-provided uplink network and nothing else; the
// sandbox reaches it by the fixed Docker alias "egress". It never mounts an
// enrollment or provider credential.
const (
	egressProxyUser      = "10001:10001"
	egressTmpfs          = "rw,nosuid,nodev,noexec,size=33554432,uid=10001,gid=10001,mode=0700"
	egressProxyRole      = "egress-proxy"
	egressServerOriginEn = "MULTICA_EGRESS_SERVER_ORIGIN"
	egressAllowedHostsEn = "MULTICA_EGRESS_ALLOWED_HOSTS"
)

// Aurora node tmpfs surfaces. Every writable directory is nosuid, nodev and
// noexec; executables live in the read-only image.
const (
	auroraWorkspaceTmpfs = "rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700"
	auroraTmpTmpfs       = "rw,nosuid,nodev,noexec,size=268435456,uid=10001,gid=10001,mode=0700"
	auroraRunTmpfs       = "rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0755"
)

// EgressProxyArgs returns the documented Docker CLI argv for one workspace
// node's egress sidecar. It mirrors the retired aurorafleet policy exactly; the
// Fleet builds the same specification through the Engine seam. workspaceNetwork
// is the per-node network the sidecar is subsequently attached to.
func EgressProxyArgs(cfg model.Config, proxyName, workspaceNetwork string) ([]string, error) {
	a := cfg.Aurora
	if a == nil || proxyName == "" || workspaceNetwork == "" {
		return nil, model.ErrInvalidRequest
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return []string{
		"run", "--detach",
		"--pull", "never",
		"--name", proxyName,
		"--user", egressProxyUser,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--pids-limit", "64",
		"--memory", "256m",
		"--cpus", "0.25",
		"--tmpfs", model.AuroraTmpMount + ":" + egressTmpfs,
		"--network", a.UplinkNetwork,
		"-e", egressServerOriginEn + "=" + a.ServerURL,
		"-e", egressAllowedHostsEn + "=" + strings.Join(a.EgressHosts, ","),
		// The image is final so nothing can follow it as a command.
		a.ProxyImage,
	}, nil
}

// EgressNetworkConnectArgs returns the documented docker CLI argv that attaches
// the sidecar to the workspace-internal network under the fixed "egress" alias.
func EgressNetworkConnectArgs(proxyName, network string) []string {
	return []string{"network", "connect", "--alias", model.AuroraEgressAlias, network, proxyName}
}

// egressProxySpec builds the Engine-level sidecar specification. It carries no
// mounts and no credential: only the exact server origin and allowlist.
func egressProxySpec(cfg model.Config, n model.Node, proxyName string) (*container.Config, container.HostConfig, error) {
	a := cfg.Aurora
	if a == nil || proxyName == "" {
		return nil, container.HostConfig{}, model.ErrInvalidRequest
	}
	if err := a.Validate(); err != nil {
		return nil, container.HostConfig{}, err
	}
	pids := int64(64)
	return &container.Config{
			Image:  a.ProxyImage,
			User:   egressProxyUser,
			Labels: labels(n.Namespace, cfg.FleetID, nodeID(n), egressProxyRole),
			Env: []string{
				egressServerOriginEn + "=" + a.ServerURL,
				egressAllowedHostsEn + "=" + strings.Join(a.EgressHosts, ","),
			},
		}, container.HostConfig{
			NetworkMode:    container.NetworkMode(a.UplinkNetwork),
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			Resources:      container.Resources{NanoCPUs: 250000000, Memory: 256 << 20, PidsLimit: &pids},
			Tmpfs:          map[string]string{model.AuroraTmpMount: egressTmpfs},
		}, nil
}

// providerSecretMounts renders the operator-staged credential files as
// read-only bind mounts at the four fixed destinations. Empty entries are
// omitted.
func providerSecretMounts(a *model.AuroraConfig) []mount.Mount {
	if a == nil {
		return nil
	}
	mounts := a.ProviderSecretMounts()
	out := make([]mount.Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, mount.Mount{Type: mount.TypeBind, Source: m.Source, Target: m.Target, ReadOnly: true})
	}
	return out
}

// defaultImagePATH is the OCI default environment every image carries. It is the
// only image-owned key the sidecar adoption check tolerates.
const defaultImagePATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// egressEnvMatches accepts the exact owned proxy variables plus the image's
// default PATH, and rejects every other key or value. A credential, enrollment
// or provider variable can therefore never be adopted from a drifted sidecar.
func egressEnvMatches(actual, want []string) bool {
	owned := map[string]string{}
	for _, entry := range want {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return false
		}
		owned[key] = value
	}
	seen := map[string]bool{}
	for _, entry := range actual {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		if wantValue, isOwned := owned[key]; isOwned {
			if value != wantValue {
				return false
			}
			continue
		}
		if key == "PATH" && value == defaultImagePATH {
			continue
		}
		return false
	}
	for key := range owned {
		if !seen[key] {
			return false
		}
	}
	return true
}

// validateEgressSidecar checks an inspected sidecar against the immutable
// policy: the proxy image, the fixed user, the uplink-only network and no
// credential or enrollment mount.
func validateEgressSidecar(cfg model.Config, n model.Node, proxyName string, i container.InspectResponse) error {
	if i.ContainerJSONBase == nil || i.Config == nil || i.HostConfig == nil || cfg.Aurora == nil || i.ID == "" {
		return model.ErrForbidden
	}
	want, wantHost, err := egressProxySpec(cfg, n, proxyName)
	if err != nil {
		return model.ErrForbidden
	}
	c, h := i.Config, i.HostConfig
	if c.Image != want.Image || c.User != want.User || !sameLabels(c.Labels, want.Labels) || c.Tty || c.OpenStdin || len(c.ExposedPorts) != 0 {
		return model.ErrForbidden
	}
	// The image's default PATH is merged into the container env by the daemon;
	// the two owned proxy variables must match exactly and no other key is
	// adopted, so a credential or enrollment variable is still drift.
	if !egressEnvMatches(c.Env, want.Env) {
		return model.ErrForbidden
	}
	if h.NetworkMode != wantHost.NetworkMode || !h.ReadonlyRootfs || h.Privileged || h.PidMode != "" || len(h.Binds) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.VolumesFrom) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts || h.RestartPolicy.Name != container.RestartPolicyDisabled || len(h.CapAdd) != 0 || !reflect.DeepEqual(h.CapDrop, wantHost.CapDrop) || !reflect.DeepEqual(h.SecurityOpt, wantHost.SecurityOpt) || !reflect.DeepEqual(h.Tmpfs, wantHost.Tmpfs) || h.NanoCPUs != wantHost.NanoCPUs || h.Memory != wantHost.Memory || h.PidsLimit == nil || *h.PidsLimit != *wantHost.PidsLimit || len(i.Mounts) != 0 {
		return model.ErrForbidden
	}
	return nil
}
