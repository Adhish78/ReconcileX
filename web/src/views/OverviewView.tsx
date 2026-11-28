import { useEffect, useState } from "react";
import {
  fetchAccounts,
  fetchAccountTransactions,
  fetchPortfolioSummary,
  formatINR
} from "../api";
import type {
  AccountSummary,
  LedgerTransactionItem,
  PersonaViewProps,
  PortfolioSummary
} from "../types";

/**
 * Primary "Overview" tab panel rendered inside App.
 *
 * Displays the active Canonical User's portfolio summary metrics, Financial Accounts
 * with live aggregated ledger balances, and an inline Ledger Transaction + Ledger Entry drilldown.
 */
export function OverviewView({ persona }: PersonaViewProps) {
  const [summary, setSummary] = useState<PortfolioSummary | null>(null);
  const [accounts, setAccounts] = useState<AccountSummary[]>([]);
  const [selectedAccountId, setSelectedAccountId] = useState<string | null>(null);
  const [transactions, setTransactions] = useState<LedgerTransactionItem[]>([]);
  const [loading, setLoading] = useState<boolean>(false);
  const [txnLoading, setTxnLoading] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!persona) {
      setSummary(null);
      setAccounts([]);
      setSelectedAccountId(null);
      setTransactions([]);
      return;
    }

    let cancelled = false;
    setLoading(true);
    setSelectedAccountId(null);
    setTransactions([]);

    Promise.all([
      fetchPortfolioSummary(persona.persona_slug),
      fetchAccounts(persona.persona_slug)
    ])
      .then(([sumData, acctList]) => {
        if (cancelled) return;
        setSummary(sumData);
        setAccounts(acctList);
        setError(null);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load overview data");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });

    return () => {
      cancelled = true;
    };
  }, [persona]);

  async function handleInspectTransactions(accountId: string) {
    setSelectedAccountId(accountId);
    setTxnLoading(true);
    setError(null);

    try {
      const txns = await fetchAccountTransactions(accountId, persona?.persona_slug);
      setTransactions(txns);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to load account transactions");
      setTransactions([]);
    } finally {
      setTxnLoading(false);
    }
  }

  const selectedAccount = accounts.find((a) => a.id === selectedAccountId) ?? null;

  return (
    <section className="card">
      <h2>Overview</h2>

      <p className="muted">
        {persona
          ? `Active Canonical User: ${persona.display_name} (${persona.persona_slug}) — ${persona.linked_identity_count} linked identities.`
          : "No active Canonical Users seeded yet."}
      </p>

      {error && (
        <div role="alert" className="error-banner">
          {error}
        </div>
      )}

      {summary && (
        <div className="metrics-grid section-gap">
          <div className="metric-box">
            <span className="metric-label">Net Worth</span>
            <span className="metric-value">{formatINR(summary.net_worth)}</span>
          </div>
          <div className="metric-box">
            <span className="metric-label">Bank Deposits</span>
            <span className="metric-value">{formatINR(summary.accounts_summary.bank_deposits)}</span>
          </div>
          <div className="metric-box">
            <span className="metric-label">Total Liabilities</span>
            <span className="metric-value">{formatINR(summary.total_liabilities)}</span>
          </div>
          <div className="metric-box">
            <span className="metric-label">DLQ Jobs</span>
            <span className="metric-value">{summary.dlq_job_count}</span>
          </div>
        </div>
      )}

      {persona && (
        <div className="section-gap">
          <h3>Financial Accounts</h3>
          {loading ? (
            <p className="muted">Loading accounts...</p>
          ) : accounts.length === 0 ? (
            <p className="muted">No Financial Accounts ingested for {persona.display_name} yet.</p>
          ) : (
            <div className="table-wrap">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Institution</th>
                    <th>Type</th>
                    <th>Account</th>
                    <th>Ledger Balance</th>
                    <th>Reported Balance</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {accounts.map((acct) => (
                    <tr key={acct.id} className={acct.id === selectedAccountId ? "is-selected" : ""}>
                      <td>{acct.institution_name}</td>
                      <td>{acct.source_type}</td>
                      <td className="mono">{acct.masked_acc_number || acct.linked_acc_ref}</td>
                      <td className="mono">{formatINR(acct.ledger_balance)}</td>
                      <td className="mono">
                        {acct.reported_balance ? formatINR(acct.reported_balance) : "—"}
                      </td>
                      <td>
                        <button
                          type="button"
                          className="btn-secondary"
                          onClick={() => void handleInspectTransactions(acct.id)}
                        >
                          Inspect Transactions
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}

      {selectedAccount && (
        <div className="drilldown-panel section-gap">
          <h3>
            Ledger Transactions — {selectedAccount.institution_name} (
            {selectedAccount.masked_acc_number || selectedAccount.linked_acc_ref})
          </h3>

          {txnLoading ? (
            <p className="muted">Loading transactions...</p>
          ) : transactions.length === 0 ? (
            <p className="muted">No ledger transactions posted on this account.</p>
          ) : (
            <div className="table-wrap">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Timestamp</th>
                    <th>Type</th>
                    <th>Narration</th>
                    <th>Double-Entry Legs</th>
                  </tr>
                </thead>
                <tbody>
                  {transactions.map((txn) => (
                    <tr key={txn.id}>
                      <td className="mono">{new Date(txn.txn_timestamp).toLocaleString()}</td>
                      <td>{txn.txn_type}</td>
                      <td>{txn.narration || "—"}</td>
                      <td>
                        <div className="entries-list">
                          {txn.entries.map((entry) => (
                            <span key={entry.id} className="entry-pill mono">
                              {entry.coa_account}: {formatINR(entry.amount)}
                            </span>
                          ))}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
