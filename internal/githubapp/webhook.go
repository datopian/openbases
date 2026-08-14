package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
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
func VerifyWebhook(r *http.Request, secret []byte) (*Delivery, error) {
	if len(secret) == 0 {
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

	if !validSignature(body, signature, secret) {
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

// validSignature compares in constant time.
//
// A byte-by-byte comparison leaks, through timing, how many leading bytes were
// correct, which turns forging a signature into a per-byte search. Plan section
// 9.3 requires constant-time comparison for exactly this reason.
func validSignature(body []byte, signature string, secret []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, prefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}
