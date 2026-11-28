package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/Adhish78/ReconcileX/internal/liability"
	"github.com/Adhish78/ReconcileX/internal/worker"
)

// TestWorkerIngestsLoanEMISplitAndReconcilesPrincipal verifies end-to-end
// LIABILITY ingestion for HOME_LOAN and AUTO_LOAN:
//   - First-ingest OPENING_ANCHOR derivation for partial windows
//   - Zero-sum EMI_REPAYMENT component split across LIABILITY:LOAN, EXPENSE:LOAN_INTEREST,
//     EXPENSE:LOAN_FEES, and EQUITY:EXTERNAL_CLEARING
//   - Omitted split fields defaulting to 100% principal
//   - DISBURSEMENT, INTEREST_ADJUSTMENT, and FEE_CHARGE posting
//   - ABS(LIABILITY:LOAN) reconciling against Summary.outstandingBalance
//   - Idempotent re-ingestion deduplication
func TestWorkerIngestsLoanEMISplitAndReconcilesPrincipal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('aarav', 'Aarav Sharma') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Partial window starting with outstanding principal 4,500,000 before May events:
	//   - May 05: EMI_REPAYMENT 43,891 = 12,000 principal + 31,391 interest + 500 fee (transactionalBalance = 4,488,000)
	//   - May 10: FEE_CHARGE 250 -> EXPENSE:LOAN_FEES (+250) vs CLEARING (-250)
	//   - May 15: INTEREST_ADJUSTMENT 150 -> EXPENSE:LOAN_INTEREST (+150) vs CLEARING (-150)
	//   - May 20: DISBURSEMENT 100,000 -> LIABILITY:LOAN (-100,000) vs CLEARING (+100,000)
	//   - Jun 05: EMI_REPAYMENT 40,000 (split omitted -> 100% principal = 40,000)
	// Expected final LIABILITY:LOAN = -4,500,000 + 12,000 - 100,000 + 40,000 = -4,548,000.0000
	// Expected ABS(LIABILITY:LOAN) = 4,548,000.0000 == Summary.outstandingBalance
	homePayload := `{
		"fipId": "HDFC-LOAN-FIP",
		"institutionName": "HDFC Home Loans",
		"Account": {
			"type": "liability",
			"subType": "HOME_LOAN",
			"version": "1.0.0",
			"maskedAccNumber": "HL-XXXX-8812",
			"linkedAccRef": "HL-REF-8812",
			"Summary": {
				"sanctionedPrincipal": "5000000.00",
				"outstandingBalance": "4548000.00",
				"currency": "INR",
				"balanceDateTime": "2026-06-05T18:00:00+05:30",
				"status": "ACTIVE"
			},
			"Transactions": {
				"startDate": "2026-05-01",
				"endDate": "2026-06-30",
				"Transaction": [
					{
						"txnId": "HL-EMI-1",
						"type": "EMI_REPAYMENT",
						"amount": "43891.00",
						"principalComponent": "12000.00",
						"interestComponent": "31391.00",
						"feeComponent": "500.00",
						"transactionalBalance": "4488000.00",
						"transactionTimestamp": "2026-05-05T09:00:00+05:30",
						"narration": "ACH/DR/HDFC HOME LOAN EMI"
					},
					{
						"txnId": "HL-FEE-1",
						"type": "FEE_CHARGE",
						"amount": "250.00",
						"transactionTimestamp": "2026-05-10T10:00:00+05:30",
						"narration": "DOC CHARGE"
					},
					{
						"txnId": "HL-INT-1",
						"type": "INTEREST_ADJUSTMENT",
						"amount": "150.00",
						"transactionTimestamp": "2026-05-15T10:00:00+05:30",
						"narration": "RATE RESET ADJ"
					},
					{
						"txnId": "HL-DISB-1",
						"type": "DISBURSEMENT",
						"amount": "100000.00",
						"transactionTimestamp": "2026-05-20T11:00:00+05:30",
						"narration": "TRANCHE 2"
					},
					{
						"txnId": "HL-EMI-2",
						"type": "EMI_REPAYMENT",
						"amount": "40000.00",
						"transactionTimestamp": "2026-06-05T09:00:00+05:30",
						"narration": "PREPAYMENT PRINCIPAL ONLY"
					}
				]
			}
		}
	}`

	job1 := enqueueJob(t, ctx, pool, &userID, "LIABILITY", homePayload)
	job2 := enqueueJob(t, ctx, pool, &userID, "LIABILITY", homePayload)

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	for i := 0; i < 2; i++ {
		if claimed, err := proc.ProcessOne(ctx, "loan-worker"); err != nil || !claimed {
			t.Fatalf("ProcessOne(%d): claimed=%v err=%v", i, claimed, err)
		}
	}

	for _, id := range []string{job1, job2} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status::text FROM ingestion_jobs WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("query job %s: %v", id, err)
		}
		if status != "COMPLETED" {
			t.Fatalf("job %s status = %s, want COMPLETED", id, status)
		}
	}

	var txnCount int
	var loanBal, intExp, feeExp, totalSum string
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM ledger_transactions),
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'LIABILITY:LOAN'), 0)::text,
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'EXPENSE:LOAN_INTEREST'), 0)::text,
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'EXPENSE:LOAN_FEES'), 0)::text,
			COALESCE(SUM(amount), 0)::text
		FROM ledger_entries
	`).Scan(&txnCount, &loanBal, &intExp, &feeExp, &totalSum); err != nil {
		t.Fatalf("query loan ledger sums: %v", err)
	}

	// 1 OPENING_ANCHOR + 5 txns = 6 deduplicated transactions.
	if txnCount != 6 {
		t.Fatalf("ledger_transactions count = %d, want 6", txnCount)
	}
	if loanBal != "-4548000.0000" {
		t.Fatalf("LIABILITY:LOAN sum = %s, want -4548000.0000", loanBal)
	}
	if intExp != "31541.0000" {
		t.Fatalf("EXPENSE:LOAN_INTEREST sum = %s, want 31541.0000 (31391 + 150)", intExp)
	}
	if feeExp != "750.0000" {
		t.Fatalf("EXPENSE:LOAN_FEES sum = %s, want 750.0000 (500 + 250)", feeExp)
	}
	if totalSum != "0.0000" {
		t.Fatalf("global ledger sum = %s, want 0.0000", totalSum)
	}

	// Verify OPENING_ANCHOR timestamp (T_0 - 1us) and idempotency key.
	var anchorKey string
	var anchorTS time.Time
	var anchorLoanAmt string
	if err := pool.QueryRow(ctx, `
		SELECT lt.idempotency_key, lt.txn_timestamp, le.amount::text
		FROM ledger_transactions lt
		JOIN ledger_entries le ON le.transaction_id = lt.id
		WHERE lt.txn_type = 'OPENING_ANCHOR' AND le.coa_account = 'LIABILITY:LOAN'
	`).Scan(&anchorKey, &anchorTS, &anchorLoanAmt); err != nil {
		t.Fatalf("query opening anchor: %v", err)
	}
	wantT0, _ := time.Parse(time.RFC3339, "2026-05-05T09:00:00+05:30")
	if anchorKey != liability.LoanAnchorIdempotencyKey("HDFC-LOAN-FIP|HL-REF-8812") ||
		!anchorTS.Equal(wantT0.Add(-time.Microsecond)) ||
		anchorLoanAmt != "-4500000.0000" {
		t.Fatalf("unexpected anchor: key=%q ts=%v amt=%s", anchorKey, anchorTS, anchorLoanAmt)
	}
}

// TestWorkerLoanDeadLetterEMISplitAndBackfillGuard verifies:
//   - Mismatched EMI_REPAYMENT split moves immediately to DEAD_LETTER (ERR_INVALID_EMI_SPLIT) with zero ledger rows
//   - Transaction predating an existing loan OPENING_ANCHOR moves immediately to DEAD_LETTER (ERR_BACKFILL_BEFORE_ANCHOR) with zero state changes
func TestWorkerLoanDeadLetterEMISplitAndBackfillGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('priya', 'Priya Nair') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Case 1: Invalid EMI split -> immediate DEAD_LETTER (ERR_INVALID_EMI_SPLIT), 0 ledger rows.
	badSplitPayload := `{
		"fipId": "ICICI-AUTO-FIP",
		"institutionName": "ICICI Auto Loans",
		"Account": {
			"type": "liability",
			"subType": "AUTO_LOAN",
			"version": "1.0.0",
			"linkedAccRef": "AL-REF-1",
			"Summary": {"outstandingBalance": "600000.00", "currency": "INR"},
			"Transactions": {
				"Transaction": [{
					"txnId": "AL-BAD-EMI",
					"type": "EMI_REPAYMENT",
					"amount": "18500.00",
					"principalComponent": "12000.00",
					"interestComponent": "6000.00",
					"feeComponent": "100.00",
					"transactionTimestamp": "2026-06-05T09:00:00+05:30"
				}]
			}
		}
	}`
	badJobID := enqueueJob(t, ctx, pool, &userID, "LIABILITY", badSplitPayload)
	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	if claimed, err := proc.ProcessOne(ctx, "loan-dlq-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne badSplit: claimed=%v err=%v", claimed, err)
	}

	var status, errCode string
	var attempts, ledgerCount int
	if err := pool.QueryRow(ctx, `
		SELECT status::text, COALESCE(last_error_code, ''), attempt_count,
			(SELECT COUNT(*) FROM ledger_transactions)
		FROM ingestion_jobs WHERE id = $1
	`, badJobID).Scan(&status, &errCode, &attempts, &ledgerCount); err != nil {
		t.Fatalf("query bad split job: %v", err)
	}
	if status != "DEAD_LETTER" || errCode != "ERR_INVALID_EMI_SPLIT" || attempts != 0 || ledgerCount != 0 {
		t.Fatalf("got status=%s code=%s attempts=%d ledgerCount=%d, want DEAD_LETTER/ERR_INVALID_EMI_SPLIT/0/0",
			status, errCode, attempts, ledgerCount)
	}

	// Case 2: Valid initial AUTO_LOAN with empty transactions derives OPENING_ANCHOR from Summary.outstandingBalance;
	// subsequent backfill predating anchor fails to DEAD_LETTER (ERR_BACKFILL_BEFORE_ANCHOR).
	initPayload := `{
		"fipId": "ICICI-AUTO-FIP",
		"institutionName": "ICICI Auto Loans",
		"Account": {
			"type": "liability",
			"subType": "AUTO_LOAN",
			"version": "1.0.0",
			"linkedAccRef": "AL-REF-1",
			"Summary": {"outstandingBalance": "600000.00", "currency": "INR"},
			"Transactions": {
				"startDate": "2026-05-01",
				"Transaction": []
			}
		}
	}`
	enqueueJob(t, ctx, pool, &userID, "LIABILITY", initPayload)
	if claimed, err := proc.ProcessOne(ctx, "loan-dlq-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne init AUTO_LOAN: claimed=%v err=%v", claimed, err)
	}

	backfillPayload := `{
		"fipId": "ICICI-AUTO-FIP",
		"institutionName": "ICICI Auto Loans",
		"Account": {
			"type": "liability",
			"subType": "AUTO_LOAN",
			"version": "1.0.0",
			"linkedAccRef": "AL-REF-1",
			"Summary": {"outstandingBalance": "618500.00", "currency": "INR"},
			"Transactions": {
				"Transaction": [{
					"txnId": "AL-OLD-1",
					"type": "EMI_REPAYMENT",
					"amount": "18500.00",
					"principalComponent": "12000.00",
					"interestComponent": "6500.00",
					"transactionTimestamp": "2026-04-05T09:00:00+05:30"
				}]
			}
		}
	}`
	bfJobID := enqueueJob(t, ctx, pool, &userID, "LIABILITY", backfillPayload)
	if claimed, err := proc.ProcessOne(ctx, "loan-dlq-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne backfill: claimed=%v err=%v", claimed, err)
	}

	var bfStatus, bfCode, reportedBal string
	var totalTxns int
	if err := pool.QueryRow(ctx, `
		SELECT j.status::text, COALESCE(j.last_error_code, ''),
			(SELECT COUNT(*) FROM ledger_transactions),
			(SELECT reported_balance::text FROM accounts WHERE natural_key = 'ICICI-AUTO-FIP|AL-REF-1')
		FROM ingestion_jobs j WHERE j.id = $1
	`, bfJobID).Scan(&bfStatus, &bfCode, &totalTxns, &reportedBal); err != nil {
		t.Fatalf("query backfill state: %v", err)
	}
	if bfStatus != "DEAD_LETTER" || bfCode != "ERR_BACKFILL_BEFORE_ANCHOR" || totalTxns != 1 || reportedBal != "600000.0000" {
		t.Fatalf("unexpected backfill state: status=%s code=%s txns=%d reportedBal=%s", bfStatus, bfCode, totalTxns, reportedBal)
	}
}
