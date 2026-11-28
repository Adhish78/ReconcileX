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
	"math/big"
	"strings"
)

// Fixed-point scaling constants:
//   - Decimal4: NUMERIC(18,4) -> 14 integer digits + 4 fractional digits (scale 10,000).
//   - Decimal6: NUMERIC(18,6) -> 12 integer digits + 6 fractional digits (scale 1,000,000).
// Both fit comfortably inside signed 64-bit integer range (~9.22 * 10^18).
const (
	scale4     int64 = 10000
	maxInt14   int64 = 99999999999999 // 14 integer digits + 4 fractional = NUMERIC(18,4)
	maxDigits4       = 14

	scale6     int64 = 1000000
	maxInt12   int64 = 999999999999 // 12 integer digits + 6 fractional = NUMERIC(18,6)
	maxDigits6       = 12
)

// Decimal4 represents a signed fixed-point monetary amount scaled by 10,000
// (4 fractional decimal places), matching PostgreSQL NUMERIC(18,4) exactly.
type Decimal4 int64

// Decimal6 represents a signed fixed-point unit or NAV quantity scaled by 1,000,000
// (6 fractional decimal places), matching PostgreSQL NUMERIC(18,6) exactly.
type Decimal6 int64

// -----------------------------------------------------------------------------
// Parsing
// -----------------------------------------------------------------------------

type decimalSpec struct {
	maxIntDigits int
	fracPlaces   int
	maxWhole     int64
	scale        int64
	typeLabel    string
	typeName     string
}

var (
	spec4 = decimalSpec{
		maxIntDigits: maxDigits4,
		fracPlaces:   4,
		maxWhole:     maxInt14,
		scale:        scale4,
		typeLabel:    "NUMERIC(18,4)",
		typeName:     "decimal4",
	}
	spec6 = decimalSpec{
		maxIntDigits: maxDigits6,
		fracPlaces:   6,
		maxWhole:     maxInt12,
		scale:        scale6,
		typeLabel:    "NUMERIC(18,6)",
		typeName:     "decimal6",
	}
)

// ParseDecimal4 parses a base-10 decimal string with at most 14 integer digits
// and 4 fractional digits into an exact Decimal4 without floating-point arithmetic.
func ParseDecimal4(raw string) (Decimal4, error) {
	v, err := parseFixedDecimal(raw, spec4)
	return Decimal4(v), err
}

// ParseDecimal4JSON parses either a JSON string ("125430.50") or an unquoted
// JSON number (125430.50) directly from raw JSON bytes without float64 decoding.
func ParseDecimal4JSON(raw json.RawMessage) (Decimal4, error) {
	v, err := parseFixedDecimalJSON(raw, spec4)
	return Decimal4(v), err
}

// ParseDecimal6 parses a base-10 decimal string with at most 12 integer digits
// and 6 fractional digits into an exact Decimal6 without floating-point arithmetic.
func ParseDecimal6(raw string) (Decimal6, error) {
	v, err := parseFixedDecimal(raw, spec6)
	return Decimal6(v), err
}

// ParseDecimal6JSON parses either a JSON string ("33.412") or an unquoted
// JSON number (33.412) directly from raw JSON bytes without float64 decoding.
func ParseDecimal6JSON(raw json.RawMessage) (Decimal6, error) {
	v, err := parseFixedDecimalJSON(raw, spec6)
	return Decimal6(v), err
}

func parseFixedDecimal(raw string, spec decimalSpec) (int64, error) {
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
	if len(intPart) > spec.maxIntDigits {
		return 0, fmt.Errorf("decimal integer part exceeds %s: %q", spec.typeLabel, raw)
	}
	if len(fracPart) > spec.fracPlaces {
		return 0, fmt.Errorf("decimal fractional precision exceeds %d places: %q", spec.fracPlaces, raw)
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
	if whole > spec.maxWhole {
		return 0, fmt.Errorf("decimal exceeds %s: %q", spec.typeLabel, raw)
	}

	// Right-pad fractional digits to spec.fracPlaces places (e.g., "5" -> 5000 for 4 places).
	var frac int64
	for i := 0; i < spec.fracPlaces; i++ {
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

	// Combine whole and fractional units; normalize "-0" to positive zero.
	total := whole*spec.scale + frac
	if neg && total != 0 {
		total = -total
	}
	return total, nil
}

func parseFixedDecimalJSON(raw json.RawMessage, spec decimalSpec) (int64, error) {
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
		return parseFixedDecimal(s, spec)
	}

	// Unquoted JSON numbers are passed as raw ASCII text straight into parseFixedDecimal.
	return parseFixedDecimal(string(trimmed), spec)
}

// -----------------------------------------------------------------------------
// Formatting & Exact Arithmetic
// -----------------------------------------------------------------------------

func formatFixedDecimal(v int64, spec decimalSpec) string {
	sign := ""
	if v < 0 {
		sign = "-"
		if v == math.MinInt64 {
			// Guard against negating math.MinInt64 in extreme formatting calls.
			whole := -(v / spec.scale)
			frac := -(v % spec.scale)
			return fmt.Sprintf("-%d.%0*d", whole, spec.fracPlaces, frac)
		}
		v = -v
	}
	whole := v / spec.scale
	frac := v % spec.scale
	return fmt.Sprintf("%s%d.%0*d", sign, whole, spec.fracPlaces, frac)
}

func checkedAddInt64(a, b int64, typeName string) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, fmt.Errorf("%s addition overflow", typeName)
	}
	return a + b, nil
}

