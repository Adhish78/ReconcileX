// Deposit ledger poster invoked by worker.Pool.ProcessOne for AA_DEPOSIT jobs.
// Executes Canonical User resolution, Financial Account upsert, double-entry
// Ledger Transaction/Entry insertion, and job completion inside one PostgreSQL
// transaction so deferred zero-sum triggers validate the entire batch at COMMIT.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/Adhish78/ReconcileX/internal/money"
	"github.com/jackc/pgx/v5"
)

// Chart of Accounts buckets used for AA DEPOSIT double-entry legs.
const (
	coaAssetBank        = "ASSET:BANK"
	coaExternalClearing = "EQUITY:EXTERNAL_CLEARING"
)

// ledgerPosting represents a normalized zero-sum double-entry posting on
// ASSET:BANK vs EQUITY:EXTERNAL_CLEARING (either an AA event or OPENING_ANCHOR).
type ledgerPosting struct {
	txnType        string
	txnTimestamp   time.Time
	valueDate      *time.Time
	narration      string
	reference      string
	idempotencyKey string
	rawMetadata    []byte
	signedAmount   money.Decimal4
}

// postDepositAndComplete atomically resolves the owner, upserts the DEPOSIT
// Financial Account, posts deduplicated zero-sum Ledger Transactions, and marks
// the leased job COMPLETED inside one database transaction.
func (p *Pool) postDepositAndComplete(ctx context.Context, job Job, workerID string, parsed aa.DepositPayload) error {
	// Step 1: Open a single transaction so account upsert, ledger postings, and job
	// completion commit together (or roll back cleanly on any trigger/lease failure).
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin deposit tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 2: Resolve the Canonical User ID from the job or existing account natural key.
	userID, err := resolveDepositCanonicalUser(ctx, tx, job.CanonicalUserID, parsed.NaturalKey, parsed.InstitutionName)
	if err != nil {
		return err
	}

	// Step 3: Upsert the DEPOSIT Financial Account and refresh its reported balance snapshot.
	accountID, depositInitialized, err := upsertDepositAccount(ctx, tx, userID, parsed)
	if err != nil {
		return err
	}

	// Step 4: Backfill guard — reject any payload containing a transaction strictly earlier
	// than the account's existing OPENING_ANCHOR timestamp before writing Ledger Transactions or Ledger Entries.
	var anchorTS time.Time
	err = tx.QueryRow(ctx, `
		SELECT txn_timestamp
		FROM ledger_transactions
		WHERE account_id = $1 AND txn_type = 'OPENING_ANCHOR'
		ORDER BY txn_timestamp ASC
		LIMIT 1
	`, accountID).Scan(&anchorTS)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup opening anchor timestamp: %w", err)
	}
	if err == nil && len(parsed.Transactions) > 0 && parsed.Transactions[0].Timestamp.Before(anchorTS) {
		return fmt.Errorf("%w: transaction %s (%s) precedes opening anchor (%s)",
			ErrBackfillBeforeAnchor,
			parsed.Transactions[0].TxnID,
			parsed.Transactions[0].Timestamp.UTC().Format(time.RFC3339Nano),
			anchorTS.UTC().Format(time.RFC3339Nano),
		)
	}

	// Step 5: On first ingestion only (neither previously initialized nor holding prior
	// Ledger Transactions), derive and post an Opening Anchor against External Clearing.
	if !depositInitialized {
		var hasExistingTxns bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM ledger_transactions WHERE account_id = $1)`,
			accountID,
		).Scan(&hasExistingTxns); err != nil {
			return fmt.Errorf("check existing ledger transactions: %w", err)
		}
		if !hasExistingTxns {
			if anchor, ok, err := buildDepositOpeningAnchor(parsed); err != nil {
				return err
			} else if ok {
				if err := insertDepositLedgerPosting(ctx, tx, accountID, job.ID, anchor); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE accounts
			SET metadata = jsonb_set(metadata, '{aa_deposit_initialized}', 'true'::jsonb, true),
			    updated_at = NOW()
			WHERE id = $1
		`, accountID); err != nil {
			return fmt.Errorf("mark deposit account initialized: %w", err)
		}
	}

	// Step 6: Post each normalized bank event with paired ASSET:BANK and External Clearing legs.
	for _, item := range parsed.Transactions {
		posting := ledgerPosting{
			txnType:        item.TxnType,
			txnTimestamp:   item.Timestamp,
			valueDate:      item.ValueDate,
			narration:      item.Narration,
			reference:      item.Reference,
			idempotencyKey: item.IdempotencyKey,
			rawMetadata:    item.RawMetadata,
			signedAmount:   item.SignedAmount,
		}
		if err := insertDepositLedgerPosting(ctx, tx, accountID, job.ID, posting); err != nil {
			return err
		}
	}

	// Step 5: Mark the job COMPLETED and attach the resolved canonical_user_id,
	// verifying that this worker still holds the PROCESSING lease.
	tag, err := tx.Exec(ctx, `
		UPDATE ingestion_jobs
		SET status = $4,
		    canonical_user_id = $3,
		    completed_at = NOW(),
		    locked_by = NULL,
		    locked_until = NULL,
		    updated_at = NOW()
		WHERE id = $1 AND status = $5 AND locked_by = $2
	`, job.ID, workerID, userID, statusCompleted, statusProcessing)
	if err != nil {
		return fmt.Errorf("complete job %s: %w", job.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %s for worker %s", ErrLeaseLost, job.ID, workerID)
	}

	// Step 6: Commit triggers PostgreSQL's DEFERRABLE trg_ledger_entries_balanced check.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit deposit tx for job %s: %w", job.ID, err)
	}

	return nil
}

