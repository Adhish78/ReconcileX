// ReBIT Account Aggregator (AA) EQUITIES v1.0.0 payload parser and validator.
//
// Normalizes ReBIT key-casing and single-object vs array multiplicity variations
// (Holder, Holding, Transaction), converts string/numeric decimals via fixed-point
// money.Decimal4 / money.Decimal6, and maps BUY/SELL/BONUS/SPLIT/RIGHTS/DIVIDEND
// onto signed monetary and unit deltas with deterministic idempotency keys.
package aa

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Adhish78/ReconcileX/internal/money"
)

// -----------------------------------------------------------------------------
// Normalized domain output types
// -----------------------------------------------------------------------------

// EquitiesPayload holds the validated demat Financial Account metadata,
// current holdings snapshot, and chronologically sorted equity transactions.
type EquitiesPayload struct {
	FipID                 string
	InstitutionName       string
	NaturalKey            string
	LinkedAccRef          string
	MaskedDematID         string
	Currency              string
	Holders               []EquitiesHolder
	HoldingMode           string // DEMAT or PHYSICAL
	HasHoldings           bool
	Holdings              []EquityHolding
	ReportedValue         *money.Decimal4
	TransactionsStartDate *time.Time
	Transactions          []EquitiesTransaction
}

// EquitiesHolder holds normalized ReBIT EQUITIES Profile.Holders.Holder attributes.
type EquitiesHolder struct {
	Name          string
	DOB           string
	Mobile        string
	DematID       string
	Address       string
	PAN           string
	Nominee       string
	Landline      string
	Email         string
	KYCCompliance string
}

// EquityHolding represents one ISIN snapshot inside Summary.Investment.Holdings.Holding[].
type EquityHolding struct {
	IssuerName      string
	ISIN            string
	ISINDescription string
	Units           money.Decimal6
	LastTradedPrice *money.Decimal4
}

// EquitiesTransaction represents one normalized demat equity event ready for
// double-entry posting on ASSET:EQUITY_COST / EQUITY:EXTERNAL_CLEARING / INCOME:SALARY_DIVIDEND.
type EquitiesTransaction struct {
	TxnID           string
	OrderID         string
	CompanyName     string
	Timestamp       time.Time
	Exchange        string
	ISIN            string
	ISINDescription string
	Narration       string
	Rate            *money.Decimal4
	Type            string // BUY, SELL, BONUS, SPLIT, DIVIDEND, RIGHTS, OTHERS
	Units           money.Decimal6
	SignedAmount    money.Decimal4
	SignedUnits     money.Decimal6
	IdempotencyKey  string
	RawMetadata     []byte
}

// -----------------------------------------------------------------------------
// ReBIT AA EQUITIES v1.0.0 raw JSON wire structs
// -----------------------------------------------------------------------------

type rawEquitiesEnvelope struct {
	FipID           string           `json:"fipId"`
	InstitutionName string           `json:"institutionName"`
	Account         *rawEquitiesAcct `json:"Account"`
}

type rawEquitiesAcct struct {
	Type            string               `json:"type"`
	MaskedDematID   string               `json:"maskedDematId"`
	MaskedAccNumber string               `json:"maskedAccNumber"` // fallback for older draft variants
	Version         string               `json:"version"`
	LinkedAccRef    string               `json:"linkedAccRef"`
	Profile         *rawEquitiesProfile  `json:"Profile"`
	Summary         *rawEquitiesSummary  `json:"Summary"`
	Transactions    *rawEquitiesTxnsWrap `json:"Transactions"`
}

type rawEquitiesProfile struct {
	Holders *rawEquitiesHolders `json:"Holders"`
}

type rawEquitiesHolders struct {
	Holder flexSlice[rawEquitiesHolder] `json:"Holder"`
}

