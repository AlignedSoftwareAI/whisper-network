package server

import "sync"

// dispatcher wakes long-polling claims when work may have appeared (DESIGN.md
// §8.2). It is a broadcast hint only: every waiter retries its claim
// transaction, which is authoritative, so a spurious or missed wake-up costs
// latency, never correctness.
//
// Waiters take the current channel *before* trying to claim; Notify closes
// it and installs a fresh one. That ordering means a notification that races
// with a failed claim is never lost.
type dispatcher struct {
	mu      sync.Mutex
	ch      chan struct{}
	closing chan struct{}
	closed  bool
}

func newDispatcher() *dispatcher {
	return &dispatcher{ch: make(chan struct{}), closing: make(chan struct{})}
}

// Wait returns a channel closed by the next Notify.
func (d *dispatcher) Wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ch
}

// Notify wakes every current waiter.
func (d *dispatcher) Notify() {
	d.mu.Lock()
	defer d.mu.Unlock()
	close(d.ch)
	d.ch = make(chan struct{})
}

// Closing returns a channel closed by Close, when the server shuts down.
func (d *dispatcher) Closing() <-chan struct{} { return d.closing }

// Close releases all current and future waiters for shutdown. It is
// idempotent.
func (d *dispatcher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		close(d.closing)
	}
}
