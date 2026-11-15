// Package auth holds the pure authentication and authorization logic of
// whisper-network: password hashing, token generation and parsing, the role
// model of DESIGN.md §5 and registration rate limiting.
//
// It performs no I/O beyond reading the system random source and has no
// database; the store persists grants, users and token hashes, and callers
// pass the loaded grants in as a [Principal].
package auth
