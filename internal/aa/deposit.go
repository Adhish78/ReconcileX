// Package aa parses and validates ReBIT Account Aggregator (AA) Financial Information
// payloads into normalized domain structs backed by fixed-point money.Decimal4.
//
// Invoked asynchronously by internal/worker after an ingestion job is claimed via
// SKIP LOCKED, keeping HTTP ingestion (POST /api/v1/ingest) fast and decoupled from
// payload-level schema validation.
package aa

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

// ErrSchemaValidation marks a permanent ReBIT AA payload validation failure.
// The worker inspects errors.Is(err, ErrSchemaValidation) and moves failing jobs
// straight to DEAD_LETTER with last_error_code = ERR_SCHEMA_VALIDATION.
var ErrSchemaValidation = errors.New("ERR_SCHEMA_VALIDATION")

// -----------------------------------------------------------------------------
// Normalized domain output types
// -----------------------------------------------------------------------------

// DepositPayload holds the validated Financial Account metadata and normalized
// Ledger Transaction items extracted from a ReBIT AA DEPOSIT payload.
type DepositPayload struct {
	FipID                 string
	InstitutionName       string
	NaturalKey            string
	LinkedAccRef          string
	MaskedAccNumber       string
	Currency              string
	HoldersType           string
	Holders               []DepositHolder
	ReportedBalance       *money.Decimal4
	ReportedBalanceAt     *time.Time
	TransactionsStartDate *time.Time
	Transactions          []DepositTransaction
}

// DepositHolder holds normalized ReBIT AA Profile.Holders.Holder attributes.
// Fields may be omitted or masked by FIPs.
type DepositHolder struct {
	Name           string
	DOB            string
	Mobile         string
	PAN            string
	Email          string
	CKYCRegistered string
	Nominee        string
	Address        string
}

// DepositTransaction represents a single normalized bank deposit event ready for
// double-entry posting on ASSET:BANK vs EQUITY:EXTERNAL_CLEARING.
type DepositTransaction struct {
	TxnID                string
	TxnType              string // CREDIT or DEBIT
	Mode                 string
	SignedAmount         money.Decimal4
	TransactionalBalance *money.Decimal4
	Timestamp            time.Time
	ValueDate            *time.Time
	Narration            string
	Reference            string
	IdempotencyKey       string
	RawMetadata          []byte
}

// -----------------------------------------------------------------------------
// ReBIT AA raw JSON wire structs
// -----------------------------------------------------------------------------

// flexSlice decodes either a JSON array `[T, ...]` or a single JSON object `T`
// (common in XML-to-JSON FIP serializers when a collection has cardinality 1).
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

// flexString decodes JSON strings or numbers into a trimmed string so ReBIT
// XSD attributes typed as xs:integer (such as Holder.mobile) parse uniformly.
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
	if trimmed[0] >= '0' && trimmed[0] <= '9' || trimmed[0] == '-' {
		var num json.Number
		if err := json.Unmarshal(trimmed, &num); err == nil {
			*f = flexString(strings.TrimSpace(num.String()))
			return nil
		}
	}
	return fmt.Errorf("expected JSON string or number")
}

// Wire structs mirror the ReBIT AA DEPOSIT Financial Information schema.
// Go's encoding/json matches struct tags case-insensitively (normalizing PascalCase
// vs lowercase element keys), while json.RawMessage and flexSlice handle string/numeric
// decimals and single-object vs array multiplicity.
type rawDepositEnvelope struct {
	FipID           string          `json:"fipId"`
	InstitutionName string          `json:"institutionName"`
	Account         *rawDepositAcct `json:"Account"`
}

type rawDepositAcct struct {
	Type            string              `json:"type"`
	MaskedAccNumber string              `json:"maskedAccNumber"`
	Version         string              `json:"version"`
	LinkedAccRef    string              `json:"linkedAccRef"`
	Profile         *rawDepositProfile  `json:"Profile"`
	Summary         *rawDepositSummary  `json:"Summary"`
	Transactions    *rawDepositTxnsWrap `json:"Transactions"`
}

