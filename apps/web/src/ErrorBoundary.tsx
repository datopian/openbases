import { Component, type ErrorInfo, type ReactNode } from "react";

/**
 * Catch render errors and show them.
 *
 * Without this, any exception thrown during render unmounts the whole tree and
 * leaves a blank white page — no message, nothing in the interface to report,
 * and the only evidence in a console the person looking at it may never open.
 * That happened on the first staging deploy, and the cost was not the bug: it
 * was that the failure carried no information at all.
 *
 * Showing the message is deliberate. This is an internal tool behind Cloudflare
 * Access, so the reader is a colleague who can act on a stack trace, not an
 * anonymous visitor to be protected from implementation detail.
 */
interface State {
  error: Error | null;
  info: string;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  override state: State = { error: null, info: "" };

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo) {
    // Also log it, so it is in the console for anyone who does open one.
    console.error("interface crashed", error, info);
    this.setState({ info: info.componentStack ?? "" });
  }

  override render() {
    const { error, info } = this.state;
    if (!error) return this.props.children;

    return (
      <main
        style={{
          fontFamily: "system-ui, sans-serif",
          maxWidth: "48rem",
          margin: "3rem auto",
          padding: "0 1.5rem",
          lineHeight: 1.55,
        }}
      >
        <h1 style={{ fontSize: "1.25rem" }}>The interface hit an error</h1>
        <p style={{ color: "#555" }}>
          This is a bug in the page, not in your access or your data. The details below are what to
          report.
        </p>
        <pre
          style={{
            background: "#f6f6f6",
            border: "1px solid #e3e3e3",
            borderRadius: "6px",
            padding: "1rem",
            overflowX: "auto",
            fontSize: "0.85rem",
            whiteSpace: "pre-wrap",
          }}
        >
          {error.name}: {error.message}
          {info ? `\n${info}` : ""}
        </pre>
        <button
          type="button"
          onClick={() => window.location.reload()}
          style={{ padding: "0.5rem 0.9rem", fontSize: "0.9rem", cursor: "pointer" }}
        >
          Reload
        </button>
      </main>
    );
  }
}
