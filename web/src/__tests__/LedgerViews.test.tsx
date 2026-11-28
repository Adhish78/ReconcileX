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

  it("displays mutual fund Folio holdings (Cost Basis and unit totals) on Overview", async () => {
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
          net_worth: "13700.0000",
          total_assets: "13700.0000",
          total_liabilities: "0.0000",
          portfolio_xirr: null,
          accounts_summary: {
            bank_deposits: "0.0000",
            mutual_funds: "13700.0000",
            equities: "0.0000",
            loans: "0.0000",
            credit_cards: "0.0000"
          },
          open_alert_count: 0,
          uncategorized_count: 0,
          dlq_job_count: 0
        });
      }
      if (url === "/api/v1/accounts?persona=aarav") {
        return jsonResponse({
          accounts: [
            {
              id: "mf-1",
              canonical_user_id: "u1",
              source_type: "MUTUAL_FUND",
              natural_key: "CAMS|12345678 / 90|INF879O01027",
              institution_name: "PPFAS Mutual Fund",
              fip_id: "",
              linked_acc_ref: "",
              masked_acc_number: "",
              folio_number: "12345678 / 90",
              isin: "INF879O01027",
              scheme_code: 122639,
              scheme_name: "Parag Parikh Flexi Cap Fund - Direct Plan - Growth",
              currency: "INR",
              ledger_balance: "13700.0000",
              ledger_units: "165.000000",
              reported_balance: "14850.0000",
              reported_units: "165.000000",
              open_alert_count: 0
            }
          ]
        });
      }
      if (url.startsWith("/api/v1/accounts/mf-1/transactions")) {
        return jsonResponse({
          account_id: "mf-1",
          total: 1,
          transactions: [
            {
              id: "txn-mf-1",
              account_id: "mf-1",
              source: "CAS_CAMS",
              txn_type: "PURCHASE_SIP",
              category: null,
              txn_timestamp: "2024-05-05T00:00:00+05:30",
              narration: "SIP Purchase - Inst 1",
              reference: null,
              idempotency_key: "k-mf-1",
              entries: [
                { id: "em1", coa_account: "ASSET:MF_COST", amount: "13700.0000", units: "165.000000" },
                { id: "em2", coa_account: "EQUITY:EXTERNAL_CLEARING", amount: "-13700.0000", units: null }
              ]
            }
          ]
        });
      }

      return jsonResponse({ error: { code: "ERR_NOT_FOUND", message: "not found" } }, 404);
    });

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    await waitForCondition(() => {
      expect(root.textContent).toContain("Mutual Fund Folio Holdings");
      expect(root.textContent).toContain("Parag Parikh Flexi Cap Fund - Direct Plan - Growth");
      expect(root.textContent).toContain("12345678 / 90");
      expect(root.textContent).toContain("INF879O01027");
      expect(root.textContent).toContain("13,700.00");
      expect(root.textContent).toContain("165.0000");
    });

    const inspectBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Inspect Transactions")
    );
    expect(inspectBtn).toBeDefined();

    await act(async () => {
      inspectBtn!.click();
    });

    await waitForCondition(() => {
      expect(root.textContent).toContain("SIP Purchase - Inst 1");
      expect(root.textContent).toContain("ASSET:MF_COST");
      expect(root.textContent).toContain("165.0000 units");
    });
  });

  it("repairs an ERR_UNIT_CHECKSUM CAS job in Reconciliation Workbench and replays it", async () => {
    let replayed = false;
    const brokenCAS = {
      file_type: "CAMS",
      cas_type: "DETAILED",
      folios: [{ folio: "F1", schemes: [{ isin: "INF879O01027", open: "0.000", close: "99.000" }] }]
    };

    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
      const url = typeof input === "string" ? input : input.toString();
      const shell = mockWorkbenchShellRequests(url, replayed ? 0 : 1);
      if (shell) return shell;

      if (url === "/api/v1/jobs?persona=aarav") {
        return jsonResponse({
          total: 1,
          jobs: [
            {
              id: "job-cas-dlq",
              canonical_user_id: "u1",
              source_type: "CAS",
              idempotency_key: null,
              status: replayed ? "COMPLETED" : "DEAD_LETTER",
              attempt_count: 0,
              max_attempts: 3,
              replay_count: replayed ? 1 : 0,
              last_error_code: replayed ? null : "ERR_UNIT_CHECKSUM",
              last_error_message: replayed ? null : "unit conservation failed",
              created_at: "2026-06-15T09:05:00Z",
              updated_at: "2026-06-15T09:05:01Z",
              completed_at: replayed ? "2026-06-15T09:10:01Z" : null
            }
          ]
        });
      }
      if (url === "/api/v1/jobs/job-cas-dlq?persona=aarav" && (!init?.method || init.method === "GET")) {
        return jsonResponse({
          job: {
            id: "job-cas-dlq",
            canonical_user_id: "u1",
            source_type: "CAS",
            idempotency_key: null,
            status: "DEAD_LETTER",
            attempt_count: 0,
            max_attempts: 3,
            replay_count: 0,
            last_error_code: "ERR_UNIT_CHECKSUM",
            last_error_message: "unit conservation failed",
            created_at: "2026-06-15T09:05:00Z",
            updated_at: "2026-06-15T09:05:01Z",
            completed_at: null,
            payload: brokenCAS
          }
        });
      }
      if (url === "/api/v1/jobs/job-cas-dlq/replay?persona=aarav" && init?.method === "POST") {
        replayed = true;
        return jsonResponse(
          {
            job: {
              id: "job-cas-dlq",
              canonical_user_id: "u1",
              source_type: "CAS",
              idempotency_key: null,
              status: "PENDING",
              attempt_count: 0,
              max_attempts: 3,
              replay_count: 1,
              last_error_code: null,
              last_error_message: null,
              created_at: "2026-06-15T09:05:00Z",
              updated_at: "2026-06-15T09:10:00Z",
              completed_at: null,
              payload: brokenCAS
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
      expect(root.textContent).toContain("ERR_UNIT_CHECKSUM");
      expect(root.textContent).toContain("CAS");
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
      expect(textarea!.value).toContain("99.000");
    });

    const repairedCAS = JSON.stringify({
      file_type: "CAMS",
      cas_type: "DETAILED",
      folios: [{ folio: "F1", schemes: [{ isin: "INF879O01027", open: "0.000", close: "50.000" }] }]
    });
    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")!.set!;
      setter.call(textarea, repairedCAS);
      textarea!.dispatchEvent(new Event("input", { bubbles: true }));
    });

    const replayBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Replay Job")
    );
    await act(async () => {
      replayBtn!.click();
    });

    await waitForCondition(() => {
      const postCall = fetchSpy.mock.calls.find(
        ([u, init]) => u === "/api/v1/jobs/job-cas-dlq/replay?persona=aarav" && init?.method === "POST"
      );
      expect(postCall).toBeDefined();
      expect(root.textContent).toContain("COMPLETED");
    });
  });

  it("displays demat equity holdings (Cost Basis and units per ISIN) and Corporate Action drilldown on Overview", async () => {
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
          net_worth: "88000.0000",
          total_assets: "88000.0000",
          total_liabilities: "0.0000",
          portfolio_xirr: null,
          accounts_summary: {
            bank_deposits: "0.0000",
            mutual_funds: "0.0000",
            equities: "88000.0000",
            loans: "0.0000",
            credit_cards: "0.0000"
          },
          open_alert_count: 0,
          uncategorized_count: 0,
          dlq_job_count: 0
        });
      }
      if (url === "/api/v1/accounts?persona=aarav") {
        return jsonResponse({
          accounts: [
            {
              id: "eq-1",
              canonical_user_id: "u1",
              source_type: "EQUITY",
              natural_key: "CDSL-FIP|DEMAT-001",
              institution_name: "Zerodha Broking (CDSL)",
              fip_id: "CDSL-FIP",
              linked_acc_ref: "DEMAT-001",
              masked_acc_number: "XXXXXXXXXXXX0987",
              currency: "INR",
              ledger_balance: "88000.0000",
              ledger_units: "80.000000",
              reported_balance: "248000.0000",
              reported_units: "80.000000",
              open_alert_count: 0,
              equity_holdings: [
                {
                  isin: "INE002A01018",
                  issuer_name: "RELIANCE INDUSTRIES LTD",
                  isin_description: "RELIANCE INDUSTRIES LTD EQ",
                  cost_basis: "50000.0000",
                  ledger_units: "60.000000",
                  reported_units: "60.000000",
                  last_traded_price: "2800.0000"
                },
                {
                  isin: "INE467B01029",
                  issuer_name: "TATA CONSULTANCY SERVICES LTD",
                  isin_description: "TCS LTD EQ",
                  cost_basis: "38000.0000",
                  ledger_units: "20.000000",
                  reported_units: "20.000000",
                  last_traded_price: "4000.0000"
                }
              ]
            }
          ]
        });
      }
      if (url.startsWith("/api/v1/accounts/eq-1/transactions")) {
        return jsonResponse({
          account_id: "eq-1",
          total: 2,
          transactions: [
            {
              id: "txn-eq-bonus",
              account_id: "eq-1",
              source: "AA_EQUITIES",
              txn_type: "BONUS",
              category: null,
              txn_timestamp: "2025-04-01T09:00:00+05:30",
              narration: "BONUS 1:2",
              reference: null,
              idempotency_key: "k-eq-bonus",
              entries: [
                { id: "ee1", coa_account: "ASSET:EQUITY_COST", amount: "0.0000", units: "20.000000" }
              ]
            },
            {
              id: "txn-eq-buy",
              account_id: "eq-1",
              source: "AA_EQUITIES",
              txn_type: "BUY",
              category: null,
              txn_timestamp: "2025-02-10T10:00:00+05:30",
              narration: "BUY",
              reference: "ORD-1",
              idempotency_key: "k-eq-buy",
              entries: [
                { id: "ee2", coa_account: "ASSET:EQUITY_COST", amount: "50000.0000", units: "20.000000" },
                { id: "ee3", coa_account: "EQUITY:EXTERNAL_CLEARING", amount: "-50000.0000", units: null }
              ]
            }
          ]
        });
      }

      return jsonResponse({ error: { code: "ERR_NOT_FOUND", message: "not found" } }, 404);
    });

    handle = await renderIntoDocument(<App />);
    const root = handle.container;

    await waitForCondition(() => {
      expect(root.textContent).toContain("Equities (Cost Basis)");
      expect(root.textContent).toContain("Demat Equity Holdings");
      expect(root.textContent).toContain("RELIANCE INDUSTRIES LTD");
      expect(root.textContent).toContain("INE002A01018");
      expect(root.textContent).toContain("50,000.00");
      expect(root.textContent).toContain("60.0000");
      expect(root.textContent).toContain("2,800.00");
      expect(root.textContent).toContain("TATA CONSULTANCY SERVICES LTD");
      expect(root.textContent).toContain("INE467B01029");
      expect(root.textContent).toContain("38,000.00");
      expect(root.textContent).toContain("20.0000");
    });

    const inspectBtn = Array.from(root.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("Inspect Transactions")
    );
    expect(inspectBtn).toBeDefined();

    await act(async () => {
      inspectBtn!.click();
    });

    await waitForCondition(() => {
      expect(root.textContent).toContain("BONUS 1:2");
      expect(root.textContent).toContain("ASSET:EQUITY_COST");
      expect(root.textContent).toContain("20.0000 units");
    });
  });
});
