package media

import (
	"bytes"
	"net/http"
)

// Content types returned by Sniff for the two fast-path formats.
const (
	TypeWAV  = "audio/wav"
	TypeFLAC = "audio/flac"
)

// Sniff returns the content type of an upload from its leading bytes (at
// least 512 are useful). The client-supplied Content-Type is never consulted.
// Anything that is not RIFF/WAVE or FLAC gets http.DetectContentType's answer.
func Sniff(head []byte) string {
	switch {
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")):
		return TypeWAV
	case bytes.HasPrefix(head, []byte("fLaC")):
		return TypeFLAC
	}
	return http.DetectContentType(head)
}
