package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// votingToken derives the per-voter authentication token:
//
//	base64url( HMAC-SHA256(seed, LP("vote") ‖ LP(identity)) )   LP(x)=len(x)":"x
//
// full 32 bytes, URL-safe base64 without padding. It signs identity verbatim —
// any normalization (e.g. lowercasing) is the caller's job, so the string put in
// the link's voter= param and the string signed here are guaranteed identical.
// The server recomputes this over the voter= param as received (URL-decoded) and
// constant-time compares (hmac.Equal).
func votingToken(seed []byte, identity string) string {
	mac := hmac.New(sha256.New, seed)
	writeLP(mac, "vote")
	writeLP(mac, identity)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// writeLP writes a length-prefixed field: the byte length in ASCII decimal, a
// colon, then the raw bytes. Length-prefixing makes the concatenation of fields
// unambiguous, so no two distinct field sets can produce the same signed bytes.
func writeLP(w io.Writer, s string) {
	// w is always an hmac.Hash here, whose Write is documented never to error.
	_, _ = fmt.Fprintf(w, "%d:", len(s))
	_, _ = io.WriteString(w, s)
}
