// Package media validates, encodes, splits and serves audio in pure Go.
//
// The server never runs a native media parser. Everything here therefore
// treats its input as hostile: bounded memory, bounded work per input byte,
// and library panics converted to errors. Canonical audio is 16 kHz, mono,
// 16-bit PCM, stored as FLAC.
package media
