// Package api implements ReconcileX's HTTP surface:
//   - Liveness and readiness probes (`GET /healthz`, `GET /readyz`)
//   - Versioned REST endpoints (`GET /api/v1/personas`, plus JSON 404 fallback under `/api/`)
//   - Embedded React SPA asset serving with client-side route fallback to `index.html`
//
// Constructed by cmd/reconcilex in production and tested directly via httptest in server_test.go.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Adhish78/ReconcileX/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// -----------------------------------------------------------------------------
// Error codes & response models
// -----------------------------------------------------------------------------

// Machine-readable error codes serialized inside apiErrorBody.Code so frontend
// and API consumers can branch reliably without parsing human messages.
const (
	errCodeDBUnavailable   = "ERR_DB_UNAVAILABLE"
	errCodePersonaNotFound = "ERR_PERSONA_NOT_FOUND"
	errCodeNotFound        = "ERR_NOT_FOUND"
	errCodeInvalidRequest  = "ERR_INVALID_REQUEST"
)

// Persona represents an ACTIVE Canonical User returned by GET /api/v1/personas,
// including how many Linked Identities are bound to that user.
type Persona struct {
	ID                  string `json:"id"`
	PersonaSlug         string `json:"persona_slug"`
	DisplayName         string `json:"display_name"`
	LinkedIdentityCount int    `json:"linked_identity_count"`
}

// apiErrorBody carries the structured fields inside ReconcileX's error envelope.
type apiErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// apiErrorEnvelope wraps apiErrorBody under a top-level "error" key so all
// 4xx/5xx API responses share one consistent JSON shape.
type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

// -----------------------------------------------------------------------------
// Server & router construction
// -----------------------------------------------------------------------------

// server holds the runtime dependencies injected into HTTP handlers.
type server struct {
	pool         *pgxpool.Pool // PostgreSQL pool (may be nil in isolated probe/UI unit tests).
	migrationsFS fs.FS         // Embedded SQL migrations checked by /readyz via db.MigrationsReady.
	webFS        fs.FS         // Embedded compiled Vite UI assets served for non-API routes.
}

// NewServer builds the net/http ServeMux wiring probes, versioned API routes,
// an explicit JSON 404 handler for unknown /api/* paths, and the embedded SPA.
func NewServer(pool *pgxpool.Pool, migrationsFS fs.FS, webFS fs.FS) http.Handler {
	s := &server{
		pool:         pool,
		migrationsFS: migrationsFS,
		webFS:        webFS,
	}

	mux := http.NewServeMux()

	// Operational probes used by Docker/orchestrators to check process health and DB readiness.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Versioned REST API endpoints.
	mux.HandleFunc("GET /api/v1/personas", s.handleGetPersonas)
	mux.HandleFunc("POST /api/v1/ingest", s.handlePostIngest)
	mux.HandleFunc("GET /api/v1/jobs", s.handleGetJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJobByID)
	mux.HandleFunc("GET /api/v1/accounts", s.handleGetAccounts)
	mux.HandleFunc("GET /api/v1/accounts/{id}/transactions", s.handleGetAccountTransactions)
	mux.HandleFunc("GET /api/v1/portfolio/summary", s.handleGetPortfolioSummary)

	// Register "/api/" before "/" so unknown API endpoints return a structured JSON 404
	// instead of falling through to the SPA's index.html fallback.
	mux.HandleFunc("/api/", s.handleUnknownAPI)

	// Serve embedded static assets (JS/CSS) or fall back to index.html for client-side routes.
	mux.HandleFunc("/", s.handleUI)

	return mux
}

// -----------------------------------------------------------------------------
// Health & readiness probes
// -----------------------------------------------------------------------------

// handleHealthz serves GET /healthz as a pure process-liveness check.
// It deliberately avoids touching PostgreSQL so container restarts aren't triggered
// merely because the database is temporarily unreachable.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz serves GET /readyz to confirm the service can handle real traffic:
// PostgreSQL must answer Ping() and every embedded SQL migration must be applied.
// Returns 503 ERR_DB_UNAVAILABLE with diagnostic details whenever unready.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Bound the probe with a short 3s timeout so stalled DB sockets don't hang healthchecks.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	ready, err := db.MigrationsReady(ctx, s.pool, s.migrationsFS)
	if err != nil || !ready {
		details := map[string]any{"reason": "pending migrations"}
		if err != nil {
			details["reason"] = err.Error()
		}

		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "database is unavailable or migrations are pending", details)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// -----------------------------------------------------------------------------
// Persona endpoints
// -----------------------------------------------------------------------------

