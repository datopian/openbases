// Command wg-dispatcher is the execution node's side of the work queue.
//
// The control plane cannot reach here — this node has no inbound port, and it
// talks to the control API outward through Cloudflare Access. So a dispatch is
// not something the node receives, it is a row the node comes and claims. This
// polls for one, runs it with wg-runner, and reports what happened.
//
// It also pushes the cell's bead state up on every pass. work_refs has existed
// since 0001 as the projection of Beads into the control plane and nothing ever
// filled it, so the interface could show projects and repositories but never the
// work itself. A process already talking to the control plane every few seconds
// is the cheapest thing to fill it with.
//
// One job at a time, deliberately. Concurrency is bounded by the budget's
// max_concurrent_agents (wg-726), and a dispatcher that ran two jobs at once
// would have to reimplement that bound rather than inherit it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/datopian/openbases/internal/check"
	"github.com/datopian/openbases/internal/gastown"
	"github.com/datopian/openbases/internal/landing"
	"github.com/datopian/openbases/internal/runner"
	"github.com/datopian/openbases/internal/version"
	"github.com/datopian/openbases/internal/work"
)

func main() {
	var (
		apiURL   = flag.String("api", os.Getenv("WG_CONTROL_API"), "control API base URL")
		cell     = flag.String("cell", getenv("WG_CELL", "oss"), "this cell's slug")
		cellRoot = flag.String("cell-root", "", "the cell's home, e.g. /srv/cells/oss")
		rig      = flag.String("rig", getenv("WG_RIG", "sandbox"), "default rig")
		runner   = flag.String("runner", "/usr/local/bin/wg-runner", "path to the agent runner")
		// Pinned by versions.lock and installed with the toolchain. Needed
		// only to create a rig the town does not have yet.
		gtBinary = flag.String("gt", getenv("WG_GT_BINARY", "/usr/local/bin/gt"), "path to the pinned gt binary")
		// The same default and the same environment variable the runner uses,
		// so the two read one file. A dispatcher pointed at a different
		// catalogue would report a harness the run did not use, which is worse
		// than reporting none.
		catalogue = flag.String("catalogue", getenv("WG_MODEL_CATALOGUE", "/etc/workgraph/models.json"),
			"role and model tables, read to report what a run uses")
		interval = flag.Duration("interval", 10*time.Second, "how often to look for work")
		deadline = flag.Duration("deadline", 3*time.Hour,
			"absolute ceiling on one job; a backstop against a loop, not a budget")
		stall = flag.Duration("stall", 10*time.Minute,
			"stop a run that has written nothing -- no output, no file changed -- for this long")
		once = flag.Bool("once", false, "do a single pass and exit")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *cellRoot == "" {
		*cellRoot = "/srv/cells/" + *cell
	}

	id, secret := os.Getenv("WG_ACCESS_CLIENT_ID"), os.Getenv("WG_ACCESS_CLIENT_SECRET")
	if *apiURL == "" || id == "" || secret == "" {
		log.Error("no control API credentials; expected WG_CONTROL_API and the Access client id and secret")
		os.Exit(2)
	}

	d := &dispatcher{
		api: strings.TrimSuffix(*apiURL, "/"), clientID: id, clientSecret: secret,
		cell: *cell, cellRoot: *cellRoot, rig: *rig, runner: *runner,
		gtBinary:  *gtBinary,
		catalogue: *catalogue,
		deadline:  *deadline, stall: *stall, log: log,
		http: &http.Client{Timeout: 60 * time.Second},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "build", version.String(), "cell", *cell, "api", d.api)

	if *once {
		d.pass(ctx)
		return
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	d.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Info("stopped cleanly")
			return
		case <-ticker.C:
			d.pass(ctx)
		}
	}
}

type dispatcher struct {
	api, clientID, clientSecret string
	cell, cellRoot, rig, runner string
	gtBinary                    string
	// catalogue is the role/model table the runner reads, so the dispatcher can
	// report what a run will use without reimplementing the lookup.
	catalogue string
	deadline  time.Duration
	// How long a run may produce NOTHING before it is stopped. See the run
	// loop for why this, and not elapsed time, is the limit that matters.
	stall time.Duration
	// How often the run loop looks. A field so a test can look often enough
	// to observe a run WHILE it is producing; 30s in production.
	tick time.Duration
	http *http.Client
	log  *slog.Logger
}

