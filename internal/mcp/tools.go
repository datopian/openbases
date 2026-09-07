package mcp

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Observer is told about each tool call, for the log.
//
// The tool name, who called it and how it went — and NOT the arguments. A brief
// is a paragraph of somebody's intent and a question is a question; neither
// belongs in a log that operators and CI read. Same rule that took prompt
// bodies out of the gateway logs (wg-90f), applied before the mistake rather
// than after it.
type Observer func(ctx context.Context, tool string, refusal Refusal, err error)

// Options configures a server.
type Options struct {
	// Caller performs the API operations.
	Caller Caller
	// Version is reported in serverInfo, so a client's logs say which build
	// answered. The same reason the interface carries a build stamp.
	Version string
	// HasShell says whether the person on the other end has a command line.
	//
	// It changes exactly one thing: what an expired credential tells the model
	// to do. Over stdio the answer is `wg login`; over a connector there is no
	// login command and the answer is to reconnect in the client. Getting this
	// wrong sends the model hunting for a shell that does not exist, which is
	// the failure that made this whole bead necessary.
	HasShell bool
	// Observe is called after each tool call. Optional; nil logs nothing, which
	// is what the stdio transport wants — its stdout IS the protocol stream, so
	// a log line written there would corrupt the conversation.
	Observe Observer
}

// Arguments for the two tools that take any.
//
// Typed structs rather than map[string]any so the SDK generates the input
// schema from them and the schema cannot disagree with what the handler reads.
// The old hand-written schemas and the hand-written argument parsing were two
// descriptions of one thing.
type askArgs struct {
	Question string `json:"question,omitempty" jsonschema:"One of the supported questions. Omit to list them."`
}

type fileWorkArgs struct {
	Brief   string `json:"brief" jsonschema:"One paragraph describing the outcome, with acceptance criteria."`
	Project string `json:"project,omitempty" jsonschema:"Project slug to file the beads into, from workgraph_project_list. Omit ONLY for company-wide work: a brief filed with no project produces beads every colleague who can log in may read. Anything client-shaped needs one, and the server refuses a project you are not a member of."`
}

type beadArgs struct {
	Bead string `json:"bead" jsonschema:"The bead id, for example wg-abc or sa-kfh."`
}

type dispatchArgs struct {
	Bead string `json:"bead" jsonschema:"The bead id, for example wg-abc."`
	// A rig, for when more than one can do the work.
	//
	// Without this the refusal was unactionable from a hosted client: the
	// PortalJS project holds datopian/portaljs AND datopian/cloud.portaljs.com,
	// so dispatch answered "several rigs hold a repository for this bead's
	// project, so name one" and there was no field in which to name one. The
	// server refuses a rig that does not hold the work, so this is a choice
	// among the candidates rather than a way round the check.
	Rig string `json:"rig,omitempty" jsonschema:"Optional. The rig to run in, when dispatch says several hold this project's repositories. It must be one of the rigs it named."`
}

type newProjectArgs struct {
	Slug string `json:"slug" jsonschema:"A short lowercase identifier, for example jackson-open-data. This is permanent and appears in bead labels."`
	Name string `json:"name" jsonschema:"The human-readable name, for example Jackson Open Data."`
	// Mandatory, and not a field a caller may omit.
	//
	// The schema's backup_owner_differs constraint makes full-cycle ownership
	// impossible to vest in one person, so there is no default this could
	// invent. A tool that guessed here would create a project whose
	// accountability was decided by a language model.
	BackupOwner  string `json:"backup_owner" jsonschema:"REQUIRED. The email address of the backup owner. Must be a different person from the primary owner - the registry refuses a project where one person holds both roles. Ask the person which colleague it should be rather than choosing."`
	PrimaryOwner string `json:"primary_owner,omitempty" jsonschema:"Optional. The primary owner's email address. Defaults to whoever is calling, which is usually right."`
	Portfolio    string `json:"portfolio,omitempty" jsonschema:"Optional. The slug of the portfolio this belongs under, for example bizdev."`
	Objective    string `json:"objective,omitempty" jsonschema:"Optional. One sentence on what this project is for."`
	Visibility   string `json:"visibility,omitempty" jsonschema:"Optional: internal (the default), public, or restricted. A restricted project also needs a cell."`
	Cell         string `json:"cell,omitempty" jsonschema:"Optional. The execution cell where this project's work runs, for example oss. Required when visibility is restricted."`
}

type attachArgs struct {
	Slug string `json:"slug" jsonschema:"The project's slug."`
	// A list, because attaching six repositories should not be six round
	// trips and because the endpoint reports per repository.
	Repositories []string `json:"repositories" jsonschema:"Repositories as owner/name, for example [\"datopian/portaljs\"]. Several may be given at once; each is reported separately, so some can succeed while others are refused."`
}

