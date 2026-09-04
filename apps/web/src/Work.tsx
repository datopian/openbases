import { useCallback, useEffect, useState } from "react";
import { api, type WorkItem, type QueueJob, type ProjectSummary } from "./api";

/**
 * Work: a brief goes in, beads come out, agents run them (WP-D2/E3, WP-F1).
 *
 * This is the page that shows the platform doing its actual job, and the shape
 * of it is decided by one constraint: the control plane cannot reach an
 * execution node. Nothing here happens synchronously. Planning and dispatch
 * QUEUE a job, the node claims it on its next pass, and the result appears
 * later.
 *
 * So the queue is not an implementation detail to hide behind a spinner — it is
 * the honest state of the system and it is shown. A button that appeared to hang
 * for four minutes while an agent thought would be a worse lie than a row that
 * says "queued".
 *
 * Costs are rendered from decimal strings and never parsed into a float. They
 * come from numeric(16,8) columns for a reason: a typical request costs $0.000088
 * and a month of them rounded to integer cents totals zero.
 */

const css = {
  card: {
    border: "1px solid #e3e3e3",
    borderRadius: "6px",
    padding: "1rem 1.25rem",
    marginBottom: "1.5rem",
    background: "#fcfcfc",
  } as const,
  muted: { color: "#666" } as const,
  small: { fontSize: "0.85rem" } as const,
  mono: {
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
  } as const,
  th: {
    textAlign: "left" as const,
    borderBottom: "2px solid #ddd",
    padding: "0.4rem 0.5rem",
    fontSize: "0.8rem",
    textTransform: "uppercase" as const,
    letterSpacing: "0.04em",
    color: "#555",
  },
  td: {
    borderBottom: "1px solid #eee",
    padding: "0.45rem 0.5rem",
    verticalAlign: "top" as const,
  },
  button: {
    padding: "0.5rem 0.9rem",
    borderRadius: "4px",
    border: "1px solid #2a6",
    background: "#2a6",
    color: "#fff",
    cursor: "pointer",
    fontSize: "0.9rem",
  } as const,
  ghost: {
    padding: "0.25rem 0.6rem",
    borderRadius: "4px",
    border: "1px solid #bbb",
    background: "#fff",
    cursor: "pointer",
    fontSize: "0.8rem",
  } as const,
};

/** Queue states, worded so a reader knows whether to keep waiting. */
function queueLabel(state: string): { text: string; colour: string } {
  switch (state) {
    case "queued":
      return { text: "queued", colour: "#96690a" };
    case "running":
      return { text: "agent running", colour: "#1a6" };
    case "done":
      return { text: "agent finished", colour: "#555" };
    case "failed":
      return { text: "agent failed", colour: "#b22" };
    default:
      return { text: "", colour: "#666" };
  }
}

/**
 * Cents as a person reads them, without going through a float.
 *
 * Trailing zeroes are trimmed rather than the number being rounded: 0.0088 cents
 * is a real request and rounding it to 0.01 — or worse, to 0 — is how a month of
 * spend came to total nothing before it was stored as numeric.
 */
function cents(raw: string): string {
  if (!raw) return "0";
  const parts = raw.split(".");
  const whole = parts[0] ?? "0";
  const frac = parts[1] ?? "";
  const trimmed = frac.replace(/0+$/, "");
  if (!trimmed) return whole;
  return `${whole}.${trimmed.slice(0, 4)}`;
}

