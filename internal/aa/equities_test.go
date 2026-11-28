// Package aa_test verifies ReBIT Account Aggregator (AA) EQUITIES v1.0.0 parsing,
// key-casing and multiplicity normalization, corporate actions, deterministic
// idempotency keys, and ErrSchemaValidation wrapping.
package aa_test

import (
	"errors"
	"testing"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/Adhish78/ReconcileX/internal/money"
)

// TestParseEquitiesValidPayload checks end-to-end normalization of a ReBIT EQUITIES
// v1.0.0 payload with multi-ISIN holdings, BUY/SELL monetary deltas, and BONUS/SPLIT
// zero-cash unit corporate actions.
func TestParseEquitiesValidPayload(t *testing.T) {
	raw := []byte(`{
		"fipId": "CDSL-FIP",
		"institutionName": "Zerodha Broking (CDSL)",
		"Account": {
			"type": "equities",
			"maskedDematId": "XXXXXXXXXXXX0987",
			"version": "1.0.0",
			"linkedAccRef": "DEMAT-REF-001",
			"Profile": {
				"Holders": {
					"Holder": [
						{
							"name": "Aarav Sharma",
							"dob": "1992-04-10",
							"mobile": "9876543210",
							"nominee": "REGISTERED",
							"dematId": "XXXXXXXXXXXX0987",
							"address": "12 MG Road, Bengaluru",
							"email": "aarav@example.com",
							"pan": "ABCDE1234F",
							"kycCompliance": true
						}
					]
				}
			},
			"Summary": {
				"currentValue": 184500.00,
				"Investment": {
					"Holdings": {
						"type": "DEMAT",
						"Holding": [
							{
								"issuerName": "RELIANCE INDUSTRIES LTD",
								"isin": "INE002A01018",
								"isinDescription": "RELIANCE INDUSTRIES LTD EQ",
								"units": 60,
								"lastTradedPrice": "2816.65"
							},
							{
								"issuerName": "TATA CONSULTANCY SERVICES LTD",
								"isin": "INE467B01029",
								"isinDescription": "TCS LTD EQ",
								"units": 15,
								"lastTradedPrice": 4100.50
							}
						]
					}
				}
			},
			"Transactions": {
				"startDate": "2025-01-01",
				"endDate": "2025-07-01",
				"Transaction": [
					{
						"txnId": "EQ-SELL-1",
						"orderId": "ORD-9002",
						"companyName": "RELIANCE INDUSTRIES LTD",
						"transactionDateTime": "2025-03-10T11:30:00+05:30",
						"exchange": "NSE",
						"isin": "INE002A01018",
						"isinDescription": "RELIANCE INDUSTRIES LTD EQ",
						"equityCategory": "EQUITY",
						"narration": "SELL",
						"rate": "2900.00",
						"type": "SELL",
						"units": 10
					},
					{
						"txnId": "EQ-BUY-1",
						"orderId": "ORD-9001",
						"companyName": "RELIANCE INDUSTRIES LTD",
						"transactionDateTime": "2025-02-04T10:00:00+05:30",
						"exchange": "NSE",
						"isin": "INE002A01018",
						"isinDescription": "RELIANCE INDUSTRIES LTD EQ",
						"equityCategory": "EQUITY",
						"narration": "BUY",
						"rate": "2750.00",
						"type": "BUY",
						"units": 40
					},
					{
						"txnId": "EQ-BONUS-1",
						"companyName": "RELIANCE INDUSTRIES LTD",
						"transactionDateTime": "2025-04-01T09:00:00+05:30",
						"isin": "INE002A01018",
						"isinDescription": "RELIANCE INDUSTRIES LTD EQ",
						"equityCategory": "EQUITY",
						"narration": "BONUS 1:1",
						"type": "BONUS",
						"units": 30
					}
				]
			}
		}
	}`)

	parsed, err := aa.ParseEquities(raw)
	if err != nil {
		t.Fatalf("ParseEquities unexpected err: %v", err)
	}

	if parsed.NaturalKey != "CDSL-FIP|DEMAT-REF-001" {
		t.Fatalf("NaturalKey = %q, want CDSL-FIP|DEMAT-REF-001", parsed.NaturalKey)
	}
	if parsed.MaskedDematID != "XXXXXXXXXXXX0987" {
		t.Fatalf("MaskedDematID = %q, want XXXXXXXXXXXX0987", parsed.MaskedDematID)
	}
	if parsed.ReportedValue == nil || parsed.ReportedValue.String() != "184500.0000" {
		t.Fatalf("ReportedValue = %v, want 184500.0000", parsed.ReportedValue)
	}
	if len(parsed.Holdings) != 2 {
		t.Fatalf("got %d holdings, want 2", len(parsed.Holdings))
	}
	if len(parsed.Transactions) != 3 {
		t.Fatalf("got %d txns, want 3", len(parsed.Transactions))
	}

	// Sorted chronologically: BUY first, SELL second, BONUS third.
	buy := parsed.Transactions[0]
	wantBuyAmt, _ := money.ParseDecimal4("110000.0000")
	wantBuyUnits, _ := money.ParseDecimal6("40.000000")
	if buy.TxnID != "EQ-BUY-1" || buy.SignedAmount != wantBuyAmt || buy.SignedUnits != wantBuyUnits {
		t.Fatalf("unexpected BUY txn: %+v", buy)
	}
	if buy.IdempotencyKey != aa.EquitiesTxnIdempotencyKey("EQ-BUY-1") {
		t.Fatalf("unexpected BUY idempotency key: %q", buy.IdempotencyKey)
	}

	sell := parsed.Transactions[1]
	wantSellAmt, _ := money.ParseDecimal4("-29000.0000")
	wantSellUnits, _ := money.ParseDecimal6("-10.000000")
	if sell.TxnID != "EQ-SELL-1" || sell.SignedAmount != wantSellAmt || sell.SignedUnits != wantSellUnits {
		t.Fatalf("unexpected SELL txn: %+v", sell)
	}

	bonus := parsed.Transactions[2]
	wantBonusUnits, _ := money.ParseDecimal6("30.000000")
	if bonus.TxnID != "EQ-BONUS-1" || bonus.SignedAmount != 0 || bonus.SignedUnits != wantBonusUnits {
		t.Fatalf("unexpected BONUS txn: %+v", bonus)
	}
}

