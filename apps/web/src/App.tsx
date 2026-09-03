import { useEffect, useState } from "react";
import { Ask } from "./Ask";
import { Inbox } from "./Inbox";
import { Work } from "./Work";
import { Device, deviceCodeFromHash } from "./Device";
import { Platform } from "./Platform";
import { CreateProject, ManageRepositories } from "./ProjectAdmin";
import {
  age,
  api,
  asList,
  isStale,
  type Identity,
  type ProjectDetail,
  type ProjectSummary,
  type RepositoryStatus,
  type ProjectWorkItem,
  type VersionInfo,
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
  td: {
    padding: "0.7rem 0.75rem 0.7rem 0",
    borderBottom: "1px solid #f0f0f0",
    verticalAlign: "top",
  } as const,
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
    <span
      style={{
        color: tone[value] ?? "#444",
        fontWeight: 600,
        fontSize: "0.85rem",
      }}
    >
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
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          gap: "1rem",
          flexWrap: "wrap",
        }}
      >
        <strong>{signal.name}</strong>
        <span
          title={signal.observed_at ?? "no observation recorded"}
          style={{
            ...css.muted,
            fontSize: "0.85rem",
            color: stale ? "#a33" : "#666",
          }}
        >
          {signal.observed_at
            ? `observed ${age(signal.observed_at)}`
            : "never observed"}
          {stale && signal.observed_at ? " — may be out of date" : ""}
        </span>
      </div>
      <div style={{ fontSize: "1.1rem", margin: "0.2rem 0 0.4rem" }}>
        {signal.value}
      </div>
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
            <div
              style={{
                display: "flex",
                justifyContent: "space-between",
                gap: "1rem",
                flexWrap: "wrap",
              }}
            >
              <strong>{r.full_name}</strong>
              <span
                style={{ ...css.muted, fontSize: "0.85rem" }}
                title={r.last_projected ?? ""}
              >
                {r.last_projected
                  ? `last activity ${age(r.last_projected)}`
                  : "never reported"}
              </span>
            </div>
            {prs.length === 0 ? (
              <p
                style={{
                  ...css.muted,
                  fontSize: "0.9rem",
                  margin: "0.4rem 0 0",
                }}
              >
                {r.last_projected
                  ? "No pull requests recorded."
                  : "Nothing has been received for this repository yet. That may mean no activity, or that the integration is not delivering."}
              </p>
            ) : (
              <table
                style={{
                  width: "100%",
                  borderCollapse: "collapse",
                  marginTop: "0.5rem",
                }}
              >
                <tbody>
                  {prs.slice(0, 8).map((pr) => (
                    <tr key={pr.number}>
                      <td style={{ ...css.td, width: "3.5rem" }}>
                        #{pr.number}
                      </td>
                      <td style={css.td}>{pr.title}</td>
                      <td style={{ ...css.td, width: "6rem" }}>{pr.state}</td>
                      <td style={{ ...css.td, width: "7rem" }}>
                        {pr.checks_state
                          ? `checks: ${pr.checks_state}`
                          : "checks: none"}
                      </td>
                      <td
                        style={{ ...css.td, width: "7rem", ...css.muted }}
                        title={pr.updated_at}
                      >
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
  const [detail, setDetail] = useState<
    (ProjectDetail & { repositories: RepositoryStatus[] }) | null
  >(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setDetail(null);
    setError(null);
    api
      .projectDetail(slug)
      .then(setDetail)
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      );
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
      {error && (
        <p style={{ color: "#a33" }}>Could not load this project: {error}</p>
      )}
      {!detail && !error && <p style={css.muted}>Loading…</p>}
      {detail && (
        <>
          <h1 style={{ marginBottom: "0.15rem" }}>{detail.name}</h1>
          <p style={{ ...css.muted, marginTop: 0 }}>
            {detail.portfolio ? `${detail.portfolio} · ` : ""}
            <Visibility value={detail.visibility} /> · {detail.status} · owner{" "}
            {detail.primary_owner}
            {detail.backup_owner ? ` (backup ${detail.backup_owner})` : ""}
          </p>

          <h2 style={{ fontSize: "1rem", marginTop: "1.75rem" }}>Status</h2>
          {asList(detail.signals).map((s) => (
            <SignalCard key={s.name} signal={s} />
          ))}

          <h2 style={{ fontSize: "1rem", marginTop: "1.75rem" }}>Work</h2>
          <ProjectWork work={asList(detail.work)} slug={detail.slug} />

          <h2 style={{ fontSize: "1rem", marginTop: "1.75rem" }}>
            Repositories
          </h2>
          <Repositories repos={asList(detail.repositories)} />
          <ManageRepositories slug={detail.slug} />
        </>
      )}
    </>
  );
}

function Portfolio({ onOpen }: { onOpen: (slug: string) => void }) {
  const [projects, setProjects] = useState<ProjectSummary[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // What the API actually handed back, described in one string.
  //
  // "No projects are visible to you" and "the response was not the shape this
  // code expects" look identical on screen, and telling them apart otherwise
  // costs a round trip through the browser console with whoever is looking at
  // it. The empty state says which it is.
  const [received, setReceived] = useState<string>("");

  // Which portfolio the reader is looking at, remembered across visits.
  //
  // The home page listed every project, which for a company with client
  // engagements, open-source projects, products and prospect work means the
  // question "how are the clients doing" is answered by a table where most
  // rows are not clients. The filter is per portfolio because that is the
  // grouping the schema actually has -- there are no subprojects.
  //
  // localStorage rather than a URL parameter: it is a per-reader preference
  // rather than a thing to share, and a link that carries somebody else's
  // filter is worse than one that carries none. Wrapped in try/catch because a
  // private window throws on access rather than returning empty.
  const [portfolio, setPortfolio] = useState<string>(() => {
    try {
      return window.localStorage.getItem("wg.portfolio") ?? "";
    } catch {
      return "";
    }
  });
  const choosePortfolio = (p: string) => {
    setPortfolio(p);
    try {
      window.localStorage.setItem("wg.portfolio", p);
    } catch {
      // A reader who cannot store a preference still gets to use the filter
      // for this visit, which is the part that matters.
    }
  };

  useEffect(() => {
    api
      .projectsRaw()
      .then((raw) => {
        setReceived(describe(raw));
        setProjects(asList((raw as { projects?: ProjectSummary[] })?.projects));
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      );
  }, []);

  if (error)
    return <p style={{ color: "#a33" }}>Could not load projects: {error}</p>;
  if (!projects) return <p style={css.muted}>Loading…</p>;
  const all = asList(projects);
  // Portfolios present in what came back, rather than a hardcoded list: a new
  // portfolio should appear here without a release, and one with no projects
  // should not offer a filter that yields an empty table.
  const portfolios = Array.from(
    new Set(all.map((p) => p.portfolio).filter((p) => p.length > 0)),
  ).sort();
  // An unknown remembered value shows everything rather than nothing. A
  // portfolio can be renamed or emptied between visits, and a filter that
  // silently hides every project is indistinguishable from a broken page.
  const active = portfolios.includes(portfolio) ? portfolio : "";
  const rows = active === "" ? all : all.filter((p) => p.portfolio === active);

  const filter =
    portfolios.length > 1 ? (
      <p style={{ margin: "0 0 0.9rem", fontSize: "0.85rem" }}>
        <span style={{ ...css.muted, marginRight: "0.6rem" }}>Portfolio</span>
        {[{ key: "", label: `All (${all.length})` }]
          .concat(
            portfolios.map((p) => ({
              key: p,
              label: `${p} (${all.filter((x) => x.portfolio === p).length})`,
            })),
          )
          .map((opt) => (
            <a
              key={opt.key}
              href="#"
              onClick={(e) => {
                e.preventDefault();
                choosePortfolio(opt.key);
              }}
              style={{
                marginRight: "0.75rem",
                textDecoration: "none",
                fontWeight: opt.key === active ? 700 : 400,
                color: opt.key === active ? "#1a1a1a" : "#0b5cad",
              }}
            >
              {opt.label}
            </a>
          ))}
      </p>
    ) : null;
  // The create form sits above both the table and the empty state, because the
  // case where somebody most needs it is the one where there is nothing to
  // list. A newly created project is prepended rather than refetched: it is
  // already in hand, and a refetch would make the thing you just made appear
  // after a delay, which reads like it did not work.
  const created = (p: ProjectSummary) => setProjects([p, ...asList(projects)]);

  if (rows.length === 0) {
    // Empty because of permissions is a different thing from empty because
    // nothing exists, and the reader cannot tell from a blank table.
    return (
      <>
        <CreateProject onCreated={created} />
        {filter}
        <p style={css.muted}>
          {active !== ""
            ? `No projects in the ${active} portfolio.`
            : "No projects are visible to you. That may be because none exist yet, or because you are not a member of any."}
          <br />
          <span style={{ fontSize: "0.75rem" }}>
            received: {received || "nothing"}
          </span>
        </p>
      </>
    );
  }

  return (
    <>
      <CreateProject onCreated={created} />
      {filter}
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
    </>
  );
}

/**
 * The project's beads (wg-43n).
 *
 * The global Work page has always shown these; opening a project said nothing
 * about them, which is what "no visibility of beads in existing projects"
 * meant. A bead reaches this list by carrying a project:<slug> label, or by
 * living in a cell that serves exactly one project.
 */
export function ProjectWork({
  work,
  slug,
}: {
  work: ProjectWorkItem[];
  slug: string;
}) {
  const rows = asList(work);
  if (rows.length === 0) {
    // Empty here is ambiguous in a way worth spelling out: it can mean the
    // project genuinely has no beads, or that its beads exist and carry no
    // project label. The second case looked exactly like the first for eighty
    // beads, so the empty state names it.
    return (
      <p style={css.muted}>
        No beads are attributed to this project. Either none exist yet, or they
        exist in a shared cell without a <code>project:{slug}</code> label —
        attribution comes from the bead, not from the cell, whenever a cell
        serves more than one project.
      </p>
    );
  }
  const open = rows.filter((w) => w.status !== "closed").length;
  return (
    <>
      <p style={{ ...css.muted, fontSize: "0.8rem", marginTop: 0 }}>
        {rows.length} bead{rows.length === 1 ? "" : "s"}, {open} still open
      </p>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={css.th}>Bead</th>
            <th style={css.th}>Title</th>
            <th style={css.th}>Kind</th>
            <th style={css.th}>Status</th>
            <th style={css.th}>Seen</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((w) => (
            <tr key={w.bead}>
              <td
                style={{
                  ...css.td,
                  fontFamily: "ui-monospace, monospace",
                  fontSize: "0.8rem",
                }}
              >
                {w.bead}
              </td>
              <td style={css.td}>
                {w.title || <span style={css.muted}>untitled</span>}
              </td>
              <td style={{ ...css.td, ...css.muted }}>{w.kind || "—"}</td>
              <td style={css.td}>{w.status || "—"}</td>
              <td
                style={{ ...css.td, ...css.muted, fontSize: "0.8rem" }}
                title={w.last_seen ?? "never projected"}
              >
                {age(w.last_seen)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

/** The project slug in the URL fragment, or null on the portfolio view. */
function slugFromHash(): string | null {
  const m = /^#\/projects\/(.+)$/.exec(window.location.hash);
  const raw = m?.[1];
  return raw ? decodeURIComponent(raw) : null;
}

function onInbox(): boolean {
  return window.location.hash === "#/inbox";
}

function onAsk(): boolean {
  return window.location.hash === "#/ask";
}

function onWork(): boolean {
  return window.location.hash === "#/work";
}

function onPlatform(): boolean {
  return window.location.hash === "#/platform";
}

/**
 * A visible build stamp.
 *
 * "The page is empty" and "your browser is running last week's JavaScript" look
 * identical from the outside, and settling which one it is otherwise costs a
 * round trip through DevTools with the person at the screen. The bundle's own
 * URL is the one thing that cannot be stale relative to the code reading it.
 */
/** A one-line description of an unknown value, for the empty state. */
function describe(v: unknown): string {
  if (v === null) return "null";
  if (v === undefined) return "undefined";
  if (Array.isArray(v)) return `array(${v.length})`;
  if (typeof v === "object")
    return `object{${Object.keys(v as object)
      .slice(0, 5)
      .join(",")}}`;
  return `${typeof v}`;
}

function buildStamp(): string {
  const script = document.querySelector<HTMLScriptElement>(
    'script[src*="/assets/"]',
  );
  const src = script?.src ?? "";
  const m = /index-([A-Za-z0-9_-]+)\.js/.exec(src);
  return m?.[1] ?? "unknown";
}

export function App() {
  const [me, setMe] = useState<Identity | null>(null);
  const [version, setVersion] = useState<VersionInfo | null>(null);
  const [slug, setSlug] = useState<string | null>(() => slugFromHash());
  const [inbox, setInbox] = useState<boolean>(() => onInbox());
  const [ask, setAsk] = useState<boolean>(() => onAsk());
  const [work, setWork] = useState<boolean>(() => onWork());
  // null when the fragment is not a device link; "" when it is one with no
  // code, so the page can offer a field instead of an error.
  const [device, setDevice] = useState<string | null>(() =>
    deviceCodeFromHash(),
  );
  const [platform, setPlatform] = useState<boolean>(() => onPlatform());

  useEffect(() => {
    api
      .me()
      .then(setMe)
      .catch(() => setMe(null));
    api
      .version()
      .then(setVersion)
      .catch(() => setVersion(null));
  }, []);

  useEffect(() => {
    const onHash = () => {
      setSlug(slugFromHash());
      setInbox(onInbox());
      setAsk(onAsk());
      setWork(onWork());
      setPlatform(onPlatform());
      setDevice(deviceCodeFromHash());
    };
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
    setInbox(false);
    setAsk(false);
    setWork(false);
    setPlatform(false);
  };

  return (
    <main style={css.page}>
      <header
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "baseline",
          gap: "1rem",
          marginBottom: "2rem",
        }}
      >
        <a
          href="#"
          style={{ ...css.link, fontSize: "1rem" }}
          onClick={(e) => {
            e.preventDefault();
            back();
          }}
        >
          Datopian Workgraph
        </a>
        <span style={{ fontSize: "0.85rem" }}>
          <a
            href="#/work"
            style={{ ...css.link, marginRight: "1rem" }}
            onClick={(e) => {
              e.preventDefault();
              window.location.hash = "#/work";
              setWork(true);
              setInbox(false);
              setAsk(false);
              setPlatform(false);
              setSlug(null);
            }}
          >
            Work
          </a>
          <a
            href="#/inbox"
            style={{ ...css.link, marginRight: "1rem" }}
            onClick={(e) => {
              e.preventDefault();
              window.location.hash = "#/inbox";
              setInbox(true);
              setAsk(false);
              setWork(false);
              setPlatform(false);
              setSlug(null);
            }}
          >
            Needs you
          </a>
          <a
            href="#/ask"
            style={{ ...css.link, marginRight: "1rem" }}
            onClick={(e) => {
              e.preventDefault();
              window.location.hash = "#/ask";
              setAsk(true);
              setInbox(false);
              setWork(false);
              setPlatform(false);
              setSlug(null);
            }}
          >
            Ask
          </a>
          <a
            href="#/platform"
            style={{ ...css.link, marginRight: "1rem" }}
            onClick={(e) => {
              e.preventDefault();
              window.location.hash = "#/platform";
              setPlatform(true);
              setAsk(false);
              setInbox(false);
              setWork(false);
              setSlug(null);
            }}
          >
            Platform
          </a>
          <span style={css.muted}>
            {me ? me.email || me.subject : "not signed in"}
          </span>
        </span>
      </header>

      {device !== null ? (
        <Device code={device} />
      ) : platform ? (
        <Platform />
      ) : work ? (
        <Work />
      ) : ask ? (
        <Ask />
      ) : inbox ? (
        <Inbox />
      ) : slug ? (
        <ProjectPage slug={slug} onBack={back} />
      ) : (
        <>
          <h1 style={{ marginBottom: "0.15rem" }}>Projects</h1>
          <p style={{ ...css.muted, marginTop: 0, marginBottom: "1.5rem" }}>
            Everything you have access to. What you cannot see is not listed.
          </p>
          <Portfolio onOpen={open} />
        </>
      )}
      <footer
        style={{
          ...css.muted,
          fontSize: "0.75rem",
          marginTop: "3rem",
          paddingTop: "0.75rem",
          borderTop: "1px solid #eee",
        }}
      >
        interface {buildStamp()}
        {version
          ? ` · api ${version.version} (${version.commit.slice(0, 8)}) · ${version.env}`
          : ""}
      </footer>
    </main>
  );
}
