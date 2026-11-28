/**
 * Root shell component for the ReconcileX UI.
 *
 * Responsibilities:
 *   1. Fetch active showcase Personas from GET /api/v1/personas on initial mount.
 *   2. Maintain the currently selected Persona slug and active view tab in local state.
 *   3. Surface backend errors in an accessible `role="alert"` banner.
 *   4. Pass the resolved `activePersona` down to Overview, Reconciliation, and Review views.
 */
import { useEffect, useState } from "react";
import { fetchPersonas } from "./api";
import type { Persona, ViewKey } from "./types";
import { OverviewView } from "./views/OverviewView";
import { ReconciliationView } from "./views/ReconciliationView";
import { ReviewView } from "./views/ReviewView";

// Ordered tab definitions driving the primary navigation strip.
const NAV_ITEMS: Array<{ key: ViewKey; label: string }> = [
  { key: "overview", label: "Overview" },
  { key: "reconciliation", label: "Reconciliation Workbench" },
  { key: "review", label: "Categorizer Review" }
];

export default function App() {
  // Active Canonical Users fetched from the backend.
  const [personas, setPersonas] = useState<Persona[]>([]);

  // Slug of the Persona currently chosen in the header <select> dropdown.
  const [activeSlug, setActiveSlug] = useState<string>("");

  // Currently selected primary view tab; defaults to Overview on first load.
  const [activeView, setActiveView] = useState<ViewKey>("overview");

  // Error banner text populated if fetchPersonas rejects (e.g., 503 ERR_DB_UNAVAILABLE).
  const [error, setError] = useState<string | null>(null);

  // Disables the Persona selector and renders "Loading..." until the initial fetch settles.
  const [loading, setLoading] = useState<boolean>(true);

  // Load all ACTIVE Personas once on mount.
  // The `cancelled` flag guards against React state updates if the component unmounts
  // (or StrictMode cleans up its first dev-mode effect run) before the promise settles.
  useEffect(() => {
    let cancelled = false;

    fetchPersonas()
      .then((list) => {
        if (cancelled) return;
        setPersonas(list);

        // Default-select the first available Persona unless the user already picked one.
        if (list.length > 0) {
          setActiveSlug((prev) => prev || list[0].persona_slug);
        }

        setError(null);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load personas");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  // Derive the full Persona object from activeSlug so child views receive display_name
  // and linked_identity_count without duplicating lookup logic.
  const activePersona = personas.find((p) => p.persona_slug === activeSlug) ?? null;

  return (
    <div className="app-shell">
      {/* Top application bar: product identity on the left, global Persona switcher on the right */}
      <header className="topbar">
        <div className="brand">
          <h1>ReconcileX</h1>
          <span className="subtitle">Double-Entry Wealth &amp; Liability Ledger</span>
        </div>

        {/* Persona dropdown selector + active Persona pill badge */}
        <div className="persona-controls">
          <label htmlFor="persona-select" className="persona-label">
            Active Persona
          </label>

          <select
            id="persona-select"
            aria-label="Active Persona"
            value={activeSlug}
            disabled={loading || personas.length === 0}
            onChange={(e) => setActiveSlug(e.target.value)}
            className="persona-select"
          >
            {personas.length === 0 ? (
              <option value="">{loading ? "Loading..." : "No personas"}</option>
            ) : (
              personas.map((p) => (
                <option key={p.id} value={p.persona_slug}>
                  {p.display_name} ({p.persona_slug})
                </option>
              ))
            )}
          </select>

          {activePersona && (
            <span data-testid="active-persona-badge" className="persona-badge">
              {activePersona.display_name}
            </span>
          )}
        </div>
      </header>

      {/* Primary view navigation tabs */}
      <nav className="nav-tabs" aria-label="Primary Views">
        {NAV_ITEMS.map((item) => (
          <button
            key={item.key}
            type="button"
            className={`nav-tab ${activeView === item.key ? "is-active" : ""}`}
            onClick={() => setActiveView(item.key)}
          >
            {item.label}
          </button>
        ))}
      </nav>

      {/* Accessible error banner surfaced when the backend returns an error or is unreachable */}
      {error && (
        <div role="alert" className="error-banner">
          {error}
        </div>
      )}

      {/* Active view panel receiving the currently selected Persona */}
      <main className="main-content">
        {activeView === "overview" && <OverviewView persona={activePersona} />}
        {activeView === "reconciliation" && <ReconciliationView persona={activePersona} />}
        {activeView === "review" && <ReviewView persona={activePersona} />}
      </main>
    </div>
  );
}
