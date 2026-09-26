package aurorafleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	// maxEnsureBodyBytes caps the ensure request body. The request carries
	// identity only, so anything larger is abuse or a future caller trying to
	// smuggle runtime configuration.
	maxEnsureBodyBytes = 4 << 10
	// enrollmentFileMode and secretDirMode are the permissions of the staged
	// enrollment secret and its parent directory.
	enrollmentFileMode = 0o400
	secretDirMode      = 0o700
	// enrollmentFileName is the file name the secret is staged under inside
	// the per-node secret directory.
	enrollmentFileName = "enrollment"
)

// uuidPattern matches the canonical 8-4-4-4-12 UUID form all identity fields
// use. Plan A persists sandbox nodes, runtimes, and daemons as UUIDs.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// enrollmentTokenPattern matches the mse_ enrollment secret plan A issues:
// the prefix plus exactly 40 lowercase hex characters. It is a format check
// only; the secret is validated against the database by the server on use.
var enrollmentTokenPattern = regexp.MustCompile(`^mse_[0-9a-f]{40}$`)

// Config wires a Controller to a node backend, the control bearer gate, and
// the secret root enrollment secrets are staged under.
type Config struct {
	Backend Backend
	// Auth gates every route except /healthz. It is required.
	Auth *ControlAuth
	// SecretRoot is the directory under which per-node enrollment secrets are
	// staged. It is required.
	SecretRoot string
}

// Controller exposes the authenticated internal workspace-node API over a
// Backend. There is deliberately no generic node surface: callers can ensure,
// inspect, and delete exactly one kind of node, with identity fields only.
type Controller struct {
	cfg Config
}

// NewController returns a Controller. A nil Backend defaults to an in-memory
// backend so a controller is always usable.
func NewController(cfg Config) *Controller {
	if cfg.Backend == nil {
		cfg.Backend = NewMemoryBackend()
	}
	return &Controller{cfg: cfg}
}

// Handler returns the controller's HTTP routes. Only these exist:
//
//	PUT    /internal/v1/workspace-nodes/{nodeID}
//	GET    /internal/v1/workspace-nodes/{nodeID}
//	DELETE /internal/v1/workspace-nodes/{nodeID}
//	GET    /healthz
//	GET    /readyz
func (c *Controller) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Get("/healthz", c.health)
	r.Group(func(r chi.Router) {
		r.Use(c.cfg.Auth.Middleware)
		r.Get("/readyz", c.ready)
		r.Route("/internal/v1/workspace-nodes", func(r chi.Router) {
			r.Put("/{nodeID}", c.ensureNode)
			r.Get("/{nodeID}", c.nodeStatus)
			r.Delete("/{nodeID}", c.deleteNode)
		})
	})

	return r
}

// health reports the process is up, independent of the node backend. It is
// the only unauthenticated route.
func (c *Controller) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready reports whether the node backend is reachable, so an orchestrator can
// hold traffic until the fleet can actually provision.
func (c *Controller) ready(w http.ResponseWriter, r *http.Request) {
	if err := c.cfg.Backend.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "node backend unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ensureRequest is the exact ensure body. There is no image, command,
// environment, label, mount, or resource field by construction: unknown JSON
// fields are rejected, so the surface cannot grow through the wire.
type ensureRequest struct {
	NodeID          string `json:"node_id"`
	WorkspaceID     string `json:"workspace_id"`
	RuntimeID       string `json:"runtime_id"`
	DaemonID        string `json:"daemon_id"`
	EnrollmentToken string `json:"enrollment_token"`
}

// ensureNode provisions (or confirms) the workspace node addressed by the
// path, staging the enrollment secret into a host file the backend mounts.
func (c *Controller) ensureNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "nodeID")
	var req ensureRequest
	body := io.LimitReader(r.Body, maxEnsureBodyBytes+1)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.NodeID != nodeID {
		writeError(w, http.StatusBadRequest, "node_id must match the URL path")
		return
	}
	if !uuidPattern.MatchString(req.NodeID) || !uuidPattern.MatchString(req.WorkspaceID) ||
		!uuidPattern.MatchString(req.RuntimeID) || !uuidPattern.MatchString(req.DaemonID) {
		writeError(w, http.StatusBadRequest, "node_id, workspace_id, runtime_id, and daemon_id must be UUIDs")
		return
	}
	if !enrollmentTokenPattern.MatchString(req.EnrollmentToken) {
		writeError(w, http.StatusBadRequest, "enrollment_token format is invalid")
		return
	}

	secretPath, err := writeEnrollmentSecret(c.cfg.SecretRoot, req.NodeID, req.EnrollmentToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stage enrollment secret")
		return
	}

	node, err := c.cfg.Backend.EnsureWorkspaceNode(r.Context(), WorkspaceNodeSpec{
		NodeID:         req.NodeID,
		WorkspaceID:    req.WorkspaceID,
		RuntimeID:      req.RuntimeID,
		DaemonID:       req.DaemonID,
		EnrollmentFile: secretPath,
	})
	removeEnrollmentSecret(secretPath)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// nodeStatus returns the node's current state.
func (c *Controller) nodeStatus(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := c.pathNodeID(w, r)
	if !ok {
		return
	}
	node, err := c.cfg.Backend.WorkspaceNodeStatus(r.Context(), nodeID)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// deleteNode destroys the node and removes any leftover staged secret.
func (c *Controller) deleteNode(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := c.pathNodeID(w, r)
	if !ok {
		return
	}
	if err := c.cfg.Backend.DeleteWorkspaceNode(r.Context(), nodeID); err != nil {
		writeBackendError(w, err)
		return
	}
	removeEnrollmentSecret(filepath.Join(c.cfg.SecretRoot, nodeID, enrollmentFileName))
	w.WriteHeader(http.StatusNoContent)
}

// pathNodeID returns the URL's node ID after checking it is a UUID, so a
// crafted path segment can never reach the backend or the secret-root path
// arithmetic below.
func (c *Controller) pathNodeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	nodeID := chi.URLParam(r, "nodeID")
	if !uuidPattern.MatchString(nodeID) {
		writeError(w, http.StatusBadRequest, "node id must be a UUID")
		return "", false
	}
	return nodeID, true
}

// writeEnrollmentSecret stages the enrollment secret at
// <secret-root>/<nodeID>/enrollment. The parent directory is 0700, the file
// is created exclusively at 0400 via an atomic rename in the same directory,
// and only the resulting absolute path ever leaves this function.
func writeEnrollmentSecret(secretRoot, nodeID, token string) (string, error) {
	dir := filepath.Join(secretRoot, nodeID)
	if err := os.MkdirAll(dir, secretDirMode); err != nil {
		return "", fmt.Errorf("create node secret dir: %w", err)
	}
	if err := os.Chmod(dir, secretDirMode); err != nil {
		return "", fmt.Errorf("restrict node secret dir: %w", err)
	}
	final := filepath.Join(dir, enrollmentFileName)
	tmp, err := os.OpenFile(final+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, enrollmentFileMode)
	if err != nil {
		return "", fmt.Errorf("stage enrollment secret: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write enrollment secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("close enrollment secret: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("publish enrollment secret: %w", err)
	}
	return final, nil
}

// removeEnrollmentSecret deletes a staged secret file, ignoring absence.
func removeEnrollmentSecret(path string) {
	_ = os.Remove(path)
}

func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "node not found")
	case errors.Is(err, ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "node backend unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "node operation failed")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
