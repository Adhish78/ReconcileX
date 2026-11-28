// Package cas parses and validates CAMS and KFintech Consolidated Account
// Statements (casparser v1.4.1 CASData JSON) using fixed-point decimal and unit
// arithmetic and enforces the pre-ledger Unit Checksum conservation gate.
package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Adhish78/ReconcileX/internal/money"
)

// Sentinel error categories inspected by internal/worker to route failing CAS
// jobs straight to DEAD_LETTER with the corresponding last_error_code.
var (
	ErrSchemaValidation = errors.New("ERR_SCHEMA_VALIDATION")
	ErrUnitChecksum     = errors.New("ERR_UNIT_CHECKSUM")
)

// unitChecksumTolerance is 0.0001 units in Decimal6 (scale 1,000,000 -> 100).
const unitChecksumTolerance = money.Decimal6(100)

// istLocation anchors timezone-less CAS calendar dates to IST (+05:30).
var istLocation = time.FixedZone("IST", 5*3600+30*60)

// -----------------------------------------------------------------------------
// Normalized domain output types
// -----------------------------------------------------------------------------

// CASData holds a validated, checksum-verified CAMS or KFintech CAS payload.
type CASData struct {
	PeriodFrom    time.Time
	PeriodTo      time.Time
	FileType      string // CAMS or KFINTECH
	CASType       string // DETAILED or SUMMARY
	InvestorInfo  InvestorInfo
	Folios        []Folio
	ParseWarnings []string
}

// InvestorInfo holds page-1 investor contact metadata from CASData.
type InvestorInfo struct {
	Name    string
	Email   string
	Address string
	Mobile  string
}

// Folio represents a mutual fund folio grouping one or more scheme holdings.
type Folio struct {
	FolioNumber string
	AMC         string
	Name        string
	PAN         string
	KYC         string
	PANKYC      string
	Schemes     []Scheme
}

// Scheme represents a single mutual fund scheme holding within a Folio.
type Scheme struct {
	Name            string
	RTA             string
	RTACode         string
	SchemeType      string
	ISIN            string
	SchemeCode      *int
	NaturalKey      string // rta|folio_number|isin
	Open            money.Decimal6
	Close           money.Decimal6
	CloseCalculated *money.Decimal6
	Valuation       *SchemeValuation
	Transactions    []Transaction
}

// SchemeValuation holds the closing valuation summary for a mutual fund scheme.
type SchemeValuation struct {
	Date  time.Time
	NAV   money.Decimal6
	Cost  *money.Decimal4
	Value money.Decimal4
}

// Transaction represents one normalized CAS scheme transaction with a
// deterministic per-transaction SHA-256 idempotency key (including same-key ordinal).
type Transaction struct {
	Date           time.Time
	DateISO        string
	Description    string
	Amount         *money.Decimal4
	Units          *money.Decimal6
	NAV            *money.Decimal6
	NAVDerived     bool
	Balance        *money.Decimal6
	Type           string
	DividendRate   *money.Decimal4
	GiftFolio      string
	IdempotencyKey string
	RawMetadata    []byte
}

// -----------------------------------------------------------------------------
// Raw casparser v1.4.1 JSON wire structs
// -----------------------------------------------------------------------------

type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexString(strings.TrimSpace(s))
		return nil
	}
	if (trimmed[0] >= '0' && trimmed[0] <= '9') || trimmed[0] == '-' {
		var num json.Number
		if err := json.Unmarshal(trimmed, &num); err == nil {
			*f = flexString(strings.TrimSpace(num.String()))
			return nil
		}
	}
	return fmt.Errorf("expected string or number")
}

type rawCASData struct {
	StatementPeriod *rawStatementPeriod `json:"statement_period"`
	FileType        string              `json:"file_type"`
	CASType         string              `json:"cas_type"`
	InvestorInfo    *rawInvestorInfo    `json:"investor_info"`
	Folios          []rawFolio          `json:"folios"`
	ParseWarnings   []string            `json:"parse_warnings"`
}

type rawStatementPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type rawInvestorInfo struct {
	Name    string     `json:"name"`
	Email   string     `json:"email"`
	Address string     `json:"address"`
	Mobile  flexString `json:"mobile"`
}

type rawFolio struct {
	Folio   string      `json:"folio"`
	AMC     string      `json:"amc"`
	Name    string      `json:"name"`
	PAN     string      `json:"PAN"`
	KYC     string      `json:"KYC"`
	PANKYC  string      `json:"PANKYC"`
	Schemes []rawScheme `json:"schemes"`
}

type rawScheme struct {
	Scheme          string              `json:"scheme"`
	Advisor         string              `json:"advisor"`
	RTACode         string              `json:"rta_code"`
	RTA             string              `json:"rta"`
	Type            string              `json:"type"`
	ISIN            string              `json:"isin"`
	AMFI            flexString          `json:"amfi"`
	Nominees        []string            `json:"nominees"`
	Open            json.RawMessage     `json:"open"`
	Close           json.RawMessage     `json:"close"`
	CloseCalculated json.RawMessage     `json:"close_calculated"`
	Valuation       *rawSchemeValuation `json:"valuation"`
	Transactions    []rawTransaction    `json:"transactions"`
}

type rawSchemeValuation struct {
	Date  string          `json:"date"`
	NAV   json.RawMessage `json:"nav"`
	Cost  json.RawMessage `json:"cost"`
	Value json.RawMessage `json:"value"`
}

type rawTransaction struct {
	Date         string          `json:"date"`
	Description  string          `json:"description"`
	Amount       json.RawMessage `json:"amount"`
	Units        json.RawMessage `json:"units"`
	NAV          json.RawMessage `json:"nav"`
	NAVDerived   bool            `json:"nav_derived"`
	Balance      json.RawMessage `json:"balance"`
	Type         string          `json:"type"`
	DividendRate json.RawMessage `json:"dividend_rate"`
	GiftFolio    flexString      `json:"gift_folio"`
}

// -----------------------------------------------------------------------------
// Parser & Pre-Ledger Unit Checksum Gate
// -----------------------------------------------------------------------------

// Parse decodes a casparser v1.4.1 CASData JSON payload, validates all dates
// and fixed-point decimals, computes deterministic idempotency keys, and enforces
// pre-ledger Unit Checksum conservation on every scheme.
func Parse(raw []byte) (CASData, error) {
	var env rawCASData
	if err := json.Unmarshal(raw, &env); err != nil {
		return CASData{}, fmt.Errorf("%w: invalid json: %v", ErrSchemaValidation, err)
	}

	fileType := strings.ToUpper(strings.TrimSpace(env.FileType))
	if fileType != "CAMS" && fileType != "KFINTECH" {
		return CASData{}, fmt.Errorf("%w: unsupported file_type %q (expected CAMS or KFINTECH)", ErrSchemaValidation, env.FileType)
	}

	casType := strings.ToUpper(strings.TrimSpace(env.CASType))
	if casType != "DETAILED" && casType != "SUMMARY" {
		return CASData{}, fmt.Errorf("%w: unsupported cas_type %q (expected DETAILED or SUMMARY)", ErrSchemaValidation, env.CASType)
	}

	var periodFrom, periodTo time.Time
	if env.StatementPeriod != nil {
		if s := strings.TrimSpace(env.StatementPeriod.From); s != "" {
			t, err := parseCASPeriodDate(s)
			if err != nil {
				return CASData{}, fmt.Errorf("%w: invalid statement_period.from: %v", ErrSchemaValidation, err)
			}
			periodFrom = t
		}
		if s := strings.TrimSpace(env.StatementPeriod.To); s != "" {
			t, err := parseCASPeriodDate(s)
			if err != nil {
				return CASData{}, fmt.Errorf("%w: invalid statement_period.to: %v", ErrSchemaValidation, err)
			}
			periodTo = t
		}
	}

	var investor InvestorInfo
	if env.InvestorInfo != nil {
		investor = InvestorInfo{
			Name:    strings.TrimSpace(env.InvestorInfo.Name),
			Email:   strings.TrimSpace(env.InvestorInfo.Email),
			Address: strings.TrimSpace(env.InvestorInfo.Address),
			Mobile:  string(env.InvestorInfo.Mobile),
		}
	}

	if len(env.Folios) == 0 {
		return CASData{}, fmt.Errorf("%w: at least one folio is required", ErrSchemaValidation)
	}

	folios := make([]Folio, 0, len(env.Folios))
	for fi, rf := range env.Folios {
		folio, err := parseFolio(rf, fileType)
		if err != nil {
			return CASData{}, fmt.Errorf("%w: folios[%d]: %v", ErrSchemaValidation, fi, err)
		}
		folios = append(folios, folio)
	}

	// Enforce pre-ledger Unit Checksum conservation across every scheme before returning.
	for _, f := range folios {
		for _, s := range f.Schemes {
			if err := verifySchemeUnitChecksum(casType, f.FolioNumber, s); err != nil {
				return CASData{}, err
			}
		}
	}

	return CASData{
		PeriodFrom:    periodFrom,
		PeriodTo:      periodTo,
		FileType:      fileType,
		CASType:       casType,
		InvestorInfo:  investor,
		Folios:        folios,
		ParseWarnings: env.ParseWarnings,
	}, nil
}

