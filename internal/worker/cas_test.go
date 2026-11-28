package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/Adhish78/ReconcileX/internal/worker"
)

// TestWorkerIngestsCASCoreTypesAndDeduplicates verifies end-to-end CAS ingestion
// across all 8 core transaction types, same-key ordinal handling, zero-sum
// double-entry conservation across ASSET:MF_COST / EQUITY:EXTERNAL_CLEARING /
// INCOME:SALARY_DIVIDEND, and idempotent re-ingestion.
func TestWorkerIngestsCASCoreTypesAndDeduplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('aarav', 'Aarav Sharma') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	casPayload := `{
		"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
		"file_type": "CAMS",
		"cas_type": "DETAILED",
		"investor_info": {"name": "Aarav Sharma"},
		"folios": [{
			"folio": "12345678 / 90",
			"amc": "PPFAS Mutual Fund",
			"PAN": "ABCDE1234F",
			"schemes": [{
				"scheme": "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
				"rta": "CAMS",
				"isin": "INF879O01027",
				"amfi": "122639",
				"open": "0.000",
				"close": "165.000",
				"valuation": {"date": "2025-03-31", "nav": "90.0000", "cost": "13700.00", "value": "14850.00"},
				"transactions": [
					{"date": "2024-04-05", "description": "Purchase", "amount": "5000.00", "units": "62.500", "type": "PURCHASE"},
					{"date": "2024-05-05", "description": "SIP Purchase - Inst 1", "amount": "5000.00", "units": "60.000", "type": "PURCHASE_SIP"},
					{"date": "2024-06-10", "description": "Switch In", "amount": "4000.00", "units": "45.000", "type": "SWITCH_IN"},
					{"date": "2024-07-15", "description": "Dividend Reinvest @ Rs. 2.00", "amount": "500.00", "units": "5.500", "type": "DIVIDEND_REINVEST"},
					{"date": "2024-08-15", "description": "Dividend Payout @ Rs. 1.00", "amount": "250.00", "units": null, "type": "DIVIDEND_PAYOUT"},
					{"date": "2024-09-10", "description": "Redemption", "amount": "-1000.00", "units": "-10.000", "type": "REDEMPTION"},
					{"date": "2024-10-10", "description": "Switch Out", "amount": "-500.00", "units": "-5.000", "type": "SWITCH_OUT"},
					{"date": "2024-11-10", "description": "SIP Purchase - Same Day", "amount": "1000.00", "units": "10.000", "type": "PURCHASE_SIP"},
					{"date": "2024-11-10", "description": "SIP Purchase - Same Day", "amount": "1000.00", "units": "10.000", "type": "PURCHASE_SIP"},
					{"date": "2024-11-12", "description": "Reversal of Purchase", "amount": "-1300.00", "units": "-13.000", "type": "REVERSAL"}
				]
			}]
		}]
	}`

	job1 := enqueueJob(t, ctx, pool, &userID, "CAS", casPayload)
	job2 := enqueueJob(t, ctx, pool, &userID, "CAS", casPayload)

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	for i := 0; i < 2; i++ {
		if claimed, err := proc.ProcessOne(ctx, "cas-worker"); err != nil || !claimed {
			t.Fatalf("ProcessOne(%d): claimed=%v err=%v", i, claimed, err)
		}
	}

	// Both jobs must finish COMPLETED.
	for _, id := range []string{job1, job2} {
		var status string
		if err := pool.QueryRow(ctx, `SELECT status::text FROM ingestion_jobs WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("query job %s: %v", id, err)
		}
		if status != "COMPLETED" {
			t.Fatalf("job %s status = %s, want COMPLETED", id, status)
		}
	}

	// Expected Cost Basis on ASSET:MF_COST:
	// +5000 + 5000 + 4000 + 500 - 1000 - 500 + 1000 + 1000 - 1300 = 13700.0000
	// Expected Units on ASSET:MF_COST:
	// +62.5 + 60 + 45 + 5.5 - 10 - 5 + 10 + 10 - 13 = 165.000000
	// Expected INCOME:SALARY_DIVIDEND: -(500 + 250) = -750.0000
	// Expected EQUITY:EXTERNAL_CLEARING: -(13700 - 250) + ... = -12950.0000
	var mfCost, mfUnits, clearing, income, totalSum string
	var txnCount int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM ledger_transactions),
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'ASSET:MF_COST'), 0)::text,
			COALESCE(SUM(units) FILTER (WHERE coa_account = 'ASSET:MF_COST'), 0)::text,
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'EQUITY:EXTERNAL_CLEARING'), 0)::text,
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'INCOME:SALARY_DIVIDEND'), 0)::text,
			COALESCE(SUM(amount), 0)::text
		FROM ledger_entries
	`).Scan(&txnCount, &mfCost, &mfUnits, &clearing, &income, &totalSum); err != nil {
		t.Fatalf("query ledger sums: %v", err)
	}
	if txnCount != 10 {
		t.Fatalf("ledger_transactions count = %d, want 10 (deduplicated)", txnCount)
	}
	if mfCost != "13700.0000" || mfUnits != "165.000000" || clearing != "-12950.0000" || income != "-750.0000" || totalSum != "0.0000" {
		t.Fatalf("unexpected ledger balances: cost=%s units=%s clearing=%s income=%s sum=%s", mfCost, mfUnits, clearing, income, totalSum)
	}

	var natKey, folioNum, isin, repUnits, repBal string
	var schemeCode int
	if err := pool.QueryRow(ctx, `
		SELECT natural_key, folio_number, isin, scheme_code, reported_units::text, reported_balance::text
		FROM accounts
		WHERE canonical_user_id = $1 AND source_type = 'MUTUAL_FUND'
	`, userID).Scan(&natKey, &folioNum, &isin, &schemeCode, &repUnits, &repBal); err != nil {
		t.Fatalf("query MUTUAL_FUND Financial Account: %v", err)
	}
	if natKey != "CAMS|12345678 / 90|INF879O01027" || folioNum != "12345678 / 90" || isin != "INF879O01027" || schemeCode != 122639 || repUnits != "165.000000" || repBal != "14850.0000" {
		t.Fatalf("unexpected Folio holding metadata: key=%q folio=%q isin=%q code=%d units=%s bal=%s", natKey, folioNum, isin, schemeCode, repUnits, repBal)
	}
}

