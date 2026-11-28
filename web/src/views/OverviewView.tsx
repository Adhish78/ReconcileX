import type { PersonaViewProps } from "../types";

export function OverviewView({ persona }: PersonaViewProps) {
  return (
    <section className="card">
      <h2>Overview</h2>
      <p className="muted">
        {persona
          ? `Active Canonical User: ${persona.display_name} (${persona.persona_slug}) — ${persona.linked_identity_count} linked identities.`
          : "No active Canonical Users seeded yet."}
      </p>
    </section>
  );
}
