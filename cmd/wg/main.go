// Command wg reaches Workgraph over HTTP, with a personal API token.
//
//	wg login                    store a token
//	wg whoami                   who this credential is
//	wg inbox                    what needs me
//	wg ask "what changed"       the chief-of-staff questions
//	wg work list|queue          what work exists, and what it cost
//	wg work plan "a brief" [--project <slug>]
//	                            queue a planning job
//	wg work dispatch wg-abc     run one bead
//	wg project list|show <slug>
//	wg tokens list
//
// NOT wg-work, which is a different program for a different situation. Its own
// doc comment gives the reason: it reaches the queue over the database because
// it runs where the database is, so a demo or an incident does not depend on a
// browser. That is right for the control node and impossible on a laptop, which
// would need a PostgreSQL connection string — and handing that out is the
// opposite of what wg-p4h exists to do.
//
// Two properties matter more than the command set (wg-p4h.6).
//
// --json on everything, in the shape the OpenAPI document specifies rather than
// a second rendering. An agent parses stdout, and a pretty table that is subtly
// not the API's shape is a bug that only ever appears in agent transcripts.
//
// Exit codes a script can branch on: 0 ok, 2 usage, 3 unauthenticated,
// 4 forbidden, 5 refused by policy, 6 rate limited, 7 conflict, 1 anything
// else. A skill can act on those; it cannot act on prose.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Exit codes. Documented here because they are an interface, not an
// implementation detail — see the package comment.
const (
	exitOK              = 0
	exitError           = 1
	exitUsage           = 2
	exitUnauthenticated = 3
	exitForbidden       = 4
	exitPolicyRefused   = 5
	exitRateLimited     = 6
	exitConflict        = 7
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	args := os.Args[2:]
	jsonOut := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}

	code, err := run(os.Args[1], rest, jsonOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wg: "+err.Error())
	}
	os.Exit(code)
}

func run(cmd string, args []string, jsonOut bool) (int, error) {
	switch cmd {
	case "login":
		return login(args)
	case "whoami":
		return get("/v1/me", jsonOut, renderWhoami)
	case "inbox":
		return get("/v1/inbox", jsonOut, renderInbox)
	case "ask":
		if len(args) == 0 {
			// The endpoint returns the questions it supports when asked
			// nothing, which is the discoverability it would otherwise lack.
			return get("/v1/ask", jsonOut, renderAsk)
		}
		return get("/v1/ask?q="+urlEscape(strings.Join(args, " ")), jsonOut, renderAsk)
	case "work":
		return work(args, jsonOut)
	case "project", "projects":
		return project(args, jsonOut)
	case "tokens":
		return get("/v1/tokens", jsonOut, renderTokens)
	case "mcp":
		// Local stdio MCP server, for a client that would rather call a tool
		// than run a command.
		return mcpServe()
	case "spec":
		// The contract, so a client author never has to ask where it is.
		return get("/v1/openapi.json", true, nil)
	case "help", "-h", "--help":
		usage()
		return exitOK, nil
	default:
		usage()
		return exitUsage, fmt.Errorf("unknown command %q", cmd)
	}
}

func work(args []string, jsonOut bool) (int, error) {
	if len(args) == 0 {
		return exitUsage, errors.New("wg work list|queue|plan|dispatch")
	}
	switch args[0] {
	case "list":
		return get("/v1/work", jsonOut, renderWork)
	case "queue":
		return get("/v1/work/queue", jsonOut, renderWork)
	case "plan":
		// A trailing --project <slug> rather than a leading flag, because the
		// brief is a multi-word positional and a flag package would need the
		// user to quote it differently from every other wg command.
		rest, project := takeProject(args[1:])
		if len(rest) == 0 {
			return exitUsage, errors.New(`wg work plan "a brief" [--project <slug>]`)
		}
		body := map[string]any{"brief": strings.Join(rest, " ")}
		if project != "" {
			body["project"] = project
		}
		return post("/v1/work/plan", body, jsonOut)
	case "dispatch":
		if len(args) < 2 {
			return exitUsage, errors.New("wg work dispatch <bead>")
		}
		return post("/v1/work/"+args[1]+"/dispatch", map[string]any{}, jsonOut)
	default:
		return exitUsage, fmt.Errorf("unknown work command %q", args[0])
	}
}

func project(args []string, jsonOut bool) (int, error) {
	if len(args) == 0 || args[0] == "list" {
		return get("/v1/projects", jsonOut, renderProjects)
	}
	if args[0] == "show" && len(args) > 1 {
		return get("/v1/projects/"+args[1]+"/detail", jsonOut, nil)
	}
	return exitUsage, errors.New("wg project list | wg project show <slug>")
}

// ---------------------------------------------------------------------------
// Credential handling
// ---------------------------------------------------------------------------

// configPath is where the token lives. 0600, in the user's config directory.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workgraph", "credentials.json"), nil
}

type credentials struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

