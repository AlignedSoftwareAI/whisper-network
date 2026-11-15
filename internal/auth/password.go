package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 limits applied when reading a stored hash, so a tampered row cannot
// make Verify allocate unbounded memory or time.
const (
	maxMemoryKiB = 256 * 1024
	maxTime      = 10
	maxThreads   = 16
	// MaxPasswordLen bounds accepted password length in bytes.
	MaxPasswordLen = 1024

	saltLen = 16
	keyLen  = 32
)

// ErrPasswordTooLong is returned when a password exceeds MaxPasswordLen.
var ErrPasswordTooLong = errors.New("password too long")

// ErrBadHash is returned when a stored hash is not a valid argon2id PHC string.
var ErrBadHash = errors.New("malformed password hash")

// PasswordParams are the argon2id cost parameters.
type PasswordParams struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultPasswordParams returns the RFC 9106 "second recommended" profile
// (m=64 MiB, t=3, p=4), suited to an interactive login on a small server.
func DefaultPasswordParams() PasswordParams {
	return PasswordParams{MemoryKiB: 64 * 1024, Time: 3, Threads: 4}
}

// HashPassword returns a PHC-format argon2id hash of password with a fresh
// random salt: $argon2id$v=19$m=...,t=...,p=...$salt$hash.
func HashPassword(password string, p PasswordParams) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", ErrPasswordTooLong
	}
	if p.MemoryKiB == 0 || p.Time == 0 || p.Threads == 0 {
		return "", errors.New("invalid argon2 parameters")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("reading random salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash, comparing
// in constant time. A wrong password is (false, nil); a malformed hash is
// (false, ErrBadHash). Callers should treat both as failed login.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordLen {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was produced with parameters other
// than p (or is unreadable) and should be re-hashed after a successful login.
func NeedsRehash(encoded string, p PasswordParams) bool {
	have, _, key, err := decodeHash(encoded)
	return err != nil || have != p || len(key) != keyLen
}

func decodeHash(encoded string) (PasswordParams, []byte, []byte, error) {
	var p PasswordParams
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrBadHash
	}
	var version int
	if n, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || n != 1 || version != argon2.Version {
		return p, nil, nil, ErrBadHash
	}
	var m, t, th uint32
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &th); err != nil || n != 3 {
		return p, nil, nil, ErrBadHash
	}
	if m == 0 || m > maxMemoryKiB || t == 0 || t > maxTime || th == 0 || th > maxThreads {
		return p, nil, nil, ErrBadHash
	}
	enc := base64.RawStdEncoding.Strict()
	salt, err := enc.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, ErrBadHash
	}
	key, err := enc.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return p, nil, nil, ErrBadHash
	}
	return PasswordParams{MemoryKiB: m, Time: t, Threads: uint8(th)}, salt, key, nil
}

const passwordAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// GeneratePassword returns a random 24-character base62 password (about 143
// bits) for the first-run admin (DESIGN.md D7).
func GeneratePassword() (string, error) {
	return randomBase62(24)
}

// randomBase62 returns n uniformly random base62 characters, using rejection
// sampling to avoid modulo bias.
func randomBase62(n int) (string, error) {
	const limit = 248 // largest multiple of 62 that fits in a byte
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("reading randomness: %w", err)
		}
		for _, b := range buf {
			if b < limit && len(out) < n {
				out = append(out, passwordAlphabet[b%62])
			}
		}
	}
	return string(out), nil
}
