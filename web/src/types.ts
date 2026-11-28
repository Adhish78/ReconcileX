/**
 * Shared TypeScript contracts mirroring the Go HTTP API payloads and UI shell state.
 * Kept in sync with internal/api/server.go.
 */

/**
 * Represents an ACTIVE Canonical User returned by GET /api/v1/personas.
 * Each Persona embodies a pre-seeded showcase user with linked external identities.
 */
export interface Persona {
  id: string;
  persona_slug: string;
  display_name: string;
  linked_identity_count: number;
}

/**
 * Standard JSON error envelope returned by every 4xx/5xx endpoint in internal/api
 * (for example, ERR_DB_UNAVAILABLE or ERR_PERSONA_NOT_FOUND).
 */
export interface ApiErrorEnvelope {
  error: {
    code: string;
    message: string;
    details?: Record<string, unknown>;
  };
}

/** Top-level tab keys supported by the App navigation bar. */
export type ViewKey = "overview" | "reconciliation" | "review";

/**
 * Common props passed from <App /> into each top-level view component so every tab
 * reacts immediately when the user switches the active Persona in the header.
 */
export interface PersonaViewProps {
  /** Currently selected Persona, or null while loading or when no Personas are seeded. */
  persona: Persona | null;
}

export interface AccountSummary {
  id: string;
  canonical_user_id: string;
  source_type: string;
  natural_key: string;
  institution_name: string;
  fip_id: string;
  linked_acc_ref: string;
  masked_acc_number: string;
  currency: string;
  ledger_balance: string;
  reported_balance: string | null;
  reported_balance_at?: string | null;
  open_alert_count: number;
}

export interface LedgerEntryItem {
  id: string;
  coa_account: string;
  amount: string;
  units: string | null;
}

export interface LedgerTransactionItem {
  id: string;
  account_id: string;
  source: string;
  txn_type: string;
  category: string | null;
  txn_timestamp: string;
  value_date?: string | null;
  narration: string | null;
  reference: string | null;
  idempotency_key: string;
  entries: LedgerEntryItem[];
}

export interface PortfolioSummary {
  persona: {
    id: string;
    persona_slug: string;
    display_name: string;
  };
  as_of: string;
  net_worth: string;
  total_assets: string;
  total_liabilities: string;
  portfolio_xirr: number | null;
  accounts_summary: {
    bank_deposits: string;
    mutual_funds: string;
    equities: string;
    loans: string;
    credit_cards: string;
  };
  open_alert_count: number;
  uncategorized_count: number;
  dlq_job_count: number;
}

export interface JobSummary {
  id: string;
  canonical_user_id: string | null;
  source_type: string;
  idempotency_key: string | null;
  status: string;
  attempt_count: number;
  max_attempts: number;
  replay_count: number;
  last_error_code: string | null;
  last_error_message: string | null;
  created_at: string;
  updated_at: string;
  completed_at: string | null;
}
