package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Adhish78/ReconcileX/internal/api"
	"github.com/Adhish78/ReconcileX/internal/worker"
	"github.com/Adhish78/ReconcileX/migrations"
)

// TestEquitiesIngestBackfillReplayAndHoldingsRead verifies the end-to-end API
// lifecycle for Issue #18:
//   - Ingest an AA_EQUITIES payload with per-ISIN Opening Anchor and BONUS/SPLIT Corporate Actions
//   - GET /api/v1/accounts returns EQUITY demat metadata, Cost Basis, units, and per-ISIN equity_holdings
//   - GET /api/v1/portfolio/summary reflects equities Cost Basis in assets & net worth
//   - Backfill predating an existing ISIN anchor lands in DEAD_LETTER (ERR_BACKFILL_BEFORE_ANCHOR)
//     and can be repaired and replayed via POST /api/v1/jobs/{id}/replay
func TestEquitiesIngestBackfillReplayAndHoldingsRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openTestPool(t, true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('aarav', 'Aarav Sharma', 'ACTIVE')`,
	); err != nil {
		t.Fatalf("insert aarav: %v", err)
	}

	srv := api.NewServer(pool, migrations.FS, stubWebFS())

	initialBody := `{
		"source_type": "AA_EQUITIES",
		"persona_slug": "aarav",
		"payload": {
			"fipId": "CDSL-FIP",
			"institutionName": "Zerodha Broking (CDSL)",
			"Account": {
				"type": "equities",
				"maskedDematId": "XXXXXXXXXXXX0987",
				"version": "1.0.0",
				"linkedAccRef": "DEMAT-API-1",
				"Summary": {
					"currentValue": "248000.00",
					"Investment": {
						"Holdings": {
							"type": "DEMAT",
							"Holding": [
								{
									"issuerName": "RELIANCE INDUSTRIES LTD",
									"isin": "INE002A01018",
									"isinDescription": "RELIANCE INDUSTRIES LTD EQ",
									"units": 60,
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
							"txnId": "EQ-API-REL-BUY",
							"companyName": "RELIANCE INDUSTRIES LTD",
							"transactionDateTime": "2025-02-10T10:00:00+05:30",
							"exchange": "NSE",
							"isin": "INE002A01018",
							"rate": "2500.00",
							"type": "BUY",
							"units": 20
						},
						{
							"txnId": "EQ-API-REL-BONUS",
							"companyName": "RELIANCE INDUSTRIES LTD",
							"transactionDateTime": "2025-04-01T09:00:00+05:30",
							"isin": "INE002A01018",
							"narration": "BONUS 1:2",
							"type": "BONUS",
							"units": 20
						},
						{
							"txnId": "EQ-API-TCS-BUY",
							"companyName": "TATA CONSULTANCY SERVICES LTD",
							"transactionDateTime": "2025-02-15T10:00:00+05:30",
							"exchange": "BSE",
							"isin": "INE467B01029",
							"rate": "3800.00",
							"type": "BUY",
							"units": 10
						},
						{
							"txnId": "EQ-API-TCS-SPLIT",
							"companyName": "TATA CONSULTANCY SERVICES LTD",
							"transactionDateTime": "2025-05-01T09:00:00+05:30",
							"isin": "INE467B01029",
							"narration": "SPLIT 1:2",
							"type": "SPLIT",
							"units": 10
						}
					]
				}
			}
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewBufferString(initialBody))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/ingest expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	if claimed, err := proc.ProcessOne(ctx, "api-eq-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne equities: claimed=%v err=%v", claimed, err)
	}

	// Step 2: Verify GET /api/v1/accounts returns EQUITY demat account and per-ISIN holdings.
	acctsReq := httptest.NewRequest(http.MethodGet, "/api/v1/accounts?persona=aarav", nil)
	acctsRec := httptest.NewRecorder()
	srv.ServeHTTP(acctsRec, acctsReq)
	if acctsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/accounts expected 200, got %d", acctsRec.Code)
	}
	var acctsResp struct {
		Accounts []struct {
			ID              string  `json:"id"`
			SourceType      string  `json:"source_type"`
			MaskedAccNumber string  `json:"masked_acc_number"`
			LedgerBalance   string  `json:"ledger_balance"`
			LedgerUnits     *string `json:"ledger_units"`
			ReportedBalance *string `json:"reported_balance"`
			ReportedUnits   *string `json:"reported_units"`
			EquityHoldings  []struct {
				ISIN            string  `json:"isin"`
				IssuerName      string  `json:"issuer_name"`
				CostBasis       string  `json:"cost_basis"`
				LedgerUnits     string  `json:"ledger_units"`
				ReportedUnits   *string `json:"reported_units"`
				LastTradedPrice *string `json:"last_traded_price"`
			} `json:"equity_holdings"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(acctsRec.Body.Bytes(), &acctsResp); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(acctsResp.Accounts) != 1 {
		t.Fatalf("expected 1 EQUITY account, got %d", len(acctsResp.Accounts))
	}
	eq := acctsResp.Accounts[0]
	if eq.SourceType != "EQUITY" ||
		eq.MaskedAccNumber != "XXXXXXXXXXXX0987" ||
		eq.LedgerBalance != "88000.0000" ||
		eq.LedgerUnits == nil || *eq.LedgerUnits != "80.000000" ||
		eq.ReportedBalance == nil || *eq.ReportedBalance != "248000.0000" {
		t.Fatalf("unexpected EQUITY account summary: %+v", eq)
	}
	if len(eq.EquityHoldings) != 2 {
		t.Fatalf("expected 2 per-ISIN equity_holdings, got %d: %+v", len(eq.EquityHoldings), eq.EquityHoldings)
	}
	rel := eq.EquityHoldings[0]
	if rel.ISIN != "INE002A01018" || rel.CostBasis != "50000.0000" || rel.LedgerUnits != "60.000000" ||
		rel.ReportedUnits == nil || *rel.ReportedUnits != "60.000000" ||
		rel.LastTradedPrice == nil || *rel.LastTradedPrice != "2800.0000" {
		t.Fatalf("unexpected RELIANCE holding: %+v", rel)
	}
	tcs := eq.EquityHoldings[1]
	if tcs.ISIN != "INE467B01029" || tcs.CostBasis != "38000.0000" || tcs.LedgerUnits != "20.000000" {
		t.Fatalf("unexpected TCS holding: %+v", tcs)
	}

	// Step 3: Verify GET /api/v1/portfolio/summary reflects equities Cost Basis.
	sumReq := httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/summary?persona=aarav", nil)
	sumRec := httptest.NewRecorder()
	srv.ServeHTTP(sumRec, sumReq)
	if sumRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/portfolio/summary expected 200, got %d", sumRec.Code)
	}
	var sumResp struct {
		NetWorth        string `json:"net_worth"`
		TotalAssets     string `json:"total_assets"`
		AccountsSummary struct {
			Equities string `json:"equities"`
		} `json:"accounts_summary"`
	}
	if err := json.Unmarshal(sumRec.Body.Bytes(), &sumResp); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sumResp.AccountsSummary.Equities != "88000.0000" || sumResp.TotalAssets != "88000.0000" || sumResp.NetWorth != "88000.0000" {
		t.Fatalf("unexpected portfolio summary: %+v", sumResp)
	}

	// Step 4: Backfill predating RELIANCE anchor -> DEAD_LETTER (ERR_BACKFILL_BEFORE_ANCHOR) -> repair & replay.
	backfillBody := `{
		"source_type": "AA_EQUITIES",
		"persona_slug": "aarav",
		"payload": {
			"fipId": "CDSL-FIP",
			"institutionName": "Zerodha Broking (CDSL)",
			"Account": {
				"type": "equities",
				"maskedDematId": "XXXXXXXXXXXX0987",
				"version": "1.0.0",
				"linkedAccRef": "DEMAT-API-1",
				"Transactions": {
					"startDate": "2025-01-01",
					"endDate": "2025-06-30",
					"Transaction": [{
						"txnId": "EQ-API-BF-1",
						"transactionDateTime": "2025-01-15T10:00:00+05:30",
						"isin": "INE002A01018",
						"rate": "2400.00",
						"type": "BUY",
						"units": 5
					}]
				}
			}
		}
	}`
	bfReq := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewBufferString(backfillBody))
	bfRec := httptest.NewRecorder()
	srv.ServeHTTP(bfRec, bfReq)
	if bfRec.Code != http.StatusAccepted {
		t.Fatalf("POST backfill expected 202, got %d", bfRec.Code)
	}
	var bfEnq struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(bfRec.Body.Bytes(), &bfEnq); err != nil {
		t.Fatalf("decode backfill ingest: %v", err)
	}

	if claimed, err := proc.ProcessOne(ctx, "api-eq-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne backfill: claimed=%v err=%v", claimed, err)
	}

	jobReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+bfEnq.JobID+"?persona=aarav", nil)
	jobRec := httptest.NewRecorder()
	srv.ServeHTTP(jobRec, jobReq)
	var jobResp struct {
		Job struct {
			Status        string  `json:"status"`
			LastErrorCode *string `json:"last_error_code"`
		} `json:"job"`
	}
	if err := json.Unmarshal(jobRec.Body.Bytes(), &jobResp); err != nil {
		t.Fatalf("decode job detail: %v", err)
	}
	if jobResp.Job.Status != "DEAD_LETTER" || jobResp.Job.LastErrorCode == nil || *jobResp.Job.LastErrorCode != "ERR_BACKFILL_BEFORE_ANCHOR" {
		t.Fatalf("expected DEAD_LETTER/ERR_BACKFILL_BEFORE_ANCHOR, got %+v", jobResp.Job)
	}

	// Repair the timestamp to post-anchor and replay via POST /api/v1/jobs/{id}/replay.
	repairedBody := `{
		"payload": {
			"fipId": "CDSL-FIP",
			"institutionName": "Zerodha Broking (CDSL)",
			"Account": {
				"type": "equities",
				"maskedDematId": "XXXXXXXXXXXX0987",
				"version": "1.0.0",
				"linkedAccRef": "DEMAT-API-1",
				"Transactions": {
					"startDate": "2025-02-01",
					"endDate": "2025-06-30",
					"Transaction": [{
						"txnId": "EQ-API-BF-1",
						"transactionDateTime": "2025-03-01T10:00:00+05:30",
						"isin": "INE002A01018",
						"rate": "2400.00",
						"type": "BUY",
						"units": 5
					}]
				}
			}
		}
	}`
	repReq := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+bfEnq.JobID+"/replay?persona=aarav", bytes.NewBufferString(repairedBody))
	repRec := httptest.NewRecorder()
	srv.ServeHTTP(repRec, repReq)
	if repRec.Code != http.StatusAccepted {
		t.Fatalf("POST replay expected 202, got %d: %s", repRec.Code, repRec.Body.String())
	}
	if claimed, err := proc.ProcessOne(ctx, "api-eq-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne repaired equities: claimed=%v err=%v", claimed, err)
	}

	// Verify Cost Basis and units updated cleanly (50000 + 5*2400 = 62000.0000, units = 65.000000).
	acctsRec2 := httptest.NewRecorder()
	srv.ServeHTTP(acctsRec2, httptest.NewRequest(http.MethodGet, "/api/v1/accounts?persona=aarav", nil))
	if err := json.Unmarshal(acctsRec2.Body.Bytes(), &acctsResp); err != nil {
		t.Fatalf("decode accounts after replay: %v", err)
	}
	relAfter := acctsResp.Accounts[0].EquityHoldings[0]
	if relAfter.CostBasis != "62000.0000" || relAfter.LedgerUnits != "65.000000" {
		t.Fatalf("unexpected RELIANCE holding after replay: %+v", relAfter)
	}
}
