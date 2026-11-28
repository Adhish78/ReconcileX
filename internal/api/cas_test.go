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

// TestCASIngestUnitChecksumDLQReplayAndHoldingsRead verifies the end-to-end API
// lifecycle for Issue #17:
//   - Ingest a CAS payload failing pre-ledger Unit Checksum -> DEAD_LETTER (ERR_UNIT_CHECKSUM)
//   - Repair the payload via POST /api/v1/jobs/{id}/replay -> worker completes ingestion
//   - GET /api/v1/accounts returns MUTUAL_FUND Folio metadata, Cost Basis, and unit totals
//   - GET /api/v1/portfolio/summary reflects mutual_funds Cost Basis in assets & net worth
func TestCASIngestUnitChecksumDLQReplayAndHoldingsRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openTestPool(t, true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('aarav', 'Aarav Sharma', 'ACTIVE')`,
	); err != nil {
		t.Fatalf("insert aarav: %v", err)
	}

	srv := api.NewServer(pool, migrations.FS, stubWebFS())

	// Step 1: Ingest corrupt CAS statement (open=0 + purchase=50.000 != close=99.000).
	corruptBody := `{
		"source_type": "CAS",
		"persona_slug": "aarav",
		"payload": {
			"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
			"file_type": "CAMS",
			"cas_type": "DETAILED",
			"investor_info": {"name": "Aarav Sharma"},
			"folios": [{
				"folio": "12345678 / 90",
				"amc": "PPFAS Mutual Fund",
				"schemes": [{
					"scheme": "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
					"rta": "CAMS",
					"isin": "INF879O01027",
					"amfi": "122639",
					"open": "0.000",
					"close": "99.000",
					"valuation": {"date": "2025-03-31", "nav": "96.5000", "value": "9553.50"},
					"transactions": [
						{"date": "2024-04-05", "description": "SIP Purchase", "amount": "4500.00", "units": "50.000", "type": "PURCHASE_SIP"}
					]
				}]
			}]
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewBufferString(corruptBody))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/ingest expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var enq struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enq); err != nil {
		t.Fatalf("decode ingest resp: %v", err)
	}

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	if claimed, err := proc.ProcessOne(ctx, "api-cas-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne corrupt CAS: claimed=%v err=%v", claimed, err)
	}

	// Verify job landed in DEAD_LETTER with ERR_UNIT_CHECKSUM.
	jobReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+enq.JobID+"?persona=aarav", nil)
	jobRec := httptest.NewRecorder()
	srv.ServeHTTP(jobRec, jobReq)
	if jobRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs/{id} expected 200, got %d", jobRec.Code)
	}
	var jobResp struct {
		Job struct {
			Status        string  `json:"status"`
			LastErrorCode *string `json:"last_error_code"`
		} `json:"job"`
	}
	if err := json.Unmarshal(jobRec.Body.Bytes(), &jobResp); err != nil {
		t.Fatalf("decode job detail: %v", err)
	}
	if jobResp.Job.Status != "DEAD_LETTER" || jobResp.Job.LastErrorCode == nil || *jobResp.Job.LastErrorCode != "ERR_UNIT_CHECKSUM" {
		t.Fatalf("expected DEAD_LETTER/ERR_UNIT_CHECKSUM, got %+v", jobResp.Job)
	}

	// Step 2: Repair close to "50.000" and replay via POST /api/v1/jobs/{id}/replay.
	repairedBody := `{
		"payload": {
			"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
			"file_type": "CAMS",
			"cas_type": "DETAILED",
			"investor_info": {"name": "Aarav Sharma"},
			"folios": [{
				"folio": "12345678 / 90",
				"amc": "PPFAS Mutual Fund",
				"schemes": [{
					"scheme": "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
					"rta": "CAMS",
					"isin": "INF879O01027",
					"amfi": "122639",
					"open": "0.000",
					"close": "50.000",
					"valuation": {"date": "2025-03-31", "nav": "96.5000", "value": "4825.00"},
					"transactions": [
						{"date": "2024-04-05", "description": "SIP Purchase", "amount": "4500.00", "units": "50.000", "type": "PURCHASE_SIP"}
					]
				}]
			}]
		}
	}`
	repReq := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+enq.JobID+"/replay?persona=aarav", bytes.NewBufferString(repairedBody))
	repRec := httptest.NewRecorder()
	srv.ServeHTTP(repRec, repReq)
	if repRec.Code != http.StatusAccepted {
		t.Fatalf("POST replay expected 202, got %d: %s", repRec.Code, repRec.Body.String())
	}

	if claimed, err := proc.ProcessOne(ctx, "api-cas-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne repaired CAS: claimed=%v err=%v", claimed, err)
	}

	// Step 3: Verify GET /api/v1/accounts returns MUTUAL_FUND Folio fields, Cost Basis, and units.
	acctsReq := httptest.NewRequest(http.MethodGet, "/api/v1/accounts?persona=aarav", nil)
	acctsRec := httptest.NewRecorder()
	srv.ServeHTTP(acctsRec, acctsReq)
	if acctsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/accounts expected 200, got %d", acctsRec.Code)
	}
	var acctsResp struct {
		Accounts []struct {
			SourceType      string  `json:"source_type"`
			InstitutionName string  `json:"institution_name"`
			FolioNumber     string  `json:"folio_number"`
			ISIN            string  `json:"isin"`
			SchemeCode      *int    `json:"scheme_code"`
			SchemeName      string  `json:"scheme_name"`
			LedgerBalance   string  `json:"ledger_balance"`
			LedgerUnits     *string `json:"ledger_units"`
			ReportedUnits   *string `json:"reported_units"`
			ReportedBalance *string `json:"reported_balance"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(acctsRec.Body.Bytes(), &acctsResp); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(acctsResp.Accounts) != 1 {
		t.Fatalf("expected 1 MUTUAL_FUND Financial Account, got %d", len(acctsResp.Accounts))
	}
	mf := acctsResp.Accounts[0]
	if mf.SourceType != "MUTUAL_FUND" ||
		mf.FolioNumber != "12345678 / 90" ||
		mf.ISIN != "INF879O01027" ||
		mf.SchemeCode == nil || *mf.SchemeCode != 122639 ||
		mf.SchemeName != "Parag Parikh Flexi Cap Fund - Direct Plan - Growth" ||
		mf.LedgerBalance != "4500.0000" ||
		mf.LedgerUnits == nil || *mf.LedgerUnits != "50.000000" ||
		mf.ReportedUnits == nil || *mf.ReportedUnits != "50.000000" {
		t.Fatalf("unexpected Folio holding summary: %+v", mf)
	}

	// Step 4: Verify GET /api/v1/portfolio/summary reflects mutual_funds Cost Basis.
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
			MutualFunds string `json:"mutual_funds"`
		} `json:"accounts_summary"`
	}
	if err := json.Unmarshal(sumRec.Body.Bytes(), &sumResp); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sumResp.AccountsSummary.MutualFunds != "4500.0000" || sumResp.TotalAssets != "4500.0000" || sumResp.NetWorth != "4500.0000" {
		t.Fatalf("unexpected portfolio summary: %+v", sumResp)
	}
}
