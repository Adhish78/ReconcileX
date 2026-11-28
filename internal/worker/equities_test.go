package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/Adhish78/ReconcileX/internal/worker"
)

// TestWorkerIngestsEquitiesCorporateActionsAndDeduplicates verifies end-to-end
// AA_EQUITIES ingestion across multiple ISINs, BUY/SELL monetary+unit postings,
// zero-cash BONUS and SPLIT unit-only Corporate Actions, and idempotent re-ingest.
func TestWorkerIngestsEquitiesCorporateActionsAndDeduplicates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('aarav', 'Aarav Sharma') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	payload := `{
		"fipId": "CDSL-FIP",
		"institutionName": "Zerodha Broking (CDSL)",
		"Account": {
			"type": "equities",
			"maskedDematId": "XXXXXXXXXXXX0987",
			"version": "1.0.0",
			"linkedAccRef": "DEMAT-001",
			"Summary": {
				"currentValue": "220000.00",
				"Investment": {
					"Holdings": {
						"type": "DEMAT",
						"Holding": [
							{
								"issuerName": "RELIANCE INDUSTRIES LTD",
								"isin": "INE002A01018",
								"isinDescription": "RELIANCE INDUSTRIES LTD EQ",
								"units": 50,
								"lastTradedPrice": "2800.00"
							},
							{
								"issuerName": "TATA CONSULTANCY SERVICES LTD",
								"isin": "INE467B01029",
								"isinDescription": "TCS LTD EQ",
								"units": 20,
								"lastTradedPrice": "4000.00"
							}
						]
					}
				}
			},
			"Transactions": {
				"startDate": "2025-01-01",
				"endDate": "2025-06-30",
				"Transaction": [
					{
						"txnId": "EQ-REL-BUY",
						"companyName": "RELIANCE INDUSTRIES LTD",
						"transactionDateTime": "2025-02-01T10:00:00+05:30",
						"exchange": "NSE",
						"isin": "INE002A01018",
						"rate": "2500.00",
						"type": "BUY",
						"units": 40
					},
					{
						"txnId": "EQ-REL-SELL",
						"companyName": "RELIANCE INDUSTRIES LTD",
						"transactionDateTime": "2025-03-01T11:00:00+05:30",
						"exchange": "NSE",
						"isin": "INE002A01018",
						"rate": "2900.00",
						"type": "SELL",
						"units": 10
					},
					{
						"txnId": "EQ-REL-BONUS",
						"companyName": "RELIANCE INDUSTRIES LTD",
						"transactionDateTime": "2025-04-01T09:00:00+05:30",
						"isin": "INE002A01018",
						"narration": "BONUS 1:1",
						"type": "BONUS",
						"units": 20
					},
					{
						"txnId": "EQ-TCS-BUY",
						"companyName": "TATA CONSULTANCY SERVICES LTD",
						"transactionDateTime": "2025-02-10T10:30:00+05:30",
						"exchange": "BSE",
						"isin": "INE467B01029",
						"rate": "3800.00",
						"type": "BUY",
						"units": 10
					},
					{
						"txnId": "EQ-TCS-SPLIT",
						"companyName": "TATA CONSULTANCY SERVICES LTD",
						"transactionDateTime": "2025-05-01T09:00:00+05:30",
						"isin": "INE467B01029",
						"narration": "STOCK SPLIT 1:2",
						"type": "SPLIT",
						"units": 10
					}
				]
			}
		}
	}`

	job1 := enqueueJob(t, ctx, pool, &userID, "AA_EQUITIES", payload)
	job2 := enqueueJob(t, ctx, pool, &userID, "AA_EQUITIES", payload)

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	for i := 0; i < 2; i++ {
		if claimed, err := proc.ProcessOne(ctx, "eq-worker"); err != nil || !claimed {
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

	// Expected Cost Basis: (40*2500 - 10*2900) + 10*3800 = 71000 + 38000 = 109000.0000
	// Expected Units: (40 - 10 + 20) + (10 + 10) = 50 + 20 = 70.000000
	var txnCount int
	var eqCost, eqUnits, clearing, totalSum string
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM ledger_transactions),
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'ASSET:EQUITY_COST'), 0)::text,
			COALESCE(SUM(units) FILTER (WHERE coa_account = 'ASSET:EQUITY_COST'), 0)::text,
			COALESCE(SUM(amount) FILTER (WHERE coa_account = 'EQUITY:EXTERNAL_CLEARING'), 0)::text,
			COALESCE(SUM(amount), 0)::text
		FROM ledger_entries
	`).Scan(&txnCount, &eqCost, &eqUnits, &clearing, &totalSum); err != nil {
		t.Fatalf("query ledger sums: %v", err)
	}
	if txnCount != 5 {
		t.Fatalf("ledger_transactions count = %d, want 5 (deduplicated)", txnCount)
	}
	if eqCost != "109000.0000" || eqUnits != "70.000000" || clearing != "-109000.0000" || totalSum != "0.0000" {
		t.Fatalf("unexpected ledger balances: cost=%s units=%s clearing=%s sum=%s", eqCost, eqUnits, clearing, totalSum)
	}

	// Verify BONUS and SPLIT posted as unit-only entries (amount = 0.0000, units > 0).
	var corpActionCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM ledger_transactions lt
		JOIN ledger_entries le ON le.transaction_id = lt.id
		WHERE lt.txn_type IN ('BONUS', 'SPLIT')
		  AND le.coa_account = 'ASSET:EQUITY_COST'
		  AND le.amount = 0.0000
		  AND le.units > 0
	`).Scan(&corpActionCount); err != nil {
		t.Fatalf("query corporate action entries: %v", err)
	}
	if corpActionCount != 2 {
		t.Fatalf("corporate action unit-only entries = %d, want 2", corpActionCount)
	}
}

