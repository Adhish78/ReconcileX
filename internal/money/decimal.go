// Package money provides fixed-point decimal arithmetic matching PostgreSQL
// NUMERIC(18,4) without IEEE-754 float64 conversions.
//
// Used by internal/aa when parsing ReBIT monetary fields, internal/worker when
// posting paired double-entry ledger legs, and internal/api when summing portfolio
// totals across asset and liability buckets.
package money

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Fixed-point scaling constants for NUMERIC(18,4): 14 integer digits + 4 fractional digits.
// Keeping scale at 10,000 ensures the maximum NUMERIC(18,4) value (99,999,999,999,999.9999,
// i.e. ~10^18 scaled units) fits comfortably within signed 64-bit integer range (~9.22 * 10^18).
const (
	scale4    int64 = 10000
	maxInt14  int64 = 99999999999999 // 14 integer digits + 4 fractional = NUMERIC(18,4)
	maxDigits       = 14
)

// Decimal4 represents a signed fixed-point monetary amount scaled by 10,000
// (4 fractional decimal places), matching PostgreSQL NUMERIC(18,4) exactly.
type Decimal4 int64

// -----------------------------------------------------------------------------
// Parsing
// -----------------------------------------------------------------------------

// ParseDecimal4 parses a base-10 decimal string with at most 14 integer digits
// and 4 fractional digits into an exact Decimal4 without floating-point arithmetic.
func ParseDecimal4(raw string) (Decimal4, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty decimal string")
	}

	// Peel off an optional leading sign before splitting integer and fractional parts.
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}

	if s == "" {
		return 0, fmt.Errorf("invalid decimal %q", raw)
	}

	// Split at the first decimal point and reject malformed inputs with extra dots or empty sides.
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("multiple decimal points in %q", raw)
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("invalid decimal %q", raw)
	}
	if len(intPart) > maxDigits {
		return 0, fmt.Errorf("decimal integer part exceeds NUMERIC(18,4): %q", raw)
	}
	if len(fracPart) > 4 {
		return 0, fmt.Errorf("decimal fractional precision exceeds 4 places: %q", raw)
	}

	// Accumulate integer digits directly in int64 to avoid float64 rounding.
	var whole int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit in %q", raw)
		}
		whole = whole*10 + int64(c-'0')
	}
	if whole > maxInt14 {
		return 0, fmt.Errorf("decimal exceeds NUMERIC(18,4): %q", raw)
	}

	// Right-pad fractional digits to 4 places (e.g., "5" -> 5000, "25" -> 2500).
	var frac int64
	for i := 0; i < 4; i++ {
		var digit int64
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("invalid fractional digit in %q", raw)
			}
			digit = int64(c - '0')
		}
		frac = frac*10 + digit
	}

	// Combine whole and fractional units; normalize "-0.0000" to positive zero.
	total := whole*scale4 + frac
	if neg && total != 0 {
		total = -total
	}
	return Decimal4(total), nil
}

// ParseDecimal4JSON parses either a JSON string ("125430.50") or an unquoted
// JSON number (125430.50) directly from raw JSON bytes without float64 decoding.
func ParseDecimal4JSON(raw json.RawMessage) (Decimal4, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, fmt.Errorf("missing decimal value")
	}

	// Quoted JSON values must be unquoted via json.Unmarshal to validate escape sequences.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return 0, fmt.Errorf("invalid decimal string json: %w", err)
		}
		return ParseDecimal4(s)
	}

	// Unquoted JSON numbers are passed as raw ASCII text straight into ParseDecimal4.
	return ParseDecimal4(string(trimmed))
}

// -----------------------------------------------------------------------------
// Formatting & Exact Arithmetic
// -----------------------------------------------------------------------------

// String formats Decimal4 as a fixed-point 4-decimal string (e.g. "125430.5000")
// matching PostgreSQL's NUMERIC(18,4)::text representation.
func (d Decimal4) String() string {
	v := int64(d)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	whole := v / scale4
	frac := v % scale4
	return fmt.Sprintf("%s%d.%04d", sign, whole, frac)
}

// Neg returns the arithmetic negation of d, used to compute the balancing
// External Clearing leg for double-entry ledger postings.
func (d Decimal4) Neg() Decimal4 {
	return -d
}

// Add returns d + other with explicit signed 64-bit overflow checking so
// extreme ledger sums fail loudly rather than wrapping silently.
func (d Decimal4) Add(other Decimal4) (Decimal4, error) {
	a, b := int64(d), int64(other)
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, fmt.Errorf("decimal4 addition overflow")
	}
	return Decimal4(a + b), nil
}

// Sub returns d - other with signed 64-bit overflow checking, guarding against
// negating math.MinInt64 before delegating to Add.
func (d Decimal4) Sub(other Decimal4) (Decimal4, error) {
	if other == math.MinInt64 {
		return 0, fmt.Errorf("decimal4 subtraction overflow")
	}
	return d.Add(-other)
}
