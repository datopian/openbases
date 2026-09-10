/**
 * What happened to one bead (wg-m07).
 *
 * Three beads were dispatched on 4 September, all reported `done`, and all
 * three had in fact reported that they could not do the work — the agent left
 * them open and wrote a comment saying exactly what it had looked for and not
 * found. That comment existed only in the graph on the execution node, and the
 * queue's `done` read as success.
 *
 * So this page leads with the outcome, then the agent's own words, then what it
 * cost. In that order because it is the order somebody asks: did it work, why
 * not, and what did I pay to find out.
 */
import { useEffect, useState, type CSSProperties } from "react";
import { age, api, asList, type BeadDetail } from "./api";

const css = {
  muted: { color: "#666" } as const,
  // The same blue the rest of the interface uses for a link out.
  link: { color: "#0b5cad", textDecoration: "none", fontWeight: 600 } as const,
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
    padding: "0.55rem 0.75rem 0.55rem 0",
    borderBottom: "1px solid #f0f0f0",
  } as const,
  h2: {
    fontSize: "1rem",
    marginTop: "1.6rem",
    marginBottom: "0.4rem",
  } as const,
  mono: { fontFamily: "ui-monospace, monospace", fontSize: "0.8rem" } as const,
  pre: {
    fontFamily: "ui-monospace, monospace",
    fontSize: "0.72rem",
    whiteSpace: "pre-wrap",
    wordBreak: "break-word",
    background: "#fafafa",
    border: "1px solid #eee",
    borderRadius: "6px",
    padding: "0.6rem 0.7rem",
    maxHeight: "18rem",
    overflowY: "auto",
    margin: 0,
  } as const,
};

/**
 * How each outcome reads, and what it means.
 *
 * `blocked` gets the plainest wording, because it is the one that was
 * previously indistinguishable from success and the one a reader is most
 * likely to misjudge.
 *
 * `landed` exists because `blocked` was being shown for work that was in an
 * open pull request — sa-kfh's change sat on datopian/portaljs#1662 while this
 * page said the agent had not finished it. Those need different words: one
 * asks somebody to unblock the agent, the other asks them to review a diff.
 */
function outcomeLabel(o: BeadDetail["outcome"]): {
  text: string;
  note: string;
  colour: string;
} {
  switch (o) {
    case "done":
      return {
        text: "Done",
        note: "the bead is closed and its change reached the repository",
        colour: "#1a7f37",
      };
    case "closed_unlanded":
      // Closed, and nothing reached the repository.
      //
      // Not an error: a bead whose work needed no code change closes exactly
      // like this. But sa-iyu also closed like this while 29 finished files
      // sat uncommitted on a node, and every surface said "Done".
      return {
        text: "Closed, nothing landed",
        note:
          "the bead is closed and no pull request exists. Correct if it needed no " +
          "code change \u2014 worth a look if it was supposed to produce one.",
        colour: "#6f42c1",
      };
    case "landed":
      return {
        text: "In review",
        note:
          "the change is in a pull request and the bead is still open. The work exists — " +
          "see below for where it went.",
        colour: "#0b5cad",
      };
    case "blocked":
      return {
        text: "Ran, did not finish",
        note:
          "the agent ran and left the bead open — usually because it could not do the work. " +
          "Its reason is below, if it gave one.",
        colour: "#9a6700",
      };
    case "failed":
      return { text: "Failed", note: "the run itself failed", colour: "#a33" };
    case "running":
      return {
        text: "Running",
        note: "an agent has it now",
        colour: "#0b5cad",
      };
    case "queued":
      return {
        text: "Queued",
        note: "waiting for an agent",
        colour: "#0b5cad",
      };
    default:
      return {
        text: "Never dispatched",
        note: "no agent has run against this bead",
        colour: "#666",
      };
  }
}

/** Cents as money, from a decimal string. numeric(16,8) does not survive a float. */
function money(cents: string | number | null | undefined): string {
  if (cents === null || cents === undefined) return "—";
  const n = typeof cents === "number" ? cents : Number.parseFloat(cents);
  if (Number.isNaN(n)) return "—";
  if (n === 0) return "0";
  return n < 100 ? `${n.toFixed(2)}¢` : `$${(n / 100).toFixed(2)}`;
}

