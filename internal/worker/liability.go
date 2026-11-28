// Loan Liability Account ledger poster invoked by worker.Pool.ProcessOne for LIABILITY jobs.
// Executes Canonical User resolution, HOME_LOAN/AUTO_LOAN Financial Account upsert,
// backfill guard, first-ingest Opening Anchor derivation, zero-sum EMI_REPAYMENT
// component splitting (ADR-0001), and job completion inside one PostgreSQL tx.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Adhish78/ReconcileX/internal/liability"
	"github.com/Adhish78/ReconcileX/internal/money"
	"github.com/jackc/pgx/v5"
)

const (
	coaLiabilityLoan       = "LIABILITY:LOAN"
	coaExpenseLoanInterest = "EXPENSE:LOAN_INTEREST"
	coaExpenseLoanFees     = "EXPENSE:LOAN_FEES"
)

type loanEntryLeg struct {
	coaAccount string
	amount     money.Decimal4
}

type loanLedgerPosting struct {
	txnType        string
	txnTS          time.Time
	valueDate      *time.Time
	narration      string
	reference      string
	idempotencyKey string
	rawMetadata    []byte
	legs           []loanEntryLeg
}

// postLoanAndComplete atomically resolves the owner, upserts the HOME_LOAN or
// AUTO_LOAN Financial Account, enforces the Opening Anchor backfill guard,
// posts deduplicated zero-sum Ledger Transactions, and completes the job.
func (p *Pool) postLoanAndComplete(ctx context.Context, job Job, workerID string, parsed liability.LoanPayload) error {
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin loan tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: Resolve Canonical User from the job or any existing loan account natural key.
	userID, err := resolveCanonicalUserForAccounts(
		ctx, tx, job.CanonicalUserID, parsed.SubType, []string{parsed.NaturalKey}, parsed.InstitutionName+" Borrower",
	)
	if err != nil {
		return err
	}

	// Step 2: Upsert the HOME_LOAN/AUTO_LOAN Financial Account and refresh reported_balance.
	accountID, loanInitialized, err := upsertLoanAccount(ctx, tx, userID, parsed)
	if err != nil {
		return err
	}

	// Step 3: Backfill guard — reject payloads containing any transaction strictly
	// earlier than the account's existing OPENING_ANCHOR timestamp.
	var anchorTS time.Time
	err = tx.QueryRow(ctx, `
		SELECT txn_timestamp
		FROM ledger_transactions
		WHERE account_id = $1 AND txn_type = 'OPENING_ANCHOR'
		ORDER BY txn_timestamp ASC
		LIMIT 1
	`, accountID).Scan(&anchorTS)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup loan opening anchor timestamp: %w", err)
	}
	if err == nil && len(parsed.Transactions) > 0 && parsed.Transactions[0].Timestamp.Before(anchorTS) {
		return fmt.Errorf("%w: transaction %s (%s) precedes opening anchor (%s)",
			ErrBackfillBeforeAnchor,
			parsed.Transactions[0].TxnID,
			parsed.Transactions[0].Timestamp.UTC().Format(time.RFC3339Nano),
			anchorTS.UTC().Format(time.RFC3339Nano),
		)
	}

	// Step 4: On first ingestion only, derive and post a non-zero Opening Anchor against External Clearing.
	if !loanInitialized {
		var hasExistingTxns bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM ledger_transactions WHERE account_id = $1)`,
			accountID,
		).Scan(&hasExistingTxns); err != nil {
			return fmt.Errorf("check existing loan ledger transactions: %w", err)
		}
		if !hasExistingTxns {
			if anchor, ok, err := buildLoanOpeningAnchor(parsed); err != nil {
				return err
			} else if ok {
				if err := insertLoanLedgerPosting(ctx, tx, accountID, job.ID, anchor); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE accounts
			SET metadata = jsonb_set(metadata, '{liability_initialized}', 'true'::jsonb, true),
			    updated_at = NOW()
			WHERE id = $1
		`, accountID); err != nil {
			return fmt.Errorf("mark loan account initialized: %w", err)
		}
	}

	// Step 5: Post each normalized loan transaction with zero-sum Chart of Accounts legs.
	for _, item := range parsed.Transactions {
		legs := buildLoanLegs(item)
		if len(legs) == 0 {
			continue
		}
		posting := loanLedgerPosting{
			txnType:        item.Type,
			txnTS:          item.Timestamp,
			valueDate:      item.ValueDate,
			narration:      item.Narration,
			reference:      item.Reference,
			idempotencyKey: item.IdempotencyKey,
			rawMetadata:    item.RawMetadata,
			legs:           legs,
		}
		if err := insertLoanLedgerPosting(ctx, tx, accountID, job.ID, posting); err != nil {
			return err
		}
	}

	// Step 6: Complete the leased job while verifying lease ownership.
	if err := completeLeasedJob(ctx, tx, job.ID, workerID, userID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit loan tx for job %s: %w", job.ID, err)
	}
	return nil
}

