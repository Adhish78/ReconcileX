import { useCallback, useEffect, useState } from "react";
import { fetchJobs } from "../api";
import type { JobSummary, PersonaViewProps } from "../types";

// Valid job_status enum values plus "" for unfiltered queue inspection.
const JOB_STATUS_FILTERS = ["", "PENDING", "PROCESSING", "RETRY_WAIT", "COMPLETED", "DEAD_LETTER"];

/**
 * "Reconciliation Workbench" tab panel rendered inside App.
 *
 * Displays the asynchronous ingestion_jobs SKIP LOCKED queue with status filtering,
 * manual refresh, and dead-letter failure diagnostics.
 */
export function ReconciliationView({ persona }: PersonaViewProps) {
  const [jobs, setJobs] = useState<JobSummary[]>([]);
  const [statusFilter, setStatusFilter] = useState<string>("");
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  // Empty slug queries all jobs when no Persona is selected; otherwise scopes to the active Persona.
  const personaSlug = persona?.persona_slug ?? "";

  // Memoized loader shared by the reactive effect and the manual "Refresh Queue" button.
  const loadJobs = useCallback(async (slug: string, status: string) => {
    setLoading(true);
    try {
      const list = await fetchJobs(slug, status);
      setJobs(list);
      setError(null);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to load ingestion jobs");
    } finally {
      setLoading(false);
    }
  }, []);

  // Refresh the queue automatically whenever the active Persona or status filter changes.
  useEffect(() => {
    void loadJobs(personaSlug, statusFilter);
  }, [loadJobs, personaSlug, statusFilter]);

  return (
    <section className="card">
      {/* Header with status filter dropdown and on-demand queue refresh button */}
      <div className="section-header">
        <div>
          <h2>Reconciliation Workbench</h2>
          <p className="muted">
            {persona
              ? `Ingestion queue, Reconciliation Alerts, and Entity Merge audit log for ${persona.display_name}.`
              : "Select an active Canonical User to inspect reconciliation status."}
          </p>
        </div>

        <div className="toolbar">
          <select
            aria-label="Filter job status"
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
            className="persona-select"
          >
            {JOB_STATUS_FILTERS.map((s) => (
              <option key={s || "ALL"} value={s}>
                {s || "All Statuses"}
              </option>
            ))}
          </select>

          <button
            type="button"
            className="btn-secondary"
            onClick={() => void loadJobs(personaSlug, statusFilter)}
          >
            Refresh Queue
          </button>
        </div>
      </div>

      {error && (
        <div role="alert" className="error-banner">
          {error}
        </div>
      )}

      {/* Ingestion queue table highlighting job status badges and DLQ error diagnostics */}
      <h3>Ingestion Jobs Queue</h3>
      {loading ? (
        <p className="muted">Loading jobs...</p>
      ) : jobs.length === 0 ? (
        <p className="muted">No ingestion jobs recorded yet.</p>
      ) : (
        <div className="table-wrap">
          <table className="data-table">
            <thead>
              <tr>
                <th>Job ID</th>
                <th>Source</th>
                <th>Status</th>
                <th>Attempts</th>
                <th>Error Code</th>
                <th>Details</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((job) => (
                <tr key={job.id}>
                  <td className="mono">{job.id}</td>
                  <td>{job.source_type}</td>
                  <td>
                    <span className={`status-badge status-${job.status.toLowerCase()}`}>
                      {job.status}
                    </span>
                  </td>
                  <td className="mono">
                    {job.attempt_count}/{job.max_attempts}
                  </td>
                  <td className="mono">{job.last_error_code || "—"}</td>
                  <td>{job.last_error_message || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
