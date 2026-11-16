package version

import "testing"

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Parsed
		ok   bool
	}{
		{"v0.5.0", Parsed{Minor: 5}, true},
		{"0.5.0", Parsed{Minor: 5}, true},
		{"v1.2.3", Parsed{Major: 1, Minor: 2, Patch: 3}, true},
		{"v0.5.0-dev+dirty", Parsed{Minor: 5, Dirty: true}, true},
		{"v0.5.0-dev+7.g420d428", Parsed{Minor: 5, Commits: 7, Hash: "420d428"}, true},
		{"v0.5.0-dev+7.g420d428.dirty", Parsed{Minor: 5, Commits: 7, Hash: "420d428", Dirty: true}, true},
		{"v0.1.10-dev+12.gabc123", Parsed{Minor: 1, Patch: 10, Commits: 12, Hash: "abc123"}, true},
		// No reachable tag: the commit count is unknown.
		{"0.0.0-dev+g420d428", Parsed{}, false},
		{"0.0.0-dev+g420d428.dirty", Parsed{}, false},
		{"", Parsed{}, false},
		{"unknown", Parsed{}, false},
		{"e2e", Parsed{}, false},
		{"v1.2", Parsed{}, false},
		{"v1.2.3.4", Parsed{}, false},
		{"v01.2.3", Parsed{}, false},
		{"v-1.2.3", Parsed{}, false},
		{"v1.2.3-rc1", Parsed{}, false},
		{"v1.2.3-dev", Parsed{}, false},
		{"v1.2.3-dev+", Parsed{}, false},
		{"v1.2.3-dev+x.g1", Parsed{}, false},
		{"v1.2.3-dev+0.gabc", Parsed{}, false},
		{"v1.2.3-dev+3.gXYZ", Parsed{}, false},
		{"v1.2.3-dev+3.abc", Parsed{}, false},
	} {
		got, ok := Parse(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		worker, server string
		want           Skew
	}{
		{"", "v0.5.0", Unknown},
		{"v0.5.0", "v0.5.0", Same},
		{"0.0.0-dev+gabc", "0.0.0-dev+gabc", Same},
		{"unknown", "unknown", Same},
		{"v0.4.9", "v0.5.0", Older},
		{"v0.5.0", "v1.0.0", Older},
		{"v0.5.0", "v0.5.1", Older},
		{"v0.6.0", "v0.5.0", Newer},
		{"0.5.0", "v0.5.0", Same}, // spelled differently
		// Dev builds: base version first, then commits since the tag.
		{"v0.5.0-dev+3.gaaa", "v0.5.0-dev+7.gbbb", Older},
		{"v0.5.0-dev+12.gaaa", "v0.5.0-dev+7.gbbb", Newer},
		{"v0.5.0", "v0.5.0-dev+7.gbbb", Older},
		{"v0.5.0-dev+7.gbbb", "v0.5.0", Newer},
		{"v0.4.0-dev+99.gaaa", "v0.5.0", Older},
		{"v0.5.0-dev+1.gaaa", "v0.4.0-dev+99.gbbb", Newer},
		// Same place in history, different build.
		{"v0.5.0-dev+7.gaaa", "v0.5.0-dev+7.gbbb", Differs},
		{"v0.5.0-dev+7.gaaa.dirty", "v0.5.0-dev+7.gaaa", Differs},
		{"v0.5.0-dev+dirty", "v0.5.0", Differs},
		{"v0.5.0-dev+dirty", "v0.4.0", Newer},
		{"v0.5.0-dev+7.gaaa.dirty", "v0.5.0-dev+3.gaaa", Newer},
		// Unparsable on either side.
		{"0.0.0-dev+gabc", "v0.5.0", Differs},
		{"v0.5.0", "0.0.0-dev+gabc.dirty", Differs},
		{"v0.5.0", "unknown", Differs},
		{"e2e", "v0.5.0", Differs},
	} {
		if got := Compare(c.worker, c.server); got != c.want {
			t.Errorf("Compare(%q, %q) = %q, want %q", c.worker, c.server, got, c.want)
		}
	}
}