// resolveDepositCanonicalUser returns the job's explicit Canonical User when provided;
// otherwise it reuses the ACTIVE owner of an existing DEPOSIT account matching
// naturalKey so unlinked re-ingests deduplicate onto the same account instead of
// spawning duplicate users.
func resolveDepositCanonicalUser(ctx context.Context, tx pgx.Tx, existing *string, naturalKey, institutionName string) (string, error) {
	if existing != nil && *existing != "" {
		return *existing, nil
	}

	// Look for an existing ACTIVE owner holding this exact DEPOSIT natural_key.
	var matchedUserID string
	err := tx.QueryRow(ctx, `
		SELECT a.canonical_user_id::text
		FROM accounts a
		JOIN users u ON u.id = a.canonical_user_id
		WHERE a.source_type = 'DEPOSIT'
		  AND a.natural_key = $1
		  AND u.status = 'ACTIVE'
		ORDER BY a.created_at ASC
		LIMIT 1
	`, naturalKey).Scan(&matchedUserID)
	if err == nil {
		return matchedUserID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("lookup existing deposit owner: %w", err)
	}

	// First-time unlinked ingest: create a non-persona ACTIVE Canonical User (NULL persona_slug).
	var userID string
	displayName := institutionName + " Account Holder"
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES (NULL, $1, 'ACTIVE') RETURNING id::text`,
		displayName,
	).Scan(&userID); err != nil {
		return "", fmt.Errorf("create canonical user: %w", err)
	}
	return userID, nil
}

// upsertDepositAccount creates or refreshes the Financial Account row keyed by
// (canonical_user_id, 'DEPOSIT', natural_key), preserving prior non-null metadata
// and reported balance fields when a subsequent payload omits them.
func upsertDepositAccount(ctx context.Context, tx pgx.Tx, userID string, parsed aa.DepositPayload) (string, bool, error) {
	var reportedBalanceStr *string
	if parsed.ReportedBalance != nil {
		s := parsed.ReportedBalance.String()
		reportedBalanceStr = &s
	}

	var accountID string
	var initialized bool
	err := tx.QueryRow(ctx, `
		INSERT INTO accounts (
			canonical_user_id, source_type, natural_key, institution_name,
			fip_id, linked_acc_ref, masked_acc_number, currency,
			reported_balance, reported_balance_at
		)
		VALUES ($1, 'DEPOSIT', $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9)
		ON CONFLICT (canonical_user_id, source_type, natural_key)
		DO UPDATE SET
			institution_name = EXCLUDED.institution_name,
			masked_acc_number = COALESCE(EXCLUDED.masked_acc_number, accounts.masked_acc_number),
			reported_balance = COALESCE(EXCLUDED.reported_balance, accounts.reported_balance),
			reported_balance_at = COALESCE(EXCLUDED.reported_balance_at, accounts.reported_balance_at),
			updated_at = NOW()
		RETURNING id::text, COALESCE((metadata->>'aa_deposit_initialized')::boolean, false)
	`,
		userID,
		parsed.NaturalKey,
		parsed.InstitutionName,
		parsed.FipID,
		parsed.LinkedAccRef,
		parsed.MaskedAccNumber,
		parsed.Currency,
		reportedBalanceStr,
		parsed.ReportedBalanceAt,
	).Scan(&accountID, &initialized)
	if err != nil {
		return "", false, fmt.Errorf("upsert deposit account: %w", err)
	}
	return accountID, initialized, nil
}

// insertDepositLedgerPosting inserts one idempotent Ledger Transaction and its
// two balancing Ledger Entries (+signed on ASSET:BANK, -signed on EQUITY:EXTERNAL_CLEARING).
func insertDepositLedgerPosting(ctx context.Context, tx pgx.Tx, accountID, jobID string, p ledgerPosting) error {
	// Insert the Ledger Transaction header first; ON CONFLICT skips rows already
	// posted by an overlapping AA fetch window using (account_id, idempotency_key).
	var txnUUID string
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (
			account_id, ingestion_job_id, source, txn_type,
			txn_timestamp, value_date, narration, reference,
			idempotency_key, raw_metadata
		)
		VALUES ($1, $2, 'AA_DEPOSIT', $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8, $9::jsonb)
		ON CONFLICT (account_id, idempotency_key) DO NOTHING
		RETURNING id::text
	`,
		accountID,
		jobID,
		p.txnType,
		p.txnTimestamp,
		p.valueDate,
		p.narration,
		p.reference,
		p.idempotencyKey,
		string(p.rawMetadata),
	).Scan(&txnUUID)

	// pgx.ErrNoRows from RETURNING means DO NOTHING fired, so both double-entry legs
	// already exist on the ledger and we can return early without duplicating entries.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert ledger_transaction (%s): %w", p.txnType, err)
	}

	// Post both balancing legs in one statement: bank delta and negated External Clearing offset.
	bankAmt := p.signedAmount.String()
	clearingAmt := p.signedAmount.Neg().String()

	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount)
		VALUES
			($1, $2, $3::coa_account, $5),
			($1, $2, $4::coa_account, $6)
	`, txnUUID, accountID, coaAssetBank, coaExternalClearing, bankAmt, clearingAmt); err != nil {
		return fmt.Errorf("insert ledger_entries (%s): %w", p.txnType, err)
	}

	return nil
}

// buildDepositOpeningAnchor derives a synthetic OPENING_ANCHOR ledgerPosting
// for a first-time DEPOSIT ingestion when the pre-window balance is non-zero.
func buildDepositOpeningAnchor(parsed aa.DepositPayload) (ledgerPosting, bool, error) {
	var anchorAmt money.Decimal4
	var anchorTS time.Time
	var derivedFrom string

	if len(parsed.Transactions) > 0 {
		t0 := parsed.Transactions[0]
		if t0.TransactionalBalance == nil {
			return ledgerPosting{}, false, nil
		}
		var err error
		anchorAmt, err = t0.TransactionalBalance.Sub(t0.SignedAmount)
		if err != nil {
			return ledgerPosting{}, false, fmt.Errorf("derive opening anchor: %w", err)
		}
		if anchorAmt == 0 {
			return ledgerPosting{}, false, nil
		}
		anchorTS = t0.Timestamp.Add(-time.Microsecond)
		derivedFrom = "transactionalBalance"
	} else {
		if parsed.ReportedBalance == nil || *parsed.ReportedBalance == 0 {
			return ledgerPosting{}, false, nil
		}
		anchorAmt = *parsed.ReportedBalance
		switch {
		case parsed.TransactionsStartDate != nil:
			anchorTS = *parsed.TransactionsStartDate
		case parsed.ReportedBalanceAt != nil:
			anchorTS = *parsed.ReportedBalanceAt
		default:
			anchorTS = time.Unix(0, 0).UTC()
		}
		derivedFrom = "summaryCurrentBalance"
	}

	metaBytes, _ := json.Marshal(map[string]any{
		"openingAnchor": true,
		"derivedFrom":   derivedFrom,
	})

	return ledgerPosting{
		txnType:        "OPENING_ANCHOR",
		txnTimestamp:   anchorTS,
		narration:      "Opening Balance Anchor",
		idempotencyKey: aa.DepositAnchorIdempotencyKey(parsed.NaturalKey),
		rawMetadata:    metaBytes,
		signedAmount:   anchorAmt,
	}, true, nil
}
