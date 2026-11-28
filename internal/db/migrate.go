// Package db provides embedded SQL schema migration execution and database
// readiness verification backed by PostgreSQL.
//
// Callers:
//   - cmd/reconcilex runs Migrate on startup when RECONCILEX_AUTO_MIGRATE=true.
//   - internal/api calls MigrationsReady inside GET /readyz to confirm both
//     PostgreSQL connectivity and schema completeness.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is a fixed arbitrary 64-bit key passed to PostgreSQL's
// pg_advisory_xact_lock so multiple ReconcileX instances booting at the same
// time serialize their migration checks rather than racing on DDL statements.
const migrationLockID int64 = 74281901

// -----------------------------------------------------------------------------
// Migration execution
// -----------------------------------------------------------------------------

// Migrate applies every unapplied *.sql migration from migrationsFS in lexical order.
// All schema changes and tracking inserts run inside one transaction guarded by a
// PostgreSQL advisory lock, making repeated calls safe and idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrationsFS fs.FS) error {
	// Discover and lexicographically sort all *.sql files (0001_..., 0002_..., etc.)
	// so migrations always execute in deterministic numeric-prefix order.
	files, err := sortedMigrationFiles(migrationsFS)
	if err != nil {
		return err
	}

	// Open a single transaction wrapping the entire migration pass; if any file fails,
	// everything rolls back cleanly so the schema is never left half-migrated.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}

	// Guarantee rollback on any early error return. Once Commit() succeeds below,
	// Rollback returns pgx.ErrTxClosed, which is intentionally discarded.
	defer func() { _ = tx.Rollback(ctx) }()

	// Acquire a transaction-scoped advisory lock; concurrent callers block here
	// until this transaction commits or rolls back, then re-read schema_migrations.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}

	// Bootstrap the migration ledger table on first boot before querying applied versions.
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	// Read the set of migration filenames that have already been applied in earlier runs.
	applied, err := fetchAppliedVersions(ctx, tx)
	if err != nil {
		return err
	}

	// Step through each embedded SQL file, skipping already-applied ones and executing
	// + recording each new migration inside the same atomic transaction.
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

	// Commit all DDL changes and version records, which automatically releases migrationLockID.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}

	return nil
}

// -----------------------------------------------------------------------------
// Readiness probe helper
// -----------------------------------------------------------------------------

// MigrationsReady reports whether PostgreSQL is reachable, schema_migrations exists,
// and every embedded migration file in migrationsFS has already been recorded.
// Returns (false, nil) when the DB is reachable but migrations are still pending.
func MigrationsReady(ctx context.Context, pool *pgxpool.Pool, migrationsFS fs.FS) (bool, error) {
	// Fail fast with an explicit error if the caller has no initialized DB pool.
	if pool == nil {
		return false, fmt.Errorf("nil database pool")
	}

	// Verify active TCP/TLS/auth connectivity to PostgreSQL before inspecting tables.
	if err := pool.Ping(ctx); err != nil {
		return false, fmt.Errorf("ping database: %w", err)
	}

	// Collect all migration filenames expected by this binary build.
	files, err := sortedMigrationFiles(migrationsFS)
	if err != nil {
		return false, err
	}

	// Use PostgreSQL's to_regclass() to check whether schema_migrations exists in the
	// current search_path without throwing a SQL relation-does-not-exist error.
	var tableName *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations')::text`).Scan(&tableName); err != nil {
		return false, fmt.Errorf("check schema_migrations: %w", err)
	}

	// NULL means schema_migrations has not been created yet, so the DB is not ready.
	if tableName == nil {
		return false, nil
	}

	// Compare applied versions in the database against every file in migrationsFS;
	// if any file is missing, report unready so traffic waits until migration finishes.
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

// -----------------------------------------------------------------------------
// Internal query & filesystem helpers
// -----------------------------------------------------------------------------

// versionQuerier abstracts over *pgxpool.Pool and pgx.Tx so fetchAppliedVersions
// can query schema_migrations either inside Migrate's transaction or directly on the pool.
type versionQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// fetchAppliedVersions queries schema_migrations and builds a lookup map of
// migration filenames that have already been executed.
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

// sortedMigrationFiles finds all top-level *.sql files in migrationsFS and sorts them
// lexicographically so numeric prefixes (0001_..., 0002_...) dictate execution order.
func sortedMigrationFiles(migrationsFS fs.FS) ([]string, error) {
	files, err := fs.Glob(migrationsFS, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}

	sort.Strings(files)
	return files, nil
}
