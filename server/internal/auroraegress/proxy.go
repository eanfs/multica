package auroraegress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Documented egress bounds. Every one is a process constant, never request
// input, so a client cannot negotiate a larger budget.
const (
	// DefaultMaxRequestHeaderBytes caps forwarded request headers.
	DefaultMaxRequestHeaderBytes int64 = 32 << 10
	// DefaultMaxRequestBodyBytes caps a forwarded HTTP request body.
	DefaultMaxRequestBodyBytes int64 = 8 << 20
	// DefaultIdleTunnelTimeout closes a tunnel with no traffic.
	DefaultIdleTunnelTimeout = 90 * time.Second
	// DefaultTotalTunnelLifetime closes a tunnel regardless of traffic.
	DefaultTotalTunnelLifetime = 35 * time.Minute
	// DefaultDialTimeout bounds connect and dial operations.
	DefaultDialTimeout = 10 * time.Second
	// DefaultResponseHeaderTimeout bounds the wait for upstream headers.
	DefaultResponseHeaderTimeout = 30 * time.Second
	// DefaultMaxTunnels bounds simultaneous CONNECT tunnels per sidecar.
	DefaultMaxTunnels = 8
)

// DialContextFunc dials a resolved upstream address. Tests inject fakes; the
// production default is a net.Dialer bounded by DefaultDialTimeout.
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config configures a Proxy. The zero value is unusable: a Proxy without a
// Policy denies every target. Limit fields default to the documented bounds.
type Config struct {
	Policy      Policy
	DialContext DialContextFunc
	Logger      *slog.Logger
	// Context, when set, closes every active tunnel on cancellation.
	Context context.Context

	MaxRequestHeaderBytes int64
	MaxRequestBodyBytes   int64
	IdleTunnelTimeout     time.Duration
	TotalTunnelLifetime   time.Duration
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	MaxTunnels            int
}

// Proxy is the bounded HTTP and CONNECT forward proxy. It performs the DNS
// resolution and public-address check through its Policy, fixes redirects (it
// never follows them), strips proxy/forwarding identity headers, and logs only
// target host/port, decision, byte counts, duration, and a connection ID.
type Proxy struct {
	policy Policy
	logger *slog.Logger

	dialContext DialContextFunc
	transport   *http.Transport

	maxRequestHeaderBytes int64
	maxRequestBodyBytes   int64
	idleTunnelTimeout     time.Duration
	totalTunnelLifetime   time.Duration
	dialTimeout           time.Duration

	slots chan struct{}

	mu     sync.Mutex
	active map[*activeTunnel]struct{}
}

// activeTunnel tracks one established CONNECT tunnel so Close can end it.
type activeTunnel struct {
	client   net.Conn
	upstream net.Conn
}

// New builds a Proxy, applying the documented defaults for unset limits.
func New(cfg Config) *Proxy {
	p := &Proxy{
		policy:                cfg.Policy,
		logger:                cfg.Logger,
		maxRequestHeaderBytes: cfg.MaxRequestHeaderBytes,
		maxRequestBodyBytes:   cfg.MaxRequestBodyBytes,
		idleTunnelTimeout:     cfg.IdleTunnelTimeout,
		totalTunnelLifetime:   cfg.TotalTunnelLifetime,
		dialTimeout:           cfg.DialTimeout,
		active:                make(map[*activeTunnel]struct{}),
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.maxRequestHeaderBytes <= 0 {
		p.maxRequestHeaderBytes = DefaultMaxRequestHeaderBytes
	}
	if p.maxRequestBodyBytes <= 0 {
		p.maxRequestBodyBytes = DefaultMaxRequestBodyBytes
	}
	if p.idleTunnelTimeout <= 0 {
		p.idleTunnelTimeout = DefaultIdleTunnelTimeout
	}
	if p.totalTunnelLifetime <= 0 {
		p.totalTunnelLifetime = DefaultTotalTunnelLifetime
	}
	if p.dialTimeout <= 0 {
		p.dialTimeout = DefaultDialTimeout
	}

	dial := cfg.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: p.dialTimeout}
		dial = d.DialContext
	}
	p.dialContext = dial

	responseHeaderTimeout := cfg.ResponseHeaderTimeout
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	p.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ResponseHeaderTimeout: responseHeaderTimeout,
		TLSHandshakeTimeout:   p.dialTimeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
	}

	maxTunnels := cfg.MaxTunnels
	if maxTunnels <= 0 {
		maxTunnels = DefaultMaxTunnels
	}
	p.slots = make(chan struct{}, maxTunnels)

	if cfg.Context != nil {
		go func() {
			<-cfg.Context.Done()
			p.Close()
		}()
	}
	return p
}

