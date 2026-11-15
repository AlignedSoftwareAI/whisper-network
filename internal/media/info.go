package media

import (
	"errors"
	"time"
)

// Canonical audio parameters. All stored audio has exactly these.
const (
	SampleRate    = 16000
	Channels      = 1
	BitsPerSample = 16

	// BytesPerSample is the size of one canonical PCM sample (s16le).
	BytesPerSample = BitsPerSample / 8

	// DefaultMaxSamples is the default length limit: 12 hours.
	DefaultMaxSamples int64 = 12 * 3600 * SampleRate

	// MinSamples is the shortest audio accepted. FLAC's minimum block size is
	// 16 samples, and the encoder cannot represent anything shorter.
	MinSamples = 16
)

// Errors returned by the validators. Callers branch with errors.Is.
var (
	// ErrNotCanonical reports well-formed audio that is not 16 kHz mono
	// 16-bit PCM. The caller should route it to a prepare task.
	ErrNotCanonical = errors.New("media: not canonical 16 kHz mono 16-bit audio")
	// ErrCorrupt reports input that is malformed, truncated or hostile.
	ErrCorrupt = errors.New("media: corrupt input")
	// ErrTooLong reports audio longer than the permitted maximum.
	ErrTooLong = errors.New("media: audio too long")
	// ErrTooShort reports audio with fewer than MinSamples samples.
	ErrTooShort = errors.New("media: audio too short")
)

// Info describes canonical audio.
type Info struct {
	// Samples is the total sample count (per channel; the audio is mono).
	Samples int64
}

// Duration is the playing time of the audio.
func (i Info) Duration() time.Duration {
	return time.Duration(i.Samples) * time.Second / SampleRate
}

// Range is a half-open sample interval [Start, End).
type Range struct {
	Start, End int64
}

// Len is the number of samples in the range.
func (r Range) Len() int64 { return r.End - r.Start }
