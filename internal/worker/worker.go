// Package worker implements ReconcileX's PostgreSQL FOR UPDATE SKIP LOCKED
// asynchronous ingestion worker pool.
//
// Started by cmd/reconcilex at runtime and invoked synchronously via ProcessOne
// in integration tests (worker_test.go and server_test.go). Claims PENDING jobs
// created by POST /api/v1/ingest and routes payloads to source-specific posters.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Adhish78/ReconcileX/internal/aa"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Default pool tuning, job lifecycle state strings matching PostgreSQL job_status,
// and dead-letter / transient error codes.
const (
	defaultConcurrency      = 4
	defaultPollInterval     = 250 * time.Millisecond
	defaultLeaseDuration    = 60 * time.Second
	defaultSweepInterval    = 10 * time.Second
	defaultBaseRetryBackoff = 5 * time.Second

	statusPending    = "PENDING"
	statusProcessing = "PROCESSING"
	statusRetryWait  = "RETRY_WAIT"
	statusCompleted  = "COMPLETED"
	statusDeadLetter = "DEAD_LETTER"

	errCodeSchemaValidation     = "ERR_SCHEMA_VALIDATION"
	errCodeBackfillBeforeAnchor = "ERR_BACKFILL_BEFORE_ANCHOR"
	errCodeTransientFault       = "ERR_TRANSIENT_FAULT"
)

var (
	// ErrBackfillBeforeAnchor marks a permanent rejection when an incoming payload
	// contains a transaction strictly earlier than the account's existing Opening Anchor.
	ErrBackfillBeforeAnchor = errors.New(errCodeBackfillBeforeAnchor)

	// ErrTransientFault marks a retryable processing fault that triggers exponential
	// RETRY_WAIT backoff until max_attempts is reached.
	ErrTransientFault = errors.New(errCodeTransientFault)

	// ErrLeaseLost indicates that a worker no longer owns the PROCESSING lease
	// for a job because it expired or was reclaimed by another worker.
	ErrLeaseLost = errors.New("worker lease lost")
)

// -----------------------------------------------------------------------------
// Pool configuration & lifecycle
// -----------------------------------------------------------------------------

// Job holds the fields returned atomically when a worker claims a leased job row.
type Job struct {
	ID              string
	CanonicalUserID *string
	SourceType      string
	Payload         []byte
}

// Config configures worker concurrency, lease/backoff timings, and processing hooks.
type Config struct {
	Concurrency      int
	PollInterval     time.Duration
	LeaseDuration    time.Duration
	SweepInterval    time.Duration
	BaseRetryBackoff time.Duration
	BeforeProcess    func(ctx context.Context, job Job) error
}

// Pool claims PENDING and RETRY_WAIT ingestion_jobs via PostgreSQL FOR UPDATE
// SKIP LOCKED and dispatches them to source-specific ledger posters.
type Pool struct {
	db               *pgxpool.Pool
	concurrency      int
	pollInterval     time.Duration
	leaseDuration    time.Duration
	sweepInterval    time.Duration
	baseRetryBackoff time.Duration
	beforeProcess    func(ctx context.Context, job Job) error
	wg               sync.WaitGroup
}

// NewPool creates a worker pool backed by db, applying safe defaults for
// zero or negative configuration durations and concurrency values.
func NewPool(db *pgxpool.Pool, cfg Config) *Pool {
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	lease := cfg.LeaseDuration
	if lease <= 0 {
		lease = defaultLeaseDuration
	}
	sweep := cfg.SweepInterval
	if sweep <= 0 {
		sweep = defaultSweepInterval
	}
	backoff := cfg.BaseRetryBackoff
	if backoff <= 0 {
		backoff = defaultBaseRetryBackoff
	}

	return &Pool{
		db:               db,
		concurrency:      conc,
		pollInterval:     poll,
		leaseDuration:    lease,
		sweepInterval:    sweep,
		baseRetryBackoff: backoff,
		beforeProcess:    cfg.BeforeProcess,
	}
}

// Start launches the configured number of worker goroutines and the periodic
// stale-lease sweeper until ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.concurrency; i++ {
		workerID := fmt.Sprintf("worker-%d", i+1)
		p.wg.Add(1)
		go func(id string) {
			defer p.wg.Done()
			p.runLoop(ctx, id)
		}(workerID)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runSweeper(ctx)
	}()
}

