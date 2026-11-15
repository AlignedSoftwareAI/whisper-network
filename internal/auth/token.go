package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/deweysasser/whisper-network/internal/api"
)

const (
	// tokenBodyLen is the base62 length of a 256-bit value (62^43 > 2^256).
	tokenBodyLen = 43
	tokenBytes   = 32
	sessionPfx   = "wns_"
)

// ErrBadToken is returned by ParseToken for anything that is not a
// well-formed client or worker token. It deliberately says nothing more.
var ErrBadToken = errors.New("malformed token")

// NewClientToken returns a fresh wnc_ API client token. Show it once; store
// only HashToken of it.
func NewClientToken() (string, error) { return newToken(api.ClientTokenPrefix) }

// NewWorkerToken returns a fresh wnw_ worker token.
func NewWorkerToken() (string, error) { return newToken(api.WorkerTokenPrefix) }

// NewSessionID returns a fresh random session identifier (cookie value).
func NewSessionID() (string, error) { return newToken(sessionPfx) }

// NewCSRFToken returns a fresh random per-session CSRF token.
func NewCSRFToken() (string, error) { return randomBase62(tokenBodyLen) }

func newToken(prefix string) (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading randomness: %w", err)
	}
	body := new(big.Int).SetBytes(b).Text(62)
	return prefix + strings.Repeat("0", tokenBodyLen-len(body)) + body, nil
}

// HashToken returns the hex sha256 of a token, session id or CSRF token:
// the form stored in the database. High-entropy secrets need no slow hash.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ParseToken validates the shape of a bearer token (known prefix, exact
// length, base62 body) and returns the kind it authenticates and its
// storage hash. Malformed input returns ErrBadToken before anything is
// hashed, so garbage never reaches a database lookup.
func ParseToken(token string) (PrincipalKind, string, error) {
	var kind PrincipalKind
	var body string
	switch {
	case strings.HasPrefix(token, api.ClientTokenPrefix):
		kind, body = KindClient, token[len(api.ClientTokenPrefix):]
	case strings.HasPrefix(token, api.WorkerTokenPrefix):
		kind, body = KindWorker, token[len(api.WorkerTokenPrefix):]
	default:
		return "", "", ErrBadToken
	}
	if !isBase62(body, tokenBodyLen) {
		return "", "", ErrBadToken
	}
	return kind, HashToken(token), nil
}

// ParseSessionID validates a session cookie value's shape and returns its
// storage hash.
func ParseSessionID(id string) (string, error) {
	body, ok := strings.CutPrefix(id, sessionPfx)
	if !ok || !isBase62(body, tokenBodyLen) {
		return "", ErrBadToken
	}
	return HashToken(id), nil
}

func isBase62(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// Equal reports whether a and b are equal, in constant time for equal
// lengths. Use it for CSRF tokens and any stored-secret comparison.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