type rawDepositProfile struct {
	Holders *rawDepositHolders `json:"Holders"`
}

type rawDepositHolders struct {
	Type   string                      `json:"type"`
	Holder flexSlice[rawDepositHolder] `json:"Holder"`
}

type rawDepositHolder struct {
	Name           string     `json:"name"`
	DOB            string     `json:"dob"`
	Mobile         flexString `json:"mobile"`
	PAN            string     `json:"pan"`
	Email          string     `json:"email"`
	CKYCRegistered string     `json:"ckycRegistered"`
	Nominee        string     `json:"nominee"`
	Address        string     `json:"address"`
}

type rawDepositSummary struct {
	CurrentBalance  json.RawMessage `json:"currentBalance"`
	Currency        string          `json:"currency"`
	BalanceDateTime string          `json:"balanceDateTime"`
	Branch          string          `json:"branch"`
}

type rawDepositTxnsWrap struct {
	StartDate   string                   `json:"startDate"`
	Transaction flexSlice[rawDepositTxn] `json:"Transaction"`
}

type rawDepositTxn struct {
	TxnID                string          `json:"txnId"`
	Type                 string          `json:"type"`
	Mode                 string          `json:"mode"`
	Amount               json.RawMessage `json:"amount"`
	TransactionalBalance json.RawMessage `json:"transactionalBalance"`
	TransactionTimestamp string          `json:"transactionTimestamp"`
	ValueDate            string          `json:"valueDate"`
	Narration            string          `json:"narration"`
	Reference            string          `json:"reference"`
}

// -----------------------------------------------------------------------------
// Parser & validation
// -----------------------------------------------------------------------------

// DepositTxnIdempotencyKey computes the deterministic per-transaction SHA-256 key
// for AA DEPOSIT rows: SHA256("AA_DEPOSIT|" + txnId). Overlapping AA fetch
// windows produce the exact same key so unique (account_id, idempotency_key)
// deduplicates re-ingested bank events safely.
func DepositTxnIdempotencyKey(txnID string) string {
	sum := sha256.Sum256([]byte("AA_DEPOSIT|" + strings.TrimSpace(txnID)))
	return hex.EncodeToString(sum[:])
}

// DepositAnchorIdempotencyKey computes the deterministic SHA-256 idempotency key
// for an AA DEPOSIT Opening Anchor: SHA256("AA_DEPOSIT|OPENING_ANCHOR|" + naturalKey + "|").
func DepositAnchorIdempotencyKey(naturalKey string) string {
	sum := sha256.Sum256([]byte("AA_DEPOSIT|OPENING_ANCHOR|" + strings.TrimSpace(naturalKey) + "|"))
	return hex.EncodeToString(sum[:])
}

