package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/datopian/openbases/internal/cost"
)

const defaultAPIBase = "https://api.cloudflare.com/client/v4"

// gatewayClient reads AI Gateway logs.
type gatewayClient struct {
	Account string
	Token   string
	HTTP    *http.Client
	// Base is the API root. Configurable so the acceptance test can point the
	// real binary at a stub and exercise paging, the stop at `since` and the
	// error paths — none of which can be arranged against the live gateway,
	// whose log is whatever it happens to contain today.
	Base string
}

// page is one response from the logs endpoint.
type page struct {
	Success bool         `json:"success"`
	Errors  []any        `json:"errors"`
	Result  []cost.Entry `json:"result"`
}

// logsSince pages the gateway log newest-first and stops at `since`.
//
// Newest-first with an explicit stop, rather than asking the API for a range:
// the endpoint has no reliable date filter, and reading forward from the
// beginning would re-read the whole retained log on every run. cap bounds a
// first import of a gateway that has never been read, so one run cannot page
// through ten million entries.
//
// Returns the entries and whether the walk reached `since` rather than hitting
// cap. A caller that stopped at cap has NOT imported everything and must say so.
func (g gatewayClient) logsSince(ctx context.Context, gateway string, since time.Time, cap int) ([]cost.Entry, bool, error) {
	var out []cost.Entry
	// 50 is the API maximum; asking for more is a 400, not a clamp.
	const perPage = 50

	for p := 1; len(out) < cap; p++ {
		q := url.Values{
			"per_page":           {fmt.Sprint(perPage)},
			"page":               {fmt.Sprint(p)},
			"order_by":           {"created_at"},
			"order_by_direction": {"desc"},
		}
		base := g.Base
		if base == "" {
			base = defaultAPIBase
		}
		endpoint := fmt.Sprintf("%s/accounts/%s/ai-gateway/gateways/%s/logs?%s",
			strings.TrimSuffix(base, "/"), url.PathEscape(g.Account), url.PathEscape(gateway), q.Encode())

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return out, false, err
		}
		req.Header.Set("Authorization", "Bearer "+g.Token)

		resp, err := g.HTTP.Do(req)
		if err != nil {
			return out, false, fmt.Errorf("gateway %s page %d: %w", gateway, p, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return out, false, fmt.Errorf("gateway %s page %d: %w", gateway, p, err)
		}
		if resp.StatusCode != http.StatusOK {
			return out, false, fmt.Errorf("gateway %s page %d: HTTP %d: %s", gateway, p, resp.StatusCode, truncate(string(body), 200))
		}

		var pg page
		if err := json.Unmarshal(body, &pg); err != nil {
			return out, false, fmt.Errorf("gateway %s page %d: %w", gateway, p, err)
		}
		if !pg.Success {
			return out, false, fmt.Errorf("gateway %s page %d: %s", gateway, p, truncate(string(body), 200))
		}
		if len(pg.Result) == 0 {
			// The end of the log. Everything down to `since` was read.
			return out, true, nil
		}

		for _, e := range pg.Result {
			// A malformed timestamp must not end the walk: it is one bad entry,
			// and stopping here would silently truncate the import. Keep it and
			// let the mapping decide.
			t, err := time.Parse(time.RFC3339, e.CreatedAt)
			if err == nil && !t.After(since) {
				return out, true, nil
			}
			out = append(out, e)
		}
	}
	return out, false, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
