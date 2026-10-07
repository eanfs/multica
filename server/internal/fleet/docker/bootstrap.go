package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/util"
)

// helperUser is the numeric uid helper containers run as. It deliberately omits
// the group: an archive copy (CopyToContainer with CopyUIDGID) makes Docker
// resolve the container's whole user spec through a chrooted getent, so the
// "uid:gid" form fails with `getent unable to find entry "10001:10001"` on
// Docker 25 - verified on the Amazon Linux 2023 host running Docker 25.0.16,
// where every annotation of the spec failed while the bare uid resolved and the
// process still ran as 10001:10001 because the image's passwd supplies the
// group. Later Docker versions accept either form.
const helperUser = "10001"

func (e *sdkEngine) InstallBootstrap(ctx context.Context, vols []Resource, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if e.client == nil || !approvedImage.MatchString(e.cfg.Image) || len(vols) != 2 || len(raw) > 1<<20 {
		return model.ErrInvalidRequest
	}
	data, secrets := vols[0], vols[1]
	if data.Role != "data" || secrets.Role != "secrets" || data.ID == secrets.ID || !e.ownsVolume(data) || !e.ownsVolume(secrets) || data.Labels["multica.fleet.node"] != secrets.Labels["multica.fleet.node"] {
		return model.ErrForbidden
	}
	if err := e.validateBootstrap(raw, data); err != nil {
		return err
	}
	for _, r := range vols {
		v, err := e.client.VolumeInspect(ctx, r.ID)
		if err != nil {
			return err
		}
		if err = validateVolume(v, r); err != nil {
			return err
		}
		if err = e.noWriter(ctx, r.ID); err != nil {
			return err
		}
	}
	h := diagnosticHost()
	// The daemon refuses CopyToContainer into a read-only rootfs, and the fixed
	// fleet-node bootstrap command consumes files from the mounted volumes rather
	// than stdin. Keep the installer writable while every other control (network
	// none, cap drop ALL, no-new-privileges, non-root, pid/memory limits) applies;
	// the archive still lands only in the two owned volumes.
	h.ReadonlyRootfs = false
	h.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: data.ID, Target: model.DataMount}, {Type: mount.TypeVolume, Source: secrets.ID, Target: "/secrets"}}
	c := &container.Config{Image: e.cfg.Image, User: helperUser, Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"bootstrap"}, Labels: labels(e.cfg.Namespace, e.cfg.FleetID, data.Labels["multica.fleet.node"], "bootstrap"), NetworkDisabled: true}
	_, err := e.runHelper(ctx, c, &h, raw)
	return err
}
func (e *sdkEngine) validateBootstrap(raw []byte, data Resource) error {
	if e.cfg.Aurora != nil {
		return e.validateAuroraBootstrap(raw, data)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return model.ErrInvalidRequest
		}
		if h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return model.ErrInvalidRequest
			}
			if _, exists := files[h.Name]; exists {
				return model.ErrInvalidRequest
			}
			files[h.Name] = b
		}
	}
	var manifest model.LayoutManifestData
	var bootstrap model.Bootstrap
	if _, err := model.DecodeStrictObject(files["data/fleet-layout.json"], &manifest); err != nil {
		return model.ErrInvalidRequest
	}
	// Bootstrap has optional omitempty tags; decode the fixed exact-key map before its typed schema.
	fields := map[string]json.RawMessage{}
	if _, err := model.DecodeStrictObject(files["secrets/bootstrap.json"], &fields); err != nil {
		return model.ErrInvalidRequest
	}
	allowed := map[string]bool{"node_token": true, "api_key": true, "server_url": true, "daemon_id": true, "base_url": true, "model": true}
	for key := range fields {
		if !allowed[key] {
			return model.ErrInvalidRequest
		}
	}
	if err := json.Unmarshal(files["secrets/bootstrap.json"], &bootstrap); err != nil {
		return model.ErrInvalidRequest
	}
	id, err := util.ParseUUID(data.Labels["multica.fleet.node"])
	if err != nil {
		return model.ErrForbidden
	}
	if bootstrap.NodeToken == "" || bootstrap.APIKey == "" || bootstrap.DaemonID == "" || bootstrap.DaemonID != manifest.DaemonID || bootstrap.ServerURL != e.cfg.APIURL {
		return model.ErrInvalidRequest
	}
	n := model.Node{ID: id, DaemonID: manifest.DaemonID}
	if err = model.ValidateLayoutManifest(files["data/fleet-layout.json"], model.LayoutIdentity{Namespace: e.cfg.Namespace, FleetID: e.cfg.FleetID, NodeID: nodeID(n), DaemonID: n.DaemonID}); err != nil {
		return model.ErrInvalidRequest
	}
	canonical, err := installerTar(n, e.cfg, bootstrap)
	if err != nil || !bytes.Equal(raw, canonical) {
		return model.ErrInvalidRequest
	}
	return nil
}

