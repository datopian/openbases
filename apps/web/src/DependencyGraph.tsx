import { useMemo, useState } from "react";
import type { ProjectWorkItem } from "./api";
import { layout, type Placed, type Readiness } from "./Dependencies";

/**
 * What a bead's readiness looks like.
 *
 * Colour is never the only carrier: every node shows its id and every state is
 * named in the legend, because a graph whose meaning is in its hues is a graph
 * some readers cannot use.
 */
const look: Record<Readiness, { fill: string; stroke: string; text: string; label: string }> = {
  attention: {
    fill: "#fdecea",
    stroke: "#c62828",
    text: "#8a1c1c",
    label: "ran and did not deliver",
  },
  ready: { fill: "#e8f5e9", stroke: "#2e7d32", text: "#1b5e20", label: "can start now" },
  blocked: { fill: "#fff8e1", stroke: "#b8860b", text: "#6b4e00", label: "waiting on other work" },
  done: { fill: "#f2f2f2", stroke: "#9e9e9e", text: "#5f5f5f", label: "closed" },
  external: { fill: "#fff", stroke: "#c9c9c9", text: "#777", label: "not in this project" },
};

// Node and spacing geometry. One place, so the arrows and the boxes cannot
// disagree about where a node is.
const NODE_W = 168;
const NODE_H = 52;
const GAP_X = 92;
const GAP_Y = 20;
const PAD = 16;

const x = (layer: number) => PAD + layer * (NODE_W + GAP_X);
const y = (row: number) => PAD + row * (NODE_H + GAP_Y);

/**
 * The project's work, drawn as what-waits-on-what.
 *
 * Layered left to right: the first column is work that can start now, and each
 * column after it waits on the one before. That is the question a dependency
 * graph is asked -- what can we start, and what does finishing this release --
 * so the layout answers it directly rather than making a reader trace a
 * force-directed cloud.
 *
 * Hand-drawn SVG rather than a graph library. The layered layout is about
 * eighty lines, a library would be the largest dependency in the bundle for a
 * page most people open occasionally, and every graph library's default -- a
 * force-directed blob -- is the wrong picture for an ordering question.
 */
export function DependencyGraph({ work }: { work: ProjectWorkItem[] }) {
  const [hover, setHover] = useState<string | null>(null);

  const plan = useMemo(
    () =>
      layout(
        work.map((w) => ({
          bead: w.bead,
          title: w.title ?? "",
          status: w.status ?? "",
          blockedBy: w.blocked_by ?? [],
          outcome: w.outcome,
        })),
      ),
    [work],
  );

  if (plan.nodes.length === 0) {
    // Said rather than drawn as an empty box: "no dependencies" is a fact
    // about the project, and an empty frame reads as a failure to load.
    return (
      <p style={{ color: "#666", fontSize: "0.9rem" }}>
        No dependencies between these beads, so there is no ordering to draw.
        Every bead can be worked on independently.
      </p>
    );
  }

  const rows = Math.max(...plan.nodes.map((n) => n.row)) + 1;
  const width = x(plan.layers - 1) + NODE_W + PAD;
  const height = y(rows - 1) + NODE_H + PAD;
  const at = new Map(plan.nodes.map((n) => [n.bead, n]));

  // An edge is lit when either end is hovered, so following a chain is
  // possible without clicking.
  const lit = (from: string, to: string) =>
    hover !== null && (hover === from || hover === to);

  const ready = plan.nodes.filter((n) => n.readiness === "ready");
  const attention = plan.nodes.filter((n) => n.readiness === "attention");

  return (
    <div>
      <p style={{ color: "#666", fontSize: "0.9rem", marginTop: 0 }}>
        Left to right: the first column can start now, and each column waits on
        the one before it.
        {ready.length > 0 && (
          <>
            {" "}
            <strong>{ready.length}</strong> bead{ready.length === 1 ? "" : "s"}{" "}
            can start now.
          </>
        )}
        {attention.length > 0 && (
          // Said in the summary as well as drawn, because the point of the
          // colour is to be noticed and a reader who scrolled past it should
          // still be told.
          <>
            {" "}
            <strong style={{ color: "#8a1c1c" }}>
              {attention.length} ran already and delivered nothing
            </strong>
            ; dispatching {attention.length === 1 ? "it" : "them"} again will
            probably do the same until the reason is fixed.
          </>
        )}
      </p>

      {plan.brokeCycle.length > 0 && (
        // Loud, because nothing in a cycle can ever start and the picture
        // cannot show that: an arrow had to be dropped to draw it at all.
        <p
          style={{
            color: "#8a1c1c",
            background: "#fdeaea",
            border: "1px solid #f3c2c2",
            borderRadius: "6px",
            padding: "0.5rem 0.7rem",
            fontSize: "0.85rem",
          }}
        >
          <strong>These beads depend on each other in a loop</strong>, so none
          of them can start: {plan.brokeCycle.join(", ")}. One arrow per loop is
          left out of the drawing; the loop itself has to be broken in Beads.
        </p>
      )}

      <div style={{ overflowX: "auto", paddingBottom: "0.5rem" }}>
        <svg
          width={width}
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          role="img"
          aria-label={`Dependency graph: ${plan.nodes.length} beads in ${plan.layers} stages`}
          style={{ display: "block", minWidth: "100%" }}
        >
          <defs>
            <marker
              id="dep-arrow"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto-start-reverse"
            >
              <path d="M 0 0 L 10 5 L 0 10 z" fill="#9aa4ae" />
            </marker>
            <marker
              id="dep-arrow-lit"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto-start-reverse"
            >
              <path d="M 0 0 L 10 5 L 0 10 z" fill="#0b5cad" />
            </marker>
          </defs>

          {/* Edges first, so a node always sits on top of its own arrows. */}
          {plan.nodes.flatMap((n) =>
            n.blockedBy.map((b) => {
              const from = at.get(b);
              if (!from) return null;
              const x1 = x(from.layer) + NODE_W;
              const y1 = y(from.row) + NODE_H / 2;
              const x2 = x(n.layer);
              const y2 = y(n.row) + NODE_H / 2;
              // A cubic curve with horizontal control points: it leaves and
              // arrives level, so the direction is legible where the arrow
              // meets the box even when the two rows are far apart.
              const dx = Math.max(28, (x2 - x1) / 2);
              const on = lit(b, n.bead);
              return (
                <path
                  key={`${b}->${n.bead}`}
                  d={`M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`}
                  fill="none"
                  stroke={on ? "#0b5cad" : "#c8d0d8"}
                  strokeWidth={on ? 2 : 1.25}
                  markerEnd={`url(#${on ? "dep-arrow-lit" : "dep-arrow"})`}
                />
              );
            }),
          )}

          {plan.nodes.map((n) => (
            <Node
              key={n.bead}
              node={n}
              dim={hover !== null && hover !== n.bead && !touches(plan.nodes, hover, n.bead)}
              onEnter={() => setHover(n.bead)}
              onLeave={() => setHover(null)}
            />
          ))}
        </svg>
      </div>

      <div
        style={{
          display: "flex",
          gap: "1rem",
          flexWrap: "wrap",
          fontSize: "0.8rem",
          color: "#555",
        }}
      >
        {(Object.keys(look) as Readiness[]).map((k) => (
          <span key={k} style={{ display: "inline-flex", alignItems: "center", gap: "0.35rem" }}>
            <span
              style={{
                width: "0.7rem",
                height: "0.7rem",
                borderRadius: "3px",
                background: look[k].fill,
                border: `1.5px solid ${look[k].stroke}`,
              }}
            />
            {look[k].label}
          </span>
        ))}
      </div>

      {plan.isolated.length > 0 && (
        <p style={{ color: "#666", fontSize: "0.85rem" }}>
          {plan.isolated.length} bead{plan.isolated.length === 1 ? "" : "s"} with
          no dependency either way {plan.isolated.length === 1 ? "is" : "are"} not
          drawn: {plan.isolated.join(", ")}.
        </p>
      )}
    </div>
  );
}

