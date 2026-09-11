package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An agent that is reading and thinking leaves no trace in the two places the
// stall watcher looks -- and gets killed for it.
//
// msf8-17x, 2026-09-11, with its dependencies already warm: stopped after
// 10m30s for "nothing written (no output, no file changed)". Its own log says
// what it was actually doing:
//
//	12:46:51 loop session.id=ses_f6f7ef25… step=5
//	12:46:51 stream model=workers-ai/@cf/zai-org/glm-5.3-flash
//	12:48:23 evaluated permission=read … portal/package.json
//
// Reading files changes none, opencode writes almost nothing to stdout, and a
// single model call on this gateway takes ninety seconds. So a working agent
// looked identical to a wedged one, and the guard that exists to stop the
// wedged one killed the working one.
//
// The agent's own log is the missing signal, and it was on disk the whole
// time: XDG_DATA_HOME points at <cellRoot>/runs/.<bead>.state, so opencode
// logs under there. It is also the only record of WHY a run did nothing --
// nothing in the job result carried it, which is why three days of empty runs
// could not be explained.
//
// transcriptDir is where a run's harness keeps its state. It matches
// runner.Plan.StatePath, deliberately duplicated rather than imported: the
// dispatcher must be able to find the log of a run it did not start, after a
// restart, when no Plan is in hand.
func transcriptDir(cellRoot, bead string) string {
	if strings.TrimSpace(cellRoot) == "" || strings.TrimSpace(bead) == "" {
		return ""
	}
	return filepath.Join(cellRoot, "runs", "."+bead+".state")
}

// agentLog is the newest log file the harness is writing, with its size and
// mtime.
//
// Newest rather than a fixed path, because the path belongs to the harness:
// opencode writes opencode/log/opencode.log, and a different harness will
// write somewhere else. Bounded to keep this cheap enough to run every tick.
func agentLog(cellRoot, bead string) (path string, size int64, at time.Time) {
	dir := transcriptDir(cellRoot, bead)
	if dir == "" {
		return "", 0, time.Time{}
	}
	seen := 0
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			return nil
		}
		if seen++; seen > 2000 {
			return filepath.SkipAll
		}
		if !strings.HasSuffix(e.Name(), ".log") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(at) {
			path, size, at = p, info.Size(), info.ModTime()
		}
		return nil
	})
	return path, size, at
}

// transcriptTail is the last few KB of the agent's log, for the job result.
//
// A tail rather than the whole thing: the log is megabytes of permission
// evaluations, and what a person needs is the end -- what it was doing when
// it stopped. Trimmed to whole lines so it does not begin mid-token.
func transcriptTail(cellRoot, bead string, limit int64) string {
	path, size, _ := agentLog(cellRoot, bead)
	if path == "" || size == 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	if limit <= 0 {
		limit = 4 << 10
	}
	start := int64(0)
	if size > limit {
		start = size - limit
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) == 0 {
		return ""
	}
	text := string(buf)
	if start > 0 {
		// Drop the partial first line.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return strings.TrimSpace(text)
}

// withTranscript appends the agent's log to a run's output.
//
// Labelled and last, so whatever the harness did say on stdout stays first
// and a reader can tell the two apart.
func withTranscript(output, tail string) string {
	if strings.TrimSpace(tail) == "" {
		return output
	}
	var b strings.Builder
	if strings.TrimSpace(output) != "" {
		b.WriteString(strings.TrimSpace(output))
		b.WriteString("\n\n")
	}
	b.WriteString("--- agent transcript (tail) ---\n")
	b.WriteString(tail)
	return b.String()
}