// ParseDeposit parses and validates a ReBIT AA DEPOSIT JSON payload.
// Every structural or semantic validation error wraps ErrSchemaValidation so the
// worker never retries permanently malformed payloads.
func ParseDeposit(raw []byte) (DepositPayload, error) {
	// Step 1: Decode outer envelope and require an Account object of type "deposit".
	var env rawDepositEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return DepositPayload{}, fmt.Errorf("%w: invalid json: %v", ErrSchemaValidation, err)
	}
	if env.Account == nil {
		return DepositPayload{}, fmt.Errorf("%w: missing Account object", ErrSchemaValidation)
	}

	acct := env.Account
	if !strings.EqualFold(strings.TrimSpace(acct.Type), "deposit") {
		return DepositPayload{}, fmt.Errorf("%w: Account.type must be deposit, got %q", ErrSchemaValidation, acct.Type)
	}

	// Step 2: Derive the Financial Account natural key from fipId + linkedAccRef.
	linkedRef := strings.TrimSpace(acct.LinkedAccRef)
	if linkedRef == "" {
		return DepositPayload{}, fmt.Errorf("%w: Account.linkedAccRef is required", ErrSchemaValidation)
	}

	fipID := strings.TrimSpace(env.FipID)
	if fipID == "" {
		fipID = "UNKNOWN-FIP"
	}

	// Fall back to Summary.branch or fipID when institutionName is omitted.
	instName := strings.TrimSpace(env.InstitutionName)
	if instName == "" && acct.Summary != nil {
		instName = strings.TrimSpace(acct.Summary.Branch)
	}
	if instName == "" {
		instName = fipID
	}

	// Step 2b: Normalize optional Profile.Holders metadata, tolerating masked or omitted fields.
	var holdersType string
	var holders []DepositHolder
	if acct.Profile != nil && acct.Profile.Holders != nil {
		holdersType = strings.ToUpper(strings.TrimSpace(acct.Profile.Holders.Type))
		holders = make([]DepositHolder, 0, len(acct.Profile.Holders.Holder))
		for _, h := range acct.Profile.Holders.Holder {
			holders = append(holders, DepositHolder{
				Name:           strings.TrimSpace(h.Name),
				DOB:            strings.TrimSpace(h.DOB),
				Mobile:         string(h.Mobile),
				PAN:            strings.TrimSpace(h.PAN),
				Email:          strings.TrimSpace(h.Email),
				CKYCRegistered: strings.TrimSpace(h.CKYCRegistered),
				Nominee:        strings.TrimSpace(h.Nominee),
				Address:        strings.TrimSpace(h.Address),
			})
		}
	}

	// Step 3: Validate optional Summary currency (INR only) and reported balance snapshot.
	currency := "INR"
	var reportedBal *money.Decimal4
	var reportedBalAt *time.Time
	if acct.Summary != nil {
		if c := strings.TrimSpace(acct.Summary.Currency); c != "" {
			if !strings.EqualFold(c, "INR") {
				return DepositPayload{}, fmt.Errorf("%w: unsupported currency %q (only INR is supported)", ErrSchemaValidation, c)
			}
			currency = "INR"
		}
		if hasJSONValue(acct.Summary.CurrentBalance) {
			bal, err := money.ParseDecimal4JSON(acct.Summary.CurrentBalance)
			if err != nil {
				return DepositPayload{}, fmt.Errorf("%w: invalid Summary.currentBalance: %v", ErrSchemaValidation, err)
			}
			reportedBal = &bal
		}
		if tsRaw := strings.TrimSpace(acct.Summary.BalanceDateTime); tsRaw != "" {
			ts, err := parseTimestamp(tsRaw)
			if err != nil {
				return DepositPayload{}, fmt.Errorf("%w: invalid Summary.balanceDateTime: %v", ErrSchemaValidation, err)
			}
			reportedBalAt = &ts
		}
	}

	// Step 4: Parse optional statement start date and normalize every Transaction row;
	// fail the entire batch if any item is malformed so partial AA payloads are never half-posted.
	var startDate *time.Time
	var txns []DepositTransaction
	if acct.Transactions != nil {
		if s := strings.TrimSpace(acct.Transactions.StartDate); s != "" {
			ts, err := parseTimestamp(s)
			if err != nil {
				return DepositPayload{}, fmt.Errorf("%w: invalid Transactions.startDate: %v", ErrSchemaValidation, err)
			}
			startDate = &ts
		}

		txns = make([]DepositTransaction, 0, len(acct.Transactions.Transaction))
		for i, rawTxn := range acct.Transactions.Transaction {
			parsedTxn, err := parseDepositTransaction(rawTxn)
			if err != nil {
				return DepositPayload{}, fmt.Errorf("%w: Transaction[%d]: %v", ErrSchemaValidation, i, err)
			}
			txns = append(txns, parsedTxn)
		}
		// Ensure Transactions[0] is always the earliest chronological event T_0.
		sort.SliceStable(txns, func(i, j int) bool {
			return txns[i].Timestamp.Before(txns[j].Timestamp)
		})
	}

	return DepositPayload{
		FipID:                 fipID,
		InstitutionName:       instName,
		NaturalKey:            fipID + "|" + linkedRef,
		LinkedAccRef:          linkedRef,
		MaskedAccNumber:       strings.TrimSpace(acct.MaskedAccNumber),
		Currency:              currency,
		HoldersType:           holdersType,
		Holders:               holders,
		ReportedBalance:       reportedBal,
		ReportedBalanceAt:     reportedBalAt,
		TransactionsStartDate: startDate,
		Transactions:          txns,
	}, nil
}

