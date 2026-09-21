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
	backend := aurorafleet.NewController(aurorafleet.Config{
		Backend:      backendFromEnv(),
		SandboxImage: os.Getenv("AURORA_SANDBOX_IMAGE"),
		ServerURL:    os.Getenv("MULTICA_SERVER_URL"),
		SandboxToken: os.Getenv("AURORA_SANDBOX_TOKEN"),
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           backend.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("aurora-fleet listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("aurora-fleet server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("aurora-fleet shutdown failed", "error", err)
	}
	slog.Info("aurora-fleet stopped")
}

// backendFromEnv selects the node backend. "memory" is the zero-dependency
// in-process backend for development and tests; "docker" (the default) shells
// out to the docker CLI.
func backendFromEnv() aurorafleet.Backend {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AURORA_FLEET_BACKEND"))) {
	case "memory":
		return aurorafleet.NewMemoryBackend()
	default:
		return aurorafleet.NewDockerBackend(os.Getenv("AURORA_SANDBOX_IMAGE"), nil)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