func upsertLoanAccount(ctx context.Context, tx pgx.Tx, userID string, parsed liability.LoanPayload) (string, bool, error) {
	reportedBalanceStr := parsed.Summary.OutstandingBalance.String()
	reportedAt := parsed.Summary.BalanceDateTime
	if reportedAt == nil {
		reportedAt = parsed.Summary.StatementDate
	}

	metaMap := map[string]any{
		"sub_type": parsed.SubType,
	}
	if parsed.Summary.Status != "" {
		metaMap["status"] = parsed.Summary.Status
	}
	if parsed.Summary.SanctionedPrincipal != nil {
		metaMap["sanctioned_principal"] = parsed.Summary.SanctionedPrincipal.String()
	}
	if parsed.Summary.InterestRatePct != nil {
		metaMap["interest_rate_pct"] = parsed.Summary.InterestRatePct.String()
	}
	if parsed.Summary.EMIAmount != nil {
		metaMap["emi_amount"] = parsed.Summary.EMIAmount.String()
	}
	if parsed.Summary.TenureMonths != nil {
		metaMap["tenure_months"] = *parsed.Summary.TenureMonths
	}
	metaBytes, _ := json.Marshal(metaMap)

	var accountID string
	var initialized bool
	err := tx.QueryRow(ctx, `
		INSERT INTO accounts (
			canonical_user_id, source_type, natural_key, institution_name,
			fip_id, linked_acc_ref, masked_acc_number, currency,
			reported_balance, reported_balance_at, metadata
		)
		VALUES ($1, $2::account_source_type, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, $11::jsonb)
		ON CONFLICT (canonical_user_id, source_type, natural_key)
		DO UPDATE SET
			institution_name = EXCLUDED.institution_name,
			masked_acc_number = COALESCE(EXCLUDED.masked_acc_number, accounts.masked_acc_number),
			reported_balance = EXCLUDED.reported_balance,
			reported_balance_at = COALESCE(EXCLUDED.reported_balance_at, accounts.reported_balance_at),
			metadata = accounts.metadata || EXCLUDED.metadata,
			updated_at = NOW()
		RETURNING id::text, COALESCE((metadata->>'liability_initialized')::boolean, false)
	`,
		userID,
		parsed.SubType,
		parsed.NaturalKey,
		parsed.InstitutionName,
		parsed.FipID,
		parsed.LinkedAccRef,
		parsed.MaskedAccNumber,
		parsed.Currency,
		reportedBalanceStr,
		reportedAt,
		string(metaBytes),
	).Scan(&accountID, &initialized)
	if err != nil {
		return "", false, fmt.Errorf("upsert loan account: %w", err)
	}
	return accountID, initialized, nil
}

// buildLoanOpeningAnchor derives a synthetic OPENING_ANCHOR posting on first ingestion
// whenever pre-window unpaid principal is positive.
func buildLoanOpeningAnchor(parsed liability.LoanPayload) (loanLedgerPosting, bool, error) {
	var openingOutstanding money.Decimal4
	var anchorTS time.Time
	var derivedFrom string

	if len(parsed.Transactions) > 0 && parsed.Transactions[0].TransactionalBalance != nil {
		t0 := parsed.Transactions[0]
		var err error
		openingOutstanding, err = t0.TransactionalBalance.Sub(t0.PrincipalDelta())
		if err != nil {
			return loanLedgerPosting{}, false, fmt.Errorf("derive loan opening anchor: %w", err)
		}
		anchorTS = t0.Timestamp.Add(-time.Microsecond)
		derivedFrom = "transactionalBalance"
	} else {
		var netDelta money.Decimal4
		for _, item := range parsed.Transactions {
			var err error
			netDelta, err = netDelta.Add(item.PrincipalDelta())
			if err != nil {
				return loanLedgerPosting{}, false, fmt.Errorf("sum loan principal deltas: %w", err)
			}
		}
		var err error
		openingOutstanding, err = parsed.Summary.OutstandingBalance.Sub(netDelta)
		if err != nil {
			return loanLedgerPosting{}, false, fmt.Errorf("derive loan opening anchor from summary: %w", err)
		}
		switch {
		case len(parsed.Transactions) > 0:
			anchorTS = parsed.Transactions[0].Timestamp.Add(-time.Microsecond)
		case parsed.TransactionsStartDate != nil:
			anchorTS = *parsed.TransactionsStartDate
		case parsed.Summary.BalanceDateTime != nil:
			anchorTS = *parsed.Summary.BalanceDateTime
		case parsed.Summary.StatementDate != nil:
			anchorTS = *parsed.Summary.StatementDate
		default:
			anchorTS = time.Unix(0, 0).UTC()
		}
		derivedFrom = "summaryOutstandingBalance"
	}

	if openingOutstanding <= 0 {
		return loanLedgerPosting{}, false, nil
	}

	metaBytes, _ := json.Marshal(map[string]any{
		"openingAnchor": true,
		"derivedFrom":   derivedFrom,
	})

	return loanLedgerPosting{
		txnType:        "OPENING_ANCHOR",
		txnTS:          anchorTS,
		narration:      "Opening Loan Principal Anchor",
		idempotencyKey: liability.LoanAnchorIdempotencyKey(parsed.NaturalKey),
		rawMetadata:    metaBytes,
		legs: []loanEntryLeg{
			{coaAccount: coaLiabilityLoan, amount: openingOutstanding.Neg()},
			{coaAccount: coaExternalClearing, amount: openingOutstanding},
		},
	}, true, nil
}