// parseDepositTransaction validates a single ReBIT AA Transaction object and maps
// its unsigned amount + CREDIT/DEBIT direction onto a signed ASSET:BANK delta.
func parseDepositTransaction(raw rawDepositTxn) (DepositTransaction, error) {
	txnID := strings.TrimSpace(raw.TxnID)
	if txnID == "" {
		return DepositTransaction{}, fmt.Errorf("missing txnId")
	}

	txnType := strings.ToUpper(strings.TrimSpace(raw.Type))
	if txnType != "CREDIT" && txnType != "DEBIT" {
		return DepositTransaction{}, fmt.Errorf("invalid transaction type %q", raw.Type)
	}

	// ReBIT transaction amounts are strictly positive; direction is carried by Type.
	amt, err := money.ParseDecimal4JSON(raw.Amount)
	if err != nil {
		return DepositTransaction{}, fmt.Errorf("invalid amount: %w", err)
	}
	if amt <= 0 {
		return DepositTransaction{}, fmt.Errorf("amount must be positive, got %s", amt.String())
	}

	// CREDIT increases ASSET:BANK (+amount); DEBIT reduces ASSET:BANK (-amount).
	signed := amt
	if txnType == "DEBIT" {
		signed = amt.Neg()
	}

	ts, err := parseTimestamp(raw.TransactionTimestamp)
	if err != nil {
		return DepositTransaction{}, fmt.Errorf("invalid transactionTimestamp: %w", err)
	}

	var valueDate *time.Time
	if strings.TrimSpace(raw.ValueDate) != "" {
		vd, err := parseTimestamp(raw.ValueDate)
		if err != nil {
			return DepositTransaction{}, fmt.Errorf("invalid valueDate: %w", err)
		}
		d := vd.UTC()
		valueDate = &d
	}

	var transactionalBal *money.Decimal4
	if hasJSONValue(raw.TransactionalBalance) {
		tb, err := money.ParseDecimal4JSON(raw.TransactionalBalance)
		if err != nil {
			return DepositTransaction{}, fmt.Errorf("invalid transactionalBalance: %w", err)
		}
		transactionalBal = &tb
	}

	// Preserve non-accounting ReBIT attributes in raw_metadata for auditability.
	mode := strings.TrimSpace(raw.Mode)
	metaMap := map[string]any{
		"txnId": txnID,
	}
	if mode != "" {
		metaMap["mode"] = mode
	}
	if transactionalBal != nil {
		metaMap["transactionalBalance"] = transactionalBal.String()
	}
	metaBytes, _ := json.Marshal(metaMap)

	return DepositTransaction{
		TxnID:                txnID,
		TxnType:              txnType,
		Mode:                 mode,
		SignedAmount:         signed,
		TransactionalBalance: transactionalBal,
		Timestamp:            ts,
		ValueDate:            valueDate,
		Narration:            strings.TrimSpace(raw.Narration),
		Reference:            strings.TrimSpace(raw.Reference),
		IdempotencyKey:       DepositTxnIdempotencyKey(txnID),
		RawMetadata:          metaBytes,
	}, nil
}

// istLocation anchors timezone-less ISO dates and date-times to IST (+05:30)
// to match Indian bank statement calendar boundaries.
var istLocation = time.FixedZone("IST", 5*3600+30*60)

// parseTimestamp accepts RFC3339 timestamps as well as timezone-less ISO-8601
// date-times and calendar dates commonly emitted by Indian FIPs (interpreted in IST).
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

// hasJSONValue reports whether a json.RawMessage field contains a non-empty, non-null value.
func hasJSONValue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
