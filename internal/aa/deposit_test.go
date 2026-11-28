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

// TestParseDepositNormalizationQuirks verifies lowercase vs PascalCase keys,
// single-object vs array multiplicity on Holder/Transaction, numeric vs string
// fields, optional/masked Holder attributes, chronological T_0 sorting, and
// empty transaction windows.
func TestParseDepositNormalizationQuirks(t *testing.T) {
	// Case 1: Lowercase keys, single-object Holder & Transaction, numeric mobile/decimals, masked PAN.
	singleLower := []byte(`{
		"fipId": "ICICI-FIP",
		"institutionName": "ICICI Bank",
		"account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX9012",
			"version": "2.0.0",
			"linkedAccRef": "ICICI-9012",
			"profile": {
				"holders": {
					"type": "SINGLE",
					"holder": {
						"name": "Aarav Sharma",
						"dob": "1992-04-10",
						"mobile": 9876543210,
						"ckycRegistered": "YES",
						"pan": "XXXXXX123F"
					}
				}
			},
			"summary": {
				"currentBalance": "25000.00",
				"currency": "INR",
				"balanceDateTime": "2026-06-10T12:00:00+05:30",
				"accountType": "REGULAR",
				"accountSubType": "SAVINGS",
				"branch": "Indiranagar",
				"facility": "NO_FACILITY_GRANTED",
				"ifsc": "ICIC0000123"
			},
			"transactions": {
				"startDate": "2026-06-01",
				"endDate": "2026-06-10",
				"transaction": {
					"txnId": "SINGLE-1",
					"type": "CREDIT",
					"mode": "UPI",
					"amount": 5000,
					"transactionalBalance": 25000.00,
					"transactionTimestamp": "2026-06-05T11:30:00+05:30",
					"narration": "UPI/CR/FRIEND",
					"reference": "123456789012"
				}
			}
		}
	}`)

	parsed, err := aa.ParseDeposit(singleLower)
	if err != nil {
		t.Fatalf("ParseDeposit(singleLower) unexpected err: %v", err)
	}
	if len(parsed.Holders) != 1 || parsed.Holders[0].Name != "Aarav Sharma" || parsed.Holders[0].Mobile != "9876543210" {
		t.Fatalf("unexpected normalized Holders: %+v", parsed.Holders)
	}
	if parsed.Holders[0].PAN != "XXXXXX123F" {
		t.Fatalf("expected masked PAN preserved on Holder, got %q", parsed.Holders[0].PAN)
	}
	if len(parsed.Transactions) != 1 {
		t.Fatalf("expected 1 single-object transaction, got %d", len(parsed.Transactions))
	}
	if parsed.Transactions[0].TransactionalBalance == nil || parsed.Transactions[0].TransactionalBalance.String() != "25000.0000" {
		t.Fatalf("expected TransactionalBalance 25000.0000, got %v", parsed.Transactions[0].TransactionalBalance)
	}
	if parsed.Transactions[0].Reference != "123456789012" {
		t.Fatalf("expected reference preserved, got %q", parsed.Transactions[0].Reference)
	}
	if parsed.TransactionsStartDate == nil || parsed.TransactionsStartDate.Format("2006-01-02Z07:00") != "2026-06-01+05:30" {
		t.Fatalf("expected TransactionsStartDate 2026-06-01T00:00:00+05:30, got %v", parsed.TransactionsStartDate)
	}

	// Case 2: Reverse-chronological transactions are sorted earliest-first so Transactions[0] is T_0,
	// and JOINT holders with masked/omitted optional fields parse cleanly.
	reverseJoint := []byte(`{
		"fipId": "SBI-FIP",
		"Account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX3344",
			"version": "2.0.0",
			"linkedAccRef": "SBI-3344",
			"Profile": {
				"Holders": {
					"type": "JOINT",
					"Holder": [
						{"name": "Primary Holder", "mobile": "XXXXXX3210"},
						{"name": "Secondary Holder", "email": "s***@example.com"}
					]
				}
			},
			"Summary": {
				"currentBalance": 12000.00,
				"currency": "INR",
				"balanceDateTime": "2026-06-20T18:00:00+05:30",
				"branch": "SBI Main"
			},
			"Transactions": {
				"startDate": "2026-06-01",
				"endDate": "2026-06-20",
				"Transaction": [
					{
						"txnId": "LATER",
						"type": "DEBIT",
						"mode": "ATM",
						"amount": "1000.00",
						"transactionalBalance": "12000.00",
						"transactionTimestamp": "2026-06-10T15:00:00+05:30",
						"narration": "ATM WDL"
					},
					{
						"txnId": "EARLIEST",
						"type": "CREDIT",
						"mode": "NEFT",
						"amount": "3000.00",
						"transactionalBalance": "13000.00",
						"transactionTimestamp": "2026-06-02T09:00:00+05:30",
						"narration": "NEFT IN"
					}
				]
			}
		}
	}`)

	parsedJoint, err := aa.ParseDeposit(reverseJoint)
	if err != nil {
		t.Fatalf("ParseDeposit(reverseJoint) unexpected err: %v", err)
	}
	if parsedJoint.HoldersType != "JOINT" || len(parsedJoint.Holders) != 2 {
		t.Fatalf("unexpected joint holders: type=%q holders=%+v", parsedJoint.HoldersType, parsedJoint.Holders)
	}
	if len(parsedJoint.Transactions) != 2 || parsedJoint.Transactions[0].TxnID != "EARLIEST" {
		t.Fatalf("expected chronological sort with EARLIEST first, got %+v", parsedJoint.Transactions)
	}

	// Case 3: Empty transaction list is valid and carries Summary.currentBalance + startDate.
	emptyTxns := []byte(`{
		"fipId": "AXIS-FIP",
		"institutionName": "Axis Bank",
		"Account": {
			"type": "deposit",
			"maskedAccNumber": "XXXXXXXX7788",
			"version": "2.0.0",
			"linkedAccRef": "AXIS-7788",
			"Summary": {
				"currentBalance": "42500.75",
				"currency": "INR",
				"balanceDateTime": "2026-06-30T23:59:59+05:30"
			},
			"Transactions": {
				"startDate": "2026-06-01",
				"endDate": "2026-06-30",
				"Transaction": []
			}
		}
	}`)

	parsedEmpty, err := aa.ParseDeposit(emptyTxns)
	if err != nil {
		t.Fatalf("ParseDeposit(emptyTxns) unexpected err: %v", err)
	}
	if len(parsedEmpty.Transactions) != 0 {
		t.Fatalf("expected 0 transactions, got %d", len(parsedEmpty.Transactions))
	}
	if parsedEmpty.ReportedBalance == nil || parsedEmpty.ReportedBalance.String() != "42500.7500" {
		t.Fatalf("expected ReportedBalance 42500.7500, got %v", parsedEmpty.ReportedBalance)
	}
}

