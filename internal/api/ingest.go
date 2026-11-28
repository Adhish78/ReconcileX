// Ingestion enqueue and job inspection HTTP handlers for ReconcileX.
//
// Owns POST /api/v1/ingest, GET /api/v1/jobs, and GET /api/v1/jobs/{id}.
// Ingestion intentionally validates only the outer JSON envelope synchronously
// and delegates source-specific payload validation and ledger posting to the
// asynchronous SKIP LOCKED worker pool in internal/worker.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pagination bounds shared across /api/v1/jobs and /api/v1/accounts/{id}/transactions.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// -----------------------------------------------------------------------------
// Request & response wire contracts
// -----------------------------------------------------------------------------

// ingestRequest is the outer JSON envelope accepted by POST /api/v1/ingest.
// Payload remains raw JSON here so schema defects inside the feed are recorded
// asynchronously in DEAD_LETTER rather than failing the HTTP enqueue path.
type ingestRequest struct {
	SourceType  string          `json:"source_type"`
	PersonaSlug string          `json:"persona_slug"`
	Payload     json.RawMessage `json:"payload"`
}

// ingestResponse returns the created (or idempotently matched) ingestion_jobs record.
type ingestResponse struct {
	JobID          string  `json:"job_id"`
	Status         string  `json:"status"`
	IdempotencyKey *string `json:"idempotency_key"`
}

