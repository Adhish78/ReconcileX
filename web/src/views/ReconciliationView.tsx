import { useCallback, useEffect, useState } from "react";
import { fetchJobDetail, fetchJobs, replayJob } from "../api";
import type { JobDetail, JobSummary, PersonaViewProps } from "../types";

// Valid job_status enum values plus "" for unfiltered queue inspection.
const JOB_STATUS_FILTERS = ["", "PENDING", "PROCESSING", "RETRY_WAIT", "COMPLETED", "DEAD_LETTER"];

// Poll every 1.5s while any job is actively progressing through the worker queue.
const IN_FLIGHT_POLL_MS = 1500;

function isInFlightStatus(status: string): boolean {
  return status === "PENDING" || status === "PROCESSING" || status === "RETRY_WAIT";
}

interface DLQEditorState {
  job: JobDetail;
  payloadText: string;
}

/**
 * "Reconciliation Workbench" tab panel rendered inside App.
 *
 * Displays the asynchronous ingestion_jobs SKIP LOCKED queue with status filtering,
 * in-flight polling, dead-letter failure diagnostics, and an interactive JSON
 * payload editor + replay workflow for DEAD_LETTER jobs.
 */
export function ReconciliationView({ persona }: PersonaViewProps) {
  const [jobs, setJobs] = useState<JobSummary[]>([]);
  const [statusFilter, setStatusFilter] = useState<string>("");
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  const [dlqEditor, setDlqEditor] = useState<DLQEditorState | null>(null);
  const [detailLoading, setDetailLoading] = useState<boolean>(false);
  const [replaying, setReplaying] = useState<boolean>(false);

  // Empty slug queries all jobs when no Persona is selected; otherwise scopes to the active Persona.
  const personaSlug = persona?.persona_slug ?? "";

  // Memoized loader shared by the reactive effect, polling timer, and manual "Refresh Queue" button.
  const loadJobs = useCallback(async (slug: string, status: string, silent = false) => {
    if (!silent) {
      setLoading(true);
    }
    try {
      const list = await fetchJobs(slug, status);
      setJobs(list);
      // Preserve any active replay/validation error banner during background silent polls.
      if (!silent) {
        setError(null);
      }
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to load ingestion jobs");
    } finally {
      if (!silent) {
        setLoading(false);
      }
    }
  }, []);

  // Refresh the queue and clear any prior DLQ editor state when the active Persona or filter changes.
  useEffect(() => {
    setDlqEditor(null);
    void loadJobs(personaSlug, statusFilter);
  }, [loadJobs, personaSlug, statusFilter]);

  // Poll automatically every 1.5s while any job is PENDING, PROCESSING, or RETRY_WAIT.
  useEffect(() => {
    if (!jobs.some((j) => isInFlightStatus(j.status))) {
      return;
    }
    const timer = window.setTimeout(() => {
      void loadJobs(personaSlug, statusFilter, true);
    }, IN_FLIGHT_POLL_MS);

    return () => {
      window.clearTimeout(timer);
    };
  }, [jobs, loadJobs, personaSlug, statusFilter]);

  async function handleInspectDLQ(jobId: string) {
    setDetailLoading(true);
    setError(null);
    try {
      const detail = await fetchJobDetail(jobId, personaSlug || undefined);
      setDlqEditor({
        job: detail,
        payloadText: JSON.stringify(detail.payload ?? {}, null, 2)
      });
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to load job payload");
    } finally {
      setDetailLoading(false);
    }
  }

  async function handleReplay() {
    if (!dlqEditor) return;

    let parsed: unknown;
    try {
      parsed = JSON.parse(dlqEditor.payloadText);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "syntax error";
      setError(`Invalid JSON payload: ${msg}`);
      return;
    }

    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      setError("Invalid JSON payload: root value must be a JSON object");
      return;
    }

    setReplaying(true);
    setError(null);
    try {
      await replayJob(dlqEditor.job.id, parsed as Record<string, unknown>, personaSlug || undefined);
      setDlqEditor(null);
      await loadJobs(personaSlug, statusFilter);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to replay job");
    } finally {
      setReplaying(false);
    }
  }

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
                <th>Actions</th>
              </tr>
            </thead>
            <tbody>
              {jobs.map((job) => (
                <tr key={job.id} className={dlqEditor?.job.id === job.id ? "is-selected" : ""}>
                  <td className="mono">{job.id}</td>
                  <td>{job.source_type}</td>
                  <td>
                    <span className={`status-badge status-${job.status.toLowerCase()}`}>
                      {job.status}
                    </span>
                  </td>
                  <td className="mono">
                    {job.attempt_count}/{job.max_attempts}
                    {job.replay_count > 0 ? ` (replayed ×${job.replay_count})` : ""}
                  </td>
                  <td className="mono">{job.last_error_code || "—"}</td>
                  <td>{job.last_error_message || "—"}</td>
                  <td>
                    {job.status === "DEAD_LETTER" ? (
                      <button
                        type="button"
                        className="btn-secondary"
                        disabled={detailLoading}
                        onClick={() => void handleInspectDLQ(job.id)}
                      >
                        Inspect / Replay
                      </button>
                    ) : (
                      "—"
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* DLQ JSON payload inspector & replay panel */}
      {dlqEditor && (
        <div className="drilldown-panel section-gap">
          <div className="section-header">
            <div>
              <h3>DLQ Payload Inspector — {dlqEditor.job.id}</h3>
              <p className="muted">
                {dlqEditor.job.last_error_code || "ERR_UNKNOWN"}:{" "}
                {dlqEditor.job.last_error_message || "No error message recorded"}
              </p>
            </div>
            <div className="toolbar">
              <button
                type="button"
                className="btn-primary"
                disabled={replaying}
                onClick={() => void handleReplay()}
              >
                {replaying ? "Replaying..." : "Replay Job"}
              </button>
              <button
                type="button"
                className="btn-secondary"
                disabled={replaying}
                onClick={() => setDlqEditor(null)}
              >
                Close
              </button>
            </div>
          </div>

          <textarea
            aria-label="DLQ JSON Payload"
            className="json-editor mono"
            rows={12}
            value={dlqEditor.payloadText}
            onChange={(e) =>
              setDlqEditor((prev) => (prev ? { ...prev, payloadText: e.target.value } : null))
            }
            spellCheck={false}
          />
        </div>
      )}
    </section>
  );
}
