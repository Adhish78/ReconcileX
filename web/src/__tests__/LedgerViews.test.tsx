/**
 * Component integration tests for the Issue #14 UI views rendered inside <App />:
 *   - Overview Financial Account balances, Indian INR formatting, and double-entry
 *     transaction drilldowns
 *   - Reconciliation Workbench ingestion_jobs queue and DLQ error code rendering
 */
import { act } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import App from "../App";
import { renderIntoDocument, type RenderHandle, waitForCondition } from "../testUtils";

// Builds a synthetic JSON Response for stubbed globalThis.fetch handlers.
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}

// Handles shared persona, portfolio summary, and empty-accounts fetch stubs for Workbench tests.
function mockWorkbenchShellRequests(url: string, dlqJobCount = 1): Response | null {
  if (url === "/api/v1/personas") {
    return jsonResponse({
      personas: [
        { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma", linked_identity_count: 2 }
      ]
    });
  }
  if (url.startsWith("/api/v1/portfolio/summary")) {
    return jsonResponse({
      persona: { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma" },
      as_of: "2026-06-15T14:20:10Z",
      net_worth: "0.0000",
      total_assets: "0.0000",
      total_liabilities: "0.0000",
      portfolio_xirr: null,
      accounts_summary: {
        bank_deposits: "0.0000",
        mutual_funds: "0.0000",
        equities: "0.0000",
        loans: "0.0000",
        credit_cards: "0.0000"
      },
      open_alert_count: 0,
      uncategorized_count: 0,
      dlq_job_count: dlqJobCount
    });
  }
  if (url.startsWith("/api/v1/accounts")) {
    return jsonResponse({ accounts: [] });
  }
  return null;
}

describe("Overview Accounts, Transaction Drilldown, and Workbench Jobs", () => {
  let handle: RenderHandle | null = null;

  // Unmount the React root and restore fetch spies after each test so DOM state stays isolated.
  afterEach(async () => {
    if (handle) {
      await handle.unmount();
      handle = null;
    }
    vi.restoreAllMocks();
  });

  it("displays Financial Account balances and transaction drilldown on Overview", async () => {
    // Stub persona, portfolio summary, account listing, and transaction drilldown endpoints.
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = typeof input === "string" ? input : input.toString();

      if (url === "/api/v1/personas") {
        return jsonResponse({
          personas: [
            { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma", linked_identity_count: 2 }
          ]
        });
      }
      if (url === "/api/v1/portfolio/summary?persona=aarav") {
        return jsonResponse({
          persona: { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma" },
          as_of: "2026-06-15T14:20:10Z",
          net_worth: "48500.0000",
          total_assets: "48500.0000",
          total_liabilities: "0.0000",
          portfolio_xirr: null,
          accounts_summary: {
            bank_deposits: "48500.0000",
            mutual_funds: "0.0000",
            equities: "0.0000",
            loans: "0.0000",
            credit_cards: "0.0000"
          },
          open_alert_count: 0,
          uncategorized_count: 0,
          dlq_job_count: 1
        });
      }
      if (url === "/api/v1/accounts?persona=aarav") {
        return jsonResponse({
          accounts: [
            {
              id: "acct-1",
              canonical_user_id: "u1",
              source_type: "DEPOSIT",
              natural_key: "HDFC-FIP|HDFC-4020",
              institution_name: "HDFC Bank",
              fip_id: "HDFC-FIP",
              linked_acc_ref: "HDFC-4020",
              masked_acc_number: "XXXXXXXX4020",
              currency: "INR",
              ledger_balance: "48500.0000",
              reported_balance: "48500.0000",
              open_alert_count: 0
            }
          ]
        });
      }
      if (url.startsWith("/api/v1/accounts/acct-1/transactions")) {
        return jsonResponse({
          account_id: "acct-1",
          total: 1,
          transactions: [
            {
              id: "txn-1",
              account_id: "acct-1",
              source: "AA_DEPOSIT",
              txn_type: "CREDIT",
              category: null,
              txn_timestamp: "2026-06-15T09:00:00Z",
              narration: "NEFT/ACME CORP/SALARY",
              reference: "UTR123",
              idempotency_key: "k1",
              entries: [
                { id: "e1", coa_account: "ASSET:BANK", amount: "50000.0000", units: null },
                { id: "e2", coa_account: "EQUITY:EXTERNAL_CLEARING", amount: "-50000.0000", units: null }
              ]
            }
          ]
        });
      }

      return jsonResponse({ error: { code: "ERR_NOT_FOUND", message: "not found" } }, 404);
    });

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    // Wait for initial Overview load to render the HDFC account row and formatted INR balance.
    await waitForCondition(() => {
      expect(root.textContent).toContain("HDFC Bank");
      expect(root.textContent).toContain("XXXXXXXX4020");
      expect(root.textContent).toContain("48,500.00");
    });

    // Trigger the inline transaction drilldown and assert both double-entry legs appear.
    const inspectBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Inspect Transactions")
    );
    expect(inspectBtn).toBeDefined();

    await act(async () => {
      inspectBtn!.click();
    });

    await waitForCondition(() => {
      expect(root.textContent).toContain("NEFT/ACME CORP/SALARY");
      expect(root.textContent).toContain("ASSET:BANK");
      expect(root.textContent).toContain("EQUITY:EXTERNAL_CLEARING");
    });
  });

  it("displays the ingestion_jobs queue and error codes in Reconciliation Workbench", async () => {
    // Stub persona, Overview background fetches, and a persona-scoped /api/v1/jobs response
    // containing both a COMPLETED job and a DEAD_LETTER job.
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = typeof input === "string" ? input : input.toString();

      if (url === "/api/v1/personas") {
        return jsonResponse({
          personas: [
            { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma", linked_identity_count: 2 }
          ]
        });
      }
      if (url.startsWith("/api/v1/portfolio/summary")) {
        return jsonResponse({
          persona: { id: "u1", persona_slug: "aarav", display_name: "Aarav Sharma" },
          as_of: "2026-06-15T14:20:10Z",
          net_worth: "0.0000",
          total_assets: "0.0000",
          total_liabilities: "0.0000",
          portfolio_xirr: null,
          accounts_summary: {
            bank_deposits: "0.0000",
            mutual_funds: "0.0000",
            equities: "0.0000",
            loans: "0.0000",
            credit_cards: "0.0000"
          },
          open_alert_count: 0,
          uncategorized_count: 0,
          dlq_job_count: 1
        });
      }
      if (url.startsWith("/api/v1/accounts")) {
        return jsonResponse({ accounts: [] });
      }
      if (url === "/api/v1/jobs?persona=aarav") {
        return jsonResponse({
          total: 2,
          jobs: [
            {
              id: "job-1",
              canonical_user_id: "u1",
              source_type: "AA_DEPOSIT",
              idempotency_key: "req-1",
              status: "COMPLETED",
              attempt_count: 0,
              max_attempts: 3,
              replay_count: 0,
              last_error_code: null,
              last_error_message: null,
              created_at: "2026-06-15T09:00:00Z",
              updated_at: "2026-06-15T09:00:01Z",
              completed_at: "2026-06-15T09:00:01Z"
            },
            {
              id: "job-2",
              canonical_user_id: "u1",
              source_type: "AA_DEPOSIT",
              idempotency_key: null,
              status: "DEAD_LETTER",
              attempt_count: 0,
              max_attempts: 3,
              replay_count: 0,
              last_error_code: "ERR_SCHEMA_VALIDATION",
              last_error_message: "missing linkedAccRef",
              created_at: "2026-06-15T09:05:00Z",
              updated_at: "2026-06-15T09:05:01Z",
              completed_at: null
            }
          ]
        });
      }

      return jsonResponse({ error: { code: "ERR_NOT_FOUND", message: "not found" } }, 404);
    });

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    // Switch to the Reconciliation Workbench tab and assert jobs and DLQ codes load for Aarav.
    const workbenchBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Reconciliation Workbench")
    );
    expect(workbenchBtn).toBeDefined();

    await act(async () => {
      workbenchBtn!.click();
    });

    await waitForCondition(() => {
      expect(fetchSpy).toHaveBeenCalledWith("/api/v1/jobs?persona=aarav");
      expect(root.textContent).toContain("COMPLETED");
      expect(root.textContent).toContain("DEAD_LETTER");
      expect(root.textContent).toContain("ERR_SCHEMA_VALIDATION");
    });
  });

  it("inspects, edits JSON payload, and replays a DEAD_LETTER job in Reconciliation Workbench", async () => {
    let replayed = false;

    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = typeof input === "string" ? input : input.toString();
      const shell = mockWorkbenchShellRequests(url, replayed ? 0 : 1);
      if (shell) return shell;

      if (url === "/api/v1/jobs?persona=aarav") {
        return jsonResponse({
          total: 1,
          jobs: [
            replayed
              ? {
                  id: "job-dlq-1",
                  canonical_user_id: "u1",
                  source_type: "AA_DEPOSIT",
                  idempotency_key: null,
                  status: "COMPLETED",
                  attempt_count: 0,
                  max_attempts: 3,
                  replay_count: 1,
                  replayed_at: "2026-06-15T09:10:00Z",
                  last_error_code: null,
                  last_error_message: null,
                  created_at: "2026-06-15T09:05:00Z",
                  updated_at: "2026-06-15T09:10:01Z",
                  completed_at: "2026-06-15T09:10:01Z"
                }
              : {
                  id: "job-dlq-1",
                  canonical_user_id: "u1",
                  source_type: "AA_DEPOSIT",
                  idempotency_key: null,
                  status: "DEAD_LETTER",
                  attempt_count: 2,
                  max_attempts: 3,
                  replay_count: 0,
                  replayed_at: null,
                  last_error_code: "ERR_SCHEMA_VALIDATION",
                  last_error_message: "Account.linkedAccRef is required",
                  created_at: "2026-06-15T09:05:00Z",
                  updated_at: "2026-06-15T09:05:01Z",
                  completed_at: null
                }
          ]
        });
      }
      if (url === "/api/v1/jobs/job-dlq-1?persona=aarav" && (!init?.method || init.method === "GET")) {
        return jsonResponse({
          job: {
            id: "job-dlq-1",
            canonical_user_id: "u1",
            source_type: "AA_DEPOSIT",
            idempotency_key: null,
            status: "DEAD_LETTER",
            attempt_count: 2,
            max_attempts: 3,
            replay_count: 0,
            last_error_code: "ERR_SCHEMA_VALIDATION",
            last_error_message: "Account.linkedAccRef is required",
            created_at: "2026-06-15T09:05:00Z",
            updated_at: "2026-06-15T09:05:01Z",
            completed_at: null,
            payload: { fipId: "HDFC-FIP", Account: { type: "deposit" } }
          }
        });
      }
      if (url === "/api/v1/jobs/job-dlq-1/replay?persona=aarav" && init?.method === "POST") {
        replayed = true;
        return jsonResponse(
          {
            job: {
              id: "job-dlq-1",
              canonical_user_id: "u1",
              source_type: "AA_DEPOSIT",
              idempotency_key: null,
              status: "PENDING",
              attempt_count: 0,
              max_attempts: 3,
              replay_count: 1,
              replayed_at: "2026-06-15T09:10:00Z",
              last_error_code: null,
              last_error_message: null,
              created_at: "2026-06-15T09:05:00Z",
              updated_at: "2026-06-15T09:10:00Z",
              completed_at: null,
              payload: { fipId: "HDFC-FIP", Account: { type: "deposit", linkedAccRef: "HDFC-4020" } }
            }
          },
          202
        );
      }

      return jsonResponse({ error: { code: "ERR_NOT_FOUND", message: "not found" } }, 404);
    });

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    const workbenchBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Reconciliation Workbench")
    );
    await act(async () => {
      workbenchBtn!.click();
    });

    await waitForCondition(() => {
      expect(root.textContent).toContain("2/3");
      expect(root.textContent).toContain("Account.linkedAccRef is required");
    });

    const inspectBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Inspect / Replay")
    );
    expect(inspectBtn).toBeDefined();

    await act(async () => {
      inspectBtn!.click();
    });

    let textarea: HTMLTextAreaElement | null = null;
    await waitForCondition(() => {
      textarea = root.querySelector<HTMLTextAreaElement>('textarea[aria-label="DLQ JSON Payload"]');
      expect(textarea).not.toBeNull();
      expect(textarea!.value).toContain("HDFC-FIP");
    });

    const repaired = JSON.stringify({ fipId: "HDFC-FIP", Account: { type: "deposit", linkedAccRef: "HDFC-4020" } }, null, 2);
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
      setter.call(textarea, repaired);
      textarea!.dispatchEvent(new Event("input", { bubbles: true }));
    });

    const replayBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Replay Job")
    );
    expect(replayBtn).toBeDefined();

    await act(async () => {
      replayBtn!.click();
    });

    await waitForCondition(() => {
      const postCall = fetchSpy.mock.calls.find(
        ([u, init]) => u === "/api/v1/jobs/job-dlq-1/replay?persona=aarav" && init?.method === "POST"
      );
      expect(postCall).toBeDefined();
      expect(JSON.parse(String(postCall![1]?.body))).toEqual({
        payload: { fipId: "HDFC-FIP", Account: { type: "deposit", linkedAccRef: "HDFC-4020" } }
      });
      expect(root.textContent).toContain("COMPLETED");
    });
  });

  it("displays client JSON validation and backend replay errors in the Workbench error banner", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = typeof input === "string" ? input : input.toString();
      const shell = mockWorkbenchShellRequests(url, 1);
      if (shell) return shell;

      if (url === "/api/v1/jobs?persona=aarav") {
        return jsonResponse({
          total: 1,
          jobs: [
            {
              id: "job-err-1",
              canonical_user_id: "u1",
              source_type: "AA_DEPOSIT",
              idempotency_key: null,
              status: "DEAD_LETTER",
              attempt_count: 3,
              max_attempts: 3,
              replay_count: 0,
              last_error_code: "ERR_TRANSIENT_FAULT",
              last_error_message: "upstream timeout",
              created_at: "2026-06-15T09:05:00Z",
              updated_at: "2026-06-15T09:05:01Z",
              completed_at: null
            }
          ]
        });
      }
      if (url === "/api/v1/jobs/job-err-1?persona=aarav") {
        return jsonResponse({
          job: {
            id: "job-err-1",
            canonical_user_id: "u1",
            source_type: "AA_DEPOSIT",
            idempotency_key: null,
            status: "DEAD_LETTER",
            attempt_count: 3,
            max_attempts: 3,
            replay_count: 0,
            last_error_code: "ERR_TRANSIENT_FAULT",
            last_error_message: "upstream timeout",
            created_at: "2026-06-15T09:05:00Z",
            updated_at: "2026-06-15T09:05:01Z",
            completed_at: null,
            payload: { fipId: "HDFC-FIP" }
          }
        });
      }
      if (url === "/api/v1/jobs/job-err-1/replay?persona=aarav" && init?.method === "POST") {
        return jsonResponse(
          { error: { code: "ERR_JOB_NOT_REPLAYABLE", message: "only DEAD_LETTER jobs can be replayed" } },
          409
        );
      }

      return jsonResponse({ error: { code: "ERR_NOT_FOUND", message: "not found" } }, 404);
    });

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    const workbenchBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Reconciliation Workbench")
    );
    await act(async () => {
      workbenchBtn!.click();
    });

    await waitForCondition(() => {
      expect(root.textContent).toContain("Inspect / Replay");
    });

    const inspectBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Inspect / Replay")
    );
    await act(async () => {
      inspectBtn!.click();
    });

    let textarea: HTMLTextAreaElement | null = null;
    await waitForCondition(() => {
      textarea = root.querySelector<HTMLTextAreaElement>('textarea[aria-label="DLQ JSON Payload"]');
      expect(textarea).not.toBeNull();
    });

    // Case 1: Invalid JSON syntax shows error in role="alert" banner.
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
      setter.call(textarea, "{broken json");
      textarea!.dispatchEvent(new Event("input", { bubbles: true }));
    });

    const replayBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Replay Job")
    );
    await act(async () => {
      replayBtn!.click();
    });

    await waitForCondition(() => {
      const alert = root.querySelector('[role="alert"]');
      expect(alert).not.toBeNull();
      expect(alert!.textContent).toMatch(/invalid json/i);
    });

    // Case 2: Valid JSON object hits 409 API error and surfaces ERR_JOB_NOT_REPLAYABLE in banner.
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
      setter.call(textarea, '{"fipId":"HDFC-FIP"}');
      textarea!.dispatchEvent(new Event("input", { bubbles: true }));
    });

    await act(async () => {
      replayBtn!.click();
    });

    await waitForCondition(() => {
      const alert = root.querySelector('[role="alert"]');
      expect(alert).not.toBeNull();
      expect(alert!.textContent).toContain("ERR_JOB_NOT_REPLAYABLE: only DEAD_LETTER jobs can be replayed");
    });
  });
});
