package media

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
)

// ratioGrace is how many samples scan decodes before it applies a
// samples-per-byte limit, so short clips of digital silence (which compress
// extremely well) are never judged by it.
const ratioGrace = 60 * SampleRate

// scanLimits bounds a scan.
type scanLimits struct {
	maxSamples int64
	// maxPerByte, if positive, is the most decoded samples per frame byte
	// (metadata excluded) a stream may average once past ratioGrace samples (see
	// AnalyzeOptions.MaxSamplesPerByte).
	maxPerByte float64
}

// scan decodes a whole FLAC stream, enforcing every rule of ValidateFLAC, and
// calls visit (if non-nil) for each frame. It checks ctx before every frame,
// so a cancelled scan stops within one frame's work rather than at the next
// read from r (which may be a large buffer away).
func scan(ctx context.Context, r io.Reader, lim scanLimits, visit func(decoded)) (info Info, err error) {
	defer recoverPanic(&err)
	maxSamples := lim.maxSamples
	if maxSamples <= 0 {
		maxSamples = DefaultMaxSamples
	}
	d, err := newDecoder(r, 0, maxSamples)
	if err != nil {
		return Info{}, err
	}
	if err := d.canonical(); err != nil {
		return Info{}, err
	}
	// The ratio is judged over frame bytes only: metadata blocks (PADDING,
	// PICTURE, ...) cost nothing to decode, so counting them would let a
	// padded stream of constant frames pass.
	framesStart := d.offset()
	declared := int64(d.info.NSamples)
	if declared > maxSamples {
		return Info{}, fmt.Errorf("%w: STREAMINFO declares %d samples, limit %d", ErrTooLong, declared, maxSamples)
	}

	sum := md5.New()
	var pcm []byte
	for {
		if err := ctx.Err(); err != nil {
			return Info{}, err
		}
		fr, err := d.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Info{}, err
		}
		if frameBytes := d.offset() - framesStart; lim.maxPerByte > 0 && d.pos > ratioGrace &&
			float64(d.pos) > lim.maxPerByte*float64(frameBytes) {
			return Info{}, fmt.Errorf("%w: %d samples from %d frame bytes is implausibly compressed for the fast path",
				ErrNotCanonical, d.pos, frameBytes)
		}
		pcm = appendS16LE(pcm[:0], fr.samples)
		sum.Write(pcm)
		if visit != nil {
			visit(fr)
		}
	}

	switch {
	case declared != 0 && d.pos != declared:
		return Info{}, corruptf("decoded %d samples, STREAMINFO says %d", d.pos, declared)
	case d.pos < MinSamples:
		return Info{}, fmt.Errorf("%w: %d samples", ErrTooShort, d.pos)
	}
	if want := d.info.MD5sum; want != [md5.Size]byte{} {
		var got [md5.Size]byte
		sum.Sum(got[:0])
		if got != want {
			return Info{}, corruptf("MD5 mismatch")
		}
	}
	return Info{Samples: d.pos}, nil
}

// ValidateFLAC fully decodes a FLAC stream read from r (positioned at its
// start). It checks that STREAMINFO is canonical (16 kHz, mono, 16-bit),
// every frame decodes with valid checksums, the sample total matches
// STREAMINFO when that is non-zero, the length is at most maxSamples
// (<= 0 selects DefaultMaxSamples), and the MD5 signature when one is present.
//
// The error matches ErrNotCanonical for a valid FLAC in another format,
// ErrCorrupt for malformed data (library panics included), ErrTooLong or
// ErrTooShort for length violations; any other error is a failure of r.
func ValidateFLAC(r io.Reader, maxSamples int64) (Info, error) {
	return ValidateFLACContext(context.Background(), r, maxSamples)
}

// ValidateFLACContext is ValidateFLAC that stops, returning ctx's error,
// within one frame of ctx ending.
func ValidateFLACContext(ctx context.Context, r io.Reader, maxSamples int64) (Info, error) {
	return scan(ctx, r, scanLimits{maxSamples: maxSamples}, nil)
}

// appendS16LE appends samples as little-endian 16-bit PCM.
func appendS16LE(b []byte, samples []int32) []byte {
	for _, s := range samples {
		b = binary.LittleEndian.AppendUint16(b, uint16(s))
	}
	return b
}
