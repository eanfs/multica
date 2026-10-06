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
	c := &container.Config{Image: e.cfg.Image, User: "10001:10001", Entrypoint: []string{"/usr/local/bin/fleet-node"}, Cmd: []string{"bootstrap"}, Labels: labels(e.cfg.Namespace, e.cfg.FleetID, data.Labels["multica.fleet.node"], "bootstrap"), NetworkDisabled: true}
	_, err := e.runHelper(ctx, c, &h, raw)
	return err
}
func (e *sdkEngine) validateBootstrap(raw []byte, data Resource) error {
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
	canonical, err := bootstrapTar(n, e.cfg, bootstrap)
	if err != nil || !bytes.Equal(raw, canonical) {
		return model.ErrInvalidRequest
	}
	return nil
}

// bootstrapTar is the only producer of installer bytes. Paths and ownership are fixed.
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
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, dir := range []string{"data/", "data/home/", "data/workspaces/", "secrets/"} {
		if e = tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0700, Uid: 10001, Gid: 10001}); e != nil {
			return nil, model.ErrInvalidRequest
		}
	}
	for _, file := range []struct {
		name string
		raw  []byte
	}{{"data/fleet-layout.json", layout}, {"secrets/bootstrap.json", secret}} {
		if e = tw.WriteHeader(&tar.Header{Name: file.name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(file.raw)), Uid: 10001, Gid: 10001}); e != nil {
			return nil, model.ErrInvalidRequest
		}
		if _, e = tw.Write(file.raw); e != nil {
			return nil, model.ErrInvalidRequest
		}
	}
	if e = tw.Close(); e != nil {
		return nil, model.ErrInvalidRequest
	}
	return out.Bytes(), nil
}
