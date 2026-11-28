/**
 * Smoke tests for the ReconcileX App shell (<App />).
 *
 * Exercises Persona loading, dropdown switching, primary view tab navigation,
 * and API error banner rendering against mocked fetch() responses in jsdom.
 */
import { act } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "../App";
import { renderIntoDocument, type RenderHandle, waitForCondition } from "../testUtils";

/**
 * Intercepts globalThis.fetch to return a 200 OK JSON payload matching
 * GET /api/v1/personas with two deterministic showcase Personas.
 */
function mockPersonasFetch(
  personas = [
    { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma", linked_identity_count: 2 },
    { id: "u2", persona_slug: "priya", display_name: "Priya Nair", linked_identity_count: 3 }
  ]
) {
  return vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ personas }), {
      status: 200,
      headers: { "Content-Type": "application/json" }
    })
  );
}

describe("ReconcileX App Shell", () => {
  let handle: RenderHandle | null = null;

  // Tear down the mounted React root and restore fetch spies after each test
  // so DOM state and mocks never leak across test cases.
  afterEach(async () => {
    if (handle) {
      await handle.unmount();
      handle = null;
    }

    vi.restoreAllMocks();
  });

  it("loads personas, renders the Persona switcher, and switches active persona", async () => {
    mockPersonasFetch();
    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    // Wait for App's mount useEffect to populate the dropdown and auto-select "aarav".
    let selectEl: HTMLSelectElement | null = null;
    await waitForCondition(() => {
      selectEl = root.querySelector<HTMLSelectElement>('select[aria-label="Active Persona"]');
      expect(selectEl).not.toBeNull();
      expect(selectEl?.value).toBe("aarav");
    });

    expect(root.querySelector('[data-testid="active-persona-badge"]')?.textContent).toContain("Aarav Sharma");

    // Dispatch a bubbling change event on the <select> to simulate switching to "priya".
    await act(async () => {
      if (!selectEl) throw new Error("missing select");
      selectEl.value = "priya";
      selectEl.dispatchEvent(new Event("change", { bubbles: true }));
    });

    // Confirm both the select value and the header badge reflect the new Persona.
    expect(selectEl!.value).toBe("priya");
    expect(root.querySelector('[data-testid="active-persona-badge"]')?.textContent).toContain("Priya Nair");
  });

  it("navigates across Overview, Reconciliation Workbench, and Categorizer Review views", async () => {
    mockPersonasFetch();
    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    // Overview should be the initial active view on first render.
    await waitForCondition(() => {
      expect(root.querySelector("h2")?.textContent).toContain("Overview");
    });

    // Helper that finds a navigation tab button by label text and clicks it inside act().
    const clickNav = async (label: string) => {
      const btn = Array.from(root.querySelectorAll("button")).find((b) =>
        b.textContent?.includes(label)
      );
      expect(btn).toBeDefined();

      await act(async () => {
        btn!.click();
      });
    };

    // Switch through each remaining primary view and verify its heading renders.
    await clickNav("Reconciliation Workbench");
    expect(root.querySelector("h2")?.textContent).toContain("Reconciliation Workbench");

    await clickNav("Categorizer Review");
    expect(root.querySelector("h2")?.textContent).toContain("Categorizer Review");
  });

  it("renders an error banner when fetching personas fails", async () => {
    // Mock a 503 ERR_DB_UNAVAILABLE envelope matching internal/api's readiness failure shape.
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(
        JSON.stringify({
          error: { code: "ERR_DB_UNAVAILABLE", message: "database is unavailable", details: {} }
        }),
        { status: 503, headers: { "Content-Type": "application/json" } }
      )
    );

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    // Confirm App parses the error envelope and displays it inside an accessible alert banner.
    await waitForCondition(() => {
      const alert = root.querySelector('[role="alert"]');
      expect(alert).not.toBeNull();
      expect(alert?.textContent).toContain("database is unavailable");
    });
  });
});
