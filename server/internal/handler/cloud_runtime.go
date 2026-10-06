package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/logger"
)

const maxCloudRuntimeRequestBodySize = 1 << 20

type cloudRuntimeProxyOptions struct {
	withUserID bool
	withQuery  bool
	withBody   bool
	billing    bool
}

func (h *Handler) GetCloudRuntimeService(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/", cloudRuntimeProxyOptions{
		withUserID: true,
	})
}

func (h *Handler) GetCloudRuntimeHealth(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/healthz", cloudRuntimeProxyOptions{})
}

func (h *Handler) GetCloudRuntimeReady(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/readyz", cloudRuntimeProxyOptions{})
}

func (h *Handler) ListCloudRuntimeNodes(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodGet, "/api/v1/nodes", cloudRuntimeProxyOptions{
		withUserID: true,
		withQuery:  true,
	})
}

func (h *Handler) CreateCloudRuntimeNode(w http.ResponseWriter, r *http.Request) {
	// Cloud now mints a node-scoped mcn_ PAT itself during /api/v1/nodes
	// and injects it into the EC2 instance via SSM bootstrap (see
	// multica-cloud docs/api/node-pat.md). We no longer forward the
	// caller's mul_ PAT — Fleet doesn't need it, and propagating a
	// long-lived user PAT into a remote machine widened the blast
	// radius of any node compromise. Hence the handler now mirrors
	// the other write endpoints: just the body, no PAT plumbing.
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/nodes", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) DeleteCloudRuntimeNode(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodDelete, "/api/v1/nodes", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) StartCloudRuntimeNode(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/nodes/start", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) StopCloudRuntimeNode(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/nodes/stop", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) RebootCloudRuntimeNode(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/nodes/reboot", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) GetCloudRuntimeNodeStatus(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/nodes/status", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) ExecCloudRuntimeNode(w http.ResponseWriter, r *http.Request) {
	h.proxyCloudRuntime(w, r, http.MethodPost, "/api/v1/nodes/exec", cloudRuntimeProxyOptions{
		withUserID: true,
		withBody:   true,
	})
}

func (h *Handler) proxyCloudRuntime(w http.ResponseWriter, r *http.Request, method, path string, opts cloudRuntimeProxyOptions) {
	client := h.CloudRuntime
	if opts.billing {
		client = h.CloudBilling
	}
	if client == nil || !client.Enabled() {
		writeFeatureDisabled(w, "cloud_runtime_not_configured", "cloud runtime is not configured")
		return
	}

	var headers http.Header
	mutation := !opts.billing && ((path == "/api/v1/nodes" && (method == http.MethodPost || method == http.MethodDelete)) || path == "/api/v1/nodes/start" || path == "/api/v1/nodes/stop" || path == "/api/v1/nodes/reboot")
	if mutation {
		keys := r.Header.Values("Idempotency-Key")
		if h.cfg.LocalFleetURL != "" && (len(keys) != 1 || strings.TrimSpace(keys[0]) == "" || len(keys[0]) > 128) {
			writeError(w, 400, "one Idempotency-Key of at most 128 bytes is required")
			return
		}
		if len(keys) > 0 {
			headers = http.Header{"Idempotency-Key": append([]string(nil), keys...)}
		}
	}
	var userID string
	if opts.withUserID {
		var ok bool
		userID, ok = requireUserID(w, r)
		if !ok {
			return
		}
	}

	var body []byte
	if opts.withBody {
		var ok bool
		body, ok = readCloudRuntimeJSONBody(w, r)
		if !ok {
			return
		}
	}

	if h.cfg.LocalFleetURL != "" && method == http.MethodPost && path == "/api/v1/nodes" && !opts.billing {
		// Installed clients send instance_type; Fleet only accepts the approved spec key.
		var in struct {
			Name         string `json:"name"`
			InstanceType string `json:"instance_type"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil || strings.TrimSpace(in.InstanceType) == "" {
			writeError(w, 400, "local create accepts only name and instance_type")
			return
		}
		body, _ = json.Marshal(struct {
			Name string `json:"name"`
			Spec string `json:"spec"`
		}{in.Name, in.InstanceType})
	}
	if h.cfg.LocalFleetURL != "" && mutation && !(method == http.MethodPost && path == "/api/v1/nodes") {
		action := model.Delete
		switch path {
		case "/api/v1/nodes/start":
			action = model.Start
		case "/api/v1/nodes/stop":
			action = model.Stop
		case "/api/v1/nodes/reboot":
			action = model.Reboot
		}
		h.requestLocalOperation(w, r, userID, body, action, method, path)
		return
	}
	var query url.Values
	if opts.withQuery {
		query = r.URL.Query()
	}

	resp, err := client.Do(r.Context(), cloudruntime.Request{
		Method:    method,
		Path:      path,
		Query:     query,
		Body:      body,
		UserID:    userID,
		RequestID: cloudRuntimeRequestID(r),
		Headers:   headers,
	})
	if err != nil {
		writeCloudRuntimeError(w, r, err)
		return
	}
	writeCloudRuntimeResponse(w, resp)
}

func readCloudRuntimeJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCloudRuntimeRequestBodySize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		writeError(w, http.StatusBadRequest, "request body is required")
		return nil, false
	}
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return nil, false
	}
	return data, true
}

func cloudRuntimeRequestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	return chimw.GetReqID(r.Context())
}

func writeCloudRuntimeResponse(w http.ResponseWriter, resp *cloudruntime.Response) {
	if requestID := resp.Header.Get("X-Request-ID"); requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	body := bytes.TrimSpace(resp.Body)
	if len(body) == 0 {
		w.WriteHeader(resp.StatusCode)
		return
	}
	if json.Valid(body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}
	writeJSON(w, resp.StatusCode, map[string]string{"error": string(body)})
}

func writeCloudRuntimeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, cloudruntime.ErrDisabled):
		writeFeatureDisabled(w, "cloud_runtime_not_configured", "cloud runtime is not configured")
	case errors.Is(err, cloudruntime.ErrInvalidBaseURL):
		writeErrorCode(w, http.StatusInternalServerError, "cloud_runtime_misconfigured", "cloud runtime is misconfigured")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "cloud runtime request timed out")
	default:
		slog.Warn("cloud runtime request failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusBadGateway, "cloud runtime request failed")
	}
}
