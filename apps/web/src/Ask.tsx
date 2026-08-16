import { useEffect, useState } from "react";
import { age, api, asList, type Answer, type Claim } from "./api";

/**
 * The chief-of-staff questions (WP-F3).
 *
 * The answers are structured queries, not a prompt. Plan section 4.4 is
 * explicit that this layer must not receive the whole company as an unbounded
 * prompt, and every default question has an exact answer in the database —
 * sending them to a model would add cost, latency and a source of invention to
 * questions that do not need one.
 *
 * Each sentence therefore carries how it was arrived at. "Observed" is read
 * from a record; "calculated" is a rule applied to records and shows the rule;
 * "inferred" would mean a model wrote it. Nothing produces inferred yet, and
 * the label exists so that when something does, it is distinguishable from the
 * first day rather than retrofitted.
 */

const st = {
  muted: { color: "#666" } as const,
  card: {
    border: "1px solid #e3e3e3",
    borderRadius: "8px",
    padding: "0.85rem 1rem",
    marginBottom: "0.6rem",
    background: "#fff",
  } as const,
  tag: {
    fontSize: "0.7rem",
    textTransform: "uppercase",
    letterSpacing: "0.05em",
    padding: "0.1rem 0.4rem",
    borderRadius: "4px",
    marginRight: "0.5rem",
  } as const,
};

/** Colour is never the only carrier: the kind is spelled out as a word. */
function KindTag({ kind }: { kind: string }) {
  const tone: Record<string, { bg: string; fg: string }> = {
    observed: { bg: "#eef6ee", fg: "#0a7d33" },
    calculated: { bg: "#eef2f8", fg: "#0b5cad" },
    inferred: { bg: "#fbf3e8", fg: "#9a6400" },
  };
  const c = tone[kind] ?? { bg: "#f0f0f0", fg: "#444" };
  return <span style={{ ...st.tag, background: c.bg, color: c.fg }}>{kind}</span>;
}

function ClaimCard({ claim }: { claim: Claim }) {
  const evidence = asList(claim.evidence);
  return (
    <div style={st.card}>
      <div>
        <KindTag kind={claim.kind} />
        {claim.statement}
      </div>
      {claim.basis && (
        <div style={{ ...st.muted, fontSize: "0.85rem", marginTop: "0.35rem" }}>{claim.basis}</div>
      )}
      <div style={{ ...st.muted, fontSize: "0.78rem", marginTop: "0.35rem" }}>
        {evidence.length > 0 ? (
          <>
            from {evidence.map((e) => `${e.kind} ${e.ref}`).join(", ")}
          </>
        ) : (
          // An assertion with no evidence is only honest when it is an
          // assertion that nothing was found, and the basis above says what
          // was searched.
          <>no records to cite — see the basis above for what was searched</>
        )}
        {claim.observed_at ? ` · observed ${age(claim.observed_at)}` : ""}
      </div>
    </div>
  );
}

export function Ask() {
  const [questions, setQuestions] = useState<string[]>([]);
  const [answer, setAnswer] = useState<Answer | null>(null);
  const [asking, setAsking] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .questions()
      .then(setQuestions)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  const ask = (q: string) => {
    setAsking(q);
    setError(null);
    api
      .ask(q)
      .then(setAnswer)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setAsking(null));
  };

  return (
    <>
      <h1 style={{ marginBottom: "0.15rem" }}>Ask</h1>
      <p style={{ ...st.muted, marginTop: 0, marginBottom: "1.25rem" }}>
        Answered from your own view of the data. Anything you cannot see is not read, so it cannot
        appear here.
      </p>

      <div style={{ marginBottom: "1.5rem" }}>
        {questions.map((q) => (
          <button
            key={q}
            type="button"
            onClick={() => ask(q)}
            disabled={asking !== null}
            style={{
              display: "block",
              width: "100%",
              textAlign: "left",
              padding: "0.5rem 0.75rem",
              marginBottom: "0.35rem",
              fontSize: "0.9rem",
              cursor: "pointer",
              background: answer?.question === q ? "#eef2f8" : "#fff",
              border: "1px solid #e3e3e3",
              borderRadius: "6px",
            }}
          >
            {q}
          </button>
        ))}
      </div>

      {error && <p style={{ color: "#a33" }}>{error}</p>}
      {asking && <p style={st.muted}>Asking…</p>}

      {answer && (
        <>
          <h2 style={{ fontSize: "1rem" }}>{answer.question}</h2>
          <p style={{ ...st.muted, fontSize: "0.85rem", marginTop: 0 }}>{answer.scope}</p>
          {asList(answer.claims).map((c, i) => (
            <ClaimCard key={`${c.statement}-${i}`} claim={c} />
          ))}
        </>
      )}
    </>
  );
}
