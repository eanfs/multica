// Command fleet runs the local Cloud Runtime control plane. It manages node
// containers through the Docker Engine API and shares the current checkout's
// PostgreSQL; the API process proxies to it with the service secret.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/fleet"
	dockersdk "github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/logger"
)

const (
	// startupPingTimeout bounds each dependency check. A dependency that does
	// not answer by then fails startup instead of leaving Fleet half-ready.
	startupPingTimeout = 10 * time.Second
	// shutdownTimeout is the graceful HTTP drain budget.
	shutdownTimeout = 5 * time.Second
	// coordinatorInterval matches the design's five-second reconcile cycle.
	coordinatorInterval = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("fleet: process failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logger.Init()

	cfg, secret, err := loadStartupConfig(os.Getenv)
	if err != nil {
		return err
	}

	// The signal context aborts startup pings too, so Ctrl-C during boot does
	// not wait for an unreachable daemon.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := newPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := ping(ctx, "database", pool.Ping); err != nil {
		return err
	}
	st := store.New(pool)

	provider, err := dockersdk.NewSDK(cfg.DockerHost)
	if err != nil {
		return fmt.Errorf("docker provider: %w", err)
	}
	defer func() {
		if err := provider.Close(); err != nil {
			slog.Warn("fleet: docker client close failed", "error", err)
		}
	}()
	if err := ping(ctx, "docker engine", provider.Ping); err != nil {
		return err
	}

	creds, err := fleet.LoadCredentials(cfg.CredentialsFile)
	if err != nil {
		return err
	}

	svc := fleet.NewService(cfg, st, provider, creds, secret)
	server := newHTTPServer(cfg.Listen, fleet.NewHTTP(svc))

	// The coordinator owns no resources beyond the pool and provider, so it is
	// joined before run returns and the deferred closes execute.
	coordinatorCtx, stopCoordinator := context.WithCancel(context.Background())
	defer stopCoordinator()
	coordinatorDone := make(chan struct{})
	go func() {
		defer close(coordinatorDone)
		svc.RunCoordinator(coordinatorCtx, coordinatorInterval)
	}()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("fleet: http listening",
			"addr", cfg.Listen,
			"namespace", cfg.Namespace,
			"specs", len(cfg.Specs),
			"max_nodes_per_owner", cfg.MaxNodesPerOwner,
			"coordinator_interval", coordinatorInterval.String(),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("serve http: %w", err)
		}
	}()

	var runErr error
	select {
	case runErr = <-serveErr:
		slog.Error("fleet: http server stopped", "error", runErr)
	case <-ctx.Done():
		slog.Info("fleet: termination signal received")
	}

	// Drain in a fixed order: stop new coordination work, let in-flight HTTP
	// requests finish, then join the coordinator before the pool and provider
	// are closed by the defers above.
	stopCoordinator()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("fleet: http shutdown incomplete", "error", err)
		_ = server.Close()
	}
	<-coordinatorDone
	slog.Info("fleet: stopped")
	return runErr
}

// loadStartupConfig keeps the service secret out of Config so no caller can
// accidentally log a struct that contains it.
func loadStartupConfig(getenv func(string) string) (fleet.Config, string, error) {
	cfg, err := fleet.Load(getenv)
	if err != nil {
		return fleet.Config{}, "", err
	}
	secret, err := cfg.Secret()
	if err != nil {
		return fleet.Config{}, "", err
	}
	return cfg, secret, nil
}

// newPool parses DATABASE_URL into a lazy pool. pgx embeds the raw connection
// string in parse errors, so the cause stays out of the returned error and out
// of the log.
func newPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, errors.New("connect database: DATABASE_URL is invalid")
	}
	return pool, nil
}

// ping checks one startup dependency with a bounded context. The database and
// Docker daemon are both optional at build time and mandatory at boot, so a
// failure stops the process before it can serve misleading readiness.
func ping(ctx context.Context, name string, check func(context.Context) error) error {
	pingCtx, cancel := context.WithTimeout(ctx, startupPingTimeout)
	defer cancel()
	if err := check(pingCtx); err != nil {
		return fmt.Errorf("ping %s: %w", name, err)
	}
	slog.Info("fleet: dependency ready", "dependency", name)
	return nil
}

// newHTTPServer sets the production timeouts. Header reads are bounded to
// stop Slowloris; request deadlines are not, because node operations are
// already bounded inside the service and the coordinator.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