// pass projects the cell's beads upward, then claims and runs at most one job.
//
// Projection first, so that a bead an agent created a moment ago is visible
// before anything else happens to it. Getting this the other way round meant a
// freshly planned bead appeared in the interface only after the NEXT pass, which
// reads as the planner having done nothing.
func (d *dispatcher) pass(ctx context.Context) {
	// What this rig holds, so the control plane can route a dispatch to a rig
	// that can do the work or refuse it (wg-ugb).
	//
	// Reported by the node because the node is where the truth is: the rig is a
	// working tree and its git remote says what it is a checkout of. Configuring
	// it centrally as well would be two statements of one fact, and the one that
	// drifts is always the one nobody looks at.
	//
	// EVERY rig in the town, not just the one the dispatcher was configured
	// with. A cell now gets a rig per repository its projects hold, and a rig
	// that never registers is a rig dispatch refuses to route to: registering
	// only the default meant eleven of the oss cell's twelve rigs were
	// invisible to routing however correctly they were provisioned.
	rigs := d.rigs()
	for _, rig := range rigs {
		if err := d.registerRig(ctx, rig); err != nil {
			// Not fatal. A pass that cannot register still projects beads and
			// still runs work; the cost is that dispatch cannot route to that
			// rig until the next successful pass.
			d.log.Warn("registering a rig", "rig", rig, "error", err)
		}
	}

	if err := d.project(ctx, rigs); err != nil {
		d.log.Error("projecting beads", "error", err)
	}

	job, err := d.claim(ctx)
	if err != nil {
		d.log.Error("claiming work", "error", err)
		return
	}
	if job == nil {
		return
	}
	if err := job.Validate(); err != nil {
		d.log.Error("refusing an unrunnable job", "job", job.ID, "error", err)
		d.report(ctx, job.ID, work.Result{OK: false, Output: err.Error()})
		return
	}

	// The rig may not exist yet, and this is where it gets made.
	//
	// Routing sends work to the rig the cell SHOULD hold, not only to one it
	// already has, so that attaching a repository and dispatching against it
	// does not need a deploy in between. The clone happens here, once, when
	// work has actually been asked for -- which is the objection to doing it on
	// attach: nothing should start cloning a dozen repositories because
	// somebody attached one from a phone.
	if err := d.ensureRig(ctx, job); err != nil {
		d.log.Error("creating the rig for a job", "job", job.ID, "rig", job.Rig, "error", err)
		// Reported as the job's failure rather than swallowed: the work cannot
		// run, and a job left claimed with no explanation is worse than one
		// that says the checkout could not be made.
		d.report(ctx, job.ID, work.Result{OK: false,
			Output: "the repository for this work could not be checked out: " + err.Error()})
		return
	}

	// The beads that existed before the run, so the ones it creates can be
	// identified afterwards and labelled with the job's project (wg-sjm).
	//
	// This replaces asking the agent to do it. internal/work's planning prompt
	// says "Label every bead you create with `wg-project-<slug>`", and on
	// 4 September a plan job produced three beads with no labels at all: the
	// instruction was followed by nobody and the beads were unattributable, so
	// no project page could show them.
	//
	// The project is known exactly when the job is enqueued -- work_queue
	// carries it, verified against the requester's membership -- and routing a
	// fact like that through a prompt and hoping it comes back is not a
	// mechanism. It governs who may read the work.
	//
	// The prompt still asks, because a bead the agent labels itself is not
	// wrong and costs nothing. Nothing depends on it.
	//
	// The job's OWN rig: the run works in the rig that holds the repository the
	// bead is about, and the beads it creates land in that rig's graph, not in
	// the dispatcher's default one.
	jobRig := d.rigFor(*job)
	before := d.beadIDs(ctx, jobRig)

	// And what the working tree already held, for the same reason: so that
	// what the run did can be told apart from what was lying about. gt leaves
	// an untracked .gitignore in a rig whose repository has none, and portaljs
	// carried sa-kfh's uncommitted change for two days -- either would
	// otherwise arrive in a pull request attributed to a bead that did not
	// make it.
	// And the checkout is brought up to date first, so the run works against
	// what is in the repository now rather than whatever was there when the rig
	// was created or last landed.
	d.refresh(jobRig)

	tree := d.treeBefore(jobRig)

	// Tell the control plane what this run is using, before it starts.
	//
	// Reported here rather than left to be inferred later: the only other
	// evidence of which model did the work is usage_records, which the cost
	// importer fills hourly, so a bead in progress showed no model at all and
	// the harness was recorded nowhere. Somebody watching a run they are
	// waiting on is exactly who asks.
	d.reportPlan(ctx, *job)

	d.log.Info("running", "job", job.ID, "kind", job.Kind, "bead", job.Bead)
	out, runErr := d.run(ctx, *job, "")

	// Does it work? The agent cannot answer that -- it has no shell -- so the
	// repository's own build or test command is run here, and if it fails the
	// agent gets one more pass with the output.
	//
	// One extra pass, not a loop. An agent that could not fix it the first
	// time is usually not one pass away, and each pass costs a full run: the
	// two on sa-kfh were 15 and 40 model calls. A second failure is reported
	// rather than ground away at.
	checked := d.check(ctx, *job, jobRig)
	if checked != nil && !checked.OK && runErr == nil {
		d.log.Info("the check failed; giving the agent the output",
			"job", job.ID, "bead", job.Bead, "command", checked.Command)
		second, secondErr := d.run(ctx, *job, checked.Feedback())
		out += "\n\n--- the check failed, so the agent was given the output and ran again ---\n\n" + second
		runErr = secondErr
		checked = d.check(ctx, *job, jobRig)
	}

	// Labelled whether the run succeeded or not. A job that produced beads and
	// then failed still produced beads, and an unattributed bead is invisible
	// on the project page it belongs to.
	if p := strings.TrimSpace(job.Project); p != "" {
		d.labelNewBeads(ctx, jobRig, before, p)
	}

	// Put whatever the run changed on a branch and open a pull request for it.
	//
	// Before this, a run that changed code closed its bead with the change
	// stranded in the rig's working tree: sa-kfh renamed a hero tab label,
	// closed correctly, and left `M site/components/home/LandingHero.tsx` on
	// the node with no commit and nothing in the interface saying so.
	//
	// After the run rather than by the agent, because the agent cannot run git
	// at all -- its tools are Read, Grep, Glob, Edit, Write and `Bash(bd:*)`,
	// and the deny list names `git push`. An agent that could push could
	// force-push main.
	//
	// Attempted whether or not the run succeeded. A run that changed code and
	// then failed still changed code, and stranding that is the bug being
	// fixed; the pull request body says which it was, so a reviewer is not
	// misled about how finished it is.
	if job.Kind == work.KindWork {
		d.landWork(ctx, *job, jobRig, out, tree, checked, runErr)
	}

	d.report(ctx, job.ID, work.Result{OK: runErr == nil, Output: out})
	if runErr != nil {
		d.log.Error("job failed", "job", job.ID, "error", runErr)
	} else {
		d.log.Info("job done", "job", job.ID)
	}
}

// rigFor is the rig a job runs in: the one dispatch chose, or the default when
// it named none. Routing chooses the rig by repository now (0084), so an empty
// one means an older control plane or a job that needs no working tree.
func (d *dispatcher) rigFor(job work.Job) string {
	if r := strings.TrimSpace(job.Rig); r != "" {
		return r
	}
	return d.rig
}