export function Bead({ id, onBack }: { id: string; onBack: () => void }) {
  const [d, setD] = useState<BeadDetail | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setD(null);
    setError(null);
    api
      .bead(id)
      .then(setD)
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      );
  }, [id]);

  const back = (
    <a
      href="#/work"
      style={{
        color: "#0b5cad",
        textDecoration: "none",
        fontWeight: 600,
        fontSize: "0.85rem",
      }}
      onClick={(e) => {
        e.preventDefault();
        onBack();
      }}
    >
      ← Work
    </a>
  );

  if (error) {
    return (
      <>
        {back}
        <h1>{id}</h1>
        <p style={{ color: "#a33" }}>Could not load this bead: {error}</p>
      </>
    );
  }
  if (!d) {
    return (
      <>
        {back}
        <p style={css.muted}>Loading…</p>
      </>
    );
  }

  const o = outcomeLabel(d.outcome);
  const models = asList(d.spend?.by_model);
  // Tolerated as absent: an older control plane does not send this field, and
  // a page that throws on it shows nothing at all rather than one section less.
  const prs = d.pull_requests ?? [];

  return (
    <>
      {back}
      <h1 style={{ marginBottom: "0.15rem", fontSize: "1.35rem" }}>
        {d.title || <span style={css.muted}>untitled</span>}
      </h1>
      <p style={{ ...css.muted, marginTop: 0, fontSize: "0.85rem" }}>
        <span style={css.mono}>{d.bead}</span>
        {d.project ? ` · ${d.project}` : " · no project"}
        {d.kind ? ` · ${d.kind}` : ""}
        {d.cell ? ` · cell ${d.cell}` : ""}
        {d.run?.rig ? ` · rig ${d.run.rig}` : ""}
      </p>

      <div style={{ ...css.card, borderLeft: `3px solid ${o.colour}` }}>
        <div style={{ fontWeight: 600, color: o.colour }}>{o.text}</div>
        <div style={{ ...css.muted, fontSize: "0.85rem" }}>{o.note}</div>
        <div
          style={{ ...css.muted, fontSize: "0.75rem", marginTop: "0.35rem" }}
        >
          bead status <b>{d.status || "unknown"}</b>
          {d.run?.finished ? ` · run finished ${age(d.run.finished)}` : ""}
          {d.last_seen ? ` · projected ${age(d.last_seen)}` : ""}
        </div>
      </div>

      {/* The agent's own words, second only to the outcome. This is the thing
          that was previously reachable only by reading the graph. */}
      {d.comment && (
        <>
          <h2 style={css.h2}>What the agent said</h2>
          <div style={css.card}>
            <p style={{ margin: 0, whiteSpace: "pre-wrap" }}>
              {d.comment.text}
            </p>
            <p
              style={{
                ...css.muted,
                fontSize: "0.75rem",
                marginBottom: 0,
                marginTop: "0.5rem",
              }}
            >
              {d.comment.by || "unknown"} · {age(d.comment.at)}
            </p>
          </div>
        </>
      )}
      {!d.comment && d.outcome === "blocked" && (
        <p style={{ ...css.muted, fontSize: "0.85rem" }}>
          The agent left no comment, so there is no stated reason. The run log
          below is the only record.
        </p>
      )}

      {/* Where the work went, directly after what the agent said, because
          "did it land?" is the next question somebody asks and the answer used
          to require reading a working tree on the execution node.

          An empty list is stated rather than hidden: for a bead whose work
          needed no code change it is the correct answer, and saying nothing
          leaves a reader unable to tell that from a landing that failed. */}
      <h2 style={css.h2}>Where it landed</h2>
      {prs.length === 0 && (
        <p style={{ ...css.muted, fontSize: "0.85rem" }}>
          {d.outcome === "done"
            ? "No branch was pushed. Either the work needed no code change, or the run changed nothing."
            : "No branch was pushed."}
        </p>
      )}
      {prs.map((pr) => (
        <div key={pr.url} style={css.card}>
          <a href={pr.url} target="_blank" rel="noreferrer" style={css.link}>
            {pr.repository}#{pr.number}
          </a>
          <div style={{ ...css.muted, fontSize: "0.75rem", marginTop: "0.35rem" }}>
            <code>{pr.head}</code> → <code>{pr.base}</code>
            {pr.opened ? ` · opened ${age(pr.opened)}` : ""}
          </div>
        </div>
      ))}
      {prs.length > 1 && (
        <p style={{ ...css.muted, fontSize: "0.75rem" }}>
          More than one, because this bead was worked more than once. The
          newest is first.
        </p>
      )}

      <h2 style={css.h2}>Model and cost</h2>

      {/* What is running it, which is answerable straight away — the node
          reports the harness and model when it starts. The table below comes
          from the hourly cost import, so for a run in flight it is empty while
          this line is already correct. */}
      {(d.run?.harness || d.run?.model) && (
        <p style={{ margin: "0 0 0.6rem" }}>
          <code>{d.run.harness ?? "unknown harness"}</code>
          {d.run.model ? (
            <>
              {" running "}
              <code>{d.run.model}</code>
            </>
          ) : null}
          {d.outcome === "running" ? " — in flight now" : ""}
        </p>
      )}
      {d.run && !d.run.harness && !d.run.model && (
        <p style={{ ...css.muted, fontSize: "0.85rem" }}>
          This run did not report which harness and model it used. Runs from
          before that was recorded show nothing here.
        </p>
      )}

      {models.length === 0 && (
        <p style={{ ...css.muted, fontSize: "0.85rem" }}>
          {d.spend?.awaiting_import
            ? "No cost counted for this run yet. Usage arrives from the AI Gateway on an hourly import, so this is not the same as free."
            : "No spend recorded. Usage arrives from the AI Gateway on an hourly import, so a run that finished in the last hour often shows nothing — which is not the same as free."}
        </p>
      )}
      {models.length > 0 && (
        <>
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr>
                <th style={css.th}>Model</th>
                <th style={css.th}>Calls</th>
                <th style={css.th}>In</th>
                <th style={css.th}>Out</th>
                <th style={css.th}>Cost</th>
              </tr>
            </thead>
            <tbody>
              {models.map((m) => (
                <tr key={`${m.provider ?? ""}/${m.model}`}>
                  <td style={{ ...css.td, ...css.mono }}>{m.model}</td>
                  <td style={css.td}>{m.calls}</td>
                  <td style={{ ...css.td, ...css.muted }}>{m.input_tokens}</td>
                  <td style={{ ...css.td, ...css.muted }}>{m.output_tokens}</td>
                  <td style={css.td}>{money(m.cents)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p style={{ ...css.muted, fontSize: "0.8rem" }}>
            {d.spend.calls} call{d.spend.calls === 1 ? "" : "s"},{" "}
            {money(d.spend.cents)} in total
            {d.spend.newest_record
              ? ` · newest record ${age(d.spend.newest_record)}`
              : ""}
          </p>
        </>
      )}

      {d.run?.log_tail && (
        <>
          <h2 style={css.h2}>Run log, last lines</h2>
          <pre style={css.pre}>{d.run.log_tail}</pre>
        </>
      )}
    </>
  );
}

/** The bead id in the URL fragment, or null. */
export function beadFromHash(): string | null {
  const m = /^#\/work\/(.+)$/.exec(window.location.hash);
  const raw = m?.[1];
  return raw ? decodeURIComponent(raw) : null;
}

/**
 * A bead id, as a link to what happened to it.
 *
 * One component because the first version of this linked ONE of the four
 * places a bead id is rendered — the work list — and left the queue rows and
 * the project page's work list as plain text. Which meant "click the bead"
 * worked or did not depending on which table you happened to be looking at,
 * and reported as the feature not existing.
 */
export function BeadLink({
  bead,
  style,
}: {
  // Optional, because a plan job has no bead of its own and the queue tables
  // render the same cell for both kinds. Absent renders nothing rather than an
  // empty link, which would be a clickable gap.
  bead?: string | null;
  style?: CSSProperties;
}) {
  if (!bead) return null;
  return (
    <a
      href={`#/work/${encodeURIComponent(bead)}`}
      style={{
        color: "#0b5cad",
        textDecoration: "none",
        fontWeight: 600,
        ...style,
      }}
      title="what happened to this bead"
    >
      {bead}
    </a>
  );
}
