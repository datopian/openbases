package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// The client half of the device authorization grant (wg-8la, RFC 8628).
//
// For a machine that cannot be handed a secret: a sandbox with no environment
// variables, a container that resets, an agent that must not be given somebody
// else's credential through a chat transcript. It asks, a person approves in a
// browser, it collects.
//
// Nothing here opens a browser. The URL is printed and the person opens it,
// because the whole reason this exists is that the caller may have no browser
// to open — and a client that tries and fails silently is worse than one that
// prints a link.

type deviceGrant struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// loginDevice runs the flow and stores the token it collects.
func loginDevice(base string) (int, error) {
	base = strings.TrimRight(base, "/")

	label := "wg on " + hostLabel()
	start, err := postJSON(base+"/v1/device/code", map[string]any{
		"client_label": label,
		// What the agent needs to be useful and nothing more. A person
		// approving this sees the list, so asking for less is worth doing.
		"scopes": []string{"project.read", "work.create", "agent.dispatch", "organisation.read"},
	})
	if err != nil {
		return exitError, err
	}
	var g deviceGrant
	if err := json.Unmarshal(start, &g); err != nil {
		return exitError, fmt.Errorf("reading the device grant: %w", err)
	}
	if g.DeviceCode == "" || g.UserCode == "" {
		return exitError, errors.New("the server returned no device grant")
	}

	// To stderr, so `wg login --device` can have its output piped without the
	// instructions ending up in whatever consumes it.
	fmt.Fprintf(os.Stderr, "\nOpen this and approve:\n\n  %s\n\n", g.VerificationURIComplete)
	fmt.Fprintf(os.Stderr, "If the link does not carry the code, open %s and enter:\n\n  %s\n\n",
		g.VerificationURI, g.UserCode)
	fmt.Fprintf(os.Stderr, "Waiting… (expires in %d minutes)\n", g.ExpiresIn/60)

	interval := time.Duration(g.Interval) * time.Second
	if interval < 2*time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(g.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		body, err := postJSON(base+"/v1/device/token", map[string]any{"device_code": g.DeviceCode})
		if err == nil {
			var out struct {
				AccessToken string `json:"access_token"`
				ExpiresIn   int    `json:"expires_in"`
				Scope       string `json:"scope"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				return exitError, fmt.Errorf("reading the token: %w", err)
			}
			if out.AccessToken == "" {
				return exitError, errors.New("the server approved the grant and returned no token")
			}
			if err := storeCredentials(base, out.AccessToken); err != nil {
				return exitError, err
			}
			fmt.Fprintf(os.Stderr, "\nApproved. Token stored, valid for %d hours, scopes: %s\n",
				out.ExpiresIn/3600, out.Scope)
			return exitOK, nil
		}

		// RFC 8628 states: pending is the normal answer for most of this
		// flow's life, slow_down means back off, and the other two are final.
		switch {
		case strings.Contains(err.Error(), "authorization_pending"):
			continue
		case strings.Contains(err.Error(), "slow_down"):
			interval += 5 * time.Second
		case strings.Contains(err.Error(), "expired_token"):
			return exitError, errors.New("the code expired before anybody approved it; run `wg login --device` again")
		case strings.Contains(err.Error(), "invalid_grant"):
			return exitError, errors.New("that grant is no longer valid; run `wg login --device` again")
		default:
			return exitError, err
		}
	}
	return exitError, errors.New("the code expired before anybody approved it; run `wg login --device` again")
}

// postJSON returns the body on 2xx and an error carrying the server's own
// wording otherwise, because the device flow's states arrive as error strings
// and the caller branches on them.
func postJSON(url string, body any) ([]byte, error) {
	enc, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(enc))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := readAllLimited(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
			Hint  string `json:"hint"`
		}
		_ = json.Unmarshal(out, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		if e.Hint != "" {
			return nil, fmt.Errorf("%s: %s", e.Error, e.Hint)
		}
		return nil, errors.New(e.Error)
	}
	return out, nil
}

// hostLabel is what the approver sees as the requesting client.
//
// Untrusted by the server, and shown anyway: a person deciding whether to
// approve deserves to know what asked, and "wg on some-container" is more
// use than "a client".
func hostLabel() string {
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return h
	}
	return "an unnamed machine"
}
