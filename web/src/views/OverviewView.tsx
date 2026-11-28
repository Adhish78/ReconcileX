import type { PersonaViewProps } from "../types";

/**
 * Primary "Overview" tab panel rendered inside App.
 *
 * Displays the active Canonical User's display name, slug, and linked identity count
 * (or an empty-state prompt when no Personas have been seeded yet), serving as the
 * anchor frame for upcoming Net Worth, holdings, and XIRR summary cards.
 */
export function OverviewView({ persona }: PersonaViewProps) {
  return (
    <section className="card">
      <h2>Overview</h2>

      {/* Show Persona identity summary once loaded, or a neutral empty state before seeding */}
      <p className="muted">
        {persona
          ? `Active Canonical User: ${persona.display_name} (${persona.persona_slug}) — ${persona.linked_identity_count} linked identities.`
          : "No active Canonical Users seeded yet."}
      </p>
    </section>
  );
}
