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

// TestLedgerSchemaAndTriggers exercises 0002_ledger_and_jobs.sql:
//   - 13-bucket coa_account and 17-value canonical_category enums
//   - accounts, ingestion_jobs, ledger_transactions (no canonical_user_id), ledger_entries
//   - covering index idx_ledger_entries_account_coa_cov
//   - DEFERRABLE INITIALLY DEFERRED SUM(amount) = 0 trigger & account_id consistency
//   - append-only triggers on ledger_entries and restricted category-only updates on ledger_transactions
func TestLedgerSchemaAndTriggers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openCleanPool(t)
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify coa_account has 13 values and canonical_category has 17 values.
	var coaCount, catCount int
	if err := pool.QueryRow(ctx, `SELECT array_length(enum_range(NULL::coa_account), 1)`).Scan(&coaCount); err != nil {
		t.Fatalf("query coa_account enum length: %v", err)
	}
	if coaCount != 13 {
		t.Fatalf("expected 13 coa_account values, got %d", coaCount)
	}
	if err := pool.QueryRow(ctx, `SELECT array_length(enum_range(NULL::canonical_category), 1)`).Scan(&catCount); err != nil {
		t.Fatalf("query canonical_category enum length: %v", err)
	}
	if catCount != 17 {
		t.Fatalf("expected 17 canonical_category values, got %d", catCount)
	}

	// Verify ledger_transactions intentionally omits canonical_user_id (ADR-0003).
	var hasCanonicalUserCol bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'ledger_transactions'
			  AND column_name = 'canonical_user_id'
		)`).Scan(&hasCanonicalUserCol); err != nil {
		t.Fatalf("check canonical_user_id column: %v", err)
	}
	if hasCanonicalUserCol {
		t.Fatalf("ledger_transactions must omit canonical_user_id per ADR-0003")
	}

	// Verify covering index for zero-heap-fetch balance aggregation exists.
	var hasCovIndex bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND tablename = 'ledger_entries'
			  AND indexname = 'idx_ledger_entries_account_coa_cov'
		)`).Scan(&hasCovIndex); err != nil {
		t.Fatalf("check covering index: %v", err)
	}
	if !hasCovIndex {
		t.Fatalf("expected idx_ledger_entries_account_coa_cov to exist on ledger_entries")
	}

	// Seed a user, account, and job to test ledger triggers.
	var userID, accountID, otherAccountID, jobID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('aarav', 'Aarav') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (canonical_user_id, source_type, natural_key, institution_name)
		 VALUES ($1, 'DEPOSIT', 'HDFC-FIP|ACC-1', 'HDFC Bank') RETURNING id`,
		userID,
	).Scan(&accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (canonical_user_id, source_type, natural_key, institution_name)
		 VALUES ($1, 'DEPOSIT', 'ICICI-FIP|ACC-2', 'ICICI Bank') RETURNING id`,
		userID,
	).Scan(&otherAccountID); err != nil {
		t.Fatalf("insert other account: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ingestion_jobs (canonical_user_id, source_type, payload, payload_sha256)
		 VALUES ($1, 'AA_DEPOSIT', '{}'::jsonb, 'sha-1') RETURNING id`,
		userID,
	).Scan(&jobID); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	// Case 1: Balanced zero-sum double-entry transaction commits cleanly.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	var txnID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_transactions (account_id, ingestion_job_id, source, txn_type, category, txn_timestamp, narration, idempotency_key)
		 VALUES ($1, $2, 'AA_DEPOSIT', 'CREDIT', 'INCOME:SALARY', NOW(), 'NEFT SALARY', 'idem-1') RETURNING id`,
		accountID, jobID,
	).Scan(&txnID); err != nil {
		t.Fatalf("insert ledger_transaction: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount)
		 VALUES ($1, $2, 'ASSET:BANK', 50000.0000), ($1, $2, 'EQUITY:EXTERNAL_CLEARING', -50000.0000)`,
		txnID, accountID,
	); err != nil {
		t.Fatalf("insert balanced ledger_entries: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit balanced tx: %v", err)
	}

	// Case 2: Unbalanced transaction must fail on COMMIT due to DEFERRABLE INITIALLY DEFERRED trigger.
	txBad, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin unbalanced tx: %v", err)
	}
	var badTxnID string
	if err := txBad.QueryRow(ctx,
		`INSERT INTO ledger_transactions (account_id, ingestion_job_id, source, txn_type, txn_timestamp, idempotency_key)
		 VALUES ($1, $2, 'AA_DEPOSIT', 'DEBIT', NOW(), 'idem-bad') RETURNING id`,
		accountID, jobID,
	).Scan(&badTxnID); err != nil {
		t.Fatalf("insert unbalanced ledger_transaction: %v", err)
	}
	if _, err := txBad.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount)
		 VALUES ($1, $2, 'ASSET:BANK', -100.0000), ($1, $2, 'EQUITY:EXTERNAL_CLEARING', 90.0000)`,
		badTxnID, accountID,
	); err != nil {
		t.Fatalf("insert unbalanced entries: %v", err)
	}
	if err := txBad.Commit(ctx); err == nil {
		t.Fatalf("expected COMMIT to fail for unbalanced ledger_entries")
	}

	// Case 3: Mismatched account_id between ledger_entries and ledger_transactions must fail on COMMIT.
	txMismatch, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin mismatch tx: %v", err)
	}
	var mismatchTxnID string
	if err := txMismatch.QueryRow(ctx,
		`INSERT INTO ledger_transactions (account_id, ingestion_job_id, source, txn_type, txn_timestamp, idempotency_key)
		 VALUES ($1, $2, 'AA_DEPOSIT', 'CREDIT', NOW(), 'idem-mismatch') RETURNING id`,
		accountID, jobID,
	).Scan(&mismatchTxnID); err != nil {
		t.Fatalf("insert mismatch ledger_transaction: %v", err)
	}
	if _, err := txMismatch.Exec(ctx,
		`INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount)
		 VALUES ($1, $2, 'ASSET:BANK', 100.0000), ($1, $3, 'EQUITY:EXTERNAL_CLEARING', -100.0000)`,
		mismatchTxnID, accountID, otherAccountID,
	); err != nil {
		t.Fatalf("insert mismatched account entries: %v", err)
	}
	if err := txMismatch.Commit(ctx); err == nil {
		t.Fatalf("expected COMMIT to fail when ledger_entries.account_id differs from ledger_transactions.account_id")
	}

	// Case 4: ledger_entries blocks UPDATE and DELETE.
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET amount = 0.0001 WHERE transaction_id = $1`, txnID); err == nil {
		t.Fatalf("expected UPDATE on ledger_entries to be blocked")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries WHERE transaction_id = $1`, txnID); err == nil {
		t.Fatalf("expected DELETE on ledger_entries to be blocked")
	}

	// Case 5: ledger_transactions blocks DELETE and non-category UPDATE, but allows (category, category_updated_at) UPDATE.
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_transactions WHERE id = $1`, txnID); err == nil {
		t.Fatalf("expected DELETE on ledger_transactions to be blocked")
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_transactions SET narration = 'TAMPERED' WHERE id = $1`, txnID); err == nil {
		t.Fatalf("expected UPDATE of narration on ledger_transactions to be blocked")
	}
	if _, err := pool.Exec(ctx,
		`UPDATE ledger_transactions SET category = 'EXPENSE:FOOD_DINING', category_updated_at = NOW() WHERE id = $1`,
		txnID,
	); err != nil {
		t.Fatalf("expected UPDATE of (category, category_updated_at) on ledger_transactions to succeed: %v", err)
	}
}
