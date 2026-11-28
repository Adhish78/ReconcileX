// Package cas_test verifies CAMS and KFintech Consolidated Account Statement
// (CASData) parsing, fixed-point decimal/unit decoding, pre-ledger Unit Checksum
// conservation, and deterministic per-transaction idempotency keys.
package cas_test

import (
	"errors"
	"testing"

	"github.com/Adhish78/ReconcileX/internal/cas"
)

// TestParseCASDetailedValidAndCoreTypes exercises a CAMS DETAILED CASData JSON
// payload with DD-MMM-YYYY statement periods, ISO dates, string-encoded decimals,
// tax rows treated as zero units, and all 8 core CAS transaction types.
func TestParseCASDetailedValidAndCoreTypes(t *testing.T) {
	raw := []byte(`{
		"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
		"file_type": "CAMS",
		"cas_type": "DETAILED",
		"investor_info": {
			"name": "AARAV SHARMA",
			"email": "aarav@example.com",
			"address": "Bengaluru",
			"mobile": "+919876543210"
		},
		"folios": [
			{
				"folio": "12345678 / 90",
				"amc": "PPFAS Mutual Fund",
				"name": "AARAV SHARMA",
				"PAN": "ABCDE1234F",
				"KYC": "OK",
				"PANKYC": "OK",
				"schemes": [
					{
						"scheme": "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
						"rta": "CAMS",
						"type": "EQUITY",
						"isin": "INF879O01027",
						"amfi": "122639",
						"open": "0.000",
						"close": "165.000",
						"close_calculated": "165.000",
						"valuation": {
							"date": "2025-03-31",
							"nav": "90.0000",
							"cost": "14800.00",
							"value": "14850.00"
						},
						"transactions": [
							{
								"date": "2024-04-05",
								"description": "Purchase",
								"amount": "5000.00",
								"units": "62.500",
								"nav": "80.0000",
								"type": "PURCHASE"
							},
							{
								"date": "2024-05-05",
								"description": "SIP Purchase - Instalment 1",
								"amount": "5000.00",
								"units": "60.000",
								"nav": "83.3333",
								"type": "PURCHASE_SIP"
							},
							{
								"date": "2024-05-05",
								"description": "*** Stamp Duty ***",
								"amount": "0.25",
								"units": null,
								"nav": null,
								"type": "STAMP_DUTY_TAX"
							},
							{
								"date": "2024-06-10",
								"description": "Switch In from Liquid",
								"amount": "4000.00",
								"units": "45.000",
								"nav": "88.8889",
								"type": "SWITCH_IN"
							},
							{
								"date": "2024-07-15",
								"description": "Dividend Reinvest @ Rs. 2.00",
								"amount": "500.00",
								"units": "5.500",
								"nav": "90.9091",
								"type": "DIVIDEND_REINVEST"
							},
							{
								"date": "2024-08-15",
								"description": "Dividend Payout @ Rs. 1.00",
								"amount": "250.00",
								"units": null,
								"nav": null,
								"type": "DIVIDEND_PAYOUT"
							},
							{
								"date": "2024-09-10",
								"description": "Redemption",
								"amount": "-1000.00",
								"units": "-10.000",
								"nav": "100.0000",
								"type": "REDEMPTION"
							},
							{
								"date": "2024-10-10",
								"description": "Switch Out to Debt",
								"amount": "-500.00",
								"units": "-5.000",
								"nav": "100.0000",
								"type": "SWITCH_OUT"
							},
							{
								"date": "2024-11-10",
								"description": "SIP Purchase - Same Day",
								"amount": "1000.00",
								"units": "10.000",
								"nav": "100.0000",
								"type": "PURCHASE_SIP"
							},
							{
								"date": "2024-11-10",
								"description": "SIP Purchase - Same Day",
								"amount": "1000.00",
								"units": "10.000",
								"nav": "100.0000",
								"type": "PURCHASE_SIP"
							},
							{
								"date": "2024-11-12",
								"description": "Reversal of Purchase",
								"amount": "-1300.00",
								"units": "-13.000",
								"nav": "100.0000",
								"type": "REVERSAL"
							}
						]
					}
				]
			}
		],
		"parse_warnings": []
	}`)

	parsed, err := cas.Parse(raw)
	if err != nil {
		t.Fatalf("cas.Parse unexpected error: %v", err)
	}

	if parsed.FileType != "CAMS" || parsed.CASType != "DETAILED" {
		t.Fatalf("unexpected header: file_type=%q cas_type=%q", parsed.FileType, parsed.CASType)
	}
	if parsed.PeriodFrom.Format("2006-01-02") != "2024-04-01" || parsed.PeriodTo.Format("2006-01-02") != "2025-03-31" {
		t.Fatalf("unexpected statement period: from=%v to=%v", parsed.PeriodFrom, parsed.PeriodTo)
	}
	if len(parsed.Folios) != 1 || len(parsed.Folios[0].Schemes) != 1 {
		t.Fatalf("expected 1 folio and 1 scheme, got %+v", parsed.Folios)
	}

	sch := parsed.Folios[0].Schemes[0]
	if sch.NaturalKey != "CAMS|12345678 / 90|INF879O01027" {
		t.Fatalf("NaturalKey = %q", sch.NaturalKey)
	}
	if sch.SchemeCode == nil || *sch.SchemeCode != 122639 {
		t.Fatalf("SchemeCode = %v, want 122639", sch.SchemeCode)
	}
	if sch.Close.String() != "165.000000" {
		t.Fatalf("Close = %s, want 165.000000", sch.Close.String())
	}
	if sch.Valuation.Value.String() != "14850.0000" {
		t.Fatalf("Valuation.Value = %s, want 14850.0000", sch.Valuation.Value.String())
	}

	// Verify same-key ordinal produces distinct deterministic keys for identical rows.
	dup1 := sch.Transactions[8]
	dup2 := sch.Transactions[9]
	if dup1.IdempotencyKey == "" || dup2.IdempotencyKey == "" || dup1.IdempotencyKey == dup2.IdempotencyKey {
		t.Fatalf("expected distinct same-key ordinal idempotency keys: %q vs %q", dup1.IdempotencyKey, dup2.IdempotencyKey)
	}

	// Re-parsing identical payload must yield byte-identical idempotency keys.
	parsed2, err := cas.Parse(raw)
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if parsed2.Folios[0].Schemes[0].Transactions[8].IdempotencyKey != dup1.IdempotencyKey ||
		parsed2.Folios[0].Schemes[0].Transactions[9].IdempotencyKey != dup2.IdempotencyKey {
		t.Fatalf("expected deterministic idempotency keys across parses")
	}
}