// validateAuroraBootstrap accepts exactly the two Aurora installer files and
// proves they are the canonical bytes for this node, namespace and identity. A
// Claude bootstrap payload is rejected and vice versa: the profiles never share
// an installer.
func (e *sdkEngine) validateAuroraBootstrap(raw []byte, data Resource) error {
	tr := tar.NewReader(bytes.NewReader(raw))
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return model.ErrInvalidRequest
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return model.ErrInvalidRequest
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return model.ErrInvalidRequest
		}
		if _, exists := files[h.Name]; exists {
			return model.ErrInvalidRequest
		}
		files[h.Name] = b
	}
	if len(files) != 2 {
		return model.ErrInvalidRequest
	}
	var manifest model.LayoutManifestData
	if _, err := model.DecodeStrictObject(files["data/fleet-layout.json"], &manifest); err != nil {
		return model.ErrInvalidRequest
	}
	token := string(files[auroraEnrollmentFile])
	if !model.ValidEnrollmentToken(token) || manifest.DaemonID == "" {
		return model.ErrInvalidRequest
	}
	id, err := util.ParseUUID(data.Labels["multica.fleet.node"])
	if err != nil {
		return model.ErrForbidden
	}
	n := model.Node{ID: id, DaemonID: manifest.DaemonID}
	if err := model.ValidateLayoutManifest(files["data/fleet-layout.json"], model.LayoutIdentity{Namespace: e.cfg.Namespace, FleetID: e.cfg.FleetID, NodeID: nodeID(n), DaemonID: n.DaemonID}); err != nil {
		return model.ErrInvalidRequest
	}
	canonical, err := installerTar(n, e.cfg, model.Bootstrap{EnrollmentToken: token, DaemonID: manifest.DaemonID})
	if err != nil || !bytes.Equal(raw, canonical) {
		return model.ErrInvalidRequest
	}
	return nil
}

// auroraEnrollmentFile is the fixed installer path for the managed enrollment
// secret. It lives inside the read-only secrets volume, next to the Claude-only
// bootstrap file it replaces.
const auroraEnrollmentFile = "secrets/aurora-enrollment"

// installerTar is the only producer of installer bytes. The deployment profile
// selects the private payload: the Claude-only node token, or the Aurora managed
// enrollment secret. Paths, ownership and the layout manifest are identical.
func installerTar(n model.Node, cfg model.Config, b model.Bootstrap) ([]byte, error) {
	if cfg.Aurora != nil {
		return auroraBootstrapTar(n, cfg, b)
	}
	return bootstrapTar(n, cfg, b)
}

// auroraBootstrapTar installs the layout manifest plus the single-use managed
// enrollment secret. It never writes a node token, API key or model URL.
func auroraBootstrapTar(n model.Node, cfg model.Config, b model.Bootstrap) ([]byte, error) {
	if !model.ValidEnrollmentToken(b.EnrollmentToken) || b.DaemonID == "" || b.DaemonID != n.DaemonID {
		return nil, model.ErrInvalidRequest
	}
	manifest := model.LayoutManifestData{Version: model.LayoutVersion, Namespace: cfg.Namespace, FleetID: cfg.FleetID, NodeID: nodeID(n), DaemonID: n.DaemonID, DataMount: model.DataMount, NodeHome: model.NodeHome, WorkspacesRoot: model.WorkspacesRoot}
	layout, e := json.Marshal(manifest)
	if e != nil {
		return nil, model.ErrInvalidRequest
	}
	return writeInstallerTar(layout, auroraEnrollmentFile, []byte(b.EnrollmentToken))
}

// writeInstallerTar is the shared archive writer, so validation can rebuild the
// exact bytes a producer would write.
func writeInstallerTar(manifest []byte, secretName string, secret []byte) ([]byte, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, dir := range []string{"data/", "data/home/", "data/workspaces/", "secrets/"} {
		if e := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0700, Uid: 10001, Gid: 10001}); e != nil {
			return nil, model.ErrInvalidRequest
		}
	}
	for _, file := range []struct {
		name string
		raw  []byte
	}{{"data/fleet-layout.json", manifest}, {secretName, secret}} {
		if e := tw.WriteHeader(&tar.Header{Name: file.name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(file.raw)), Uid: 10001, Gid: 10001}); e != nil {
			return nil, model.ErrInvalidRequest
		}
		if _, e := tw.Write(file.raw); e != nil {
			return nil, model.ErrInvalidRequest
		}
	}
	if e := tw.Close(); e != nil {
		return nil, model.ErrInvalidRequest
	}
	return out.Bytes(), nil
}

// bootstrapTar is the only producer of the Claude-only installer bytes. Paths and ownership are fixed.
func bootstrapTar(n model.Node, cfg model.Config, b model.Bootstrap) ([]byte, error) {
	manifest := model.LayoutManifestData{Version: model.LayoutVersion, Namespace: cfg.Namespace, FleetID: cfg.FleetID, NodeID: nodeID(n), DaemonID: n.DaemonID, DataMount: model.DataMount, NodeHome: model.NodeHome, WorkspacesRoot: model.WorkspacesRoot}
	layout, e := json.Marshal(manifest)
	if e != nil {
		return nil, model.ErrInvalidRequest
	}
	secret, e := json.Marshal(b)
	if e != nil {
		return nil, model.ErrInvalidRequest
	}
	return writeInstallerTar(layout, "secrets/bootstrap.json", secret)
}
