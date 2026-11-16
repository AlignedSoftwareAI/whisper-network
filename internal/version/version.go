// Package version parses whisper-network version strings and compares a
// worker's version with the server's (version skew, DESIGN.md §6.3).
//
// Version strings come from utils/version.sh, which maps `git describe`:
//
//	v0.5.0                    a tagged release
//	v0.5.0-dev+dirty          the tag with uncommitted changes
//	v0.5.0-dev+7.g420d428     7 commits after the tag
//	v0.5.0-dev+7.g420d428.dirty
//	0.0.0-dev+g420d428        no tag reachable (commit count unknown)
//	0.0.0-dev+g420d428.dirty
//
// A dev build N commits after a tag is newer than that tag, so the commit
// count orders builds sharing a base version (this is not semver
// precedence, which would rank a pre-release below its release).
package version

import (
	"strconv"
	"strings"
)

// Skew is how a worker's version relates to the server's.
type Skew string

const (
	Unknown Skew = ""        // the worker reported no version
	Same    Skew = "same"    // identical version strings
	Older   Skew = "older"   // the worker is behind the server
	Newer   Skew = "newer"   // the worker is ahead of the server
	Differs Skew = "differs" // not identical and not orderable
)

// Parsed is a version string broken into its orderable parts.
type Parsed struct {
	Major, Minor, Patch int
	// Commits is the number of commits after the base version's tag; 0 for
	// a tagged build.
	Commits int
	Hash    string // abbreviated commit hash, without the "g"; "" for a tag
	Dirty   bool   // built with uncommitted changes
}

// Parse parses s in the formats listed in the package documentation, with
// or without the leading "v". It reports false for anything else,
// including a build with no reachable tag (0.0.0-dev+g...), whose place in
// history the string does not tell.
func Parse(s string) (Parsed, bool) {
	var p Parsed
	s = strings.TrimPrefix(s, "v")
	base, rest, dev := strings.Cut(s, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return p, false
	}
	for i, dst := range []*int{&p.Major, &p.Minor, &p.Patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 || parts[i] != strconv.Itoa(n) {
			return p, false
		}
		*dst = n
	}
	if !dev {
		return p, true
	}
	build, ok := strings.CutPrefix(rest, "dev+")
	if !ok || build == "" {
		return p, false
	}
	if b, ok := strings.CutSuffix(build, "dirty"); ok {
		p.Dirty = true
		build = strings.TrimSuffix(b, ".")
		if build == "" {
			return p, true // v0.5.0-dev+dirty
		}
	}
	count, hash, ok := strings.Cut(build, ".")
	if !ok {
		return p, false // no commit count: no tag, or malformed
	}
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 || !isHash(hash) {
		return p, false
	}
	p.Commits, p.Hash = n, hash[1:]
	return p, true
}

// isHash reports whether s is "g" followed by lowercase hex digits.
func isHash(s string) bool {
	if len(s) < 2 || s[0] != 'g' {
		return false
	}
	for _, c := range s[1:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Compare places a worker's version relative to the server's. Identical
// strings, or strings parsing to the same build, are Same. Otherwise, where both parse, the base versions are
// compared, then the commit counts after that base; builds that tie on both
// (a different hash, or one dirty) Differ. Anything unparsable Differs. An
// empty worker version is Unknown.
func Compare(worker, server string) Skew {
	switch {
	case worker == "":
		return Unknown
	case worker == server:
		return Same
	}
	w, ok1 := Parse(worker)
	s, ok2 := Parse(server)
	if !ok1 || !ok2 {
		return Differs
	}
	for _, d := range [][2]int{{w.Major, s.Major}, {w.Minor, s.Minor}, {w.Patch, s.Patch}, {w.Commits, s.Commits}} {
		switch {
		case d[0] < d[1]:
			return Older
		case d[0] > d[1]:
			return Newer
		}
	}
	if w == s {
		return Same // the same build, spelled with and without the "v"
	}
	return Differs
}