type noArgs struct{}

// Route is one tool's HTTP shape, kept as data so a test can assert the
// mapping without calling anything.
type Route struct {
	Tool   string
	Method string
	// Path is the fixed part. A tool that puts an argument in the path or the
	// query builds the rest in its handler.
	Path string
	// ReadOnly mirrors the annotation the client sees. Held here too so the
	// test that checks "nothing that spends money is marked read-only" reads
	// one table rather than reaching into the SDK's structs.
	ReadOnly bool
	// SpendsMoney means an agent runs and the person is billed. Every such
	// tool must be annotated non-read-only so clients prompt.
	SpendsMoney bool
}

// Routes is the tool-to-route table, in the order the tools are advertised.
//
// Order matters more than it looks: the model reads the list top to bottom, and
// the read-only tools come first so that the cheapest useful thing is the first
// thing it sees.
var Routes = []Route{
	{Tool: "workgraph_inbox", Method: http.MethodGet, Path: "/v1/inbox", ReadOnly: true},
	{Tool: "workgraph_ask", Method: http.MethodGet, Path: "/v1/ask", ReadOnly: true},
	{Tool: "workgraph_work_list", Method: http.MethodGet, Path: "/v1/work", ReadOnly: true},
	{Tool: "workgraph_bead", Method: http.MethodGet, Path: "/v1/work/{bead}", ReadOnly: true},
	{Tool: "workgraph_project_list", Method: http.MethodGet, Path: "/v1/projects", ReadOnly: true},
	// The writes that set a project up. None of them spends money: they change
	// the registry, and the registry is what later decides where work runs and
	// who may read it.
	//
	// Reads and creates only, which is a deliberate line and not an oversight.
	// A detach tool was written for symmetry and removed again: it is the
	// irreversible direction, withdrawing the route work travels on, and
	// symmetry is not a good enough reason to widen a guardrail somebody set
	// on purpose. Detaching stays an API call, where the person doing it has
	// read what they are doing.
	{Tool: "workgraph_project_create", Method: http.MethodPost, Path: "/v1/projects"},
	{Tool: "workgraph_repositories_attach", Method: http.MethodPost, Path: "/v1/projects/{slug}/repositories"},
	{Tool: "workgraph_file_work", Method: http.MethodPost, Path: "/v1/work/plan", SpendsMoney: true},
	{Tool: "workgraph_dispatch", Method: http.MethodPost, Path: "/v1/work/{bead}/dispatch", SpendsMoney: true},
}

// The tool definitions, as package-level values.
//
// Declared here rather than inline in NewServer so that a test can read exactly
// what a model will be told without constructing a server or reaching into the
// SDK. The tool list IS the prompt, so the assertions about it — how many there
// are, that the spending ones say so, that nothing exists for an action no
// credential may take — are assertions about this data.
var (
	toolInbox = &sdk.Tool{
		Name: "workgraph_inbox",
		Description: "What needs this person's attention in Workgraph right now. " +
			"Use for 'what needs me', 'what's on my plate', 'anything waiting on me'.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Needs you", ReadOnlyHint: true, DestructiveHint: ptr(false),
		},
	}

	toolAsk = &sdk.Tool{
		Name: "workgraph_ask",
		Description: "Ask Workgraph's chief of staff a question about the portfolio. " +
			"It answers a FIXED set of questions; call with no question to list them, " +
			"then use one verbatim. Do not invent a question it does not answer.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Ask", ReadOnlyHint: true, DestructiveHint: ptr(false),
		},
	}

	toolWorkList = &sdk.Tool{
		Name: "workgraph_work_list",
		Description: "Work items with their status and what each has cost so far. " +
			"Use for 'what is in flight', 'what did that cost', bead status.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Work", ReadOnlyHint: true, DestructiveHint: ptr(false),
		},
	}

	// The answer to "I dispatched that -- did anything happen?".
	//
	// Its own tool rather than more fields on workgraph_work_list, because the
	// list is read to scan and this is read to understand one thing: a list
	// carrying every bead's log tail, comment and per-model spend would be
	// unreadable on a phone and expensive to produce.
	toolBead = &sdk.Tool{
		Name: "workgraph_bead",
		Description: "Everything known about one bead: whether the work finished, " +
			"why not if it did not, what the agent said about it, and which model it " +
			"used and what that cost. Use after dispatching, and for 'what happened to', " +
			"'did that work', 'why did that fail', 'what did that cost'.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Bead detail", ReadOnlyHint: true, DestructiveHint: ptr(false),
		},
	}

	toolProjectList = &sdk.Tool{
		Name:        "workgraph_project_list",
		Description: "Projects this person can see.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Projects", ReadOnlyHint: true, DestructiveHint: ptr(false),
		},
	}

	toolProjectCreate = &sdk.Tool{
		Name: "workgraph_project_create",
		Description: "Create a project. Needs a slug, a name, and the email of a " +
			"BACKUP OWNER who is a different person from the primary owner — the " +
			"registry refuses a project where one person holds both, so ask who " +
			"it should be rather than choosing. The primary owner defaults to " +
			"whoever is asking. The slug is permanent: it appears in bead labels " +
			"and decides who can read the work.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Create a project", ReadOnlyHint: false, DestructiveHint: ptr(false),
		},
	}

	toolRepositoriesAttach = &sdk.Tool{
		Name: "workgraph_repositories_attach",
		Description: "Attach repositories to a project, as owner/name. This is what " +
			"makes work dispatchable against them: a cell gets one rig per " +
			"attached repository, and a dispatch is routed to the rig holding " +
			"the code the bead is about. Several at once is fine and each is " +
			"reported separately.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Attach repositories", ReadOnlyHint: false, DestructiveHint: ptr(false),
		},
	}

	// The two that spend money. ReadOnlyHint false and DestructiveHint true so
	// a client PROMPTS before running them, and SPENDS MONEY in the description
	// as well — an annotation is a hint a client may ignore, and the
	// description is what the model itself reads.
	toolFileWork = &sdk.Tool{
		Name: "workgraph_file_work",
		Description: "File a planning job from a brief, which produces beads. " +
			"SPENDS MONEY: an agent runs. Confirm with the person first. " +
			"The brief needs a checkable outcome, not 'make it better'.",
		Annotations: &sdk.ToolAnnotations{
			Title: "File work (spends money)", ReadOnlyHint: false, DestructiveHint: ptr(true),
		},
	}

	toolDispatch = &sdk.Tool{
		Name: "workgraph_dispatch",
		Description: "Run one bead. SPENDS MONEY: an agent runs against it. " +
			"Confirm with the person before calling this for work they did not ask you to run. " +
			"If it answers that several rigs hold this project's repositories, call it again " +
			"with one of the rigs it named.",
		Annotations: &sdk.ToolAnnotations{
			Title: "Dispatch (spends money)", ReadOnlyHint: false, DestructiveHint: ptr(true),
		},
	}
)

