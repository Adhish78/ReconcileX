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
