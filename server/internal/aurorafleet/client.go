package aurorafleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// controlConnectTimeout bounds dialing the fleet controller.
	controlConnectTimeout = 10 * time.Second
	// controlRequestDeadline bounds each fleet control request end to end.
	controlRequestDeadline = 60 * time.Second
	// maxResponseBytes caps how much of a response body is read. The node
	// status payload is tiny; anything near the cap is a hostile or broken
	// peer.
	maxResponseBytes = 1 << 20
)

// EnsureRequest is the ensure body the server sends to the fleet controller.
// It carries identity and the single-use enrollment secret only.
type EnsureRequest struct {
	NodeID          string `json:"node_id"`
	WorkspaceID     string `json:"workspace_id"`
	RuntimeID       string `json:"runtime_id"`
	DaemonID        string `json:"daemon_id"`
	EnrollmentToken string `json:"enrollment_token"`
}

// ControlClient is the server-side typed client for the authenticated fleet
// control API. Its bearer token is read once, at construction, from the
// configured file.
type ControlClient struct {
	baseURL string
	auth    *ControlAuth
	http    *http.Client
}

// NewControlClient builds a client for the fleet controller at baseURL,
// authenticating with the token stored at tokenFilePath. Plain HTTP is
// accepted only for loopback addresses; anything else must use HTTPS.
func NewControlClient(baseURL, tokenFilePath string) (*ControlClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse fleet control URL: %w", err)
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Host) {
		return nil, fmt.Errorf("fleet control URL %s must use HTTPS for non-loopback hosts", baseURL)
	}
	auth, err := LoadControlAuth(tokenFilePath)
	if err != nil {
		return nil, err
	}
	return &ControlClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    auth,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: controlConnectTimeout}).DialContext,
			},
			// CheckRedirect returning the sentinel keeps the first response
			// without following it: credentials must never be replayed to
			// another origin.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// isLoopbackHost reports whether the URL host is a loopback address or the
// loopback hostname.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// EnsureWorkspaceNode provisions or confirms the workspace node.
func (c *ControlClient) EnsureWorkspaceNode(ctx context.Context, req EnsureRequest) (Node, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Node{}, fmt.Errorf("encode ensure request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPut, c.baseURL+"/internal/v1/workspace-nodes/"+req.NodeID, body)
	if err != nil {
		return Node{}, err
	}
	var node Node
	if err := decodeResponse(resp, &node); err != nil {
		return Node{}, err
	}
	return node, nil
}

// WorkspaceNodeStatus returns the node's fleet-reported state.
func (c *ControlClient) WorkspaceNodeStatus(ctx context.Context, nodeID string) (Node, error) {
	resp, err := c.do(ctx, http.MethodGet, c.baseURL+"/internal/v1/workspace-nodes/"+nodeID, nil)
	if err != nil {
		return Node{}, err
	}
	var node Node
	if err := decodeResponse(resp, &node); err != nil {
		return Node{}, err
	}
	return node, nil
}

// DeleteWorkspaceNode destroys the node.
func (c *ControlClient) DeleteWorkspaceNode(ctx context.Context, nodeID string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.baseURL+"/internal/v1/workspace-nodes/"+nodeID, nil)
	if err != nil {
		return err
	}
	defer drainClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	return nil
}

// do performs one fleet control request: it applies the 60s request deadline
// and the bearer token, and returns the response with its body still open.
// The caller must drain or close it to release the deadline.
func (c *ControlClient) do(ctx context.Context, method, rawURL string, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, controlRequestDeadline)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build fleet request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(c.auth.token))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("fleet control request failed: %w", err)
	}
	// The request deadline is released when the body is drained, so every
	// path through the typed methods closes the response.
	resp.Body = cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelReadCloser releases the request deadline when the body is closed.
type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// drainClose discards the remainder of a capped body and closes it.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
}

// readCapped reads the response body, failing when it exceeds the cap.
func readCapped(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read fleet response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("fleet response exceeds %d byte cap", maxResponseBytes)
	}
	return body, nil
}

// decodeResponse decodes a successful JSON response or converts an error
// status into an error carrying only the server's message. The request's
// enrollment secret never appears here.
func decodeResponse(resp *http.Response, dst *Node) error {
	defer drainClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError(resp)
	}
	body, err := readCapped(resp)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode fleet response: %w", err)
	}
	return nil
}

// statusError builds an error from a non-2xx response, echoing only the
// server's status and its own error message.
func statusError(resp *http.Response) error {
	body, _ := readCapped(resp)
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Error != "" {
		return fmt.Errorf("fleet control returned %d: %s", resp.StatusCode, payload.Error)
	}
	return fmt.Errorf("fleet control returned %d", resp.StatusCode)
}
