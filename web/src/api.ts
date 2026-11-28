/**
 * Thin REST API client used by the React shell to communicate with the Go backend
 * (either embedded on the same origin in production or proxied by Vite during dev).
 */
import type { ApiErrorEnvelope, Persona } from "./types";

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
 * Fetches all ACTIVE Canonical Users that have a persona_slug from GET /api/v1/personas.
 * Throws a descriptive Error (displayed in App's error banner) if the server returns an error.
 */
export async function fetchPersonas(): Promise<Persona[]> {
  const res = await fetch("/api/v1/personas");

  if (!res.ok) {
    throw new Error(await parseErrorMessage(res));
  }

  const data = (await res.json()) as { personas?: Persona[] };

  // Normalize missing or null fields to an empty array so callers can map safely.
  return data.personas ?? [];
}
