package liability_test

import (
	"errors"
	"testing"

	"github.com/Adhish78/ReconcileX/internal/liability"
	"github.com/Adhish78/ReconcileX/internal/money"
)

// TestParseLoanValidHomeAndAutoLoan verifies LIABILITY v1.0.0 parsing for HOME_LOAN
// and AUTO_LOAN, ReBIT single-object/array multiplicity, string/numeric decimals,
// explicit EMI split components, omitted EMI split defaulting to 100% principal,
// chronological sorting, and deterministic idempotency keys.
func TestParseLoanValidHomeAndAutoLoan(t *testing.T) {
	homeRaw := []byte(`{
		"fipId": "HDFC-LOAN-FIP",
		"institutionName": "HDFC Home Loans",
		"Account": {
			"type": "liability",
			"subType": "HOME_LOAN",
			"version": "1.0.0",
			"maskedAccNumber": "HL-XXXX-8812",
			"linkedAccRef": "HL-REF-8812",
			"Profile": {
				"Holders": {
					"type": "SINGLE",
					"Holder": {
						"name": "Aarav Sharma",
						"pan": "ABCDE1234A",
						"mobile": 9800000001,
						"email": "aarav@example.com"
					}
				}
			},
			"Summary": {
				"sanctionedPrincipal": "5000000.00",
				"outstandingBalance": 4450000.00,
				"interestRatePct": "8.50",
				"emiAmount": "43391.00",
				"tenureMonths": 240,
				"disbursementDate": "2024-04-01",
				"statementDate": "2026-06-30",
				"nextEmiDate": "2026-07-05",
				"balanceDateTime": "2026-06-30T18:00:00+05:30",
				"currency": "INR",
				"status": "ACTIVE"
			},
			"Transactions": {
				"startDate": "2026-05-01",
				"endDate": "2026-06-30",
				"Transaction": [
					{
						"txnId": "HL-EMI-02",
						"type": "EMI_REPAYMENT",
						"amount": 43391.00,
						"transactionTimestamp": "2026-06-05T09:00:00+05:30",
						"valueDate": "2026-06-05",
						"transactionalBalance": "4450000.00",
						"narration": "ACH/DR/HDFC HOME LOAN EMI",
						"reference": "EMI202606"
					},
					{
						"txnId": "HL-EMI-01",
						"type": "EMI_REPAYMENT",
						"amount": "43891.00",
						"principalComponent": "12000.00",
						"interestComponent": 31391.00,
						"feeComponent": "500.00",
						"transactionTimestamp": "2026-05-05T09:00:00+05:30",
						"transactionalBalance": "4493391.00",
						"narration": "NACH/DR/HDFC LOAN EMI + LATE FEE"
					},
					{
						"txnId": "HL-FEE-01",
						"type": "FEE_CHARGE",
						"amount": "250.00",
						"transactionTimestamp": "2026-05-10T10:00:00+05:30",
						"narration": "DOC PROCESSING FEE"
					},
					{
						"txnId": "HL-INT-01",
						"type": "INTEREST_ADJUSTMENT",
						"amount": "150.00",
						"transactionTimestamp": "2026-05-15T10:00:00+05:30",
						"narration": "INTEREST RATE RESET ADJ"
					},
					{
						"txnId": "HL-DISB-01",
						"type": "DISBURSEMENT",
						"amount": "100000.00",
						"transactionTimestamp": "2026-05-01T10:00:00+05:30",
						"transactionalBalance": "4505391.00",
						"narration": "TRANCHE DISBURSEMENT"
					}
				]
			}
		}
	}`)

	parsed, err := liability.Parse(homeRaw)
	if err != nil {
		t.Fatalf("Parse home loan unexpected err: %v", err)
	}

	if parsed.SubType != "HOME_LOAN" {
		t.Fatalf("SubType = %q, want HOME_LOAN", parsed.SubType)
	}
	if parsed.NaturalKey != "HDFC-LOAN-FIP|HL-REF-8812" {
		t.Fatalf("NaturalKey = %q, want HDFC-LOAN-FIP|HL-REF-8812", parsed.NaturalKey)
	}
	if parsed.Summary.OutstandingBalance.String() != "4450000.0000" {
		t.Fatalf("OutstandingBalance = %s, want 4450000.0000", parsed.Summary.OutstandingBalance)
	}
	if parsed.Summary.SanctionedPrincipal == nil || parsed.Summary.SanctionedPrincipal.String() != "5000000.0000" {
		t.Fatalf("SanctionedPrincipal = %v, want 5000000.0000", parsed.Summary.SanctionedPrincipal)
	}
	if len(parsed.Holders) != 1 || parsed.Holders[0].Mobile != "9800000001" {
		t.Fatalf("unexpected holders: %+v", parsed.Holders)
	}
	if len(parsed.Transactions) != 5 {
		t.Fatalf("got %d txns, want 5", len(parsed.Transactions))
	}

	// Sorted chronologically: DISB (May 1), EMI-01 (May 5), FEE (May 10), INT (May 15), EMI-02 (Jun 5).
	if parsed.Transactions[0].TxnID != "HL-DISB-01" || parsed.Transactions[4].TxnID != "HL-EMI-02" {
		t.Fatalf("expected chronological sort, got first=%s last=%s", parsed.Transactions[0].TxnID, parsed.Transactions[4].TxnID)
	}

	// Explicit EMI split: 12000 + 31391 + 500 == 43891.
	emi1 := parsed.Transactions[1]
	wantP1, _ := money.ParseDecimal4("12000.0000")
	wantI1, _ := money.ParseDecimal4("31391.0000")
	wantF1, _ := money.ParseDecimal4("500.0000")
	if emi1.PrincipalComponent != wantP1 || emi1.InterestComponent != wantI1 || emi1.FeeComponent != wantF1 {
		t.Fatalf("emi1 split = (%s, %s, %s), want (%s, %s, %s)",
			emi1.PrincipalComponent, emi1.InterestComponent, emi1.FeeComponent, wantP1, wantI1, wantF1)
	}
	if emi1.IdempotencyKey != liability.LoanTxnIdempotencyKey("HL-EMI-01") {
		t.Fatalf("unexpected IdempotencyKey %q", emi1.IdempotencyKey)
	}

	// Omitted split fields default to 100% principal.
	emi2 := parsed.Transactions[4]
	wantP2, _ := money.ParseDecimal4("43391.0000")
	if emi2.PrincipalComponent != wantP2 || emi2.InterestComponent != 0 || emi2.FeeComponent != 0 {
		t.Fatalf("emi2 defaulted split = (%s, %s, %s), want (%s, 0, 0)",
			emi2.PrincipalComponent, emi2.InterestComponent, emi2.FeeComponent, wantP2)
	}

	// AUTO_LOAN with empty transactions and zero outstandingBalance parses cleanly.
	autoRaw := []byte(`{
		"fipId": "ICICI-LOAN-FIP",
		"institutionName": "ICICI Auto Finance",
		"Account": {
			"type": "liability",
			"subType": "AUTO_LOAN",
			"version": "1.0.0",
			"maskedAccNumber": "AL-XXXX-3301",
			"linkedAccRef": "AL-REF-3301",
			"Summary": {
				"outstandingBalance": "0.00",
				"currency": "INR",
				"status": "CLOSED"
			},
			"Transactions": {
				"Transaction": []
			}
		}
	}`)
	autoParsed, err := liability.Parse(autoRaw)
	if err != nil {
		t.Fatalf("Parse auto loan unexpected err: %v", err)
	}
	if autoParsed.SubType != "AUTO_LOAN" || autoParsed.Summary.OutstandingBalance != 0 {
		t.Fatalf("unexpected auto loan parsed: %+v", autoParsed)
	}
}

