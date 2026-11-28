// Equities demat ledger poster invoked by worker.Pool.ProcessOne for AA_EQUITIES jobs.
// Executes Canonical User resolution, EQUITY Financial Account upsert, backfill guard,
// per-ISIN unit-only Opening Anchor derivation, zero-sum BUY/SELL and unit-only
// BONUS/SPLIT Corporate Action postings, and job completion inside one PostgreSQL tx.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/Adhish78/ReconcileX/internal/money"
	"github.com/jackc/pgx/v5"
)

const coaAssetEquityCost = "ASSET:EQUITY_COST"

type equityHoldingMeta struct {
	ISIN            string  `json:"isin"`
	IssuerName      string  `json:"issuer_name,omitempty"`
	ISINDescription string  `json:"isin_description,omitempty"`
	Units           string  `json:"units"`
	LastTradedPrice *string `json:"last_traded_price,omitempty"`
}

type equityEntryLeg struct {
	coaAccount string
	amount     money.Decimal4
	units      *money.Decimal6
}

type equityLedgerPosting struct {
	txnType        string
	txnTS          time.Time
	narration      string
	reference      string
	idempotencyKey string
	rawMetadata    []byte
	legs           []equityEntryLeg
}

// postEquitiesAndComplete atomically resolves the owner, upserts the EQUITY
// demat Financial Account, checks per-ISIN backfill guards, emits first-time
// per-ISIN Opening Anchors, posts deduplicated ledger legs, and completes the job.
func (p *Pool) postEquitiesAndComplete(ctx context.Context, job Job, workerID string, parsed aa.EquitiesPayload) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin equities tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: Resolve Canonical User from the job or any existing EQUITY natural key.
	userID, err := resolveCanonicalUserForAccounts(
		ctx, tx, job.CanonicalUserID, "EQUITY", []string{parsed.NaturalKey}, parsed.InstitutionName+" Demat Holder",
	)
	if err != nil {
		return err
	}

	// Step 2: Upsert the EQUITY demat Financial Account and read existing metadata.
	accountID, metaMap, err := upsertEquitiesAccount(ctx, tx, userID, parsed)
	if err != nil {
		return err
	}

	// Step 3: Per-ISIN backfill guard — reject payloads containing any transaction
	// strictly earlier than that ISIN's existing OPENING_ANCHOR timestamp.
	if err := checkEquitiesBackfillGuard(ctx, tx, accountID, parsed.Transactions); err != nil {
		return err
	}

	// Step 4: Derive and post unit-only Opening Anchors for newly observed ISINs
	// whenever Holdings.Holding[].units - SUM(txn.units) != 0.
	if err := postEquitiesOpeningAnchors(ctx, tx, accountID, job.ID, parsed, metaMap); err != nil {
		return err
	}

	// Step 5: Post normalized BUY/SELL, BONUS/SPLIT Corporate Actions, RIGHTS, and DIVIDEND legs.
	for _, item := range parsed.Transactions {
		legs, ok := buildEquitiesLegs(item)
		if !ok {
			continue
		}
		posting := equityLedgerPosting{
			txnType:        item.Type,
			txnTS:          item.Timestamp,
			narration:      item.Narration,
			reference:      item.OrderID,
			idempotencyKey: item.IdempotencyKey,
			rawMetadata:    item.RawMetadata,
			legs:           legs,
		}
		if err := insertEquitiesLedgerPosting(ctx, tx, accountID, job.ID, posting); err != nil {
			return err
		}
	}

	// Step 6: Complete the leased job while verifying lease ownership.
	if err := completeLeasedJob(ctx, tx, job.ID, workerID, userID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit equities tx for job %s: %w", job.ID, err)
	}
	return nil
}

