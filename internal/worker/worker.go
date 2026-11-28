// Package worker implements ReconcileX's PostgreSQL FOR UPDATE SKIP LOCKED
// asynchronous ingestion worker pool.
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

const (
	defaultConcurrency  = 4
	defaultPollInterval = 250 * time.Millisecond

	statusPending    = "PENDING"
	statusProcessing = "PROCESSING"
	statusCompleted  = "COMPLETED"
	statusDeadLetter = "DEAD_LETTER"

	errCodeSchemaValidation = "ERR_SCHEMA_VALIDATION"
)

// Config configures the SKIP LOCKED worker pool.
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

type claimedJob struct {
	ID              string
	CanonicalUserID *string
	SourceType      string
	Payload         []byte
}

// NewPool creates a worker pool backed by db.
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

// Wait blocks until all worker goroutines started by Start have exited.
func (p *Pool) Wait() {
	p.wg.Wait()
}

func (p *Pool) runLoop(ctx context.Context, workerID string) {
	for {
		if ctx.Err() != nil {
			return
		}

		claimed, err := p.ProcessOne(ctx, workerID)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("worker %s error: %v", workerID, err)
		}

		if claimed {
			continue
		}

		timer := time.NewTimer(p.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ProcessOne claims at most one PENDING job via FOR UPDATE SKIP LOCKED, processes it,
// and updates it to COMPLETED or DEAD_LETTER. Returns (false, nil) when the queue is idle.
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
		parsed, err := aa.ParseDeposit(job.Payload)
		if err != nil {
			if errors.Is(err, aa.ErrSchemaValidation) {
				if dlqErr := p.markDeadLetter(ctx, job.ID, workerID, errCodeSchemaValidation, err.Error()); dlqErr != nil {
					return true, dlqErr
				}
				return true, nil
			}
			return true, err
		}
		if err := p.postDepositAndComplete(ctx, job, workerID, parsed); err != nil {
			return true, err
		}
	default:
		if err := p.markDeadLetter(ctx, job.ID, workerID, errCodeSchemaValidation, fmt.Sprintf("unsupported source_type %q", job.SourceType)); err != nil {
			return true, err
		}
	}

	return true, nil
}

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