// TestParseEquitiesNormalizationQuirks verifies lowercase keys, single-object
// multiplicity on Holder/Holding/Transaction, numeric vs string fields, leading
// whitespace in XSD enums (" OTHERS"), and boolean/string kycCompliance.
func TestParseEquitiesNormalizationQuirks(t *testing.T) {
	raw := []byte(`{
		"fipid": "NSDL-FIP",
		"account": {
			"type": "EQUITIES",
			"maskedDematId": "IN3000001234",
			"version": "1.0.0",
			"linkedAccRef": "NSDL-REF-1",
			"profile": {
				"holders": {
					"holder": {
						"name": "Priya Nair",
						"mobile": 9876543210,
						"pan": "XXXXX1234X",
						"kycCompliance": "true"
					}
				}
			},
			"summary": {
				"currentValue": "50000.00",
				"investment": {
					"holdings": {
						"type": "DEMAT",
						"holding": {
							"issuerName": "INFOSYS LTD",
							"isin": "ine009a01021",
							"isinDescription": "INFOSYS LTD EQ",
							"units": "20",
							"lastTradedPrice": 1500.25
						}
					}
				}
			},
			"transactions": {
				"startDate": "2025-01-01",
				"endDate": "2025-06-01",
				"transaction": {
					"txnId": "EQ-SPLIT-1",
					"companyName": "INFOSYS LTD",
					"transactionDateTime": "2025-05-01",
					"exchange": " NSE",
					"isin": "ine009a01021",
					"isinDescription": "INFOSYS LTD EQ",
					"equityCategory": "EQUITY",
					"type": "SPLIT",
					"units": 10
				}
			}
		}
	}`)

	parsed, err := aa.ParseEquities(raw)
	if err != nil {
		t.Fatalf("ParseEquities quirk payload err: %v", err)
	}
	if parsed.InstitutionName != "NSDL-FIP" {
		t.Fatalf("InstitutionName fallback = %q, want NSDL-FIP", parsed.InstitutionName)
	}
	if len(parsed.Holders) != 1 || parsed.Holders[0].Mobile != "9876543210" {
		t.Fatalf("unexpected Holders: %+v", parsed.Holders)
	}
	if len(parsed.Holdings) != 1 || parsed.Holdings[0].ISIN != "INE009A01021" || parsed.Holdings[0].Units.String() != "20.000000" {
		t.Fatalf("unexpected Holdings: %+v", parsed.Holdings)
	}
	if len(parsed.Transactions) != 1 || parsed.Transactions[0].Type != "SPLIT" || parsed.Transactions[0].Exchange != "NSE" {
		t.Fatalf("unexpected Transactions: %+v", parsed.Transactions)
	}
	if parsed.Transactions[0].SignedAmount != 0 || parsed.Transactions[0].SignedUnits.String() != "10.000000" {
		t.Fatalf("expected zero-cash +10 units on SPLIT, got %+v", parsed.Transactions[0])
	}
}

