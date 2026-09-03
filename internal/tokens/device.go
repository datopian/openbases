package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/datopian/workgraph/internal/authz"
)

// The device authorization grant (RFC 8628), for a client that cannot hold a
// secret and cannot open a browser.
//
// Minting requires an interactive session, deliberately -- a token that could
// mint its own successor makes revocation meaningless. That left a sandboxed
// agent with no path at all. This adds one without weakening the rule: the
// agent asks, a PERSON approves in a browser, the agent collects. The human act
// stays human; only the typing moves.

// DeviceGrant is what a client is told when it starts a flow.
type DeviceGrant struct {
	// DeviceCode is the client's secret. It is returned once and stored only
	// as a digest, exactly like a token.
	DeviceCode string `json:"device_code"`
	// UserCode is what a person types. Short, because they type it.
	UserCode string `json:"user_code"`
	// VerificationURI is where they go. VerificationURIComplete carries the
	// code already filled in, which is what a client should offer first.
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	// Interval is the minimum seconds between polls. RFC 8628 calls a client
	// that ignores it a client that gets slow_down.
	Interval int `json:"interval"`
}

// Device flow states, in RFC 8628's vocabulary so a client can branch on the
// same strings it would use against any other device-flow server.
const (
	DevicePending = "authorization_pending"
	DeviceExpired = "expired_token"
	DeviceInvalid = "invalid_grant"
	DeviceSlow    = "slow_down"
)

// ErrDeviceNotApproved and friends are returned by Redeem for a grant that
// cannot produce a token yet or ever.
var (
	ErrDevicePending = errors.New(DevicePending)
	ErrDeviceExpired = errors.New(DeviceExpired)
	ErrDeviceInvalid = errors.New(DeviceInvalid)
)

// devicePrefix marks a device code so a misplaced one is recognisable in a log
// as a credential rather than as noise. Distinct from Prefix, because the two
// are not interchangeable and a client that sends one where the other belongs
// should get a clear refusal.
const devicePrefix = "wgd_"

// userCodeAlphabet omits I, O, 0 and 1.
//
// This code is read aloud, typed by hand, and sometimes photographed. The
// ambiguous characters are the difference between a flow that works and a
// support question, and the entropy lost is bought back by a ten-minute
// lifetime and a failure cap.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// StartDevice begins a flow. No credential is required, which is the point.
func (s *Store) StartDevice(ctx context.Context, clientLabel string, scopes []string, baseURL string) (DeviceGrant, error) {
	if strings.TrimSpace(clientLabel) == "" {
		return DeviceGrant{}, errors.New("a device grant needs a client label: it is what the approver sees")
	}
	if err := ValidateScopes(scopes); err != nil {
		return DeviceGrant{}, err
	}
	if scopes == nil {
		scopes = []string{}
	}

	secret, err := randomDeviceCode()
	if err != nil {
		return DeviceGrant{}, err
	}
	userCode, err := randomUserCode()
	if err != nil {
		return DeviceGrant{}, err
	}
	digest := sha256.Sum256([]byte(secret))

	const ttl = 10 * time.Minute
	var expires time.Time
	// No user session: the caller has no identity yet, which is the whole
	// reason this endpoint exists. The function is SECURITY DEFINER.
	err = s.db.QueryRowContext(ctx,
		`SELECT system_device_start($1, $2, $3, $4, $5::interval)`,
		digest[:], userCode, strings.TrimSpace(clientLabel), pgTextArray(scopes),
		fmt.Sprintf("%d seconds", int(ttl.Seconds())),
	).Scan(&expires)
	if err != nil {
		return DeviceGrant{}, fmt.Errorf("starting a device grant: %w", err)
	}

	base := strings.TrimRight(baseURL, "/")
	return DeviceGrant{
		DeviceCode:              secret,
		UserCode:                userCode,
		VerificationURI:         base + "/device",
		VerificationURIComplete: base + "/device?code=" + userCode,
		ExpiresIn:               int(ttl.Seconds()),
		// Five seconds, which is what the RFC suggests and what the client
		// scripts in this organisation already use.
		Interval: 5,
	}, nil
}