// runSweeper periodically reclaims expired PROCESSING leases on sweepInterval.
func (p *Pool) runSweeper(ctx context.Context) {
	ticker := time.NewTicker(p.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.ReclaimStaleLeases(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("stale lease sweeper error: %v", err)
			}
		}
	}
}

// Wait blocks until all worker goroutines started by Start have exited cleanly.
func (p *Pool) Wait() {
	p.wg.Wait()
}

// runLoop continuously drains available jobs without sleeping while the queue has work,
// backing off for pollInterval only when the queue reports idle.
func (p *Pool) runLoop(ctx context.Context, workerID string) {
	for {
		if ctx.Err() != nil {
			return
		}

		claimed, err := p.ProcessOne(ctx, workerID)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("worker %s error: %v", workerID, err)
		}

		// Immediately attempt another claim when a job was processed to drain bursts quickly.
		if claimed {
			continue
		}

		// Queue was idle: sleep until either pollInterval elapses or shutdown cancels ctx.
		timer := time.NewTimer(p.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// -----------------------------------------------------------------------------
// Job claiming & dispatch
// -----------------------------------------------------------------------------

// ProcessOne claims at most one ready PENDING or RETRY_WAIT job via FOR UPDATE
// SKIP LOCKED, dispatches it by source_type, and transitions it to COMPLETED,
// RETRY_WAIT, or DEAD_LETTER.
// Returns (false, nil) when no ready job is available in the queue.
func (p *Pool) ProcessOne(ctx context.Context, workerID string) (bool, error) {
	job, found, err := p.claimNextJob(ctx, workerID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	if p.beforeProcess != nil {
		if err := p.beforeProcess(ctx, job); err != nil {
			return true, p.handleProcessingError(ctx, job.ID, workerID, err)
		}
	}

	switch job.SourceType {
	case "AA_DEPOSIT":
		// Parse and validate the ReBIT AA payload before opening a ledger write transaction.
		parsed, err := aa.ParseDeposit(job.Payload)
		if err != nil {
			return true, p.handleProcessingError(ctx, job.ID, workerID, err)
		}

		// Post account upsert, ledger entries, and job completion in one atomic DB transaction.
		if err := p.postDepositAndComplete(ctx, job, workerID, parsed); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return true, err
			}
			return true, p.handleProcessingError(ctx, job.ID, workerID, err)
		}

	default:
		// Unimplemented or unknown source types fail permanently to DEAD_LETTER so they
		// do not spin in PENDING.
		if err := p.markDeadLetter(ctx, job.ID, workerID, errCodeSchemaValidation, fmt.Sprintf("unsupported source_type %q", job.SourceType)); err != nil {
			return true, err
		}
	}

	return true, nil
}

// handleProcessingError routes permanent schema validation failures straight to
// DEAD_LETTER and treats all other processing errors as transient faults subject
// to exponential RETRY_WAIT backoff up to max_attempts.
func (p *Pool) handleProcessingError(ctx context.Context, jobID, workerID string, procErr error) error {
	if errors.Is(procErr, aa.ErrSchemaValidation) {
		return p.markDeadLetter(ctx, jobID, workerID, errCodeSchemaValidation, procErr.Error())
	}
	if errors.Is(procErr, ErrBackfillBeforeAnchor) {
		return p.markDeadLetter(ctx, jobID, workerID, errCodeBackfillBeforeAnchor, procErr.Error())
	}
	return p.markRetryOrDeadLetter(ctx, jobID, workerID, errCodeTransientFault, procErr.Error())
}

// claimNextJob atomically selects one ready PENDING or RETRY_WAIT job using
// FOR UPDATE SKIP LOCKED and flips it to PROCESSING with a fenced worker lease
// so concurrent workers never contend on or double-claim the same row.
func (p *Pool) claimNextJob(ctx context.Context, workerID string) (Job, bool, error) {
	var job Job
	err := p.db.QueryRow(ctx, `
		WITH next_job AS (
			SELECT id
			FROM ingestion_jobs
			WHERE status IN ($2, $3)
			  AND next_run_at <= NOW()
			ORDER BY next_run_at ASC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE ingestion_jobs j
		SET status = $4,
		    locked_by = $1,
		    locked_until = NOW() + ($5::bigint * INTERVAL '1 microsecond'),
		    updated_at = NOW()
		FROM next_job
		WHERE j.id = next_job.id
		RETURNING j.id::text, j.canonical_user_id::text, j.source_type, j.payload
	`, workerID, statusPending, statusRetryWait, statusProcessing, p.leaseDuration.Microseconds()).Scan(
		&job.ID, &job.CanonicalUserID, &job.SourceType, &job.Payload,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim next job: %w", err)
	}

	return job, true, nil
}

// retryOrDeadLetterUpdateSQL builds the shared retry-backoff / DEAD_LETTER UPDATE
// statement parameterized by a target-row CTE (`WHERE` selection) so live worker
// fault handling and stale-lease reclamation execute identical state transitions.
func retryOrDeadLetterUpdateSQL(targetCTE string) string {
	return `
		WITH target AS (` + targetCTE + `)
		UPDATE ingestion_jobs j
		SET attempt_count = j.attempt_count + 1,
		    status = CASE
		        WHEN j.attempt_count + 1 >= j.max_attempts THEN $1::job_status
		        ELSE $2::job_status
		    END,
		    next_run_at = CASE
		        WHEN j.attempt_count + 1 >= j.max_attempts THEN j.next_run_at
		        ELSE NOW() + ($5::bigint * POWER(2, LEAST(j.attempt_count, 20))::bigint * INTERVAL '1 microsecond')
		    END,
		    last_error_code = $3,
		    last_error_message = $4,
		    locked_by = NULL,
		    locked_until = NULL,
		    updated_at = NOW()
		FROM target
		WHERE j.id = target.id`
}

// markRetryOrDeadLetter increments attempt_count on a leased PROCESSING job and
// either schedules RETRY_WAIT at baseRetryBackoff * 2^(attempt_count - 1) or
// moves the job to DEAD_LETTER once attempt_count >= max_attempts.
func (p *Pool) markRetryOrDeadLetter(ctx context.Context, jobID, workerID, errCode, errMsg string) error {
	query := retryOrDeadLetterUpdateSQL(`
		SELECT id FROM ingestion_jobs
		WHERE id = $6 AND status = $7 AND locked_by = $8
	`)
	tag, err := p.db.Exec(ctx, query,
		statusDeadLetter, statusRetryWait, errCode, errMsg, p.baseRetryBackoff.Microseconds(),
		jobID, statusProcessing, workerID,
	)
	if err != nil {
		return fmt.Errorf("schedule retry or dead_letter for job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %s for worker %s", ErrLeaseLost, jobID, workerID)
	}
	return nil
}

// ReclaimStaleLeases sweeps PROCESSING rows whose locked_until lease has expired
// (locked_until < NOW()) and applies the same retry-backoff or DEAD_LETTER
// transitions as transient worker faults without disrupting active leases.
func (p *Pool) ReclaimStaleLeases(ctx context.Context) (int, error) {
	query := retryOrDeadLetterUpdateSQL(`
		SELECT id FROM ingestion_jobs
		WHERE status = $6 AND locked_until < NOW()
		ORDER BY locked_until ASC
		FOR UPDATE SKIP LOCKED
	`)
	tag, err := p.db.Exec(ctx, query,
		statusDeadLetter, statusRetryWait, errCodeTransientFault, "worker lease expired before completion", p.baseRetryBackoff.Microseconds(),
		statusProcessing,
	)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// markDeadLetter transitions a leased PROCESSING job directly to DEAD_LETTER with
// diagnostic error fields and clears the worker lease. Fencing on (status, locked_by)
// prevents overwriting a job whose lease was already reclaimed.
func (p *Pool) markDeadLetter(ctx context.Context, jobID, workerID, errCode, errMsg string) error {
	tag, err := p.db.Exec(ctx, `
		UPDATE ingestion_jobs
		SET status = $5,
		    last_error_code = $3,
		    last_error_message = $4,
		    locked_by = NULL,
		    locked_until = NULL,
		    updated_at = NOW()
		WHERE id = $1 AND status = $6 AND locked_by = $2
	`, jobID, workerID, errCode, errMsg, statusDeadLetter, statusProcessing)
	if err != nil {
		return fmt.Errorf("mark job %s dead_letter: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %s for worker %s", ErrLeaseLost, jobID, workerID)
	}
	return nil
}