// TestParseCASSummaryValid verifies KFintech SUMMARY CASData decoding and open == close conservation.
func TestParseCASSummaryValid(t *testing.T) {
	raw := []byte(`{
		"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
		"file_type": "KFINTECH",
		"cas_type": "SUMMARY",
		"investor_info": {"name": "PRIYA NAIR", "email": "priya@example.com", "address": "", "mobile": "9876543211"},
		"folios": [
			{
				"folio": "99887766",
				"amc": "Mirae Asset Mutual Fund",
				"PAN": "FGHIJ5678K",
				"schemes": [
					{
						"scheme": "Mirae Asset ELSS Tax Saver Fund - Direct Plan - Growth",
						"rta": "",
						"isin": "INF769K01DM9",
						"amfi": "135781",
						"open": "100.0000",
						"close": "100.0001",
						"valuation": {
							"date": "2025-03-31",
							"nav": "55.0000",
							"cost": "4500.00",
							"value": "5500.01"
						},
						"transactions": []
					}
				]
			}
		]
	}`)

	parsed, err := cas.Parse(raw)
	if err != nil {
		t.Fatalf("cas.Parse SUMMARY unexpected error: %v", err)
	}
	sch := parsed.Folios[0].Schemes[0]
	if sch.RTA != "KFINTECH" {
		t.Fatalf("expected RTA fallback to KFINTECH, got %q", sch.RTA)
	}
	if sch.NaturalKey != "KFINTECH|99887766|INF769K01DM9" {
		t.Fatalf("NaturalKey = %q", sch.NaturalKey)
	}
}

