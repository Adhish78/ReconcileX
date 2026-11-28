import type { PersonaViewProps } from "../types";

/**
 * "Categorizer Review" tab panel rendered inside App.
 *
 * Frame for reviewing raw transaction Narrations routed to UNCATEGORIZED_REVIEW
 * when deterministic Payment Rail rules cannot classify them with high confidence.
 */
export function ReviewView({ persona }: PersonaViewProps) {
  return (
    <section className="card">
      <h2>Categorizer Review</h2>

      {/* Contextual summary scoped to the currently active Persona */}
      <p className="muted">
        {persona
          ? `Uncategorized Review queue for ${persona.display_name}.`
          : "Select an active Canonical User to review narrations."}
      </p>
    </section>
  );
}
