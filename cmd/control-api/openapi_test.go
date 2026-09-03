package main

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The contract test wg-p4h.5 exists for.
//
// A spec that drifts is worse than no spec, because a client author trusts it.
// This fails in BOTH directions: a route the document does not describe, and a
// document entry no route serves. The first sends an integrator looking for a
// bug in their own code; the second reads as coverage for something that does
// not exist.
//
// The repository already carries the scar this prevents. The /v1/inbox handler
// has a comment about an envelope on one endpoint and a bare array on another
// making the projects table render empty while the server returned all three
// rows — one team, one client, one afternoon. With external clients that is a
// support thread.

const specPath = "../../internal/apispec/openapi.json"

type openAPI struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]operation `json:"paths"`
	Components struct {
		Schemas         map[string]json.RawMessage `json:"schemas"`
		SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
	} `json:"components"`
}

type operation struct {
	Summary     string                     `json:"summary"`
	Description string                     `json:"description"`
	OperationID string                     `json:"operationId"`
	Responses   map[string]json.RawMessage `json:"responses"`
	Parameters  []struct {
		Name string `json:"name"`
		In   string `json:"in"`
	} `json:"parameters"`
}

func loadSpec(t *testing.T) openAPI {
	t.Helper()
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}
	var s openAPI
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s is not valid JSON: %v", specPath, err)
	}
	return s
}

