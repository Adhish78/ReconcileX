// CAS mutual fund ledger poster invoked by worker.Pool.ProcessOne for CAS jobs.
// Runs only after cas.Parse has verified schema validity and pre-ledger Unit Checksum
// conservation, ensuring failing statements never write partial accounts or ledger rows.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Adhish78/ReconcileX/internal/cas"
	"github.com/Adhish78/ReconcileX/internal/money"
	"github.com/jackc/pgx/v5"
)

// Chart of Accounts entries used for core CAS mutual fund double-entry legs.
const (
	coaAssetMFCost            = "ASSET:MF_COST"
	coaIncomeSalaryDividend   = "INCOME:SALARY_DIVIDEND"
)

type casEntryLeg struct {
	coaAccount string
	amount     money.Decimal4
	units      *money.Decimal6
}

// postCASAndComplete atomically resolves the owner, upserts each MUTUAL_FUND
// Folio/ISIN Financial Account, posts deduplicated zero-sum Ledger Transactions
// for core CAS transaction types, and marks the leased job COMPLETED.
func (p *Pool) postCASAndComplete(ctx context.Context, job Job, workerID string, parsed cas.CASData) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cas tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: Resolve Canonical User from the job or any existing MUTUAL_FUND natural key.
	userID, err := resolveCASCanonicalUser(ctx, tx, job.CanonicalUserID, parsed)
	if err != nil {
		return err
	}

	// Step 2: Upsert each scheme's Financial Account and post its core CAS transactions.
	for _, folio := range parsed.Folios {
		for _, scheme := range folio.Schemes {
			accountID, err := upsertCASAccount(ctx, tx, userID, folio, scheme)
			if err != nil {
				return err
			}

			source := "CAS_" + scheme.RTA
			for _, item := range scheme.Transactions {
				leg1, leg2, ok := buildCoreCASLegs(item)
				if !ok {
					// Tax and corporate-action rows are deferred to Issue #20; MISC/UNKNOWN are no-ops.
					continue
				}
				if err := insertCASLedgerPosting(ctx, tx, accountID, job.ID, source, item, leg1, leg2); err != nil {
					return err
				}
			}
		}
	}

	// Step 3: Complete the leased job while verifying this worker still holds the lease.
	if err := completeLeasedJob(ctx, tx, job.ID, workerID, userID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cas tx for job %s: %w", job.ID, err)
	}
	return nil
}

func resolveCASCanonicalUser(ctx context.Context, tx pgx.Tx, existing *string, parsed cas.CASData) (string, error) {
	keys := make([]string, 0)
	for _, f := range parsed.Folios {
		for _, s := range f.Schemes {
			keys = append(keys, s.NaturalKey)
		}
	}

	displayName := parsed.InvestorInfo.Name
	if displayName == "" {
		displayName = "CAS Mutual Fund Investor"
	}
	return resolveCanonicalUserForAccounts(ctx, tx, existing, "MUTUAL_FUND", keys, displayName)
}

func upsertCASAccount(ctx context.Context, tx pgx.Tx, userID string, folio cas.Folio, scheme cas.Scheme) (string, error) {
	instName := folio.AMC
	if instName == "" {
		instName = scheme.RTA
	}

	reportedUnitsStr := scheme.Close.String()
	var reportedBalStr *string
	var reportedBalAt *time.Time
	if scheme.Valuation != nil {
		s := scheme.Valuation.Value.String()
		reportedBalStr = &s
		dt := scheme.Valuation.Date
		reportedBalAt = &dt
	}

	metaMap := map[string]any{
		"scheme_name": scheme.Name,
		"rta":         scheme.RTA,
	}
	if scheme.SchemeType != "" {
		metaMap["scheme_type"] = scheme.SchemeType
	}
	if scheme.RTACode != "" {
		metaMap["rta_code"] = scheme.RTACode
	}
	metaBytes, _ := json.Marshal(metaMap)

	var accountID string
	err := tx.QueryRow(ctx, `
		INSERT INTO accounts (
			canonical_user_id, source_type, natural_key, institution_name,
			folio_number, isin, scheme_code, currency,
			reported_balance, reported_units, reported_balance_at, metadata
		)
		VALUES ($1, 'MUTUAL_FUND', $2, $3, $4, $5, $6, 'INR', $7, $8, $9, $10::jsonb)
		ON CONFLICT (canonical_user_id, source_type, natural_key)
		DO UPDATE SET
			institution_name = EXCLUDED.institution_name,
			folio_number = EXCLUDED.folio_number,
			isin = EXCLUDED.isin,
			scheme_code = COALESCE(EXCLUDED.scheme_code, accounts.scheme_code),
			reported_balance = COALESCE(EXCLUDED.reported_balance, accounts.reported_balance),
			reported_units = EXCLUDED.reported_units,
			reported_balance_at = COALESCE(EXCLUDED.reported_balance_at, accounts.reported_balance_at),
			metadata = accounts.metadata || EXCLUDED.metadata,
			updated_at = NOW()
		RETURNING id::text
	`,
		userID,
		scheme.NaturalKey,
		instName,
		folio.FolioNumber,
		scheme.ISIN,
		scheme.SchemeCode,
		reportedBalStr,
		reportedUnitsStr,
		reportedBalAt,
		string(metaBytes),
	).Scan(&accountID)
	if err != nil {
		return "", fmt.Errorf("upsert cas account (%s): %w", scheme.NaturalKey, err)
	}
	return accountID, nil
}

