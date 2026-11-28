package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Adhish78/ReconcileX/internal/api"
	"github.com/Adhish78/ReconcileX/internal/db"
	"github.com/Adhish78/ReconcileX/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RECONCILEX_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://reconcilex:reconcilex@localhost:5433/reconcilex_test?sslmode=disable"
	}
	return dsn
}

func openTestPool(t *testing.T, migrate bool) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "test_api"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS test_api CASCADE; CREATE SCHEMA test_api;`); err != nil {
		t.Fatalf("reset test_api schema: %v", err)
	}
	if migrate {
		if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return pool
}

func stubWebFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><html><body>ReconcileX</body></html>")},
	}
}

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, string(body))
	}
	return errResp.Error.Code
}

func TestHealthzAndReadyzProbes(t *testing.T) {
	// 1. Nil/unconnected pool: /healthz -> 200, /readyz -> 503 ERR_DB_UNAVAILABLE.
	srvUnconnected := api.NewServer(nil, migrations.FS, stubWebFS())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srvUnconnected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz expected 200, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srvUnconnected.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz without db expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "ERR_DB_UNAVAILABLE" {
		t.Fatalf("expected ERR_DB_UNAVAILABLE, got %q", code)
	}

	// 2. Connected but unmigrated pool: /readyz -> 503 ERR_DB_UNAVAILABLE.
	unmigratedPool := openTestPool(t, false)
	srvUnmigrated := api.NewServer(unmigratedPool, migrations.FS, stubWebFS())
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srvUnmigrated.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz unmigrated expected 503, got %d", rec.Code)
	}

	// 3. Connected and migrated pool: /readyz -> 200 OK.
	migratedPool := openTestPool(t, true)
	srvReady := api.NewServer(migratedPool, migrations.FS, stubWebFS())
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srvReady.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz migrated expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetPersonasListsActiveUsersAndHandlesUnknownSlug(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openTestPool(t, true)

	var aaravID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('aarav', 'Aarav Sharma', 'ACTIVE') RETURNING id`,
	).Scan(&aaravID); err != nil {
		t.Fatalf("insert aarav: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES ('priya', 'Priya Nair', 'ACTIVE')`,
	); err != nil {
		t.Fatalf("insert priya: %v", err)
	}
	// Non-persona ACTIVE user (NULL persona_slug) must not appear in /api/v1/personas.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status) VALUES (NULL, 'Ingested User', 'ACTIVE')`,
	); err != nil {
		t.Fatalf("insert null-slug user: %v", err)
	}
	// MERGED user must not be listed or resolved as ACTIVE.
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (persona_slug, display_name, status, merged_into_user_id) VALUES ('dup-aarav', 'Aarav Dup', 'MERGED', $1)`,
		aaravID,
	); err != nil {
		t.Fatalf("insert merged user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO linked_identities (canonical_user_id, identity_type, identity_hash, is_verified, source)
		 VALUES ($1, 'PAN_HASH', 'pan-aarav', true, 'CAS'), ($1, 'MOBILE_HASH', 'mob-aarav', true, 'AA_DEPOSIT')`,
		aaravID,
	); err != nil {
		t.Fatalf("insert linked identities: %v", err)
	}

	srv := api.NewServer(pool, migrations.FS, stubWebFS())

	// List all ACTIVE personas.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/personas", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/personas expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var listResp struct {
		Personas []struct {
			ID                  string `json:"id"`
			PersonaSlug         string `json:"persona_slug"`
			DisplayName         string `json:"display_name"`
			LinkedIdentityCount int    `json:"linked_identity_count"`
		} `json:"personas"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode personas response: %v", err)
	}
	if len(listResp.Personas) != 2 {
		t.Fatalf("expected 2 ACTIVE personas, got %d (%+v)", len(listResp.Personas), listResp.Personas)
	}
	if listResp.Personas[0].PersonaSlug != "aarav" || listResp.Personas[0].LinkedIdentityCount != 2 {
		t.Fatalf("unexpected first persona: %+v", listResp.Personas[0])
	}
	if listResp.Personas[1].PersonaSlug != "priya" || listResp.Personas[1].LinkedIdentityCount != 0 {
		t.Fatalf("unexpected second persona: %+v", listResp.Personas[1])
	}

	// Query known persona slug.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/personas?persona=priya", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/personas?persona=priya expected 200, got %d", rec.Code)
	}

	// Query MERGED user slug -> 404 ERR_PERSONA_NOT_FOUND.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/personas?persona=dup-aarav", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/personas?persona=dup-aarav expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "ERR_PERSONA_NOT_FOUND" {
		t.Fatalf("expected ERR_PERSONA_NOT_FOUND for merged user, got %q", code)
	}

	// Query unknown persona slug -> 404 ERR_PERSONA_NOT_FOUND.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/personas?persona=nonexistent", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/personas?persona=nonexistent expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "ERR_PERSONA_NOT_FOUND" {
		t.Fatalf("expected ERR_PERSONA_NOT_FOUND, got %q", code)
	}
}

func TestEmbeddedUIAndUnknownAPIRoute(t *testing.T) {
	srv := api.NewServer(nil, migrations.FS, stubWebFS())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / expected 200, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/unknown expected 404, got %d", rec.Code)
	}
}
