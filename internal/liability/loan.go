// Package liability parses and validates ReconcileX's internal LIABILITY v1.0.0
// ingestion payloads (HOME_LOAN and AUTO_LOAN) into normalized domain structs
// backed by fixed-point money.Decimal4.
//
// Invoked asynchronously by internal/worker after a job is claimed via SKIP LOCKED.
package liability

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Adhish78/ReconcileX/internal/money"
)

var (
	// ErrSchemaValidation marks a permanent LIABILITY payload schema defect.
	// The worker transitions failing jobs directly to DEAD_LETTER with
	// last_error_code = ERR_SCHEMA_VALIDATION.
	ErrSchemaValidation = errors.New("ERR_SCHEMA_VALIDATION")

	// ErrInvalidEMISplit marks an EMI_REPAYMENT whose principalComponent,
	// interestComponent, and feeComponent are negative or do not sum to amount.
	// The worker transitions failing jobs directly to DEAD_LETTER with
	// last_error_code = ERR_INVALID_EMI_SPLIT.
	ErrInvalidEMISplit = errors.New("ERR_INVALID_EMI_SPLIT")
)

// -----------------------------------------------------------------------------
// Normalized domain output types
// -----------------------------------------------------------------------------

// LoanPayload holds validated Financial Account metadata, loan Summary metrics,
// and chronologically sorted loan Transactions for HOME_LOAN and AUTO_LOAN feeds.
type LoanPayload struct {
	FipID                 string
	InstitutionName       string
	SubType               string // HOME_LOAN or AUTO_LOAN
	Version               string
	NaturalKey            string
	LinkedAccRef          string
	MaskedAccNumber       string
	Currency              string
	HoldersType           string
	Holders               []Holder
	Summary               LoanSummary
	TransactionsStartDate *time.Time
	Transactions          []LoanTransaction
}

// Holder holds normalized Profile.Holders.Holder attributes for a Liability Account.
type Holder struct {
	Name   string
	DOB    string
	Mobile string
	PAN    string
	Email  string
}

// LoanSummary holds institution-reported loan summary figures with a strictly
// non-negative OutstandingBalance reconciled against ABS(LIABILITY:LOAN).
type LoanSummary struct {
	SanctionedPrincipal *money.Decimal4
	OutstandingBalance  money.Decimal4
	InterestRatePct     *money.Decimal4
	EMIAmount           *money.Decimal4
	TenureMonths        *int
	DisbursementDate    *time.Time
	StatementDate       *time.Time
	NextEMIDate         *time.Time
	BalanceDateTime     *time.Time
	Status              string
}

// LoanTransaction represents a single normalized loan ledger event
// (DISBURSEMENT, EMI_REPAYMENT, INTEREST_ADJUSTMENT, or FEE_CHARGE).
type LoanTransaction struct {
	TxnID                string
	Type                 string
	Amount               money.Decimal4
	PrincipalComponent   money.Decimal4
	InterestComponent    money.Decimal4
	FeeComponent         money.Decimal4
	TransactionalBalance *money.Decimal4
	Timestamp            time.Time
	ValueDate            *time.Time
	Narration            string
	Reference            string
	IdempotencyKey       string
	RawMetadata          []byte
}

// PrincipalDelta returns the signed change to unpaid principal caused by this
// transaction (+amount for DISBURSEMENT, -principalComponent for EMI_REPAYMENT, 0 otherwise).
func (t LoanTransaction) PrincipalDelta() money.Decimal4 {
	switch t.Type {
	case "DISBURSEMENT":
		return t.Amount
	case "EMI_REPAYMENT":
		return t.PrincipalComponent.Neg()
	default:
		return 0
	}
}

// -----------------------------------------------------------------------------
// Raw JSON wire structs
// -----------------------------------------------------------------------------

type flexSlice[T any] []T

func (f *flexSlice[T]) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = nil
		return nil
	}
	switch trimmed[0] {
	case '[':
		var items []T
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		*f = items
		return nil
	case '{':
		var single T
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return err
		}
		*f = []T{single}
		return nil
	default:
		return fmt.Errorf("expected JSON array or object")
	}
}

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
	return fmt.Errorf("expected JSON string or number")
}

type rawEnvelope struct {
	FipID           string      `json:"fipId"`
	InstitutionName string      `json:"institutionName"`
	Account         *rawAccount `json:"Account"`
}

