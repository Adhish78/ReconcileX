# Embed migrations and UI into a two-container zero-setup runtime

## Status

accepted

## Context

ReconcileX must boot cleanly from a single `docker compose up` with zero manual migrations, zero external message brokers, and zero live-network fragility against `api.mfapi.in`, while still isolating automated test runs (`go test -race`, fuzzing, `golangci-lint`, `benchstat`, Vitest) from showcase data.

## Decision

The runtime `docker compose up` topology consists strictly of two services: `postgres:16-alpine` and a single multi-stage `reconcilex` Go binary that embeds SQL migrations and the compiled Vite `web/dist` bundle via `embed.FS`. On startup (`RECONCILEX_AUTO_MIGRATE=true`, `RECONCILEX_AUTO_SEED=true`), `reconcilex` applies pending migrations, seeds the three personas only when `users` is empty, runs `4` `SKIP LOCKED` worker goroutines, and attempts `mfapi.in` NAV refreshes under short timeouts with automatic fallback to bundled `fixtures/nav/` snapshots (`RECONCILEX_OFFLINE_NAV` skips HTTP entirely). Test suites run under a separate `--profile test` (`postgres-test` on `tmpfs`, `test-go`, `test-web`) so tests never touch the live showcase volume.

## Considered Options

- **Separate Nginx/Node frontend + API + worker containers**: Rejected because it adds container orchestration overhead and breaks the single-binary, two-container zero-setup contract.
- **Hard failure when `api.mfapi.in` is unreachable**: Rejected because offline environments or upstream rate limits would prevent valuation and XIRR showcase rendering on first boot.

## Consequences

- Reviewers can launch the full UI, API, workers, and seeded personas with one `docker compose up` command without running separate migration or seed scripts.
- All frontend assets and migrations are compiled into the Go binary at build time (`node:20-alpine` -> `golang:1.23-alpine`), requiring a rebuild when `web/` changes outside Vite dev mode.