// run invokes wg-runner, which owns the working directory, the settings, the
// deadline and the teardown. Nothing about how an agent is started belongs here.
func (d *dispatcher) run(ctx context.Context, job work.Job, extra string) (string, error) {
	rig := d.rigFor(job)
	// A plan job has no bead of its own, so it needs a name for its run
	// directory and its cost attribution. The job id is the honest one: the
	// spend belongs to the act of planning, not to any bead it produces.
	bead := job.Bead
	if bead == "" {
		bead = "plan-" + job.ID
	}

	// The rig's canonical working tree, which is what `gt rig add` calls the
	// refinery. Told to the agent rather than left to be found: routing now
	// guarantees this rig holds the repository the bead's project owns, so the
	// path is known here and an agent that has to search finds whatever else
	// is in reach instead.
	job.Checkout = d.cellRoot + "/town/" + rig + "/refinery/rig"
	instructions := job.Instructions()
	if e := strings.TrimSpace(extra); e != "" {
		// Appended, not substituted: the second pass is the same job with what
		// the check said added to it, so the bead, the acceptance criteria and
		// the checkout are all still in front of the agent.
		instructions += "\n\n" + e
	}

	args := []string{
		"-bead", bead,
		"-cell", d.cell,
		"-rig", rig,
		"-cell-root", d.cellRoot,
		"-role", job.Role(),
		"-instructions", instructions,
		"-deadline", d.deadline.String(),
	}
	// The gateway token comes from the cell's own agent settings, which is
	// where the gastown role put it — the same place scripts/dispatch_bead.sh
	// reads it from, and for the same reason: read here rather than passed in,
	// so a dispatch cannot run with a different token than the cell's own
	// agents use.
	//
	// wg-runner refuses without it, and that refusal is the point. A run with
	// no gateway does not fail; it succeeds straight against the provider,
	// untagged, unmetered and outside every budget — the one failure this whole
	// chain exists to prevent.
	token, err := d.gatewayToken()
	if err != nil {
		return "", err
	}

	// Cancellable independently of ctx, so a stalled run can be stopped
	// without tearing down the dispatcher's own pass.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	cmd := exec.CommandContext(runCtx, d.runner, args...)
	// SIGTERM, not the SIGKILL that CommandContext sends by default.
	//
	// wg-runner registers its teardown before it creates anything and runs it
	// on every path including a signal -- but only a signal it can catch.
	// Under SIGKILL the teardown does not run and the agent it started is
	// orphaned: the process that was stopped for being stalled keeps running,
	// with nothing watching it, which is worse than the stall.
	//
	// WaitDelay is the fallback. A runner that ignores SIGTERM is killed after
	// it, and -- the reason it matters here -- Wait stops blocking on output
	// pipes an orphaned grandchild still holds open. Without it a stalled run
	// hung this dispatcher until the agent's own sleep finished, which the
	// test for this caught by hanging for two minutes.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	// Appended rather than replacing the environment, because the runner also
	// needs PATH and HOME from the unit. The token is passed to one child and
	// is never logged; wg-runner writes it into a settings file outside the run
	// directory so the agent itself cannot read it back.
	cmd.Env = append(os.Environ(), "WG_AI_GATEWAY_TOKEN="+token)

	// Started rather than run to completion, so the run can be reported on
	// while it happens.
	//
	// CombinedOutput blocks until the process exits, which is why a two-hour
	// run and a wedged one looked identical from the control plane: it heard
	// nothing between claim and finish. The output is still collected in full
	// and returned exactly as before -- what changes is that its SIZE and the
	// time of the last write are observable meanwhile, and those two facts are
	// the difference between "thinking" and "stuck".
	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Thirty seconds: frequent enough that a stalled run is obvious within a
	// minute, rare enough that a two-hour run costs 240 requests rather than
	// one per second. The dispatcher already talks to the control plane on
	// every pass, so this is the same connection doing more.
	every := d.tick
	if every <= 0 {
		every = 30 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()

	// A run ends when it STOPS PRODUCING, not when a clock runs out.
	//
	// The deadline used to be elapsed time, and it was the wrong measurement.
	// Three of sa-7dc's four runs were killed by it in the middle of real
	// work: one had installed dependencies and written a portal and was
	// stopped at 15 minutes, and the pull request it left was reported as a
	// failure. Meanwhile the run that deserved stopping -- thirty minutes
	// during which it wrote no file at all, only `find` over other rigs --
	// was allowed to use its whole allowance, because elapsed time cannot
	// tell those two apart.
	//
	// Progress is two facts, and either one counts: the agent wrote something
	// to its output, or it changed a file in its checkout. A run that is doing
	// neither for as long as `stall` is not thinking, and every extra minute
	// is spend against a bead that is not moving.
	//
	// The absolute ceiling stays, far above any real task, because "produces
	// something every few minutes" is also the shape of a loop. It is a
	// backstop rather than a budget now: reaching it is a bug report, not a
	// normal outcome.
	progress := func() (string, int) {
		_, at := newestChange(job.Checkout)
		return at.String(), buf.len()
	}
	lastMark, lastBytes := progress()
	lastMoved := time.Now()

	for {
		select {
		case err := <-done:
			return strings.TrimSpace(buf.String()), err
		case <-tick.C:
			mark, bytes := progress()
			if mark != lastMark || bytes != lastBytes {
				lastMark, lastBytes, lastMoved = mark, bytes, time.Now()
			}

			idle := time.Since(lastMoved)
			if d.stall > 0 && idle >= d.stall {
				// Said in the log AND carried into the heartbeat, so the
				// reason survives wherever somebody looks first. A run that
				// simply stops with no explanation is what the old deadline
				// did, and it took reading a transcript to find out why.
				d.log.Warn("stopping a stalled run",
					"job", job.ID, "bead", job.Bead,
					"idle", idle.Round(time.Second), "stall", d.stall)
				d.heartbeat(ctx, job.ID, fmt.Sprintf(
					"stopped: nothing written for %s (no output, no file changed)",
					idle.Round(time.Second)))
				cancelRun()

				// The stall is the reason, always -- wrapped around whatever
				// Wait reports rather than deferred to it. Wait describes the
				// mechanism ("signal: terminated"), which is what we just did
				// to it and says nothing about why. The first version only
				// substituted this when Wait returned nil, so every real stop
				// reached the bead as "signal: terminated" and the operator
				// had to guess. The test caught it.
				return strings.TrimSpace(buf.String()), fmt.Errorf(
					"stalled: nothing written for %s -- no output and no file "+
						"changed in the checkout (%v)",
					idle.Round(time.Second), <-done)
			}
			d.heartbeat(ctx, job.ID, buf.progress()+touched(job.Checkout))
		}
	}
}

// syncBuffer collects the run's output and can be read while it is being
// written.
//
// A plain bytes.Buffer would race: the child's output arrives on the exec
// package's goroutine and the heartbeat reads from this one. Not a hypothetical
// -- the race detector runs in CI and would have caught it, which is a slower
// way to learn it than a mutex.
type syncBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	last time.Time
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = time.Now()
	return b.buf.Write(p)
}

// len is the bytes written so far, for comparing one tick against the next.
func (b *syncBuffer) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// progress is what the node can honestly say about the run's OUTPUT.
//
// Bytes written and how long since the last write. Read on its own this is
// misleading for the default runtime, which is why touched() exists beside it:
// opencode buffers its stdout and delivers it at exit, so a healthy opencode
// run sits at "186 B of output, last wrote 7m ago" for its whole length. That
// is exactly what sa-7dc's first run reported while it was installing
// dependencies and writing a portal.
//
// The original comment here said "silence is real evidence: an agent that has
// produced nothing for ten minutes is not thinking out loud". True of claude,
// which streams. Not true of opencode, and opencode is what polecat runs.
func (b *syncBuffer) progress() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.buf.Len()
	if b.last.IsZero() {
		return "no output yet"
	}
	return fmt.Sprintf("%s of output, last wrote %s ago",
		humanBytes(n), time.Since(b.last).Round(time.Second))
}

// touched reports how recently the run changed a file in its checkout.
//
// The liveness signal that works for BOTH runtimes. An agent's whole job is to
// change files, so the newest mtime under its checkout is evidence about the
// work itself rather than about how its harness happens to buffer stdout.
// Returns "" rather than a guess when it cannot tell, so a caller appends
// nothing instead of appending something reassuring.
//
// Bounded deliberately:
//
//	.git is skipped -- git writes to it constantly, so including it would
//	report every run as busy, which is the failure this replaces in the
//	opposite direction;
//
//	anything on landing.Ephemeral is skipped, because `npm install` touches
//	tens of thousands of files and walking them each half-minute would cost
//	more than the run;
//
//	the walk stops after 20,000 entries, so a large repository slows the
//	heartbeat rather than stalling the dispatcher.
func touched(checkout string) string {
	name, at := newestChange(checkout)
	if at.IsZero() {
		return ""
	}
	return fmt.Sprintf("; wrote %s %s ago", name, time.Since(at).Round(time.Second))
}

// newestChange is the most recently modified file under the checkout, and
// when. Zero time when there is nothing to report.
//
// Split out from touched so the dispatcher can COMPARE it between ticks rather
// than only print it. That comparison is what decides whether a run is working
// or wedged, which is the thing a deadline should be measuring.
func newestChange(checkout string) (string, time.Time) {
	if strings.TrimSpace(checkout) == "" {
		return "", time.Time{}
	}
	var newest time.Time
	var name string
	seen := 0
	_ = filepath.WalkDir(checkout, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(checkout, path)
		if relErr != nil {
			return nil
		}
		if e.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || matchesEphemeral(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if seen++; seen > 20000 {
			return filepath.SkipAll
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest, name = info.ModTime(), rel
		}
		return nil
	})
	return name, newest
}

// matchesEphemeral reports whether a path is inside a directory landing will
// not commit. Shared with the landing so the two cannot disagree about what
// counts as build output.
func matchesEphemeral(rel string) bool {
	for _, p := range landing.Ephemeral {
		if strings.HasPrefix(rel+"/", p) || strings.TrimSuffix(p, "/") == filepath.Base(rel) {
			return true
		}
	}
	return false
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// heartbeat tells the control plane the run is still alive.
//
// Best effort and never fatal: a heartbeat that fails must not end a run that
// is working. The reply says whether the control plane still considers the job
// live, and a false stops the reporting -- writing heartbeats into a job
// somebody has already cancelled is noise, and noise is what makes a signal
// like this stop being read.
func (d *dispatcher) heartbeat(ctx context.Context, jobID, note string) {
	if jobID == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"note": note})
	if err != nil {
		return
	}
	if _, err := d.call(ctx, http.MethodPost,
		"/v1/node/work/"+jobID+"/heartbeat", bytes.NewReader(body)); err != nil {
		d.log.Debug("reporting a heartbeat", "job", jobID, "error", err)
	}
}