// login stores a token read from stdin or the environment.
//
// Never from a command-line argument. An argument is visible in ps, lands in
// shell history, and is the single most common way a credential leaks from a
// CLI — the same discipline plan §9.2 already requires of the GitHub
// installation token.
func login(args []string) (int, error) {
	base := "https://api-staging.openbases.com"
	if len(args) > 0 {
		base = strings.TrimRight(args[0], "/")
	}

	token := os.Getenv("WG_TOKEN")
	if token == "" {
		// Says what is true. wg does not echo the token, but the TERMINAL does
		// when a person types or pastes into it, and the old wording — "it will
		// not be echoed" — read as a promise that the secret stays off the
		// screen. It does not, and somebody trusting that would paste a live
		// credential into a shared screen or a recorded session.
		//
		// The pipe form avoids it entirely and is what the prompt now
		// recommends, because it is the only version where the secret never
		// reaches the terminal, the scrollback, or shell history.
		fmt.Fprintln(os.Stderr, "Paste the token and press Enter.")
		fmt.Fprintln(os.Stderr, "Note: your terminal will show it. To avoid that, pipe it instead:")
		fmt.Fprintln(os.Stderr, "  <command that prints the token> | wg login")
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exitError, fmt.Errorf("reading the token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return exitUsage, errors.New("no token given; set WG_TOKEN or pipe it in")
	}
	if !strings.HasPrefix(token, "wgp_") {
		// Caught here rather than on the first 401, which would send the
		// operator to check the server.
		return exitUsage, errors.New(`that does not look like a Workgraph token (they start with "wgp_")`)
	}

	path, err := configPath()
	if err != nil {
		return exitError, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return exitError, err
	}
	body, err := json.Marshal(credentials{BaseURL: base, Token: token})
	if err != nil {
		return exitError, err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return exitError, err
	}
	fmt.Printf("stored for %s in %s\n", base, path)
	return exitOK, nil
}

// errNoCredential is "there is no token here", as distinct from "the token was
// rejected".
//
// A sentinel rather than a string, because both have to exit 3 and the skill
// tells agents to branch on the exit code and never on message text. Before
// this, a missing credential exited 1 (a generic error) while a REJECTED one
// exited 3 -- so the more common case, and the only one a fresh machine ever
// hits, was the one the documented table got wrong.
var errNoCredential = errors.New("not logged in: run `wg login`")

func loadCredentials() (credentials, error) {
	if t := os.Getenv("WG_TOKEN"); t != "" {
		base := os.Getenv("WG_API")
		if base == "" {
			base = "https://api-staging.openbases.com"
		}
		return credentials{BaseURL: strings.TrimRight(base, "/"), Token: t}, nil
	}
	path, err := configPath()
	if err != nil {
		return credentials{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return credentials{}, errNoCredential
	}
	var c credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return credentials{}, fmt.Errorf("%s is not readable: %w", path, err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func do(method, path string, body any) (int, []byte, error) {
	c, err := loadCredentials()
	if err != nil {
		return 0, nil, err
	}

	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, payload)
	if err != nil {
		return 0, nil, err
	}
	// The token goes in a header and never in the URL: a URL reaches access
	// logs, proxies and browser history.
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		// Every write is retry-safe by default. A CLI invocation that times out
		// is exactly the case idempotency exists for, and requiring the caller
		// to remember a flag would mean it is absent when it matters.
		req.Header.Set("Idempotency-Key", newIdempotencyKey())
	}

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, out, err
}

// codeFor maps an HTTP status to the exit code a script branches on.
func codeFor(status int, body []byte) int {
	switch status {
	case http.StatusUnauthorized:
		return exitUnauthenticated
	case http.StatusTooManyRequests:
		return exitRateLimited
	case http.StatusConflict:
		return exitConflict
	case http.StatusForbidden:
		// Two different problems wearing the same status. A scope or role
		// refusal is fixed by changing the credential; a policy refusal is
		// fixed by asking a human. Different exit codes so a script can tell.
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		switch e.Code {
		case "token_scope_insufficient", "role_grant_missing", "token_self_service_forbidden":
			return exitForbidden
		default:
			return exitPolicyRefused
		}
	}
	if status >= 200 && status < 300 {
		return exitOK
	}
	return exitError
}

func get(path string, jsonOut bool, render func([]byte)) (int, error) {
	status, body, err := do(http.MethodGet, path, nil)
	if err != nil {
		return exitFor(err), err
	}
	return emit(status, body, jsonOut, render)
}

func post(path string, body any, jsonOut bool) (int, error) {
	status, out, err := do(http.MethodPost, path, body)
	if err != nil {
		return exitFor(err), err
	}
	return emit(status, out, jsonOut, nil)
}

// exitFor maps a pre-request failure to an exit code.
//
// Only one of them is interesting: no credential is exit 3, the same as a
// rejected one, because to a caller they mean the same thing -- run `wg login`.
// Everything else is a generic failure.
func exitFor(err error) int {
	if errors.Is(err, errNoCredential) {
		return exitUnauthenticated
	}
	return exitError
}

func emit(status int, body []byte, jsonOut bool, render func([]byte)) (int, error) {
	code := codeFor(status, body)
	if code != exitOK {
		// The server's message, verbatim. Rewording it would make the CLI and
		// the API disagree about what happened.
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		if e.Code != "" {
			msg += " (" + e.Code + ")"
		}
		if jsonOut {
			os.Stdout.Write(body)
			fmt.Println()
		}
		return code, errors.New(msg)
	}
	if jsonOut || render == nil {
		os.Stdout.Write(body)
		fmt.Println()
		return exitOK, nil
	}
	render(body)
	return exitOK, nil
}

// takeProject pulls a trailing `--project <slug>` out of a brief's words.
//
// Without a project the beads are filed company-wide, and since 0071 that means
// every colleague who can log in can read them. That is right for company work
// and wrong for anything client-shaped, which is why this exists at all: the
// API has taken a project since 0072 and this CLI could not send one, so every
// brief filed through `wg` was company-wide whether the person meant it or not.
func takeProject(words []string) (rest []string, project string) {
	for i := 0; i < len(words); i++ {
		if words[i] != "--project" && !strings.HasPrefix(words[i], "--project=") {
			rest = append(rest, words[i])
			continue
		}
		if after, ok := strings.CutPrefix(words[i], "--project="); ok {
			project = after
			continue
		}
		if i+1 < len(words) {
			project = words[i+1]
			i++
		}
	}
	return rest, project
}
