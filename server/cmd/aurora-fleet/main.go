// Command aurora-fleet is the self-host Aurora runtime fleet controller.
//
// It exposes the cloudruntime-compatible node API the main server proxies to,
// and provisions sandbox nodes (Docker containers running the Multica daemon)
// on demand. Configuration is environment-driven:
//
//	AURORA_FLEET_ADDR        listen address (default ":8081")
//	AURORA_FLEET_BACKEND     node backend: "docker" or "memory" (default "docker")
//	AURORA_SANDBOX_IMAGE     sandbox image to provision (docker backend)
//	MULTICA_SERVER_URL       main server the sandbox daemon dials (injected into nodes)
//	AURORA_SANDBOX_TOKEN     managed-registration secret (injected into nodes)
package main

import (
	"context"
	"errors"
	"log/slog"
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

	addr := envOr("AURORA_FLEET_ADDR", ":8081")
	ctrl := aurorafleet.NewController(aurorafleet.Config{
		Backend:      backendFromEnv(),
		SandboxImage: os.Getenv("AURORA_SANDBOX_IMAGE"),
		ServerURL:    os.Getenv(aurorafleet.EnvServerURL),
		SandboxToken: os.Getenv(aurorafleet.EnvSandboxToken),
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
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("aurora-fleet shutdown failed", "error", err)
	}
	slog.Info("aurora-fleet stopped")
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
