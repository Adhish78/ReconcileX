// Command reconcilex is the single-binary runtime entrypoint for ReconcileX.
//
// Startup sequence:
//   1. Resolve runtime configuration from environment variables and CLI flags.
//   2. Register OS signal handling (SIGINT/SIGTERM) for graceful shutdown.
//   3. Connect to PostgreSQL via pgxpool and optionally run embedded SQL migrations.
//   4. Unwrap the compiled Vite/React UI bundle from web.DistFS().
//   5. Start the HTTP server (api.NewServer) and drain active requests on exit.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Adhish78/ReconcileX/internal/api"
	"github.com/Adhish78/ReconcileX/internal/db"
	"github.com/Adhish78/ReconcileX/migrations"
	"github.com/Adhish78/ReconcileX/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

// -----------------------------------------------------------------------------
// Configuration loading
// -----------------------------------------------------------------------------

// config holds runtime settings resolved from environment variables (with safe
// local defaults) and overridden by optional CLI flags.
type config struct {
	dsn         string // PostgreSQL connection string (RECONCILEX_DSN / -dsn).
	port        string // TCP port the HTTP server binds to (RECONCILEX_PORT / -port).
	autoMigrate bool   // Controls whether embedded SQL migrations run at startup (RECONCILEX_AUTO_MIGRATE / -auto-migrate).
}

// loadConfig resolves configuration in two layers: environment variable defaults first,
// followed by CLI flag parsing so explicit flags always take precedence.
func loadConfig(args []string) (config, error) {
	cfg := config{
		dsn:         envOrDefault("RECONCILEX_DSN", "postgres://reconcilex:reconcilex@localhost:5432/reconcilex?sslmode=disable"),
		port:        envOrDefault("RECONCILEX_PORT", "8080"),
		autoMigrate: envBoolOrDefault("RECONCILEX_AUTO_MIGRATE", true),
	}

	// Build an isolated FlagSet instead of mutating the global flag.CommandLine
	// so loadConfig remains deterministic and easy to unit test.
	fs := flag.NewFlagSet("reconcilex", flag.ContinueOnError)
	fs.StringVar(&cfg.dsn, "dsn", cfg.dsn, "PostgreSQL connection string")
	fs.StringVar(&cfg.port, "port", cfg.port, "HTTP listen port")
	fs.BoolVar(&cfg.autoMigrate, "auto-migrate", cfg.autoMigrate, "Run embedded SQL migrations on startup")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	return cfg, nil
}

// envOrDefault reads key from the process environment and returns fallback when unset or empty.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

// envBoolOrDefault parses a boolean environment variable ("true", "1", "false", etc.),
// returning fallback if the variable is missing or cannot be parsed as a bool.
func envBoolOrDefault(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}

	return parsed
}

// -----------------------------------------------------------------------------
// Application lifecycle
// -----------------------------------------------------------------------------

func main() {
	// Step 1: Parse environment variables and CLI arguments before allocating resources.
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	// Step 2: Tie the root context to SIGINT/SIGTERM so container stops trigger graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Step 3: Initialize the PostgreSQL connection pool; pgxpool validates the DSN now
	// and manages concurrent connections lazily across handlers.
	pool, err := pgxpool.New(ctx, cfg.dsn)
	if err != nil {
		log.Fatalf("create postgres pool: %v", err)
	}
	defer pool.Close()

	// Step 4: When auto-migration is enabled (default in compose.yaml), apply any pending
	// embedded SQL migrations inside a bounded 30s context before accepting traffic.
	if cfg.autoMigrate {
		migCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := db.Migrate(migCtx, pool, migrations.FS)
		cancel()

		if err != nil {
			log.Fatalf("run startup migrations: %v", err)
		}

		log.Printf("database migrations verified")
	}

	// Step 5: Load the compiled Vite/React SPA bundle embedded at build time in web/dist.
	webFS, err := web.DistFS()
	if err != nil {
		log.Fatalf("load embedded web assets: %v", err)
	}

	// Step 6: Wire the HTTP server with ReadHeaderTimeout to mitigate slowloris header attacks.
	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           api.NewServer(pool, migrations.FS, webFS),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Run ListenAndServe in a background goroutine so main can select concurrently on
	// both OS termination signals and unexpected server startup/bind errors.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("reconcilex listening on :%s", cfg.port)
		errCh <- srv.ListenAndServe()
	}()

	// Step 7: Block until either a shutdown signal arrives or ListenAndServe returns.
	select {
	case <-ctx.Done():
		// Allow in-flight HTTP requests up to 10 seconds to finish cleanly before exit.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}

	case err := <-errCh:
		// http.ErrServerClosed is expected when Shutdown() closes the listener; only fail on real errors.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}
}