// TestWorkerCASUnitChecksumFailureRoutesToDeadLetterWithZeroWrites confirms that a
// Unit Checksum mismatch fails straight to DEAD_LETTER (ERR_UNIT_CHECKSUM) with
// zero accounts, Ledger Transactions, or Ledger Entries written.
func TestWorkerCASUnitChecksumFailureRoutesToDeadLetterWithZeroWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('priya', 'Priya Nair') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	corruptCAS := `{
		"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
		"file_type": "CAMS",
		"cas_type": "DETAILED",
		"investor_info": {"name": "Priya Nair"},
		"folios": [{
			"folio": "CORRUPT-1",
			"amc": "HDFC Mutual Fund",
			"schemes": [{
				"scheme": "HDFC Large Cap Fund",
				"rta": "CAMS",
				"isin": "INF179K01YV8",
				"amfi": "119018",
				"open": "0.000",
				"close": "99.000",
				"valuation": {"date": "2025-03-31", "nav": "1000.00", "value": "99000.00"},
				"transactions": [
					{"date": "2024-04-05", "description": "Purchase", "amount": "10000.00", "units": "10.000", "type": "PURCHASE"}
				]
			}]
		}]
	}`

	jobID := enqueueJob(t, ctx, pool, &userID, "CAS", corruptCAS)
	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	if claimed, err := proc.ProcessOne(ctx, "cas-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne corrupt CAS: claimed=%v err=%v", claimed, err)
	}

	var status, errCode string
	var attempts int
	if err := pool.QueryRow(ctx, `
		SELECT status::text, COALESCE(last_error_code, ''), attempt_count
		FROM ingestion_jobs WHERE id = $1
	`, jobID).Scan(&status, &errCode, &attempts); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if status != "DEAD_LETTER" || errCode != "ERR_UNIT_CHECKSUM" || attempts != 0 {
		t.Fatalf("expected DEAD_LETTER/ERR_UNIT_CHECKSUM/0 attempts, got %s/%s/%d", status, errCode, attempts)
	}

	var accts, txns, entries int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM accounts),
			(SELECT COUNT(*) FROM ledger_transactions),
			(SELECT COUNT(*) FROM ledger_entries)
	`).Scan(&accts, &txns, &entries); err != nil {
		t.Fatalf("count writes: %v", err)
	}
	if accts != 0 || txns != 0 || entries != 0 {
		t.Fatalf("expected zero DB writes on ERR_UNIT_CHECKSUM, got accts=%d txns=%d entries=%d", accts, txns, entries)
	}
}

// TestWorkerCASSummaryValidAndUnitChecksumDeadLetter verifies KFintech SUMMARY CAS
// ingestion (|open - close| <= 0.0001) and rejects SUMMARY checksum mismatches to
// DEAD_LETTER (ERR_UNIT_CHECKSUM) with zero writes.
func TestWorkerCASSummaryValidAndUnitChecksumDeadLetter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('rohan', 'Rohan Mehta') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	corruptSummary := `{
		"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
		"file_type": "KFINTECH",
		"cas_type": "SUMMARY",
		"investor_info": {"name": "Rohan Mehta"},
		"folios": [{
			"folio": "KFIN-BAD-1",
			"amc": "Axis Mutual Fund",
			"schemes": [{
				"scheme": "Axis ELSS Tax Saver Fund",
				"rta": "KFINTECH",
				"isin": "INF846K01EW2",
				"amfi": "120503",
				"open": "25.0000",
				"close": "26.0000",
				"valuation": {"date": "2025-03-31", "nav": "117.8556", "value": "3064.25"},
				"transactions": []
			}]
		}]
	}`

	validSummary := `{
		"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
		"file_type": "KFINTECH",
		"cas_type": "SUMMARY",
		"investor_info": {"name": "Rohan Mehta"},
		"folios": [{
			"folio": "KFIN-OK-1",
			"amc": "Axis Mutual Fund",
			"schemes": [{
				"scheme": "Axis ELSS Tax Saver Fund",
				"rta": "KFINTECH",
				"isin": "INF846K01EW2",
				"amfi": "120503",
				"open": "25.0000",
				"close": "25.0001",
				"valuation": {"date": "2025-03-31", "nav": "117.8556", "value": "2946.40"},
				"transactions": []
			}]
		}]
	}`

	badJob := enqueueJob(t, ctx, pool, &userID, "CAS", corruptSummary)
	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	if claimed, err := proc.ProcessOne(ctx, "cas-summary-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne corrupt SUMMARY: claimed=%v err=%v", claimed, err)
	}

	var status, errCode string
	if err := pool.QueryRow(ctx, `SELECT status::text, COALESCE(last_error_code, '') FROM ingestion_jobs WHERE id = $1`, badJob).Scan(&status, &errCode); err != nil {
		t.Fatalf("query bad summary job: %v", err)
	}
	if status != "DEAD_LETTER" || errCode != "ERR_UNIT_CHECKSUM" {
		t.Fatalf("expected DEAD_LETTER/ERR_UNIT_CHECKSUM, got %s/%s", status, errCode)
	}

	var acctCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&acctCount); err != nil || acctCount != 0 {
		t.Fatalf("expected 0 accounts after corrupt SUMMARY, got %d (err=%v)", acctCount, err)
	}

	goodJob := enqueueJob(t, ctx, pool, &userID, "CAS", validSummary)
	if claimed, err := proc.ProcessOne(ctx, "cas-summary-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne valid SUMMARY: claimed=%v err=%v", claimed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ingestion_jobs WHERE id = $1`, goodJob).Scan(&status); err != nil {
		t.Fatalf("query good summary job: %v", err)
	}
	if status != "COMPLETED" {
		t.Fatalf("expected COMPLETED for valid SUMMARY, got %s", status)
	}

	var natKey, repUnits string
	if err := pool.QueryRow(ctx, `
		SELECT natural_key, reported_units::text
		FROM accounts WHERE canonical_user_id = $1 AND source_type = 'MUTUAL_FUND'
	`, userID).Scan(&natKey, &repUnits); err != nil {
		t.Fatalf("query SUMMARY Folio holding: %v", err)
	}
	if natKey != "KFINTECH|KFIN-OK-1|INF846K01EW2" || repUnits != "25.000100" {
		t.Fatalf("unexpected SUMMARY Folio holding: key=%q units=%s", natKey, repUnits)
	}
}
