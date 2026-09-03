/**
 * What the platform is doing (wg-7bh).
 *
 * Every number here already existed in the database and nothing could reach it:
 * three cells, twenty thousand agent health events, thirteen open alerts and
 * seventy-five unattributed beads, all visible only from a database shell on the
 * control node. "How many cells, how many agents are running, how deep is the
 * queue, is the work graph healthy" are the questions somebody asks when
 * something feels wrong, and the answer being reachable only by the person with
 * psql is the same failure as an alert nobody receives.
 *
 * Company management only, enforced in system_platform_state rather than here.
 * A hidden panel is not a permission check.
 */
import { useEffect, useState } from "react";
import { age, api, asList, isStale, type PlatformState } from "./api";

const css = {
  muted: { color: "#666" } as const,
  card: {
    border: "1px solid #e3e3e3",
    borderRadius: "8px",
    padding: "0.9rem 1.1rem",
    marginBottom: "0.75rem",
    background: "#fff",
  } as const,
  th: {
    textAlign: "left",
    fontWeight: 600,
    fontSize: "0.75rem",
    textTransform: "uppercase",
    letterSpacing: "0.04em",
    color: "#666",
    borderBottom: "1px solid #e3e3e3",
    padding: "0.45rem 0.75rem 0.45rem 0",
  } as const,
  td: {
    padding: "0.6rem 0.75rem 0.6rem 0",
    borderBottom: "1px solid #f0f0f0",
    verticalAlign: "top",
  } as const,
  h2: {
    fontSize: "1rem",
    marginTop: "1.75rem",
    marginBottom: "0.5rem",
  } as const,
  big: { fontSize: "1.5rem", fontWeight: 600 } as const,
  warn: { color: "#a33" } as const,
};

/** One headline number. */
function Stat({
  label,
  value,
  note,
}: {
  label: string;
  value: string | number;
  note?: string;
}) {
  return (
    <div style={{ ...css.card, flex: "1 1 8rem", marginBottom: 0 }}>
      <div style={css.big}>{value}</div>
      <div
        style={{
          ...css.muted,
          fontSize: "0.75rem",
          textTransform: "uppercase",
        }}
      >
        {label}
      </div>
      {note && <div style={{ ...css.muted, fontSize: "0.75rem" }}>{note}</div>}
    </div>
  );
}