func parseFolio(rf rawFolio, defaultRTA string) (Folio, error) {
	folioNum := strings.TrimSpace(rf.Folio)
	if folioNum == "" {
		return Folio{}, fmt.Errorf("folio is required")
	}

	amc := strings.TrimSpace(rf.AMC)
	if len(rf.Schemes) == 0 {
		return Folio{}, fmt.Errorf("schemes cannot be empty")
	}

	schemes := make([]Scheme, 0, len(rf.Schemes))
	for si, rs := range rf.Schemes {
		sch, err := parseScheme(rs, folioNum, defaultRTA)
		if err != nil {
			return Folio{}, fmt.Errorf("schemes[%d]: %v", si, err)
		}
		schemes = append(schemes, sch)
	}

	return Folio{
		FolioNumber: folioNum,
		AMC:         amc,
		Name:        strings.TrimSpace(rf.Name),
		PAN:         strings.ToUpper(strings.TrimSpace(rf.PAN)),
		KYC:         strings.TrimSpace(rf.KYC),
		PANKYC:      strings.TrimSpace(rf.PANKYC),
		Schemes:     schemes,
	}, nil
}

func parseScheme(rs rawScheme, folioNum, defaultRTA string) (Scheme, error) {
	schemeName := strings.TrimSpace(rs.Scheme)
	if schemeName == "" {
		return Scheme{}, fmt.Errorf("scheme name is required")
	}

	isin := strings.ToUpper(strings.TrimSpace(rs.ISIN))
	if isin == "" {
		return Scheme{}, fmt.Errorf("isin is required")
	}

	rta := strings.ToUpper(strings.TrimSpace(rs.RTA))
	if rta == "" {
		rta = defaultRTA
	}

	var schemeCode *int
	if rawAMFI := string(rs.AMFI); rawAMFI != "" {
		code, err := strconv.Atoi(rawAMFI)
		if err != nil || code <= 0 {
			return Scheme{}, fmt.Errorf("invalid amfi scheme code %q", rawAMFI)
		}
		schemeCode = &code
	}

	openUnits, err := money.ParseDecimal6JSON(rs.Open)
	if err != nil {
		return Scheme{}, fmt.Errorf("invalid open units: %w", err)
	}
	closeUnits, err := money.ParseDecimal6JSON(rs.Close)
	if err != nil {
		return Scheme{}, fmt.Errorf("invalid close units: %w", err)
	}

	var closeCalc *money.Decimal6
	if hasJSONValue(rs.CloseCalculated) {
		cc, err := money.ParseDecimal6JSON(rs.CloseCalculated)
		if err != nil {
			return Scheme{}, fmt.Errorf("invalid close_calculated: %w", err)
		}
		closeCalc = &cc
	}

	var valuation *SchemeValuation
	if rs.Valuation != nil {
		val, err := parseValuation(*rs.Valuation)
		if err != nil {
			return Scheme{}, fmt.Errorf("valuation: %w", err)
		}
		valuation = &val
	}

	// Track per-scheme same-key occurrence counts so identical transactions within
	// the same statement receive distinct deterministic ordinals (0, 1, ...).
	sameKeyCounts := make(map[string]int, len(rs.Transactions))
	txns := make([]Transaction, 0, len(rs.Transactions))
	for ti, rt := range rs.Transactions {
		txn, err := parseTransaction(rt, rta, sameKeyCounts)
		if err != nil {
			return Scheme{}, fmt.Errorf("transactions[%d]: %w", ti, err)
		}
		txns = append(txns, txn)
	}

	return Scheme{
		Name:            schemeName,
		RTA:             rta,
		RTACode:         strings.TrimSpace(rs.RTACode),
		SchemeType:      strings.ToUpper(strings.TrimSpace(rs.Type)),
		ISIN:            isin,
		SchemeCode:      schemeCode,
		NaturalKey:      rta + "|" + folioNum + "|" + isin,
		Open:            openUnits,
		Close:           closeUnits,
		CloseCalculated: closeCalc,
		Valuation:       valuation,
		Transactions:    txns,
	}, nil
}

