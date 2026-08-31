package workspace

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// sourcesJSON is the subscription shape established against the live API.
//
// Embedded rather than duplicated in Go. The event types here were established
// one at a time with validateOnly, because the documented list contains types a
// shared-drive target rejects — and the first deployment failed on three of four
// sources because the list was retyped into Go from memory as
// `permission.v3.updated` when the verified name is `permission.v3.edited`. A
// single wrong entry is not a degraded subscription: Google refuses the whole
// create, so the source silently never subscribes.
//
// So there is one copy, it lives next to the code that uses it, and it is the
// one that was tested against Google.
//
//go:embed sources.json
var sourcesJSON []byte

// VerifiedSources is the contents of sources.json.
type VerifiedSources struct {
	Drives []struct {
		Name       string `json:"name"`
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
		Why        string `json:"why"`
	} `json:"drives"`
	DriveSubscription struct {
		IncludeDescendants bool     `json:"includeDescendants"`
		EventTypes         []string `json:"eventTypes"`
		Verified           string   `json:"verified"`
	} `json:"drive_subscription"`
	Meet struct {
		SpaceCode    string `json:"space_code"`
		DisplayName  string `json:"display_name"`
		SpaceID      string `json:"space_id"`
		Visibility   string `json:"visibility"`
		Subscription struct {
			EventTypes []string `json:"eventTypes"`
			Verified   string   `json:"verified"`
		} `json:"subscription"`
	} `json:"meet"`
}

// LoadVerified reads the embedded file.
func LoadVerified() (VerifiedSources, error) {
	var v VerifiedSources
	if err := json.Unmarshal(sourcesJSON, &v); err != nil {
		return v, fmt.Errorf("reading the embedded source definitions: %w", err)
	}
	return v, nil
}

// VerifiedWants is the event types to subscribe to, from the embedded file.
//
// Refuses an empty set for either kind rather than returning one. An empty set
// makes every existing subscription look drifted, and drift is repaired by
// replacement — so the reconciler would delete and recreate on every pass while
// reporting success.
func VerifiedWants() (Wants, error) {
	v, err := LoadVerified()
	if err != nil {
		return nil, err
	}
	if len(v.DriveSubscription.EventTypes) == 0 {
		return nil, fmt.Errorf("sources.json lists no Drive event types")
	}
	if len(v.Meet.Subscription.EventTypes) == 0 {
		return nil, fmt.Errorf("sources.json lists no Meet event types")
	}
	if !v.DriveSubscription.IncludeDescendants {
		// Required and immutable for a shared drive. A file that said otherwise
		// would produce subscriptions Google refuses outright.
		return nil, fmt.Errorf("sources.json says includeDescendants is false, " +
			"which a shared-drive subscription is refused for")
	}
	return Wants{
		KindDrive: v.DriveSubscription.EventTypes,
		KindMeet:  v.Meet.Subscription.EventTypes,
	}, nil
}