// JobSummary represents an ingestion_jobs record in list responses without the
// potentially large raw JSON payload.
type JobSummary struct {
	ID               string     `json:"id"`
	CanonicalUserID  *string    `json:"canonical_user_id"`
	SourceType       string     `json:"source_type"`
	IdempotencyKey   *string    `json:"idempotency_key"`
	Status           string     `json:"status"`
	AttemptCount     int        `json:"attempt_count"`
	MaxAttempts      int        `json:"max_attempts"`
	ReplayCount      int        `json:"replay_count"`
	LastErrorCode    *string    `json:"last_error_code"`
	LastErrorMessage *string    `json:"last_error_message"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}

// JobDetail extends JobSummary with the full raw JSON payload for DLQ inspection.
type JobDetail struct {
	JobSummary
	Payload json.RawMessage `json:"payload"`
}

type jobsListResponse struct {
	Jobs  []JobSummary `json:"jobs"`
	Total int          `json:"total"`
}

type jobDetailResponse struct {
	Job JobDetail `json:"job"`
}

// -----------------------------------------------------------------------------
// Shared SQL projections & validation helpers
// -----------------------------------------------------------------------------

// Shared column list kept in strict lockstep with scanJobSummary so list and
// detail queries scan identical fields.
const jobSummaryColumns = `
	id::text,
	canonical_user_id::text,
	source_type,
	idempotency_key,
	status::text,
	attempt_count,
	max_attempts,
	replay_count,
	last_error_code,
	last_error_message,
	created_at,
	updated_at,
	completed_at`

// scanJobSummary scans jobSummaryColumns followed by any extra trailing columns
// (such as the raw payload on single-job detail lookups).
func scanJobSummary(row pgx.Row, extraDest ...any) (JobSummary, error) {
	var j JobSummary
	dest := append([]any{
		&j.ID, &j.CanonicalUserID, &j.SourceType, &j.IdempotencyKey,
		&j.Status, &j.AttemptCount, &j.MaxAttempts, &j.ReplayCount,
		&j.LastErrorCode, &j.LastErrorMessage, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt,
	}, extraDest...)
	err := row.Scan(dest...)
	return j, err
}

// isAllowedIngestSource whitelists supported feed names at the enqueue boundary.
func isAllowedIngestSource(sourceType string) bool {
	switch sourceType {
	case "AA_DEPOSIT", "AA_EQUITIES", "CAS", "LIABILITY":
		return true
	default:
		return false
	}
}

// isValidJobStatus validates optional ?status= filters against the job_status enum.
func isValidJobStatus(status string) bool {
	switch status {
	case "PENDING", "PROCESSING", "RETRY_WAIT", "COMPLETED", "DEAD_LETTER":
		return true
	default:
		return false
	}
}

// -----------------------------------------------------------------------------
// HTTP handlers
// -----------------------------------------------------------------------------

// handlePostIngest serves POST /api/v1/ingest.
// Validates only the outer JSON envelope synchronously (returning 202 Accepted
// when a new job is enqueued, or 200 OK when an Idempotency-Key matches an
// existing job) and lets background SKIP LOCKED workers validate and post it.
func (s *server) handlePostIngest(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	// Cap request bodies at 8 MiB to guard memory while accommodating multi-year statements.
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "failed to read request body", nil)
		return
	}

	// Step 1: Validate outer envelope syntax, supported source_type, and JSON object payload.
	var req ingestRequest
	if err := json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON request body", map[string]any{"reason": err.Error()})
		return
	}

	req.SourceType = strings.TrimSpace(req.SourceType)
	if !isAllowedIngestSource(req.SourceType) {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "unsupported or missing source_type", map[string]any{"source_type": req.SourceType})
		return
	}

	// Require payload to be a JSON object rather than an array, scalar, or null.
	var payloadObj map[string]json.RawMessage
	if err := json.Unmarshal(req.Payload, &payloadObj); err != nil || payloadObj == nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "payload must be a JSON object", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Step 2: If persona_slug is specified, bind the job to that ACTIVE Canonical User;
	// omit canonical_user_id when unlinked so the worker can resolve or create the user.
	var canonicalUserID *string
	if slug := strings.TrimSpace(req.PersonaSlug); slug != "" {
		uid, err := s.lookupActiveUserIDBySlug(ctx, slug)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errCodePersonaNotFound, "persona not found", map[string]any{"persona": slug})
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to lookup persona", map[string]any{"reason": err.Error()})
			return
		}
		canonicalUserID = &uid
	}

	// Step 3: Capture optional Idempotency-Key header and hash raw payload for auditability.
	var idempotencyKey *string
	if rawKey := strings.TrimSpace(r.Header.Get("Idempotency-Key")); rawKey != "" {
		idempotencyKey = &rawKey
	}

	sum := sha256.Sum256(req.Payload)
	payloadSHA := hex.EncodeToString(sum[:])

	// Step 4: Insert the PENDING job; ON CONFLICT DO NOTHING detects matching
	// non-null Idempotency-Key headers without failing the transaction.
	var resp ingestResponse
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ingestion_jobs (canonical_user_id, source_type, idempotency_key, payload, payload_sha256)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id::text, status::text, idempotency_key
	`, canonicalUserID, req.SourceType, idempotencyKey, string(req.Payload), payloadSHA).Scan(&resp.JobID, &resp.Status, &resp.IdempotencyKey)

	// When ON CONFLICT skipped the insert, return the existing job with 200 OK.
	if errors.Is(err, pgx.ErrNoRows) && idempotencyKey != nil {
		if lookupErr := s.pool.QueryRow(ctx, `
			SELECT id::text, status::text, idempotency_key
			FROM ingestion_jobs
			WHERE idempotency_key = $1
		`, *idempotencyKey).Scan(&resp.JobID, &resp.Status, &resp.IdempotencyKey); lookupErr != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to load existing idempotent job", map[string]any{"reason": lookupErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to enqueue ingestion job", map[string]any{"reason": err.Error()})
		return
	}

	writeJSON(w, http.StatusAccepted, resp)
}