func upsertEquitiesAccount(
	ctx context.Context,
	tx pgx.Tx,
	userID string,
	parsed aa.EquitiesPayload,
) (string, map[string]any, error) {
	var reportedBalStr *string
	if parsed.ReportedValue != nil {
		s := parsed.ReportedValue.String()
		reportedBalStr = &s
	}

	var reportedUnitsStr *string
	if len(parsed.Holdings) > 0 {
		var totalUnits money.Decimal6
		for _, h := range parsed.Holdings {
			var err error
			totalUnits, err = totalUnits.Add(h.Units)
			if err != nil {
				return "", nil, fmt.Errorf("sum equity reported units: %w", err)
			}
		}
		s := totalUnits.String()
		reportedUnitsStr = &s
	}

	var accountID string
	var existingMetaBytes []byte
	err := tx.QueryRow(ctx, `
		INSERT INTO accounts (
			canonical_user_id, source_type, natural_key, institution_name,
			fip_id, linked_acc_ref, masked_acc_number, currency,
			reported_balance, reported_units
		)
		VALUES ($1, 'EQUITY', $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9)
		ON CONFLICT (canonical_user_id, source_type, natural_key)
		DO UPDATE SET
			institution_name = EXCLUDED.institution_name,
			masked_acc_number = COALESCE(EXCLUDED.masked_acc_number, accounts.masked_acc_number),
			reported_balance = COALESCE(EXCLUDED.reported_balance, accounts.reported_balance),
			reported_units = COALESCE(EXCLUDED.reported_units, accounts.reported_units),
			updated_at = NOW()
		RETURNING id::text, metadata
	`,
		userID,
		parsed.NaturalKey,
		parsed.InstitutionName,
		parsed.FipID,
		parsed.LinkedAccRef,
		parsed.MaskedDematID,
		parsed.Currency,
		reportedBalStr,
		reportedUnitsStr,
	).Scan(&accountID, &existingMetaBytes)
	if err != nil {
		return "", nil, fmt.Errorf("upsert equity account: %w", err)
	}

	metaMap := make(map[string]any)
	if len(existingMetaBytes) > 0 {
		if err := json.Unmarshal(existingMetaBytes, &metaMap); err != nil {
			return "", nil, fmt.Errorf("decode equity account metadata: %w", err)
		}
	}

	if parsed.HoldingMode != "" {
		metaMap["holding_mode"] = parsed.HoldingMode
	}
	if len(parsed.Holdings) > 0 {
		holdingsMeta := make(map[string]equityHoldingMeta, len(parsed.Holdings))
		for _, h := range parsed.Holdings {
			var ltpStr *string
			if h.LastTradedPrice != nil {
				s := h.LastTradedPrice.String()
				ltpStr = &s
			}
			holdingsMeta[h.ISIN] = equityHoldingMeta{
				ISIN:            h.ISIN,
				IssuerName:      h.IssuerName,
				ISINDescription: h.ISINDescription,
				Units:           h.Units.String(),
				LastTradedPrice: ltpStr,
			}
		}
		metaMap["holdings"] = holdingsMeta
	}

	return accountID, metaMap, nil
}

