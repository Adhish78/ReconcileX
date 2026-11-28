CREATE TYPE user_status AS ENUM ('ACTIVE', 'MERGED');
CREATE TYPE identity_type AS ENUM ('PAN_HASH', 'MOBILE_HASH', 'EMAIL_HASH');

CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    persona_slug TEXT UNIQUE,
    display_name TEXT NOT NULL,
    status user_status NOT NULL DEFAULT 'ACTIVE',
    merged_into_user_id UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT users_status_merge_check CHECK (
        (status = 'ACTIVE' AND merged_into_user_id IS NULL)
        OR (status = 'MERGED' AND merged_into_user_id IS NOT NULL AND merged_into_user_id <> id)
    )
);

CREATE INDEX idx_users_active_personas ON users (persona_slug) WHERE status = 'ACTIVE';

CREATE TABLE linked_identities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_user_id UUID NOT NULL REFERENCES users(id),
    identity_type identity_type NOT NULL,
    identity_hash TEXT NOT NULL,
    is_verified BOOLEAN NOT NULL DEFAULT FALSE,
    source TEXT NOT NULL,
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT linked_identities_type_hash_unique UNIQUE (identity_type, identity_hash)
);

CREATE INDEX idx_linked_identities_user ON linked_identities (canonical_user_id, identity_type);
