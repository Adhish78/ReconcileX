// Package aa_test verifies ReBIT Account Aggregator (AA) DEPOSIT payload parsing,
// fixed-point sign mapping, deterministic idempotency keys, and schema error wrapping.
package aa_test

import (
	"errors"
	"testing"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/Adhish78/ReconcileX/internal/money"
)

// TestParseDepositValidPayload checks end-to-end normalization of a ReBIT DEPOSIT
// document containing both string-encoded and number-encoded monetary amounts.
func TestParseDepositValidPayload(t *testing.T) {
	raw := []byte(`{
		"fipId": "HDFC-FIP",
		"institutionName": "HDFC Bank",
		"Account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX4020",
			"version": "2.0.0",
			"linkedAccRef": "HDFC-REF-4020",
			"Summary": {
				"currentBalance": 125430.50,
				"currency": "INR",
				"balanceDateTime": "2026-06-15T14:20:10+05:30",
				"branch": "MG Road Bengaluru",
				"ifsc": "HDFC0000123"
			},
			"Transactions": {
				"startDate": "2026-06-01",
				"endDate": "2026-06-30",
				"Transaction": [
					{
						"txnId": "TXN-1001",
						"type": "CREDIT",
						"mode": "NEFT",
						"amount": "50000.00",
						"transactionalBalance": "125430.50",
						"transactionTimestamp": "2026-06-15T09:00:00+05:30",
						"valueDate": "2026-06-15T00:00:00+05:30",
						"narration": "NEFT/ACME CORP/SALARY",
						"reference": "UTR12345"
					},
					{
						"txnId": "TXN-1002",
						"type": "DEBIT",
						"mode": "UPI",
						"amount": 1500.25,
						"transactionalBalance": 123930.25,
						"transactionTimestamp": "2026-06-15T14:20:10+05:30",
						"valueDate": "2026-06-15",
						"narration": "UPI/DR/416712345678/SWIGGY",
						"reference": "416712345678"
					}
				]
			}
		}
	}`)

	// Parse the payload and verify account-level natural key and reported balance.
	parsed, err := aa.ParseDeposit(raw)
	if err != nil {
		t.Fatalf("ParseDeposit unexpected err: %v", err)
	}

	if parsed.NaturalKey != "HDFC-FIP|HDFC-REF-4020" {
		t.Fatalf("NaturalKey = %q, want HDFC-FIP|HDFC-REF-4020", parsed.NaturalKey)
	}
	if parsed.InstitutionName != "HDFC Bank" {
		t.Fatalf("InstitutionName = %q, want HDFC Bank", parsed.InstitutionName)
	}
	if parsed.ReportedBalance == nil || parsed.ReportedBalance.String() != "125430.5000" {
		t.Fatalf("ReportedBalance = %v, want 125430.5000", parsed.ReportedBalance)
	}
	if len(parsed.Transactions) != 2 {
		t.Fatalf("got %d txns, want 2", len(parsed.Transactions))
	}

	// CREDIT maps to positive signed amount on ASSET:BANK.
	credit := parsed.Transactions[0]
	wantCredit, _ := money.ParseDecimal4("50000.0000")
	if credit.SignedAmount != wantCredit {
		t.Fatalf("credit SignedAmount = %s, want %s", credit.SignedAmount, wantCredit)
	}
	if credit.IdempotencyKey == "" || credit.IdempotencyKey == parsed.Transactions[1].IdempotencyKey {
		t.Fatalf("expected distinct non-empty deterministic idempotency keys")
	}

	// Same txnId produces identical deterministic idempotency_key across re-ingests.
	if got := aa.DepositTxnIdempotencyKey("TXN-1001"); got != credit.IdempotencyKey {
		t.Fatalf("IdempotencyKey = %q, want %q", credit.IdempotencyKey, got)
	}

	// DEBIT maps to negative signed amount on ASSET:BANK.
	debit := parsed.Transactions[1]
	wantDebit, _ := money.ParseDecimal4("-1500.2500")
	if debit.SignedAmount != wantDebit {
		t.Fatalf("debit SignedAmount = %s, want %s", debit.SignedAmount, wantDebit)
	}
}

// TestParseDepositSchemaValidationErrors ensures every structural or semantic
// defect wraps aa.ErrSchemaValidation so workers route bad payloads to DEAD_LETTER.
func TestParseDepositSchemaValidationErrors(t *testing.T) {
	badPayloads := []struct {
		name string
		raw  string
	}{
		{"invalid json", `{not-json`},
		{"missing Account", `{}`},
		{"wrong account type", `{"Account":{"type":"equities","linkedAccRef":"REF-1"}}`},
		{"missing linkedAccRef", `{"Account":{"type":"deposit"}}`},
		{"non-INR currency", `{"Account":{"type":"deposit","linkedAccRef":"REF-1","Summary":{"currency":"USD","currentBalance":"10.00"}}}`},
		{"missing txnId", `{"Account":{"type":"deposit","linkedAccRef":"REF-1","Transactions":{"Transaction":[{"type":"CREDIT","amount":"100","transactionTimestamp":"2026-06-15T09:00:00Z"}]}}}`},
		{"invalid txn type", `{"Account":{"type":"deposit","linkedAccRef":"REF-1","Transactions":{"Transaction":[{"txnId":"T1","type":"HOLD","amount":"100","transactionTimestamp":"2026-06-15T09:00:00Z"}]}}}`},
		{"zero amount", `{"Account":{"type":"deposit","linkedAccRef":"REF-1","Transactions":{"Transaction":[{"txnId":"T1","type":"CREDIT","amount":"0","transactionTimestamp":"2026-06-15T09:00:00Z"}]}}}`},
		{"bad timestamp", `{"Account":{"type":"deposit","linkedAccRef":"REF-1","Transactions":{"Transaction":[{"txnId":"T1","type":"CREDIT","amount":"10","transactionTimestamp":"not-a-date"}]}}}`},
	}

	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := aa.ParseDeposit([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, aa.ErrSchemaValidation) {
				t.Fatalf("expected ErrSchemaValidation for %s, got %v", tc.name, err)
			}
		})
	}
}
