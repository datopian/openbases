package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Meet is the Google Meet REST API, narrowed to reading what a finished
// conference produced.
//
// Every method here is covered by the meetings.space.readonly scope already
// granted to the delegation -- checked against the v2 discovery document rather
// than assumed, because a missing scope is a 403 at run time and nowhere else.
type Meet struct {
	Token   func(ctx context.Context) (string, error)
	HTTP    *http.Client
	BaseURL string // tests only
}

const meetBase = "https://meet.googleapis.com/v2"

// ConferenceRecord is one held conference.
type ConferenceRecord struct {
	Name      string `json:"name"`
	Space     string `json:"space"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
}

// Transcript is one transcript of a conference.
type Transcript struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	StartTime       string `json:"startTime"`
	EndTime         string `json:"endTime"`
	DocsDestination *struct {
		Document  string `json:"document"`
		ExportURI string `json:"exportUri"`
	} `json:"docsDestination"`
}

// Ready reports whether the transcript is finished and readable.
//
// A transcript still being written has entries, and reading it then produces a
// partial record that looks complete. FILE_GENERATED is the state that says
// Google has finished.
func (t Transcript) Ready() bool { return t.State == "FILE_GENERATED" }

// TranscriptEntry is one utterance.
type TranscriptEntry struct {
	Name         string `json:"name"`
	Participant  string `json:"participant"`
	Text         string `json:"text"`
	LanguageCode string `json:"languageCode"`
	StartTime    string `json:"startTime"`
	EndTime      string `json:"endTime"`
}

// Participant is somebody who was in the conference.
type Participant struct {
	Name          string `json:"name"`
	EarliestStart string `json:"earliestStartTime"`
	LatestEnd     string `json:"latestEndTime"`
	SignedinUser  *struct {
		User        string `json:"user"`
		DisplayName string `json:"displayName"`
	} `json:"signedinUser"`
	AnonymousUser *struct {
		DisplayName string `json:"displayName"`
	} `json:"anonymousUser"`
	PhoneUser *struct {
		DisplayName string `json:"displayName"`
	} `json:"phoneUser"`
}

// Identity reports who this participant is, and whether they are identifiable.
//
// The three shapes are not interchangeable. A signed-in user has a directory
// identity that can be matched to a Workgraph user; an anonymous or phone
// participant has a display name they chose. Recording the difference matters
// because an ACL entry naming "Nikola's Personal Assistant" is a claim about a
// string, not about a person.
func (p Participant) Identity() (name string, kind string) {
	switch {
	case p.SignedinUser != nil:
		return p.SignedinUser.DisplayName, "user"
	case p.PhoneUser != nil:
		return p.PhoneUser.DisplayName, "phone"
	case p.AnonymousUser != nil:
		return p.AnonymousUser.DisplayName, "anonymous"
	}
	return "", "unknown"
}

func (m *Meet) get(ctx context.Context, path string, q url.Values, out any) error {
	tok, err := m.Token(ctx)
	if err != nil {
		return fmt.Errorf("getting a delegated token: %w", err)
	}
	base := m.BaseURL
	if base == "" {
		base = meetBase
	}
	u := base + "/" + strings.TrimPrefix(path, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// Named, for the same reason the Workspace Events client is: Google answers
	// an unnamed client with a bot challenge that reads like an auth failure.
	req.Header.Set("User-Agent", "workgraph-workspace/1")

	c := m.HTTP
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := make([]byte, 2000)
		n, _ := resp.Body.Read(body)
		return &APIError{Status: resp.StatusCode, Body: string(body[:n])}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ConferenceRecords lists the conferences held in a space, newest first.
func (m *Meet) ConferenceRecords(ctx context.Context, space string) ([]ConferenceRecord, error) {
	var out []ConferenceRecord
	page := ""
	for {
		q := url.Values{}
		// The filter is EBNF and the quotes are part of it.
		q.Set("filter", fmt.Sprintf("space.name=%q", space))
		if page != "" {
			q.Set("pageToken", page)
		}
		var resp struct {
			ConferenceRecords []ConferenceRecord `json:"conferenceRecords"`
			NextPageToken     string             `json:"nextPageToken"`
		}
		if err := m.get(ctx, "conferenceRecords", q, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.ConferenceRecords...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
}

// Transcripts lists a conference's transcripts.
func (m *Meet) Transcripts(ctx context.Context, record string) ([]Transcript, error) {
	var resp struct {
		Transcripts []Transcript `json:"transcripts"`
	}
	err := m.get(ctx, record+"/transcripts", nil, &resp)
	return resp.Transcripts, err
}

// Participants lists who was in a conference.
func (m *Meet) Participants(ctx context.Context, record string) ([]Participant, error) {
	var out []Participant
	page := ""
	for {
		q := url.Values{}
		if page != "" {
			q.Set("pageToken", page)
		}
		var resp struct {
			Participants  []Participant `json:"participants"`
			NextPageToken string        `json:"nextPageToken"`
		}
		if err := m.get(ctx, record+"/participants", q, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Participants...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
}

// Entries reads a transcript in full, in order.
//
// Paged deliberately rather than capped: a transcript truncated at an arbitrary
// page is worse than none, because everything downstream would treat a partial
// meeting as the whole meeting and nothing in the record would say otherwise.
func (m *Meet) Entries(ctx context.Context, transcript string) ([]TranscriptEntry, error) {
	var out []TranscriptEntry
	page := ""
	for {
		q := url.Values{"pageSize": {"1000"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var resp struct {
			TranscriptEntries []TranscriptEntry `json:"transcriptEntries"`
			NextPageToken     string            `json:"nextPageToken"`
		}
		if err := m.get(ctx, transcript+"/entries", q, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.TranscriptEntries...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		page = resp.NextPageToken
	}
}
