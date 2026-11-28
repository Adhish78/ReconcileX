// Package money_test verifies fixed-point NUMERIC(18,4) parsing, formatting,
// and exact arithmetic invariants in internal/money.
package money_test

import (
	"encoding/json"
	"testing"

	"github.com/Adhish78/ReconcileX/internal/money"
)

// TestDecimal4ParseFormatAndArithmetic exercises string/JSON decimal parsing,
// canonical 4-decimal formatting, exact fixed-point addition, and malformed input rejection.
func TestDecimal4ParseFormatAndArithmetic(t *testing.T) {
	// Step 1: Verify normalization across zero, partial decimals, negatives, and max 14-digit integers.
	cases := []struct {
		input string
		want  string
	}{
		{"0", "0.0000"},
		{"125430.5", "125430.5000"},
		{"125430.50", "125430.5000"},
		{"-1500.25", "-1500.2500"},
		{"0.0001", "0.0001"},
		{"-0.0000", "0.0000"},
		{"99999999999999.9999", "99999999999999.9999"},
	}

	for _, tc := range cases {
		got, err := money.ParseDecimal4(tc.input)
		if err != nil {
			t.Fatalf("ParseDecimal4(%q) unexpected err: %v", tc.input, err)
		}
		if got.String() != tc.want {
			t.Fatalf("ParseDecimal4(%q).String() = %q, want %q", tc.input, got.String(), tc.want)
		}
	}

	// Step 2: Verify exact fixed-point addition avoids classic IEEE-754 float64 0.1 + 0.2 drift.
	a, _ := money.ParseDecimal4("0.1")
	b, _ := money.ParseDecimal4("0.2")
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.String() != "0.3000" {
		t.Fatalf("0.1 + 0.2 = %q, want 0.3000", sum.String())
	}

	// Step 3: Verify ParseDecimal4JSON handles both quoted strings and unquoted JSON numbers without float64.
	fromStr, err := money.ParseDecimal4JSON(json.RawMessage(`"125430.50"`))
	if err != nil || fromStr.String() != "125430.5000" {
		t.Fatalf("ParseDecimal4JSON string failed: got=%v err=%v", fromStr, err)
	}
	fromNum, err := money.ParseDecimal4JSON(json.RawMessage(`125430.50`))
	if err != nil || fromNum.String() != "125430.5000" {
		t.Fatalf("ParseDecimal4JSON number failed: got=%v err=%v", fromNum, err)
	}

	// Step 4: Reject empty, non-numeric, multi-dot, and over-precision (>4 decimal places) inputs.
	for _, bad := range []string{"", "abc", "12.34567", "12.3.4", "NaN"} {
		if _, err := money.ParseDecimal4(bad); err == nil {
			t.Fatalf("expected ParseDecimal4(%q) to fail", bad)
		}
	}
}

// TestDecimal6ParseFormatAndArithmetic exercises fixed-point NUMERIC(18,6) unit parsing,
// canonical 6-decimal formatting, exact addition/subtraction/Abs, and overflow/precision guards.
func TestDecimal6ParseFormatAndArithmetic(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"0", "0.000000"},
		{"33.412", "33.412000"},
		{"-10.000", "-10.000000"},
		{"0.0001", "0.000100"},
		{"0.000001", "0.000001"},
		{"-0.000000", "0.000000"},
		{"999999999999.999999", "999999999999.999999"},
	}

	for _, tc := range cases {
		got, err := money.ParseDecimal6(tc.input)
		if err != nil {
			t.Fatalf("ParseDecimal6(%q) unexpected err: %v", tc.input, err)
		}
		if got.String() != tc.want {
			t.Fatalf("ParseDecimal6(%q).String() = %q, want %q", tc.input, got.String(), tc.want)
		}
	}

	a, _ := money.ParseDecimal6("100.125")
	b, _ := money.ParseDecimal6("-33.412")
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.String() != "66.713000" {
		t.Fatalf("100.125 + (-33.412) = %q, want 66.713000", sum.String())
	}

	diff, err := sum.Sub(a)
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if diff.Abs().String() != "33.412000" {
		t.Fatalf("Abs(diff) = %q, want 33.412000", diff.Abs().String())
	}

	fromStr, err := money.ParseDecimal6JSON(json.RawMessage(`"33.412"`))
	if err != nil || fromStr.String() != "33.412000" {
		t.Fatalf("ParseDecimal6JSON string failed: got=%v err=%v", fromStr, err)
	}
	fromNum, err := money.ParseDecimal6JSON(json.RawMessage(`33.412`))
	if err != nil || fromNum.String() != "33.412000" {
		t.Fatalf("ParseDecimal6JSON number failed: got=%v err=%v", fromNum, err)
	}

	for _, bad := range []string{"", "abc", "12.3456789", "1000000000000.000000", "12.3.4", "NaN"} {
		if _, err := money.ParseDecimal6(bad); err == nil {
			t.Fatalf("expected ParseDecimal6(%q) to fail", bad)
		}
	}
}

// TestDecimal4MulDecimal6 verifies exact fixed-point multiplication of a Decimal4
// price/rate by Decimal6 units with half-up rounding and overflow protection.
func TestDecimal4MulDecimal6(t *testing.T) {
	rate, _ := money.ParseDecimal4("2750.00")
	units, _ := money.ParseDecimal6("30")
	got, err := rate.MulDecimal6(units)
	if err != nil {
		t.Fatalf("MulDecimal6: %v", err)
	}
	if got.String() != "82500.0000" {
		t.Fatalf("2750.00 * 30 = %s, want 82500.0000", got.String())
	}

	// Half-up rounding on fractional product: 100.1235 * 10.5 = 1051.29675 -> 1051.2968.
	fracRate, _ := money.ParseDecimal4("100.1235")
	fracUnits, _ := money.ParseDecimal6("10.5")
	rounded, err := fracRate.MulDecimal6(fracUnits)
	if err != nil {
		t.Fatalf("MulDecimal6 fractional: %v", err)
	}
	if rounded.String() != "1051.2968" {
		t.Fatalf("100.1235 * 10.5 = %s, want 1051.2968", rounded.String())
	}

	// Overflow guard on huge NUMERIC(18,4) product.
	hugeRate, _ := money.ParseDecimal4("99999999999999.0000")
	twoUnits, _ := money.ParseDecimal6("2")
	if _, err := hugeRate.MulDecimal6(twoUnits); err == nil {
		t.Fatalf("expected overflow error for product exceeding NUMERIC(18,4)")
	}
}
