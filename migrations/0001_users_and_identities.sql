-- =============================================================================
-- Migration 0001: Canonical Users & Linked Identities
--
-- Establishes the identity-resolution foundation for ReconcileX:
--   - `users`: stores Canonical Users (ACTIVE person records and MERGED
--     records soft-pointed to a surviving user).
--   - `linked_identities`: binds hashed external identifiers (PAN, mobile,
--     email) to a Canonical User with verification and source provenance.
--
-- Executed automatically on startup by internal/db.Migrate inside a single
-- advisory-locked transaction.
-- =============================================================================

-- Tracks whether a Canonical User is currently live (ACTIVE) or has been
-- consolidated into another Canonical User during an Entity Merge (MERGED).
CREATE TYPE user_status AS ENUM ('ACTIVE', 'MERGED');

-- Restricts the external identifier types that can participate in identity
-- resolution and Entity Merge matching. Raw PII values are never stored.
CREATE TYPE identity_type AS ENUM ('PAN_HASH', 'MOBILE_HASH', 'EMAIL_HASH');


-- -----------------------------------------------------------------------------
-- Table: users (Canonical Users)
--
-- Holds one row per Canonical User. Showcase Personas carry a unique
-- `persona_slug`, whereas normal ingested users keep `persona_slug` NULL.
-- -----------------------------------------------------------------------------
CREATE TABLE users (
    -- Surrogate UUID primary key generated automatically by PostgreSQL.
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Optional URL-safe slug for pre-seeded showcase Personas (e.g., 'aarav', 'priya');
    -- NULL for regular ingested users that are not showcase Personas.
    persona_slug TEXT UNIQUE,

    -- Human-readable display name shown in the Persona switcher and API responses.
    display_name TEXT NOT NULL,

    -- Lifecycle state of this Canonical User; newly created users start ACTIVE.
    status user_status NOT NULL DEFAULT 'ACTIVE',

    -- Self-referential pointer to the surviving Canonical User when status = 'MERGED'.
    -- Preserves historical auditability without deleting superseded user rows.
    merged_into_user_id UUID REFERENCES users(id),

    -- UTC audit timestamps recording creation and last modification.
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Enforce Entity Merge state consistency at the database level so invalid
    -- combinations fail immediately:
    --   1. ACTIVE users must never carry a merged_into_user_id pointer.
    --   2. MERGED users must point to a surviving user and cannot self-merge.
    CONSTRAINT users_status_merge_check CHECK (
        (status = 'ACTIVE' AND merged_into_user_id IS NULL)
        OR (status = 'MERGED' AND merged_into_user_id IS NOT NULL AND merged_into_user_id <> id)
    )
);

-- Partial index covering only ACTIVE rows so GET /api/v1/personas can resolve
-- active persona slugs quickly without scanning MERGED records.
CREATE INDEX idx_users_active_personas ON users (persona_slug) WHERE status = 'ACTIVE';


-- -----------------------------------------------------------------------------
-- Table: linked_identities
--
-- Associates hashed external identifiers with a Canonical User along with
-- verification status and upstream source provenance (e.g., AA vs. CAS).
-- -----------------------------------------------------------------------------
CREATE TABLE linked_identities (
    -- Surrogate UUID primary key for this identity link row.
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Foreign key to the owning Canonical User in `users`.
    canonical_user_id UUID NOT NULL REFERENCES users(id),

    -- Classification of the hashed identifier (PAN_HASH, MOBILE_HASH, or EMAIL_HASH).
    identity_type identity_type NOT NULL,

    -- Cryptographic hash of the external identifier (raw PAN/phone/email is never stored).
    identity_hash TEXT NOT NULL,

    -- Whether the upstream source verified ownership (e.g., AA FIP verified mobile
    -- vs. unverified CAS contact mobile). Only verified identifiers drive merges.
    is_verified BOOLEAN NOT NULL DEFAULT FALSE,

    -- Upstream ingestion source that reported this identifier (e.g., 'CAS', 'AA_DEPOSIT').
    source TEXT NOT NULL,

    -- Timestamp when verification occurred, or NULL for unverified links.
    verified_at TIMESTAMPTZ,

    -- Timestamp when this link row was first inserted.
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Global uniqueness invariant: a given (identity_type, identity_hash) pair
    -- can belong to at most one Canonical User row at any time.
    CONSTRAINT linked_identities_type_hash_unique UNIQUE (identity_type, identity_hash)
);

-- Composite index accelerating lookups of all identities (optionally filtered by type)
-- belonging to a given Canonical User during Entity Merge and /api/v1/personas counts.
CREATE INDEX idx_linked_identities_user ON linked_identities (canonical_user_id, identity_type);