// gatewayToken reads the cell's AI Gateway token out of its agent settings.
//
// The token is not stored on its own anywhere. It lives inside the
// ANTHROPIC_CUSTOM_HEADERS value that Claude Code sends, because the gateway
// only honours the credential when it arrives as a header on the request — an
// environment variable is ignored, and a run then reaches the gateway untagged.
func (d *dispatcher) gatewayToken() (string, error) {
	path := filepath.Join(d.cellRoot, ".claude", "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the cell's agent settings for the gateway token: %w", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	m := gatewayTokenPattern.FindStringSubmatch(settings.Env["ANTHROPIC_CUSTOM_HEADERS"])
	if len(m) != 2 || m[1] == "" {
		return "", fmt.Errorf("no AI Gateway token in %s; refusing to run, because a run without it "+
			"escapes every budget", path)
	}
	return m[1], nil
}

var gatewayTokenPattern = regexp.MustCompile(`cf-aig-authorization:\s*Bearer\s+(\S+)`)

// project reads the cell's beads and sends them to the control plane.
//
// Reading and sending are separate because only one of them can be tested
// without a cell: readBeads shells out to bd against a real Dolt database,
// while sendBeads is the part that has to get the endpoint right — and getting
// the endpoint wrong is what actually happened.
func (d *dispatcher) project(ctx context.Context, rigs []string) error {
	// One list from every rig's graph, because a bead graph is per-rig: each
	// rig has its own Dolt database and its own id prefix, and `bd list` in one
	// rig cannot see another's. A cell with twelve rigs projecting only the
	// first would show a twelfth of its work.
	//
	// Ids do not collide across rigs -- that is what the prefix is for -- so
	// the lists concatenate.
	var all []beadRow
	var failed []string
	for _, rig := range rigs {
		beads, err := d.readBeads(ctx, rig)
		if err != nil {
			// Reported and skipped rather than fatal. A rig whose graph is
			// mid-initialisation, or whose Dolt server is briefly down, must
			// not take the other rigs' beads out of the interface with it.
			d.log.Warn("reading a rig's beads", "rig", rig, "error", err)
			failed = append(failed, rig)
			continue
		}
		all = append(all, beads...)
	}
	if err := d.sendBeads(ctx, all); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not read %d of %d rigs: %s",
			len(failed), len(rigs), strings.Join(failed, ", "))
	}
	return nil
}

// rigs lists the town's rigs, read from disk.
//
// The directory is the truth, deliberately: `gt rig add` creates the directory
// and writes mayor/rigs.json, and a rig that exists on disk but is missing from
// the registry file is exactly the half-created case that most needs to be seen.
//
// A directory is a rig when its config.json SAYS it is a rig. The presence of
// the file is not enough, and this is not hypothetical: town/settings holds a
// config.json with "type": "town-settings", so the first version of this treated
// it as a rig and the staging dispatcher logged, every pass,
//
//	level=WARN msg="reading a rig's beads" rig=settings error="bd list: signal: killed"
//
// -- `bd` there does not fail, it HANGS until the deadline kills it, so a
// non-rig in the town costs a whole pass rather than a log line. It also
// registered `settings` as a rig holding no repository.
//
// The configured rig is always included, even if the town has no directory for
// it, so a cell whose town has not been built yet behaves as it did before.
func (d *dispatcher) rigs() []string {
	found := []string{}
	entries, err := os.ReadDir(d.cellRoot + "/town")
	if err != nil {
		d.log.Warn("reading the town", "error", err)
		return []string{d.rig}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !isRig(d.cellRoot + "/town/" + e.Name() + "/config.json") {
			continue
		}
		found = append(found, e.Name())
	}
	if !slices.Contains(found, d.rig) {
		found = append(found, d.rig)
	}
	return found
}

// isRig reports whether a config.json declares its directory to be a rig.
//
// A file that cannot be read or parsed is NOT a rig. That is the safe
// direction: a directory wrongly included is polled by bd on every pass and
// registered as routable, while one wrongly excluded is only invisible until
// somebody looks at the town.
func isRig(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var config struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return false
	}
	return config.Type == "rig"
}

// sendBeads projects a bead list upward. An empty list is not sent: it would be
// indistinguishable from a cell whose graph failed to open, and the control
// plane would then show the backlog as empty.
func (d *dispatcher) sendBeads(ctx context.Context, beads []beadRow) error {
	if len(beads) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"cell": d.cell, "beads": beads})
	if err != nil {
		return err
	}
	_, err = d.call(ctx, http.MethodPost, "/v1/node/work/project", bytes.NewReader(body))
	return err
}

type beadRow struct {
	Bead   string `json:"bead"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Labels, because project attribution comes from the bead now: a
	// project:<slug> label is what lets one cell serve several projects
	// (wg-43n). Omitted when empty, so an older control plane sees the payload
	// it always saw.
	Labels []string `json:"labels,omitempty"`
	// The most recent comment, when the bead has one (wg-m07).
	//
	// The agent's own words are the most useful artefact of a run — on
	// 4 September three runs exited 0, left their beads open, and explained in
	// a comment that they could not proceed — and they were reachable only by
	// reading the graph on this node. Carried up so a person can see them.
	// Blockers are the beads this one depends on, by id.
	//
	// bd has been returning these all along, in `dependencies` with a
	// depends_on_id and a type, and the projection read past them -- so
	// work_links held no rows in production and the project page showed work
	// that cannot start as though nobody had picked it up. Omitted when empty,
	// so an older control plane sees the payload it always saw.
	Blockers  []string `json:"blockers,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	CommentAt string   `json:"comment_at,omitempty"`
	CommentBy string   `json:"comment_by,omitempty"`
	// commentCount is bd's own count, used to decide whether to ask for the
	// comments at all. Not sent upward.
	commentCount int
}

