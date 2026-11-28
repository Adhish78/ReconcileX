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
