package auroraegress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyBoundsHeadersBodiesAndTunnelLifetime(t *testing.T) {
	t.Run("request headers are bounded to 32 KiB", func(t *testing.T) {
		if DefaultMaxRequestHeaderBytes != 32<<10 {
			t.Fatalf("DefaultMaxRequestHeaderBytes = %d, want %d", DefaultMaxRequestHeaderBytes, 32<<10)
		}
		p := mustPolicy(t)
		proxy := New(Config{Policy: p, DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("upstream must not be dialed")
		}})
		req := httptest.NewRequest(http.MethodGet, "http://multica.example.com/anything", nil)
		req.Header.Set("X-Huge", strings.Repeat("a", int(DefaultMaxRequestHeaderBytes)))
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestHeaderFieldsTooLarge)
		}
	})

	t.Run("request bodies are bounded to 8 MiB", func(t *testing.T) {
		if DefaultMaxRequestBodyBytes != 8<<20 {
			t.Fatalf("DefaultMaxRequestBodyBytes = %d, want %d", DefaultMaxRequestBodyBytes, 8<<20)
		}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()
		u, err := url.Parse(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewPolicy("http://localhost:"+u.Port(), nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Resolve = staticResolver(net.ParseIP("127.0.0.1"))
		proxy := New(Config{Policy: p, MaxRequestBodyBytes: 1024})
		srv := httptest.NewServer(proxy)
		defer srv.Close()

		conn := dialProxy(t, srv.Listener.Addr().String())
		body := strings.Repeat("a", 64<<10)
		_, _ = fmt.Fprintf(conn, "POST http://localhost:%s/upload HTTP/1.1\r\nHost: localhost:%s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			u.Port(), u.Port(), len(body), body)
		br := bufio.NewReader(conn)
		if code := readProxyResponse(t, br); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("idle tunnels are closed after the idle timeout", func(t *testing.T) {
		if DefaultIdleTunnelTimeout != 90*time.Second {
			t.Fatalf("DefaultIdleTunnelTimeout = %v, want 90s", DefaultIdleTunnelTimeout)
		}
		p := mustPolicy(t)
		p.Resolve = staticResolver(net.ParseIP("93.184.216.34"))
		release := make(chan struct{})
		proxy := New(Config{
			Policy:              p,
			IdleTunnelTimeout:   150 * time.Millisecond,
			TotalTunnelLifetime: time.Minute,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() { <-release; _ = server.Close() }()
				return client, nil
			},
		})
		srv := httptest.NewServer(proxy)
		defer srv.Close()
		defer close(release)

		conn := dialProxy(t, srv.Listener.Addr().String())
		_, _ = fmt.Fprintf(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
		br := bufio.NewReader(conn)
		if code := readProxyResponse(t, br); code != http.StatusOK {
			t.Fatalf("CONNECT status = %d, want %d", code, http.StatusOK)
		}
		assertTunnelClosed(t, conn, br, "idle")
	})

	t.Run("total tunnel lifetime is bounded to 35 minutes", func(t *testing.T) {
		if DefaultTotalTunnelLifetime != 35*time.Minute {
			t.Fatalf("DefaultTotalTunnelLifetime = %v, want 35m", DefaultTotalTunnelLifetime)
		}
		p := mustPolicy(t)
		p.Resolve = staticResolver(net.ParseIP("93.184.216.34"))
		release := make(chan struct{})
		proxy := New(Config{
			Policy:              p,
			IdleTunnelTimeout:   time.Minute,
			TotalTunnelLifetime: 200 * time.Millisecond,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() { <-release; _ = server.Close() }()
				return client, nil
			},
		})
		srv := httptest.NewServer(proxy)
		defer srv.Close()
		defer close(release)

		conn := dialProxy(t, srv.Listener.Addr().String())
		_, _ = fmt.Fprintf(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
		br := bufio.NewReader(conn)
		if code := readProxyResponse(t, br); code != http.StatusOK {
			t.Fatalf("CONNECT status = %d, want %d", code, http.StatusOK)
		}
		assertTunnelClosed(t, conn, br, "long-lived")
	})

	t.Run("simultaneous tunnels are bounded to 8 per sidecar", func(t *testing.T) {
		if DefaultMaxTunnels != 8 {
			t.Fatalf("DefaultMaxTunnels = %d, want 8", DefaultMaxTunnels)
		}
		p := mustPolicy(t)
		p.Resolve = staticResolver(net.ParseIP("93.184.216.34"))
		release := make(chan struct{})
		proxy := New(Config{
			Policy:              p,
			MaxTunnels:          1,
			IdleTunnelTimeout:   time.Minute,
			TotalTunnelLifetime: time.Minute,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				go func() { <-release; _ = server.Close() }()
				return client, nil
			},
		})
		srv := httptest.NewServer(proxy)
		defer srv.Close()
		defer close(release)

		first := dialProxy(t, srv.Listener.Addr().String())
		_, _ = fmt.Fprintf(first, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
		if code := readProxyResponse(t, bufio.NewReader(first)); code != http.StatusOK {
			t.Fatalf("first CONNECT status = %d, want %d", code, http.StatusOK)
		}

		second := dialProxy(t, srv.Listener.Addr().String())
		_, _ = fmt.Fprintf(second, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
		if code := readProxyResponse(t, bufio.NewReader(second)); code != http.StatusServiceUnavailable {
			t.Fatalf("second CONNECT status = %d, want %d", code, http.StatusServiceUnavailable)
		}
	})
}
