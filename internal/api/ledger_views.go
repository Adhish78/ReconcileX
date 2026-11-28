// Persona-scoped read-only ledger endpoints for ReconcileX.
//
// Owns GET /api/v1/accounts, GET /api/v1/accounts/{id}/transactions, and
// GET /api/v1/portfolio/summary. All balances are computed live at read time from
// ledger_entries using fixed-point NUMERIC(18,4) strings and money.Decimal4
// arithmetic—never cached on accounts (ADR-0003).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Adhish78/ReconcileX/internal/money"
	"github.com/jackc/pgx/v5"
)

// -----------------------------------------------------------------------------
// Response models
// -----------------------------------------------------------------------------

// EquityHoldingSummary represents one per-ISIN equity holding inside an EQUITY
// demat Financial Account, combining live ASSET:EQUITY_COST ledger aggregates
// with reported snapshot metadata.
type EquityHoldingSummary struct {
	ISIN            string  `json:"isin"`
	IssuerName      string  `json:"issuer_name"`
	ISINDescription string  `json:"isin_description"`
	CostBasis       string  `json:"cost_basis"`
	LedgerUnits     string  `json:"ledger_units"`
	ReportedUnits   *string `json:"reported_units"`
	LastTradedPrice *string `json:"last_traded_price"`
}

// AccountSummary represents a Financial Account with its live aggregated ledger balance.
type AccountSummary struct {
	ID                string                 `json:"id"`
	CanonicalUserID   string                 `json:"canonical_user_id"`
	SourceType        string                 `json:"source_type"`
	NaturalKey        string                 `json:"natural_key"`
	InstitutionName   string                 `json:"institution_name"`
	FipID             string                 `json:"fip_id"`
	LinkedAccRef      string                 `json:"linked_acc_ref"`
	MaskedAccNumber   string                 `json:"masked_acc_number"`
	FolioNumber       string                 `json:"folio_number"`
	ISIN              string                 `json:"isin"`
	SchemeCode        *int                   `json:"scheme_code"`
	SchemeName        string                 `json:"scheme_name"`
	Currency          string                 `json:"currency"`
	LedgerBalance     string                 `json:"ledger_balance"`
	LedgerUnits       *string                `json:"ledger_units"`
	ReportedBalance   *string                `json:"reported_balance"`
	ReportedUnits     *string                `json:"reported_units"`
	ReportedBalanceAt *time.Time             `json:"reported_balance_at"`
	OpenAlertCount    int                    `json:"open_alert_count"`
	EquityHoldings    []EquityHoldingSummary `json:"equity_holdings,omitempty"`
}

// LedgerEntryItem represents a single immutable Chart of Accounts leg returned
// inside transaction drilldowns.
type LedgerEntryItem struct {
	ID         string  `json:"id"`
	CoaAccount string  `json:"coa_account"`
	Amount     string  `json:"amount"`
	Units      *string `json:"units"`
}

// LedgerTransactionItem represents a zero-sum Ledger Transaction paired with its
// nested double-entry Ledger Entries.
type LedgerTransactionItem struct {
	ID             string            `json:"id"`
	AccountID      string            `json:"account_id"`
	Source         string            `json:"source"`
	TxnType        string            `json:"txn_type"`
	Category       *string           `json:"category"`
	TxnTimestamp   time.Time         `json:"txn_timestamp"`
	ValueDate      *string           `json:"value_date"`
	Narration      *string           `json:"narration"`
	Reference      *string           `json:"reference"`
	IdempotencyKey string            `json:"idempotency_key"`
	Entries        []LedgerEntryItem `json:"entries"`
}

type personaSummary struct {
	ID          string `json:"id"`
	PersonaSlug string `json:"persona_slug"`
	DisplayName string `json:"display_name"`
}

type accountsListResponse struct {
	Accounts []AccountSummary `json:"accounts"`
}

type accountTransactionsResponse struct {
	AccountID    string                  `json:"account_id"`
	Transactions []LedgerTransactionItem `json:"transactions"`
	Total        int                     `json:"total"`
}

