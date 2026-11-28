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
	NextRunAt        time.Time  `json:"next_run_at"`
	ReplayedAt       *time.Time `json:"replayed_at"`
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

// replayRequest is the optional JSON body accepted by POST /api/v1/jobs/{id}/replay.
type replayRequest struct {
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

// Shared column list kept in strict lockstep with scanJobSummary so list,
// detail, and replay queries scan identical fields.
const jobSummaryColumns = `
	id::text,
	canonical_user_id::text,
	source_type,
	idempotency_key,
	status::text,
	attempt_count,
	max_attempts,
	replay_count,
	next_run_at,
	replayed_at,
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
		&j.NextRunAt, &j.ReplayedAt,
		&j.LastErrorCode, &j.LastErrorMessage, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt,
	}, extraDest...)
	err := row.Scan(dest...)
	return j, err
}

type validatedPayload struct {
	Raw    string
	SHA256 string
}

// validateAndHashPayloadObject checks that raw is a JSON object (not an array,
// scalar, or null) and returns its canonical string and SHA-256 hex digest.
func validateAndHashPayloadObject(raw json.RawMessage) (validatedPayload, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return validatedPayload{}, false
	}
	sum := sha256.Sum256(raw)
	return validatedPayload{
		Raw:    string(raw),
		SHA256: hex.EncodeToString(sum[:]),
	}, true
}

// resolveOptionalPersonaFilter resolves an optional ?persona=<slug> query param
// into a Canonical User UUID string, returning ("", true) when omitted or writing
// an error response and returning ("", false) when lookup fails.
func (s *server) resolveOptionalPersonaFilter(ctx context.Context, w http.ResponseWriter, slug string) (string, bool) {
	trimmed := strings.TrimSpace(slug)
	if trimmed == "" {
		return "", true
	}
	persona, ok := s.resolvePersonaOrError(ctx, w, trimmed)
	if !ok {
		return "", false
	}
	return persona.ID, true
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
	validated, ok := validateAndHashPayloadObject(req.Payload)
	if !ok {
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

	// Step 3: Capture optional Idempotency-Key header.
	var idempotencyKey *string
	if rawKey := strings.TrimSpace(r.Header.Get("Idempotency-Key")); rawKey != "" {
		idempotencyKey = &rawKey
	}

	// Step 4: Insert the PENDING job; ON CONFLICT DO NOTHING detects matching
	// non-null Idempotency-Key headers without failing the transaction.
	var resp ingestResponse
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ingestion_jobs (canonical_user_id, source_type, idempotency_key, payload, payload_sha256)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id::text, status::text, idempotency_key
	`, canonicalUserID, req.SourceType, idempotencyKey, validated.Raw, validated.SHA256).Scan(&resp.JobID, &resp.Status, &resp.IdempotencyKey)

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
	userFilter, ok := s.resolveOptionalPersonaFilter(ctx, w, q.Get("persona"))
	if !ok {
		return
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

	userFilter, ok := s.resolveOptionalPersonaFilter(ctx, w, r.URL.Query().Get("persona"))
	if !ok {
		return
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

// handlePostJobReplay serves POST /api/v1/jobs/{id}/replay.
// Transitions a DEAD_LETTER job back to PENDING, resets attempt_count = 0,
// increments replay_count, records replayed_at, and optionally replaces payload
// and payload_sha256 when a replacement JSON object is provided.
func (s *server) handlePostJobReplay(w http.ResponseWriter, r *http.Request) {
	if !s.requirePool(w) {
		return
	}

	jobID := strings.TrimSpace(r.PathValue("id"))
	if !isValidUUID(jobID) {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "invalid job id", map[string]any{"id": jobID})
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "failed to read request body", nil)
		return
	}

	// Empty body (or omitted/null payload) replays the stored payload as-is;
	// a non-null payload must be a valid JSON object replacing payload + payload_sha256.
	var newPayloadStr *string
	var newPayloadSHA *string
	if len(bytes.TrimSpace(bodyBytes)) > 0 {
		var req replayRequest
		dec := json.NewDecoder(bytes.NewReader(bodyBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON request body", map[string]any{"reason": err.Error()})
			return
		}
		var trailing any
		if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "unexpected trailing JSON data", nil)
			return
		}
		trimmedPayload := bytes.TrimSpace(req.Payload)
		if len(trimmedPayload) > 0 && string(trimmedPayload) != "null" {
			validated, ok := validateAndHashPayloadObject(req.Payload)
			if !ok {
				writeError(w, http.StatusBadRequest, errCodeInvalidRequest, "payload must be a JSON object", nil)
				return
			}
			newPayloadStr = &validated.Raw
			newPayloadSHA = &validated.SHA256
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userFilter, ok := s.resolveOptionalPersonaFilter(ctx, w, r.URL.Query().Get("persona"))
	if !ok {
		return
	}

	var outPayload json.RawMessage
	summary, err := scanJobSummary(s.pool.QueryRow(ctx, `
		UPDATE ingestion_jobs
		SET status = 'PENDING',
		    attempt_count = 0,
		    next_run_at = NOW(),
		    locked_by = NULL,
		    locked_until = NULL,
		    last_error_code = NULL,
		    last_error_message = NULL,
		    completed_at = NULL,
		    replay_count = replay_count + 1,
		    replayed_at = NOW(),
		    updated_at = NOW(),
		    payload = COALESCE($3::jsonb, payload),
		    payload_sha256 = COALESCE($4, payload_sha256)
		WHERE id = $1
		  AND ($2 = '' OR canonical_user_id::text = $2)
		  AND status = 'DEAD_LETTER'
		RETURNING `+jobSummaryColumns+`, payload
	`, jobID, userFilter, newPayloadStr, newPayloadSHA), &outPayload)

	if errors.Is(err, pgx.ErrNoRows) {
		var currentStatus string
		lookupErr := s.pool.QueryRow(ctx, `
			SELECT status::text
			FROM ingestion_jobs
			WHERE id = $1
			  AND ($2 = '' OR canonical_user_id::text = $2)
		`, jobID, userFilter).Scan(&currentStatus)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errCodeNotFound, "job not found", map[string]any{"id": jobID})
			return
		}
		if lookupErr != nil {
			writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to inspect job status", map[string]any{"reason": lookupErr.Error()})
			return
		}
		writeError(w, http.StatusConflict, errCodeJobNotReplayable, "only DEAD_LETTER jobs can be replayed", map[string]any{"id": jobID, "status": currentStatus})
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to replay job", map[string]any{"reason": err.Error()})
		return
	}

	writeJSON(w, http.StatusAccepted, jobDetailResponse{
		Job: JobDetail{
			JobSummary: summary,
			Payload:    outPayload,
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
