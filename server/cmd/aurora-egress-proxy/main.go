// Command aurora-egress-proxy runs the Aurora sandbox egress sidecar: a narrow
// HTTP/CONNECT forward proxy that reaches only the configured Multica server
// origin and the compiled provider hosts. It listens on the workspace-internal
// network and exposes its health probe on a separate loopback port.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/auroraegress"
)

const (
	envServerOrigin = "MULTICA_EGRESS_SERVER_ORIGIN"
	envAllowedHosts = "MULTICA_EGRESS_ALLOWED_HOSTS"
	envListenAddr   = "MULTICA_EGRESS_LISTEN_ADDR"
	envHealthAddr   = "MULTICA_EGRESS_HEALTH_ADDR"

	defaultListenAddr = "0.0.0.0:3128"
	defaultHealthAddr = "127.0.0.1:3129"

	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, logger); err != nil {
		logger.Error("egress sidecar stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	origin := strings.TrimSpace(os.Getenv(envServerOrigin))
	if origin == "" {
		return fmt.Errorf("%s is required", envServerOrigin)
	}

	// NewPolicy rejects wildcard hosts and any entry that is not an exact
	// host:443, so a malformed or over-broad allowlist fails closed at startup.
	policy, err := auroraegress.NewPolicy(origin, splitList(os.Getenv(envAllowedHosts)))
	if err != nil {
		return err
	}

	// The proxy resolves and authorizes every target itself; its Context makes
	// cancellation end every established tunnel.
	proxy := auroraegress.New(auroraegress.Config{Policy: policy, Logger: logger, Context: ctx})
	defer proxy.Close()

	listenAddr := envOrDefault(envListenAddr, defaultListenAddr)
	healthAddr := envOrDefault(envHealthAddr, defaultHealthAddr)

	proxyServer := &http.Server{
		Addr:              listenAddr,
		Handler:           proxy,
		MaxHeaderBytes:    proxy.MaxHeaderBytes(),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthServer := &http.Server{
		Addr:              healthAddr,
		Handler:           healthMux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	failures := make(chan error, 2)
	go func() {
		if serveErr := proxyServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			failures <- fmt.Errorf("proxy listener %s: %w", listenAddr, serveErr)
		}
	}()
	go func() {
		if serveErr := healthServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			failures <- fmt.Errorf("health listener %s: %w", healthAddr, serveErr)
		}
	}()
	logger.Info("egress sidecar listening",
		"listen", listenAddr,
		"health", healthAddr,
		"providers", len(auroraegress.CompiledProviderTLSHosts()),
	)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-failures:
	}

	proxy.Close()
	shutdown(proxyServer, healthServer)
	if runErr != nil {
		return runErr
	}
	logger.Info("egress sidecar stopped")
	return nil
}

// shutdown drains both listeners within the grace period.
func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(ctx)
	}
}

// splitList parses a comma-separated environment list, dropping empty entries.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
