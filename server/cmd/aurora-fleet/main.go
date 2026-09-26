// Command aurora-fleet is the Aurora workspace sandbox fleet controller.
//
// It exposes the authenticated internal workspace-node API the main server
// calls to ensure, inspect, and delete per-workspace sandbox nodes.
// Configuration is environment-driven:
//
//	AURORA_FLEET_ADDR                  listen address (default ":8081")
//	AURORA_FLEET_BACKEND               node backend: "docker" or "memory" (default "docker")
//	AURORA_FLEET_CONTROL_TOKEN_FILE    file holding the fleet control bearer token (required)
//	AURORA_FLEET_SECRET_ROOT           root directory for staged enrollment secrets (required)
//
// The control token file must be a regular file with mode 0400 or 0600
// holding at least 32 random bytes as base64url or hex. The token is never
// accepted from the environment and never logged.
//
// A non-loopback listen address is a startup error: the fleet API is an
// internal control surface, and remote binds require the TLS configuration
// that arrives with the isolation hardening.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/aurorafleet"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("aurora-fleet failed", "error", err)
		os.Exit(1)
	}
	slog.Info("aurora-fleet stopped")
}

func run() error {
	addr := envOr("AURORA_FLEET_ADDR", ":8081")
	if host, _, err := net.SplitHostPort(addr); err == nil && host != "" && !isLoopbackHostMain(host) {
		return fmt.Errorf("listen address %s is not loopback: the fleet API requires a TLS-terminated remote bind, which is not supported yet", addr)
	}

	tokenFile := strings.TrimSpace(os.Getenv("AURORA_FLEET_CONTROL_TOKEN_FILE"))
	if tokenFile == "" {
		return errors.New("AURORA_FLEET_CONTROL_TOKEN_FILE is required")
	}
	secretRoot := strings.TrimSpace(os.Getenv("AURORA_FLEET_SECRET_ROOT"))
	if secretRoot == "" {
		return errors.New("AURORA_FLEET_SECRET_ROOT is required")
	}
	auth, err := aurorafleet.LoadControlAuth(tokenFile)
	if err != nil {
		return fmt.Errorf("load fleet control token: %w", err)
	}

	ctrl := aurorafleet.NewController(aurorafleet.Config{
		Backend:    backendFromEnv(),
		Auth:       auth,
		SecretRoot: secretRoot,
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           ctrl.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("aurora-fleet listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("aurora-fleet server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("aurora-fleet shutdown failed", "error", err)
	}
	return nil
}

// backendFromEnv selects the node backend. "memory" is the zero-dependency
// in-process backend for development and tests; "docker" (the default) shells
// out to the docker CLI.
func backendFromEnv() aurorafleet.Backend {
	if strings.ToLower(strings.TrimSpace(os.Getenv("AURORA_FLEET_BACKEND"))) == "memory" {
		return aurorafleet.NewMemoryBackend()
	}
	return aurorafleet.NewDockerBackend()
}

func isLoopbackHostMain(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
