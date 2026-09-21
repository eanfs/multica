package aurorafleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Env keys the controller injects into every provisioned node so the sandbox
// daemon can reach the main server and register as managed. They mirror the
// server's own configuration names: MULTICA_SERVER_URL is the API/control-plane
// base the daemon dials, AURORA_SANDBOX_TOKEN is the shared secret presented to
// POST /api/daemon/managed/register.
const (
	EnvServerURL    = "MULTICA_SERVER_URL"
	EnvSandboxToken = "AURORA_SANDBOX_TOKEN"
)

// Config wires a Controller to a node backend and the sandbox bootstrap values
// injected into provisioned nodes.
type Config struct {
	Backend Backend
	// SandboxImage is provisioned when a create request omits an image.
	SandboxImage string
	// ServerURL is the main Multica server the sandbox daemon dials.
	ServerURL string
	// SandboxToken is the managed-registration secret. It is injected into
	// nodes and never returned to the API caller.
	SandboxToken string
}

// Controller exposes the cloudruntime-compatible node API over a Backend. The
// main server's cloudruntime proxy — baseURL pointed here — forwards these exact
// paths verbatim, so the controller mirrors that surface: GET/POST/DELETE
// /api/v1/nodes and /nodes/{start,stop,reboot,status,exec}, plus /healthz and
// /readyz.
type Controller struct {
	cfg Config
	// env is the bootstrap environment injected into every provisioned node,
	// built once from the immutable config instead of per request.
	env map[string]string
}

// NewController returns a Controller. A nil Backend defaults to an in-memory
// backend so a controller is always usable.
func NewController(cfg Config) *Controller {
	if cfg.Backend == nil {
		cfg.Backend = NewMemoryBackend()
	}
	return &Controller{cfg: cfg, env: bootstrapEnv(cfg)}
}

// Handler returns the controller's HTTP routes.
func (c *Controller) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Get("/healthz", c.health)
	r.Get("/readyz", c.ready)
	r.Get("/api/v1/", c.serviceInfo)

	r.Route("/api/v1/nodes", func(r chi.Router) {
		r.Get("/", c.listNodes)
		r.Post("/", c.createNode)
		r.Delete("/", c.deleteNode)
		r.Post("/start", c.startNode)
		r.Post("/stop", c.stopNode)
		r.Post("/reboot", c.rebootNode)
		r.Post("/status", c.statusNode)
		r.Post("/exec", c.execNode)
	})

	return r
}

// health reports the process is up, independent of the node backend.
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

func (c *Controller) serviceInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "aurora-fleet",
		"endpoint": "/api/v1/nodes",
	})
}

// nodeActionRequest is the body for the id-addressed operations
// (delete/start/stop/reboot/status/exec).
type nodeActionRequest struct {
	ID string `json:"id"`
}

// createNodeRequest is the self-host provision body. All fields are optional:
// the controller fills image and env from its own config.
type createNodeRequest struct {
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels"`
}

// execNodeRequest carries the command to run inside a node.
type execNodeRequest struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
}

func (c *Controller) createNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	node, err := c.cfg.Backend.Create(r.Context(), CreateRequest{
		Name:   req.Name,
		Image:  firstNonEmpty(req.Image, c.cfg.SandboxImage),
		Env:    c.env,
		Labels: req.Labels,
	})
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, node)
}

func (c *Controller) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := c.cfg.Backend.List(r.Context())
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (c *Controller) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, ok := c.nodeID(w, r)
	if !ok {
		return
	}
	if err := c.cfg.Backend.Terminate(r.Context(), id); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Controller) startNode(w http.ResponseWriter, r *http.Request) {
	c.applyNodeAction(w, r, c.cfg.Backend.Start)
}

func (c *Controller) stopNode(w http.ResponseWriter, r *http.Request) {
	c.applyNodeAction(w, r, c.cfg.Backend.Stop)
}

func (c *Controller) rebootNode(w http.ResponseWriter, r *http.Request) {
	c.applyNodeAction(w, r, c.cfg.Backend.Reboot)
}

// nodeID decodes an id-addressed action body and returns the node id, writing a
// 400 when the body is malformed or the id is missing.
func (c *Controller) nodeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req nodeActionRequest
	if !decodeJSON(w, r, &req) {
		return "", false
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return "", false
	}
	return req.ID, true
}

// applyNodeAction runs a mutating backend operation addressed by id, then
// returns the node's post-operation state.
func (c *Controller) applyNodeAction(w http.ResponseWriter, r *http.Request, op func(ctx context.Context, id string) error) {
	id, ok := c.nodeID(w, r)
	if !ok {
		return
	}
	if err := op(r.Context(), id); err != nil {
		writeBackendError(w, err)
		return
	}
	node, err := c.cfg.Backend.Status(r.Context(), id)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (c *Controller) statusNode(w http.ResponseWriter, r *http.Request) {
	id, ok := c.nodeID(w, r)
	if !ok {
		return
	}
	node, err := c.cfg.Backend.Status(r.Context(), id)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (c *Controller) execNode(w http.ResponseWriter, r *http.Request) {
	var req execNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	if len(req.Command) == 0 {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	res, err := c.cfg.Backend.Exec(r.Context(), req.ID, req.Command)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": res.ExitCode,
		"stdout":    string(res.Stdout),
		"stderr":    string(res.Stderr),
	})
}

// bootstrapEnv is the environment injected into every provisioned node. It is
// the controller's secret: it never appears in a response body.
func bootstrapEnv(cfg Config) map[string]string {
	env := make(map[string]string, 2)
	if cfg.ServerURL != "" {
		env[EnvServerURL] = cfg.ServerURL
	}
	if cfg.SandboxToken != "" {
		env[EnvSandboxToken] = cfg.SandboxToken
	}
	return env
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
