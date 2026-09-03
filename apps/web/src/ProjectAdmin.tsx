/**
 * Creating a project, and changing which repositories it holds (wg-1dm, wg-m6p).
 *
 * Both were reachable only by writing SQL on the node. Nothing here decides
 * authorisation: the API refuses what the caller may not do, and this renders
 * the refusal. A hidden button is not a permission check.
 */
import { useEffect, useState } from "react";
import {
  api,
  asList,
  type AttachResult,
  type ProjectSummary,
  type Repository,
} from "./api";

const css = {
  muted: { color: "#666" } as const,
  card: {
    border: "1px solid #e3e3e3",
    borderRadius: "8px",
    padding: "1rem 1.15rem",
    marginBottom: "0.75rem",
    background: "#fff",
  } as const,
  label: {
    display: "block",
    fontSize: "0.75rem",
    textTransform: "uppercase",
    letterSpacing: "0.04em",
    color: "#666",
    fontWeight: 600,
    marginBottom: "0.2rem",
  } as const,
  input: {
    width: "100%",
    padding: "0.45rem 0.55rem",
    border: "1px solid #ccc",
    borderRadius: "5px",
    fontSize: "0.9rem",
    fontFamily: "inherit",
    boxSizing: "border-box",
  } as const,
  button: {
    padding: "0.5rem 0.9rem",
    border: "1px solid #0b5cad",
    background: "#0b5cad",
    color: "#fff",
    borderRadius: "5px",
    fontWeight: 600,
    cursor: "pointer",
    fontSize: "0.9rem",
  } as const,
  quiet: {
    padding: "0.25rem 0.5rem",
    border: "1px solid #ccc",
    background: "#fff",
    color: "#444",
    borderRadius: "4px",
    cursor: "pointer",
    fontSize: "0.75rem",
  } as const,
  row: { display: "flex", gap: "0.75rem", flexWrap: "wrap" } as const,
  error: { color: "#a33" } as const,
};

/** One labelled field. */
function Field(props: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  hint?: string;
  required?: boolean;
}) {
  return (
    <div style={{ flex: "1 1 14rem", marginBottom: "0.7rem" }}>
      <label style={css.label}>
        {props.label}
        {props.required ? " *" : ""}
      </label>
      <input
        style={css.input}
        value={props.value}
        placeholder={props.placeholder}
        onChange={(e) => props.onChange(e.target.value)}
      />
      {props.hint && (
        <p style={{ ...css.muted, fontSize: "0.75rem", margin: "0.2rem 0 0" }}>
          {props.hint}
        </p>
      )}
    </div>
  );
}

/**
 * The create form.
 *
 * A backup owner is a required field rather than an optional one, because the
 * schema refuses full-cycle ownership by a single person and discovering that
 * at the 400 wastes the attempt. The primary owner is left blank on purpose:
 * empty means the caller, which is the common case and the thing that makes
 * creation quick.
 */
