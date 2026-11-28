import type { PersonaViewProps } from "../types";

export function ReviewView({ persona }: PersonaViewProps) {
  return (
    <section className="card">
      <h2>Categorizer Review</h2>
      <p className="muted">
        {persona
          ? `Uncategorized Review queue for ${persona.display_name}.`
          : "Select an active Canonical User to review narrations."}
      </p>
    </section>
  );
}
