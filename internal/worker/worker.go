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
// and dead-letter error codes.
const (
	defaultConcurrency  = 4
	defaultPollInterval = 250 * time.Millisecond

	statusPending    = "PENDING"
	statusProcessing = "PROCESSING"
	statusCompleted  = "COMPLETED"
	statusDeadLetter = "DEAD_LETTER"

	errCodeSchemaValidation = "ERR_SCHEMA_VALIDATION"
)

// -----------------------------------------------------------------------------
// Pool configuration & lifecycle
// -----------------------------------------------------------------------------

// Config configures worker concurrency and idle queue polling cadence.
type Config struct {
	Concurrency  int
	PollInterval time.Duration
}

// Pool claims PENDING ingestion_jobs via PostgreSQL FOR UPDATE SKIP LOCKED
// and dispatches them to source-specific ledger posters.
type Pool struct {
	db           *pgxpool.Pool
	concurrency  int
	pollInterval time.Duration
	wg           sync.WaitGroup
}

// claimedJob holds the fields returned atomically when a worker leases a job row.
type claimedJob struct {
	ID              string
	CanonicalUserID *string
	SourceType      string
	Payload         []byte
}

// NewPool creates a worker pool backed by db, applying safe defaults for
// zero or negative Concurrency and PollInterval values.
func NewPool(db *pgxpool.Pool, cfg Config) *Pool {
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}

	return &Pool{
		db:           db,
		concurrency:  conc,
		pollInterval: poll,
	}
}

// Start launches the configured number of worker goroutines until ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.concurrency; i++ {
		workerID := fmt.Sprintf("worker-%d", i+1)
		p.wg.Add(1)
		go func(id string) {
			defer p.wg.Done()
			p.runLoop(ctx, id)
		}(workerID)
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

// ProcessOne claims at most one PENDING job via FOR UPDATE SKIP LOCKED, dispatches it
// by source_type, and transitions it to COMPLETED or DEAD_LETTER.
// Returns (false, nil) when no ready job is available in the queue.
func (p *Pool) ProcessOne(ctx context.Context, workerID string) (bool, error) {
	job, found, err := p.claimNextJob(ctx, workerID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	switch job.SourceType {
	case "AA_DEPOSIT":
		// Parse and validate the ReBIT AA payload before opening a ledger write transaction.
		parsed, err := aa.ParseDeposit(job.Payload)
		if err != nil {
			// Permanent schema errors bypass retries and route directly to DEAD_LETTER.
			if errors.Is(err, aa.ErrSchemaValidation) {
				if dlqErr := p.markDeadLetter(ctx, job.ID, workerID, errCodeSchemaValidation, err.Error()); dlqErr != nil {
					return true, dlqErr
				}
				return true, nil
			}
			return true, err
		}

		// Post account upsert, ledger entries, and job completion in one atomic DB transaction.
		if err := p.postDepositAndComplete(ctx, job, workerID, parsed); err != nil {
			return true, err
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

// claimNextJob atomically selects one ready PENDING job using FOR UPDATE SKIP LOCKED
// and flips it to PROCESSING with a 60-second worker lease so concurrent workers
// never contend on or double-claim the same row.
func (p *Pool) claimNextJob(ctx context.Context, workerID string) (claimedJob, bool, error) {
	var job claimedJob
	err := p.db.QueryRow(ctx, `
		WITH next_job AS (
			SELECT id
			FROM ingestion_jobs
			WHERE status = $2
			  AND next_run_at <= NOW()
			ORDER BY next_run_at ASC, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE ingestion_jobs j
		SET status = $3,
		    locked_by = $1,
		    locked_until = NOW() + INTERVAL '60 seconds',
		    updated_at = NOW()
		FROM next_job
		WHERE j.id = next_job.id
		RETURNING j.id::text, j.canonical_user_id::text, j.source_type, j.payload
	`, workerID, statusPending, statusProcessing).Scan(&job.ID, &job.CanonicalUserID, &job.SourceType, &job.Payload)

	if errors.Is(err, pgx.ErrNoRows) {
		return claimedJob{}, false, nil
	}
	if err != nil {
		return claimedJob{}, false, fmt.Errorf("claim next job: %w", err)
	}

	return job, true, nil
}

// markDeadLetter transitions a leased PROCESSING job to DEAD_LETTER with diagnostic
// error fields and clears the worker lease. Checking locked_by guards against
// overwriting a job whose lease was already reclaimed by another worker.
func (p *Pool) markDeadLetter(ctx context.Context, jobID, workerID, errCode, errMsg string) error {
	_, err := p.db.Exec(ctx, `
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
	return nil
}