type rawAccount struct {
	Type            string           `json:"type"`
	SubType         string           `json:"subType"`
	Version         string           `json:"version"`
	MaskedAccNumber string           `json:"maskedAccNumber"`
	LinkedAccRef    string           `json:"linkedAccRef"`
	Profile         *rawProfile      `json:"Profile"`
	Summary         *rawLoanSummary  `json:"Summary"`
	Transactions    *rawTxnsWrap     `json:"Transactions"`
}

type rawProfile struct {
	Holders *rawHolders `json:"Holders"`
}

type rawHolders struct {
	Type   string              `json:"type"`
	Holder flexSlice[rawHolder] `json:"Holder"`
}

type rawHolder struct {
	Name   string     `json:"name"`
	DOB    string     `json:"dob"`
	Mobile flexString `json:"mobile"`
	PAN    string     `json:"pan"`
	Email  string     `json:"email"`
}

type rawLoanSummary struct {
	SanctionedPrincipal json.RawMessage `json:"sanctionedPrincipal"`
	OutstandingBalance  json.RawMessage `json:"outstandingBalance"`
	InterestRatePct     json.RawMessage `json:"interestRatePct"`
	EMIAmount           json.RawMessage `json:"emiAmount"`
	TenureMonths        *int            `json:"tenureMonths"`
	DisbursementDate    string          `json:"disbursementDate"`
	StatementDate       string          `json:"statementDate"`
	NextEMIDate         string          `json:"nextEmiDate"`
	BalanceDateTime     string          `json:"balanceDateTime"`
	Currency            string          `json:"currency"`
	Status              string          `json:"status"`
}

type rawTxnsWrap struct {
	StartDate   string                 `json:"startDate"`
	EndDate     string                 `json:"endDate"`
	Transaction flexSlice[rawLoanTxn]  `json:"Transaction"`
}

type rawLoanTxn struct {
	TxnID                string          `json:"txnId"`
	Type                 string          `json:"type"`
	Amount               json.RawMessage `json:"amount"`
	PrincipalComponent   json.RawMessage `json:"principalComponent"`
	InterestComponent    json.RawMessage `json:"interestComponent"`
	FeeComponent         json.RawMessage `json:"feeComponent"`
	TransactionalBalance json.RawMessage `json:"transactionalBalance"`
	TransactionTimestamp string          `json:"transactionTimestamp"`
	ValueDate            string          `json:"valueDate"`
	Narration            string          `json:"narration"`
	Reference            string          `json:"reference"`
}

// -----------------------------------------------------------------------------
// Deterministic idempotency keys & parser
// -----------------------------------------------------------------------------

// LoanTxnIdempotencyKey computes the deterministic per-transaction SHA-256 key
// for LIABILITY rows: SHA256("LIABILITY|" + txnId).
func LoanTxnIdempotencyKey(txnID string) string {
	sum := sha256.Sum256([]byte("LIABILITY|" + strings.TrimSpace(txnID)))
	return hex.EncodeToString(sum[:])
}

// LoanAnchorIdempotencyKey computes the deterministic SHA-256 idempotency key
// for a LIABILITY Opening Anchor: SHA256("LIABILITY|OPENING_ANCHOR|" + naturalKey + "|").
func LoanAnchorIdempotencyKey(naturalKey string) string {
	sum := sha256.Sum256([]byte("LIABILITY|OPENING_ANCHOR|" + strings.TrimSpace(naturalKey) + "|"))
	return hex.EncodeToString(sum[:])
}

