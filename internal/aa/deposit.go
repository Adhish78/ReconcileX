// Package aa parses ReBIT Account Aggregator (AA) Financial Information payloads.
package aa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Adhish78/ReconcileX/internal/money"
)

// ErrSchemaValidation indicates a permanent ReBIT AA payload validation error
// that must transition an ingestion_jobs row directly to DEAD_LETTER with ERR_SCHEMA_VALIDATION.
var ErrSchemaValidation = errors.New("ERR_SCHEMA_VALIDATION")

// DepositPayload holds normalized account and transaction data parsed from an AA DEPOSIT payload.
type DepositPayload struct {
	FipID             string
	InstitutionName   string
	NaturalKey        string
	LinkedAccRef      string
	MaskedAccNumber   string
	Currency          string
	ReportedBalance   *money.Decimal4
	ReportedBalanceAt *time.Time
	Transactions      []DepositTransaction
}

// DepositTransaction represents a single normalized bank deposit ledger event.
type DepositTransaction struct {
	TxnID          string
	TxnType        string // CREDIT or DEBIT
	Mode           string
	SignedAmount   money.Decimal4
	Timestamp      time.Time
	ValueDate      *time.Time
	Narration      string
	Reference      string
	IdempotencyKey string
	RawMetadata    []byte
}

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
	Summary         *rawDepositSummary  `json:"Summary"`
	Transactions    *rawDepositTxnsWrap `json:"Transactions"`
}

type rawDepositSummary struct {
	CurrentBalance  json.RawMessage `json:"currentBalance"`
	Currency        string          `json:"currency"`
	BalanceDateTime string          `json:"balanceDateTime"`
	Branch          string          `json:"branch"`
	IFSC            string          `json:"ifsc"`
	AccountSubType  string          `json:"accountSubType"`
}

type rawDepositTxnsWrap struct {
	StartDate   string          `json:"startDate"`
	EndDate     string          `json:"endDate"`
	Transaction []rawDepositTxn `json:"Transaction"`
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

// DepositTxnIdempotencyKey computes the deterministic per-transaction SHA-256 key
// for AA DEPOSIT transactions: SHA256("AA_DEPOSIT|" + txnId).
func DepositTxnIdempotencyKey(txnID string) string {
	sum := sha256.Sum256([]byte("AA_DEPOSIT|" + strings.TrimSpace(txnID)))
	return hex.EncodeToString(sum[:])
}

// ParseDeposit parses and validates a baseline ReBIT AA DEPOSIT JSON payload.
// All schema/validation errors wrap ErrSchemaValidation.
func ParseDeposit(raw []byte) (DepositPayload, error) {
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

	linkedRef := strings.TrimSpace(acct.LinkedAccRef)
	if linkedRef == "" {
		return DepositPayload{}, fmt.Errorf("%w: Account.linkedAccRef is required", ErrSchemaValidation)
	}

	fipID := strings.TrimSpace(env.FipID)
	if fipID == "" {
		fipID = "UNKNOWN-FIP"
	}

	instName := strings.TrimSpace(env.InstitutionName)
	if instName == "" && acct.Summary != nil {
		instName = strings.TrimSpace(acct.Summary.Branch)
	}
	if instName == "" {
		instName = fipID
	}

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

	var txns []DepositTransaction
	if acct.Transactions != nil {
		txns = make([]DepositTransaction, 0, len(acct.Transactions.Transaction))
		for i, rawTxn := range acct.Transactions.Transaction {
			parsedTxn, err := parseDepositTransaction(rawTxn)
			if err != nil {
				return DepositPayload{}, fmt.Errorf("%w: Transaction[%d]: %v", ErrSchemaValidation, i, err)
			}
			txns = append(txns, parsedTxn)
		}
	}

	return DepositPayload{
		FipID:             fipID,
		InstitutionName:   instName,
		NaturalKey:        fipID + "|" + linkedRef,
		LinkedAccRef:      linkedRef,
		MaskedAccNumber:   strings.TrimSpace(acct.MaskedAccNumber),
		Currency:          currency,
		ReportedBalance:   reportedBal,
		ReportedBalanceAt: reportedBalAt,
		Transactions:      txns,
	}, nil
}

func parseDepositTransaction(raw rawDepositTxn) (DepositTransaction, error) {
	txnID := strings.TrimSpace(raw.TxnID)
	if txnID == "" {
		return DepositTransaction{}, fmt.Errorf("missing txnId")
	}

	txnType := strings.ToUpper(strings.TrimSpace(raw.Type))
	if txnType != "CREDIT" && txnType != "DEBIT" {
		return DepositTransaction{}, fmt.Errorf("invalid transaction type %q", raw.Type)
	}

	amt, err := money.ParseDecimal4JSON(raw.Amount)
	if err != nil {
		return DepositTransaction{}, fmt.Errorf("invalid amount: %w", err)
	}
	if amt <= 0 {
		return DepositTransaction{}, fmt.Errorf("amount must be positive, got %s", amt.String())
	}

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

	metaMap := map[string]any{
		"txnId": txnID,
	}
	if mode := strings.TrimSpace(raw.Mode); mode != "" {
		metaMap["mode"] = mode
	}
	if hasJSONValue(raw.TransactionalBalance) {
		tb, err := money.ParseDecimal4JSON(raw.TransactionalBalance)
		if err != nil {
			return DepositTransaction{}, fmt.Errorf("invalid transactionalBalance: %w", err)
		}
		metaMap["transactionalBalance"] = tb.String()
	}
	metaBytes, _ := json.Marshal(metaMap)

	return DepositTransaction{
		TxnID:          txnID,
		TxnType:        txnType,
		Mode:           strings.TrimSpace(raw.Mode),
		SignedAmount:   signed,
		Timestamp:      ts,
		ValueDate:      valueDate,
		Narration:      strings.TrimSpace(raw.Narration),
		Reference:      strings.TrimSpace(raw.Reference),
		IdempotencyKey: DepositTxnIdempotencyKey(txnID),
		RawMetadata:    metaBytes,
	}, nil
}

func parseTimestamp(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", raw)
}

func hasJSONValue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}