func parseValuation(rv rawSchemeValuation) (SchemeValuation, error) {
	dt, err := parseISODate(rv.Date)
	if err != nil {
		return SchemeValuation{}, fmt.Errorf("invalid date: %w", err)
	}

	nav, err := money.ParseDecimal6JSON(rv.NAV)
	if err != nil {
		return SchemeValuation{}, fmt.Errorf("invalid nav: %w", err)
	}

	var cost *money.Decimal4
	if hasJSONValue(rv.Cost) {
		c, err := money.ParseDecimal4JSON(rv.Cost)
		if err != nil {
			return SchemeValuation{}, fmt.Errorf("invalid cost: %w", err)
		}
		cost = &c
	}

	value, err := money.ParseDecimal4JSON(rv.Value)
	if err != nil {
		return SchemeValuation{}, fmt.Errorf("invalid value: %w", err)
	}

	return SchemeValuation{
		Date:  dt,
		NAV:   nav,
		Cost:  cost,
		Value: value,
	}, nil
}

func parseOptionalJSON[T any](raw json.RawMessage, fieldName string, parser func(json.RawMessage) (T, error)) (*T, error) {
	if !hasJSONValue(raw) {
		return nil, nil
	}
	v, err := parser(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", fieldName, err)
	}
	return &v, nil
}

