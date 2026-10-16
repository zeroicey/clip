package clip

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// ownerTokenBytes is the entropy of a clip's owner token: 32 bytes = 256 bits.
//
// It is the only credential in this module, and it is what separates "anyone may
// read this" from "anyone may rewrite this", so it is sized like a key, not like
// a nonce.
const ownerTokenBytes = 32

// readRandom fills b with cryptographically secure random bytes.
func readRandom(b []byte) (int, error) { return rand.Read(b) }

// newOwnerToken mints a token and its stored hash.
//
// The plaintext is returned exactly once, at creation, and never persisted —
// the same property the agent-key design has, and for the same reason: a
// database snapshot must not contain a usable credential.
func newOwnerToken() (token string, hash []byte, err error) {
	buf := make([]byte, ownerTokenBytes)
	if _, err := readRandom(buf); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashOwnerToken(token), nil
}

// hashOwnerToken is the storage form of a token.
func hashOwnerToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// tokensMatch compares two stored hashes in constant time.
//
// The comparison is over hashes, so a timing signal here would leak only
// information about a value the caller already had to guess in full. It is still
// constant-time because the cost of doing it correctly is one function call.
func tokensMatch(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}