// TestParseCASUnitChecksumViolations enforces the <= 0.0001 conservation tolerance
// across DETAILED (ignoring tax/misc units) and SUMMARY statements.
func TestParseCASUnitChecksumViolations(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "DETAILED mismatch beyond 0.0001",
			raw: `{
				"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
				"file_type": "CAMS",
				"cas_type": "DETAILED",
				"investor_info": {"name": "AARAV"},
				"folios": [{
					"folio": "F1",
					"amc": "HDFC Mutual Fund",
					"schemes": [{
						"scheme": "HDFC Large Cap",
						"rta": "CAMS",
						"isin": "INF179K01YV8",
						"open": "0.000",
						"close": "10.0002",
						"valuation": {"date": "2025-03-31", "nav": "100.00", "value": "1000.02"},
						"transactions": [
							{"date": "2024-04-05", "description": "Purchase", "amount": "1000.00", "units": "10.0000", "type": "PURCHASE"},
							{"date": "2024-04-05", "description": "*** MISC ***", "amount": null, "units": null, "type": "MISC"}
						]
					}]
				}]
			}`,
		},
		{
			name: "SUMMARY open != close beyond 0.0001",
			raw: `{
				"statement_period": {"from": "01-Apr-2024", "to": "31-Mar-2025"},
				"file_type": "KFINTECH",
				"cas_type": "SUMMARY",
				"investor_info": {"name": "AARAV"},
				"folios": [{
					"folio": "F2",
					"amc": "Axis Mutual Fund",
					"schemes": [{
						"scheme": "Axis ELSS",
						"rta": "KFINTECH",
						"isin": "INF846K01EW2",
						"open": "50.0000",
						"close": "50.0005",
						"valuation": {"date": "2025-03-31", "nav": "100.00", "value": "5000.05"},
						"transactions": []
					}]
				}]
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cas.Parse([]byte(tc.raw))
			if !errors.Is(err, cas.ErrUnitChecksum) {
				t.Fatalf("expected ErrUnitChecksum, got %v", err)
			}
		})
	}
}

// TestParseCASSchemaValidationErrors ensures malformed payloads wrap ErrSchemaValidation.
func TestParseCASSchemaValidationErrors(t *testing.T) {
	badPayloads := []struct {
		name string
		raw  string
	}{
		{"invalid json", `{broken`},
		{"unsupported file_type NSDL", `{"file_type":"NSDL","cas_type":"DETAILED","folios":[]}`},
		{"unsupported cas_type UNKNOWN", `{"file_type":"CAMS","cas_type":"UNKNOWN","folios":[]}`},
		{"bad statement period", `{"statement_period":{"from":"2024-04-01","to":"31-Mar-2025"},"file_type":"CAMS","cas_type":"DETAILED","folios":[]}`},
		{"missing isin", `{"statement_period":{"from":"01-Apr-2024","to":"31-Mar-2025"},"file_type":"CAMS","cas_type":"DETAILED","folios":[{"folio":"F1","amc":"AMC","schemes":[{"scheme":"S","open":"0","close":"0","valuation":{"date":"2025-03-31","nav":"10","value":"0"},"transactions":[]}]}]}`},
		{"bad txn iso date", `{"statement_period":{"from":"01-Apr-2024","to":"31-Mar-2025"},"file_type":"CAMS","cas_type":"DETAILED","folios":[{"folio":"F1","amc":"AMC","schemes":[{"scheme":"S","isin":"INF1","open":"0","close":"1","valuation":{"date":"2025-03-31","nav":"10","value":"10"},"transactions":[{"date":"01-Apr-2024","description":"P","amount":"10","units":"1","type":"PURCHASE"}]}]}]}`},
	}

	for _, tc := range badPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cas.Parse([]byte(tc.raw))
			if !errors.Is(err, cas.ErrSchemaValidation) {
				t.Fatalf("expected ErrSchemaValidation for %s, got %v", tc.name, err)
			}
		})
	}
}
