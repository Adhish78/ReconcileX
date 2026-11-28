// Deposit ledger poster invoked by worker.Pool.ProcessOne for AA_DEPOSIT jobs.
// Executes Canonical User resolution, Financial Account upsert, double-entry
// Ledger Transaction/Entry insertion, and job completion inside one PostgreSQL
// transaction so deferred zero-sum triggers validate the entire batch at COMMIT.
package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/jackc/pgx/v5"
)

// Chart of Accounts buckets used for AA DEPOSIT double-entry legs.
const (
	coaAssetBank        = "ASSET:BANK"
	coaExternalClearing = "EQUITY:EXTERNAL_CLEARING"
)

// postDepositAndComplete atomically resolves the owner, upserts the DEPOSIT
// Financial Account, posts deduplicated zero-sum Ledger Transactions, and marks
// the leased job COMPLETED inside one database transaction.
func (p *Pool) postDepositAndComplete(ctx context.Context, job claimedJob, workerID string, parsed aa.DepositPayload) error {
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
	accountID, err := upsertDepositAccount(ctx, tx, userID, parsed)
	if err != nil {
		return err
	}

	// Step 4: Post each normalized bank event with paired ASSET:BANK and External Clearing legs.
	for _, item := range parsed.Transactions {
		if err := insertDepositLedgerTransaction(ctx, tx, accountID, job.ID, item); err != nil {
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
		return fmt.Errorf("lost lease on job %s for worker %s", job.ID, workerID)
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
func upsertDepositAccount(ctx context.Context, tx pgx.Tx, userID string, parsed aa.DepositPayload) (string, error) {
	var reportedBalanceStr *string
	if parsed.ReportedBalance != nil {
		s := parsed.ReportedBalance.String()
		reportedBalanceStr = &s
	}

	var accountID string
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
		RETURNING id::text
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
	).Scan(&accountID)
	if err != nil {
		return "", fmt.Errorf("upsert deposit account: %w", err)
	}
	return accountID, nil
}

// insertDepositLedgerTransaction inserts one idempotent Ledger Transaction and its
// two balancing Ledger Entries (+signed on ASSET:BANK, -signed on EQUITY:EXTERNAL_CLEARING).
func insertDepositLedgerTransaction(ctx context.Context, tx pgx.Tx, accountID, jobID string, item aa.DepositTransaction) error {
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
		item.TxnType,
		item.Timestamp,
		item.ValueDate,
		item.Narration,
		item.Reference,
		item.IdempotencyKey,
		string(item.RawMetadata),
	).Scan(&txnUUID)

	// pgx.ErrNoRows from RETURNING means DO NOTHING fired, so both double-entry legs
	// already exist on the ledger and we can return early without duplicating entries.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("insert ledger_transaction (%s): %w", item.TxnID, err)
	}

	// Post both balancing legs in one statement: bank delta and negated External Clearing offset.
	bankAmt := item.SignedAmount.String()
	clearingAmt := item.SignedAmount.Neg().String()

	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (transaction_id, account_id, coa_account, amount)
		VALUES
			($1, $2, $3::coa_account, $5),
			($1, $2, $4::coa_account, $6)
	`, txnUUID, accountID, coaAssetBank, coaExternalClearing, bankAmt, clearingAmt); err != nil {
		return fmt.Errorf("insert ledger_entries (%s): %w", item.TxnID, err)
	}

	return nil
}