function age(iso: string): string {
  if (!iso) return "";
  const secs = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (secs < 60) return `${Math.round(secs)}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}

export function Work() {
  const [items, setItems] = useState<WorkItem[] | null>(null);
  const [queue, setQueue] = useState<QueueJob[] | null>(null);
  const [brief, setBrief] = useState("");
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [onlyOpen, setOnlyOpen] = useState(true);
  const [projects, setProjects] = useState<ProjectSummary[]>([]);
  const [project, setProject] = useState("");

  const refresh = useCallback(async () => {
    try {
      const [w, q] = await Promise.all([api.work(), api.workQueue()]);
      setItems(w);
      setQueue(q);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  // Loaded once rather than on the five-second poll: the list of projects
  // somebody belongs to does not change while they are looking at this page,
  // and re-fetching it twelve times a minute to render an unchanged dropdown
  // is a request per user per five seconds for nothing.
  //
  // /v1/projects is already filtered to what the caller may see, so this
  // selector cannot offer a project they could not file into -- the check that
  // matters is still the one in system_enqueue_work, because a selector is a
  // convenience and not a boundary.
  useEffect(() => {
    api
      .projects()
      .then(setProjects)
      .catch(() => setProjects([]));
  }, []);

  useEffect(() => {
    refresh();
    // Polled rather than pushed. Everything here changes on the execution
    // node's schedule, not the browser's, and five seconds is well inside the
    // time an agent takes to do anything worth seeing.
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, [refresh]);

  const plan = async () => {
    if (!brief.trim() || busy) return;
    setBusy(true);
    setNote(null);
    try {
      const res = await api.plan(brief.trim(), project || undefined);
      setNote(
        `Planning job ${res.job.slice(0, 8)} queued${project ? ` for ${project}` : ""}. ` +
          `An agent will pick it up and file beads.`,
      );
      setBrief("");
      refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const dispatch = async (bead: string) => {
    setNote(null);
    try {
      const res = await api.dispatch(bead);
      setNote(`${bead} queued as job ${res.job.slice(0, 8)}.`);
      refresh();
    } catch (e) {
      // A budget refusal arrives here and is the most useful error this page
      // can show, so it is surfaced verbatim rather than as "request failed".
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const active = (queue ?? []).filter(
    (j) => j.status === "queued" || j.status === "running",
  );
  const recent = (queue ?? [])
    .filter((j) => j.status !== "queued" && j.status !== "running")
    .slice(0, 5);
  const shown = (items ?? []).filter((i) =>
    onlyOpen ? i.status !== "closed" : true,
  );

  return (
    <section>
      <h1 style={{ marginBottom: "0.25rem" }}>Work</h1>
      <p style={{ ...css.muted, ...css.small, marginTop: 0 }}>
        A brief becomes beads; beads become agent runs. Nothing here happens
        instantly — the control plane cannot reach an execution node, so a node
        claims each job on its next pass.
      </p>

      {error && (
        <div
          style={{ ...css.card, borderColor: "#e0b4b4", background: "#fff8f8" }}
        >
          <strong>Something went wrong.</strong>
          <div style={{ ...css.small, ...css.mono, marginTop: "0.35rem" }}>
            {error}
          </div>
        </div>
      )}

      <div style={css.card}>
        <label htmlFor="brief" style={{ fontWeight: 600 }}>
          Give an agent a brief
        </label>
        <p style={{ ...css.muted, ...css.small, marginTop: "0.25rem" }}>
          It files beads and does none of the work, so you can read the plan
          before any of it runs.
        </p>
        {/*
          Above the brief, not beside the button: the project decides who can
          read the beads this produces, so it belongs where somebody sees it
          before they type rather than next to the thing they press afterwards.
        */}
        <div style={{ marginBottom: "0.6rem" }}>
          <label
            htmlFor="project"
            style={{ ...css.small, ...css.muted, display: "block" }}
          >
            File the beads into
          </label>
          <select
            id="project"
            value={project}
            onChange={(e) => setProject(e.target.value)}
            style={{
              padding: "0.4rem",
              fontFamily: "inherit",
              fontSize: "0.9rem",
              borderRadius: "4px",
              border: "1px solid #ccc",
              minWidth: "18rem",
            }}
          >
            <option value="">No project — company-wide work</option>
            {projects.map((p) => (
              <option key={p.slug} value={p.slug}>
                {p.name} ({p.slug})
              </option>
            ))}
          </select>
          {project === "" && (
            <div style={{ ...css.small, ...css.muted, marginTop: "0.25rem" }}>
              Company-wide beads are readable by everyone who can log in. Pick a
              project for anything client-related.
            </div>
          )}
        </div>
        <textarea
          id="brief"
          value={brief}
          onChange={(e) => setBrief(e.target.value)}
          rows={4}
          placeholder="e.g. Add a CHANGELOG to the sandbox repository and describe how releases are cut."
          style={{
            width: "100%",
            padding: "0.6rem",
            fontFamily: "inherit",
            fontSize: "0.95rem",
            borderRadius: "4px",
            border: "1px solid #ccc",
          }}
        />
        <div
          style={{
            marginTop: "0.6rem",
            display: "flex",
            alignItems: "center",
            gap: "0.75rem",
          }}
        >
          <button
            style={{ ...css.button, opacity: busy || !brief.trim() ? 0.5 : 1 }}
            onClick={plan}
            disabled={busy || !brief.trim()}
          >
            {busy ? "Queueing…" : "Plan this"}
          </button>
          {note && <span style={{ ...css.small, color: "#1a6" }}>{note}</span>}
        </div>
      </div>

      {active.length > 0 && (
        <div style={css.card}>
          <strong>In flight</strong>
          <table
            style={{
              width: "100%",
              borderCollapse: "collapse",
              marginTop: "0.5rem",
            }}
          >
            <tbody>
              {active.map((j) => (
                <tr key={j.id}>
                  <td style={{ ...css.td, ...css.mono, ...css.small }}>
                    {j.id.slice(0, 8)}
                  </td>
                  <td style={css.td}>
                    {j.kind === "plan" ? "planning" : `working ${j.bead}`}
                  </td>
                  <td style={{ ...css.td, color: queueLabel(j.status).colour }}>
                    {queueLabel(j.status).text}
                  </td>
                  <td style={{ ...css.td, ...css.small, ...css.muted }}>
                    {age(j.created_at)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {recent.length > 0 && (
        <div style={css.card}>
          <strong>Recently finished</strong>
          <table
            style={{
              width: "100%",
              borderCollapse: "collapse",
              marginTop: "0.5rem",
            }}
          >
            <tbody>
              {recent.map((j) => (
                <tr key={j.id}>
                  <td style={{ ...css.td, ...css.mono, ...css.small }}>
                    {j.id.slice(0, 8)}
                  </td>
                  <td style={css.td}>
                    {j.kind === "plan" ? "planning" : j.bead}
                  </td>
                  <td style={{ ...css.td, color: queueLabel(j.status).colour }}>
                    {queueLabel(j.status).text}
                  </td>
                  <td style={{ ...css.td, ...css.small, ...css.muted }}>
                    {/* The agent's own words. A failure a person can read here is
                        a failure they do not need a terminal on the node for. */}
                    {(j.result || "").slice(0, 160)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "baseline",
        }}
      >
        <h2 style={{ marginBottom: "0.5rem" }}>Beads</h2>
        <button style={css.ghost} onClick={() => setOnlyOpen((v) => !v)}>
          {onlyOpen
            ? "showing open — show all"
            : "showing all — show open only"}
        </button>
      </div>

      {items === null && <p style={css.muted}>Loading…</p>}

      {items !== null && shown.length === 0 && (
        <div style={css.card}>
          <strong>No work is projected yet.</strong>
          <p style={{ ...css.small, ...css.muted, marginBottom: 0 }}>
            Beads reach this page from an execution node's dispatcher, or from{" "}
            <code style={css.mono}>wg-work sync-hq</code> on the control node.
            An empty list here means nothing has reported, not that there is no
            work.
          </p>
        </div>
      )}

      {items !== null && shown.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr>
              <th style={css.th}>Bead</th>
              <th style={css.th}>Title</th>
              <th style={css.th}>Project</th>
              <th style={css.th}>Status</th>
              <th style={css.th}>Agent</th>
              <th style={{ ...css.th, textAlign: "right" }}>Cents</th>
              <th style={css.th}></th>
            </tr>
          </thead>
          <tbody>
            {shown.slice(0, 100).map((i) => {
              const q = queueLabel(i.queue_state);
              const busyNow =
                i.queue_state === "queued" || i.queue_state === "running";
              return (
                <tr key={`${i.cell}:${i.bead}`}>
                  {/* The id is the way in to what happened to it (wg-m07):
                      outcome, the agent's own comment, and per-model spend. A
                      list is read to scan; that page is read to understand one
                      thing. */}
                  <td style={{ ...css.td, ...css.mono, ...css.small }}>
                    <a
                      href={`#/work/${encodeURIComponent(i.bead)}`}
                      style={{
                        color: "#0b5cad",
                        textDecoration: "none",
                        fontWeight: 600,
                      }}
                      title="what happened to this bead"
                    >
                      {i.bead}
                    </a>
                  </td>
                  <td style={css.td}>
                    {i.title || <span style={css.muted}>untitled</span>}
                  </td>
                  {/* "company" rather than blank: an empty cell reads as
                      missing data, and a bead with no project is a real and
                      different thing -- work everybody who can log in may
                      read. */}
                  <td style={{ ...css.td, ...css.small }}>
                    {i.project || <span style={css.muted}>company</span>}
                  </td>
                  <td style={{ ...css.td, ...css.small }}>{i.status}</td>
                  <td style={{ ...css.td, ...css.small, color: q.colour }}>
                    {q.text}
                    {i.queued_at && (
                      <span style={{ ...css.muted, marginLeft: "0.4rem" }}>
                        {age(i.queued_at)}
                      </span>
                    )}
                  </td>
                  <td
                    style={{
                      ...css.td,
                      ...css.small,
                      ...css.mono,
                      textAlign: "right",
                    }}
                  >
                    {/* Blank rather than 0 when nothing was spent: a zero
                        implies it ran and cost nothing. */}
                    {i.requests > 0 ? (
                      cents(i.spent_cents)
                    ) : (
                      <span style={css.muted}>–</span>
                    )}
                  </td>
                  <td style={{ ...css.td, textAlign: "right" }}>
                    {i.status !== "closed" && (
                      <button
                        style={{ ...css.ghost, opacity: busyNow ? 0.45 : 1 }}
                        disabled={busyNow}
                        onClick={() => dispatch(i.bead)}
                      >
                        {busyNow ? "in flight" : "send to an agent"}
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}