type rawEquitiesHolder struct {
	Name          string          `json:"name"`
	DOB           string          `json:"dob"`
	Mobile        flexString      `json:"mobile"`
	DematID       string          `json:"dematId"`
	Address       string          `json:"address"`
	PAN           string          `json:"pan"`
	Nominee       string          `json:"nominee"`
	Landline      flexString      `json:"landline"`
	Email         string          `json:"email"`
	KYCCompliance json.RawMessage `json:"kycCompliance"`
}

type rawEquitiesSummary struct {
	CurrentValue json.RawMessage       `json:"currentValue"`
	Investment   *rawEquitiesInvest    `json:"Investment"`
}

type rawEquitiesInvest struct {
	Holdings *rawEquitiesHoldings `json:"Holdings"`
}

type rawEquitiesHoldings struct {
	Type    string                    `json:"type"`
	Holding flexSlice[rawEquityHolding] `json:"Holding"`
}

type rawEquityHolding struct {
	IssuerName      string          `json:"issuerName"`
	ISIN            string          `json:"isin"`
	ISINDescription string          `json:"isinDescription"`
	Units           json.RawMessage `json:"units"`
	LastTradedPrice json.RawMessage `json:"lastTradedPrice"`
}

type rawEquitiesTxnsWrap struct {
	StartDate   string                     `json:"startDate"`
	EndDate     string                     `json:"endDate"`
	Transaction flexSlice[rawEquitiesTxn]  `json:"Transaction"`
}

type rawEquitiesTxn struct {
	TxnID               string          `json:"txnId"`
	OrderID             string          `json:"orderId"`
	CompanyName         string          `json:"companyName"`
	TransactionDateTime string          `json:"transactionDateTime"`
	Exchange            string          `json:"exchange"`
	ISIN                string          `json:"isin"`
	ISINDescription     string          `json:"isinDescription"`
	EquityCategory      string          `json:"equityCategory"`
	Narration           string          `json:"narration"`
	Rate                json.RawMessage `json:"rate"`
	Type                string          `json:"type"`
	Units               json.RawMessage `json:"units"`
}

// -----------------------------------------------------------------------------
// Deterministic idempotency keys & parser
// -----------------------------------------------------------------------------

// EquitiesTxnIdempotencyKey computes the deterministic per-transaction SHA-256 key
// for AA EQUITIES rows: SHA256("AA_EQUITIES|" + txnId).
func EquitiesTxnIdempotencyKey(txnID string) string {
	sum := sha256.Sum256([]byte("AA_EQUITIES|" + strings.TrimSpace(txnID)))
	return hex.EncodeToString(sum[:])
}

// EquitiesAnchorIdempotencyKey computes the deterministic per-ISIN SHA-256 key
// for an AA EQUITIES Opening Anchor: SHA256("AA_EQUITIES|OPENING_ANCHOR|" + naturalKey + "|" + isin).
func EquitiesAnchorIdempotencyKey(naturalKey, isin string) string {
	sum := sha256.Sum256([]byte(
		"AA_EQUITIES|OPENING_ANCHOR|" + strings.TrimSpace(naturalKey) + "|" + strings.ToUpper(strings.TrimSpace(isin)),
	))
	return hex.EncodeToString(sum[:])
}