// TestEquitiesAnchorIdempotencyKey verifies deterministic per-ISIN SHA-256 keys.
func TestEquitiesAnchorIdempotencyKey(t *testing.T) {
	k1 := aa.EquitiesAnchorIdempotencyKey("CDSL-FIP|DEMAT-1", "INE002A01018")
	k2 := aa.EquitiesAnchorIdempotencyKey(" CDSL-FIP|DEMAT-1 ", "ine002a01018")
	k3 := aa.EquitiesAnchorIdempotencyKey("CDSL-FIP|DEMAT-1", "INE467B01029")
	if len(k1) != 64 || k1 != k2 || k1 == k3 {
		t.Fatalf("unexpected equity anchor keys: k1=%q k2=%q k3=%q", k1, k2, k3)
	}
}

// TestParseEquitiesSchemaValidationErrors ensures every structural or semantic
// defect wraps aa.ErrSchemaValidation.
func TestParseEquitiesSchemaValidationErrors(t *testing.T) {
	badPayloads := []struct {
		name string
		raw  string
	}{
		{"invalid json", `{not-json`},
		{"missing Account", `{}`},
		{"wrong account type", `{"Account":{"type":"deposit","linkedAccRef":"REF-1"}}`},
		{"missing linkedAccRef", `{"Account":{"type":"equities"}}`},
		{"negative currentValue", `{"Account":{"type":"equities","linkedAccRef":"D1","Summary":{"currentValue":"-10"}}}`},
		{"missing holding isin", `{"Account":{"type":"equities","linkedAccRef":"D1","Summary":{"Investment":{"Holdings":{"Holding":[{"units":10}]}}}}}`},
		{"missing txnId", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"type":"BUY","isin":"INE1","rate":"100","units":1,"transactionDateTime":"2025-01-01T10:00:00Z"}]}}}`},
		{"buy missing rate", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"txnId":"T1","type":"BUY","isin":"INE1","units":1,"transactionDateTime":"2025-01-01T10:00:00Z"}]}}}`},
		{"sell zero rate", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"txnId":"T1","type":"SELL","isin":"INE1","rate":"0","units":1,"transactionDateTime":"2025-01-01T10:00:00Z"}]}}}`},
		{"invalid txn type", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"txnId":"T1","type":"SWAP","isin":"INE1","units":1,"transactionDateTime":"2025-01-01T10:00:00Z"}]}}}`},
		{"zero units on BONUS", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"txnId":"T1","type":"BONUS","isin":"INE1","units":0,"transactionDateTime":"2025-01-01T10:00:00Z"}]}}}`},
		{"zero units on SPLIT", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"txnId":"T1","type":"SPLIT","isin":"INE1","units":0,"transactionDateTime":"2025-01-01T10:00:00Z"}]}}}`},
		{"bad timestamp", `{"Account":{"type":"equities","linkedAccRef":"D1","Transactions":{"Transaction":[{"txnId":"T1","type":"BONUS","isin":"INE1","units":1,"transactionDateTime":"not-a-date"}]}}}`},
	}

	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := aa.ParseEquities([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, aa.ErrSchemaValidation) {
				t.Fatalf("expected ErrSchemaValidation for %s, got %v", tc.name, err)
			}
		})
	}
}

// TestParseEquitiesReverseSplitNegativeUnits verifies that SPLIT accepts signed
// non-zero unit adjustments (such as reverse splits/consolidations) with zero cash.
func TestParseEquitiesReverseSplitNegativeUnits(t *testing.T) {
	raw := []byte(`{
		"fipId": "CDSL-FIP",
		"Account": {
			"type": "equities",
			"linkedAccRef": "DEMAT-REV-1",
			"Transactions": {
				"Transaction": [{
					"txnId": "EQ-RSPLIT-1",
					"transactionDateTime": "2025-06-01T09:00:00+05:30",
					"isin": "INE002A01018",
					"narration": "REVERSE SPLIT 2:1",
					"type": "SPLIT",
					"units": -15
				}]
			}
		}
	}`)

	parsed, err := aa.ParseEquities(raw)
	if err != nil {
		t.Fatalf("ParseEquities reverse split err: %v", err)
	}
	if parsed.HasHoldings {
		t.Fatalf("expected HasHoldings=false when Summary.Investment.Holdings is omitted")
	}
	if len(parsed.Transactions) != 1 || parsed.Transactions[0].SignedAmount != 0 || parsed.Transactions[0].SignedUnits.String() != "-15.000000" {
		t.Fatalf("unexpected reverse split txn: %+v", parsed.Transactions)
	}
}
