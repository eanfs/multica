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

// VerifyTimeout bounds one mcn_ verification call. Auth must fail fast, so the
// verifier client shares the fleet transport but not the node proxy's budget.
const VerifyTimeout = 5 * time.Second

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
	origin := originOf(cfg.BaseURL)
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &secretTransport{
			base:   http.DefaultTransport,
			origin: origin,
			secret: cfg.Secret,
		},
		// Follow redirects only when they stay on the fleet origin. A
		// cross-origin 307/308 would otherwise replay the request body (and,
		// without this guard, any body-derived state) at a host the operator
		// never configured; returning ErrUseLastResponse surfaces the redirect
		// itself instead of silently reproducing the call elsewhere.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if origin != "" && req.URL.Scheme+"://"+req.URL.Host != origin {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}}
}

// HTTPClient returns the underlying client so callers can share it with other
// Fleet-facing transports (the cloud runtime proxy and the mcn_ verifier).
func (c *Client) HTTPClient() *http.Client { return c.http }

// VerifyHTTPClient returns a client that reuses the fleet transport (and its
// connection pool) but caps every request at VerifyTimeout. The mcn_ verifier
// uses it so a hung control plane cannot hold an auth request for the full
// node-proxy budget.
func (c *Client) VerifyHTTPClient() *http.Client {
	return &http.Client{
		Transport:     c.http.Transport,
		Timeout:       VerifyTimeout,
		CheckRedirect: c.http.CheckRedirect,
	}
}

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
