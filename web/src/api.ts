import type { ApiErrorEnvelope, Persona } from "./types";

async function parseErrorMessage(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as Partial<ApiErrorEnvelope>;
    if (body.error?.message) {
      return `${body.error.code}: ${body.error.message}`;
    }
  } catch {
    // Fall back to HTTP status when response body is not JSON.
  }
  return `HTTP ${res.status} ${res.statusText}`;
}

export async function fetchPersonas(): Promise<Persona[]> {
  const res = await fetch("/api/v1/personas");
  if (!res.ok) {
    throw new Error(await parseErrorMessage(res));
  }
  const data = (await res.json()) as { personas?: Persona[] };
  return data.personas ?? [];
}