func checkEquitiesBackfillGuard(
	ctx context.Context,
	tx pgx.Tx,
	accountID string,
	txns []aa.EquitiesTransaction,
) error {
	if len(txns) == 0 {
		return nil
	}

	rows, err := tx.Query(ctx, `
		SELECT raw_metadata->>'isin', txn_timestamp
		FROM ledger_transactions
		WHERE account_id = $1 AND txn_type = 'OPENING_ANCHOR' AND raw_metadata ? 'isin'
	`, accountID)
	if err != nil {
		return fmt.Errorf("lookup equity opening anchors: %w", err)
	}
	defer rows.Close()

	anchorByISIN := make(map[string]time.Time)
	for rows.Next() {
		var isin string
		var ts time.Time
		if err := rows.Scan(&isin, &ts); err != nil {
			return fmt.Errorf("scan equity opening anchor: %w", err)
		}
		if existing, ok := anchorByISIN[isin]; !ok || ts.Before(existing) {
			anchorByISIN[isin] = ts
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate equity opening anchors: %w", err)
	}

	for _, item := range txns {
		if anchorTS, ok := anchorByISIN[item.ISIN]; ok && item.Timestamp.Before(anchorTS) {
			return fmt.Errorf("%w: transaction %s (%s) for ISIN %s precedes opening anchor (%s)",
				ErrBackfillBeforeAnchor,
				item.TxnID,
				item.Timestamp.UTC().Format(time.RFC3339Nano),
				item.ISIN,
				anchorTS.UTC().Format(time.RFC3339Nano),
			)
		}
	}

	return nil
}

func postEquitiesOpeningAnchors(
	ctx context.Context,
	tx pgx.Tx,
	accountID, jobID string,
	parsed aa.EquitiesPayload,
	metaMap map[string]any,
) error {
	initializedISINs := extractInitializedISINs(metaMap)

	// Also treat any ISIN that already has ledger_transactions on this account as initialized.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT raw_metadata->>'isin'
		FROM ledger_transactions
		WHERE account_id = $1 AND raw_metadata ? 'isin'
	`, accountID)
	if err != nil {
		return fmt.Errorf("query existing equity isins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var isin string
		if err := rows.Scan(&isin); err != nil {
			return fmt.Errorf("scan existing equity isin: %w", err)
		}
		if isin != "" {
			initializedISINs[isin] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate existing equity isins: %w", err)
	}

	holdingByISIN := make(map[string]aa.EquityHolding, len(parsed.Holdings))
	isinSet := make(map[string]struct{})
	for _, h := range parsed.Holdings {
		if existing, ok := holdingByISIN[h.ISIN]; ok {
			sumUnits, err := existing.Units.Add(h.Units)
			if err != nil {
				return fmt.Errorf("sum duplicate holding units for %s: %w", h.ISIN, err)
			}
			existing.Units = sumUnits
			if existing.IssuerName == "" {
				existing.IssuerName = h.IssuerName
			}
			if existing.ISINDescription == "" {
				existing.ISINDescription = h.ISINDescription
			}
			holdingByISIN[h.ISIN] = existing
		} else {
			holdingByISIN[h.ISIN] = h
		}
		isinSet[h.ISIN] = struct{}{}
	}

	txnSumByISIN := make(map[string]money.Decimal6)
	earliestTxnByISIN := make(map[string]aa.EquitiesTransaction)
	for _, item := range parsed.Transactions {
		isinSet[item.ISIN] = struct{}{}
		sum, err := txnSumByISIN[item.ISIN].Add(item.SignedUnits)
		if err != nil {
			return fmt.Errorf("sum transaction units for %s: %w", item.ISIN, err)
		}
		txnSumByISIN[item.ISIN] = sum
		if _, exists := earliestTxnByISIN[item.ISIN]; !exists {
			earliestTxnByISIN[item.ISIN] = item
		}
	}

	isins := make([]string, 0, len(isinSet))
	for isin := range isinSet {
		isins = append(isins, isin)
	}
	sort.Strings(isins)

	for _, isin := range isins {
		if initializedISINs[isin] {
			continue
		}

		// Without a Holdings block, do not fabricate a synthetic negative anchor from transactions alone.
		if !parsed.HasHoldings {
			initializedISINs[isin] = true
			continue
		}

		holding := holdingByISIN[isin]
		diffUnits, err := holding.Units.Sub(txnSumByISIN[isin])
		if err != nil {
			return fmt.Errorf("derive equity opening anchor for %s: %w", isin, err)
		}

		if diffUnits != 0 {
			var anchorTS time.Time
			var derivedFrom string
			if t0, hasTxn := earliestTxnByISIN[isin]; hasTxn {
				anchorTS = t0.Timestamp.Add(-time.Microsecond)
				derivedFrom = "holdingsMinusTransactions"
			} else if parsed.TransactionsStartDate != nil {
				anchorTS = *parsed.TransactionsStartDate
				derivedFrom = "holdingsSnapshot"
			} else {
				anchorTS = time.Unix(0, 0).UTC()
				derivedFrom = "holdingsSnapshot"
			}

			anchorMeta := map[string]any{
				"openingAnchor": true,
				"isin":          isin,
				"derivedFrom":   derivedFrom,
			}
			if holding.IssuerName != "" {
				anchorMeta["companyName"] = holding.IssuerName
			}
			if holding.ISINDescription != "" {
				anchorMeta["isinDescription"] = holding.ISINDescription
			}
			anchorMetaBytes, _ := json.Marshal(anchorMeta)

			unitsCopy := diffUnits
			posting := equityLedgerPosting{
				txnType:        "OPENING_ANCHOR",
				txnTS:          anchorTS,
				narration:      "Opening Equity Anchor (" + isin + ")",
				idempotencyKey: aa.EquitiesAnchorIdempotencyKey(parsed.NaturalKey, isin),
				rawMetadata:    anchorMetaBytes,
				legs: []equityEntryLeg{
					{
						coaAccount: coaAssetEquityCost,
						amount:     0,
						units:      &unitsCopy,
					},
				},
			}
			if err := insertEquitiesLedgerPosting(ctx, tx, accountID, jobID, posting); err != nil {
				return err
			}
		}

		initializedISINs[isin] = true
	}

	metaMap["aa_equities_initialized_isins"] = initializedISINs
	metaBytes, err := json.Marshal(metaMap)
	if err != nil {
		return fmt.Errorf("marshal equity account metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE accounts
		SET metadata = $2::jsonb,
		    updated_at = NOW()
		WHERE id = $1
	`, accountID, string(metaBytes)); err != nil {
		return fmt.Errorf("update equity account metadata: %w", err)
	}

	return nil
}

