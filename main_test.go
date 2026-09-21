package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

var testSeed = []byte("test-seed-do-not-use-in-production")

func lpString(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }

// Regression guard: if this vector ever changes, the signed-bytes format
// changed and every previously issued voting link would stop verifying.
func TestVotingTokenKnownAnswer(t *testing.T) {
	const want = "GvDkDqw9gbQM4zQAIkzDTsADsb20NgwsTtTj0laDQ5g"
	if got := votingToken(testSeed, "alex@example.com"); got != want {
		t.Fatalf("votingToken = %q, want %q (signed-bytes format changed?)", got, want)
	}
}

// votingToken must equal an independent implementation of the documented
// construction: base64url(HMAC-SHA256(seed, LP("vote") ‖ LP(identity))). This is
// the contract a server implementer relies on.
func TestVotingTokenMatchesSpec(t *testing.T) {
	for _, id := range []string{"alex@example.com", "b@c.io", "", "unicodé@x.com"} {
		mac := hmac.New(sha256.New, testSeed)
		mac.Write([]byte(lpString("vote") + lpString(id)))
		want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if got := votingToken(testSeed, id); got != want {
			t.Errorf("identity %q: votingToken=%q spec=%q", id, got, want)
		}
	}
}

// votingToken must sign its input verbatim — no internal normalization — so the
// caller's normalization is the single source of truth.
func TestVotingTokenSignsVerbatim(t *testing.T) {
	lower := votingToken(testSeed, "alex@example.com")
	if upper := votingToken(testSeed, "Alex@Example.com"); upper == lower {
		t.Fatal("votingToken normalized case; it must sign the identity verbatim")
	}
	if again := votingToken(testSeed, "alex@example.com"); again != lower {
		t.Fatal("votingToken is not deterministic")
	}
	if votingToken(testSeed, "a@x.com") == votingToken(testSeed, "b@x.com") {
		t.Fatal("distinct identities produced the same token")
	}
}

// serverValidToken mirrors the README's server-side recipe: sign the voter=
// param exactly as received (URL-decoded) and constant-time compare. No
// normalization here — the mailer already produced the canonical identity.
func serverValidToken(seed []byte, voterParam, gotToken string) bool {
	mac := hmac.New(sha256.New, seed)
	mac.Write([]byte(lpString("vote") + lpString(voterParam)))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(gotToken))
}

func TestServerVerifyRoundTrip(t *testing.T) {
	const identity = "alex@example.com" // already normalized mailer-side
	tok := votingToken(testSeed, identity)

	if !serverValidToken(testSeed, identity, tok) {
		t.Fatal("valid token was rejected")
	}
	if serverValidToken(testSeed, identity, tok+"x") {
		t.Fatal("tampered token was accepted")
	}
	if serverValidToken(testSeed, "mallory@example.com", tok) {
		t.Fatal("token was accepted for a different voter")
	}
	if serverValidToken([]byte("a-different-seed"), identity, tok) {
		t.Fatal("token was accepted under a different seed")
	}
}

func TestWriteLP(t *testing.T) {
	var b strings.Builder
	writeLP(&b, "vote")
	writeLP(&b, "alex@example.com")
	if got, want := b.String(), "4:vote16:alex@example.com"; got != want {
		t.Fatalf("writeLP = %q, want %q", got, want)
	}

	// Length-prefixing disambiguates field splits that plain concatenation
	// collides: ("vote","aX") and ("votea","X") both concat to "voteaX".
	if lpPair("vote", "aX") == lpPair("votea", "X") {
		t.Fatal("length-prefixed encoding is ambiguous")
	}
}

func lpPair(a, b string) string {
	var sb strings.Builder
	writeLP(&sb, a)
	writeLP(&sb, b)
	return sb.String()
}
