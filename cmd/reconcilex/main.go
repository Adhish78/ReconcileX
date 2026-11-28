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

type config struct {
	dsn         string
	port        string
	autoMigrate bool
}

func loadConfig(args []string) (config, error) {
	cfg := config{
		dsn:         envOrDefault("RECONCILEX_DSN", "postgres://reconcilex:reconcilex@localhost:5432/reconcilex?sslmode=disable"),
		port:        envOrDefault("RECONCILEX_PORT", "8080"),
		autoMigrate: envBoolOrDefault("RECONCILEX_AUTO_MIGRATE", true),
	}

	fs := flag.NewFlagSet("reconcilex", flag.ContinueOnError)
	fs.StringVar(&cfg.dsn, "dsn", cfg.dsn, "PostgreSQL connection string")
	fs.StringVar(&cfg.port, "port", cfg.port, "HTTP listen port")
	fs.BoolVar(&cfg.autoMigrate, "auto-migrate", cfg.autoMigrate, "Run embedded SQL migrations on startup")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.dsn)
	if err != nil {
		log.Fatalf("create postgres pool: %v", err)
	}
	defer pool.Close()

	if cfg.autoMigrate {
		migCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := db.Migrate(migCtx, pool, migrations.FS)
		cancel()
		if err != nil {
			log.Fatalf("run startup migrations: %v", err)
		}
		log.Printf("database migrations verified")
	}

	webFS, err := web.DistFS()
	if err != nil {
		log.Fatalf("load embedded web assets: %v", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           api.NewServer(pool, migrations.FS, webFS),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("reconcilex listening on :%s", cfg.port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}
}
