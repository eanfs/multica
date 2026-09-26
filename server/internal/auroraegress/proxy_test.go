package auroraegress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe buffer for captured structured logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dialProxy opens a raw connection to the proxy under test.
func dialProxy(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readProxyResponse reads a status line plus headers and returns the code.
func readProxyResponse(t *testing.T, br *bufio.Reader) int {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(fields) < 2 {
		t.Fatalf("malformed status line %q", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("malformed status code in %q", line)
	}
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read response headers: %v", err)
		}
		if header == "\r\n" || header == "\n" {
			break
		}
	}
	return code
}

// assertTunnelClosed reads from an established tunnel and requires the proxy
// to close it rather than blocking forever.
func assertTunnelClosed(t *testing.T, conn net.Conn, br *bufio.Reader, kind string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	start := time.Now()
	_, err := br.ReadByte()
	if err == nil {
		t.Fatalf("%s tunnel delivered an unexpected byte", kind)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("%s tunnel was not closed within the bound (waited %v)", kind, time.Since(start))
	}
}

func TestProxyPinsValidatedDNSAddressesForDial(t *testing.T) {
	var mu sync.Mutex
	resolveCount := 0
	dialed := []string{}

	p := mustPolicy(t)
	p.Resolve = func(context.Context, string) ([]net.IPAddr, error) {
		mu.Lock()
		resolveCount++
		mu.Unlock()
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	proxy := New(Config{
		Policy: p,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				_, _ = io.Copy(server, server)
			}()
			return client, nil
		},
	})
	srv := httptest.NewServer(proxy)
	defer srv.Close()

	conn := dialProxy(t, srv.Listener.Addr().String())
	if _, err := fmt.Fprintf(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	if code := readProxyResponse(t, br); code != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want %d", code, http.StatusOK)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel payload: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read tunnel echo: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("tunnel echo = %q, want %q", echo, "ping")
	}

	mu.Lock()
	gotDialed := append([]string(nil), dialed...)
	gotResolves := resolveCount
	mu.Unlock()
	if gotResolves != 1 {
		t.Errorf("resolver called %d times, want exactly 1", gotResolves)
	}
	if len(gotDialed) != 1 || gotDialed[0] != "93.184.216.34:443" {
		t.Errorf("dialer called with %v, want [93.184.216.34:443]", gotDialed)
	}
}

func TestProxyLogsHostWithoutPathQueryOrAuthorization(t *testing.T) {
	type upstreamRequest struct {
		path      string
		query     string
		auth      string
		proxyAuth string
	}
	seen := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- upstreamRequest{
			path:      r.URL.Path,
			query:     r.URL.RawQuery,
			auth:      r.Header.Get("Authorization"),
			proxyAuth: r.Header.Get("Proxy-Authorization"),
		}
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
	var logs syncBuffer
	proxy := New(Config{Policy: p, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	srv := httptest.NewServer(proxy)
	defer srv.Close()

	const secretPath = "/super-secret-path"
	const secretQuery = "token=abc"
	const proxyCredential = "supersecrettoken"
	const clientCredential = "secondsecret"
	conn := dialProxy(t, srv.Listener.Addr().String())
	_, _ = fmt.Fprintf(conn,
		"GET http://localhost:%s%s?%s HTTP/1.1\r\nHost: localhost:%s\r\n"+
			"Proxy-Authorization: Bearer %s\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n",
		u.Port(), secretPath, secretQuery, u.Port(), proxyCredential, clientCredential)
	br := bufio.NewReader(conn)
	if code := readProxyResponse(t, br); code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}

	got := <-seen
	if got.path != secretPath || got.query != secretQuery {
		t.Errorf("upstream saw path=%q query=%q, want the original target", got.path, got.query)
	}
	if got.auth != "Bearer "+clientCredential {
		t.Errorf("upstream Authorization = %q, want the client credential forwarded", got.auth)
	}
	if got.proxyAuth != "" {
		t.Errorf("Proxy-Authorization leaked upstream: %q", got.proxyAuth)
	}

	line := logs.String()
	for _, secret := range []string{secretPath, secretQuery, proxyCredential, clientCredential} {
		if strings.Contains(line, secret) {
			t.Errorf("logs contain %q: %s", secret, line)
		}
	}
	for _, field := range []string{"\"decision\"", "\"host\"", "\"port\"", "\"connection_id\"", "localhost"} {
		if !strings.Contains(line, field) {
			t.Errorf("logs are missing %s: %s", field, line)
		}
	}
}
