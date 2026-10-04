package model

import (
	"fmt"
	"strings"
)

const (
	DataMount      = "/data"
	NodeHome       = "/data/home"
	WorkspacesRoot = "/data/workspaces"
	LayoutVersion  = 1
	LayoutManifest = "/data/fleet-layout.json"
)

type LayoutIdentity struct{ Namespace, FleetID, NodeID, DaemonID string }

// LayoutManifestData is the sole v1 manifest schema shared by producers and validators.
// All fields are required; identities are exact opaque strings, not inferred from paths.
type LayoutManifestData struct {
	Version        int    `json:"version"`
	Namespace      string `json:"namespace"`
	FleetID        string `json:"fleet_id"`
	NodeID         string `json:"node_id"`
	DaemonID       string `json:"daemon_id"`
	DataMount      string `json:"data_mount"`
	NodeHome       string `json:"node_home"`
	WorkspacesRoot string `json:"workspaces_root"`
}

// ValidateLayoutManifest validates bytes only. Filesystem trust and ownership are caller concerns.
func ValidateLayoutManifest(raw []byte, want LayoutIdentity) error {
	if strings.TrimSpace(want.Namespace) == "" || strings.TrimSpace(want.FleetID) == "" ||
		strings.TrimSpace(want.NodeID) == "" || strings.TrimSpace(want.DaemonID) == "" {
		return ErrInvalidRequest
	}
	var manifest LayoutManifestData
	if _, err := decodeObject(raw, &manifest); err != nil {
		return err
	}
	if manifest.Version != LayoutVersion || manifest.Namespace != want.Namespace || manifest.FleetID != want.FleetID ||
		manifest.NodeID != want.NodeID || manifest.DaemonID != want.DaemonID || manifest.DataMount != DataMount ||
		manifest.NodeHome != NodeHome || manifest.WorkspacesRoot != WorkspacesRoot {
		return fmt.Errorf("%w: layout identity, version or paths mismatch", ErrInvalidRequest)
	}
	return nil
}