// specOperations returns "METHOD /path" for everything the document describes.
func specOperations(t *testing.T) []string {
	t.Helper()
	spec := loadSpec(t)
	var out []string
	for path, ops := range spec.Paths {
		for method := range ops {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

func TestSpecAndMuxAgree(t *testing.T) {
	live := registered(t)
	documented := specOperations(t)

	inSpec := map[string]bool{}
	for _, o := range documented {
		inSpec[o] = true
	}
	inMux := map[string]bool{}
	for _, o := range live {
		inMux[o] = true
	}

	var undocumented, stale []string
	for _, r := range live {
		if !inSpec[r] {
			undocumented = append(undocumented, r)
		}
	}
	for _, o := range documented {
		if !inMux[o] {
			stale = append(stale, o)
		}
	}

	if len(undocumented) > 0 {
		t.Errorf("routes the API serves but internal/apispec/openapi.json does not describe:\n  %s\n\n"+
			"A client author reading the spec will not find these, and will assume they do not exist.",
			strings.Join(undocumented, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("described in internal/apispec/openapi.json but served by nothing:\n  %s\n\n"+
			"This reads as coverage for an endpoint that does not exist.",
			strings.Join(stale, "\n  "))
	}
}

func TestSpecIsOpenAPI31WithTheStructureAClientNeeds(t *testing.T) {
	spec := loadSpec(t)
	if !strings.HasPrefix(spec.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", spec.OpenAPI)
	}
	if spec.Info.Title == "" || spec.Info.Version == "" {
		t.Error("info.title and info.version are required")
	}
	if len(spec.Paths) == 0 {
		t.Fatal("the document describes no paths")
	}
	if _, ok := spec.Components.Schemas["Error"]; !ok {
		t.Error("no Error schema: every endpoint can fail, and a client needs the shape")
	}
	if _, ok := spec.Components.SecuritySchemes["bearerToken"]; !ok {
		t.Error("no bearerToken security scheme, which is the whole point of an API a tool can reach")
	}
}

// Every operation must say what it does and what can go wrong. An entry with a
// path and nothing else documents that a URL exists, which a client author
// could have guessed.
func TestEveryOperationIsDescribedAndCanFail(t *testing.T) {
	spec := loadSpec(t)
	for path, ops := range spec.Paths {
		for method, op := range ops {
			where := strings.ToUpper(method) + " " + path
			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("%s has no summary", where)
			}
			if op.OperationID == "" {
				t.Errorf("%s has no operationId; generators need one", where)
			}
			if _, public := unauthenticated[where]; !public {
				for _, code := range []string{"401", "403"} {
					if _, ok := op.Responses[code]; !ok {
						t.Errorf("%s documents no %s response; every endpoint behind the chain can return one", where, code)
					}
				}
			}
			ok2xx := false
			for code := range op.Responses {
				if strings.HasPrefix(code, "2") {
					ok2xx = true
				}
			}
			if !ok2xx {
				t.Errorf("%s documents no success response", where)
			}
		}
	}
}

// Writes that a program retries must document the retry contract rather than
// leaving a client to discover 409 in production.
//
// notIdempotent lists the writes where that contract does not apply, each with
// the reason. An exemption is a claim about the endpoint's nature, so it needs
// one -- and the alternative was documenting a header the handler ignores,
// which is worse than an undocumented one: a client would set it and believe
// something.
// unauthenticated lists the routes registered on the plain mux rather than
// behind the auth chain, each with the reason it has to be reachable without a
// credential.
//
// This guard assumed every documented route was authenticated, which was true
// until the device flow. Documenting 401 and 403 on a route that cannot return
// them would be as wrong as omitting them from one that can -- and the list
// itself is the useful part: a route appearing here is a claim that deserves
// reading, because it is a way in without a credential.
var unauthenticated = map[string]string{
	"POST /v1/device/code":  "RFC 8628: the caller has no credential yet, which is the point of the flow",
	"POST /v1/device/token": "RFC 8628: polled with the device code, which is itself the secret",
	// Both pre-existing, and both invisible until the guard was widened to the
	// plain mux. Neither was documented at all.
	"GET /v1/openapi.json":                 "the contract itself, discoverable before a client has a credential",
	"POST /v1/integrations/github/webhook": "called by GitHub, authenticated by HMAC rather than by Access",
}

var notIdempotent = map[string]string{
	// The device flow (RFC 8628, wg-8la).
	//
	// /device/token is DESIGNED to be polled: it answers authorization_pending
	// repeatedly and then, once, a token. An idempotency key would cache the
	// first answer and the flow would never complete -- the cache would be
	// working exactly as intended and the feature would be broken.
	"POST /v1/device/token": "polled by contract; caching the first answer would break the flow",
	// The grant is single-use in the database, so a repeated approval is
	// already refused by the same UPDATE that consumes it. A key would add a
	// second, weaker guard in front of a correct one.
	"POST /v1/device/approve": "single-use in the schema; a key would duplicate a stronger guard",
	// No credential exists yet, so there is nobody to scope a key to, and the
	// grant it creates is worthless without the device code it returns once.
	"POST /v1/device/code": "unauthenticated, and each call creates a distinct grant",
	// GitHub decides its own retries and sends its own delivery id; an
	// Idempotency-Key header is not something it can be asked to set.
	"POST /v1/integrations/github/webhook": "the caller is GitHub, which sets its own delivery id",
}

func TestWritesDocumentIdempotencyAndRateLimiting(t *testing.T) {
	spec := loadSpec(t)
	for path, ops := range spec.Paths {
		for method, op := range ops {
			if method == "get" {
				continue
			}
			where := strings.ToUpper(method) + " " + path
			if _, exempt := notIdempotent[where]; exempt {
				continue
			}

			hasKey := false
			for _, p := range op.Parameters {
				if p.In == "header" && p.Name == "Idempotency-Key" {
					hasKey = true
				}
			}
			if !hasKey {
				t.Errorf("%s does not document Idempotency-Key; a retrying client cannot know it is safe", where)
			}
			for _, code := range []string{"409", "429"} {
				if _, ok := op.Responses[code]; !ok {
					t.Errorf("%s documents no %s response", where, code)
				}
			}
		}
	}
}

// The error codes the API actually emits must all appear in the enum a client
// reads. A code that only exists in Go is one a client cannot branch on.
func TestEveryEmittedErrorCodeIsDocumented(t *testing.T) {
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	spec := string(b)

	var emitted []string
	entries, _ := os.ReadDir(".")
	re := regexp.MustCompile(`"code":\s*"([a-z_]+)"`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			emitted = append(emitted, m[1])
		}
	}
	if len(emitted) == 0 {
		t.Fatal("found no error codes in the package; the pattern this test scans for has changed")
	}

	seen := map[string]bool{}
	for _, code := range emitted {
		if seen[code] {
			continue
		}
		seen[code] = true
		if !strings.Contains(spec, `"`+code+`"`) {
			t.Errorf("the API emits code %q, which the spec's enum does not list; "+
				"a client cannot branch on a code it has never been told about", code)
		}
	}
}

// /mcp is served and is deliberately NOT in the OpenAPI document (wg-p4h.11).
//
// The spec describes a REST surface: paths, methods, and JSON bodies a client
// generator can turn into functions. /mcp is a JSON-RPC transport behind one
// path, where the interesting contract is the tool list — which MCP clients
// discover by calling tools/list, not by reading OpenAPI. Describing it as a
// REST route would produce a generated client with one method called `postMcp`
// taking `any`, which is worse than an honest omission.
//
// It is asserted rather than left implicit because of HOW it currently escapes
// the agreement test: TestSpecAndMuxAgree scans for `HandleFunc("METHOD /path"`
// and /mcp is registered with `mux.Handle("/mcp", …)` — no method prefix — so
// it is invisible to the scanner by accident. A later refactor to HandleFunc
// would break that test for a reason nobody could see. This test is the
// decision written down: /mcp is served, and it is not in the spec.
func TestMCPIsServedAndDeliberatelyAbsentFromTheSpec(t *testing.T) {
	spec := loadSpec(t)
	for path := range spec.Paths {
		if strings.HasPrefix(path, "/mcp") {
			t.Errorf("the spec describes %q; /mcp is a JSON-RPC transport and its "+
				"contract is the tool list, discovered with tools/list", path)
		}
	}

	// And it really is served, so this test cannot pass by the route having
	// been deleted.
	src, err := os.ReadFile("mcp.go")
	if err != nil {
		t.Fatalf("reading mcp.go: %v", err)
	}
	if !strings.Contains(string(src), `mux.Handle("/mcp"`) {
		t.Error("mcp.go no longer registers /mcp; if the transport moved, this test should move with it")
	}
}
