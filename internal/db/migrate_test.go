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

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RECONCILEX_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://reconcilex:reconcilex@localhost:5433/reconcilex_test?sslmode=disable"
	}
	return dsn
}

func openCleanPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "test_db"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS test_db CASCADE; CREATE SCHEMA test_db;`); err != nil {
		t.Fatalf("reset test_db schema: %v", err)
	}
	return pool
}

func TestMigrateAppliesInitialSchemaIdempotently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openCleanPool(t)

	// Before migrations run, readiness check must report unready.
	ready, err := db.MigrationsReady(ctx, pool, migrations.FS)
	if err == nil && ready {
		t.Fatalf("expected unmigrated database not to be ready")
	}

	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// Running twice must be idempotent.
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	ready, err = db.MigrationsReady(ctx, pool, migrations.FS)
	if err != nil || !ready {
		t.Fatalf("expected migrations to be ready, got ready=%v err=%v", ready, err)
	}
}

func TestUsersAndLinkedIdentitiesConstraints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openCleanPool(t)

	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Valid ACTIVE user inserts cleanly.
	var activeID string
	err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('aarav', 'Aarav Sharma', 'ACTIVE') RETURNING id`,
	).Scan(&activeID)
	if err != nil {
		t.Fatalf("insert active user: %v", err)
	}

	// ACTIVE user with merged_into_user_id must fail check constraint.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status, merged_into_user_id) VALUES ('bad-active', 'Bad', 'ACTIVE', $1)`,
		activeID,
	); err == nil {
		t.Fatalf("expected check violation for ACTIVE user with merged_into_user_id")
	}

	// MERGED user without merged_into_user_id must fail check constraint.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('bad-merged', 'Bad', 'MERGED')`,
	); err == nil {
		t.Fatalf("expected check violation for MERGED user without merged_into_user_id")
	}

	// MERGED user pointing to itself must fail check constraint.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, persona_slug, display_name, status, merged_into_user_id) VALUES ($1, 'self-merge', 'Self', 'MERGED', $1)`,
		"11111111-1111-1111-1111-111111111111",
	); err == nil {
		t.Fatalf("expected check violation for self-merged user")
	}

	// Valid linked identity inserts cleanly, duplicate (identity_type, identity_hash) fails.
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