// buildCoreCASLegs maps the 8 core CAS transaction types to exact zero-sum
// double-entry pairs across ASSET:MF_COST, EQUITY:EXTERNAL_CLEARING, and INCOME:SALARY_DIVIDEND.
func buildCoreCASLegs(txn cas.Transaction) (casEntryLeg, casEntryLeg, bool) {
	switch txn.Type {
	case "PURCHASE", "PURCHASE_SIP", "SWITCH_IN", "REVERSAL":
		amt := *txn.Amount
		return casEntryLeg{coaAccount: coaAssetMFCost, amount: amt, units: txn.Units},
			casEntryLeg{coaAccount: coaExternalClearing, amount: amt.Neg()},
			true

	case "REDEMPTION", "SWITCH_OUT":
		absAmt := txn.Amount.Abs()
		negUnits := txn.Units.Abs().Neg()
		return casEntryLeg{coaAccount: coaAssetMFCost, amount: absAmt.Neg(), units: &negUnits},
			casEntryLeg{coaAccount: coaExternalClearing, amount: absAmt},
			true

	case "DIVIDEND_REINVEST":
		amt := *txn.Amount
		return casEntryLeg{coaAccount: coaAssetMFCost, amount: amt, units: txn.Units},
			casEntryLeg{coaAccount: coaIncomeSalaryDividend, amount: amt.Neg()},
			true

	case "DIVIDEND_PAYOUT":
		amt := *txn.Amount
		return casEntryLeg{coaAccount: coaExternalClearing, amount: amt},
			casEntryLeg{coaAccount: coaIncomeSalaryDividend, amount: amt.Neg()},
			true

	default:
		return casEntryLeg{}, casEntryLeg{}, false
	}
}

func insertCASLedgerPosting(
	ctx context.Context,
	tx pgx.Tx,
	accountID, jobID, source string,
	item cas.Transaction,
	primaryLeg, offsetLeg casEntryLeg,
) error {
	var txnUUID string
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (
			account_id, ingestion_job_id, source, txn_type,
			txn_timestamp, value_date, narration,
			idempotency_key, raw_metadata
		)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9::jsonb)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING id::text
	`,
		accountID,
		jobID,
		source,
		item.Type,
		item.Date,
		item.Date,
		item.Description,
		item.IdempotencyKey,
		string(item.RawMetadata),
	).Scan(&txnUUID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert cas ledger_transaction (%s): %w", item.Type, err)
	}

	var primaryUnits, offsetUnits *string
	if primaryLeg.units != nil {
		s := primaryLeg.units.String()
		primaryUnits = &s
	}
	if offsetLeg.units != nil {
		s := offsetLeg.units.String()
		offsetUnits = &s
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount, units)
		VALUES
			($1, $2, $3::coa_account, $5, $7),
			($1, $2, $4::coa_account, $6, $8)
	`, txnUUID, accountID, primaryLeg.coaAccount, offsetLeg.coaAccount, primaryLeg.amount.String(), offsetLeg.amount.String(), primaryUnits, offsetUnits); err != nil {
		return fmt.Errorf("insert cas ledger_entries (%s): %w", item.Type, err)
	}

	return nil
}