// handleGetPersonas serves GET /api/v1/personas.
//   - With no `persona` query param: returns all ACTIVE Canonical Users that have a persona_slug.
//   - With `?persona=<slug>`: returns that single active Persona, or 404 ERR_PERSONA_NOT_FOUND
//     if the slug does not exist or belongs to a MERGED user.
func (s *server) handleGetPersonas(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "database connection is not initialized", map[string]any{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Normalize optional ?persona=<slug> query filter; empty string means "list all".
	slug := strings.TrimSpace(r.URL.Query().Get("persona"))

	personas, err := s.queryActivePersonas(ctx, slug)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query personas", map[string]any{"reason": err.Error()})
		return
	}

	// Only return 404 when the caller explicitly requested a specific slug that matched nothing;
	// an empty database on an unfiltered list request returns 200 with `{"personas": []}`.
	if slug != "" && len(personas) == 0 {
		writeError(w, http.StatusNotFound, errCodePersonaNotFound, "persona not found", map[string]any{"persona": slug})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"personas": personas})
}

// queryActivePersonas fetches ACTIVE Canonical Users with non-null persona_slugs and
// joins linked_identities to count how many hashed identifiers belong to each Persona.
// Passing an empty slug returns every active Persona ordered deterministically by slug.
func (s *server) queryActivePersonas(ctx context.Context, slug string) ([]Persona, error) {
	// LEFT JOIN ensures Personas with zero linked identities still appear with count 0;
	// WHERE filters out non-persona users (NULL slug) and soft-merged users (status = 'MERGED').
	rows, err := s.pool.Query(ctx, `
		SELECT
			u.id::text,
			u.persona_slug,
			u.display_name,
			COUNT(li.id)::int AS linked_identity_count
		FROM users u
		LEFT JOIN linked_identities li ON li.canonical_user_id = u.id
		WHERE u.status = 'ACTIVE'
		  AND u.persona_slug IS NOT NULL
		  AND ($1 = '' OR u.persona_slug = $1)
		GROUP BY u.id, u.persona_slug, u.display_name
		ORDER BY u.persona_slug ASC, u.created_at ASC
	`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Pre-allocate an empty non-nil slice so Go encodes empty results as `[]` rather than `null`.
	personas := make([]Persona, 0)

	for rows.Next() {
		var p Persona
		if err := rows.Scan(&p.ID, &p.PersonaSlug, &p.DisplayName, &p.LinkedIdentityCount); err != nil {
			return nil, err
		}
		personas = append(personas, p)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return personas, nil
}

// handleUnknownAPI catches any unrecognized route under /api/ and responds with
// a standard JSON 404 envelope rather than serving the React index.html page.
func (s *server) handleUnknownAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, errCodeNotFound, "endpoint not found", map[string]any{})
}

// -----------------------------------------------------------------------------
// Embedded SPA static file serving
// -----------------------------------------------------------------------------

// handleUI serves compiled frontend files from webFS and falls back to index.html
// for unknown non-API paths so client-side React routing works on direct page reloads.
func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if s.webFS == nil {
		http.NotFound(w, r)
		return
	}

	// Sanitize the URL path with path.Clean (blocking ".." traversal) and drop the
	// leading "/" because Go's io/fs expects relative paths without a leading slash.
	cleaned := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if cleaned == "" {
		cleaned = "index.html"
	}

	// Try serving an exact static asset match first (e.g., index.html, assets/index-*.js).
	if data, ok := readStaticFile(s.webFS, cleaned); ok {
		serveBytes(w, cleaned, data)
		return
	}

	// SPA fallback: return index.html so React can handle client-side routes.
	if data, ok := readStaticFile(s.webFS, "index.html"); ok {
		serveBytes(w, "index.html", data)
		return
	}

	http.NotFound(w, r)
}

// readStaticFile opens a non-directory file from fsys, returning (nil, false) if it
// does not exist, cannot be read, or resolves to a directory.
func readStaticFile(fsys fs.FS, name string) ([]byte, bool) {
	info, err := fs.Stat(fsys, name)
	if err != nil || info.IsDir() {
		return nil, false
	}

	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, false
	}

	return data, true
}

// serveBytes writes raw file bytes with a MIME Content-Type inferred from the filename extension.
func serveBytes(w http.ResponseWriter, name string, data []byte) {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// -----------------------------------------------------------------------------
// JSON response helpers
// -----------------------------------------------------------------------------

// writeJSON sets the UTF-8 JSON Content-Type header, writes status, and encodes payload.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError writes ReconcileX's uniform `{ "error": { code, message, details } }` payload.
// Nil details maps are normalized to an empty map so JSON always emits `{}` instead of `null`.
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}

	writeJSON(w, status, apiErrorEnvelope{
		Error: apiErrorBody{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}