// accountsSummaryBreakdown breaks Net Worth assets and liabilities into fixed-point
// decimal strings by Financial Account container type.
type accountsSummaryBreakdown struct {
	BankDeposits string `json:"bank_deposits"`
	MutualFunds  string `json:"mutual_funds"`
	Equities     string `json:"equities"`
	Loans        string `json:"loans"`
	CreditCards  string `json:"credit_cards"`
}

// portfolioSummaryResponse mirrors the top-level Overview metrics card payload.
type portfolioSummaryResponse struct {
	Persona            personaSummary           `json:"persona"`
	AsOf               string                   `json:"as_of"`
	NetWorth           string                   `json:"net_worth"`
	TotalAssets        string                   `json:"total_assets"`
	TotalLiabilities   string                   `json:"total_liabilities"`
	PortfolioXIRR      *float64                 `json:"portfolio_xirr"`
	AccountsSummary    accountsSummaryBreakdown `json:"accounts_summary"`
	OpenAlertCount     int                      `json:"open_alert_count"`
	UncategorizedCount int                      `json:"uncategorized_count"`
	DLQJobCount        int                      `json:"dlq_job_count"`
}

// -----------------------------------------------------------------------------
// Shared preconditions & persona resolution
// -----------------------------------------------------------------------------

// requirePool returns false and writes 503 ERR_DB_UNAVAILABLE when the handler is
// invoked without an initialized database pool (such as isolated probe tests).
func (s *server) requirePool(w http.ResponseWriter) bool {
	if s.pool == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "database connection is not initialized", nil)
		return false
	}
	return true
}