// Parse parses and validates an internal LIABILITY v1.0.0 JSON payload for
// HOME_LOAN and AUTO_LOAN accounts.
func Parse(raw []byte) (LoanPayload, error) {
	var env rawEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return LoanPayload{}, fmt.Errorf("%w: invalid json: %v", ErrSchemaValidation, err)
	}
	if env.Account == nil {
		return LoanPayload{}, fmt.Errorf("%w: missing Account object", ErrSchemaValidation)
	}

	acct := env.Account
	if !strings.EqualFold(strings.TrimSpace(acct.Type), "liability") {
		return LoanPayload{}, fmt.Errorf("%w: Account.type must be liability, got %q", ErrSchemaValidation, acct.Type)
	}

	subType := strings.ToUpper(strings.TrimSpace(acct.SubType))
	if subType != "HOME_LOAN" && subType != "AUTO_LOAN" {
		return LoanPayload{}, fmt.Errorf("%w: unsupported loan subType %q", ErrSchemaValidation, acct.SubType)
	}

	version := strings.TrimSpace(acct.Version)
	if version == "" {
		version = "1.0.0"
	} else if version != "1.0.0" {
		return LoanPayload{}, fmt.Errorf("%w: unsupported version %q", ErrSchemaValidation, acct.Version)
	}

	linkedRef := strings.TrimSpace(acct.LinkedAccRef)
	if linkedRef == "" {
		return LoanPayload{}, fmt.Errorf("%w: Account.linkedAccRef is required", ErrSchemaValidation)
	}

	fipID := strings.TrimSpace(env.FipID)
	if fipID == "" {
		fipID = "UNKNOWN-FIP"
	}
	instName := strings.TrimSpace(env.InstitutionName)
	if instName == "" {
		instName = fipID
	}

	holdersType, holders := parseHolders(acct.Profile)

	summary, currency, err := parseLoanSummary(acct.Summary)
	if err != nil {
		return LoanPayload{}, err
	}

	startDate, txns, err := parseLoanTransactions(acct.Transactions)
	if err != nil {
		return LoanPayload{}, err
	}

	return LoanPayload{
		FipID:                 fipID,
		InstitutionName:       instName,
		SubType:               subType,
		Version:               version,
		NaturalKey:            fipID + "|" + linkedRef,
		LinkedAccRef:          linkedRef,
		MaskedAccNumber:       strings.TrimSpace(acct.MaskedAccNumber),
		Currency:              currency,
		HoldersType:           holdersType,
		Holders:               holders,
		Summary:               summary,
		TransactionsStartDate: startDate,
		Transactions:          txns,
	}, nil
}

func parseHolders(profile *rawProfile) (string, []Holder) {
	if profile == nil || profile.Holders == nil {
		return "", nil
	}
	holdersType := strings.ToUpper(strings.TrimSpace(profile.Holders.Type))
	holders := make([]Holder, 0, len(profile.Holders.Holder))
	for _, h := range profile.Holders.Holder {
		holders = append(holders, Holder{
			Name:   strings.TrimSpace(h.Name),
			DOB:    strings.TrimSpace(h.DOB),
			Mobile: string(h.Mobile),
			PAN:    strings.TrimSpace(h.PAN),
			Email:  strings.TrimSpace(h.Email),
		})
	}
	return holdersType, holders
}