// handleGetJobs serves GET /api/v1/jobs with optional ?status=, ?persona=,
// ?limit=, and ?offset= filters, ordered newest-first.
func (s *server) handleGetJobs(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	q := r.URL.Query()
	statusFilter := strings.TrimSpace(q.Get("status"))
	if statusFilter != "" && !isValidJobStatus(statusFilter) {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "invalid status filter", map[string]any{"status": statusFilter})
		return
	}

	limit, offset, err := parsePagination(q.Get("limit"), q.Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, err.Error(), nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Resolve optional ?persona=<slug> into canonical_user_id so callers can scope
	// the queue to a single Persona without exposing other Personas' jobs.
	var userFilter string
	if slug := strings.TrimSpace(q.Get("persona")); slug != "" {
		persona, ok := s.resolvePersonaOrError(ctx, w, slug)
		if !ok {
			return
		}
		userFilter = persona.ID
	}

	// Count total matching jobs for pagination before fetching the requested slice.
	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int
		FROM ingestion_jobs
		WHERE ($1 = '' OR status::text = $1)
		  AND ($2 = '' OR canonical_user_id::text = $2)
	`, statusFilter, userFilter).Scan(&total); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to count jobs", map[string]any{"reason": err.Error()})
		return
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+jobSummaryColumns+`
		FROM ingestion_jobs
		WHERE ($1 = '' OR status::text = $1)
		  AND ($2 = '' OR canonical_user_id::text = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4
	`, statusFilter, userFilter, limit, offset)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query jobs", map[string]any{"reason": err.Error()})
		return
	}
	defer rows.Close()

	// Pre-allocate an empty slice so JSON serialization yields [] instead of null when empty.
	jobs := make([]JobSummary, 0)
	for rows.Next() {
		j, err := scanJobSummary(rows)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to scan job", map[string]any{"reason": err.Error()})
			return
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to iterate jobs", map[string]any{"reason": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, jobsListResponse{
		Jobs:  jobs,
		Total: total,
	})
}

// handleGetJobByID serves GET /api/v1/jobs/{id}, returning the full job detail
// including its raw JSON payload and enforcing optional ?persona= ownership scoping.
func (s *server) handleGetJobByID(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	jobID := strings.TrimSpace(r.PathValue("id"))
	if !isValidUUID(jobID) {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "invalid job id", map[string]any{"id": jobID})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var userFilter string
	if slug := strings.TrimSpace(r.URL.Query().Get("persona")); slug != "" {
		persona, ok := s.resolvePersonaOrError(ctx, w, slug)
		if !ok {
			return
		}
		userFilter = persona.ID
	}

	var payload json.RawMessage
	summary, err := scanJobSummary(s.pool.QueryRow(ctx, `
		SELECT `+jobSummaryColumns+`, payload
		FROM ingestion_jobs
		WHERE id = $1
		  AND ($2 = '' OR canonical_user_id::text = $2)
	`, jobID, userFilter), &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errCodeNotFound, "job not found", map[string]any{"id": jobID})
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to fetch job", map[string]any{"reason": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, jobDetailResponse{
		Job: JobDetail{
			JobSummary: summary,
			Payload:    payload,
		},
	})
}

// -----------------------------------------------------------------------------
// Query & UUID parsing helpers
// -----------------------------------------------------------------------------

// lookupActiveUserIDBySlug resolves an ACTIVE Canonical User UUID by persona_slug.
func (s *server) lookupActiveUserIDBySlug(ctx context.Context, slug string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text
		FROM users
		WHERE persona_slug = $1 AND status = 'ACTIVE'
	`, slug).Scan(&id)
	return id, err
}

// parsePagination validates optional limit (1..maxPageLimit) and offset (>= 0) query params.
func parsePagination(limitRaw, offsetRaw string) (int, int, error) {
	limit := defaultPageLimit
	if s := strings.TrimSpace(limitRaw); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 || n > maxPageLimit {
			return 0, 0, errors.New("invalid limit parameter")
		}
		limit = n
	}

	offset := 0
	if s := strings.TrimSpace(offsetRaw); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, 0, errors.New("invalid offset parameter")
		}
		offset = n
	}

	return limit, offset, nil
}

// isValidUUID performs a zero-allocation canonical 8-4-4-4-12 UUID format check
// before passing path IDs into PostgreSQL UUID parameters.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