// MaxHeaderBytes exposes the request header bound so the sidecar command can
// set http.Server.MaxHeaderBytes to the same value.
func (p *Proxy) MaxHeaderBytes() int { return int(p.maxRequestHeaderBytes) }

// Close ends every active tunnel. It is idempotent.
func (p *Proxy) Close() {
	p.mu.Lock()
	tunnels := make([]*activeTunnel, 0, len(p.active))
	for t := range p.active {
		tunnels = append(tunnels, t)
	}
	p.mu.Unlock()
	for _, t := range tunnels {
		_ = t.client.Close()
		_ = t.upstream.Close()
	}
}

// ServeHTTP dispatches CONNECT tunnels and forward HTTP requests.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

// handleHTTP forwards one bounded plain-HTTP request. Only the exact server
// origin is eligible; provider endpoints require CONNECT.
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	id := newConnectionID()
	start := time.Now()
	target := r.URL
	if target == nil || !target.IsAbs() || target.Host == "" {
		p.log(id, "denied", "", "", 0, 0, start, "absolute_form_required")
		http.Error(w, "proxy requires an absolute-form request URI", http.StatusBadRequest)
		return
	}
	host, port := target.Hostname(), effectivePort(target)
	if headerBytes(r) > p.maxRequestHeaderBytes {
		p.log(id, "denied", host, port, 0, 0, start, "headers_too_large")
		w.WriteHeader(http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	out, err := p.policy.Validate(r.Context(), target, false)
	if err != nil {
		p.log(id, "denied", host, port, 0, 0, start, "target_refused")
		http.Error(w, "egress target refused", http.StatusForbidden)
		return
	}

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	outReq.URL = cloneURL(target)
	outReq.URL.Host = out.Host
	outReq.Host = target.Host
	outReq.Header = r.Header.Clone()
	stripProxyHeaders(outReq.Header)
	outReq.Body = &cappedReader{r: r.Body, remaining: p.maxRequestBodyBytes}
	outReq.ContentLength = r.ContentLength
	outReq.GetBody = nil

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		status := http.StatusBadGateway
		reason := "upstream_error"
		if errors.Is(err, errRequestBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
			reason = "body_too_large"
		}
		p.log(id, "denied", host, port, 0, 0, start, reason)
		http.Error(w, "egress upstream error", status)
		return
	}
	defer resp.Body.Close()

	header := w.Header()
	copyHeaders(header, resp.Header)
	w.WriteHeader(resp.StatusCode)
	written, _ := io.Copy(w, resp.Body)
	p.log(id, "allowed", host, port, 0, written, start, "")
}

// handleConnect establishes one bounded raw tunnel to an authorized target.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	id := newConnectionID()
	start := time.Now()
	target := &url.URL{Scheme: "https", Host: r.Host}
	host, port := target.Hostname(), target.Port()

	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		p.log(id, "denied", host, port, 0, 0, start, "too_many_tunnels")
		http.Error(w, "too many concurrent tunnels", http.StatusServiceUnavailable)
		return
	}

	out, err := p.policy.Validate(r.Context(), target, true)
	if err != nil {
		p.log(id, "denied", host, port, 0, 0, start, "target_refused")
		http.Error(w, "egress target refused", http.StatusForbidden)
		return
	}
	upstream, err := p.dialTarget(r.Context(), out)
	if err != nil {
		p.log(id, "denied", host, port, 0, 0, start, "dial_failed")
		http.Error(w, "egress upstream unavailable", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}

	in, written := p.tunnel(client, upstream)
	p.log(id, "allowed", host, port, in, written, start, "")
}

