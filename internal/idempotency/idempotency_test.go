package idempotency

import (
	"strings"
	"testing"
)

func TestDigestSeparatesDifferentBodies(t *testing.T) {
	a := Digest([]byte(`{"bead":"wg-1"}`))
	b := Digest([]byte(`{"bead":"wg-2"}`))
	if a == b {
		t.Fatal("two different requests hash the same, so a conflict would read as a replay")
	}
	if a != Digest([]byte(`{"bead":"wg-1"}`)) {
		t.Fatal("the same request hashes differently, so a replay would read as a conflict")
	}
}

// Whitespace is not semantics to a JSON parser, but it IS a different byte
// string. Recording that this is a conflict rather than a replay: the mechanism
// compares bytes, and a client that reformats its body between retries gets a
// 409 rather than a silently wrong replay.
func TestReformattedBodyIsTreatedAsDifferent(t *testing.T) {
	if Digest([]byte(`{"a":1}`)) == Digest([]byte(`{ "a": 1 }`)) {
		t.Fatal("reformatting changed nothing, which is not what byte comparison does")
	}
}

func TestOverlongKeyIsRefusedRatherThanTruncated(t *testing.T) {
	if ValidKey(strings.Repeat("k", MaxKeyLength+1)) {
		t.Fatal("an over-long key was accepted; truncating would make two keys collide, " +
			"which is the one thing this mechanism must never do")
	}
	if !ValidKey(strings.Repeat("k", MaxKeyLength)) {
		t.Fatal("a key at the limit was refused")
	}
	if ValidKey("") {
		t.Fatal("an empty key is not a key")
	}
}

func TestMaxKeyLengthMatchesTheMigration(t *testing.T) {
	if MaxKeyLength != 255 {
		t.Fatalf("MaxKeyLength is %d; 0042_idempotency.sql enforces 255", MaxKeyLength)
	}
}
