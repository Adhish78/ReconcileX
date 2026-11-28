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

// TestLiabilityIngestNetWorthAndReplay verifies the end-to-end API lifecycle for Issue #19:
//   - Ingest an AA_DEPOSIT asset + HOME_LOAN and AUTO_LOAN Liability Accounts with split EMI_REPAYMENT
//   - GET /api/v1/accounts returns non-negative Outstanding Balance magnitude in ledger_balance
//   - GET /api/v1/accounts/{id}/transactions returns multi-leg EMI_REPAYMENT Chart of Accounts entries
//   - GET /api/v1/portfolio/summary subtracts loan Outstanding Balances from Net Worth
//   - Invalid EMI split lands in DEAD_LETTER (ERR_INVALID_EMI_SPLIT) and replays cleanly after repair
func TestLiabilityIngestNetWorthAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool := openTestPool(t, true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('aarav', 'Aarav Sharma', 'ACTIVE')`,
	); err != nil {
		t.Fatalf("insert aarav: %v", err)
	}

	srv := api.NewServer(pool, migrations.FS, stubWebFS())

	depositBody := `{
		"source_type": "AA_DEPOSIT",
		"persona_slug": "aarav",
		"payload": {
			"fipId": "HDFC-FIP",
			"institutionName": "HDFC Bank",
			"Account": {
				"type": "deposit",
				"maskedAccNumber": "XXXXXXXX4020",
				"version": "2.0.0",
				"linkedAccRef": "HDFC-4020",
				"Summary": {"currentBalance": "1000000.00", "currency": "INR"},
				"Transactions": {
					"Transaction": [{
						"txnId": "DEP-LOAN-1",
						"type": "CREDIT",
						"mode": "NEFT",
						"amount": "1000000.00",
						"transactionTimestamp": "2026-05-01T09:00:00+05:30",
						"narration": "NEFT/SALARY"
					}]
				}
			}
		}
	}`

	homeLoanBody := `{
		"source_type": "LIABILITY",
		"persona_slug": "aarav",
		"payload": {
			"fipId": "HDFC-LOAN-FIP",
			"institutionName": "HDFC Home Loans",
			"Account": {
				"type": "liability",
				"subType": "HOME_LOAN",
				"version": "1.0.0",
				"maskedAccNumber": "HL-XXXX-8812",
				"linkedAccRef": "HL-API-8812",
				"Summary": {
					"sanctionedPrincipal": "800000.00",
					"outstandingBalance": "600000.00",
					"currency": "INR",
					"balanceDateTime": "2026-06-05T18:00:00+05:30",
					"status": "ACTIVE"
				},
				"Transactions": {
					"Transaction": [{
						"txnId": "HL-API-EMI-1",
						"type": "EMI_REPAYMENT",
						"amount": "25500.00",
						"principalComponent": "15000.00",
						"interestComponent": "10000.00",
						"feeComponent": "500.00",
						"transactionalBalance": "600000.00",
						"transactionTimestamp": "2026-06-05T09:00:00+05:30",
						"narration": "ACH/DR/HDFC HOME LOAN EMI"
					}]
				}
			}
		}
	}`

	autoLoanBody := `{
		"source_type": "LIABILITY",
		"persona_slug": "aarav",
		"payload": {
			"fipId": "ICICI-AUTO-FIP",
			"institutionName": "ICICI Auto Loans",
			"Account": {
				"type": "liability",
				"subType": "AUTO_LOAN",
				"version": "1.0.0",
				"maskedAccNumber": "AL-XXXX-3301",
				"linkedAccRef": "AL-API-3301",
				"Summary": {
					"outstandingBalance": "250000.00",
					"currency": "INR",
					"status": "ACTIVE"
				},
				"Transactions": {
					"startDate": "2026-06-01",
					"Transaction": []
				}
			}
		}
	}`

	for _, body := range []string{depositBody, homeLoanBody, autoLoanBody} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /api/v1/ingest expected 202, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	proc := worker.NewPool(pool, worker.Config{Concurrency: 1})
	for i := 0; i < 3; i++ {
		if claimed, err := proc.ProcessOne(ctx, "api-loan-worker"); err != nil || !claimed {
			t.Fatalf("ProcessOne(%d): claimed=%v err=%v", i, claimed, err)
		}
	}

	// Verify GET /api/v1/accounts returns non-negative Outstanding Balances for loans.
	acctsRec := httptest.NewRecorder()
	srv.ServeHTTP(acctsRec, httptest.NewRequest(http.MethodGet, "/api/v1/accounts?persona=aarav", nil))
	if acctsRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/accounts expected 200, got %d", acctsRec.Code)
	}
	var acctsResp struct {
		Accounts []struct {
			ID              string  `json:"id"`
			SourceType      string  `json:"source_type"`
			LedgerBalance   string  `json:"ledger_balance"`
			ReportedBalance *string `json:"reported_balance"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(acctsRec.Body.Bytes(), &acctsResp); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(acctsResp.Accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(acctsResp.Accounts))
	}
	var homeLoanID string
	for _, a := range acctsResp.Accounts {
		switch a.SourceType {
		case "HOME_LOAN":
			homeLoanID = a.ID
			if a.LedgerBalance != "600000.0000" || a.ReportedBalance == nil || *a.ReportedBalance != "600000.0000" {
				t.Fatalf("unexpected HOME_LOAN account: %+v", a)
			}
		case "AUTO_LOAN":
			if a.LedgerBalance != "250000.0000" || a.ReportedBalance == nil || *a.ReportedBalance != "250000.0000" {
				t.Fatalf("unexpected AUTO_LOAN account: %+v", a)
			}
		}
	}
	if homeLoanID == "" {
		t.Fatalf("HOME_LOAN account missing from /api/v1/accounts")
	}

	// Verify transaction drilldown on HOME_LOAN includes the 4-leg EMI_REPAYMENT.
	txnsRec := httptest.NewRecorder()
	srv.ServeHTTP(txnsRec, httptest.NewRequest(http.MethodGet, "/api/v1/accounts/"+homeLoanID+"/transactions?persona=aarav", nil))
	if txnsRec.Code != http.StatusOK {
		t.Fatalf("GET transactions expected 200, got %d", txnsRec.Code)
	}
	var txnsResp struct {
		Transactions []struct {
			TxnType string `json:"txn_type"`
			Entries []struct {
				CoaAccount string `json:"coa_account"`
				Amount     string `json:"amount"`
			} `json:"entries"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(txnsRec.Body.Bytes(), &txnsResp); err != nil {
		t.Fatalf("decode transactions: %v", err)
	}
	if len(txnsResp.Transactions) != 2 || txnsResp.Transactions[0].TxnType != "EMI_REPAYMENT" || len(txnsResp.Transactions[0].Entries) != 4 {
		t.Fatalf("unexpected HOME_LOAN transactions: %+v", txnsResp.Transactions)
	}

	// Verify GET /api/v1/portfolio/summary subtracts 850,000 from 1,000,000 -> Net Worth 150,000.0000.
	sumRec := httptest.NewRecorder()
	srv.ServeHTTP(sumRec, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/summary?persona=aarav", nil))
	if sumRec.Code != http.StatusOK {
		t.Fatalf("GET summary expected 200, got %d", sumRec.Code)
	}
	var sumResp struct {
		NetWorth         string `json:"net_worth"`
		TotalAssets      string `json:"total_assets"`
		TotalLiabilities string `json:"total_liabilities"`
		AccountsSummary  struct {
			BankDeposits string `json:"bank_deposits"`
			Loans        string `json:"loans"`
		} `json:"accounts_summary"`
	}
	if err := json.Unmarshal(sumRec.Body.Bytes(), &sumResp); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sumResp.TotalAssets != "1000000.0000" ||
		sumResp.AccountsSummary.Loans != "850000.0000" ||
		sumResp.TotalLiabilities != "850000.0000" ||
		sumResp.NetWorth != "150000.0000" {
		t.Fatalf("unexpected portfolio summary with loans: %+v", sumResp)
	}

	// Invalid EMI split lands in DEAD_LETTER (ERR_INVALID_EMI_SPLIT), repairs via replay, and reduces balance.
	badSplitBody := `{
		"source_type": "LIABILITY",
		"persona_slug": "aarav",
		"payload": {
			"fipId": "ICICI-AUTO-FIP",
			"institutionName": "ICICI Auto Loans",
			"Account": {
				"type": "liability",
				"subType": "AUTO_LOAN",
				"version": "1.0.0",
				"linkedAccRef": "AL-API-3301",
				"Summary": {"outstandingBalance": "240000.00", "currency": "INR"},
				"Transactions": {
					"Transaction": [{
						"txnId": "AL-API-EMI-BAD",
						"type": "EMI_REPAYMENT",
						"amount": "15000.00",
						"principalComponent": "10000.00",
						"interestComponent": "4000.00",
						"transactionTimestamp": "2026-06-15T09:00:00+05:30"
					}]
				}
			}
		}
	}`
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewBufferString(badSplitBody)))
	var badEnq struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(badRec.Body.Bytes(), &badEnq)

	if claimed, err := proc.ProcessOne(ctx, "api-loan-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne bad split: claimed=%v err=%v", claimed, err)
	}

	jobRec := httptest.NewRecorder()
	srv.ServeHTTP(jobRec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+badEnq.JobID+"?persona=aarav", nil))
	var jobResp struct {
		Job struct {
			Status        string  `json:"status"`
			LastErrorCode *string `json:"last_error_code"`
		} `json:"job"`
	}
	_ = json.Unmarshal(jobRec.Body.Bytes(), &jobResp)
	if jobResp.Job.Status != "DEAD_LETTER" || jobResp.Job.LastErrorCode == nil || *jobResp.Job.LastErrorCode != "ERR_INVALID_EMI_SPLIT" {
		t.Fatalf("expected DEAD_LETTER/ERR_INVALID_EMI_SPLIT, got %+v", jobResp.Job)
	}

	repairedBody := `{
		"payload": {
			"fipId": "ICICI-AUTO-FIP",
			"institutionName": "ICICI Auto Loans",
			"Account": {
				"type": "liability",
				"subType": "AUTO_LOAN",
				"version": "1.0.0",
				"linkedAccRef": "AL-API-3301",
				"Summary": {"outstandingBalance": "240000.00", "currency": "INR"},
				"Transactions": {
					"Transaction": [{
						"txnId": "AL-API-EMI-BAD",
						"type": "EMI_REPAYMENT",
						"amount": "15000.00",
						"principalComponent": "10000.00",
						"interestComponent": "5000.00",
						"transactionTimestamp": "2026-06-15T09:00:00+05:30"
					}]
				}
			}
		}
	}`
	repRec := httptest.NewRecorder()
	srv.ServeHTTP(repRec, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+badEnq.JobID+"/replay?persona=aarav", bytes.NewBufferString(repairedBody)))
	if repRec.Code != http.StatusAccepted {
		t.Fatalf("POST replay expected 202, got %d: %s", repRec.Code, repRec.Body.String())
	}
	if claimed, err := proc.ProcessOne(ctx, "api-loan-worker"); err != nil || !claimed {
		t.Fatalf("ProcessOne repaired loan: claimed=%v err=%v", claimed, err)
	}

	sumRec2 := httptest.NewRecorder()
	srv.ServeHTTP(sumRec2, httptest.NewRequest(http.MethodGet, "/api/v1/portfolio/summary?persona=aarav", nil))
	_ = json.Unmarshal(sumRec2.Body.Bytes(), &sumResp)
	if sumResp.AccountsSummary.Loans != "840000.0000" || sumResp.NetWorth != "160000.0000" {
		t.Fatalf("expected loans=840000.0000 net_worth=160000.0000 after replay, got %+v", sumResp)
	}
}
