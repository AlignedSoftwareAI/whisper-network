package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSniff(t *testing.T) {
	tests := map[string]struct {
		in   []byte
		want string
	}{
		"wav":        {toWAV(make([]int16, 32)), "audio/wav"},
		"riff avi":   {[]byte("RIFF\x00\x00\x00\x00AVI LIST"), "video/avi"},
		"flac":       {[]byte("fLaC\x00\x00\x00\x22"), "audio/flac"},
		"mp3":        {[]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), "audio/mpeg"},
		"html":       {[]byte("<!DOCTYPE html><html>"), "text/html; charset=utf-8"},
		"empty":      {nil, "text/plain; charset=utf-8"},
		"short riff": {[]byte("RIFF"), "text/plain; charset=utf-8"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Sniff(tc.in))
		})
	}
}
