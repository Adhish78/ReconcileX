import type { PersonaViewProps } from "../types";

export function ReconciliationView({ persona }: PersonaViewProps) {
  return (
    <section className="card">
      <h2>Reconciliation Workbench</h2>
      <p className="muted">
        {persona
          ? `Ingestion queue, Reconciliation Alerts, and Entity Merge audit log for ${persona.display_name}.`
          : "Select an active Canonical User to inspect reconciliation status."}
      </p>
    </section>
  );
}
