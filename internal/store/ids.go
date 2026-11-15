package store

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/google/uuid"
)

// Identifier prefixes for random IDs.
const (
	LeasePrefix  = "lse_"
	ClientPrefix = "cli_"
)

// DefaultNewID is the production ID generator: UUIDv7 for jobs (time-ordered),
// UUIDv4 for workers, and 128 random bits with a prefix for leases and clients.
func DefaultNewID(kind IDKind) string {
	switch kind {
	case IDJob:
		return uuid.Must(uuid.NewV7()).String()
	case IDWorker:
		return uuid.NewString()
	case IDLease:
		return LeasePrefix + random128()
	case IDClient:
		return ClientPrefix + random128()
	}
	panic("store: unknown IDKind")
}

func random128() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// NewJobID returns a fresh job ID. The server needs the ID before the job row
// exists, to stream the upload into the job's blob directory.
func (s *Store) NewJobID() string { return s.cfg.NewID(IDJob) }