func extractInitializedISINs(metaMap map[string]any) map[string]bool {
	out := make(map[string]bool)
	raw, ok := metaMap["aa_equities_initialized_isins"]
	if !ok || raw == nil {
		return out
	}
	if m, ok := raw.(map[string]any); ok {
		for k, v := range m {
			if b, isBool := v.(bool); isBool && b {
				out[k] = true
			}
		}
	}
	return out
}

func buildEquitiesLegs(item aa.EquitiesTransaction) ([]equityEntryLeg, bool) {
	switch item.Type {
	case "BUY", "SELL":
		unitsCopy := item.SignedUnits
		return []equityEntryLeg{
			{coaAccount: coaAssetEquityCost, amount: item.SignedAmount, units: &unitsCopy},
			{coaAccount: coaExternalClearing, amount: item.SignedAmount.Neg()},
		}, true

	case "BONUS", "SPLIT":
		unitsCopy := item.SignedUnits
		return []equityEntryLeg{
			{coaAccount: coaAssetEquityCost, amount: 0, units: &unitsCopy},
		}, true

	case "RIGHTS":
		unitsCopy := item.SignedUnits
		if item.SignedAmount != 0 {
			return []equityEntryLeg{
				{coaAccount: coaAssetEquityCost, amount: item.SignedAmount, units: &unitsCopy},
				{coaAccount: coaExternalClearing, amount: item.SignedAmount.Neg()},
			}, true
		}
		return []equityEntryLeg{
			{coaAccount: coaAssetEquityCost, amount: 0, units: &unitsCopy},
		}, true

	case "DIVIDEND":
		if item.SignedAmount <= 0 {
			return nil, false
		}
		return []equityEntryLeg{
			{coaAccount: coaExternalClearing, amount: item.SignedAmount},
			{coaAccount: coaIncomeSalaryDividend, amount: item.SignedAmount.Neg()},
		}, true

	default:
		return nil, false
	}
}

func insertEquitiesLedgerPosting(
	ctx context.Context,
	tx pgx.Tx,
	accountID, jobID string,
	p equityLedgerPosting,
) error {
	if len(p.legs) == 0 {
		return nil
	}

	var txnUUID string
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (
			account_id, ingestion_job_id, source, txn_type,
			txn_timestamp, narration, reference,
			idempotency_key, raw_metadata
		)
		VALUES ($1, $2, 'AA_EQUITIES', $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8::jsonb)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING id::text
	`,
		accountID,
		jobID,
		p.txnType,
		p.txnTS,
		p.narration,
		p.reference,
		p.idempotencyKey,
		string(p.rawMetadata),
	).Scan(&txnUUID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert equity ledger_transaction (%s): %w", p.txnType, err)
	}

	for _, leg := range p.legs {
		var unitsStr *string
		if leg.units != nil {
			s := leg.units.String()
			unitsStr = &s
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount, units)
			VALUES ($1, $2, $3::coa_account, $4, $5)
		`, txnUUID, accountID, leg.coaAccount, leg.amount.String(), unitsStr); err != nil {
			return fmt.Errorf("insert equity ledger_entry (%s/%s): %w", p.txnType, leg.coaAccount, err)
		}
	}

	return nil
}
