package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"
	"time"
)

// Human rendering. Every one of these has a --json counterpart carrying the
// API's own shape; these exist so a person reading a terminal is not parsing
// JSON by eye, and never so an agent has to.

func newIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// A key that is not unique is worse than no key: it would make two
		// different requests collide and replay each other's responses.
		return ""
	}
	return "wg-cli-" + hex.EncodeToString(b)
}

func urlEscape(s string) string { return url.QueryEscape(s) }

func tw() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

func renderWhoami(body []byte) {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		os.Stdout.Write(body)
		return
	}
	for _, k := range []string{"user_id", "email", "subject", "service"} {
		if x, ok := v[k]; ok && x != nil && x != "" {
			fmt.Printf("%-10s %v\n", k, x)
		}
	}
}

func renderInbox(body []byte) {
	var v struct {
		Items []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Kind    string `json:"kind"`
			Project string `json:"project"`
			Status  string `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &v) != nil {
		os.Stdout.Write(body)
		return
	}
	if len(v.Items) == 0 {
		fmt.Println("nothing needs you")
		return
	}
	w := tw()
	fmt.Fprintln(w, "ID\tKIND\tPROJECT\tTITLE")
	for _, it := range v.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", short(it.ID), it.Kind, it.Project, it.Title)
	}
	w.Flush()
}

func renderAsk(body []byte) {
	var v map[string]any
	if json.Unmarshal(body, &v) != nil {
		os.Stdout.Write(body)
		return
	}
	// Asked nothing, the endpoint returns what it can answer. Printing that
	// plainly is the difference between a usable tool and one that 400s.
	if qs, ok := v["questions"].([]any); ok && v["answer"] == nil {
		fmt.Println("ask one of:")
		for _, q := range qs {
			fmt.Printf("  %v\n", q)
		}
		return
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
}

func renderWork(body []byte) {
	var v struct {
		Work []struct {
			Bead       string `json:"bead"`
			Title      string `json:"title"`
			Status     string `json:"status"`
			QueueState string `json:"queue_state"`
			SpentCents string `json:"spent_cents"`
		} `json:"work"`
	}
	if json.Unmarshal(body, &v) != nil {
		os.Stdout.Write(body)
		return
	}
	if len(v.Work) == 0 {
		fmt.Println("no work")
		return
	}
	w := tw()
	fmt.Fprintln(w, "BEAD\tSTATUS\tQUEUE\tSPENT\tTITLE")
	for _, it := range v.Work {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", it.Bead, it.Status, it.QueueState, it.SpentCents, it.Title)
	}
	w.Flush()
}

func renderProjects(body []byte) {
	var v struct {
		Projects []struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if json.Unmarshal(body, &v) != nil {
		os.Stdout.Write(body)
		return
	}
	w := tw()
	fmt.Fprintln(w, "SLUG\tNAME")
	for _, p := range v.Projects {
		fmt.Fprintf(w, "%s\t%s\n", p.Slug, p.Name)
	}
	w.Flush()
}

func renderTokens(body []byte) {
	var v struct {
		Tokens []struct {
			ID        string   `json:"id"`
			Label     string   `json:"label"`
			Scopes    []string `json:"scopes"`
			ExpiresAt string   `json:"expires_at"`
			Live      bool     `json:"live"`
		} `json:"tokens"`
	}
	if json.Unmarshal(body, &v) != nil {
		os.Stdout.Write(body)
		return
	}
	w := tw()
	fmt.Fprintln(w, "ID\tLIVE\tEXPIRES\tLABEL")
	for _, t := range v.Tokens {
		exp := t.ExpiresAt
		if p, err := time.Parse(time.RFC3339, exp); err == nil {
			exp = p.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%v\t%s\t%s\n", short(t.ID), t.Live, exp, t.Label)
	}
	w.Flush()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func usage() {
	fmt.Fprint(os.Stderr, `wg — Workgraph over HTTP

  wg login [base-url]         store a token (WG_TOKEN, or piped on stdin)
  wg whoami                   who this credential is
  wg inbox                    what needs me
  wg ask ["question"]         ask the chief of staff; no question lists what it answers
  wg work list|queue          what work exists, and what it cost
  wg work plan "a brief"      queue a planning job
  wg work dispatch <bead>     run one bead
  wg project list|show <slug>
  wg tokens list              this credential's siblings
  wg spec                     the OpenAPI contract

  --json                      the API's own shape, on any command

Exit codes: 0 ok, 2 usage, 3 unauthenticated, 4 forbidden, 5 refused by policy,
6 rate limited, 7 conflict, 1 other.
`)
}
