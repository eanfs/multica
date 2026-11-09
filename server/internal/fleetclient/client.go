// Package fleetclient provides the HTTP client used for service-to-service
// calls from the API to a Fleet control plane. It attaches the shared service
// secret on every request to the configured fleet origin and never forwards
// that secret to a redirect target on another origin.
package fleetclient

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTimeout = 35 * time.Second

type Config struct {
	// BaseURL is the fleet control plane origin, for example
	// http://127.0.0.1:8091. Only requests to this origin are authenticated.
	BaseURL string
	// Secret is the shared service secret. It is never logged or returned by
	// the client; the caller is responsible for reading it from a private file.
	Secret string
	// Timeout bounds every request. Zero uses defaultTimeout.
	Timeout time.Duration
}

type Client struct {
	http *http.Client
}

// New builds a client whose transport stamps Authorization: Bearer <secret>
// on requests that stay on the configured fleet origin.
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &secretTransport{
			base:   http.DefaultTransport,
			origin: originOf(cfg.BaseURL),
			secret: cfg.Secret,
		},
	}}
}

// HTTPClient returns the underlying client so callers can share it with other
// Fleet-facing transports (the cloud runtime proxy and the mcn_ verifier).
func (c *Client) HTTPClient() *http.Client { return c.http }

type secretTransport struct {
	base   http.RoundTripper
	origin string
	secret string
}

func (t *secretTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Never attach the service secret to a redirect that left the fleet
	// origin. Compare scheme and host only: a path or query never determines
	// who receives the request.
	if t.secret != "" && req.URL.Scheme+"://"+req.URL.Host == t.origin {
		cloned := req.Clone(req.Context())
		cloned.Header.Set("Authorization", "Bearer "+t.secret)
		return t.base.RoundTrip(cloned)
	}
	return t.base.RoundTrip(req)
}

// originOf normalizes a URL to scheme://host, ignoring credentials and any
// path, query, or fragment. An unparsable URL yields an empty origin so it can
// never match a real request.
func originOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