func checkedSubInt64(a, b int64, typeName string) (int64, error) {
	if b == math.MinInt64 {
		return 0, fmt.Errorf("%s subtraction overflow", typeName)
	}
	return checkedAddInt64(a, -b, typeName)
}

// String formats Decimal4 as a fixed-point 4-decimal string (e.g. "125430.5000")
// matching PostgreSQL's NUMERIC(18,4)::text representation.
func (d Decimal4) String() string {
	return formatFixedDecimal(int64(d), spec4)
}

// Neg returns the arithmetic negation of d, used to compute the balancing
// External Clearing leg for double-entry ledger postings.
func (d Decimal4) Neg() Decimal4 {
	return -d
}

// Abs returns the non-negative magnitude of d.
func (d Decimal4) Abs() Decimal4 {
	if d < 0 {
		return -d
	}
	return d
}

// Add returns d + other with explicit signed 64-bit overflow checking so
// extreme ledger sums fail loudly rather than wrapping silently.
func (d Decimal4) Add(other Decimal4) (Decimal4, error) {
	sum, err := checkedAddInt64(int64(d), int64(other), spec4.typeName)
	return Decimal4(sum), err
}

// Sub returns d - other with signed 64-bit overflow checking, guarding against
// negating math.MinInt64 before delegating to Add.
func (d Decimal4) Sub(other Decimal4) (Decimal4, error) {
	diff, err := checkedSubInt64(int64(d), int64(other), spec4.typeName)
	return Decimal4(diff), err
}

// MulDecimal6 multiplies a Decimal4 price/rate by a Decimal6 quantity and rounds
// half-away-from-zero back to Decimal4 without IEEE-754 floating-point conversions.
func (d Decimal4) MulDecimal6(units Decimal6) (Decimal4, error) {
	prod := new(big.Int).Mul(big.NewInt(int64(d)), big.NewInt(int64(units)))
	denom := big.NewInt(scale6)
	quot, rem := new(big.Int).QuoRem(prod, denom, new(big.Int))

	twiceAbsRem := new(big.Int).Lsh(new(big.Int).Abs(rem), 1)
	if twiceAbsRem.Cmp(denom) >= 0 {
		if prod.Sign() >= 0 {
			quot.Add(quot, big.NewInt(1))
		} else {
			quot.Sub(quot, big.NewInt(1))
		}
	}

	maxRaw := maxInt14*scale4 + (scale4 - 1)
	if !quot.IsInt64() {
		return 0, fmt.Errorf("decimal4 multiplication overflow")
	}
	v := quot.Int64()
	if v > maxRaw || v < -maxRaw {
		return 0, fmt.Errorf("decimal4 multiplication exceeds NUMERIC(18,4)")
	}
	return Decimal4(v), nil
}

// String formats Decimal6 as a fixed-point 6-decimal string (e.g. "33.412000")
// matching PostgreSQL's NUMERIC(18,6)::text representation.
func (d Decimal6) String() string {
	return formatFixedDecimal(int64(d), spec6)
}

// Neg returns the arithmetic negation of d.
func (d Decimal6) Neg() Decimal6 {
	return -d
}

// Abs returns the non-negative magnitude of d.
func (d Decimal6) Abs() Decimal6 {
	if d < 0 {
		return -d
	}
	return d
}

// Add returns d + other with explicit signed 64-bit overflow checking.
func (d Decimal6) Add(other Decimal6) (Decimal6, error) {
	sum, err := checkedAddInt64(int64(d), int64(other), spec6.typeName)
	return Decimal6(sum), err
}

// Sub returns d - other with explicit signed 64-bit overflow checking.
func (d Decimal6) Sub(other Decimal6) (Decimal6, error) {
	diff, err := checkedSubInt64(int64(d), int64(other), spec6.typeName)
	return Decimal6(diff), err
}