// dialTarget dials the pinned addresses for a provider target, or the exact
// host name for the server-origin exception. It never performs a second DNS
// lookup for a provider target.
func (p *Proxy) dialTarget(ctx context.Context, out Target) (net.Conn, error) {
	if len(out.Addrs) == 0 {
		return p.dialContext(ctx, "tcp", out.Host)
	}
	_, port, err := net.SplitHostPort(out.Host)
	if err != nil {
		return nil, fmt.Errorf("pin target %q: %w", out.Host, err)
	}
	var lastErr error
	for _, addr := range out.Addrs {
		conn, err := p.dialContext(ctx, "tcp", net.JoinHostPort(addr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no resolved addresses")
	}
	return nil, lastErr
}

// tunnel copies bytes both ways until either side ends, the idle timeout, or
// the total lifetime. It returns bytes client->upstream and upstream->client.
func (p *Proxy) tunnel(client, upstream net.Conn) (int64, int64) {
	t := &activeTunnel{client: client, upstream: upstream}
	p.mu.Lock()
	p.active[t] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.active, t)
		p.mu.Unlock()
		_ = client.Close()
		_ = upstream.Close()
	}()

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}

	start := time.Now()
	var wg sync.WaitGroup
	var toUpstream, toClient int64
	wg.Add(2)
	go func() {
		defer wg.Done()
		toUpstream, _ = p.copyDirection(upstream, client, start)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		toClient, _ = p.copyDirection(client, upstream, start)
		closeBoth()
	}()
	wg.Wait()
	return toUpstream, toClient
}

// copyDirection copies src into dst, refreshing the idle deadline on every
// read and never reading past the tunnel's absolute lifetime.
func (p *Proxy) copyDirection(dst, src net.Conn, start time.Time) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		deadline := time.Now().Add(p.idleTunnelTimeout)
		if hard := start.Add(p.totalTunnelLifetime); hard.Before(deadline) {
			deadline = hard
		}
		if err := src.SetReadDeadline(deadline); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// log emits one structured decision record. It deliberately carries no URL,
// path, query, header value, or user identity.
func (p *Proxy) log(id, decision, host, port string, bytesIn, bytesOut int64, start time.Time, reason string) {
	attrs := []any{
		"connection_id", id,
		"decision", decision,
		"host", host,
		"port", port,
		"bytes_in", bytesIn,
		"bytes_out", bytesOut,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	p.logger.Info("egress", attrs...)
}

// errRequestBodyTooLarge is returned by cappedReader when a request body
// exceeds the configured bound.
var errRequestBodyTooLarge = errors.New("egress: request body too large")

// cappedReader enforces a hard read cap while allowing the caller one byte of
// slack to detect an over-limit body rather than treating an exact-size body
// as too large.
type cappedReader struct {
	r         io.ReadCloser
	remaining int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		return 0, errRequestBodyTooLarge
	}
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.remaining {
		n = int(c.remaining)
		c.remaining = 0
		return n, errRequestBodyTooLarge
	}
	c.remaining -= int64(n)
	return n, err
}

func (c *cappedReader) Close() error { return c.r.Close() }

// hopByHopHeaders are stripped in both directions, along with any header named
// by Connection.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	"Forwarded",
}

// stripProxyHeaders removes proxy authentication, forwarding identity, and
// hop-by-hop headers.
func stripProxyHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			h.Del(name)
		}
	}
}

// copyHeaders copies upstream response headers, dropping hop-by-hop headers.
func copyHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
	stripProxyHeaders(dst)
}

// headerBytes approximates the wire size of a request's start line and headers
// so the handler can enforce a bound even on a server configured with a larger
// MaxHeaderBytes.
func headerBytes(r *http.Request) int64 {
	total := int64(len(r.Method)) + int64(len(r.RequestURI)) + int64(len(r.Proto)) + 4
	for name, values := range r.Header {
		for _, value := range values {
			total += int64(len(name)) + int64(len(value)) + 4
		}
	}
	return total
}

func cloneURL(u *url.URL) *url.URL {
	clone := *u
	return &clone
}

var connectionCounter uint64

// newConnectionID returns a random, opaque per-connection identifier.
func newConnectionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("c%016x", atomic.AddUint64(&connectionCounter, 1))
	}
	return hex.EncodeToString(b[:])
}
