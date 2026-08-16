import { useEffect, useState } from "react";
import { age, api, asList, type AttentionItem, type Branch, type Refusal } from "./api";

/**
 * The attention inbox (WP-F2).
 *
 * Deliberately not a flat ranked list. The design note for this work package
 * argues a flat list is "a better organised version of overwhelm" — it hands
 * someone everything at once and leaves the choosing to them. This shows a few
 * candidates per project and lets the reader descend.
 *
 * The more important decision: a project with nothing to do still appears, and
 * says WHY. An empty list reads as "nothing needs me"; if the truth is
 * "everything here is waiting on someone else's decision", the reader walks
 * away from a project that is stuck. Those are different facts and the page
 * states which one it is.
 */

const s = {
  card: {
    border: "1px solid #e3e3e3",
    borderRadius: "8px",
    padding: "1rem 1.15rem",
    marginBottom: "0.75rem",
    background: "#fff",
  } as const,
  muted: { color: "#666" } as const,
  btn: {
    padding: "0.35rem 0.75rem",
    fontSize: "0.85rem",
    cursor: "pointer",
    marginRight: "0.5rem",
  } as const,
};

/**
 * Why this item is here.
 *
 * The score is rendered with its explanation, never alone. An unexplained
 * ranking cannot be tuned and should not be trusted — and a number with no
 * reasoning invites the reader to assume the system knows something it does not.
 */
function Explanation({ item }: { item: AttentionItem }) {
  const entries = Object.entries(item.explanation ?? {});
  return (
    <div style={{ ...s.muted, fontSize: "0.85rem", marginTop: "0.35rem" }}>
      <span title="Higher scores are shown first">score {item.score.toFixed(2)}</span>
      {entries.length > 0 && " — "}
      {entries.map(([k, v], i) => (
        <span key={k}>
          {i > 0 && ", "}
          {k}: {String(v)}
        </span>
      ))}
      {entries.length === 0 && " — no explanation was recorded for this rule"}
    </div>
  );
}

function Decide({ item, onDone }: { item: AttentionItem; onDone: () => void }) {
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);

  if (!item.approval_request_id) return null;

  const send = async (approve: boolean) => {
    setBusy(true);
    setRefusal(null);
    try {
      // seen_digest is deliberately empty here: this view does not render the
      // action's digest, so claiming to have seen one would defeat the check
      // that catches an action changing under the approver.
      await api.decide(item.approval_request_id!, approve, reason, "");
      onDone();
    } catch (e: unknown) {
      const r = (e as { refusal?: Refusal }).refusal;
      setRefusal(r ?? { code: "error", policy: "", message: String(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={{ marginTop: "0.6rem" }}>
      <input
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        placeholder="Reason (required to reject)"
        style={{ padding: "0.35rem", fontSize: "0.85rem", width: "20rem", marginRight: "0.5rem" }}
      />
      <button type="button" style={s.btn} disabled={busy} onClick={() => void send(true)}>
        Approve
      </button>
      <button type="button" style={s.btn} disabled={busy} onClick={() => void send(false)}>
        Reject
      </button>
      {refusal && (
        <div
          style={{
            marginTop: "0.5rem",
            padding: "0.6rem 0.8rem",
            border: "1px solid #e0c0c0",
            background: "#fdf6f6",
            borderRadius: "6px",
            fontSize: "0.85rem",
          }}
        >
          <strong>{refusal.message}</strong>
          {refusal.policy && (
            <div style={s.muted}>policy: {refusal.policy}</div>
          )}
          {refusal.allowed_actor && <div style={s.muted}>who can: {refusal.allowed_actor}</div>}
          {refusal.remediation && <div style={{ marginTop: "0.25rem" }}>{refusal.remediation}</div>}
        </div>
      )}
    </div>
  );
}

function Candidate({ item, onChanged }: { item: AttentionItem; onChanged: () => void }) {
  return (
    <div style={s.card}>
      <div style={{ display: "flex", justifyContent: "space-between", gap: "1rem", flexWrap: "wrap" }}>
        <strong>{item.rule}</strong>
        <span style={{ ...s.muted, fontSize: "0.85rem" }} title={item.created_at}>
          raised {age(item.created_at)}
        </span>
      </div>
      <Explanation item={item} />
      <Decide item={item} onDone={onChanged} />
    </div>
  );
}

function BranchView({ branch, onChanged }: { branch: Branch; onChanged: () => void }) {
  const candidates = asList(branch.candidates);
  return (
    <section style={{ marginBottom: "2rem" }}>
      <h2 style={{ fontSize: "1rem", marginBottom: "0.4rem" }}>{branch.project_name}</h2>
      {candidates.length === 0 ? (
        <p style={{ ...s.muted, fontSize: "0.9rem", margin: 0 }}>
          {branch.reason ?? "Nothing here needs you."}
        </p>
      ) : (
        <>
          {candidates.map((c) => (
            <Candidate key={c.id} item={c} onChanged={onChanged} />
          ))}
          {branch.remaining > 0 && (
            <p style={{ ...s.muted, fontSize: "0.85rem" }}>
              {branch.remaining} more in this project, not shown. Showing the top few is
              deliberate: the whole list is what you already had.
            </p>
          )}
        </>
      )}
    </section>
  );
}

export function Inbox() {
  const [branches, setBranches] = useState<Branch[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    api
      .branches()
      .then(setBranches)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, [nonce]);

  if (error) return <p style={{ color: "#a33" }}>Could not load the inbox: {error}</p>;
  if (!branches) return <p style={s.muted}>Loading…</p>;

  const withWork = branches.filter((b) => asList(b.candidates).length > 0);

  return (
    <>
      <h1 style={{ marginBottom: "0.15rem" }}>Needs you</h1>
      <p style={{ ...s.muted, marginTop: 0, marginBottom: "1.5rem" }}>
        {withWork.length === 0
          ? "Nothing is waiting on you right now. Projects with no candidates say why below."
          : `${withWork.length} project${withWork.length === 1 ? " has" : "s have"} something for you.`}
      </p>
      {branches.length === 0 && (
        <p style={s.muted}>
          No projects are visible to you. That may be because none exist yet, or because you are
          not a member of any.
        </p>
      )}
      {branches.map((b) => (
        <BranchView key={b.project_slug} branch={b} onChanged={() => setNonce((n) => n + 1)} />
      ))}
    </>
  );
}