export function Platform() {
  const [state, setState] = useState<PlatformState | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .platform()
      .then(setState)
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      );
  }, []);

  if (error) {
    return (
      <>
        <h1>Platform</h1>
        <p style={css.warn}>Could not load platform state: {error}</p>
        <p style={css.muted}>
          This view is company management only. If that is the refusal, an
          organisation_admin or executive grant is what it needs.
        </p>
      </>
    );
  }
  if (!state) return <p style={css.muted}>Loading…</p>;

  const cells = asList(state.cells);
  const graphs = asList(state.graphs);
  const alerts = asList(state.alerts);
  const unattributed = asList(state.unattributed);
  const queue = state.queue ?? {};
  const queueTotal = Object.values(queue).reduce((a, b) => a + b, 0);
  const liveAgents = cells.reduce((a, c) => a + c.agents_live, 0);
  const capacity = cells.reduce((a, c) => a + c.max_concurrent_agents, 0);
  const openAlerts = alerts.reduce((a, x) => a + x.count, 0);
  const homeless = unattributed.reduce((a, x) => a + x.beads, 0);

  return (
    <>
      <h1 style={{ marginBottom: "0.15rem" }}>Platform</h1>
      <p style={{ ...css.muted, marginTop: 0, fontSize: "0.85rem" }}>
        as of {age(state.observed_at)}
      </p>

      <div
        style={{
          display: "flex",
          gap: "0.75rem",
          flexWrap: "wrap",
          marginBottom: "0.5rem",
        }}
      >
        <Stat label="Cells" value={cells.length} />
        <Stat
          label="Agents live"
          value={liveAgents}
          note={`of ${capacity} allowed, seen in the last 15 min`}
        />
        <Stat label="Queue" value={queueTotal} note={describeQueue(queue)} />
        <Stat label="Beads" value={state.beads} />
        <Stat label="Projects" value={state.projects} />
        <Stat label="Repositories" value={state.repositories} />
      </div>

      <h2 style={css.h2}>Execution cells</h2>
      {cells.length === 0 && <p style={css.muted}>No cells are registered.</p>}
      {cells.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr>
              <th style={css.th}>Cell</th>
              <th style={css.th}>Trust</th>
              <th style={css.th}>Agents</th>
              <th style={css.th}>Rigs</th>
              <th style={css.th}>Projects</th>
              <th style={css.th}>Escalations 1h</th>
              <th style={css.th}>Last report</th>
            </tr>
          </thead>
          <tbody>
            {cells.map((c) => (
              <tr key={c.cell}>
                <td style={css.td}>{c.cell}</td>
                <td style={{ ...css.td, ...css.muted }}>{c.trust_domain}</td>
                <td style={css.td}>
                  {c.agents_live} / {c.max_concurrent_agents}
                  {c.agents_ever > c.agents_live && (
                    <span style={{ ...css.muted, fontSize: "0.75rem" }}>
                      {" "}
                      ({c.agents_ever} ever)
                    </span>
                  )}
                </td>
                <td style={{ ...css.td, ...css.muted, fontSize: "0.8rem" }}>
                  {asList(c.rigs).join(", ") || "—"}
                </td>
                <td style={css.td}>
                  {c.projects}
                  {/* More than one project on a cell is why bead attribution
                      needs a label, so it is worth flagging where somebody is
                      already looking at the cell. */}
                  {c.projects > 1 && (
                    <span style={{ ...css.muted, fontSize: "0.7rem" }}>
                      {" "}
                      shared
                    </span>
                  )}
                </td>
                <td
                  style={
                    c.escalations_last_hour > 0
                      ? { ...css.td, ...css.warn }
                      : css.td
                  }
                >
                  {c.escalations_last_hour}
                </td>
                <td
                  style={{
                    ...css.td,
                    ...(isStale(c.last_report, 1) ? css.warn : css.muted),
                    fontSize: "0.8rem",
                  }}
                  title={c.last_report ?? "never"}
                >
                  {age(c.last_report)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2 style={css.h2}>Work graphs</h2>
      <p style={{ ...css.muted, fontSize: "0.8rem", marginTop: 0 }}>
        Whether each graph is <em>readable by the service user</em> is checked
        by the monitor and arrives in the inbox as{" "}
        <code>platform_graph_unreadable</code>. This is what has been projected
        out of each one.
      </p>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={css.th}>Graph</th>
            <th style={css.th}>Scope</th>
            <th style={css.th}>Project</th>
            <th style={css.th}>Cell</th>
            <th style={css.th}>Beads</th>
            <th style={css.th}>Last projected</th>
          </tr>
        </thead>
        <tbody>
          {graphs.map((g) => (
            <tr key={g.name}>
              <td style={css.td}>{g.name}</td>
              <td style={{ ...css.td, ...css.muted }}>{g.scope}</td>
              <td style={{ ...css.td, ...css.muted }}>{g.project ?? "—"}</td>
              <td style={{ ...css.td, ...css.muted }}>{g.cell ?? "—"}</td>
              <td style={css.td}>{g.beads}</td>
              <td
                style={{ ...css.td, ...css.muted, fontSize: "0.8rem" }}
                title={g.last_seen ?? "never"}
              >
                {age(g.last_seen)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {homeless > 0 && (
        <>
          <h2 style={css.h2}>Beads belonging to no project</h2>
          <p style={{ ...css.muted, fontSize: "0.85rem", marginTop: 0 }}>
            A bead is attributed by a <code>project:&lt;slug&gt;</code> label,
            and falls back to the cell only when the cell serves exactly one
            project. These carry neither, so no project page can show them —
            which is how sixteen beads from a demo went missing.
          </p>
          {unattributed.map((u) => (
            <div key={`${u.cell}-${u.graph ?? ""}`} style={css.card}>
              <b>{u.beads}</b> beads in{" "}
              <code>{u.graph ?? "an unknown graph"}</code> on cell{" "}
              <code>{u.cell}</code>, which serves {u.projects_on_cell} project
              {u.projects_on_cell === 1 ? "" : "s"}.
            </div>
          ))}
        </>
      )}

      <h2 style={css.h2}>Open alerts</h2>
      {openAlerts === 0 && <p style={css.muted}>Nothing open.</p>}
      {alerts.map((a) => (
        <div key={a.rule} style={css.card}>
          <b>{a.count}</b> × <code>{a.rule}</code>
          <span style={{ ...css.muted, fontSize: "0.8rem" }}>
            {" "}
            newest {age(a.newest)}
          </span>
        </div>
      ))}
    </>
  );
}

/** "3 running, 1 failed" — the queue in words. */
function describeQueue(queue: Record<string, number>): string {
  const parts = Object.entries(queue)
    .filter(([, n]) => n > 0)
    .map(([state, n]) => `${n} ${state}`);
  return parts.length > 0 ? parts.join(", ") : "empty";
}
