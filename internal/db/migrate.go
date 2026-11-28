package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fixed PostgreSQL advisory lock key to serialize concurrent startup migrations.
const migrationLockID int64 = 74281901

// Migrate applies all unapplied SQL migrations from migrationsFS in lexical order.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrationsFS fs.FS) error {
	files, err := sortedMigrationFiles(migrationsFS)
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	applied, err := fetchAppliedVersions(ctx, tx)
	if err != nil {
		return err
	}

	for _, name := range files {
		if applied[name] {
			continue
		}
		sqlBytes, err := fs.ReadFile(migrationsFS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("execute migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

// MigrationsReady reports whether the database is reachable and all embedded migrations are applied.
func MigrationsReady(ctx context.Context, pool *pgxpool.Pool, migrationsFS fs.FS) (bool, error) {
	if pool == nil {
		return false, fmt.Errorf("nil database pool")
	}
	if err := pool.Ping(ctx); err != nil {
		return false, fmt.Errorf("ping database: %w", err)
	}

	files, err := sortedMigrationFiles(migrationsFS)
	if err != nil {
		return false, err
	}

	var tableName *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations')::text`).Scan(&tableName); err != nil {
		return false, fmt.Errorf("check schema_migrations: %w", err)
	}
	if tableName == nil {
		return false, nil
	}

	applied, err := fetchAppliedVersions(ctx, pool)
	if err != nil {
		return false, err
	}
	for _, name := range files {
		if !applied[name] {
			return false, nil
		}
	}
	return true, nil
}

type versionQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func fetchAppliedVersions(ctx context.Context, q versionQuerier) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan migration version: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration versions: %w", err)
	}
	return applied, nil
}

func sortedMigrationFiles(migrationsFS fs.FS) ([]string, error) {
	files, err := fs.Glob(migrationsFS, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(files)
	return files, nil
}
