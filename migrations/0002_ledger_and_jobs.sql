-- =============================================================================
-- Migration 0002: Double-Entry Ledger Core, Accounts, and Ingestion Jobs
--
-- Owns the immutable double-entry ledger schema (ADR-0003) and the PostgreSQL
-- SKIP LOCKED asynchronous ingestion queue. Downstream consumers include:
--   - internal/worker: claims PENDING jobs, upserts Financial Accounts, and
--     posts zero-sum Ledger Transactions and Ledger Entries.
--   - internal/api: serves /api/v1/ingest, /api/v1/jobs, /api/v1/accounts, and
--     /api/v1/portfolio/summary using live ledger_entries aggregation.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Domain Enums
-- -----------------------------------------------------------------------------

-- 13-bucket Chart of Accounts used exclusively for zero-sum double-entry balancing.
-- Kept separate from Canonical Category so analytical reclassification never mutates
-- accounting legs.
CREATE TYPE coa_account AS ENUM (
    'ASSET:BANK',
    'ASSET:MF_COST',
    'ASSET:EQUITY_COST',
    'LIABILITY:LOAN',
    'LIABILITY:CARD',
    'EQUITY:EXTERNAL_CLEARING',
    'EXPENSE:UPI_SPEND',
    'EXPENSE:TAX_STAMP_DUTY',
    'EXPENSE:LOAN_INTEREST',
    'EXPENSE:LOAN_FEES',
    'EXPENSE:CARD_INTEREST',
    'EXPENSE:CARD_FEES',
    'INCOME:SALARY_DIVIDEND'
);

-- 17-value analytical Canonical Category enum assigned to Ledger Transactions.
-- UNCATEGORIZED_REVIEW acts as the explicit terminal fallback for human review.
CREATE TYPE canonical_category AS ENUM (
    'TRANSFER:INVESTMENT',
    'TRANSFER:CARD_PAYMENT',
    'TRANSFER:LOAN_EMI',
    'TRANSFER:SELF',
    'TRANSFER:P2P',
    'TRANSFER:CASH_WITHDRAWAL',
    'EXPENSE:FOOD_DINING',
    'EXPENSE:GROCERIES',
    'EXPENSE:UTILITIES_BILLS',
    'EXPENSE:TRAVEL_COMMUTE',
    'EXPENSE:SHOPPING',
    'EXPENSE:HEALTHCARE',
    'EXPENSE:RENT_HOUSING',
    'EXPENSE:TAX_FEE',
    'INCOME:SALARY',
    'INCOME:DIVIDEND_INTEREST',
    'UNCATEGORIZED_REVIEW'
);

-- Supported Financial Account container types across AA, CAS, and Liability feeds.
CREATE TYPE account_source_type AS ENUM (
    'DEPOSIT',
    'MUTUAL_FUND',
    'EQUITY',
    'HOME_LOAN',
    'AUTO_LOAN',
    'CREDIT_CARD'
);

-- Lifecycle states for asynchronous FOR UPDATE SKIP LOCKED ingestion_jobs processing.
CREATE TYPE job_status AS ENUM (
    'PENDING',
    'PROCESSING',
    'RETRY_WAIT',
    'COMPLETED',
    'DEAD_LETTER'
);


-- -----------------------------------------------------------------------------
-- Table: accounts (Financial Accounts)
--
-- One row per reconcilable Financial Account owned by a Canonical User.
-- Computed balances and units are never cached on this table; they are always
-- aggregated live from ledger_entries to prevent cached-balance drift (ADR-0003).
-- -----------------------------------------------------------------------------
CREATE TABLE accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_user_id UUID NOT NULL REFERENCES users(id),
    source_type account_source_type NOT NULL,
    natural_key TEXT NOT NULL,
    institution_name TEXT NOT NULL,
    fip_id TEXT,
    linked_acc_ref TEXT,
    masked_acc_number TEXT,
    folio_number TEXT,
    isin TEXT,
    scheme_code INTEGER,
    currency CHAR(3) NOT NULL DEFAULT 'INR',

    -- Latest institution-reported snapshot used later for Drift reconciliation checks.
    reported_balance NUMERIC(18,4),
    reported_units NUMERIC(18,6),
    reported_balance_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Deduplicate re-ingested statements onto the same Financial Account per owner.
    CONSTRAINT accounts_user_source_key_unique UNIQUE (canonical_user_id, source_type, natural_key)
);