// readBeads asks bd for the cell's beads as JSON.
func (d *dispatcher) readBeads(ctx context.Context, rig string) ([]beadRow, error) {
	cmd := exec.CommandContext(ctx, "bd", "list", "--all", "--json")
	cmd.Dir = d.cellRoot + "/town/" + rig
	cmd.Env = append(os.Environ(), "HOME="+d.cellRoot)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}

	// bd's JSON shape is its own, and it has changed between versions, so this
	// reads defensively: anything without an id is skipped rather than failing
	// the pass, because one odd row must not stop the rest being visible.
	var raw []map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		var wrapper struct {
			Issues []map[string]any `json:"issues"`
		}
		if err2 := json.Unmarshal(out, &wrapper); err2 != nil {
			return nil, fmt.Errorf("bd list returned neither a list nor {issues}: %w", err)
		}
		raw = wrapper.Issues
	}

	rows := make([]beadRow, 0, len(raw))
	for _, r := range raw {
		id, _ := r["id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		row := beadRow{Bead: id}
		row.Title, _ = r["title"].(string)
		row.Kind, _ = r["issue_type"].(string)
		if row.Kind == "" {
			row.Kind, _ = r["type"].(string)
		}
		row.Status, _ = r["status"].(string)
		if n, ok := r["comment_count"].(float64); ok {
			row.commentCount = int(n)
		}
		// Only `blocks` dependencies. bd also records `tracks`, which is a
		// reference rather than an ordering constraint -- treating it as a
		// blocker would show work as unstartable when it is merely related.
		if deps, ok := r["dependencies"].([]any); ok {
			for _, d := range deps {
				dep, ok := d.(map[string]any)
				if !ok {
					continue
				}
				if kind, _ := dep["type"].(string); kind != "blocks" {
					continue
				}
				on, _ := dep["depends_on_id"].(string)
				if strings.TrimSpace(on) != "" {
					row.Blockers = append(row.Blockers, on)
				}
			}
		}
		// bd omits the field entirely for a bead with no labels rather than
		// emitting an empty list, so absence has to be tolerated.
		if raw, ok := r["labels"].([]any); ok {
			for _, l := range raw {
				if s, ok := l.(string); ok && strings.TrimSpace(s) != "" {
					row.Labels = append(row.Labels, s)
				}
			}
		}
		rows = append(rows, row)
	}
	// The newest comment, for the beads that have one.
	//
	// One `bd show` per commented bead rather than per bead: on this cell that
	// is a handful out of thirty-odd, and a show for every bead on every
	// fifteen-second pass would be a real cost for a field that is usually
	// absent. bd's list output does not carry comments, only a count, which is
	// exactly the discriminator needed.
	for i := range rows {
		if rows[i].commentCount == 0 {
			continue
		}
		text, at, by, err := d.lastComment(ctx, rig, rows[i].Bead)
		if err != nil {
			// Not fatal. A bead whose comments cannot be read is still worth
			// projecting with its status; losing the whole pass over one
			// unreadable comment would be the wrong trade.
			d.log.Warn("reading comments", "bead", rows[i].Bead, "error", err)
			continue
		}
		rows[i].Comment, rows[i].CommentAt, rows[i].CommentBy = text, at, by
	}

	return rows, nil
}

// lastComment returns the newest comment on one bead.
func (d *dispatcher) lastComment(ctx context.Context, rig, bead string) (text, at, by string, err error) {
	cmd := exec.CommandContext(ctx, "bd", "show", bead, "--json")
	cmd.Dir = d.cellRoot + "/town/" + rig
	cmd.Env = append(os.Environ(), "HOME="+d.cellRoot)
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("bd show %s: %w", bead, err)
	}

	// bd show returns either an object or a single-element array, depending on
	// version. Both are read, for the same reason readBeads reads two shapes.
	var one map[string]any
	if err := json.Unmarshal(out, &one); err != nil {
		var many []map[string]any
		if err2 := json.Unmarshal(out, &many); err2 != nil || len(many) == 0 {
			return "", "", "", fmt.Errorf("bd show %s returned neither an object nor a list", bead)
		}
		one = many[0]
	}

	list, ok := one["comments"].([]any)
	if !ok || len(list) == 0 {
		return "", "", "", nil
	}
	// The LAST one. bd returns them oldest first, and the useful comment is the
	// most recent thing the agent said.
	c, ok := list[len(list)-1].(map[string]any)
	if !ok {
		return "", "", "", nil
	}
	text, _ = c["text"].(string)
	by, _ = c["author"].(string)
	at, _ = c["created_at"].(string)
	return text, at, by, nil
}

func (d *dispatcher) claim(ctx context.Context) (*work.Job, error) {
	body, err := d.call(ctx, http.MethodPost, "/v1/node/work/claim?cell="+d.cell, nil)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil // 204: nothing to do, which is the common case
	}
	var job work.Job
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (d *dispatcher) report(ctx context.Context, id string, res work.Result) {
	body, err := json.Marshal(res)
	if err != nil {
		d.log.Error("encoding a result", "job", id, "error", err)
		return
	}
	if _, err := d.call(ctx, http.MethodPost, "/v1/node/work/"+id+"/result", bytes.NewReader(body)); err != nil {
		// Loud, because a job stuck in `running` forever is how a queue quietly
		// stops. Nothing here can fix it; the control plane has to notice.
		d.log.Error("could not report a finished job; it will sit as running", "job", id, "error", err)
	}
}

func (d *dispatcher) call(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.api+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("CF-Access-Client-Id", d.clientID)
	req.Header.Set("CF-Access-Client-Secret", d.clientSecret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(out), 200))
	}
	// A rejected request does not arrive as a 4xx. Cloudflare Access answers an
	// unauthenticated request with 200 and its own login PAGE, so the status
	// check above passes and the HTML then fails to parse as JSON — which is
	// how this presented in the journal:
	//
	//	msg="claiming work" error="invalid character '<' looking for beginning of value"
	//
	// That says nothing about Access, nothing about which path, and nothing
	// about what to do, and it repeated every ten seconds. The real cause was
	// this client calling /v1/work instead of /v1/node/work: the right token
	// for the wrong application, which Access answers by asking a daemon to log
	// in. Naming it here costs one header check.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") && len(bytes.TrimSpace(out)) > 0 {
		return nil, fmt.Errorf("%s %s: expected JSON, got %q — this is Cloudflare Access serving its login page, "+
			"which means the service token was not accepted for this path", method, path, ct)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// beadIDs returns the ids currently in the rig's graph, as a set.
//
// Errors are swallowed to an empty set on purpose: this is used to work out
// which beads are NEW, and an empty "before" makes every bead look new. That is
// the safe direction — labelling a bead with the project of the job that just
// ran is at worst redundant, while missing one leaves it invisible. A wrong
// label is refused upstream anyway: system_project_bead raises if a bead names
// two projects.
func (d *dispatcher) beadIDs(ctx context.Context, rig string) map[string]bool {
	out := map[string]bool{}
	rows, err := d.readBeads(ctx, rig)
	if err != nil {
		d.log.Warn("listing beads before a run", "rig", rig, "error", err)
		return out
	}
	for _, r := range rows {
		out[r.Bead] = true
	}
	return out
}

// labelNewBeads puts the job's project on every bead that appeared during it.
func (d *dispatcher) labelNewBeads(ctx context.Context, rig string, before map[string]bool, project string) {
	rows, err := d.readBeads(ctx, rig)
	if err != nil {
		d.log.Error("listing beads after a run; new beads may be unattributed",
			"project", project, "error", err)
		return
	}

	label := "wg-project-" + project
	for _, r := range rows {
		if before[r.Bead] {
			continue
		}
		// Already labelled, by the agent following the prompt or by an earlier
		// pass. Skipping keeps the log quiet about work already done.
		if slices.Contains(r.Labels, label) {
			continue
		}
		// A bead that already names a DIFFERENT project is left alone and
		// reported. Adding ours would make it name two, which
		// system_project_bead refuses outright — better to say so here than to
		// break the whole projection pass.
		if other, ok := otherProjectLabel(r.Labels, label); ok {
			d.log.Warn("a new bead already names another project; leaving it alone",
				"bead", r.Bead, "has", other, "job_project", project)
			continue
		}
		cmd := exec.CommandContext(ctx, "bd", "update", r.Bead, "--add-label", label)
		cmd.Dir = d.cellRoot + "/town/" + rig
		cmd.Env = append(os.Environ(), "HOME="+d.cellRoot)
		if out, err := cmd.CombinedOutput(); err != nil {
			d.log.Error("labelling a new bead", "bead", r.Bead, "label", label,
				"error", err, "output", strings.TrimSpace(string(out)))
			continue
		}
		d.log.Info("labelled a new bead", "bead", r.Bead, "label", label)
	}
}

