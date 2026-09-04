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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/datopian/workgraph/internal/version"
	"github.com/datopian/workgraph/internal/work"
)

func main() {
	var (
		apiURL   = flag.String("api", os.Getenv("WG_CONTROL_API"), "control API base URL")
		cell     = flag.String("cell", getenv("WG_CELL", "oss"), "this cell's slug")
		cellRoot = flag.String("cell-root", "", "the cell's home, e.g. /srv/cells/oss")
		rig      = flag.String("rig", getenv("WG_RIG", "sandbox"), "default rig")
		runner   = flag.String("runner", "/usr/local/bin/wg-runner", "path to the agent runner")
		interval = flag.Duration("interval", 10*time.Second, "how often to look for work")
		deadline = flag.Duration("deadline", 15*time.Minute, "how long one job may take")
		once     = flag.Bool("once", false, "do a single pass and exit")
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
		deadline: *deadline, log: log,
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
	deadline                    time.Duration
	http                        *http.Client
	log                         *slog.Logger
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

	d.log.Info("running", "job", job.ID, "kind", job.Kind, "bead", job.Bead)
	out, runErr := d.run(ctx, *job)

	// Labelled whether the run succeeded or not. A job that produced beads and
	// then failed still produced beads, and an unattributed bead is invisible
	// on the project page it belongs to.
	if p := strings.TrimSpace(job.Project); p != "" {
		d.labelNewBeads(ctx, jobRig, before, p)
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
func (d *dispatcher) run(ctx context.Context, job work.Job) (string, error) {
	rig := d.rigFor(job)
	// A plan job has no bead of its own, so it needs a name for its run
	// directory and its cost attribution. The job id is the honest one: the
	// spend belongs to the act of planning, not to any bead it produces.
	bead := job.Bead
	if bead == "" {
		bead = "plan-" + job.ID
	}

	args := []string{
		"-bead", bead,
		"-cell", d.cell,
		"-rig", rig,
		"-cell-root", d.cellRoot,
		"-role", job.Role(),
		"-instructions", job.Instructions(),
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

	cmd := exec.CommandContext(ctx, d.runner, args...)
	// Appended rather than replacing the environment, because the runner also
	// needs PATH and HOME from the unit. The token is passed to one child and
	// is never logged; wg-runner writes it into a settings file outside the run
	// directory so the agent itself cannot read it back.
	cmd.Env = append(os.Environ(), "WG_AI_GATEWAY_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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
	Comment   string `json:"comment,omitempty"`
	CommentAt string `json:"comment_at,omitempty"`
	CommentBy string `json:"comment_by,omitempty"`
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