-- Speeds up persona-scoped account listings and summary rollups.
CREATE INDEX idx_accounts_user_source ON accounts (canonical_user_id, source_type);

-- Partial index for AMFI scheme lookups on mutual fund holdings.
CREATE INDEX idx_accounts_scheme_code ON accounts (scheme_code) WHERE scheme_code IS NOT NULL;


-- -----------------------------------------------------------------------------
-- Table: ingestion_jobs
--
-- PostgreSQL-backed asynchronous ingestion queue claimed via FOR UPDATE SKIP LOCKED.
-- Enqueued synchronously by POST /api/v1/ingest and drained by internal/worker.
-- -----------------------------------------------------------------------------
CREATE TABLE ingestion_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Nullable because unlinked payloads may arrive without a persona_slug and
    -- resolve or create their Canonical User asynchronously inside the worker.
    canonical_user_id UUID REFERENCES users(id),
    source_type TEXT NOT NULL,
    idempotency_key TEXT,
    status job_status NOT NULL DEFAULT 'PENDING',
    payload JSONB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INT NOT NULL DEFAULT 3 CHECK (max_attempts > 0),

    -- Scheduling and worker lease fields preventing concurrent double-claims.
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_by TEXT,
    locked_until TIMESTAMPTZ,

    -- Failure diagnostics and DLQ replay metadata inspected in the Workbench UI.
    last_error_code TEXT,
    last_error_message TEXT,
    replay_count INT NOT NULL DEFAULT 0 CHECK (replay_count >= 0),
    replayed_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Deduplicate HTTP retries carrying the same Idempotency-Key header.
CREATE UNIQUE INDEX uidx_ingestion_jobs_idempotency
    ON ingestion_jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- Covering partial index for the worker's FOR UPDATE SKIP LOCKED claim query.
CREATE INDEX idx_ingestion_jobs_claim
    ON ingestion_jobs (next_run_at ASC, created_at ASC)
    WHERE status IN ('PENDING', 'RETRY_WAIT');

-- Partial index for future stale-lease recovery sweeps on crashed workers.
CREATE INDEX idx_ingestion_jobs_stale_lease
    ON ingestion_jobs (locked_until ASC)
    WHERE status = 'PROCESSING';

-- Partial index supporting DLQ inspection ordered by most recent failures.
CREATE INDEX idx_ingestion_jobs_dlq
    ON ingestion_jobs (updated_at DESC)
    WHERE status = 'DEAD_LETTER';


-- -----------------------------------------------------------------------------
-- Table: ledger_transactions
--
-- Atomic financial events per Financial Account. Intentionally omits
-- canonical_user_id (ADR-0003) so Entity Merges only reassign accounts without
-- rewriting historical ledger rows.
-- -----------------------------------------------------------------------------
CREATE TABLE ledger_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id UUID NOT NULL REFERENCES accounts(id),
    ingestion_job_id UUID REFERENCES ingestion_jobs(id),
    source TEXT NOT NULL,
    txn_type TEXT NOT NULL,
    category canonical_category,
    category_updated_at TIMESTAMPTZ,
    txn_timestamp TIMESTAMPTZ NOT NULL,
    value_date DATE,
    narration TEXT,
    reference TEXT,
    idempotency_key TEXT NOT NULL,
    raw_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Deterministic per-account deduplication across overlapping statement windows.
    CONSTRAINT ledger_txns_account_idempotency_unique UNIQUE (account_id, idempotency_key)
);

-- Supports paginated transaction history ordered newest-first per account.
CREATE INDEX idx_ledger_txns_account_time
    ON ledger_transactions (account_id, txn_timestamp DESC);

-- Partial index powering the Review queue of UNCATEGORIZED_REVIEW transactions.
CREATE INDEX idx_ledger_txns_uncategorized
    ON ledger_transactions (created_at DESC)
    WHERE category = 'UNCATEGORIZED_REVIEW';

-- Supports category-filtered spend/income queries.
CREATE INDEX idx_ledger_txns_category
    ON ledger_transactions (category, txn_timestamp DESC)
    WHERE category IS NOT NULL;

