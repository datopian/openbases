import { useEffect, useState } from "react";
import {
  age,
  api,
  asList,
  isStale,
  type Identity,
  type ProjectDetail,
  type ProjectSummary,
  type RepositoryStatus,
  type Signal,
} from "./api";

/**
 * Portfolio and project interface (WP-F1).
 *
 * The acceptance criterion is that a non-technical reader understands project
 * status without a terminal, and that every derived status explains its inputs
 * and freshness. That rules out the obvious design: a table of coloured dots.
 * A dot cannot say "this repository has never reported", and that sentence is
 * the single most useful thing this page can tell someone, because a broken
 * integration otherwise looks exactly like a quiet project.
 *
 * So every signal renders its basis and its age, and staleness is called out
 * rather than left for the reader to compute from a timestamp.
 */

const css = {
  page: {
    fontFamily: "system-ui, -apple-system, sans-serif",
    maxWidth: "68rem",
    margin: "0 auto",
    padding: "2rem 1.5rem 4rem",
    lineHeight: 1.55,
    color: "#1a1a1a",
  } as const,
  muted: { color: "#666" } as const,
  card: {
    border: "1px solid #e3e3e3",
    borderRadius: "8px",
    padding: "1rem 1.15rem",
    marginBottom: "0.75rem",
    background: "#fff",
  } as const,
  th: {
    textAlign: "left",
    fontWeight: 600,
    fontSize: "0.8rem",
    textTransform: "uppercase",
    letterSpacing: "0.04em",
    color: "#666",
    borderBottom: "1px solid #e3e3e3",
    padding: "0.5rem 0.75rem 0.5rem 0",
  } as const,
  td: { padding: "0.7rem 0.75rem 0.7rem 0", borderBottom: "1px solid #f0f0f0", verticalAlign: "top" } as const,
  link: { color: "#0b5cad", textDecoration: "none", fontWeight: 600 } as const,
};

/** Visibility is shown as a word, never only as a colour. */
function Visibility({ value }: { value: string }) {
  const tone: Record<string, string> = {
    public: "#0a7d33",
    internal: "#0b5cad",
    confidential: "#a33",
    restricted: "#a33",
  };
  return (
    <span style={{ color: tone[value] ?? "#444", fontWeight: 600, fontSize: "0.85rem" }}>
      {value}
    </span>
  );
}

/**
 * One derived signal.
 *
 * The basis is always rendered, not tucked behind a tooltip: a reader deciding
 * whether to act on "0 open pull requests" needs to know whether that means the
 * work is done or that nothing has ever been received.
 */
function SignalCard({ signal }: { signal: Signal }) {
  const stale = isStale(signal.observed_at);
  return (
    <div style={css.card}>
      <div style={{ display: "flex", justifyContent: "space-between", gap: "1rem", flexWrap: "wrap" }}>
        <strong>{signal.name}</strong>
        <span
          title={signal.observed_at ?? "no observation recorded"}
          style={{ ...css.muted, fontSize: "0.85rem", color: stale ? "#a33" : "#666" }}
        >
          {signal.observed_at ? `observed ${age(signal.observed_at)}` : "never observed"}
          {stale && signal.observed_at ? " — may be out of date" : ""}
        </span>
      </div>
      <div style={{ fontSize: "1.1rem", margin: "0.2rem 0 0.4rem" }}>{signal.value}</div>
      <div style={{ ...css.muted, fontSize: "0.9rem" }}>{signal.basis}</div>
    </div>
  );
}