// ParseEquities parses and validates a ReBIT AA EQUITIES v1.0.0 JSON payload.
// All structural or semantic defects wrap ErrSchemaValidation so the worker
// transitions malformed payloads directly to DEAD_LETTER.
func ParseEquities(raw []byte) (EquitiesPayload, error) {
	var env rawEquitiesEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return EquitiesPayload{}, fmt.Errorf("%w: invalid json: %v", ErrSchemaValidation, err)
	}
	if env.Account == nil {
		return EquitiesPayload{}, fmt.Errorf("%w: missing Account object", ErrSchemaValidation)
	}

	acct := env.Account
	if !strings.EqualFold(strings.TrimSpace(acct.Type), "equities") {
		return EquitiesPayload{}, fmt.Errorf("%w: Account.type must be equities, got %q", ErrSchemaValidation, acct.Type)
	}

	linkedRef := strings.TrimSpace(acct.LinkedAccRef)
	if linkedRef == "" {
		return EquitiesPayload{}, fmt.Errorf("%w: Account.linkedAccRef is required", ErrSchemaValidation)
	}

	fipID := strings.TrimSpace(env.FipID)
	if fipID == "" {
		fipID = "UNKNOWN-FIP"
	}

	instName := strings.TrimSpace(env.InstitutionName)
	if instName == "" {
		instName = fipID
	}

	maskedDemat := strings.TrimSpace(acct.MaskedDematID)
	if maskedDemat == "" {
		maskedDemat = strings.TrimSpace(acct.MaskedAccNumber)
	}

	var holders []EquitiesHolder
	if acct.Profile != nil && acct.Profile.Holders != nil {
		holders = make([]EquitiesHolder, 0, len(acct.Profile.Holders.Holder))
		for _, h := range acct.Profile.Holders.Holder {
			holders = append(holders, EquitiesHolder{
				Name:          strings.TrimSpace(h.Name),
				DOB:           strings.TrimSpace(h.DOB),
				Mobile:        string(h.Mobile),
				DematID:       strings.TrimSpace(h.DematID),
				Address:       strings.TrimSpace(h.Address),
				PAN:           strings.TrimSpace(h.PAN),
				Nominee:       strings.TrimSpace(h.Nominee),
				Landline:      string(h.Landline),
				Email:         strings.TrimSpace(h.Email),
				KYCCompliance: parseFlexibleScalar(h.KYCCompliance),
			})
		}
	}

	var reportedVal *money.Decimal4
	holdingMode := "DEMAT"
	hasHoldings := false
	var holdings []EquityHolding
	if acct.Summary != nil {
		if hasJSONValue(acct.Summary.CurrentValue) {
			val, err := money.ParseDecimal4JSON(acct.Summary.CurrentValue)
			if err != nil {
				return EquitiesPayload{}, fmt.Errorf("%w: invalid Summary.currentValue: %v", ErrSchemaValidation, err)
			}
			if val < 0 {
				return EquitiesPayload{}, fmt.Errorf("%w: Summary.currentValue must be non-negative", ErrSchemaValidation)
			}
			reportedVal = &val
		}

		if acct.Summary.Investment != nil && acct.Summary.Investment.Holdings != nil {
			hasHoldings = true
			hWrap := acct.Summary.Investment.Holdings
			if m := strings.ToUpper(strings.TrimSpace(hWrap.Type)); m != "" {
				if m != "DEMAT" && m != "PHYSICAL" {
					return EquitiesPayload{}, fmt.Errorf("%w: invalid Holdings.type %q", ErrSchemaValidation, hWrap.Type)
				}
				holdingMode = m
			}
			holdings = make([]EquityHolding, 0, len(hWrap.Holding))
			for i, rawH := range hWrap.Holding {
				parsedH, err := parseEquityHolding(rawH)
				if err != nil {
					return EquitiesPayload{}, fmt.Errorf("%w: Holding[%d]: %v", ErrSchemaValidation, i, err)
				}
				holdings = append(holdings, parsedH)
			}
		}
	}

	var startDate *time.Time
	var txns []EquitiesTransaction
	if acct.Transactions != nil {
		if s := strings.TrimSpace(acct.Transactions.StartDate); s != "" {
			ts, err := parseTimestamp(s)
			if err != nil {
				return EquitiesPayload{}, fmt.Errorf("%w: invalid Transactions.startDate: %v", ErrSchemaValidation, err)
			}
			startDate = &ts
		}
		if e := strings.TrimSpace(acct.Transactions.EndDate); e != "" {
			if _, err := parseTimestamp(e); err != nil {
				return EquitiesPayload{}, fmt.Errorf("%w: invalid Transactions.endDate: %v", ErrSchemaValidation, err)
			}
		}

		txns = make([]EquitiesTransaction, 0, len(acct.Transactions.Transaction))
		for i, rawTxn := range acct.Transactions.Transaction {
			parsedTxn, err := parseEquitiesTransaction(rawTxn)
			if err != nil {
				return EquitiesPayload{}, fmt.Errorf("%w: Transaction[%d]: %v", ErrSchemaValidation, i, err)
			}
			txns = append(txns, parsedTxn)
		}
		// Sort chronologically so earliest events precede later ones deterministically.
		sort.SliceStable(txns, func(i, j int) bool {
			return txns[i].Timestamp.Before(txns[j].Timestamp)
		})
	}

	return EquitiesPayload{
		FipID:                 fipID,
		InstitutionName:       instName,
		NaturalKey:            fipID + "|" + linkedRef,
		LinkedAccRef:          linkedRef,
		MaskedDematID:         maskedDemat,
		Currency:              "INR",
		Holders:               holders,
		HoldingMode:           holdingMode,
		HasHoldings:           hasHoldings,
		Holdings:              holdings,
		ReportedValue:         reportedVal,
		TransactionsStartDate: startDate,
		Transactions:          txns,
	}, nil
}

