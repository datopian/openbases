package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Events is the Google Workspace Events API, narrowed to what the subscription
// lifecycle needs.
//
// Written rather than generated because five calls do not justify a dependency
// that pulls in the whole of Google's client surface, and because every one of
// them has a failure mode worth naming where it happens.
type Events struct {
	// Token returns a delegated access token. A function rather than a string
	// because these expire in an hour and a reconciliation pass can outlive one.
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
	// BaseURL is overridable for tests only.
	BaseURL string
}

const eventsBase = "https://workspaceevents.googleapis.com/v1"

// GoogleSubscription is Google's view of a subscription, in the fields we use.
type GoogleSubscription struct {
	Name                 string   `json:"name,omitempty"`
	TargetResource       string   `json:"targetResource,omitempty"`
	EventTypes           []string `json:"eventTypes,omitempty"`
	State                string   `json:"state,omitempty"`
	SuspensionReason     string   `json:"suspensionReason,omitempty"`
	ExpireTime           string   `json:"expireTime,omitempty"`
	NotificationEndpoint *struct {
		PubsubTopic string `json:"pubsubTopic,omitempty"`
	} `json:"notificationEndpoint,omitempty"`
	DriveOptions *struct {
		IncludeDescendants bool `json:"includeDescendants,omitempty"`
	} `json:"driveOptions,omitempty"`
}

// Expiry parses Google's expireTime, or reports that there was not one.
func (g GoogleSubscription) Expiry() (time.Time, bool) {
	if g.ExpireTime == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, g.ExpireTime)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// State maps Google's state onto ours, refusing to guess.
//
// An unrecognised state is 'failed' rather than 'active', because the safe
// direction is to replace something we do not understand rather than to leave
// it running and assume it works.
func (g GoogleSubscription) LifecycleState() State {
	switch strings.ToUpper(g.State) {
	case "ACTIVE":
		return StateActive
	case "SUSPENDED":
		return StateSuspended
	case "DELETED":
		return StateDeleted
	default:
		return StateFailed
	}
}

// CreateRequest is what a new subscription needs.
type CreateRequest struct {
	Target     string
	EventTypes []string
	Topic      string
	// IncludeDescendants must be true for a shared drive and is IMMUTABLE, so
	// getting it wrong means replacing the subscription rather than fixing it.
	// Verified against all three drives on 2026-08-30: without it the API
	// refuses with "include_descendants must be true for SharedDrive
	// subscriptions".
	IncludeDescendants bool
}