// Tools returns the curated set, in the order it is advertised.
//
// Read-only tools first: the model reads the list top to bottom, so the
// cheapest useful thing is the first thing it sees.
func Tools() []*sdk.Tool {
	return []*sdk.Tool{
		toolInbox, toolAsk, toolWorkList, toolBead, toolProjectList,
		toolProjectCreate, toolRepositoriesAttach,
		toolFileWork, toolDispatch,
	}
}

// NewServer builds the server with the curated tool set registered.
//
// The descriptions say what each tool is FOR rather than what it calls: a model
// choosing between tools is reading intent, not routes.
func NewServer(o Options) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "workgraph", Version: o.Version}, nil)
	c := o.Caller
	shell := o.HasShell
	obs := o.Observe

	// --- read-only ---------------------------------------------------------

	sdk.AddTool(s, toolInbox, func(ctx context.Context, _ *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, any, error) {
		return plain(ctx, c, shell, obs, "workgraph_inbox", http.MethodGet, "/v1/inbox", nil)
	})

	sdk.AddTool(s, toolAsk, func(ctx context.Context, _ *sdk.CallToolRequest, a askArgs) (*sdk.CallToolResult, any, error) {
		path := "/v1/ask"
		if q := strings.TrimSpace(a.Question); q != "" {
			path += "?q=" + url.QueryEscape(q)
		}
		return plain(ctx, c, shell, obs, "workgraph_ask", http.MethodGet, path, nil)
	})

	sdk.AddTool(s, toolWorkList, func(ctx context.Context, _ *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, any, error) {
		return plain(ctx, c, shell, obs, "workgraph_work_list", http.MethodGet, "/v1/work", nil)
	})

	sdk.AddTool(s, toolBead, func(ctx context.Context, _ *sdk.CallToolRequest, a beadArgs) (*sdk.CallToolResult, any, error) {
		bead := strings.TrimSpace(a.Bead)
		if bead == "" {
			return errorResult("a bead id is required"), nil, nil
		}
		// Escaped: a bead id reaches a path segment.
		return plain(ctx, c, shell, obs, "workgraph_bead", http.MethodGet,
			"/v1/work/"+url.PathEscape(bead), nil)
	})

	sdk.AddTool(s, toolProjectList, func(ctx context.Context, _ *sdk.CallToolRequest, _ noArgs) (*sdk.CallToolResult, any, error) {
		return plain(ctx, c, shell, obs, "workgraph_project_list", http.MethodGet, "/v1/projects", nil)
	})

	// --- spends money ------------------------------------------------------
	//
	// ReadOnlyHint false and DestructiveHint true so a client PROMPTS before
	// running them. The description says SPENDS MONEY in words as well, because
	// the annotation is a hint a client may ignore and the description is what
	// the model reads.

	sdk.AddTool(s, toolFileWork, func(ctx context.Context, _ *sdk.CallToolRequest, a fileWorkArgs) (*sdk.CallToolResult, any, error) {
		brief := strings.TrimSpace(a.Brief)
		if brief == "" {
			return errorResult("a brief is required, and it needs a checkable outcome"), nil, nil
		}
		body := map[string]any{"brief": brief}
		if p := strings.TrimSpace(a.Project); p != "" {
			body["project"] = p
		}
		return plain(ctx, c, shell, obs, "workgraph_file_work", http.MethodPost, "/v1/work/plan", body)
	})

	sdk.AddTool(s, toolProjectCreate, func(ctx context.Context, _ *sdk.CallToolRequest, a newProjectArgs) (*sdk.CallToolResult, any, error) {
		body := map[string]any{}
		for field, value := range map[string]string{
			"slug":          a.Slug,
			"name":          a.Name,
			"backup_owner":  a.BackupOwner,
			"primary_owner": a.PrimaryOwner,
			"portfolio":     a.Portfolio,
			"objective":     a.Objective,
			"visibility":    a.Visibility,
			"cell":          a.Cell,
		} {
			if v := strings.TrimSpace(value); v != "" {
				body[field] = v
			}
		}
		// Refused here rather than by the server, because the message a model
		// can act on is the one that names the missing field. The server
		// refuses these too — this is not the check, it is the better wording
		// of it.
		for _, required := range []string{"slug", "name", "backup_owner"} {
			if _, ok := body[required]; !ok {
				return errorResult("a project needs a " + required +
					"; the backup owner must be a different person from the primary " +
					"owner, so ask which colleague it should be rather than choosing one"), nil, nil
			}
		}
		return plain(ctx, c, shell, obs, "workgraph_project_create", http.MethodPost,
			"/v1/projects", body)
	})

	sdk.AddTool(s, toolRepositoriesAttach, func(ctx context.Context, _ *sdk.CallToolRequest, a attachArgs) (*sdk.CallToolResult, any, error) {
		slug := strings.TrimSpace(a.Slug)
		if slug == "" {
			return errorResult("a project slug is required"), nil, nil
		}
		repos := make([]string, 0, len(a.Repositories))
		for _, r := range a.Repositories {
			if r = strings.TrimSpace(r); r != "" {
				repos = append(repos, r)
			}
		}
		if len(repos) == 0 {
			return errorResult("name at least one repository, as owner/name"), nil, nil
		}
		return plain(ctx, c, shell, obs, "workgraph_repositories_attach", http.MethodPost,
			"/v1/projects/"+url.PathEscape(slug)+"/repositories",
			map[string]any{"repositories": repos})
	})

	sdk.AddTool(s, toolDispatch, func(ctx context.Context, _ *sdk.CallToolRequest, a dispatchArgs) (*sdk.CallToolResult, any, error) {
		bead := strings.TrimSpace(a.Bead)
		if bead == "" {
			return errorResult("a bead id is required"), nil, nil
		}
		// Escaped, because a bead id reaches a path segment. The CLI
		// interpolated it raw, which was safe only because the caller was the
		// same process that had just read it from the server.
		body := map[string]any{}
		if rig := strings.TrimSpace(a.Rig); rig != "" {
			body["rig"] = rig
		}
		return plain(ctx, c, shell, obs, "workgraph_dispatch", http.MethodPost,
			"/v1/work/"+url.PathEscape(bead)+"/dispatch", body)
	})

	return s
}

// plain runs one call and turns the answer into a tool result.
//
// The body is returned as text verbatim. It is already the compact shape
// `wg --json` prints, which matters on a phone: mobile clients render tool
// results inline, and the full OpenAPI object for a work list would fill the
// screen with fields nobody asked for.
func plain(ctx context.Context, c Caller, hasShell bool, obs Observer, tool, method, path string, body any) (*sdk.CallToolResult, any, error) {
	status, resp, err := c.Call(ctx, method, path, body)
	// Logged before the result is shaped, so a refusal is recorded whether or
	// not the model ever reads it.
	if obs != nil {
		obs(ctx, tool, Classify(status, resp), err)
	}
	if err != nil {
		// A transport fault rather than a decision. Still a readable result:
		// the model can say "Workgraph is unreachable" instead of failing
		// opaquely.
		return errorResult(err.Error()), nil, nil
	}
	if Classify(status, resp) != RefusalNone {
		return errorResult(refusalText(status, resp, hasShell)), nil, nil
	}
	return textResult(strings.TrimSpace(string(resp))), nil, nil
}