func parseEquityHolding(raw rawEquityHolding) (EquityHolding, error) {
	isin := strings.ToUpper(strings.TrimSpace(raw.ISIN))
	if isin == "" {
		return EquityHolding{}, fmt.Errorf("missing isin")
	}

	units, err := money.ParseDecimal6JSON(raw.Units)
	if err != nil {
		return EquityHolding{}, fmt.Errorf("invalid units: %w", err)
	}
	if units < 0 {
		return EquityHolding{}, fmt.Errorf("units must be non-negative, got %s", units.String())
	}

	var ltp *money.Decimal4
	if hasJSONValue(raw.LastTradedPrice) {
		p, err := money.ParseDecimal4JSON(raw.LastTradedPrice)
		if err != nil {
			return EquityHolding{}, fmt.Errorf("invalid lastTradedPrice: %w", err)
		}
		if p < 0 {
			return EquityHolding{}, fmt.Errorf("lastTradedPrice must be non-negative, got %s", p.String())
		}
		ltp = &p
	}

	return EquityHolding{
		IssuerName:      strings.TrimSpace(raw.IssuerName),
		ISIN:            isin,
		ISINDescription: strings.TrimSpace(raw.ISINDescription),
		Units:           units,
		LastTradedPrice: ltp,
	}, nil
}

