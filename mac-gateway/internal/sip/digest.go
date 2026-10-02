package sip

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// Digest auth without qop (RFC 2617 simplified profile).
// Both gateway and iOS app implement exactly this — see docs/PROTOCOL.md.

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NewNonce returns a random hex nonce for WWW-Authenticate challenges.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Challenge builds a WWW-Authenticate header value.
func Challenge(realm, nonce string) string {
	return fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5`, realm, nonce)
}

// ParseAuthHeader parses `Digest k="v", k2="v2", ...` into a map.
func ParseAuthHeader(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimSpace(h)
	if i := strings.Index(h, " "); i >= 0 {
		h = h[i+1:]
	}
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// VerifyDigest checks an Authorization header for method/uri.
// qop is not used in this profile.
func VerifyDigest(username, password, realm, method string, authHeader string) bool {
	if !strings.HasPrefix(authHeader, "Digest ") {
		return false
	}
	p := ParseAuthHeader(authHeader)
	if p["username"] != username || p["nonce"] == "" || p["uri"] == "" || p["response"] == "" {
		return false
	}
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + p["uri"])
	expect := md5Hex(ha1 + ":" + p["nonce"] + ":" + ha2)
	return subtle.ConstantTimeCompare([]byte(expect), []byte(p["response"])) == 1
}

// BuildAuthHeader builds the client's Authorization header value (no qop).
func BuildAuthHeader(username, password, realm, nonce, method, uri string) string {
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	resp := md5Hex(ha1 + ":" + nonce + ":" + ha2)
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`,
		username, realm, nonce, uri, resp)
}
