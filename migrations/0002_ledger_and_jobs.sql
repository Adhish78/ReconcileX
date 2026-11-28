-- =============================================================================
-- Migration 0002: Double-Entry Ledger Core, Accounts, and Ingestion Jobs
--
-- Establishes the immutable double-entry ledger schema (ADR-0003) and the
-- PostgreSQL SKIP LOCKED job queue:
--   - Enums: coa_account (13 buckets), canonical_category (17 values),
--     account_source_type, job_status
--   - Tables: accounts, ingestion_jobs, ledger_transactions, ledger_entries
--   - Covering index on ledger_entries for zero-heap-fetch balance aggregation
--   - Database triggers enforcing zero-sum transactions (DEFERRABLE),
--     append-only ledger_entries, and category-only updates on ledger_transactions
-- =============================================================================

-- 13-bucket Chart of Accounts used for zero-sum double-entry ledger postings.
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

-- 17-value analytical Canonical Category enum kept separate from Chart of Accounts.
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

-- Supported Financial Account source types across AA, CAS, and Liabilities.
CREATE TYPE account_source_type AS ENUM (
    'DEPOSIT',
    'MUTUAL_FUND',
    'EQUITY',
    'HOME_LOAN',
    'AUTO_LOAN',
    'CREDIT_CARD'
);

-- Lifecycle states for asynchronous SKIP LOCKED ingestion_jobs processing.
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
-- Computed balances and units are never cached here; they are aggregated live
-- from ledger_entries.
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
    reported_balance NUMERIC(18,4),
    reported_units NUMERIC(18,6),
    reported_balance_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT accounts_user_source_key_unique UNIQUE (canonical_user_id, source_type, natural_key)
);

CREATE INDEX idx_accounts_user_source ON accounts (canonical_user_id, source_type);
CREATE INDEX idx_accounts_scheme_code ON accounts (scheme_code) WHERE scheme_code IS NOT NULL;


-- -----------------------------------------------------------------------------
-- Table: ingestion_jobs
--
-- PostgreSQL-backed asynchronous ingestion queue claimed via FOR UPDATE SKIP LOCKED.
-- -----------------------------------------------------------------------------
CREATE TABLE ingestion_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_user_id UUID REFERENCES users(id),
    source_type TEXT NOT NULL,
    idempotency_key TEXT,
    status job_status NOT NULL DEFAULT 'PENDING',
    payload JSONB NOT NULL,
    payload_sha256 TEXT NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INT NOT NULL DEFAULT 3 CHECK (max_attempts > 0),
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_by TEXT,
    locked_until TIMESTAMPTZ,
    last_error_code TEXT,
    last_error_message TEXT,
    replay_count INT NOT NULL DEFAULT 0 CHECK (replay_count >= 0),
    replayed_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX uidx_ingestion_jobs_idempotency
    ON ingestion_jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_ingestion_jobs_claim
    ON ingestion_jobs (next_run_at ASC, created_at ASC)
    WHERE status IN ('PENDING', 'RETRY_WAIT');

CREATE INDEX idx_ingestion_jobs_stale_lease
    ON ingestion_jobs (locked_until ASC)
    WHERE status = 'PROCESSING';

CREATE INDEX idx_ingestion_jobs_dlq
    ON ingestion_jobs (updated_at DESC)
    WHERE status = 'DEAD_LETTER';


-- -----------------------------------------------------------------------------
-- Table: ledger_transactions
--
-- Atomic financial events per Financial Account. Intentionally omits
-- canonical_user_id (ADR-0003) so Entity Merges never rewrite historical rows.
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

    CONSTRAINT ledger_txns_account_idempotency_unique UNIQUE (account_id, idempotency_key)
);

CREATE INDEX idx_ledger_txns_account_time
    ON ledger_transactions (account_id, txn_timestamp DESC);

CREATE INDEX idx_ledger_txns_uncategorized
    ON ledger_transactions (created_at DESC)
    WHERE category = 'UNCATEGORIZED_REVIEW';

CREATE INDEX idx_ledger_txns_category
    ON ledger_transactions (category, txn_timestamp DESC)
    WHERE category IS NOT NULL;

-- Block DELETE and allow UPDATE solely on (category, category_updated_at).
CREATE OR REPLACE FUNCTION fn_ledger_transactions_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ledger_transactions rows are immutable and cannot be deleted';
    END IF;

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

    CONSTRAINT ledger_entries_nonzero_check CHECK (
        amount <> 0 OR (units IS NOT NULL AND units <> 0)
    ),
    CONSTRAINT ledger_entries_units_coa_check CHECK (
        units IS NULL OR coa_account IN ('ASSET:MF_COST', 'ASSET:EQUITY_COST')
    )
);

-- Covering index enabling index-only balance and unit aggregation without heap fetches.
CREATE INDEX idx_ledger_entries_account_coa_cov
    ON ledger_entries (account_id, coa_account)
    INCLUDE (amount, units);

CREATE INDEX idx_ledger_entries_txn
    ON ledger_entries (transaction_id);

-- Strict append-only immutability trigger blocking UPDATE and DELETE on ledger_entries.
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

-- Deferred zero-sum and account-ownership consistency trigger per transaction_id.
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

        SELECT lt.account_id INTO tx_account_id
        FROM ledger_transactions lt
        WHERE lt.id = NEW.transaction_id;

        IF tx_account_id IS NULL OR tx_account_id <> NEW.account_id THEN
            RAISE EXCEPTION 'ledger_entry account_id (%) does not match ledger_transaction (%) account_id (%)',
                NEW.account_id, NEW.transaction_id, tx_account_id;
        END IF;
    END IF;

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