func parseEquitiesTransaction(raw rawEquitiesTxn) (EquitiesTransaction, error) {
	txnID := strings.TrimSpace(raw.TxnID)
	if txnID == "" {
		return EquitiesTransaction{}, fmt.Errorf("missing txnId")
	}

	isin := strings.ToUpper(strings.TrimSpace(raw.ISIN))
	if isin == "" {
		return EquitiesTransaction{}, fmt.Errorf("missing isin")
	}

	// Trim whitespace on enum values because ReBIT equities.xsd contains " OTHERS".
	txnType := strings.ToUpper(strings.TrimSpace(raw.Type))
	switch txnType {
	case "BUY", "SELL", "BONUS", "SPLIT", "DIVIDEND", "RIGHTS", "OTHERS":
	default:
		return EquitiesTransaction{}, fmt.Errorf("invalid transaction type %q", raw.Type)
	}

	ts, err := parseTimestamp(raw.TransactionDateTime)
	if err != nil {
		return EquitiesTransaction{}, fmt.Errorf("invalid transactionDateTime: %w", err)
	}

	units, err := money.ParseDecimal6JSON(raw.Units)
	if err != nil {
		return EquitiesTransaction{}, fmt.Errorf("invalid units: %w", err)
	}
	// SPLIT permits signed non-zero unit deltas (forward or reverse split/consolidation);
	// all other unit-bearing equity transactions require strictly positive units.
	if txnType == "SPLIT" {
		if units == 0 {
			return EquitiesTransaction{}, fmt.Errorf("SPLIT units must be non-zero")
		}
	} else if units <= 0 {
		return EquitiesTransaction{}, fmt.Errorf("units must be positive, got %s", units.String())
	}

	var rate *money.Decimal4
	if hasJSONValue(raw.Rate) {
		r, err := money.ParseDecimal4JSON(raw.Rate)
		if err != nil {
			return EquitiesTransaction{}, fmt.Errorf("invalid rate: %w", err)
		}
		if r < 0 {
			return EquitiesTransaction{}, fmt.Errorf("rate must be non-negative, got %s", r.String())
		}
		rate = &r
	}

	var signedAmount money.Decimal4
	var signedUnits money.Decimal6

	switch txnType {
	case "BUY", "SELL":
		amt, err := computeEquityTradeAmount(txnType, rate, units)
		if err != nil {
			return EquitiesTransaction{}, err
		}
		signedAmount = amt
		signedUnits = units
		if txnType == "SELL" {
			signedAmount = amt.Neg()
			signedUnits = units.Neg()
		}

	case "BONUS", "SPLIT":
		// Corporate Actions adjust units without synthetic cash (Cost Basis preserved).
		signedAmount = 0
		signedUnits = units

	case "RIGHTS":
		signedUnits = units
		if rate != nil && *rate > 0 {
			amt, err := rate.MulDecimal6(units)
			if err != nil {
				return EquitiesTransaction{}, fmt.Errorf("compute RIGHTS amount: %w", err)
			}
			signedAmount = amt
		}

	case "DIVIDEND":
		// Cash dividend does not alter demat unit count.
		if rate != nil && *rate > 0 {
			amt, err := rate.MulDecimal6(units)
			if err != nil {
				return EquitiesTransaction{}, fmt.Errorf("compute DIVIDEND amount: %w", err)
			}
			signedAmount = amt
		}
		signedUnits = 0

	case "OTHERS":
		signedAmount = 0
		signedUnits = 0
	}

	exchange := strings.ToUpper(strings.TrimSpace(raw.Exchange))
	orderID := strings.TrimSpace(raw.OrderID)
	companyName := strings.TrimSpace(raw.CompanyName)
	isinDesc := strings.TrimSpace(raw.ISINDescription)
	narration := strings.TrimSpace(raw.Narration)

	metaMap := map[string]any{
		"txnId": txnID,
		"isin":  isin,
	}
	if orderID != "" {
		metaMap["orderId"] = orderID
	}
	if companyName != "" {
		metaMap["companyName"] = companyName
	}
	if exchange != "" {
		metaMap["exchange"] = exchange
	}
	if isinDesc != "" {
		metaMap["isinDescription"] = isinDesc
	}
	if rate != nil {
		metaMap["rate"] = rate.String()
	}
	metaBytes, _ := json.Marshal(metaMap)

	return EquitiesTransaction{
		TxnID:           txnID,
		OrderID:         orderID,
		CompanyName:     companyName,
		Timestamp:       ts,
		Exchange:        exchange,
		ISIN:            isin,
		ISINDescription: isinDesc,
		Narration:       narration,
		Rate:            rate,
		Type:            txnType,
		Units:           units,
		SignedAmount:    signedAmount,
		SignedUnits:     signedUnits,
		IdempotencyKey:  EquitiesTxnIdempotencyKey(txnID),
		RawMetadata:     metaBytes,
	}, nil
}

func computeEquityTradeAmount(txnType string, rate *money.Decimal4, units money.Decimal6) (money.Decimal4, error) {
	if rate == nil || *rate <= 0 {
		return 0, fmt.Errorf("%s requires positive rate", txnType)
	}
	amt, err := rate.MulDecimal6(units)
	if err != nil {
		return 0, fmt.Errorf("compute %s amount: %w", txnType, err)
	}
	if amt <= 0 {
		return 0, fmt.Errorf("%s monetary amount must be positive", txnType)
	}
	return amt, nil
}

func parseFlexibleScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return strings.TrimSpace(str)
		}
	}
	return s
}
