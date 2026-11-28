// Package api_test exercises ReconcileX's HTTP probes, /api/v1/personas endpoint,
// error envelope formatting, and embedded SPA routing via httptest.NewRecorder.
// Tests requiring PostgreSQL run against the isolated `test_api` schema inside
// compose's ephemeral postgres-test instance so they never collide with internal/db tests.
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

// -----------------------------------------------------------------------------
// Test harness helpers
// -----------------------------------------------------------------------------

// testDSN resolves the PostgreSQL connection string from RECONCILEX_TEST_DSN,
// falling back to the local compose postgres-test port (5433) when run directly.
func testDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("RECONCILEX_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://reconcilex:reconcilex@localhost:5433/reconcilex_test?sslmode=disable"
	}

	return dsn
}

// openTestPool connects to the test PostgreSQL instance and recreates an isolated
// `test_api` schema. When migrate is true, embedded migrations are applied immediately;
// passing false leaves the schema empty so /readyz can be tested against an unmigrated DB.
func openTestPool(t *testing.T, migrate bool) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}

	// Pin search_path to test_api so API tests stay isolated from db_test's test_db schema.
	cfg.ConnConfig.RuntimeParams["search_path"] = "test_api"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)

	// Wipe and recreate test_api so every test starts with deterministic state.
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

// stubWebFS provides a minimal in-memory filesystem containing index.html so Go API
// tests run cleanly even before `npm run build` has populated web/dist.
func stubWebFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><html><body>ReconcileX</body></html>")},
	}
}

// decodeErrorCode parses ReconcileX's `{ "error": { "code": ... } }` JSON envelope
// and returns the machine-readable error code for assertions.
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

// -----------------------------------------------------------------------------
// Probe, Persona API, and Routing Tests
// -----------------------------------------------------------------------------

// TestHealthzAndReadyzProbes verifies that /healthz stays 200 OK even without a DB,
// while /readyz transitions from 503 ERR_DB_UNAVAILABLE (nil or unmigrated DB) to 200 OK
// once migrations have been applied.
func TestHealthzAndReadyzProbes(t *testing.T) {
	// Case 1: Nil/unconnected pool -> /healthz still succeeds (200), /readyz fails (503 ERR_DB_UNAVAILABLE).
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

	// Case 2: Connected to PostgreSQL, but migrations have not run yet -> /readyz returns 503.
	unmigratedPool := openTestPool(t, false)
	srvUnmigrated := api.NewServer(unmigratedPool, migrations.FS, stubWebFS())
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srvUnmigrated.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz unmigrated expected 503, got %d", rec.Code)
	}

	// Case 3: Connected and fully migrated -> /readyz returns 200 OK.
	migratedPool := openTestPool(t, true)
	srvReady := api.NewServer(migratedPool, migrations.FS, stubWebFS())
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	srvReady.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz migrated expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGetPersonasListsActiveUsersAndHandlesUnknownSlug verifies /api/v1/personas behavior:
//   - Lists only ACTIVE users with non-null persona_slugs and accurate linked_identity_count.
//   - Excludes ingested ACTIVE users (NULL persona_slug) and MERGED users.
//   - Filters by ?persona=<slug> and returns 404 ERR_PERSONA_NOT_FOUND for unknown or MERGED slugs.
func TestGetPersonasListsActiveUsersAndHandlesUnknownSlug(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openTestPool(t, true)

	// Seed two active showcase Personas ('aarav' and 'priya'), one regular user with NULL slug,
	// one MERGED duplicate pointing at aarav, and two linked identities bound to aarav.
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

	// Check 1: Unfiltered list returns only the two ACTIVE Personas sorted alphabetically with identity counts.
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

	// Check 2: Filtering by a known ACTIVE persona slug succeeds with 200 OK.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/personas?persona=priya", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/personas?persona=priya expected 200, got %d", rec.Code)
	}

	// Check 3: Querying a MERGED user's slug must return 404 ERR_PERSONA_NOT_FOUND.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/personas?persona=dup-aarav", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/personas?persona=dup-aarav expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "ERR_PERSONA_NOT_FOUND" {
		t.Fatalf("expected ERR_PERSONA_NOT_FOUND for merged user, got %q", code)
	}

	// Check 4: Querying an unknown persona slug must return 404 ERR_PERSONA_NOT_FOUND.
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

// TestEmbeddedUIAndUnknownAPIRoute verifies that "/" serves the embedded SPA index.html
// while unknown "/api/*" endpoints return 404 rather than falling back to the UI shell.
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
