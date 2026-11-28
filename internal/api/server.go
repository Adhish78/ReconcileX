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

const (
	errCodeDBUnavailable   = "ERR_DB_UNAVAILABLE"
	errCodePersonaNotFound = "ERR_PERSONA_NOT_FOUND"
	errCodeNotFound        = "ERR_NOT_FOUND"
)

// Persona represents an ACTIVE Canonical User returned by /api/v1/personas.
type Persona struct {
	ID                  string `json:"id"`
	PersonaSlug         string `json:"persona_slug"`
	DisplayName         string `json:"display_name"`
	LinkedIdentityCount int    `json:"linked_identity_count"`
}

type apiErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

type server struct {
	pool         *pgxpool.Pool
	migrationsFS fs.FS
	webFS        fs.FS
}

// NewServer constructs the HTTP handler for health probes, versioned APIs, and the embedded UI.
func NewServer(pool *pgxpool.Pool, migrationsFS fs.FS, webFS fs.FS) http.Handler {
	s := &server{
		pool:         pool,
		migrationsFS: migrationsFS,
		webFS:        webFS,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /api/v1/personas", s.handleGetPersonas)
	mux.HandleFunc("/api/", s.handleUnknownAPI)
	mux.HandleFunc("/", s.handleUI)
	return mux
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
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

func (s *server) handleGetPersonas(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "database connection is not initialized", map[string]any{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	slug := strings.TrimSpace(r.URL.Query().Get("persona"))
	personas, err := s.queryActivePersonas(ctx, slug)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errCodeDBUnavailable, "failed to query personas", map[string]any{"reason": err.Error()})
		return
	}
	if slug != "" && len(personas) == 0 {
		writeError(w, http.StatusNotFound, errCodePersonaNotFound, "persona not found", map[string]any{"persona": slug})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": personas})
}

func (s *server) queryActivePersonas(ctx context.Context, slug string) ([]Persona, error) {
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

func (s *server) handleUnknownAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, errCodeNotFound, "endpoint not found", map[string]any{})
}

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if s.webFS == nil {
		http.NotFound(w, r)
		return
	}

	cleaned := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if cleaned == "" {
		cleaned = "index.html"
	}

	if data, ok := readStaticFile(s.webFS, cleaned); ok {
		serveBytes(w, cleaned, data)
		return
	}

	// SPA fallback to index.html for client-side routes.
	if data, ok := readStaticFile(s.webFS, "index.html"); ok {
		serveBytes(w, "index.html", data)
		return
	}
	http.NotFound(w, r)
}

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

func serveBytes(w http.ResponseWriter, name string, data []byte) {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

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
