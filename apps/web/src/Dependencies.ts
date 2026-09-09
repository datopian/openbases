/**
 * Laying out a dependency graph so it can be read.
 *
 * Kept apart from the rendering because it is the part with decisions in it,
 * and the only part worth testing: layering, cycles, and what counts as ready.
 */

/** One bead as the layout needs it. */
export interface DepNode {
  bead: string;
  title: string;
  status: string;
  blockedBy: string[];
  /** What happened to it, when something has. */
  outcome?: string;
}

export type Readiness =
  /** Closed. Nothing is waiting on it any more. */
  | "done"
  /** Open, and every blocker is closed: this can start now. */
  | "ready"
  /** Open, with at least one open blocker. */
  | "blocked"
  /**
   * Dispatched already, and it did not deliver.
   *
   * The state this whole field exists for. A run that exits zero and leaves
   * the bead open is an agent reporting it could not finish, and a run that
   * failed outright is worse -- and both used to be drawn as `ready`, in
   * green, identical to work nobody had touched. Somebody looking at the
   * graph to decide what to do next would pick it up again and get the same
   * nothing.
   *
   * Ranked ahead of readiness deliberately: whether a bead is startable
   * matters less than whether starting it has already been tried.
   */
  | "attention"
  /** Named as a blocker but not in this project's work. */
  | "external";

export interface Placed {
  bead: string;
  title: string;
  status: string;
  readiness: Readiness;
  /** Layer, counted from the work that can start now. */
  layer: number;
  /** Position within the layer. */
  row: number;
  blockedBy: string[];
}

export interface Layout {
  nodes: Placed[];
  layers: number;
  /** Beads with no dependency either way, which the graph does not draw. */
  isolated: string[];
  /**
   * Edges dropped to break a cycle, as "a -> b".
   *
   * Reported rather than swallowed. A dependency cycle is a mistake in the
   * graph -- nothing can start -- and a layout that quietly ignored one edge
   * would draw a tidy picture of an impossible plan.
   */
  brokeCycle: string[];
}

/**
 * Arrange beads into layers, where a bead sits after everything it waits on.
 *
 * LONGEST path from the ready work, not shortest. With shortest, a bead whose
 * two blockers are three layers apart would be drawn beside the nearer one and
 * its other edge would point backwards -- which reads as "this can start" when
 * it cannot. Longest path is what makes every arrow point one way.
 */
export function layout(nodes: DepNode[]): Layout {
  const byId = new Map(nodes.map((n) => [n.bead, n]));

  // Only the connected part is drawn. A project with fifty beads and twelve in
  // a dependency chain would otherwise show thirty-eight isolated dots, and
  // the chain -- the only thing a graph is for -- would be lost among them.
  const hasEdge = new Set<string>();
  for (const n of nodes) {
    for (const b of n.blockedBy) {
      hasEdge.add(n.bead);
      hasEdge.add(b);
    }
  }

  // A bead that has already been tried and delivered nothing is drawn even
  // with no edges.
  //
  // The reason for hiding an unconnected bead is noise: thirty-eight dots
  // would drown a chain of twelve. That reason does not apply here, and the
  // reason against hiding it is stronger -- sa-iyu ran for 5m45s, delivered
  // nothing, has no dependencies, and was therefore not in the graph AT ALL.
  // The one bead a reader most needed to see was the one the noise rule
  // removed.
  const needsAttention = (n: DepNode) =>
    !isClosed(n.status) && (n.outcome === "blocked" || n.outcome === "failed");
  for (const n of nodes) {
    if (needsAttention(n)) hasEdge.add(n.bead);
  }

  const isolated = nodes.filter((n) => !hasEdge.has(n.bead)).map((n) => n.bead);

  // Blockers named but not present: another project's work, or a cross-graph
  // reference that arrives as external:<prefix>:<id>. Drawn, because an arrow
  // from nowhere is worse than a node saying "not in this project".
  const external = new Set<string>();
  for (const n of nodes) {
    for (const b of n.blockedBy) if (!byId.has(b)) external.add(b);
  }

  const ids = [...hasEdge].filter((id) => byId.has(id) || external.has(id));
  const blockers = (id: string) =>
    (byId.get(id)?.blockedBy ?? []).filter((b) => ids.includes(b));

  // Longest-path layering, with an explicit cycle guard.
  //
  // A depth-first walk with a "currently visiting" set: an edge back into the
  // current path is a cycle, and it is dropped and reported rather than
  // followed. Without that guard this recursion does not terminate, and a
  // dependency cycle is a thing a person can create in Beads.
  const layer = new Map<string, number>();
  const visiting = new Set<string>();
  const brokeCycle: string[] = [];

  const depth = (id: string): number => {
    const known = layer.get(id);
    if (known !== undefined) return known;
    if (visiting.has(id)) {
      // Reported by the caller's edge, not here: this frame does not know
      // which edge closed the loop.
      return 0;
    }
    visiting.add(id);
    let d = 0;
    for (const b of blockers(id)) {
      if (visiting.has(b)) {
        brokeCycle.push(`${b} -> ${id}`);
        continue;
      }
      d = Math.max(d, depth(b) + 1);
    }
    visiting.delete(id);
    layer.set(id, d);
    return d;
  };
  for (const id of ids) depth(id);

  const readiness = (id: string): Readiness => {
    const n = byId.get(id);
    if (!n) return "external";
    if (isClosed(n.status)) return "done";
    // Before readiness, because "this was already tried and produced nothing"
    // is more useful than "this could be started".
    if (n.outcome === "blocked" || n.outcome === "failed") return "attention";
    // Ready means every blocker is CLOSED, which is what `bd ready` means. A
    // blocker this page cannot see is treated as unfinished: claiming work is
    // ready on the strength of a status nobody knows is the wrong way to be
    // wrong.
    const open = n.blockedBy.filter((b) => {
      const dep = byId.get(b);
      return !dep || !isClosed(dep.status);
    });
    return open.length === 0 ? "ready" : "blocked";
  };

  // Ordered within a layer by readiness then id, so the eye finds startable
  // work first and the arrangement does not move between renders.
  // attention first: a bead that has already been tried and delivered nothing
  // is the thing a reader most needs to see.
  const rank: Record<Readiness, number> = {
    attention: 0,
    ready: 1,
    blocked: 2,
    external: 3,
    done: 4,
  };
  const placed: Placed[] = ids
    .map((id) => {
      const n = byId.get(id);
      return {
        bead: id,
        title: n?.title ?? "",
        status: n?.status ?? "",
        readiness: readiness(id),
        layer: layer.get(id) ?? 0,
        row: 0,
        blockedBy: blockers(id),
      };
    })
    .sort(
      (a, b) =>
        a.layer - b.layer ||
        rank[a.readiness] - rank[b.readiness] ||
        a.bead.localeCompare(b.bead),
    );

  const seen = new Map<number, number>();
  for (const p of placed) {
    const row = seen.get(p.layer) ?? 0;
    p.row = row;
    seen.set(p.layer, row + 1);
  }

  return {
    nodes: placed,
    layers: placed.length ? Math.max(...placed.map((p) => p.layer)) + 1 : 0,
    isolated,
    brokeCycle: [...new Set(brokeCycle)],
  };
}

/**
 * Whether a bead's status counts as finished.
 *
 * Beads writes `closed`, and `done` appears in some graphs. Both mean the
 * blocker is lifted, and treating an unrecognised status as finished would
 * report work as ready when it is not -- so anything else is unfinished.
 */
export function isClosed(status: string): boolean {
  const s = status.toLowerCase();
  return s === "closed" || s === "done";
}