func parseTransaction(rt rawTransaction, rta string, sameKeyCounts map[string]int) (Transaction, error) {
	dt, err := parseISODate(rt.Date)
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid date: %w", err)
	}
	dateISO := dt.Format("2006-01-02")

	txnType := strings.ToUpper(strings.TrimSpace(rt.Type))
	if !isKnownCASTxnType(txnType) {
		return Transaction{}, fmt.Errorf("unsupported transaction type %q", rt.Type)
	}

	amount, err := parseOptionalJSON(rt.Amount, "amount", money.ParseDecimal4JSON)
	if err != nil {
		return Transaction{}, err
	}
	units, err := parseOptionalJSON(rt.Units, "units", money.ParseDecimal6JSON)
	if err != nil {
		return Transaction{}, err
	}
	nav, err := parseOptionalJSON(rt.NAV, "nav", money.ParseDecimal6JSON)
	if err != nil {
		return Transaction{}, err
	}
	balance, err := parseOptionalJSON(rt.Balance, "balance", money.ParseDecimal6JSON)
	if err != nil {
		return Transaction{}, err
	}
	divRate, err := parseOptionalJSON(rt.DividendRate, "dividend_rate", money.ParseDecimal4JSON)
	if err != nil {
		return Transaction{}, err
	}

	if err := validateCoreTxnFields(txnType, amount, units); err != nil {
		return Transaction{}, err
	}

	desc := strings.TrimSpace(rt.Description)
	amtKey := ""
	if amount != nil {
		amtKey = amount.String()
	}
	unitsKey := ""
	if units != nil {
		unitsKey = units.String()
	}

	baseKey := buildBaseTxnIdempotencyInput("CAS_"+rta, dateISO, txnType, amtKey, unitsKey, desc)
	ordinal := sameKeyCounts[baseKey]
	sameKeyCounts[baseKey] = ordinal + 1
	idemKey := TxnIdempotencyKey("CAS_"+rta, dateISO, txnType, amtKey, unitsKey, desc, ordinal)

	metaMap := map[string]any{
		"date":    dateISO,
		"ordinal": ordinal,
	}
	if nav != nil {
		metaMap["nav"] = nav.String()
	}
	if rt.NAVDerived {
		metaMap["nav_derived"] = true
	}
	if balance != nil {
		metaMap["balance"] = balance.String()
	}
	if divRate != nil {
		metaMap["dividend_rate"] = divRate.String()
	}
	if gf := string(rt.GiftFolio); gf != "" {
		metaMap["gift_folio"] = gf
	}
	metaBytes, _ := json.Marshal(metaMap)

	return Transaction{
		Date:           dt,
		DateISO:        dateISO,
		Description:    desc,
		Amount:         amount,
		Units:          units,
		NAV:            nav,
		NAVDerived:     rt.NAVDerived,
		Balance:        balance,
		Type:           txnType,
		DividendRate:   divRate,
		GiftFolio:      string(rt.GiftFolio),
		IdempotencyKey: idemKey,
		RawMetadata:    metaBytes,
	}, nil
}

// validateCoreTxnFields enforces amount/unit sign invariants on the 8 core CAS
// transaction types so inconsistent rows fail fast with ErrSchemaValidation.
func validateCoreTxnFields(txnType string, amount *money.Decimal4, units *money.Decimal6) error {
	switch txnType {
	case "PURCHASE", "PURCHASE_SIP", "SWITCH_IN", "DIVIDEND_REINVEST":
		if amount == nil || *amount <= 0 {
			return fmt.Errorf("%s requires positive amount", txnType)
		}
		if units == nil || *units <= 0 {
			return fmt.Errorf("%s requires positive units", txnType)
		}
	case "REDEMPTION", "SWITCH_OUT":
		if amount == nil || *amount >= 0 {
			return fmt.Errorf("%s requires negative amount", txnType)
		}
		if units == nil || *units >= 0 {
			return fmt.Errorf("%s requires negative units", txnType)
		}
	case "DIVIDEND_PAYOUT":
		if amount == nil || *amount <= 0 {
			return fmt.Errorf("DIVIDEND_PAYOUT requires positive amount")
		}
		if units != nil && *units != 0 {
			return fmt.Errorf("DIVIDEND_PAYOUT cannot carry non-zero units")
		}
	case "REVERSAL":
		if amount == nil || *amount == 0 || units == nil || *units == 0 {
			return fmt.Errorf("REVERSAL requires non-zero amount and units")
		}
		if (*amount > 0 && *units < 0) || (*amount < 0 && *units > 0) {
			return fmt.Errorf("REVERSAL amount (%s) and units (%s) must share sign", amount.String(), units.String())
		}
	}
	return nil
}