// TestDepositAnchorIdempotencyKey verifies deterministic SHA-256 hashing for Opening Anchors.
func TestDepositAnchorIdempotencyKey(t *testing.T) {
	k1 := aa.DepositAnchorIdempotencyKey("HDFC-FIP|HDFC-REF-4020")
	k2 := aa.DepositAnchorIdempotencyKey(" HDFC-FIP|HDFC-REF-4020 ")
	k3 := aa.DepositAnchorIdempotencyKey("ICICI-FIP|ICICI-9012")
	if len(k1) != 64 || k1 != k2 || k1 == k3 {
		t.Fatalf("unexpected anchor idempotency keys: k1=%q k2=%q k3=%q", k1, k2, k3)
	}
}

// FuzzParseAADeposit fuzz-tests ReBIT AA DEPOSIT parsing against arbitrary byte
// sequences, asserting zero panics and basic structural invariants on valid parses.
func FuzzParseAADeposit(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"fipId":"HDFC-FIP","Account":{"type":"deposit","linkedAccRef":"REF-1","Profile":{"Holders":{"type":"SINGLE","Holder":{"name":"Aarav","mobile":9876543210,"pan":"XXXXXX123F"}}},"Summary":{"currentBalance":"1000.50","currency":"INR"},"Transactions":{"startDate":"2026-06-01","Transaction":[{"txnId":"T1","type":"CREDIT","amount":500.25,"transactionalBalance":"1000.50","transactionTimestamp":"2026-06-05T10:00:00+05:30"}]}}}`),
		[]byte(`{"fipid":"SBI-FIP","account":{"type":"deposit","linkedaccref":"REF-2","summary":{"currentbalance":0,"currency":"INR"},"transactions":{"startdate":"2026-06-01","transaction":[]}}}`),
		[]byte(`{"Account":{"type":"deposit","linkedAccRef":"REF-3","Transactions":{"Transaction":{"txnId":"T2","type":"DEBIT","amount":"100","transactionTimestamp":"2026-06-01","reference":999}}}}`),
		[]byte(`{}`),
		[]byte(`not-json`),
		[]byte(`{"Account":{"type":"deposit","linkedAccRef":"R","Transactions":{"Transaction":123}}}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := aa.ParseDeposit(data)
		if err != nil {
			if !errors.Is(err, aa.ErrSchemaValidation) {
				t.Fatalf("expected ErrSchemaValidation, got %v", err)
			}
			return
		}
		if parsed.NaturalKey == "" || parsed.Currency != "INR" {
			t.Fatalf("invalid parsed metadata: %+v", parsed)
		}
		for i, txn := range parsed.Transactions {
			if txn.IdempotencyKey == "" || txn.SignedAmount == 0 {
				t.Fatalf("invalid parsed txn[%d]: %+v", i, txn)
			}
			if i > 0 && txn.Timestamp.Before(parsed.Transactions[i-1].Timestamp) {
				t.Fatalf("transactions not sorted chronologically at index %d", i)
			}
		}
	})
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
