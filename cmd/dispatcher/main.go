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
	if err := d.project(ctx); err != nil {
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

	d.log.Info("running", "job", job.ID, "kind", job.Kind, "bead", job.Bead)
	out, runErr := d.run(ctx, *job)
	d.report(ctx, job.ID, work.Result{OK: runErr == nil, Output: out})
	if runErr != nil {
		d.log.Error("job failed", "job", job.ID, "error", runErr)
	} else {
		d.log.Info("job done", "job", job.ID)
	}
}

// run invokes wg-runner, which owns the working directory, the settings, the
// deadline and the teardown. Nothing about how an agent is started belongs here.
func (d *dispatcher) run(ctx context.Context, job work.Job) (string, error) {
	rig := job.Rig
	if rig == "" {
		rig = d.rig
	}
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
func (d *dispatcher) project(ctx context.Context) error {
	beads, err := d.readBeads(ctx)
	if err != nil {
		return err
	}
	return d.sendBeads(ctx, beads)
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
}

// readBeads asks bd for the cell's beads as JSON.
func (d *dispatcher) readBeads(ctx context.Context) ([]beadRow, error) {
	cmd := exec.CommandContext(ctx, "bd", "list", "--all", "--json")
	cmd.Dir = d.cellRoot + "/town/" + d.rig
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
	return rows, nil
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