// resolvePersonaOrError looks up an ACTIVE Canonical User by slug (defaulting to
// "aarav" when omitted) and writes 404 ERR_PERSONA_NOT_FOUND if missing or MERGED.
func (s *server) resolvePersonaOrError(ctx context.Context, w http.ResponseWriter, slug string) (personaSummary, bool) {
	if slug == "" {
		slug = "aarav"
	}

	var p personaSummary
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, persona_slug, display_name
		FROM users
		WHERE persona_slug = $1 AND status = 'ACTIVE'
	`, slug).Scan(&p.ID, &p.PersonaSlug, &p.DisplayName)

	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errCodePersonaNotFound, "persona not found", map[string]any{"persona": slug})
		return personaSummary{}, false
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to resolve persona", map[string]any{"reason": err.Error()})
		return personaSummary{}, false
	}

	return p, true
}

// -----------------------------------------------------------------------------
// Accounts & transaction drilldown handlers
// -----------------------------------------------------------------------------

// handleGetAccounts serves GET /api/v1/accounts?persona=<slug>.
// Aggregates live ledger balances per Financial Account directly from ledger_entries,
// filtering to asset and liability Chart of Accounts buckets so External Clearing
// offsets are excluded.
func (s *server) handleGetAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	persona, ok := s.resolvePersonaOrError(ctx, w, strings.TrimSpace(r.URL.Query().Get("persona")))
	if !ok {
		return
	}

	// LEFT JOIN ledger_entries so accounts with zero posted transactions still appear
	// with "0.0000", and filter coa_account to principal/asset buckets (excluding clearing).
	rows, err := s.pool.Query(ctx, `
		SELECT
			a.id::text,
			a.canonical_user_id::text,
			a.source_type::text,
			a.natural_key,
			a.institution_name,
			COALESCE(a.fip_id, ''),
			COALESCE(a.linked_acc_ref, ''),
			COALESCE(a.masked_acc_number, ''),
			COALESCE(a.folio_number, ''),
			COALESCE(a.isin, ''),
			a.scheme_code,
			COALESCE(a.metadata->>'scheme_name', ''),
			a.currency,
			CASE
				WHEN a.source_type IN ('HOME_LOAN', 'AUTO_LOAN') THEN
					ABS(COALESCE(SUM(le.amount) FILTER (
						WHERE le.coa_account = 'LIABILITY:LOAN'
					), 0))::NUMERIC(18,4)::text
				ELSE
					COALESCE(SUM(le.amount) FILTER (
						WHERE le.coa_account IN ('ASSET:BANK', 'ASSET:MF_COST', 'ASSET:EQUITY_COST', 'LIABILITY:CARD')
					), 0)::NUMERIC(18,4)::text
			END AS ledger_balance,
			CASE
				WHEN a.source_type = 'MUTUAL_FUND' THEN
					COALESCE(SUM(le.units) FILTER (
						WHERE le.coa_account = 'ASSET:MF_COST'
					), 0)::NUMERIC(18,6)::text
				WHEN a.source_type = 'EQUITY' THEN
					COALESCE(SUM(le.units) FILTER (
						WHERE le.coa_account = 'ASSET:EQUITY_COST'
					), 0)::NUMERIC(18,6)::text
				ELSE NULL
			END AS ledger_units,
			a.reported_balance::text,
			a.reported_units::text,
			a.reported_balance_at,
			COALESCE(a.metadata->'holdings', '{}'::jsonb)
		FROM accounts a
		LEFT JOIN ledger_entries le ON le.account_id = a.id
		WHERE a.canonical_user_id = $1
		GROUP BY a.id
		ORDER BY a.created_at ASC, a.institution_name ASC
	`, persona.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query accounts", map[string]any{"reason": err.Error()})
		return
	}
	defer rows.Close()

	accounts := make([]AccountSummary, 0)
	equityIndices := make(map[string]int)
	equityIDs := make([]string, 0)
	holdingsMetaByAcct := make(map[string]map[string]storedEquityHoldingMeta)

	for rows.Next() {
		var a AccountSummary
		var holdingsRaw []byte
		if err := rows.Scan(
			&a.ID, &a.CanonicalUserID, &a.SourceType, &a.NaturalKey,
			&a.InstitutionName, &a.FipID, &a.LinkedAccRef, &a.MaskedAccNumber,
			&a.FolioNumber, &a.ISIN, &a.SchemeCode, &a.SchemeName,
			&a.Currency, &a.LedgerBalance, &a.LedgerUnits, &a.ReportedBalance, &a.ReportedUnits, &a.ReportedBalanceAt,
			&holdingsRaw,
		); err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to scan account", map[string]any{"reason": err.Error()})
			return
		}
		if a.SourceType == "EQUITY" {
			equityIndices[a.ID] = len(accounts)
			equityIDs = append(equityIDs, a.ID)
			if len(holdingsRaw) > 0 {
				var parsedMeta map[string]storedEquityHoldingMeta
				if err := json.Unmarshal(holdingsRaw, &parsedMeta); err == nil && len(parsedMeta) > 0 {
					holdingsMetaByAcct[a.ID] = parsedMeta
				}
			}
		}
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to iterate accounts", map[string]any{"reason": err.Error()})
		return
	}

	if len(equityIDs) > 0 {
		if err := s.hydrateEquityHoldings(ctx, accounts, equityIndices, equityIDs, holdingsMetaByAcct); err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query equity holdings", map[string]any{"reason": err.Error()})
			return
		}
	}

	writeJSON(w, http.StatusOK, accountsListResponse{Accounts: accounts})
}

type storedEquityHoldingMeta struct {
	ISIN            string  `json:"isin"`
	IssuerName      string  `json:"issuer_name"`
	ISINDescription string  `json:"isin_description"`
	Units           string  `json:"units"`
	LastTradedPrice *string `json:"last_traded_price"`
}

func (s *server) hydrateEquityHoldings(
	ctx context.Context,
	accounts []AccountSummary,
	equityIndices map[string]int,
	equityIDs []string,
	holdingsMetaByAcct map[string]map[string]storedEquityHoldingMeta,
) error {
	rows, err := s.pool.Query(ctx, `
		SELECT
			lt.account_id::text,
			lt.raw_metadata->>'isin' AS isin,
			COALESCE(MAX(NULLIF(lt.raw_metadata->>'companyName', '')), '') AS company_name,
			COALESCE(MAX(NULLIF(lt.raw_metadata->>'isinDescription', '')), '') AS isin_desc,
			COALESCE(SUM(le.amount) FILTER (WHERE le.coa_account = 'ASSET:EQUITY_COST'), 0)::NUMERIC(18,4)::text AS cost_basis,
			COALESCE(SUM(le.units) FILTER (WHERE le.coa_account = 'ASSET:EQUITY_COST'), 0)::NUMERIC(18,6)::text AS ledger_units
		FROM ledger_transactions lt
		JOIN ledger_entries le ON le.transaction_id = lt.id
		WHERE lt.account_id = ANY($1::uuid[])
		  AND lt.raw_metadata ? 'isin'
		GROUP BY lt.account_id, lt.raw_metadata->>'isin'
		ORDER BY lt.raw_metadata->>'isin' ASC
	`, equityIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	byAcctISIN := make(map[string]map[string]EquityHoldingSummary, len(equityIDs))
	for rows.Next() {
		var acctID, isin, compName, isinDesc, costBasis, ledgerUnits string
		if err := rows.Scan(&acctID, &isin, &compName, &isinDesc, &costBasis, &ledgerUnits); err != nil {
			return err
		}
		if isin == "" {
			continue
		}
		if byAcctISIN[acctID] == nil {
			byAcctISIN[acctID] = make(map[string]EquityHoldingSummary)
		}
		byAcctISIN[acctID][isin] = EquityHoldingSummary{
			ISIN:            isin,
			IssuerName:      compName,
			ISINDescription: isinDesc,
			CostBasis:       costBasis,
			LedgerUnits:     ledgerUnits,
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for acctID, idx := range equityIndices {
		perISIN := byAcctISIN[acctID]
		if perISIN == nil {
			perISIN = make(map[string]EquityHoldingSummary)
		}
		metaMap := holdingsMetaByAcct[acctID]
		for isin, meta := range metaMap {
			h, exists := perISIN[isin]
			if !exists {
				h = EquityHoldingSummary{
					ISIN:        isin,
					CostBasis:   "0.0000",
					LedgerUnits: "0.000000",
				}
			}
			if meta.IssuerName != "" {
				h.IssuerName = meta.IssuerName
			}
			if meta.ISINDescription != "" {
				h.ISINDescription = meta.ISINDescription
			}
			if meta.Units != "" {
				if parsedU, parseErr := money.ParseDecimal6(meta.Units); parseErr == nil {
					uStr := parsedU.String()
					h.ReportedUnits = &uStr
				}
			}
			if meta.LastTradedPrice != nil && *meta.LastTradedPrice != "" {
				if parsedP, parseErr := money.ParseDecimal4(*meta.LastTradedPrice); parseErr == nil {
					pStr := parsedP.String()
					h.LastTradedPrice = &pStr
				}
			}
			perISIN[isin] = h
		}

		isins := make([]string, 0, len(perISIN))
		for isin := range perISIN {
			isins = append(isins, isin)
		}
		sort.Strings(isins)

		list := make([]EquityHoldingSummary, 0, len(isins))
		for _, isin := range isins {
			list = append(list, perISIN[isin])
		}
		accounts[idx].EquityHoldings = list
	}

	return nil
}

// handleGetAccountTransactions serves GET /api/v1/accounts/{id}/transactions.
// Fetches a paginated slice of Ledger Transactions for the requested Financial Account
// (enforcing optional ?persona= ownership) and batch-hydrates all nested Ledger Entries
// in a second query to avoid N+1 round trips.
func (s *server) handleGetAccountTransactions(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	accountID := strings.TrimSpace(r.PathValue("id"))
	if !isValidUUID(accountID) {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "invalid account id", map[string]any{"id": accountID})
		return
	}

	q := r.URL.Query()
	limit, offset, err := parsePagination(q.Get("limit"), q.Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Step 1: When ?persona= is supplied, ensure the requested account belongs to that
	// Canonical User; cross-persona lookups return 404 rather than leaking existence.
	var userFilter string
	if slug := strings.TrimSpace(q.Get("persona")); slug != "" {
		persona, ok := s.resolvePersonaOrError(ctx, w, slug)
		if !ok {
			return
		}
		userFilter = persona.ID
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM accounts
			WHERE id = $1 AND ($2 = '' OR canonical_user_id::text = $2)
		)
	`, accountID, userFilter).Scan(&exists); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to check account", map[string]any{"reason": err.Error()})
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, errCodeNotFound, "account not found", map[string]any{"id": accountID})
		return
	}

	// Step 2: Count total transactions on the account and fetch the requested page newest-first.
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM ledger_transactions WHERE account_id = $1`, accountID).Scan(&total); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to count transactions", map[string]any{"reason": err.Error()})
		return
	}

	rows, err := s.pool.Query(ctx, `
		SELECT
			id::text,
			account_id::text,
			source,
			txn_type,
			category::text,
			txn_timestamp,
			value_date::text,
			narration,
			reference,
			idempotency_key
		FROM ledger_transactions
		WHERE account_id = $1
		ORDER BY txn_timestamp DESC, created_at DESC
		LIMIT $2 OFFSET $3
	`, accountID, limit, offset)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query transactions", map[string]any{"reason": err.Error()})
		return
	}
	defer rows.Close()

	txns := make([]LedgerTransactionItem, 0)
	txnIDs := make([]string, 0)
	indexByID := make(map[string]int)

	for rows.Next() {
		var item LedgerTransactionItem
		if err := rows.Scan(
			&item.ID, &item.AccountID, &item.Source, &item.TxnType,
			&item.Category, &item.TxnTimestamp, &item.ValueDate,
			&item.Narration, &item.Reference, &item.IdempotencyKey,
		); err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to scan transaction", map[string]any{"reason": err.Error()})
			return
		}
		item.Entries = make([]LedgerEntryItem, 0, 2)
		indexByID[item.ID] = len(txns)
		txnIDs = append(txnIDs, item.ID)
		txns = append(txns, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to iterate transactions", map[string]any{"reason": err.Error()})
		return
	}

	// Step 3: Hydrate double-entry legs for the page in a single ANY($1::uuid[]) query
	// and attach each entry to its parent LedgerTransactionItem via indexByID.
	if len(txnIDs) > 0 {
		entryRows, err := s.pool.Query(ctx, `
			SELECT
				id::text,
				transaction_id::text,
				coa_account::text,
				amount::text,
				units::text
			FROM ledger_entries
			WHERE transaction_id = ANY($1::uuid[])
			ORDER BY created_at ASC, id ASC
		`, txnIDs)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query ledger entries", map[string]any{"reason": err.Error()})
			return
		}
		defer entryRows.Close()

		for entryRows.Next() {
			var txnID string
			var entry LedgerEntryItem
			if err := entryRows.Scan(&entry.ID, &txnID, &entry.CoaAccount, &entry.Amount, &entry.Units); err != nil {
				writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to scan ledger entry", map[string]any{"reason": err.Error()})
				return
			}
			if idx, ok := indexByID[txnID]; ok {
				txns[idx].Entries = append(txns[idx].Entries, entry)
			}
		}
		if err := entryRows.Err(); err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to iterate ledger entries", map[string]any{"reason": err.Error()})
			return
		}
	}

	writeJSON(w, http.StatusOK, accountTransactionsResponse{
		AccountID:    accountID,
		Transactions: txns,
		Total:        total,
	})
}

// -----------------------------------------------------------------------------
// Portfolio summary handler
// -----------------------------------------------------------------------------

// handleGetPortfolioSummary serves GET /api/v1/portfolio/summary?persona=<slug>.
// Aggregates asset and liability totals from ledger_entries using exact
// money.Decimal4 arithmetic and attaches DLQ and uncategorized review counts.
func (s *server) handleGetPortfolioSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	persona, ok := s.resolvePersonaOrError(ctx, w, strings.TrimSpace(r.URL.Query().Get("persona")))
	if !ok {
		return
	}

	// Step 1: Aggregate principal/cost ledger balances grouped by Financial Account source_type.
	rows, err := s.pool.Query(ctx, `
		SELECT
			a.source_type::text,
			COALESCE(SUM(le.amount) FILTER (
				WHERE le.coa_account IN ('ASSET:BANK', 'ASSET:MF_COST', 'ASSET:EQUITY_COST', 'LIABILITY:LOAN', 'LIABILITY:CARD')
			), 0)::NUMERIC(18,4)::text
		FROM accounts a
		LEFT JOIN ledger_entries le ON le.account_id = a.id
		WHERE a.canonical_user_id = $1
		GROUP BY a.source_type
	`, persona.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to aggregate portfolio balances", map[string]any{"reason": err.Error()})
		return
	}
	defer rows.Close()

	// Accumulate each container bucket in fixed-point Decimal4 without float64.
	var bankDeposits, mutualFunds, equities, loans, creditCards money.Decimal4
	for rows.Next() {
		var srcType, amtStr string
		if err := rows.Scan(&srcType, &amtStr); err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to scan portfolio summary row", map[string]any{"reason": err.Error()})
			return
		}
		amt, parseErr := money.ParseDecimal4(amtStr)
		if parseErr != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "invalid decimal from database", map[string]any{"reason": parseErr.Error()})
			return
		}
		switch srcType {
		case "DEPOSIT":
			bankDeposits, err = bankDeposits.Add(amt)
		case "MUTUAL_FUND":
			mutualFunds, err = mutualFunds.Add(amt)
		case "EQUITY":
			equities, err = equities.Add(amt)
		case "HOME_LOAN", "AUTO_LOAN":
			loans, err = loans.Add(amt.Neg())
		case "CREDIT_CARD":
			creditCards, err = creditCards.Add(amt)
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "decimal arithmetic overflow", map[string]any{"reason": err.Error()})
			return
		}
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to iterate portfolio summary rows", map[string]any{"reason": err.Error()})
		return
	}

	// Step 2: Derive TotalAssets, TotalLiabilities, and NetWorth (Assets - Liabilities).
	totalAssets, err := bankDeposits.Add(mutualFunds)
	if err == nil {
		totalAssets, err = totalAssets.Add(equities)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "asset sum overflow", map[string]any{"reason": err.Error()})
		return
	}

	totalLiabilities, err := loans.Add(creditCards)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "liability sum overflow", map[string]any{"reason": err.Error()})
		return
	}

	netWorth, err := totalAssets.Sub(totalLiabilities)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "net worth overflow", map[string]any{"reason": err.Error()})
		return
	}

	// Step 3: Fetch operational badge counts scoped to this Persona (DLQ jobs and
	// transactions awaiting human review).
	var dlqCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int
		FROM ingestion_jobs
		WHERE status = 'DEAD_LETTER' AND canonical_user_id = $1
	`, persona.ID).Scan(&dlqCount); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to count dlq jobs", map[string]any{"reason": err.Error()})
		return
	}

	var uncategorizedCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int
		FROM ledger_transactions lt
		JOIN accounts a ON a.id = lt.account_id
		WHERE a.canonical_user_id = $1 AND lt.category = 'UNCATEGORIZED_REVIEW'
	`, persona.ID).Scan(&uncategorizedCount); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to count uncategorized txns", map[string]any{"reason": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, portfolioSummaryResponse{
		Persona:          persona,
		AsOf:             time.Now().UTC().Format(time.RFC3339),
		NetWorth:         netWorth.String(),
		TotalAssets:      totalAssets.String(),
		TotalLiabilities: totalLiabilities.String(),
		PortfolioXIRR:    nil,
		AccountsSummary: accountsSummaryBreakdown{
			BankDeposits: bankDeposits.String(),
			MutualFunds:  mutualFunds.String(),
			Equities:     equities.String(),
			Loans:        loans.String(),
			CreditCards:  creditCards.String(),
		},
		OpenAlertCount:     0,
		UncategorizedCount: uncategorizedCount,
		DLQJobCount:        dlqCount,
	})
}
