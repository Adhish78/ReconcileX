// Package db_test runs integration tests against a real PostgreSQL instance
// (the ephemeral tmpfs postgres-test service in compose.yaml) inside an isolated
// `test_db` schema so test runs never touch live runtime data or other packages' schemas.
package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Adhish78/ReconcileX/internal/db"
	"github.com/Adhish78/ReconcileX/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDSN resolves the PostgreSQL connection string for DB tests from RECONCILEX_TEST_DSN,
// falling back to the local compose postgres-test instance when run outside Docker.
func testDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("RECONCILEX_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://reconcilex:reconcilex@localhost:5433/reconcilex_test?sslmode=disable"
	}

	return dsn
}

// openCleanPool connects to the test database and resets a dedicated "test_db" schema.
// Using a package-specific schema isolates internal/db tests from internal/api tests
// (`test_api`) even when `go test ./...` executes both packages concurrently.
func openCleanPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}

	// Pin every pooled connection's search_path to test_db so DDL and queries stay isolated.
	cfg.ConnConfig.RuntimeParams["search_path"] = "test_db"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)

	// Drop and recreate test_db so every test starts with zero tables, enums, or rows.
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS test_db CASCADE; CREATE SCHEMA test_db;`); err != nil {
		t.Fatalf("reset test_db schema: %v", err)
	}

	return pool
}

// TestMigrateAppliesInitialSchemaIdempotently verifies the full migration lifecycle:
// an empty schema reports unready, Migrate applies 0001 cleanly, a second Migrate
// is a safe no-op, and MigrationsReady transitions to true afterward.
func TestMigrateAppliesInitialSchemaIdempotently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openCleanPool(t)

	// Step 1: Before any migration runs, MigrationsReady must report false because
	// schema_migrations does not yet exist in test_db.
	ready, err := db.MigrationsReady(ctx, pool, migrations.FS)
	if err == nil && ready {
		t.Fatalf("expected unmigrated database not to be ready")
	}

	// Step 2: First migration pass creates schema_migrations and executes 0001_users_and_identities.sql.
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	// Step 3: Second pass must skip already-applied versions without failing on existing DDL.
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	// Step 4: Readiness check must now confirm every embedded migration is applied.
	ready, err = db.MigrationsReady(ctx, pool, migrations.FS)
	if err != nil || !ready {
		t.Fatalf("expected migrations to be ready, got ready=%v err=%v", ready, err)
	}
}

// TestUsersAndLinkedIdentitiesConstraints exercises every database-level invariant
// in 0001_users_and_identities.sql: ACTIVE/MERGED check rules, self-merge prevention,
// and unique (identity_type, identity_hash) enforcement.
func TestUsersAndLinkedIdentitiesConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openCleanPool(t)

	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Case 1: Valid ACTIVE Canonical User without merged_into_user_id inserts cleanly.
	var activeID string
	err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('aarav', 'Aarav Sharma', 'ACTIVE') RETURNING id`,
	).Scan(&activeID)
	if err != nil {
		t.Fatalf("insert active user: %v", err)
	}

	// Case 2: ACTIVE user with merged_into_user_id populated violates users_status_merge_check.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status, merged_into_user_id) VALUES ('bad-active', 'Bad', 'ACTIVE', $1)`,
		activeID,
	); err == nil {
		t.Fatalf("expected check violation for ACTIVE user with merged_into_user_id")
	}

	// Case 3: MERGED user missing merged_into_user_id violates users_status_merge_check.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('bad-merged', 'Bad', 'MERGED')`,
	); err == nil {
		t.Fatalf("expected check violation for MERGED user without merged_into_user_id")
	}

	// Case 4: MERGED user pointing to its own UUID violates the merged_into_user_id <> id guard.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, persona_slug, display_name, status, merged_into_user_id) VALUES ($1, 'self-merge', 'Self', 'MERGED', $1)`,
		"11111111-1111-1111-1111-111111111111",
	); err == nil {
		t.Fatalf("expected check violation for self-merged user")
	}

	// Case 5: Valid linked_identities row succeeds, while inserting a duplicate
	// (identity_type, identity_hash) pair fails linked_identities_type_hash_unique.
	if _, err := pool.Exec(ctx,
		`INSERT INTO linked_identities (canonical_user_id, identity_type, identity_hash, is_verified, source, verified_at)
		 VALUES ($1, 'PAN_HASH', 'hash-pan-1', true, 'AA_DEPOSIT', NOW())`,
		activeID,
	); err != nil {
		t.Fatalf("insert linked identity: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO linked_identities (canonical_user_id, identity_type, identity_hash, is_verified, source)
		 VALUES ($1, 'PAN_HASH', 'hash-pan-1', false, 'CAS')`,
		activeID,
	); err == nil {
		t.Fatalf("expected unique violation on duplicate (identity_type, identity_hash)")
	}
}
