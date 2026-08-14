package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

var secret = []byte("a-webhook-secret-for-tests")

func signed(body string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidWebhookIsAccepted(t *testing.T) {
	body := `{"action":"opened"}`
	r := httptest.NewRequest("POST", "/v1/integrations/github/webhook", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", signed(body))
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	r.Header.Set("X-GitHub-Event", "pull_request")

	d, err := VerifyWebhook(r, secret)
	if err != nil {
		t.Fatalf("a correctly signed delivery must be accepted: %v", err)
	}
	if d.ID != "delivery-1" || d.Event != "pull_request" || string(d.Body) != body {
		t.Errorf("unexpected delivery: %+v", d)
	}
}

// The core property: a forged payload must be refused.
func TestForgedPayloadIsRejected(t *testing.T) {
	honest := `{"action":"opened"}`
	forged := `{"action":"closed"}`

	r := httptest.NewRequest("POST", "/x", strings.NewReader(forged))
	r.Header.Set("X-Hub-Signature-256", signed(honest)) // signature of a different body
	r.Header.Set("X-GitHub-Delivery", "delivery-1")

	if _, err := VerifyWebhook(r, secret); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a payload that does not match its signature must be rejected, got %v", err)
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	body := `{"action":"opened"}`
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", signed(body))
	r.Header.Set("X-GitHub-Delivery", "delivery-1")

	if _, err := VerifyWebhook(r, []byte("a-different-secret")); !errors.Is(err, ErrBadSignature) {
		t.Fatal("a delivery signed with another secret must be rejected")
	}
}

// An empty secret must never make everything verify.
func TestEmptySecretRefusesEverything(t *testing.T) {
	body := `{}`
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	r.Header.Set("X-GitHub-Delivery", "d")

	if _, err := VerifyWebhook(r, nil); !errors.Is(err, ErrBadSignature) {
		t.Fatal("an unconfigured secret must refuse, not accept")
	}
}

func TestMissingOrMalformedSignatureIsRejected(t *testing.T) {
	body := `{}`
	for name, sig := range map[string]string{
		"missing":      "",
		"no prefix":    strings.Repeat("a", 64),
		"wrong prefix": "sha1=" + strings.Repeat("a", 40),
		"not hex":      "sha256=zzzz",
		"short":        "sha256=abcd",
		"empty digest": "sha256=",
	} {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
		if sig != "" {
			r.Header.Set("X-Hub-Signature-256", sig)
		}
		r.Header.Set("X-GitHub-Delivery", "d")
		if _, err := VerifyWebhook(r, secret); err == nil {
			t.Errorf("%s signature must be rejected", name)
		}
	}
}

// Without a delivery ID a retry cannot be recognised, so the request is refused
// rather than risking double processing.
func TestDeliveryWithoutIDIsRejected(t *testing.T) {
	body := `{}`
	r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", signed(body))

	if _, err := VerifyWebhook(r, secret); !errors.Is(err, ErrBadSignature) {
		t.Fatal("a delivery with no ID must be rejected")
	}
}

func TestOversizePayloadIsRejected(t *testing.T) {
	big := strings.Repeat("x", maxPayload+10)
	r := httptest.NewRequest("POST", "/x", strings.NewReader(big))
	r.Header.Set("X-Hub-Signature-256", signed(big))
	r.Header.Set("X-GitHub-Delivery", "d")

	if _, err := VerifyWebhook(r, secret); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("an oversize payload must be refused, got %v", err)
	}
}
