import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { ErrorBoundary } from "./ErrorBoundary";

const root = document.getElementById("root");
if (!root) throw new Error("#root is missing from index.html");

// A last-resort handler for anything thrown outside React's render cycle.
//
// The error boundary catches render errors, but a failure during module
// evaluation or in an async callback never reaches it and leaves the same blank
// page. Writing the message into the document means the person looking at the
// screen can report it without being asked to open a console.
function fatal(message: string) {
  if (root && root.childElementCount === 0) {
    root.textContent = `The interface failed to start: ${message}`;
    root.setAttribute(
      "style",
      "font-family:system-ui,sans-serif;max-width:48rem;margin:3rem auto;padding:0 1.5rem;color:#a33",
    );
  }
}
window.addEventListener("error", (e) => fatal(e.message));
window.addEventListener("unhandledrejection", (e) =>
  fatal(String((e.reason as Error)?.message ?? e.reason)),
);

createRoot(root).render(
  <StrictMode>
    <ErrorBoundary>
      <App />
    </ErrorBoundary>
  </StrictMode>,
);
