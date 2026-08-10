import { useEffect, useState } from "react";
import { api, type VersionInfo } from "./api";

/**
 * Bootstrap shell.
 *
 * The portfolio home, attention inbox, project page, knowledge review inbox and
 * chief-of-staff interface are delivered by WP-F1, WP-F2, WP-F3 and WP-H3. This
 * shell exists so that the build, type check and deployment path are real from
 * the first commit rather than assembled later.
 */
export function App() {
  const [version, setVersion] = useState<VersionInfo | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .version()
      .then(setVersion)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  return (
    <main
      style={{
        fontFamily: "system-ui, sans-serif",
        maxWidth: "48rem",
        margin: "4rem auto",
        padding: "0 1.5rem",
        lineHeight: 1.6,
      }}
    >
      <h1 style={{ marginBottom: "0.25rem" }}>Datopian Workgraph</h1>
      <p style={{ color: "#555", marginTop: 0 }}>
        Shared, permissioned company control plane.
      </p>

      <section aria-labelledby="status">
        <h2 id="status" style={{ fontSize: "1rem" }}>
          Build
        </h2>
        {error && (
          <p role="alert" style={{ color: "#b00" }}>
            Control API unreachable: {error}
          </p>
        )}
        {version && (
          <dl style={{ display: "grid", gridTemplateColumns: "8rem 1fr", gap: "0.25rem 1rem" }}>
            <dt>Environment</dt>
            <dd style={{ margin: 0 }}>{version.env}</dd>
            <dt>Version</dt>
            <dd style={{ margin: 0 }}>{version.version}</dd>
            <dt>Commit</dt>
            <dd style={{ margin: 0 }}>
              <code>{version.commit}</code>
            </dd>
          </dl>
        )}
        {!version && !error && <p>Loading…</p>}
      </section>

      <section aria-labelledby="next">
        <h2 id="next" style={{ fontSize: "1rem" }}>
          Not yet implemented
        </h2>
        <p>
          Portfolio home, attention inbox, project pages, the knowledge review inbox and the
          chief-of-staff interface are tracked as work packages WP-F1 to WP-F3 and WP-H3. This
          screen deliberately shows only what the API can actually prove.
        </p>
      </section>
    </main>
  );
}
