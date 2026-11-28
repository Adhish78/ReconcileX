/**
 * Thin REST API client used by the React shell to communicate with the Go backend
 * (either embedded on the same origin in production or proxied by Vite during dev).
 */
import type {
  AccountSummary,
  ApiErrorEnvelope,
  JobDetail,
  JobSummary,
  LedgerTransactionItem,
  Persona,
  PortfolioSummary
} from "./types";

/**
 * Converts a non-2xx fetch Response into a readable error message.
 * Attempts to decode ReconcileX's `{ error: { code, message } }` JSON envelope first,
 * falling back to the raw HTTP status line if the response body is plain text or HTML.
 */
async function parseErrorMessage(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as Partial<ApiErrorEnvelope>;
    if (body.error?.message) {
      return `${body.error.code}: ${body.error.message}`;
    }
  } catch {
    // Fall back to HTTP status when response body is not JSON.
  }

  return `HTTP ${res.status} ${res.statusText}`;
}

/**
 * Shared fetch wrapper that validates HTTP status, surfaces backend API error
 * envelopes via parseErrorMessage, and parses the JSON body as T.
 */
async function fetchJSON<T>(url: string, init?: RequestInit): Promise<T> {
  const res = init ? await fetch(url, init) : await fetch(url);
  if (!res.ok) {
    throw new Error(await parseErrorMessage(res));
  }
  return (await res.json()) as T;
}

/**
 * Fetches all ACTIVE Canonical Users that have a persona_slug from GET /api/v1/personas.
 * Throws a descriptive Error (displayed in App's error banner) if the server returns an error.
 */
export async function fetchPersonas(): Promise<Persona[]> {
  const data = await fetchJSON<{ personas?: Persona[] }>("/api/v1/personas");
  return data.personas ?? [];
}

/**
 * Fetches the persona-scoped portfolio summary (fixed-point decimal strings).
 * Returns null if the response omits summary fields (e.g., partial test mocks).
 */
export async function fetchPortfolioSummary(personaSlug: string): Promise<PortfolioSummary | null> {
  const data = await fetchJSON<Partial<PortfolioSummary>>(
    `/api/v1/portfolio/summary?persona=${encodeURIComponent(personaSlug)}`
  );
  if (typeof data.net_worth !== "string" || !data.accounts_summary) {
    return null;
  }
  return data as PortfolioSummary;
}

/** Fetches Financial Accounts scoped to a Persona. */
export async function fetchAccounts(personaSlug: string): Promise<AccountSummary[]> {
  const data = await fetchJSON<{ accounts?: AccountSummary[] }>(
    `/api/v1/accounts?persona=${encodeURIComponent(personaSlug)}`
  );
  return data.accounts ?? [];
}

/** Fetches paginated Ledger Transactions and nested Ledger Entries for one account. */
export async function fetchAccountTransactions(
  accountId: string,
  personaSlug?: string,
  limit = 50,
  offset = 0
): Promise<LedgerTransactionItem[]> {
  const params = new URLSearchParams({
    limit: String(limit),
    offset: String(offset)
  });
  if (personaSlug) {
    params.set("persona", personaSlug);
  }
  const data = await fetchJSON<{ transactions?: LedgerTransactionItem[] }>(
    `/api/v1/accounts/${encodeURIComponent(accountId)}/transactions?${params.toString()}`
  );
  return data.transactions ?? [];
}

/** Fetches ingestion_jobs queue rows, optionally filtered by persona and status. */
export async function fetchJobs(personaSlug = "", status = ""): Promise<JobSummary[]> {
  const params = new URLSearchParams();
  if (personaSlug) {
    params.set("persona", personaSlug);
  }
  if (status) {
    params.set("status", status);
  }
  const qs = params.toString();
  const data = await fetchJSON<{ jobs?: JobSummary[] }>(`/api/v1/jobs${qs ? `?${qs}` : ""}`);
  return data.jobs ?? [];
}

/** Fetches full job detail including raw JSON payload for DLQ inspection. */
export async function fetchJobDetail(jobId: string, personaSlug?: string): Promise<JobDetail> {
  const qs = personaSlug ? `?persona=${encodeURIComponent(personaSlug)}` : "";
  const data = await fetchJSON<{ job: JobDetail }>(
    `/api/v1/jobs/${encodeURIComponent(jobId)}${qs}`
  );
  return data.job;
}

/** Replays a DEAD_LETTER job with an optional edited JSON object payload. */
export async function replayJob(
  jobId: string,
  payload?: Record<string, unknown>,
  personaSlug?: string
): Promise<JobDetail> {
  const qs = personaSlug ? `?persona=${encodeURIComponent(personaSlug)}` : "";
  const body = payload !== undefined ? { payload } : {};
  const data = await fetchJSON<{ job: JobDetail }>(
    `/api/v1/jobs/${encodeURIComponent(jobId)}/replay${qs}`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }
  );
  return data.job;
}

/**
 * Formats a fixed-point decimal string (e.g. "48500.0000" or "-1500.2500")
 * using Indian numbering grouping (lakhs/crores) directly on string digits so
 * large integers never lose precision in JS IEEE-754 numbers.
 */
export function formatINR(decimalStr: string | null | undefined): string {
  if (!decimalStr) {
    return "₹0.00";
  }

  // Separate sign, whole digits, and fractional digits without Number() conversion.
  const trimmed = decimalStr.trim();
  const neg = trimmed.startsWith("-");
  const unsigned = neg ? trimmed.slice(1) : trimmed;
  const [intRaw, fracRaw = ""] = unsigned.split(".");
  const intDigits = intRaw.replace(/^0+(?=\d)/, "") || "0";

  // Indian numbering system: keep the rightmost 3 digits together, then group preceding pairs.
  let grouped = intDigits;
  if (intDigits.length > 3) {
    const tail = intDigits.slice(-3);
    const head = intDigits.slice(0, -3).replace(/\B(?=(\d{2})+(?!\d))/g, ",");
    grouped = `${head},${tail}`;
  }

  // Display 2 fractional digits (paise) while preserving trailing-zero padding.
  const fracTwo = (fracRaw + "00").slice(0, 2);
  return `${neg ? "-" : ""}₹${grouped}.${fracTwo}`;
}