// verifySchemeUnitChecksum enforces pre-ledger Unit Checksum conservation:
//   - SUMMARY: |open - close| <= 0.0001
//   - DETAILED: |open + SUM(t.units) - close| <= 0.0001 (tax & MISC rows = 0 units)
func verifySchemeUnitChecksum(casType, folioNum string, s Scheme) error {
	var expected money.Decimal6
	if casType == "SUMMARY" {
		expected = s.Open
	} else {
		running := s.Open
		for _, t := range s.Transactions {
			if isZeroUnitChecksumType(t.Type) || t.Units == nil {
				continue
			}
			var err error
			running, err = running.Add(*t.Units)
			if err != nil {
				return fmt.Errorf("%w: unit sum overflow on folio %s isin %s: %v", ErrUnitChecksum, folioNum, s.ISIN, err)
			}
		}
		expected = running
	}

	diff, err := expected.Sub(s.Close)
	if err != nil {
		return fmt.Errorf("%w: unit checksum diff overflow on folio %s isin %s: %v", ErrUnitChecksum, folioNum, s.ISIN, err)
	}
	if diff.Abs() > unitChecksumTolerance {
		return fmt.Errorf(
			"%w: folio %s (%s) unit conservation failed: expected %s, close %s (diff %s > 0.000100)",
			ErrUnitChecksum, folioNum, s.ISIN, expected.String(), s.Close.String(), diff.Abs().String(),
		)
	}
	return nil
}

func isZeroUnitChecksumType(txnType string) bool {
	switch txnType {
	case "STAMP_DUTY_TAX", "STT_TAX", "TDS_TAX", "MISC":
		return true
	default:
		return false
	}
}

func isKnownCASTxnType(txnType string) bool {
	switch txnType {
	case "PURCHASE", "PURCHASE_SIP", "SWITCH_IN", "REDEMPTION", "SWITCH_OUT",
		"DIVIDEND_REINVEST", "DIVIDEND_PAYOUT", "REVERSAL",
		"STAMP_DUTY_TAX", "STT_TAX", "TDS_TAX",
		"SWITCH_IN_MERGER", "SWITCH_OUT_MERGER", "SEGREGATION",
		"GIFT_IN", "GIFT_OUT", "MISC", "UNKNOWN":
		return true
	default:
		return false
	}
}

// -----------------------------------------------------------------------------
// Deterministic Idempotency & Date Helpers
// -----------------------------------------------------------------------------

func normalizeDescription(desc string) string {
	return strings.Join(strings.Fields(strings.ToUpper(strings.TrimSpace(desc))), " ")
}

func buildBaseTxnIdempotencyInput(source, dateISO, txnType, amountStr, unitsStr, desc string) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s",
		strings.TrimSpace(source),
		strings.TrimSpace(dateISO),
		strings.TrimSpace(txnType),
		strings.TrimSpace(amountStr),
		strings.TrimSpace(unitsStr),
		normalizeDescription(desc),
	)
}

// TxnIdempotencyKey computes the deterministic SHA-256 per-transaction key
// covering (source | date | type | amount | units | normalized_description | ordinal).
func TxnIdempotencyKey(source, dateISO, txnType, amountStr, unitsStr, desc string, ordinal int) string {
	base := buildBaseTxnIdempotencyInput(source, dateISO, txnType, amountStr, unitsStr, desc)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", base, ordinal)))
	return hex.EncodeToString(sum[:])
}

func parseCASPeriodDate(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if len(s) != 11 {
		return time.Time{}, fmt.Errorf("expected DD-MMM-YYYY, got %q", raw)
	}
	t, err := time.ParseInLocation("02-Jan-2006", s, istLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected DD-MMM-YYYY, got %q", raw)
	}
	return t, nil
}

func parseISODate(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if len(s) != 10 {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, got %q", raw)
	}
	t, err := time.ParseInLocation("2006-01-02", s, istLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected YYYY-MM-DD, got %q", raw)
	}
	return t, nil
}

func hasJSONValue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