// otherProjectLabel reports a project label that is not the wanted one.
func otherProjectLabel(labels []string, wanted string) (string, bool) {
	for _, l := range labels {
		if l == wanted {
			continue
		}
		if strings.HasPrefix(l, "wg-project-") || strings.HasPrefix(l, "project:") {
			return l, true
		}
	}
	return "", false
}

// registerRig tells the control plane which repository one rig holds.
func (d *dispatcher) registerRig(ctx context.Context, rig string) error {
	owner, name := d.rigRepository(ctx, rig)
	body := map[string]any{"cell": d.cell, "rig": rig}
	if owner != "" && name != "" {
		body["provider"] = "github"
		body["owner"] = owner
		body["name"] = name
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = d.call(ctx, http.MethodPost, "/v1/node/rigs", bytes.NewReader(encoded))
	return err
}

// rigRepository reads the rig's git remote, or returns empty when it has none.
//
// A rig legitimately may have none — the witness and the mayor are rigs with no
// working tree — so "no remote" is reported as no repository rather than as an
// error. What must not happen is guessing: a rig whose repository is unknown is
// a rig dispatch will refuse to route to, which is the safe direction.
func (d *dispatcher) rigRepository(ctx context.Context, rig string) (owner, name string) {
	dir := d.cellRoot + "/town/" + rig
	// The bare repository beside the working tree is where the remote lives on
	// this layout; the working tree itself is a worktree of it.
	for _, gitDir := range []string{dir + "/.repo.git", dir} {
		cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "remote", "get-url", "origin")
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if o, n, ok := parseGitHubRemote(strings.TrimSpace(string(out))); ok {
			return o, n
		}
	}
	return "", ""
}

// parseGitHubRemote pulls owner and repository out of a git remote URL.
//
// Both forms, because a rig may be cloned either way and a remote that does not
// parse must report NOTHING rather than a partial guess: an owner without a
// name cannot be joined to project_repositories and would silently match no
// project, which looks identical to a rig that holds nothing.
func parseGitHubRemote(url string) (owner, name string, ok bool) {
	u := strings.TrimSuffix(strings.TrimSpace(url), ".git")
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/",
	} {
		if rest, found := strings.CutPrefix(u, prefix); found {
			parts := strings.Split(strings.Trim(rest, "/"), "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return "", "", false
			}
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}

// landWork commits what a run changed, pushes it, and opens a pull request.
//
// Every failure here is reported and swallowed. The run has already happened
// and its result is about to be recorded; turning a landing problem into a
// failed job would misreport the work, and the changes stay in the working
// tree either way, which is where they were before any of this existed.
func (d *dispatcher) landWork(ctx context.Context, job work.Job, rig, out string,
	before []landing.Change, checked *check.Result, runErr error) {
	dir := d.cellRoot + "/town/" + rig + "/refinery/rig"
	if _, err := os.Stat(dir); err != nil {
		// A rig with no refinery working tree holds no code to change. The
		// witness and mayor are like this.
		return
	}
	git := landing.Exec(dir, d.cellRoot)

	base := d.defaultBranch(git, rig)
	if base == "" {
		d.log.Warn("cannot tell which branch to open against; not landing",
			"bead", job.Bead, "rig", rig)
		return
	}

	// Read once and reused for the commit and the pull request, so a title
	// that changes between the two cannot happen.
	title := d.beadTitle(ctx, rig, job.Bead)
	summary := agentSummary(job.Bead, out)

	res, err := landing.Land(git, landing.Spec{
		Bead:    job.Bead,
		Title:   title,
		Base:    base,
		Summary: summary,
		Before:  before,
	})
	if err != nil {
		d.log.Error("landing a change", "bead", job.Bead, "rig", rig, "error", err)
		return
	}
	if res == nil {
		// Nothing to land. The ordinary case for a bead whose answer was that
		// no code change was needed -- sa-4yn was exactly that -- so this is
		// deliberately not a warning.
		d.log.Info("nothing to land", "bead", job.Bead, "rig", rig)
		return
	}
	d.log.Info("pushed a branch", "bead", job.Bead, "branch", res.Branch,
		"commit", res.Commit, "files", strings.Join(res.Files, " "),
		"skipped", strings.Join(res.Skipped, " "))
	if res.Warning != "" {
		// The work landed and something afterwards did not. Reported at warn
		// rather than folded into the line above, because it needs somebody to
		// look: a rig left on a bead's branch gives the next landing an odd
		// base.
		d.log.Warn("the landing was not clean", "bead", job.Bead, "warning", res.Warning)
	}

	url, err := d.openPullRequest(ctx, job, rig, res, base, summary, title, checked, runErr)
	if err != nil {
		d.log.Error("opening a pull request", "bead", job.Bead,
			"branch", res.Branch, "error", err)
		return
	}
	d.log.Info("pull request", "bead", job.Bead, "url", url)

	// On the bead, so the agent's own record says where the work went. The
	// interface reads this too, through work_refs.last_comment.
	cmd := exec.CommandContext(ctx, "bd", "comment", job.Bead,
		"Landed as "+url+" (branch "+res.Branch+", commit "+res.Commit+").")
	cmd.Dir = d.cellRoot + "/town/" + d.rigOwning(job.Bead, rig)
	cmd.Env = append(os.Environ(), "HOME="+d.cellRoot)
	if b, err := cmd.CombinedOutput(); err != nil {
		d.log.Warn("commenting the pull request onto the bead", "bead", job.Bead,
			"error", err, "output", strings.TrimSpace(string(b)))
	}
}

// openPullRequest asks the control plane to open it. The node cannot: opening
// one needs the App key, and the key stays on the control node.
func (d *dispatcher) openPullRequest(ctx context.Context, job work.Job, rig string,
	res *landing.Result, base, summary, title string, checked *check.Result, runErr error) (string, error) {

	ok := runErr == nil

	// What the reviewer needs to know first is whether the agent thought it
	// had finished, because a pull request from an unfinished run looks
	// identical to one from a finished one.
	state := "The agent closed the bead."
	if !ok {
		// Named, not merely reported.
		//
		// "The run FAILED" on its own is what a reviewer got, and with the
		// stall stop it is all they got: wg-runner is signalled, so it prints
		// no status line, agentSummary finds no marker and returns nothing,
		// and the pull request carries a failure with no evidence anywhere in
		// it. The dispatcher knows the reason -- "nothing written for 10m1s
		// (no output, no file changed)" -- and it is one sentence.
		state = "The run FAILED. The change is here because it exists, not " +
			"because it is finished."
		if reason := strings.TrimSpace(runErr.Error()); reason != "" {
			state += "\n\nWhy it ended: `" + reason + "`"
		}
	} else if job.Bead != "" {
		state = "The agent completed its run. See the bead for whether it closed it."
	}
	// Whether it was checked, stated before anything else a reviewer reads.
	// An unchecked change and a change whose tests pass look identical in a
	// diff, and the difference is most of what a reviewer wants to know.
	checkedLine := "**Not checked.** No build or test command is configured for this " +
		"repository, so nothing here has been executed."
	if checked != nil {
		switch {
		case checked.OK:
			checkedLine = "**The repository's own check passed** after this change: `" +
				checked.Command + "` (" + checked.Took.String() + ")."
		case checked.TimedOut:
			checkedLine = "**The check did not finish.** `" + checked.Command +
				"` was still running after " + checked.Took.String() + " and was stopped."
		default:
			checkedLine = "**The check FAILED** after this change: `" + checked.Command +
				"`. The agent was given the output and ran again; it still fails.\n\n" +
				"<details><summary>check output</summary>\n\n```\n" +
				checked.Output + "\n```\n\n</details>"
		}
	}

	body := "Bead `" + job.Bead + "`.\n\n" + state + "\n\n" + checkedLine + "\n\n" +
		fileSummary(res.Files) + "\n\n" +
		"Opened by a Workgraph agent run. Nobody has reviewed this."
	body += tail(summary)

	payload, err := json.Marshal(map[string]any{
		"cell": d.cell, "rig": rig, "bead": job.Bead,
		"head": res.Branch, "base": base,
		"title": subject(title, job.Bead),
		"body":  body,
		"job":   job.ID,
	})
	if err != nil {
		return "", err
	}
	raw, err := d.call(ctx, http.MethodPost, "/v1/node/pull-requests", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	var answer struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", err
	}
	if answer.URL == "" {
		return "", errors.New("the control plane returned no pull request URL")
	}
	return answer.URL, nil
}

// defaultBranch is the branch the repository itself considers default.
//
// Never assumed to be main. Half the repositories a cell now holds are not
// ours -- ckan/ckan's default branch is `master` -- and a pull request opened
// against a branch that does not exist is refused by GitHub after the work has
// already been pushed.
//
// Two sources, in this order, because neither alone is enough. Measured on the
// oss cell: `git symbolic-ref refs/remotes/origin/HEAD` fails in every one of
// the twelve rigs, because `gt rig add` clones without recording a remote
// HEAD, so git alone would skip landing everywhere. And config.json is gt's
// record of what it detected at clone time, which is right today and stale if
// the repository is ever renamed -- so git is asked first, as the live answer.
func (d *dispatcher) defaultBranch(git landing.Git, rig string) string {
	if out, err := git("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if _, branch, found := strings.Cut(strings.TrimSpace(out), "/"); found && branch != "" {
			return branch
		}
	}
	raw, err := os.ReadFile(d.cellRoot + "/town/" + rig + "/config.json")
	if err != nil {
		return ""
	}
	var config struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return ""
	}
	return strings.TrimSpace(config.DefaultBranch)
}