-- Immutability guard for ledger_transactions: blocks all DELETE operations and
-- rejects any UPDATE that modifies columns other than (category, category_updated_at).
CREATE OR REPLACE FUNCTION fn_ledger_transactions_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ledger_transactions rows are immutable and cannot be deleted';
    END IF;

    -- Check every immutable column with IS DISTINCT FROM so NULL-to-NULL remains unchanged.
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.account_id IS DISTINCT FROM OLD.account_id
        OR NEW.ingestion_job_id IS DISTINCT FROM OLD.ingestion_job_id
        OR NEW.source IS DISTINCT FROM OLD.source
        OR NEW.txn_type IS DISTINCT FROM OLD.txn_type
        OR NEW.txn_timestamp IS DISTINCT FROM OLD.txn_timestamp
        OR NEW.value_date IS DISTINCT FROM OLD.value_date
        OR NEW.narration IS DISTINCT FROM OLD.narration
        OR NEW.reference IS DISTINCT FROM OLD.reference
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.raw_metadata IS DISTINCT FROM OLD.raw_metadata
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'ledger_transactions permits UPDATE solely on (category, category_updated_at)';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_ledger_transactions_immutable
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW
    EXECUTE FUNCTION fn_ledger_transactions_immutable();


-- -----------------------------------------------------------------------------
-- Table: ledger_entries
--
-- Append-only signed monetary and optional unit legs belonging to a
-- Ledger Transaction. Protected by zero-sum DEFERRABLE and immutability triggers.
-- -----------------------------------------------------------------------------
CREATE TABLE ledger_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES ledger_transactions(id),
    account_id UUID NOT NULL REFERENCES accounts(id),
    coa_account coa_account NOT NULL,
    amount NUMERIC(18,4) NOT NULL,
    units NUMERIC(18,6),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Allow zero-cash unit-only Corporate Actions / Costless Anchors when units <> 0,
    -- while forbidding completely empty zero-amount, zero-unit entries.
    CONSTRAINT ledger_entries_nonzero_check CHECK (
        amount <> 0 OR (units IS NOT NULL AND units <> 0)
    ),

    -- Restrict unit tracking strictly to investment cost buckets.
    CONSTRAINT ledger_entries_units_coa_check CHECK (
        units IS NULL OR coa_account IN ('ASSET:MF_COST', 'ASSET:EQUITY_COST')
    )
);

-- Covering index enabling index-only balance and unit aggregation without heap fetches.
CREATE INDEX idx_ledger_entries_account_coa_cov
    ON ledger_entries (account_id, coa_account)
    INCLUDE (amount, units);

-- Supports transaction-to-entries drilldown lookups.
CREATE INDEX idx_ledger_entries_txn
    ON ledger_entries (transaction_id);

-- Strict append-only immutability trigger blocking UPDATE and DELETE on ledger_entries.
-- Corrections must be posted as compensating Ledger Transactions rather than edits.
CREATE OR REPLACE FUNCTION fn_ledger_entries_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries rows are append-only and cannot be modified or deleted';
END;
$$;

CREATE TRIGGER trg_ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW
    EXECUTE FUNCTION fn_ledger_entries_immutable();

-- Deferred zero-sum and account-ownership consistency trigger evaluated per transaction_id
-- at COMMIT time so multi-leg inserts can complete within the transaction before checking.
CREATE OR REPLACE FUNCTION fn_ledger_entries_check_balanced()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_txn_id UUID;
    tx_account_id UUID;
    entry_sum NUMERIC(18,4);
BEGIN
    IF TG_OP = 'DELETE' THEN
        target_txn_id := OLD.transaction_id;
    ELSE
        target_txn_id := NEW.transaction_id;

        -- Ensure every Ledger Entry belongs to the same Financial Account as its parent
        -- Ledger Transaction so cross-account entries cannot corrupt per-account sums.
        SELECT lt.account_id INTO tx_account_id
        FROM ledger_transactions lt
        WHERE lt.id = NEW.transaction_id;

        IF tx_account_id IS NULL OR tx_account_id <> NEW.account_id THEN
            RAISE EXCEPTION 'ledger_entry account_id (%) does not match ledger_transaction (%) account_id (%)',
                NEW.account_id, NEW.transaction_id, tx_account_id;
        END IF;
    END IF;

    -- Verify double-entry conservation: all signed monetary legs of the transaction
    -- must sum to exactly zero before PostgreSQL allows COMMIT to succeed.
    SELECT COALESCE(SUM(le.amount), 0) INTO entry_sum
    FROM ledger_entries le
    WHERE le.transaction_id = target_txn_id;

    IF entry_sum <> 0 THEN
        RAISE EXCEPTION 'ledger_transaction (%) is unbalanced: SUM(amount) = %', target_txn_id, entry_sum;
    END IF;

    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_ledger_entries_balanced
    AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION fn_ledger_entries_check_balanced();
