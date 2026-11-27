# Seed showcase data via deterministic Go generator and real ingestion pipeline

## Status

accepted

## Context

ReconcileX must auto-seed three multi-account personas (`Aarav`, `Priya`, `Rohan`), 10,500+ realistic Indian banking **Narrations**, pre-seeded **Reconciliation Alerts**, an **Entity Merge**, and a `DEAD_LETTER` job on cold Docker boot (`RECONCILEX_AUTO_SEED=true`) and `POST /api/v1/chaos/reset`, while remaining fast (`~2–3s`), reproducible across runs, and immune to repository bloat or wall-clock drift.

## Decision

Static CAS, AA, Liability, Chaos, and offline NAV snapshots live under `fixtures/` and are embedded into the binary via `embed.FS` alongside a shared `internal/seeder` package used by `RECONCILEX_AUTO_SEED`, `POST /api/v1/chaos/reset`, and `cmd/seeder`. The 10,500+ banking **Narrations** are synthesized deterministically in Go (fixed PRNG seed, exact `NUMERIC(18,4)` running `transactionalBalance` chaining) anchored to fixed calendar dates (`2024-04-01`–`2026-10-31`, with `fixtures/nav/` extending to `2026-11-20`) and executed in ~500-transaction chunks through the real `ingestion_jobs`, categorizer, reconciliation, and merge pipeline rather than direct SQL inserts. Clock-independent `N/A` XIRR on Rohan's liquid holding uses `GIFT_IN` (single-sign / `< 2` cash-flow guard) rather than a `< 1`-day window.

## Considered Options

- **Static `seed.sql` dump or multi-megabyte committed JSON fixtures**: Rejected because raw SQL seeding bypasses double-entry ledger triggers, `OPENING_ANCHOR` derivation, categorizer execution, and drift checks, while huge static JSON files bloat git history.
- **Dates computed relative to `time.Now()`**: Rejected because moving timestamps alter `idempotency_key` hashes, `ACTUAL/365` XIRR day counts, and golden test outputs—and can cause `< 1`-day XIRR edge cases to age out when live `api.mfapi.in` NAVs advance.

## Consequences

- Every seeded balance, `OPENING_ANCHOR`, `Reconciliation Alert`, `Entity Merge`, and `DEAD_LETTER` row is produced by the same production code paths exercised by live `/api/v1/ingest` and Chaos requests.
- Benchmarks (`V1 -> V2`) and UI states remain byte-for-byte reproducible across resets, offline environments, and future dates.