export function CreateProject({
  onCreated,
}: {
  onCreated: (p: ProjectSummary) => void;
}) {
  const [open, setOpen] = useState(false);
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [backup, setBackup] = useState("");
  const [portfolio, setPortfolio] = useState("");
  const [objective, setObjective] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (!open) {
    return (
      <p style={{ margin: "0 0 1rem" }}>
        <button style={css.button} onClick={() => setOpen(true)}>
          New project
        </button>
      </p>
    );
  }

  const submit = () => {
    setBusy(true);
    setError(null);
    api
      .createProject({
        slug,
        name,
        backup_owner: backup,
        portfolio: portfolio || undefined,
        objective: objective || undefined,
      })
      .then((p) => {
        setOpen(false);
        setSlug("");
        setName("");
        setBackup("");
        setPortfolio("");
        setObjective("");
        onCreated(p);
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => setBusy(false));
  };

  return (
    <div style={css.card}>
      <h2 style={{ fontSize: "1rem", marginTop: 0 }}>New project</h2>
      <div style={css.row}>
        <Field
          label="Slug"
          value={slug}
          onChange={setSlug}
          placeholder="roseville-poc"
          hint="Lower-case letters, digits and single hyphens. It reaches URLs and unit names."
          required
        />
        <Field
          label="Name"
          value={name}
          onChange={setName}
          placeholder="Roseville PoC"
          required
        />
      </div>
      <div style={css.row}>
        <Field
          label="Backup owner"
          value={backup}
          onChange={setBackup}
          placeholder="someone@datopian.com"
          hint="Required, and a different person: ownership must not rest on one pair of hands."
          required
        />
        <Field
          label="Portfolio"
          value={portfolio}
          onChange={setPortfolio}
          placeholder="client"
          hint="Optional. Projects group under a portfolio; there are no subprojects."
        />
      </div>
      <Field
        label="Objective"
        value={objective}
        onChange={setObjective}
        placeholder="What this project is for"
      />
      <p style={{ ...css.muted, fontSize: "0.8rem" }}>
        You become the primary owner. Both owners are made members, so the
        project is visible to them and not only to administrators.
      </p>
      {error && <p style={css.error}>{error}</p>}
      <div style={{ display: "flex", gap: "0.5rem" }}>
        <button style={css.button} onClick={submit} disabled={busy}>
          {busy ? "Creating…" : "Create"}
        </button>
        <button
          style={css.quiet}
          onClick={() => setOpen(false)}
          disabled={busy}
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

/** How one attach result reads. */
function resultText(r: AttachResult): string {
  switch (r.status) {
    case "attached":
      return `${r.full_name} attached`;
    case "already_attached":
      return `${r.full_name} was already on this project`;
    case "taken":
      // The name is withheld when row-level security hides the holding
      // project, so the sentence has to work without it.
      return r.taken_by
        ? `${r.full_name} belongs to ${r.taken_by}, so it was left alone`
        : `${r.full_name} belongs to another project, so it was left alone`;
    default:
      return `${r.full_name}: ${r.status}`;
  }
}

/**
 * Attach and detach, for one project.
 *
 * Several at once, because onboarding a project with six repositories should
 * not be six actions, and because the answer is per repository: one held
 * elsewhere is reported and skipped rather than failing the batch.
 */
export function ManageRepositories({ slug }: { slug: string }) {
  const [repos, setRepos] = useState<Repository[] | null>(null);
  const [input, setInput] = useState("");
  const [results, setResults] = useState<AttachResult[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = () => {
    api
      .repositories(slug)
      .then(setRepos)
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      );
  };

  useEffect(load, [slug]);

  const attach = () => {
    // One per line or comma-separated, because a list of repositories arrives
    // pasted far more often than typed.
    const names = input
      .split(/[\s,]+/)
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
    if (names.length === 0) return;
    setBusy(true);
    setError(null);
    api
      .attachRepositories(slug, names)
      .then((rs) => {
        setResults(rs);
        setInput("");
        load();
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => setBusy(false));
  };

  const detach = (r: Repository) => {
    setBusy(true);
    setError(null);
    api
      .detachRepository(slug, r.owner, r.name)
      .then(() => {
        setResults([]);
        load();
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => setBusy(false));
  };

  return (
    <div style={css.card}>
      <h3 style={{ fontSize: "0.9rem", marginTop: 0 }}>
        Attached repositories
      </h3>
      {repos === null && !error && <p style={css.muted}>Loading…</p>}
      {repos !== null && asList(repos).length === 0 && (
        <p style={css.muted}>None attached yet.</p>
      )}
      {asList(repos).map((r) => (
        <p
          key={r.full_name}
          style={{ margin: "0.3rem 0", display: "flex", gap: "0.6rem" }}
        >
          <span
            style={{
              fontFamily: "ui-monospace, monospace",
              fontSize: "0.85rem",
            }}
          >
            {r.full_name}
          </span>
          <span style={{ ...css.muted, fontSize: "0.75rem" }}>
            {r.default_branch}
          </span>
          <button style={css.quiet} onClick={() => detach(r)} disabled={busy}>
            Detach
          </button>
        </p>
      ))}

      <div style={{ marginTop: "0.9rem" }}>
        <label style={css.label}>Attach repositories</label>
        <textarea
          style={{
            ...css.input,
            minHeight: "3.5rem",
            fontFamily: "ui-monospace, monospace",
          }}
          value={input}
          placeholder={"datopian/workgraph\ndatopian/portaljs"}
          onChange={(e) => setInput(e.target.value)}
        />
        <p
          style={{
            ...css.muted,
            fontSize: "0.75rem",
            margin: "0.2rem 0 0.5rem",
          }}
        >
          One per line, or comma separated. A GitHub URL works too. A repository
          already held by another project is reported and left alone rather than
          moved.
        </p>
        <button style={css.button} onClick={attach} disabled={busy}>
          {busy ? "Working…" : "Attach"}
        </button>
      </div>

      {error && <p style={css.error}>{error}</p>}
      {results.length > 0 && (
        <ul style={{ ...css.muted, fontSize: "0.8rem", marginBottom: 0 }}>
          {results.map((r) => (
            <li key={r.full_name}>{resultText(r)}</li>
          ))}
        </ul>
      )}
    </div>
  );
}