/** Whether two beads are directly connected, in either direction. */
function touches(nodes: Placed[], a: string, b: string): boolean {
  return nodes.some(
    (n) =>
      (n.bead === a && n.blockedBy.includes(b)) ||
      (n.bead === b && n.blockedBy.includes(a)),
  );
}

function Node({
  node,
  dim,
  onEnter,
  onLeave,
}: {
  node: Placed;
  dim: boolean;
  onEnter: () => void;
  onLeave: () => void;
}) {
  const c = look[node.readiness];
  return (
    <g
      transform={`translate(${x(node.layer)}, ${y(node.row)})`}
      opacity={dim ? 0.35 : 1}
      onMouseEnter={onEnter}
      onMouseLeave={onLeave}
      style={{ cursor: node.readiness === "external" ? "default" : "pointer" }}
    >
      <title>
        {node.bead}
        {node.title ? ` — ${node.title}` : ""}
        {`\n${c.label}`}
        {node.blockedBy.length > 0 ? `\nwaiting on ${node.blockedBy.join(", ")}` : ""}
      </title>
      <rect
        width={NODE_W}
        height={NODE_H}
        rx={8}
        fill={c.fill}
        stroke={c.stroke}
        strokeWidth={1.5}
        strokeDasharray={node.readiness === "external" ? "4 3" : undefined}
      />
      {/* The id is a link, so the graph is a way into a bead rather than a
          picture beside one. Rendered through foreignObject because an SVG
          <a> cannot carry the app's own styling and hash routing reliably. */}
      <foreignObject width={NODE_W} height={NODE_H}>
        <div
          style={{
            font: "600 0.78rem ui-monospace, monospace",
            padding: "7px 9px 0",
            color: c.text,
            whiteSpace: "nowrap",
            overflow: "hidden",
            textOverflow: "ellipsis",
          }}
        >
          {node.readiness === "external" ? (
            node.bead
          ) : (
            <a
              href={`#/work/${encodeURIComponent(node.bead)}`}
              style={{ color: c.text, textDecoration: "none" }}
            >
              {node.bead}
            </a>
          )}
          <div
            style={{
              font: "400 0.72rem system-ui, sans-serif",
              color: "#5b5b5b",
              overflow: "hidden",
              textOverflow: "ellipsis",
              marginTop: "1px",
            }}
          >
            {node.readiness === "external" ? "not in this project" : node.title || "untitled"}
          </div>
        </div>
      </foreignObject>
    </g>
  );
}
