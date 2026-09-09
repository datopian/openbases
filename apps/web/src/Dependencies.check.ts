/**
 * Checks the dependency layout. Run by CI; see the `web` job.
 *
 * A plain script compiled with the tsc that is already here, rather than a test
 * runner. The layout is pure -- no DOM, no fetch -- so it needs nothing but a
 * way to call it, and vitest would be a new devDependency for one file. The
 * repository's own convention is a small script that checks one thing and says
 * what broke.
 */
// The .js extension is what emitted ESM needs at run time; TypeScript
// resolves it to the .ts source and Vite does the same.
import { layout, isClosed, type DepNode } from "./Dependencies.js";

const problems: string[] = [];
function check(what: string, ok: boolean, detail = "") {
  if (!ok) problems.push(`${what}${detail ? `: ${detail}` : ""}`);
}

const node = (
  bead: string,
  blockedBy: string[] = [],
  status = "open",
  outcome?: string,
): DepNode => ({
  bead,
  title: bead + " title",
  status,
  blockedBy,
  outcome,
});

// A chain lays out in order, one bead per stage.
{
  const l = layout([node("a"), node("b", ["a"]), node("c", ["b"])]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;
  check("a chain has three stages", l.layers === 3, `got ${l.layers}`);
  check("the unblocked bead is first", at("a").layer === 0, `got ${at("a").layer}`);
  check("the last bead is last", at("c").layer === 2, `got ${at("c").layer}`);
}

// LONGEST path, not shortest.
//
// d waits on a directly AND on c, which is two stages further along. Laid out
// by shortest path it would sit beside b and its arrow from c would point
// backwards -- which reads as startable when it is not.
{
  const l = layout([
    node("a"),
    node("b", ["a"]),
    node("c", ["b"]),
    node("d", ["a", "c"]),
  ]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;
  check(
    "a bead sits after its furthest blocker",
    at("d").layer === 3,
    `d is at layer ${at("d").layer}, wanted 3 (after c at ${at("c").layer})`,
  );
  for (const n of l.nodes) {
    for (const b of n.blockedBy) {
      const from = l.nodes.find((x) => x.bead === b)!;
      check(
        "every arrow points forwards",
        from.layer < n.layer,
        `${b} (layer ${from.layer}) -> ${n.bead} (layer ${n.layer})`,
      );
    }
  }
}

// A cycle terminates, is reported, and still draws.
//
// Without the visiting guard this recursion does not return, so the page would
// hang rather than render -- and a person can create a cycle in Beads.
{
  const l = layout([node("a", ["c"]), node("b", ["a"]), node("c", ["b"])]);
  check("a cycle still produces a layout", l.nodes.length === 3, `got ${l.nodes.length}`);
  check("the broken edge is reported", l.brokeCycle.length >= 1, "nothing reported");
}

// Readiness is about the blockers' STATUS, not their presence. This is what
// `bd ready` means, and the reason the graph is worth looking at.
{
  const l = layout([node("done-1", [], "closed"), node("next", ["done-1"])]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;
  check("a closed blocker does not block", at("next").readiness === "ready", at("next").readiness);
  check("a closed bead reads as done", at("done-1").readiness === "done", at("done-1").readiness);
}
{
  const l = layout([node("open-1"), node("next", ["open-1"])]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;
  check("an open blocker blocks", at("next").readiness === "blocked", at("next").readiness);
  check("the blocker itself is ready", at("open-1").readiness === "ready", at("open-1").readiness);
}

// A blocker this page cannot see is treated as unfinished. Calling work ready
// on the strength of a status nobody knows is the wrong way to be wrong.
{
  const l = layout([node("here", ["external:sa:sa-9"])]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;
  check("an unknown blocker blocks", at("here").readiness === "blocked", at("here").readiness);
  check(
    "the unknown blocker is drawn as external",
    at("external:sa:sa-9")?.readiness === "external",
    String(at("external:sa:sa-9")?.readiness),
  );
}

// Beads with no dependency either way are listed, not drawn: forty isolated
// dots would drown the twelve that form a chain.
{
  const l = layout([node("a"), node("b", ["a"]), node("lonely")]);
  check("an isolated bead is not drawn", !l.nodes.some((n) => n.bead === "lonely"));
  check("an isolated bead is reported", l.isolated.includes("lonely"), l.isolated.join(","));
}

// Both spellings of finished, because treating an unrecognised status as
// closed would report work ready when it is not.
check("closed counts as closed", isClosed("closed") && isClosed("CLOSED"));
check("done counts as closed", isClosed("done"));
check("open does not", !isClosed("open") && !isClosed("in_progress"));


// A bead that has already been tried and delivered nothing is not "ready".
//
// This is the state the field exists for. sa-iyu ran for 5m45s, exited zero,
// delivered nothing and left its bead open -- and the graph drew it green,
// identical to work nobody had touched, so the obvious next action was to
// dispatch it again and get the same nothing.
//
// The blockers here are CLOSED on purpose. An earlier version of this fixture
// gave `fresh` an open blocker and then asserted it was ready, which it is
// not -- the assertion was wrong rather than the code, and it only surfaced
// once the checks below the report block started running at all.
{
  const l = layout([
    // Finished, so it blocks nothing.
    node("shut-1", [], "closed"),
    // Tried and delivered nothing. Unconnected, and drawn anyway: the bead
    // that most needs seeing was the one the "hide isolated beads" rule
    // removed.
    node("tried", [], "open", "blocked"),
    node("failed-1", [], "open", "failed"),
    // Blocked only by finished work, so these are startable and their
    // outcome decides how they read.
    node("fresh", ["shut-1"], "open", "never_dispatched"),
    node("landed-1", ["shut-1"], "open", "landed"),
    // A ready bead in layer 0, so the ordering check has something to
    // compare against. Without one its loop body never ran and it passed
    // with the ranking reversed.
    node("ready-0", [], "open", "never_dispatched"),
    node("needs-ready-0", ["ready-0"]),
    // Unconnected with nothing wrong: still hidden.
    node("lonely-ok", [], "open", "never_dispatched"),
  ]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;

  check("a run that delivered nothing needs attention",
    at("tried").readiness === "attention", at("tried").readiness);
  check("a failed run needs attention",
    at("failed-1").readiness === "attention", at("failed-1").readiness);
  check("an unconnected bead needing attention is still drawn",
    !!at("failed-1"), "it was left out of the graph");
  check("an unconnected bead with nothing wrong is still hidden",
    !l.nodes.some((n) => n.bead === "lonely-ok") && l.isolated.includes("lonely-ok"),
    l.isolated.join(","));
  check("an untouched bead behind finished work is ready",
    at("fresh").readiness === "ready", at("fresh").readiness);
  // `landed` is work awaiting review, not work that stalled -- calling it
  // attention would cry wolf on every open pull request.
  check("work in a pull request is not flagged",
    at("landed-1").readiness === "ready", at("landed-1").readiness);

  // Attention sorts before ready WITHIN a layer, because whether starting a
  // bead has already been tried matters more than whether it can start.
  //
  // Asserted as a property over the layer rather than between two named
  // beads: a comparison of two rows is satisfiable by accident.
  for (const l0 of l.nodes.filter((n) => n.layer === 0 && n.readiness === "ready")) {
    const worse = l.nodes.filter(
      (n) => n.layer === 0 && n.readiness === "attention" && n.row > l0.row,
    );
    check("attention sorts above ready in the same layer", worse.length === 0,
      `${worse.map((n) => n.bead).join(",")} sit below ready ${l0.bead}`);
  }
}

// A closed bead stays done even if its last run reported nothing: the work is
// finished, whatever the run said.
{
  const l = layout([node("shut", [], "closed", "blocked"), node("x", ["shut"])]);
  const at = (id: string) => l.nodes.find((n) => n.bead === id)!;
  check("a closed bead is done regardless of its last run",
    at("shut").readiness === "done", at("shut").readiness);
}

// The report goes LAST, and that is not cosmetic.
//
// It used to sit in the middle: checks appended after it pushed their
// problems into a list nobody read again, and the script printed success and
// exited zero with failures recorded. Every check added below that line was
// unfailable, which took a deliberate mutation of the ranking to notice --
// the same shape of bug this file exists to catch.
if (problems.length > 0) {
  for (const p of problems) console.error(`  ${p}`);
  // Thrown rather than process.exit, which would need @types/node for one
  // call. An uncaught throw is a non-zero exit, which is what CI reads.
  throw new Error(`${problems.length} problem(s) in the dependency layout`);
}
console.log("the dependency layout orders, breaks cycles, and knows what is ready");