// TestWorkerEquitiesPerISINOpeningAnchorsAndBackfillGuard verifies:
//   - Per-ISIN unit-only OPENING_ANCHOR derivation on first ingestion when Holding.units - SUM(txn.units) != 0
//   - Deterministic per-ISIN idempotency and no duplicate anchors on forward re-ingestion
//   - Immediate DEAD_LETTER (ERR_BACKFILL_BEFORE_ANCHOR) with zero writes when a payload predates an ISIN anchor
func TestWorkerEquitiesPerISINOpeningAnchorsAndBackfillGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openWorkerTestPool(t)
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name) VALUES ('priya', 'Priya Nair') RETURNING id::text`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// First ingest across 3 ISINs:
	// - INE002A01018 (RELIANCE): holding 50, BUY 20 -> anchor +30 units at T_0 - 1us
	// - INE009A01021 (INFY): holding 15, 0 txns -> anchor +15 units at startDate
	// - INE467B01029 (TCS): holding 10, BUY 10 -> diff 0 -> no anchor
	firstPayload := `{
		"fipId": "CDSL-FIP",
		"institutionName": "Zerodha Broking",
		"Account": {
			"type": "equities",
			"maskedDematId": "XXXXXXXXXXXX1122",
			"version": "1.0.0",
			"linkedAccRef": "DEMAT-ANCH-1",
			"Summary": {
				"currentValue": "185000.00",
				"Investment": {
					"Holdings": {
						"type": "DEMAT",
						"Holding": [
							{"issuerName": "RELIANCE", "isin": "INE002A01018", "units": 50, "lastTradedPrice": "2800.00"},
							{"issuerName": "INFOSYS", "isin": "INE009A01021", "units": 15, "lastTradedPrice": "1500.00"},
							{"issuerName": "TCS", "isin": "INE467B01029", "units": 10, "lastTradedPrice": "2250.00"}
						]
					}
				}
			},
			"Transactions": {
				"startDate": "2025-01-01",
				"endDate": "2025-03-31",
				"Transaction": [
					{
						"txnId": "EQ-A1",
						"transactionDateTime": "2025-02-04T10:00:00+05:30",
						"isin": "INE002A01018",
						"rate": "2750.00",
						"type": "BUY",
						"units": 20
					},
					{
						"txnId": "EQ-C1",
						"transactionDateTime": "2025-02-10T10:00:00+05:30",
						"isin": "INE467B01029",
						"rate": "2200.00",
						"type": "BUY",
						"units": 10
					}
				]
			}
		}
	}`

	// Forward window: adds BUY 5 on RELIANCE, BONUS 15 on INFY, BUY 5 on TCS.
	// Must NOT create new anchors for any already-initialized ISIN.
	forwardPayload := `{
		"fipId": "CDSL-FIP",
		"institutionName": "Zerodha Broking",
		"Account": {
			"type": "equities",
			"maskedDematId": "XXXXXXXXXXXX1122",
			"version": "1.0.0",
			"linkedAccRef": "DEMAT-ANCH-1",
			"Summary": {
				"currentValue": "225000.00",
				"Investment": {
					"Holdings": {
						"type": "DEMAT",
						"Holding": [
							{"issuerName": "RELIANCE", "isin": "INE002A01018", "units": 55, "lastTradedPrice": "2850.00"},
							{"issuerName": "INFOSYS", "isin": "INE009A01021", "units": 30, "lastTradedPrice": "1550.00"},
							{"issuerName": "TCS", "isin": "INE467B01029", "units": 15, "lastTradedPrice": "2300.00"}
						]
					}
				}
			},
			"Transactions": {
				"startDate": "2025-04-01",
				"endDate": "2025-06-30",
				"Transaction": [
					{
						"txnId": "EQ-A2",
						"transactionDateTime": "2025-04-05T11:00:00+05:30",
						"isin": "INE002A01018",
						"rate": "2800.00",
						"type": "BUY",
						"units": 5
					},
					{
						"txnId": "EQ-B2",
						"transactionDateTime": "2025-05-01T09:00:00+05:30",
						"isin": "INE009A01021",
						"type": "BONUS",
						"units": 15
					},
					{
						"txnId": "EQ-C2",
						"transactionDateTime": "2025-05-15T14:00:00+05:30",
						"isin": "INE467B01029",
						"rate": "2300.00",
						"type": "BUY",
						"units": 5
					}
				]
			}
		}
	}`

	enqueueJob(t, ctx, pool, &userID, "AA_EQUITIES", firstPayload)
	enqueueJob(t, ctx, pool, &userID, "AA_EQUITIES", forwardPayload)

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	for i := 0; i < 2; i++ {
		if claimed, err := proc.ProcessOne(ctx, "eq-anch-worker"); err != nil || !claimed {
			t.Fatalf("ProcessOne(%d): claimed=%v err=%v", i, claimed, err)
		}
	}

	// Verify exactly 2 OPENING_ANCHOR rows (RELIANCE +30, INFY +15; none for TCS).
	var anchorCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ledger_transactions WHERE txn_type = 'OPENING_ANCHOR'
	`).Scan(&anchorCount); err != nil {
		t.Fatalf("count anchors: %v", err)
	}
	if anchorCount != 2 {
		t.Fatalf("OPENING_ANCHOR count = %d, want 2", anchorCount)
	}

	// Verify RELIANCE anchor key, timestamp (T_0 - 1us), amount = 0.0000, units = 30.000000.
	var relKey, relAmt, relUnits string
	var relTS time.Time
	if err := pool.QueryRow(ctx, `
		SELECT lt.idempotency_key, lt.txn_timestamp, le.amount::text, le.units::text
		FROM ledger_transactions lt
		JOIN ledger_entries le ON le.transaction_id = lt.id
		WHERE lt.txn_type = 'OPENING_ANCHOR' AND lt.raw_metadata->>'isin' = 'INE002A01018'
	`).Scan(&relKey, &relTS, &relAmt, &relUnits); err != nil {
		t.Fatalf("query RELIANCE anchor: %v", err)
	}
	wantRelKey := aa.EquitiesAnchorIdempotencyKey("CDSL-FIP|DEMAT-ANCH-1", "INE002A01018")
	wantRelT0, _ := time.Parse(time.RFC3339, "2025-02-04T10:00:00+05:30")
	if relKey != wantRelKey || !relTS.Equal(wantRelT0.Add(-time.Microsecond)) || relAmt != "0.0000" || relUnits != "30.000000" {
		t.Fatalf("unexpected RELIANCE anchor: key=%q ts=%v amt=%s units=%s", relKey, relTS, relAmt, relUnits)
	}

	// Verify per-ISIN net Cost Basis and units after forward ingest.
	checkISIN := func(isin, wantCost, wantUnits string) {
		t.Helper()
		var cost, units string
		if err := pool.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(le.amount) FILTER (WHERE le.coa_account = 'ASSET:EQUITY_COST'), 0)::text,
				COALESCE(SUM(le.units) FILTER (WHERE le.coa_account = 'ASSET:EQUITY_COST'), 0)::text
			FROM ledger_transactions lt
			JOIN ledger_entries le ON le.transaction_id = lt.id
			WHERE lt.raw_metadata->>'isin' = $1
		`, isin).Scan(&cost, &units); err != nil {
			t.Fatalf("query ISIN %s: %v", isin, err)
		}
		if cost != wantCost || units != wantUnits {
			t.Fatalf("ISIN %s: got cost=%s units=%s, want cost=%s units=%s", isin, cost, units, wantCost, wantUnits)
		}
	}
	checkISIN("INE002A01018", "69000.0000", "55.000000")
	checkISIN("INE009A01021", "0.0000", "30.000000")
	checkISIN("INE467B01029", "33500.0000", "15.000000")

	// Backfill payload containing a transaction predating RELIANCE's anchor must fail
	// immediately to DEAD_LETTER (ERR_BACKFILL_BEFORE_ANCHOR) with zero DB mutations.
	backfillPayload := `{
		"fipId": "CDSL-FIP",
		"institutionName": "Zerodha Broking",
		"Account": {
			"type": "equities",
			"maskedDematId": "XXXXXXXXXXXX1122",
			"version": "1.0.0",
			"linkedAccRef": "DEMAT-ANCH-1",
			"Summary": {"currentValue": "999999.00"},
			"Transactions": {
				"startDate": "2025-01-01",
				"endDate": "2025-07-01",
				"Transaction": [
					{
						"txnId": "EQ-BF-OLD",
						"transactionDateTime": "2025-01-15T10:00:00+05:30",
						"isin": "INE002A01018",
						"rate": "2600.00",
						"type": "BUY",
						"units": 10
					},
					{
						"txnId": "EQ-BF-NEW-TCS",
						"transactionDateTime": "2025-06-20T10:00:00+05:30",
						"isin": "INE467B01029",
						"rate": "2400.00",
						"type": "BUY",
						"units": 5
					}
				]
			}
		}
	}`
	bfJobID := enqueueJob(t, ctx, pool, &userID, "AA_EQUITIES", backfillPayload)
	if claimed, err := proc.ProcessOne(ctx, "eq-anch-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne backfill: claimed=%v err=%v", claimed, err)
	}

	var status, errCode string
	var attempts int
	if err := pool.QueryRow(ctx, `
		SELECT status::text, COALESCE(last_error_code, ''), attempt_count
		FROM ingestion_jobs WHERE id = $1
	`, bfJobID).Scan(&status, &errCode, &attempts); err != nil {
		t.Fatalf("query backfill job: %v", err)
	}
	if status != "DEAD_LETTER" || errCode != "ERR_BACKFILL_BEFORE_ANCHOR" || attempts != 0 {
		t.Fatalf("expected DEAD_LETTER/ERR_BACKFILL_BEFORE_ANCHOR/0 attempts, got %s/%s/%d", status, errCode, attempts)
	}

	// Existing transaction count (7: 2 anchors + 5 txns) and reported_balance (225000.0000) must be unchanged.
	var totalTxns int
	var reportedBal string
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM ledger_transactions),
			reported_balance::text
		FROM accounts WHERE natural_key = 'CDSL-FIP|DEMAT-ANCH-1'
	`).Scan(&totalTxns, &reportedBal); err != nil {
		t.Fatalf("query post-backfill state: %v", err)
	}
	if totalTxns != 7 || reportedBal != "225000.0000" {
		t.Fatalf("backfill mutated state: totalTxns=%d reportedBal=%s", totalTxns, reportedBal)
	}

	// Reverse split (negative units on SPLIT) preserves Cost Basis while reducing units,
	// and omitting Summary.Investment.Holdings does not fabricate a negative anchor on a new ISIN.
	noHoldingsPayload := `{
		"fipId": "CDSL-FIP",
		"institutionName": "Zerodha Broking",
		"Account": {
			"type": "equities",
			"maskedDematId": "XXXXXXXXXXXX1122",
			"version": "1.0.0",
			"linkedAccRef": "DEMAT-ANCH-1",
			"Transactions": {
				"startDate": "2025-05-01",
				"endDate": "2025-06-30",
				"Transaction": [
					{
						"txnId": "EQ-RSPLIT-1",
						"transactionDateTime": "2025-06-01T09:00:00+05:30",
						"isin": "INE467B01029",
						"narration": "REVERSE SPLIT 3:1",
						"type": "SPLIT",
						"units": -10
					},
					{
						"txnId": "EQ-NEW-NOHOLDING",
						"transactionDateTime": "2025-06-05T10:00:00+05:30",
						"isin": "INE040A01034",
						"rate": "1600.00",
						"type": "BUY",
						"units": 10
					}
				]
			}
		}
	}`
	enqueueJob(t, ctx, pool, &userID, "AA_EQUITIES", noHoldingsPayload)
	if claimed, err := proc.ProcessOne(ctx, "eq-anch-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne noHoldingsPayload: claimed=%v err=%v", claimed, err)
	}

	// TCS: Cost Basis preserved at 33500.0000, units reduced from 15 to 5.
	checkISIN("INE467B01029", "33500.0000", "5.000000")
	// HDFCBANK (no Holdings block): Cost Basis 16000.0000, units 10.000000 (no fabricated negative anchor).
	checkISIN("INE040A01034", "16000.0000", "10.000000")

	var finalAnchorCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_transactions WHERE txn_type = 'OPENING_ANCHOR'`).Scan(&finalAnchorCount); err != nil {
		t.Fatalf("count final anchors: %v", err)
	}
	if finalAnchorCount != 2 {
		t.Fatalf("OPENING_ANCHOR count = %d, want 2 (no fabricated anchor)", finalAnchorCount)
	}
}
