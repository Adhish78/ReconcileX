// Package worker_test runs PostgreSQL integration tests for the FOR UPDATE SKIP LOCKED
// worker pool in an isolated `test_worker` schema so concurrent `go test ./...` runs
// never collide with test_db or test_api.
package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Adhish78/ReconcileX/internal/db"
	"github.com/Adhish78/ReconcileX/internal/worker"
	"github.com/Adhish78/ReconcileX/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// -----------------------------------------------------------------------------
// Test harness helpers
// -----------------------------------------------------------------------------

// testDSN resolves the test PostgreSQL DSN from RECONCILEX_TEST_DSN or falls back
// to the local compose postgres-test container on port 5433.
func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("RECONCILEX_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://reconcilex:reconcilex@localhost:5433/reconcilex_test?sslmode=disable"
}

// openWorkerTestPool resets the dedicated `test_worker` schema and applies all
// embedded SQL migrations before returning a connection pool scoped to that schema.
func openWorkerTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}

	// Isolate worker tests from db_test and api_test during parallel package test runs.
	cfg.ConnConfig.RuntimeParams["search_path"] = "test_worker"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS test_worker CASCADE; CREATE SCHEMA test_worker;`); err != nil {
		t.Fatalf("reset test_worker schema: %v", err)
	}
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return pool
}

// enqueueJob inserts a PENDING ingestion_jobs record with its payload SHA-256 digest.
func enqueueJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID *string, sourceType, payload string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(payload))
	sha := hex.EncodeToString(sum[:])

	var jobID string
	err := pool.QueryRow(ctx,
		`INSERT INTO ingestion_jobs (canonical_user_id, source_type, payload, payload_sha256)
		 VALUES ($1, $2, $3::jsonb, $4) RETURNING id::text`,
		userID, sourceType, payload, sha,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	return jobID
}

// -----------------------------------------------------------------------------
// Worker integration tests
// -----------------------------------------------------------------------------

// TestWorkerPostsDepositDeduplicatesAndRoutesInvalidToDLQ verifies single-worker
// processing: valid AA_DEPOSIT posting, overlapping transaction deduplication across
// sequential jobs, zero-sum clearing balance, and DEAD_LETTER routing on schema failure.
func TestWorkerPostsDepositDeduplicatesAndRoutesInvalidToDLQ(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('aarav', 'Aarav Sharma') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Step 1: Enqueue an initial AA_DEPOSIT payload (T-1 +50,000, T-2 -1,500),
	// an overlapping payload replaying T-2 and adding T-3 (-500), and a malformed payload.
	validPayload := `{
		"fipId": "HDFC-FIP",
		"institutionName": "HDFC Bank",
		"Account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX4020",
			"version": "2.0.0",
			"linkedAccRef": "ACC-4020",
			"Summary": {
				"currentBalance": "48500.00",
				"currency": "INR",
				"balanceDateTime": "2026-06-15T14:20:10+05:30"
			},
			"Transactions": {
				"Transaction": [
					{
						"txnId": "T-1",
						"type": "CREDIT",
						"mode": "NEFT",
						"amount": "50000.00",
						"transactionTimestamp": "2026-06-15T09:00:00+05:30",
						"narration": "NEFT SALARY"
					},
					{
						"txnId": "T-2",
						"type": "DEBIT",
						"mode": "UPI",
						"amount": "1500.00",
						"transactionTimestamp": "2026-06-15T14:20:10+05:30",
						"narration": "UPI/DR/SWIGGY"
					}
				]
			}
		}
	}`

	job1 := enqueueJob(t, ctx, pool, &userID, "AA_DEPOSIT", validPayload)

	// Second job replays overlapping txn T-2 and adds T-3; T-2 must deduplicate cleanly.
	overlapPayload := `{
		"fipId": "HDFC-FIP",
		"institutionName": "HDFC Bank",
		"Account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX4020",
			"version": "2.0.0",
			"linkedAccRef": "ACC-4020",
			"Summary": {
				"currentBalance": "48000.00",
				"currency": "INR",
				"balanceDateTime": "2026-06-16T10:00:00+05:30"
			},
			"Transactions": {
				"Transaction": [
					{
						"txnId": "T-2",
						"type": "DEBIT",
						"mode": "UPI",
						"amount": "1500.00",
						"transactionTimestamp": "2026-06-15T14:20:10+05:30",
						"narration": "UPI/DR/SWIGGY"
					},
					{
						"txnId": "T-3",
						"type": "DEBIT",
						"mode": "UPI",
						"amount": "500.00",
						"transactionTimestamp": "2026-06-16T10:00:00+05:30",
						"narration": "UPI/DR/CAFE"
					}
				]
			}
		}
	}`
	job2 := enqueueJob(t, ctx, pool, &userID, "AA_DEPOSIT", overlapPayload)
	badJob := enqueueJob(t, ctx, pool, &userID, "AA_DEPOSIT", `{"Account":{"type":"deposit"}}`)

	// Step 2: Drain all 3 queued jobs deterministically via ProcessOne.
	proc := worker.NewPool(pool, worker.Config{Concurrency: 1, PollInterval: 20 * time.Millisecond})
	for i := 0; i < 3; i++ {
		claimed, err := proc.ProcessOne(ctx, "worker-test")
		if err != nil {
			t.Fatalf("ProcessOne(%d): %v", i, err)
		}
		if !claimed {
			t.Fatalf("expected job to be claimed on iteration %d", i)
		}
	}

	// Step 3: Verify job1 and job2 finished COMPLETED while badJob routed to DEAD_LETTER.
	var status1, status2, statusBad, errCode string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ingestion_jobs WHERE id = $1`, job1).Scan(&status1); err != nil {
		t.Fatalf("query job1: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ingestion_jobs WHERE id = $1`, job2).Scan(&status2); err != nil {
		t.Fatalf("query job2: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text, COALESCE(last_error_code, '') FROM ingestion_jobs WHERE id = $1`, badJob).Scan(&statusBad, &errCode); err != nil {
		t.Fatalf("query badJob: %v", err)
	}
	if status1 != "COMPLETED" || status2 != "COMPLETED" {
		t.Fatalf("expected job1 and job2 COMPLETED, got %s, %s", status1, status2)
	}
	if statusBad != "DEAD_LETTER" || errCode != "ERR_SCHEMA_VALIDATION" {
		t.Fatalf("expected badJob DEAD_LETTER/ERR_SCHEMA_VALIDATION, got %s/%s", statusBad, errCode)
	}

	// Step 4: Verify exact bank balance: 50000 - 1500 - 500 = 48000.0000 (T-2 deduplicated), and clearing = -48000.0000.
	var bankBal, clearingBal string
	if err := pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(le.amount) FILTER (WHERE le.coa_account = 'ASSET:BANK'), 0)::text,
			COALESCE(SUM(le.amount) FILTER (WHERE le.coa_account = 'EQUITY:EXTERNAL_CLEARING'), 0)::text
		FROM ledger_entries le
		JOIN accounts a ON a.id = le.account_id
		WHERE a.canonical_user_id = $1
	`, userID).Scan(&bankBal, &clearingBal); err != nil {
		t.Fatalf("query balances: %v", err)
	}
	if bankBal != "48000.0000" || clearingBal != "-48000.0000" {
		t.Fatalf("unexpected balances: bank=%s clearing=%s", bankBal, clearingBal)
	}
}

// TestConcurrentSkipLockedWorkersNoDoublePosting races 8 concurrent goroutines
// against 16 queued jobs for the same Financial Account to verify SKIP LOCKED
// claim safety and exact balance aggregation without double-posting.
func TestConcurrentSkipLockedWorkersNoDoublePosting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('priya', 'Priya Nair') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Enqueue 16 distinct 100.00 CREDIT jobs targeting the same ICICI deposit account.
	const jobCount = 16
	for i := 0; i < jobCount; i++ {
		payload := fmt.Sprintf(`{
			"fipId": "ICICI-FIP",
			"institutionName": "ICICI Bank",
			"Account": {
				"type": "deposit",
				"maskedAccNumber": "XXXXXXXX9911",
				"version": "2.0.0",
				"linkedAccRef": "ICICI-9911",
				"Transactions": {
					"Transaction": [{
						"txnId": "CONC-TXN-%02d",
						"type": "CREDIT",
						"mode": "IMPS",
						"amount": "100.00",
						"transactionTimestamp": "2026-06-15T10:00:00+05:30",
						"narration": "IMPS/CR/TEST-%02d"
					}]
				}
			}
		}`, i, i)
		enqueueJob(t, ctx, pool, &userID, "AA_DEPOSIT", payload)
	}

	// Run 8 concurrent worker goroutines draining the queue via SKIP LOCKED.
	proc := worker.NewPool(pool, worker.Config{Concurrency: 8, PollInterval: 10 * time.Millisecond})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			workerName := fmt.Sprintf("w-%d", id)
			for {
				claimed, err := proc.ProcessOne(ctx, workerName)
				if err != nil {
					t.Errorf("worker %s err: %v", workerName, err)
					return
				}
				if !claimed {
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Verify all 16 jobs completed and the bank balance equals exactly 16 * 100 = 1600.0000.
	var completedCount int
	var bankBal string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM ingestion_jobs WHERE status = 'COMPLETED'`).Scan(&completedCount); err != nil {
		t.Fatalf("count completed: %v", err)
	}
	if completedCount != jobCount {
		t.Fatalf("expected %d COMPLETED jobs, got %d", jobCount, completedCount)
	}

	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(le.amount), 0)::text
		FROM ledger_entries le
		JOIN accounts a ON a.id = le.account_id
		WHERE a.canonical_user_id = $1 AND le.coa_account = 'ASSET:BANK'
	`, userID).Scan(&bankBal); err != nil {
		t.Fatalf("query bank balance: %v", err)
	}
	if bankBal != "1600.0000" {
		t.Fatalf("expected 1600.0000, got %s", bankBal)
	}
}

// TestWorkerUnlinkedReingestReusesExistingAccountAndDeduplicates ensures two
// unlinked jobs (canonical_user_id = NULL) sharing the same DEPOSIT natural_key
// resolve to the same Canonical User and Financial Account without double-posting.
func TestWorkerUnlinkedReingestReusesExistingAccountAndDeduplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)

	payload := `{
		"fipId": "AXIS-FIP",
		"institutionName": "Axis Bank",
		"Account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX1111",
			"version": "2.0.0",
			"linkedAccRef": "AXIS-1111",
			"Transactions": {
				"Transaction": [{
					"txnId": "UNLINKED-1",
					"type": "CREDIT",
					"mode": "NEFT",
					"amount": "2500.00",
					"transactionTimestamp": "2026-06-15T10:00:00+05:30",
					"narration": "NEFT CREDIT"
				}]
			}
		}
	}`

	enqueueJob(t, ctx, pool, nil, "AA_DEPOSIT", payload)
	enqueueJob(t, ctx, pool, nil, "AA_DEPOSIT", payload)

	// Process both unlinked jobs sequentially; the second job must reuse the first user/account.
	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	for i := 0; i < 2; i++ {
		if claimed, err := proc.ProcessOne(ctx, "w-unlinked"); err != nil || !claimed {
			t.Fatalf("ProcessOne(%d): claimed=%v err=%v", i, claimed, err)
		}
	}

	var userCount, accountCount int
	var bal string
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&accountCount); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0)::text FROM ledger_entries WHERE coa_account = 'ASSET:BANK'`).Scan(&bal); err != nil {
		t.Fatalf("sum balance: %v", err)
	}
	if userCount != 1 || accountCount != 1 || bal != "2500.0000" {
		t.Fatalf("expected 1 user, 1 account, 2500.0000 balance; got users=%d accounts=%d bal=%s", userCount, accountCount, bal)
	}
}