// beadTitle reads the bead's title, for the commit subject.
func (d *dispatcher) beadTitle(ctx context.Context, rig, bead string) string {
	rows, err := d.readBeads(ctx, d.rigOwning(bead, rig))
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.Bead == bead {
			return r.Title
		}
	}
	return ""
}

// rigOwning is the rig whose graph holds a bead, which is not always the rig
// the work ran in: a bead filed in one rig can be about another's code.
func (d *dispatcher) rigOwning(bead, fallback string) string {
	prefix, _, found := strings.Cut(strings.TrimSpace(bead), "-")
	if !found || prefix == "" {
		return fallback
	}
	for _, rig := range d.rigs() {
		raw, err := os.ReadFile(d.cellRoot + "/town/" + rig + "/config.json")
		if err != nil {
			continue
		}
		var config struct {
			Type  string `json:"type"`
			Beads struct {
				Prefix string `json:"prefix"`
			} `json:"beads"`
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			continue
		}
		if config.Type == "rig" && config.Beads.Prefix == prefix {
			return rig
		}
	}
	return fallback
}

// agentSummary is what the agent said, pulled out of the runner's output.
//
// wg-runner prints "<bead>: <status> in <duration>" and then the agent's own
// words, so everything after that line is the summary. Parsing an output format
// is fragile, which is why a failure to find the marker returns nothing rather
// than a guess: an empty pull request body is honest and a body containing the
// runner's log lines is not.
func agentSummary(bead, out string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, bead+": ") && strings.Contains(line, " in ") {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	return ""
}

// fileSummary describes what changed, at a size a person will read.
//
// The list used to be every path inline, backticked and comma-separated. For a
// scaffold that is 77 of them in one paragraph -- accurate, and the least
// readable thing in the pull request once the transcript had been folded away.
// A reviewer wants the shape first: which trees were touched and how much.
//
// Small changes are still listed in full, because for three files a summary is
// worse than the thing it summarises.
func fileSummary(files []string) string {
	if len(files) == 0 {
		return "No files changed."
	}
	if len(files) <= 10 {
		return "Files: `" + strings.Join(files, "`, `") + "`"
	}

	// Grouped by top-level entry, in the order they first appear, so the
	// summary reads like the tree rather than like a sorted index.
	type group struct {
		name string
		n    int
	}
	var groups []group
	at := map[string]int{}
	for _, f := range files {
		top := f
		if i := strings.Index(f, "/"); i >= 0 {
			top = f[:i] + "/"
		}
		if idx, ok := at[top]; ok {
			groups[idx].n++
			continue
		}
		at[top] = len(groups)
		groups = append(groups, group{name: top, n: 1})
	}

	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.n == 1 && !strings.HasSuffix(g.name, "/") {
			parts = append(parts, "`"+g.name+"`")
			continue
		}
		parts = append(parts, fmt.Sprintf("`%s` (%d)", g.name, g.n))
	}

	return fmt.Sprintf("**%d files changed:** %s\n\n<details><summary>every path</summary>\n\n```\n%s\n```\n\n</details>",
		len(files), strings.Join(parts, ", "), strings.Join(files, "\n"))
}

// ansi matches the escape sequences a terminal harness writes.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// tail is the agent's account of the run, as a reviewer should see it.
//
// agentSummary returns everything wg-runner printed after its status line,
// which for the claude harness is the agent's own prose. For opencode -- what
// every polecat runs -- it is the raw terminal transcript, because opencode
// buffers its output and flushes the whole session at exit. That went into
// datopian/msf#1 verbatim: `^[[0m` escape sequences, `$ ls -la` command
// echoes, and pages of directory listings, under a bare `---`. Unreadable, and
// it read as though the pull request body were corrupt.
//
// Dropping it was the other option and it is worse: for a run that FAILED, the
// transcript is the most useful thing in the pull request, and a reviewer
// asking "what did it actually do" has nowhere else to look.
//
// So it is presented as what it is. Escape sequences stripped, the last lines
// only, folded into a <details> block that says it is a log, and inside a
// fence so that whatever the agent printed cannot be read as markdown. Prose
// is left alone -- an agent that wrote a real summary gets it shown plainly.
func tail(summary string) string {
	clean := strings.TrimSpace(ansi.ReplaceAllString(summary, ""))
	if clean == "" {
		return ""
	}
	if !looksLikeATranscript(clean) {
		return "\n\n---\n\n" + clean
	}

	// The end, not the beginning: a run that failed failed at the end, and the
	// first lines are the agent orienting itself.
	lines := strings.Split(clean, "\n")
	const keep = 40
	elided := ""
	if len(lines) > keep {
		elided = fmt.Sprintf(" — last %d of %d lines", keep, len(lines))
		lines = lines[len(lines)-keep:]
	}
	return "\n\n<details><summary>Run log" + elided + "</summary>\n\n```\n" +
		strings.Join(lines, "\n") + "\n```\n\n</details>"
}

// looksLikeATranscript reports whether this is a terminal session rather than
// something the agent wrote for a person to read.
//
// Two signals, either sufficient: the command echoes a harness prints, and the
// shape of a directory listing. Deliberately generous -- misreading prose as a
// log folds it into a details block, which is untidy; misreading a log as
// prose is what produced the unreadable pull request.
func looksLikeATranscript(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "$ "), strings.HasPrefix(line, "> "):
			return true
		case strings.HasPrefix(line, "total ") && len(line) < 20:
			return true
		case strings.HasPrefix(line, "drwx"), strings.HasPrefix(line, "-rw-"):
			return true
		}
	}
	return false
}

