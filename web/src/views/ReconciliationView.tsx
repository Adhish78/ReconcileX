import type { PersonaViewProps } from "../types";

/**
 * "Reconciliation Workbench" tab panel rendered inside App.
 *
 * Frame for inspecting ingestion queue progress, Reconciliation Alerts (BALANCE_DRIFT,
 * UNIT_DRIFT, VALUATION_DRIFT), and Entity Merge audit history scoped to the selected
 * Canonical User.
 */
export function ReconciliationView({ persona }: PersonaViewProps) {
  return (
    <section className="card">
      <h2>Reconciliation Workbench</h2>

      {/* Contextual summary scoped to the currently active Persona */}
      <p className="muted">
        {persona
          ? `Ingestion queue, Reconciliation Alerts, and Entity Merge audit log for ${persona.display_name}.`
          : "Select an active Canonical User to inspect reconciliation status."}
      </p>
    </section>
  );
}
