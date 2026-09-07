// Package check runs a repository's own build or test command against what an
// agent just wrote.
//
// The agent cannot do this itself. Its tools are Read, Grep, Glob, Edit, Write
// and `Bash(bd:*)`, so it reasons about the code and stops -- and a pull request
// then arrives having never been executed. This is the step that tells it, and
// the reviewer, whether the change works.
//
// # Why the runner does it rather than the agent
//
// Running a project's tests means running code FROM the project: `npm test` does
// whatever package.json says, `make test` whatever the Makefile says. The agent
// can edit both. So granting the agent a shell for this would be granting it
// arbitrary execution by a short route, which is precisely what the tool policy
// withholds -- an agent that can run anything can push.
//
// Doing it here does not remove that: a script the agent edited still runs. What
// it removes is the agent's ability to run commands of its own choosing
// interactively, and it lets the one thing that actually matters be closed off:
//
//	the check runs with NO git credential helper, so nothing it starts can mint
//	a token or push. The helper lives in the cell user's global git config, and
//	GIT_CONFIG_GLOBAL=/dev/null unsets it for this process and its children.
//
// The residual risk is stated rather than hidden: a script in the repository can
// still reach the network and read the cell's files. That is the same code the
// repository's own CI runs, so it is not a new judgement about the repository --
// the new part is that an agent could alter it before it runs here, and the
// answers to that are the per-repository opt-in, the timeout below, the cell's
// cgroup slice, and a human reading the diff before anything merges.
package check

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// MaxOutput is how much of the command's output is kept.
//
// A failing build can print megabytes, and this ends up in a pull request body
// and a bead comment. The TAIL is kept rather than the head: a compiler names
// the file and line at the end of its output, and a test runner puts the
// summary there.
const MaxOutput = 8 << 10

// Result is what running the command produced.
type Result struct {
	// Command is what ran, recorded because a check that passed for the wrong
	// reason -- a command that does nothing -- is worse than no check.
	Command string
	OK      bool
	// TimedOut distinguishes "the tests fail" from "the tests never finish",
	// which need different responses from whoever reads it.
	TimedOut bool
	Output   string
	Took     time.Duration
}

// Run executes the command in dir, or returns nil when there is nothing to run.
//
// A repository with no command configured is the ordinary case and not a
// failure: nil means "not checked", which is what every repository is until
// somebody opts it in.
func Run(ctx context.Context, dir, command string, timeout time.Duration) (*Result, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, errors.New("a check needs a timeout; without one a hanging build holds the rig")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	// Through a shell, because a check command is a command line -- `npm ci &&
	// npm test` is the ordinary shape and splitting on spaces would break it.
	// This is not a widening: the command was configured by a person, and
	// whatever it invokes could invoke a shell anyway.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = environment()

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	res := &Result{
		Command:  command,
		OK:       err == nil,
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Output:   tail(out.String(), MaxOutput),
		Took:     time.Since(started).Round(time.Second),
	}
	if res.TimedOut {
		res.OK = false
	}
	return res, nil
}

// environment is what the check runs with.
func environment() []string {
	keep := []string{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		// The gateway credential. A check has no business talking to the model
		// gateway, and leaving it in the environment of repository-controlled
		// code would put the cell's token there.
		case "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS",
			// And the control plane's service credential, for the same reason:
			// it is what mints git tokens.
			"WG_ACCESS_CLIENT_ID", "WG_ACCESS_CLIENT_SECRET":
			continue
		case "GIT_CONFIG_GLOBAL":
			continue
		}
		keep = append(keep, kv)
	}
	// No global git config, so no credential helper. This is the one mitigation
	// that closes the route the tool policy's `git push` denial exists to
	// close: a script cannot ask the control plane for a token, because git
	// will not know how to.
	return append(keep, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
}

// tail keeps the last n bytes, on a line boundary, and says what it dropped.
func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "[…earlier output dropped…]\n" + cut
}

// Feedback is what an agent is told when a check failed, so it can fix it.
//
// The command and the output, and nothing about how to respond: the agent has
// its bead and its acceptance criteria, and telling it what to conclude is how
// a prompt starts arguing with the evidence.
func (r *Result) Feedback() string {
	if r == nil || r.OK {
		return ""
	}
	what := "failed"
	if r.TimedOut {
		what = "did not finish in time"
	}
	return "The repository's own check " + what + " after your change.\n\n" +
		"Command: " + r.Command + "\n\n" +
		"Output:\n" + r.Output + "\n\n" +
		"Fix the cause if it is your change. If the check was already failing " +
		"before you touched anything, or it is failing for a reason unrelated " +
		"to the bead, say so in a comment and leave it."
}