func (e *Events) do(ctx context.Context, method, path string, body any, out any) error {
	tok, err := e.Token(ctx)
	if err != nil {
		return fmt.Errorf("getting a delegated token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	base := e.BaseURL
	if base == "" {
		base = eventsBase
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Named, because Google answers an unnamed client with a bot challenge that
	// reads exactly like an authentication failure.
	req.Header.Set("User-Agent", "workgraph-workspace/1")

	c := e.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// APIError carries Google's status and message.
//
// A type rather than a string because the caller's decision differs by status:
// a 404 on a subscription means recreate, a 403 means the delegation or scope
// is wrong and retrying will never help.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("google returned %d: %s", e.Status, body)
}

// NotFound reports whether the resource is gone, which is a recreate rather
// than a retry.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// Denied reports whether this will never succeed as configured — a missing
// scope, a delegation that was withdrawn, or a source we may not read.
func (e *APIError) Denied() bool {
	return e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized
}

// Create makes a subscription. validateOnly checks the request without creating
// anything, which is how the accepted event-type set was established.
func (e *Events) Create(ctx context.Context, r CreateRequest, validateOnly bool) (GoogleSubscription, error) {
	body := map[string]any{
		"targetResource":       r.Target,
		"eventTypes":           r.EventTypes,
		"notificationEndpoint": map[string]string{"pubsubTopic": r.Topic},
	}
	if r.IncludeDescendants {
		body["driveOptions"] = map[string]bool{"includeDescendants": true}
	}
	path := "/subscriptions"
	if validateOnly {
		path += "?validateOnly=true"
	}
	var op operation
	if err := e.do(ctx, http.MethodPost, path, body, &op); err != nil {
		return GoogleSubscription{}, err
	}
	if validateOnly {
		// Nothing was created, so there is no subscription to wait for and
		// nothing to return but the absence of an error.
		return GoogleSubscription{}, nil
	}
	return e.await(ctx, op)
}

// Get reads one subscription.
func (e *Events) Get(ctx context.Context, name string) (GoogleSubscription, error) {
	var out GoogleSubscription
	err := e.do(ctx, http.MethodGet, "/"+strings.TrimPrefix(name, "/"), nil, &out)
	return out, err
}

// Renew extends a subscription's expiry.
//
// A PATCH of ttl, not a new subscription: renewing keeps the id, so nothing
// that arrived under it is re-delivered. `ttl: "0s"` asks for the maximum the
// target allows, which is what we always want — a shorter one only means
// renewing more often.
func (e *Events) Renew(ctx context.Context, name string) (GoogleSubscription, error) {
	var op operation
	path := "/" + strings.TrimPrefix(name, "/") + "?updateMask=ttl"
	if err := e.do(ctx, http.MethodPatch, path, map[string]string{"ttl": "0s"}, &op); err != nil {
		return GoogleSubscription{}, err
	}
	return e.await(ctx, op)
}

// Reactivate revives a suspended subscription, keeping its id.
func (e *Events) Reactivate(ctx context.Context, name string) (GoogleSubscription, error) {
	var op operation
	if err := e.do(ctx, http.MethodPost, "/"+strings.TrimPrefix(name, "/")+":reactivate", map[string]any{}, &op); err != nil {
		return GoogleSubscription{}, err
	}
	return e.await(ctx, op)
}

// Delete removes a subscription. A 404 is success: the desired end state is
// that it does not exist, and it does not.
func (e *Events) Delete(ctx context.Context, name string) error {
	var op operation
	err := e.do(ctx, http.MethodDelete, "/"+strings.TrimPrefix(name, "/"), nil, &op)
	var apiErr *APIError
	if err != nil && asAPIError(err, &apiErr) && apiErr.NotFound() {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = e.await(ctx, op)
	return err
}

// List returns every subscription Google holds for this project.
//
// Used by reconciliation to find subscriptions we have no record of — the ones
// a crashed run created and never wrote down, which would otherwise deliver
// into a topic nobody is reconciling.
func (e *Events) List(ctx context.Context, filter string) ([]GoogleSubscription, error) {
	var out []GoogleSubscription
	page := ""
	for {
		q := url.Values{}
		if filter != "" {
			q.Set("filter", filter)
		}
		if page != "" {
			q.Set("pageToken", page)
		}
		var resp struct {
			Subscriptions []GoogleSubscription `json:"subscriptions"`
			NextPageToken string               `json:"nextPageToken"`
		}
		if err := e.do(ctx, http.MethodGet, "/subscriptions?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Subscriptions...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
}

// operation is a long-running operation.
//
// create, patch, reactivate and delete all return one of these rather than the
// subscription — verified against the v1 discovery document, which lists
// Operation as the response type for all four and Subscription only for get.
// Unmarshalling the Operation straight into a GoogleSubscription is silently
// wrong in the worst way: `name` comes back as "operations/<id>", so we would
// store an operation id as the subscription name, find no state and mark it
// failed, and replace the subscription on every pass while reporting success.
type operation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Error    *statusError    `json:"error"`
	Response json.RawMessage `json:"response"`
}

// statusError is google.rpc.Status.
type statusError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *statusError) Error() string {
	return fmt.Sprintf("google reported code %d: %s", s.Code, s.Message)
}

// opTimeout bounds how long we wait for an operation to finish. Bounded because
// a reconciliation pass runs on a timer: waiting forever means the next pass
// never starts, and a subscription that stops being reconciled stops being
// renewed.
const opTimeout = 90 * time.Second

// await waits for an operation and returns the subscription it produced.
//
// An empty response is not an error: delete's response is Empty, and
// validateOnly returns a done operation with nothing in it. Callers that need a
// subscription check for one; callers that do not, do not.
func (e *Events) await(ctx context.Context, op operation) (GoogleSubscription, error) {
	deadline := time.Now().Add(opTimeout)
	wait := 250 * time.Millisecond
	for !op.Done {
		if time.Now().After(deadline) {
			return GoogleSubscription{}, fmt.Errorf(
				"operation %s did not finish within %s; it may yet succeed, "+
					"so the next reconciliation must find it rather than create a second one",
				op.Name, opTimeout)
		}
		select {
		case <-ctx.Done():
			return GoogleSubscription{}, ctx.Err()
		case <-time.After(wait):
		}
		if wait < 4*time.Second {
			wait *= 2
		}
		var next operation
		if err := e.do(ctx, http.MethodGet, "/"+strings.TrimPrefix(op.Name, "/"), nil, &next); err != nil {
			return GoogleSubscription{}, fmt.Errorf("polling operation %s: %w", op.Name, err)
		}
		next.Name = op.Name // an in-progress poll may omit it
		op = next
	}
	if op.Error != nil {
		return GoogleSubscription{}, op.Error
	}
	var out GoogleSubscription
	if len(op.Response) == 0 || string(op.Response) == "{}" {
		return out, nil
	}
	if err := json.Unmarshal(op.Response, &out); err != nil {
		return out, fmt.Errorf("reading the subscription out of operation %s: %w", op.Name, err)
	}
	return out, nil
}

func asAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}