// ApproveDevice records a person's approval. The approver comes from the
// authenticated session, never from the request.
func (s *Store) ApproveDevice(ctx context.Context, userCode, approverID string) (bool, error) {
	if approverID == "" {
		return false, errors.New("approving a device grant needs a named person")
	}
	var ok bool
	err := authz.WithUser(ctx, s.db, approverID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT system_device_approve($1, $2::uuid)`, userCode, approverID).Scan(&ok)
	})
	if err != nil {
		return false, fmt.Errorf("approving a device grant: %w", err)
	}
	return ok, nil
}

// DeviceRequest is what the approval page shows before anybody clicks.
type DeviceRequest struct {
	ClientLabel string    `json:"client_label"`
	Scopes      []string  `json:"scopes"`
	ExpiresAt   time.Time `json:"expires_at"`
	Approved    bool      `json:"approved"`
}

// LookupDevice describes a pending grant to whoever is being asked to approve
// it.
//
// A failed lookup is counted before the error is returned, because a short
// code typed by a human is the weakest thing in this flow. The counting is a
// separate statement: the lookup function raises on a miss, and a raise rolls
// back, so an increment inside it would be undone by the very refusal it is
// meant to record.
func (s *Store) LookupDevice(ctx context.Context, userCode string) (DeviceRequest, error) {
	var r DeviceRequest
	var scopes []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT client_label, scopes::text, expires_at, already_approved
		   FROM system_device_lookup($1)`, userCode,
	).Scan(&r.ClientLabel, &scopes, &r.ExpiresAt, &r.Approved)
	if err != nil {
		if _, e := s.db.ExecContext(ctx, `SELECT system_device_note_failure($1)`, userCode); e != nil {
			// Worth knowing about, not worth failing over: the caller's answer
			// is "that code is not valid" either way.
			return DeviceRequest{}, fmt.Errorf("%w (and the failure count was not recorded: %v)", ErrNotFound, e)
		}
		return DeviceRequest{}, ErrNotFound
	}
	r.Scopes = parseTextArray(string(scopes))
	return r, nil
}

// RedeemDevice exchanges an approved device code for a token.
//
// The token is minted in the APPROVER's name, with the scopes they saw, and
// bound to the grant in a way that consumes it. Eight hours rather than the
// ninety-day maximum: this is a credential for one agent session, and the
// person approving it is not agreeing to three months.
func (s *Store) RedeemDevice(ctx context.Context, deviceCode string) (string, Token, error) {
	digest, ok := deviceDigest(deviceCode)
	if !ok {
		return "", Token{}, ErrDeviceInvalid
	}

	var state string
	var approver sql.NullString
	var scopes []byte
	var label sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT state, approver, scopes::text, coalesce(client_label,'')
		   FROM system_device_poll($1)`, digest[:],
	).Scan(&state, &approver, &scopes, &label)
	if err != nil {
		return "", Token{}, fmt.Errorf("polling a device grant: %w", err)
	}

	switch state {
	case DevicePending:
		return "", Token{}, ErrDevicePending
	case DeviceExpired:
		return "", Token{}, ErrDeviceExpired
	case DeviceInvalid:
		return "", Token{}, ErrDeviceInvalid
	case "approved":
	default:
		return "", Token{}, fmt.Errorf("unexpected device grant state %q", state)
	}
	if !approver.Valid {
		return "", Token{}, ErrDeviceInvalid
	}

	const deviceTokenLifetime = 8 * time.Hour
	secret, t, err := s.Mint(ctx, approver.String,
		"device: "+label.String, parseTextArray(string(scopes)),
		s.now().Add(deviceTokenLifetime), "device-flow")
	if err != nil {
		return "", Token{}, err
	}

	// Bind last. If this fails the token exists and the grant does not name it,
	// which is the safe direction: the client gets an error and retries, the
	// second attempt finds the grant unredeemed, and the orphan expires in
	// eight hours. The other order would hand out a credential the grant had
	// already been consumed for.
	var bound bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT system_device_redeem($1, $2::uuid)`, digest[:], t.ID).Scan(&bound); err != nil {
		return "", Token{}, fmt.Errorf("binding the minted token to the grant: %w", err)
	}
	if !bound {
		// Somebody redeemed it between the poll and the bind. Refuse rather
		// than return a token nothing points at.
		return "", Token{}, ErrDeviceInvalid
	}
	return secret, t, nil
}

func randomDeviceCode() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	return devicePrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomUserCode returns XXXX-XXXX from the unambiguous alphabet.
//
// rand.Int rather than modulo on a byte: 32 divides 256 evenly so modulo would
// happen to be uniform here, but that is a property of this alphabet's length
// and not of the code, and the next person to shorten the alphabet would not
// notice they had introduced a bias.
func randomUserCode() (string, error) {
	out := make([]byte, 0, 9)
	for i := 0; i < 8; i++ {
		if i == 4 {
			out = append(out, '-')
		}
		n, err := randIndex(len(userCodeAlphabet))
		if err != nil {
			return "", err
		}
		out = append(out, userCodeAlphabet[n])
	}
	return string(out), nil
}

// randIndex returns a uniform index below n.
//
// crypto/rand.Int rather than modulo on a byte. 32 divides 256 evenly, so
// modulo would be uniform for THIS alphabet -- and that is a property of the
// alphabet's length, not of the code. The next person to shorten it would
// introduce a bias with no way to notice.
func randIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("reading randomness: %w", err)
	}
	return int(v.Int64()), nil
}

func deviceDigest(secret string) ([32]byte, bool) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, devicePrefix) {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(secret)), true
}
