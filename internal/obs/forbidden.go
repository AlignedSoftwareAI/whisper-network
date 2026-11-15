package obs

import "strings"

// forbiddenFields are field names that must never appear in a log line
// (DESIGN.md §10.1): credentials, and content that belongs to the requester.
var forbiddenFields = []string{
	"token", "password", "authorization", "cookie", "prompt", "transcript", "metadata",
}

// lengthSuffixes mark a field that records only the size of forbidden data
// (token_len, transcript_bytes), which is allowed.
var lengthSuffixes = []string{"len", "length", "bytes", "size", "count"}

// ForbiddenFields returns the field names that must never be logged. Their
// lengths may be logged under a name such as token_len.
func ForbiddenFields() []string {
	return append([]string(nil), forbiddenFields...)
}

// IsForbiddenField reports whether a log field key names forbidden data. The
// comparison is case-insensitive and looks at each word of the key, split on
// "_", "-" and ".": "Authorization", "set-cookie" and "client_token" are
// forbidden, "token_len" is not.
func IsForbiddenField(key string) bool {
	words := strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]
	for _, s := range lengthSuffixes {
		if last == s && len(words) > 1 {
			return false
		}
	}
	for _, w := range words {
		for _, f := range forbiddenFields {
			if w == f {
				return true
			}
		}
	}
	return false
}