func parseLoanSummary(raw *rawLoanSummary) (LoanSummary, string, error) {
	if raw == nil {
		return LoanSummary{}, "", fmt.Errorf("%w: Summary object is required", ErrSchemaValidation)
	}

	currency := "INR"
	if c := strings.TrimSpace(raw.Currency); c != "" {
		if !strings.EqualFold(c, "INR") {
			return LoanSummary{}, "", fmt.Errorf("%w: unsupported currency %q (only INR is supported)", ErrSchemaValidation, c)
		}
	}

	if !hasJSONValue(raw.OutstandingBalance) {
		return LoanSummary{}, "", fmt.Errorf("%w: Summary.outstandingBalance is required", ErrSchemaValidation)
	}
	outstanding, err := money.ParseDecimal4JSON(raw.OutstandingBalance)
	if err != nil {
		return LoanSummary{}, "", fmt.Errorf("%w: invalid Summary.outstandingBalance: %v", ErrSchemaValidation, err)
	}
	if outstanding < 0 {
		return LoanSummary{}, "", fmt.Errorf("%w: Summary.outstandingBalance must be non-negative, got %s", ErrSchemaValidation, outstanding.String())
	}

	sanctioned, err := parseOptionalNonNegDecimal4(raw.SanctionedPrincipal, "Summary.sanctionedPrincipal")
	if err != nil {
		return LoanSummary{}, "", err
	}
	ratePct, err := parseOptionalNonNegDecimal4(raw.InterestRatePct, "Summary.interestRatePct")
	if err != nil {
		return LoanSummary{}, "", err
	}
	emiAmt, err := parseOptionalNonNegDecimal4(raw.EMIAmount, "Summary.emiAmount")
	if err != nil {
		return LoanSummary{}, "", err
	}

	if raw.TenureMonths != nil && *raw.TenureMonths < 0 {
		return LoanSummary{}, "", fmt.Errorf("%w: Summary.tenureMonths must be non-negative", ErrSchemaValidation)
	}

	disbDate, err := parseOptionalDate(raw.DisbursementDate, "Summary.disbursementDate")
	if err != nil {
		return LoanSummary{}, "", err
	}
	stmtDate, err := parseOptionalDate(raw.StatementDate, "Summary.statementDate")
	if err != nil {
		return LoanSummary{}, "", err
	}
	nextEMI, err := parseOptionalDate(raw.NextEMIDate, "Summary.nextEmiDate")
	if err != nil {
		return LoanSummary{}, "", err
	}
	balAt, err := parseOptionalDate(raw.BalanceDateTime, "Summary.balanceDateTime")
	if err != nil {
		return LoanSummary{}, "", err
	}

	status := strings.ToUpper(strings.TrimSpace(raw.Status))
	if status != "" && status != "ACTIVE" && status != "CLOSED" {
		return LoanSummary{}, "", fmt.Errorf("%w: invalid Summary.status %q", ErrSchemaValidation, raw.Status)
	}

	return LoanSummary{
		SanctionedPrincipal: sanctioned,
		OutstandingBalance:  outstanding,
		InterestRatePct:     ratePct,
		EMIAmount:           emiAmt,
		TenureMonths:        raw.TenureMonths,
		DisbursementDate:    disbDate,
		StatementDate:       stmtDate,
		NextEMIDate:         nextEMI,
		BalanceDateTime:     balAt,
		Status:              status,
	}, currency, nil
}

func parseLoanTransactions(wrap *rawTxnsWrap) (*time.Time, []LoanTransaction, error) {
	if wrap == nil {
		return nil, nil, nil
	}

	startDate, err := parseOptionalDate(wrap.StartDate, "Transactions.startDate")
	if err != nil {
		return nil, nil, err
	}
	if _, err := parseOptionalDate(wrap.EndDate, "Transactions.endDate"); err != nil {
		return nil, nil, err
	}

	txns := make([]LoanTransaction, 0, len(wrap.Transaction))
	for i, rawTxn := range wrap.Transaction {
		parsedTxn, err := parseLoanTransaction(rawTxn)
		if err != nil {
			if errors.Is(err, ErrInvalidEMISplit) {
				return nil, nil, fmt.Errorf("Transaction[%d]: %w", i, err)
			}
			return nil, nil, fmt.Errorf("%w: Transaction[%d]: %v", ErrSchemaValidation, i, err)
		}
		txns = append(txns, parsedTxn)
	}

	sort.SliceStable(txns, func(i, j int) bool {
		return txns[i].Timestamp.Before(txns[j].Timestamp)
	})

	return startDate, txns, nil
}