// buildLoanLegs maps normalized loan transactions onto zero-sum Chart of Accounts legs:
//   - DISBURSEMENT: credits LIABILITY:LOAN (-amount) vs EQUITY:EXTERNAL_CLEARING (+amount)
//   - EMI_REPAYMENT: debits LIABILITY:LOAN (+principal), EXPENSE:LOAN_INTEREST (+interest),
//     EXPENSE:LOAN_FEES (+fee) against EQUITY:EXTERNAL_CLEARING (-amount)
//   - INTEREST_ADJUSTMENT: debits EXPENSE:LOAN_INTEREST (+amount) vs EQUITY:EXTERNAL_CLEARING (-amount)
//   - FEE_CHARGE: debits EXPENSE:LOAN_FEES (+amount) vs EQUITY:EXTERNAL_CLEARING (-amount)
func buildLoanLegs(item liability.LoanTransaction) []loanEntryLeg {
	switch item.Type {
	case "DISBURSEMENT":
		return []loanEntryLeg{
			{coaAccount: coaLiabilityLoan, amount: item.Amount.Neg()},
			{coaAccount: coaExternalClearing, amount: item.Amount},
		}

	case "EMI_REPAYMENT":
		legs := make([]loanEntryLeg, 0, 4)
		if item.PrincipalComponent > 0 {
			legs = append(legs, loanEntryLeg{coaAccount: coaLiabilityLoan, amount: item.PrincipalComponent})
		}
		if item.InterestComponent > 0 {
			legs = append(legs, loanEntryLeg{coaAccount: coaExpenseLoanInterest, amount: item.InterestComponent})
		}
		if item.FeeComponent > 0 {
			legs = append(legs, loanEntryLeg{coaAccount: coaExpenseLoanFees, amount: item.FeeComponent})
		}
		legs = append(legs, loanEntryLeg{coaAccount: coaExternalClearing, amount: item.Amount.Neg()})
		return legs

	case "INTEREST_ADJUSTMENT", "FEE_CHARGE":
		expenseCoA := coaExpenseLoanFees
		if item.Type == "INTEREST_ADJUSTMENT" {
			expenseCoA = coaExpenseLoanInterest
		}
		return []loanEntryLeg{
			{coaAccount: expenseCoA, amount: item.Amount},
			{coaAccount: coaExternalClearing, amount: item.Amount.Neg()},
		}

	default:
		return nil
	}
}

func insertLoanLedgerPosting(ctx context.Context, tx pgx.Tx, accountID, jobID string, p loanLedgerPosting) error {
	if len(p.legs) == 0 {
		return nil
	}

	var txnUUID string
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (
			account_id, ingestion_job_id, source, txn_type,
			txn_timestamp, value_date, narration, reference,
			idempotency_key, raw_metadata
		)
		VALUES ($1, $2, 'LIABILITY', $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9::jsonb)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING id::text
	`,
		accountID,
		jobID,
		p.txnType,
		p.txnTS,
		p.valueDate,
		p.narration,
		p.reference,
		p.idempotencyKey,
		string(p.rawMetadata),
	).Scan(&txnUUID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert loan ledger_transaction (%s): %w", p.txnType, err)
	}

	for _, leg := range p.legs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount)
			VALUES ($1, $2, $3::coa_account, $4)
		`, txnUUID, accountID, leg.coaAccount, leg.amount.String()); err != nil {
			return fmt.Errorf("insert loan ledger_entry (%s/%s): %w", p.txnType, leg.coaAccount, err)
		}
	}

	return nil
}
