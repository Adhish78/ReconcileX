import { useEffect, useState } from "react";
import { fetchPersonas } from "./api";
import type { Persona, ViewKey } from "./types";
import { OverviewView } from "./views/OverviewView";
import { ReconciliationView } from "./views/ReconciliationView";
import { ReviewView } from "./views/ReviewView";

const NAV_ITEMS: Array<{ key: ViewKey; label: string }> = [
  { key: "overview", label: "Overview" },
  { key: "reconciliation", label: "Reconciliation Workbench" },
  { key: "review", label: "Categorizer Review" }
];

export default function App() {
  const [personas, setPersonas] = useState<Persona[]>([]);
  const [activeSlug, setActiveSlug] = useState<string>("");
  const [activeView, setActiveView] = useState<ViewKey>("overview");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState<boolean>(true);

  useEffect(() => {
    let cancelled = false;
    fetchPersonas()
      .then((list) => {
        if (cancelled) return;
        setPersonas(list);
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

  const activePersona = personas.find((p) => p.persona_slug === activeSlug) ?? null;

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="brand">
          <h1>ReconcileX</h1>
          <span className="subtitle">Double-Entry Wealth &amp; Liability Ledger</span>
        </div>

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

      {error && (
        <div role="alert" className="error-banner">
          {error}
        </div>
      )}

      <main className="main-content">
        {activeView === "overview" && <OverviewView persona={activePersona} />}
        {activeView === "reconciliation" && <ReconciliationView persona={activePersona} />}
        {activeView === "review" && <ReviewView persona={activePersona} />}
      </main>
    </div>
  );
}
