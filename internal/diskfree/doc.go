// Package diskfree reports the free space on the filesystem holding a path.
// The server uses it for its data directory and the worker for its state
// directory; both take it as an injectable function so tests can fake it.
package diskfree
