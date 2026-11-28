export interface Persona {
  id: string;
  persona_slug: string;
  display_name: string;
  linked_identity_count: number;
}

export interface ApiErrorEnvelope {
  error: {
    code: string;
    message: string;
    details?: Record<string, unknown>;
  };
}

export type ViewKey = "overview" | "reconciliation" | "review";

export interface PersonaViewProps {
  persona: Persona | null;
}