func parseLoanTransaction(raw rawLoanTxn) (LoanTransaction, error) {
	txnID := strings.TrimSpace(raw.TxnID)
	if txnID == "" {
		return LoanTransaction{}, fmt.Errorf("missing txnId")
	}

	txnType := strings.ToUpper(strings.TrimSpace(raw.Type))
	switch txnType {
	case "DISBURSEMENT", "EMI_REPAYMENT", "INTEREST_ADJUSTMENT", "FEE_CHARGE":
	default:
		return LoanTransaction{}, fmt.Errorf("invalid loan transaction type %q", raw.Type)
	}

	amt, err := money.ParseDecimal4JSON(raw.Amount)
	if err != nil {
		return LoanTransaction{}, fmt.Errorf("invalid amount: %w", err)
	}
	if amt <= 0 {
		return LoanTransaction{}, fmt.Errorf("amount must be positive, got %s", amt.String())
	}

	ts, err := parseTimestamp(raw.TransactionTimestamp)
	if err != nil {
		return LoanTransaction{}, fmt.Errorf("invalid transactionTimestamp: %w", err)
	}

	valueDate, err := parseOptionalDate(raw.ValueDate, "valueDate")
	if err != nil {
		return LoanTransaction{}, err
	}
	if valueDate != nil {
		utc := valueDate.UTC()
		valueDate = &utc
	}

	transactionalBal, err := parseOptionalNonNegDecimal4(raw.TransactionalBalance, "transactionalBalance")
	if err != nil {
		return LoanTransaction{}, err
	}

	var principal, interest, fee money.Decimal4
	if txnType == "EMI_REPAYMENT" {
		principal, interest, fee, err = resolveEMISplit(amt, raw.PrincipalComponent, raw.InterestComponent, raw.FeeComponent)
		if err != nil {
			return LoanTransaction{}, err
		}
	}

	metaMap := map[string]any{
		"txnId": txnID,
	}
	if transactionalBal != nil {
		metaMap["transactionalBalance"] = transactionalBal.String()
	}
	if txnType == "EMI_REPAYMENT" {
		metaMap["principalComponent"] = principal.String()
		metaMap["interestComponent"] = interest.String()
		metaMap["feeComponent"] = fee.String()
	}
	metaBytes, _ := json.Marshal(metaMap)

	return LoanTransaction{
		TxnID:                txnID,
		Type:                 txnType,
		Amount:               amt,
		PrincipalComponent:   principal,
		InterestComponent:    interest,
		FeeComponent:         fee,
		TransactionalBalance: transactionalBal,
		Timestamp:            ts,
		ValueDate:            valueDate,
		Narration:            strings.TrimSpace(raw.Narration),
		Reference:            strings.TrimSpace(raw.Reference),
		IdempotencyKey:       LoanTxnIdempotencyKey(txnID),
		RawMetadata:          metaBytes,
	}, nil
}

// resolveEMISplit validates EMI_REPAYMENT component sums against amount.
// When all three split fields are omitted/null, the repayment defaults to 100% principal.
func resolveEMISplit(
	amount money.Decimal4,
	rawPrincipal, rawInterest, rawFee json.RawMessage,
) (money.Decimal4, money.Decimal4, money.Decimal4, error) {
	hasP := hasJSONValue(rawPrincipal)
	hasI := hasJSONValue(rawInterest)
	hasF := hasJSONValue(rawFee)

	if !hasP && !hasI && !hasF {
		return amount, 0, 0, nil
	}

	principal, err := parseOptionalSplitComponent(rawPrincipal, "principalComponent")
	if err != nil {
		return 0, 0, 0, err
	}
	interest, err := parseOptionalSplitComponent(rawInterest, "interestComponent")
	if err != nil {
		return 0, 0, 0, err
	}
	fee, err := parseOptionalSplitComponent(rawFee, "feeComponent")
	if err != nil {
		return 0, 0, 0, err
	}

	sum, err := principal.Add(interest)
	if err == nil {
		sum, err = sum.Add(fee)
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: component sum overflow: %v", ErrInvalidEMISplit, err)
	}
	if sum != amount {
		return 0, 0, 0, fmt.Errorf("%w: principal (%s) + interest (%s) + fee (%s) = %s != amount (%s)",
			ErrInvalidEMISplit, principal.String(), interest.String(), fee.String(), sum.String(), amount.String())
	}

	return principal, interest, fee, nil
}

func parseOptionalSplitComponent(raw json.RawMessage, field string) (money.Decimal4, error) {
	v, err := parseNonNegDecimal4Field(raw, field, ErrInvalidEMISplit)
	if err != nil || v == nil {
		return 0, err
	}
	return *v, nil
}

func parseOptionalNonNegDecimal4(raw json.RawMessage, field string) (*money.Decimal4, error) {
	return parseNonNegDecimal4Field(raw, field, ErrSchemaValidation)
}

func parseNonNegDecimal4Field(raw json.RawMessage, field string, sentinel error) (*money.Decimal4, error) {
	if !hasJSONValue(raw) {
		return nil, nil
	}
	v, err := money.ParseDecimal4JSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid %s: %v", sentinel, field, err)
	}
	if v < 0 {
		return nil, fmt.Errorf("%w: %s must be non-negative, got %s", sentinel, field, v.String())
	}
	return &v, nil
}

func parseOptionalDate(raw, field string) (*time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	ts, err := parseTimestamp(s)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid %s: %v", ErrSchemaValidation, field, err)
	}
	return &ts, nil
}

var istLocation = time.FixedZone("IST", 5*3600+30*60)

func parseTimestamp(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, istLocation); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", raw)
}

func hasJSONValue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