// subject is the pull request title: the bead's title when it has one, and the
// bead id when it does not.
func subject(title, bead string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t + " (" + bead + ")"
	}
	return "Work on " + bead
}

// treeBefore is what a rig's working tree already held, read before the run.
//
// Errors are swallowed to nil, and that is the UNSAFE direction here, so it is
// worth saying why it is still right: a nil snapshot means the landing treats
// everything dirty as the run's work, which is what it did before any of this
// existed. The alternative -- refusing to land when the tree cannot be read --
// loses the agent's work entirely. A stray file in a pull request is visible
// and removable; work that never left the node is neither.
func (d *dispatcher) treeBefore(rig string) []landing.Change {
	dir := d.cellRoot + "/town/" + rig + "/refinery/rig"
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	changes, err := landing.Changes(landing.Exec(dir, d.cellRoot))
	if err != nil {
		d.log.Warn("reading the working tree before a run", "rig", rig, "error", err)
		return nil
	}
	if len(changes) > 0 {
		// Worth a line in the log: a tree that is dirty before a run started
		// means a previous landing did not finish, or somebody edited by hand.
		paths := make([]string, 0, len(changes))
		for _, c := range changes {
			paths = append(paths, c.Path)
		}
		d.log.Info("the working tree was already dirty; these will not be landed",
			"rig", rig, "paths", strings.Join(paths, " "))
	}
	return changes
}

// refresh fast-forwards a rig's base branch before a run.
//
// Non-fatal in every direction. A stale checkout produces a pull request based
// on an old commit, which GitHub shows as needing a rebase and a person can
// see; refusing to run produces nothing and a queue that looks stuck. The
// first is recoverable and visible, so it is the one to prefer -- but it is
// logged at warn either way, because a rig that stops refreshing goes on
// producing plausible pull requests against a base that drifts further every
// time.
func (d *dispatcher) refresh(rig string) {
	dir := d.cellRoot + "/town/" + rig + "/refinery/rig"
	if _, err := os.Stat(dir); err != nil {
		return
	}
	git := landing.Exec(dir, d.cellRoot)
	base := d.defaultBranch(git, rig)
	if base == "" {
		return
	}
	moved, why := landing.Refresh(git, base)
	switch {
	case why != "":
		d.log.Warn("the checkout was not refreshed; the run works against a stale base",
			"rig", rig, "base", base, "reason", why)
	case moved:
		d.log.Info("refreshed the checkout", "rig", rig, "base", base)
	}
}

// check runs the repository's own build or test command against what the agent
// wrote, or reports nothing when the repository has not been opted in.
//
// The agent cannot do this: it has no shell, deliberately. See internal/check
// for why running the command here rather than granting one is the difference
// that matters.
func (d *dispatcher) check(ctx context.Context, job work.Job, rig string) *check.Result {
	if job.Kind != work.KindWork || strings.TrimSpace(job.Check) == "" {
		return nil
	}
	dir := d.cellRoot + "/town/" + rig + "/refinery/rig"
	res, err := check.Run(ctx, dir, job.Check, d.checkDeadline())
	if err != nil {
		d.log.Error("running the repository's check", "bead", job.Bead,
			"rig", rig, "command", job.Check, "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	d.log.Info("checked", "bead", job.Bead, "command", res.Command,
		"ok", res.OK, "timed_out", res.TimedOut, "took", res.Took)
	return res
}

// checkDeadline is how long a check may take.
//
// A share of the job's own deadline rather than a separate number, so a cell
// configured for short jobs does not spend twice as long checking as running.
// Half, because a failing check is followed by a second agent pass and another
// check, and the whole sequence has to fit.
func (d *dispatcher) checkDeadline() time.Duration {
	if d.deadline <= 0 {
		return 5 * time.Minute
	}
	return d.deadline / 2
}

// reportPlan tells the control plane which harness and model a job will use.
//
// Resolved with the runner's own Catalogue rather than by reading the file and
// applying the precedence rule again here. That rule -- the catalogue per key,
// the built-in table otherwise -- exists in one place, and a second copy of it
// in the dispatcher would disagree with the runner on the day somebody changed
// one of them, and would disagree silently, because both would still produce a
// plausible-looking answer.
//
// Every failure is swallowed to a log line. This is observability: a run that
// happens without being described is worth more than no run, and the endpoint
// answers `recorded: false` for a job it no longer holds rather than failing.
//
// Swallowed, but LOUDLY, and the catalogue is the reason. An unset path used to
// read as "no catalogue" and fall back to the built-in table, which says claude
// and sonnet — so a misconfigured dispatcher would have reported the wrong
// harness and model on every run rather than reporting none. LoadCatalogue
// refuses an empty path now, and this logs it rather than proceeding.
func (d *dispatcher) reportPlan(ctx context.Context, job work.Job) {
	catalogue, err := runner.LoadCatalogue(d.catalogue)
	if err != nil {
		d.log.Warn("reading the model catalogue to report what a run uses",
			"path", d.catalogue, "error", err)
		return
	}
	harness, model := catalogue.PlannedFor(job.Role())
	if harness == "" && model == "" {
		return
	}
	payload, err := json.Marshal(map[string]any{"runtime": harness, "model": model})
	if err != nil {
		return
	}
	if _, err := d.call(ctx, http.MethodPost,
		"/v1/node/work/"+url.PathEscape(job.ID)+"/plan", bytes.NewReader(payload)); err != nil {
		d.log.Warn("reporting what a run uses", "job", job.ID, "error", err)
		return
	}
	d.log.Info("run plan", "job", job.ID, "harness", harness, "model", model)
}

// ensureRig creates the job's rig if the town does not have it.
//
// Does nothing in the common case, which is why it is safe on every job: the
// directory check is a stat, and a rig that exists returns immediately.
//
// The name is never invented here. Both this and routing read it from
// system_rigs_wanted, so the rig the job names and the rig this creates cannot
// disagree -- and a rig created under a different name would be invisible to
// routing however correctly it was cloned, which has happened before.
func (d *dispatcher) ensureRig(ctx context.Context, job *work.Job) error {
	rig := strings.TrimSpace(job.Rig)
	if rig == "" {
		return nil // the dispatcher's default rig, which exists by construction
	}
	town := d.cellRoot + "/town"
	if st, err := os.Stat(town + "/" + rig); err == nil && st.IsDir() {
		return nil
	}

	// What to clone comes with the job. The node has no database -- it has no
	// inbound port and reaches the control plane outward -- so the control
	// plane, which knows which repository a rig should hold, sends it. Same
	// reasoning as the check command.
	if job.CloneURL == "" || job.Prefix == "" {
		return fmt.Errorf("rig %s does not exist and the job carries no clone URL and "+
			"prefix, so it cannot be created. Either the control plane is older than "+
			"this node, or no repository is registered for this bead's project", rig)
	}

	d.log.Info("creating a rig for work that needs it",
		"rig", rig, "cell", d.cell)
	if err := gastown.AddRig(ctx, d.gtBinary, town, rig, job.CloneURL, job.Prefix); err != nil {
		return err
	}
	d.log.Info("rig created", "rig", rig)

	// Registered immediately so routing can see it without waiting for the
	// next pass. Not fatal if it fails: the checkout exists and the work can
	// run, and the next pass registers it.
	if err := d.registerRig(ctx, rig); err != nil {
		d.log.Warn("registering a rig just created", "rig", rig, "error", err)
	}
	return nil
}
