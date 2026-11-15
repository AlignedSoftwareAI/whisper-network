package server

import "sync"

// jobLocks serializes the few multi-step operations on one job that touch
// both blobs and the database: accepting canonical audio, assembling the
// transcript, and deleting the job. There is exactly one server process
// (DESIGN.md non-goals), so an in-memory lock is sufficient. Entries are
// reference counted and removed when unused, so memory stays bounded by
// concurrent operations.
type jobLocks struct {
	mu sync.Mutex
	m  map[string]*jobLock
}

type jobLock struct {
	sync.Mutex
	refs int
}

func newJobLocks() *jobLocks { return &jobLocks{m: map[string]*jobLock{}} }

// Lock locks jobID and returns its unlock function.
func (l *jobLocks) Lock(jobID string) (unlock func()) {
	l.mu.Lock()
	jl := l.m[jobID]
	if jl == nil {
		jl = &jobLock{}
		l.m[jobID] = jl
	}
	jl.refs++
	l.mu.Unlock()

	jl.Lock()
	return func() {
		jl.Unlock()
		l.mu.Lock()
		if jl.refs--; jl.refs == 0 {
			delete(l.m, jobID)
		}
		l.mu.Unlock()
	}
}