function Repositories({ repos }: { repos: RepositoryStatus[] }) {
  if (repos.length === 0) {
    return <p style={css.muted}>No repositories are linked to this project.</p>;
  }
  return (
    <>
      {repos.map((r) => {
        const prs = asList(r.pull_requests);
        return (
          <div key={r.full_name} style={css.card}>
            <div style={{ display: "flex", justifyContent: "space-between", gap: "1rem", flexWrap: "wrap" }}>
              <strong>{r.full_name}</strong>
              <span style={{ ...css.muted, fontSize: "0.85rem" }} title={r.last_projected ?? ""}>
                {r.last_projected ? `last activity ${age(r.last_projected)}` : "never reported"}
              </span>
            </div>
            {prs.length === 0 ? (
              <p style={{ ...css.muted, fontSize: "0.9rem", margin: "0.4rem 0 0" }}>
                {r.last_projected
                  ? "No pull requests recorded."
                  : "Nothing has been received for this repository yet. That may mean no activity, or that the integration is not delivering."}
              </p>
            ) : (
              <table style={{ width: "100%", borderCollapse: "collapse", marginTop: "0.5rem" }}>
                <tbody>
                  {prs.slice(0, 8).map((pr) => (
                    <tr key={pr.number}>
                      <td style={{ ...css.td, width: "3.5rem" }}>#{pr.number}</td>
                      <td style={css.td}>{pr.title}</td>
                      <td style={{ ...css.td, width: "6rem" }}>{pr.state}</td>
                      <td style={{ ...css.td, width: "7rem" }}>
                        {pr.checks_state ? `checks: ${pr.checks_state}` : "checks: none"}
                      </td>
                      <td style={{ ...css.td, width: "7rem", ...css.muted }} title={pr.updated_at}>
                        {age(pr.updated_at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        );
      })}
    </>
  );
}

function ProjectPage({ slug, onBack }: { slug: string; onBack: () => void }) {
  const [detail, setDetail] = useState<(ProjectDetail & { repositories: RepositoryStatus[] }) | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setDetail(null);
    setError(null);
    api
      .projectDetail(slug)
      .then(setDetail)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, [slug]);

  return (
    <>
      <a
        href="#"
        style={{ ...css.link, fontSize: "0.9rem" }}
        onClick={(e) => {
          e.preventDefault();
          onBack();
        }}
      >
        ← All projects
      </a>
      {error && <p style={{ color: "#a33" }}>Could not load this project: {error}</p>}
      {!detail && !error && <p style={css.muted}>Loading…</p>}
      {detail && (
        <>
          <h1 style={{ marginBottom: "0.15rem" }}>{detail.name}</h1>
          <p style={{ ...css.muted, marginTop: 0 }}>
            {detail.portfolio ? `${detail.portfolio} · ` : ""}
            <Visibility value={detail.visibility} /> · {detail.status} · owner {detail.primary_owner}
            {detail.backup_owner ? ` (backup ${detail.backup_owner})` : ""}
          </p>

          <h2 style={{ fontSize: "1rem", marginTop: "1.75rem" }}>Status</h2>
          {asList(detail.signals).map((s) => <SignalCard key={s.name} signal={s} />)}

          <h2 style={{ fontSize: "1rem", marginTop: "1.75rem" }}>Repositories</h2>
          <Repositories repos={asList(detail.repositories)} />
        </>
      )}
    </>
  );
}

function Portfolio({ onOpen }: { onOpen: (slug: string) => void }) {
  const [projects, setProjects] = useState<ProjectSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .projects()
      .then(setProjects)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  if (error) return <p style={{ color: "#a33" }}>Could not load projects: {error}</p>;
  if (!projects) return <p style={css.muted}>Loading…</p>;
  const rows = asList(projects);
  if (rows.length === 0) {
    // Empty because of permissions is a different thing from empty because
    // nothing exists, and the reader cannot tell from a blank table.
    return (
      <p style={css.muted}>
        No projects are visible to you. That may be because none exist yet, or because you are not a
        member of any.
      </p>
    );
  }

  return (
    <table style={{ width: "100%", borderCollapse: "collapse" }}>
      <thead>
        <tr>
          <th style={css.th}>Project</th>
          <th style={css.th}>Portfolio</th>
          <th style={css.th}>Visibility</th>
          <th style={css.th}>Status</th>
          <th style={css.th}>Owner</th>
          <th style={css.th}>Repos</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((p) => (
          <tr key={p.slug}>
            <td style={css.td}>
              <a
                href={`#/projects/${p.slug}`}
                style={css.link}
                onClick={(e) => {
                  e.preventDefault();
                  onOpen(p.slug);
                }}
              >
                {p.name}
              </a>
            </td>
            <td style={{ ...css.td, ...css.muted }}>{p.portfolio || "—"}</td>
            <td style={css.td}>
              <Visibility value={p.visibility} />
            </td>
            <td style={css.td}>{p.status}</td>
            <td style={{ ...css.td, ...css.muted }}>{p.primary_owner}</td>
            <td style={css.td}>{p.repositories}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** The project slug in the URL fragment, or null on the portfolio view. */
function slugFromHash(): string | null {
  const m = /^#\/projects\/(.+)$/.exec(window.location.hash);
  const raw = m?.[1];
  return raw ? decodeURIComponent(raw) : null;
}

export function App() {
  const [me, setMe] = useState<Identity | null>(null);
  const [slug, setSlug] = useState<string | null>(() => slugFromHash());

  useEffect(() => {
    api.me().then(setMe).catch(() => setMe(null));
  }, []);

  useEffect(() => {
    const onHash = () => setSlug(slugFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  const open = (s: string) => {
    window.location.hash = `#/projects/${encodeURIComponent(s)}`;
    setSlug(s);
  };
  const back = () => {
    window.location.hash = "";
    setSlug(null);
  };

  return (
    <main style={css.page}>
      <header style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "1rem", marginBottom: "2rem" }}>
        <a href="#" style={{ ...css.link, fontSize: "1rem" }} onClick={(e) => { e.preventDefault(); back(); }}>
          Datopian Workgraph
        </a>
        <span style={{ ...css.muted, fontSize: "0.85rem" }}>
          {me ? me.email || me.subject : "not signed in"}
        </span>
      </header>

      {slug ? <ProjectPage slug={slug} onBack={back} /> : (
        <>
          <h1 style={{ marginBottom: "0.15rem" }}>Projects</h1>
          <p style={{ ...css.muted, marginTop: 0, marginBottom: "1.5rem" }}>
            Everything you have access to. What you cannot see is not listed.
          </p>
          <Portfolio onOpen={open} />
        </>
      )}
    </main>
  );
}