// TestParseLoanRejectsInvalidBalanceAndEMISplit verifies permanent validation errors:
//   - Negative or missing Summary.outstandingBalance -> ErrSchemaValidation
//   - Unsupported subType / type / currency -> ErrSchemaValidation
//   - EMI_REPAYMENT component mismatch or negative component -> ErrInvalidEMISplit
func TestParseLoanRejectsInvalidBalanceAndEMISplit(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr error
	}{
		{
			name: "negative outstandingBalance",
			payload: `{
				"fipId": "HDFC-LOAN-FIP",
				"Account": {
					"type": "liability",
					"subType": "HOME_LOAN",
					"version": "1.0.0",
					"linkedAccRef": "HL-1",
					"Summary": {"outstandingBalance": "-100.00", "currency": "INR"}
				}
			}`,
			wantErr: liability.ErrSchemaValidation,
		},
		{
			name: "missing Summary.outstandingBalance",
			payload: `{
				"fipId": "HDFC-LOAN-FIP",
				"Account": {
					"type": "liability",
					"subType": "AUTO_LOAN",
					"version": "1.0.0",
					"linkedAccRef": "AL-1",
					"Summary": {"currency": "INR"}
				}
			}`,
			wantErr: liability.ErrSchemaValidation,
		},
		{
			name: "unsupported subType",
			payload: `{
				"fipId": "HDFC-LOAN-FIP",
				"Account": {
					"type": "liability",
					"subType": "PERSONAL_LOAN",
					"version": "1.0.0",
					"linkedAccRef": "PL-1",
					"Summary": {"outstandingBalance": "1000.00", "currency": "INR"}
				}
			}`,
			wantErr: liability.ErrSchemaValidation,
		},
		{
			name: "non-INR currency",
			payload: `{
				"fipId": "HDFC-LOAN-FIP",
				"Account": {
					"type": "liability",
					"subType": "HOME_LOAN",
					"version": "1.0.0",
					"linkedAccRef": "HL-1",
					"Summary": {"outstandingBalance": "1000.00", "currency": "USD"}
				}
			}`,
			wantErr: liability.ErrSchemaValidation,
		},
		{
			name: "EMI split sum mismatch",
			payload: `{
				"fipId": "HDFC-LOAN-FIP",
				"Account": {
					"type": "liability",
					"subType": "HOME_LOAN",
					"version": "1.0.0",
					"linkedAccRef": "HL-1",
					"Summary": {"outstandingBalance": "100000.00", "currency": "INR"},
					"Transactions": {
						"Transaction": [{
							"txnId": "EMI-BAD-1",
							"type": "EMI_REPAYMENT",
							"amount": "20000.00",
							"principalComponent": "12000.00",
							"interestComponent": "7000.00",
							"feeComponent": "500.00",
							"transactionTimestamp": "2026-06-05T09:00:00+05:30"
						}]
					}
				}
			}`,
			wantErr: liability.ErrInvalidEMISplit,
		},
		{
			name: "negative EMI feeComponent",
			payload: `{
				"fipId": "HDFC-LOAN-FIP",
				"Account": {
					"type": "liability",
					"subType": "AUTO_LOAN",
					"version": "1.0.0",
					"linkedAccRef": "AL-1",
					"Summary": {"outstandingBalance": "100000.00", "currency": "INR"},
					"Transactions": {
						"Transaction": [{
							"txnId": "EMI-BAD-2",
							"type": "EMI_REPAYMENT",
							"amount": "15000.00",
							"principalComponent": "15500.00",
							"interestComponent": "0.00",
							"feeComponent": "-500.00",
							"transactionTimestamp": "2026-06-05T09:00:00+05:30"
						}]
					}
				}
			}`,
			wantErr: liability.ErrInvalidEMISplit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := liability.Parse([]byte(tc.payload))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse err = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}
