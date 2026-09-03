import { useCallback, useEffect, useState } from "react";
import { api } from "./api";

/**
 * The approval page for a device authorization grant (wg-8la, RFC 8628).
 *
 * A machine that cannot hold a secret asks for a code; this is where a person
 * decides whether it gets one. The token that results is minted in the
 * approver's name, so this page is the moment the attribution is created.
 *
 * Two deliberate refusals.
 *
 * It does NOT approve on load, even though the link carries the code. The code
 * in the URL saves the typing, not the deciding, and a link travels through
 * chat, email and screen shares — a page that approved on load would turn one
 * into a credential.
 *
 * It does NOT distinguish an unknown code from an expired or already-used one.
 * The API deliberately answers all three the same way, so that a mistyped code
 * teaches nobody which codes exist, and a page that helpfully explained the
 * difference would undo that.
 */
export function Device({ code: initial }: { code: string }) {
  const [code, setCode] = useState(initial);
  const [req, setReq] = useState<{
    client_label: string;
    scopes: string[];
    expires_at: string;
    approved: boolean;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);

  const look = useCallback(async (c: string) => {
    setError(null);
    setReq(null);
    if (!c.trim()) return;
    try {
      setReq(await api.deviceRequest(c.trim().toUpperCase()));
    } catch {
      // One message for every reason, matching the API.
      setError("That code is not valid. It may have expired, or already been used.");
    }
  }, []);

  useEffect(() => {
    if (initial) look(initial);
  }, [initial, look]);

  const approve = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.deviceApprove(code.trim().toUpperCase());
      setDone(true);
    } catch {
      setError("That code is not valid. It may have expired, or already been used.");
    } finally {
      setBusy(false);
    }
  };

  if (done) {
    return (
      <div style={{ maxWidth: "34rem" }}>
        <h2>Approved</h2>
        <p>
          The client is collecting its token now. You can close this page.
        </p>
        <p style={{ color: "#666", fontSize: "0.9rem" }}>
          It is valid for eight hours and is recorded against your name. Revoke
          it from the Tokens page if you did not expect this.
        </p>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: "34rem" }}>
      <h2>Approve a device</h2>
      <p style={{ color: "#666", fontSize: "0.9rem" }}>
        A program is asking for a credential in your name. Approve it only if
        you started it.
      </p>

      <div style={{ margin: "1rem 0" }}>
        <label htmlFor="code" style={{ display: "block", fontSize: "0.85rem", color: "#666" }}>
          Code
        </label>
        <input
          id="code"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          onBlur={() => look(code)}
          placeholder="ABCD-2345"
          style={{
            fontFamily: "ui-monospace, monospace",
            fontSize: "1.1rem",
            padding: "0.4rem",
            letterSpacing: "0.08em",
            textTransform: "uppercase",
          }}
        />
      </div>

      {error && (
        <div style={{ padding: "0.75rem", background: "#fdf1f1", border: "1px solid #e7c3c3" }}>
          {error}
        </div>
      )}

      {req && !req.approved && (
        <div style={{ border: "1px solid #ddd", padding: "1rem" }}>
          <div style={{ fontSize: "0.85rem", color: "#666" }}>Asking</div>
          {/* Rendered as text, never as markup: the label is whatever the
              client called itself and nobody verified it. Said out loud below
              for the same reason. */}
          <div style={{ fontWeight: 600, margin: "0.2rem 0 0.6rem" }}>{req.client_label}</div>

          <div style={{ fontSize: "0.85rem", color: "#666" }}>To be able to</div>
          <ul style={{ margin: "0.2rem 0 0.6rem 1.1rem", padding: 0 }}>
            {req.scopes.length === 0 && <li>read only what you can already read</li>}
            {req.scopes.map((s) => (
              <li key={s}>{describeScope(s)}</li>
            ))}
          </ul>

          <div style={{ fontSize: "0.8rem", color: "#666", marginBottom: "0.8rem" }}>
            The name above is self-reported by the program and has not been
            verified. Expires {new Date(req.expires_at).toLocaleTimeString()}.
          </div>

          <button onClick={approve} disabled={busy} style={{ padding: "0.45rem 0.9rem" }}>
            {busy ? "Approving…" : "Approve for eight hours"}
          </button>
        </div>
      )}

      {req?.approved && (
        <div style={{ padding: "0.75rem", background: "#f4f8f4", border: "1px solid #cadeca" }}>
          That code has already been approved. The client should have its token.
        </div>
      )}
    </div>
  );
}

/**
 * A dotted action name in words.
 *
 * "agent.dispatch" tells somebody who wrote the authorisation layer what it
 * means and tells nobody else. A person approving a credential should be able
 * to read what they are agreeing to.
 */
function describeScope(s: string): string {
  switch (s) {
    case "project.read":
      return "see the projects you are a member of";
    case "organisation.read":
      return "see company-wide information";
    case "work.create":
      return "file work — turn a brief into beads";
    case "work.update":
      return "change work items";
    case "work.assign":
      return "assign work to people";
    case "agent.dispatch":
      return "run an agent on a bead, which spends money";
    case "agent.inspect":
      return "see what agents are doing";
    case "agent.stop":
      return "stop a running agent";
    case "repository.read":
      return "see repository status";
    case "pull_request.create":
      return "open pull requests";
    case "audit.read":
      return "read the audit log";
    default:
      return s;
  }
}

/** The code from the URL fragment, if the link carried one. */
export function deviceCodeFromHash(): string | null {
  const m = /^#\/device(?:\/(.+))?$/.exec(window.location.hash);
  if (!m) return null;
  return m[1] ? decodeURIComponent(m[1]).toUpperCase() : "";
}
