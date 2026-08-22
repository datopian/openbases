package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	// ErrBadSignature covers a missing, malformed, or incorrect signature.
	// They are one error on purpose: distinguishing them tells a forger which
	// part of their attempt to change.
	ErrBadSignature = errors.New("webhook signature verification failed")

	// ErrPayloadTooLarge bounds what an unauthenticated caller can make us
	// read. The endpoint is reachable by anyone who learns the URL, and
	// signature checking happens after the body is read.
	ErrPayloadTooLarge = errors.New("webhook payload too large")
)

// maxPayload is GitHub's documented 25 MB ceiling.
const maxPayload = 25 << 20

// Delivery is a verified webhook.
type Delivery struct {
	// ID is GitHub's X-GitHub-Delivery, the idempotency key. GitHub retries,
	// and a retried delivery must not be processed twice (plan section 9.3).
	ID    string
	Event string
	Body  []byte
}

// VerifyWebhook authenticates an inbound webhook and returns the delivery.
//
// The signature is checked BEFORE the payload is parsed. Parsing first would
// mean running a JSON decoder over attacker-controlled input on an endpoint
// that anyone can reach.
//
// Several secrets may be supplied, and any one of them authenticates the
// delivery. That exists to remove an outage window rather than for convenience:
// GitHub signs with exactly one secret and this endpoint used to accept exactly
// one, so during a rotation every delivery was rejected until both sides agreed.
// No ordering avoids it — an early draft of the rotation script claimed
// deploying first did, which is simply wrong. Accepting the previous secret as
// well makes the rotation: add the old value as PREVIOUS, deploy, change GitHub,
// then drop PREVIOUS on the next deploy. No window at any point.
//
// It is not data loss without this — GitHub retries, and the Advanced tab can
// redeliver — but it made a routine rotation feel risky enough to postpone,
// which is how a credential goes unrotated.
func VerifyWebhook(r *http.Request, secrets ...[]byte) (*Delivery, error) {
	usable := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		if len(s) > 0 {
			usable = append(usable, s)
		}
	}
	if len(usable) == 0 {
		// Refusing to run without a secret is deliberate: an empty secret would
		// make every forged request verify.
		return nil, fmt.Errorf("%w: no webhook secret configured", ErrBadSignature)
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		return nil, ErrBadSignature
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayload+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPayload {
		return nil, ErrPayloadTooLarge
	}

	if !validSignature(body, signature, usable) {
		return nil, ErrBadSignature
	}

	id := r.Header.Get("X-GitHub-Delivery")
	if id == "" {
		// Without a delivery ID there is no idempotency key, so a retry could
		// not be recognised. Better to refuse than to process something twice.
		return nil, fmt.Errorf("%w: delivery has no ID", ErrBadSignature)
	}

	return &Delivery{
		ID:    id,
		Event: r.Header.Get("X-GitHub-Event"),
		Body:  body,
	}, nil
}

// validSignature compares in constant time against every candidate secret.
//
// A byte-by-byte comparison leaks, through timing, how many leading bytes were
// correct, which turns forging a signature into a per-byte search. Plan section
// 9.3 requires constant-time comparison for exactly this reason.
//
// EVERY secret is tried, always, and the results are accumulated without a
// branch. Returning as soon as one matches would make a delivery signed with the
// current secret measurably faster than one signed with the previous, which
// tells an observer which secret they hold — and during a rotation that is
// exactly the thing worth not telling them.
func validSignature(body []byte, signature string, secrets [][]byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, prefix))
	if err != nil {
		return false
	}

	var matched int
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		// Bitwise OR rather than || : the logical operator short-circuits, so
		// once one secret matched the rest would not be computed, and the work
		// done would depend on which secret was correct.
		matched |= subtle.ConstantTimeCompare(provided, mac.Sum(nil))
	}
	return matched == 1
}
