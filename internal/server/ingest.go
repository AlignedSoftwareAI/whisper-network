package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// canonical is analyzed, validated canonical audio ready to record.
type canonical struct {
	store.Canonical
	Ranges []store.SampleRange
	Index  *media.Index
}

// fastPathSamplesPerByte bounds how well a fast-path upload may compress
// (media.AnalyzeOptions.MaxSamplesPerByte), counted over frame bytes only.
// Measured with media.EncodeFLAC (4096-sample frames), samples per frame
// byte:
//
//	jfk.wav (11 s real speech)                 0.76
//	synthetic speech, 2 min                    0.77
//	±1 / ±4 LSB noise floor, 10 min            3.95 / 2.17
//	jfk + 1 min digital silence                4.87
//	jfk + 10 min digital silence              37.8
//	digital silence / constant frames, 10 min 339 (4974 with 65535-sample frames)
//
// Real recordings, whose quietest parts still carry a noise floor, stay
// under 5; only long runs of exact digital silence go higher. Uploads above
// the bound are not rejected: they go to a prepare task, so a sandboxed
// worker spends the CPU instead. The bound therefore errs low: it keeps
// the server's decode cost at most 32 samples (2 ms of audio) per uploaded
// frame byte.
const fastPathSamplesPerByte = 32

// analyze fully decodes the canonical FLAC in f from its start (validating
// it, DESIGN.md §8.1), choosing split points and building the seek index,
// and hashes the file in the same pass. maxPerByte, if positive, is
// media.AnalyzeOptions.MaxSamplesPerByte. It stops within a frame, with
// ctx's error, once ctx ends; callers must check ctx before judging the
// audio, since the decoder may report the cut-off input as corrupt.
func (s *Server) analyze(ctx context.Context, f *os.File, maxPerByte float64) (*canonical, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h := sha256.New()
	cr := &countReader{r: io.TeeReader(ctxReader{ctx: ctx, r: f}, h)}
	a, err := media.AnalyzeFLAC(ctx, cr, media.AnalyzeOptions{
		MaxSamples: s.maxSamples(), Target: s.cfg.TaskTarget, Window: s.cfg.SplitWindow,
		MaxSamplesPerByte: maxPerByte,
	})
	if err != nil {
		return nil, err
	}
	// The decoder may stop before the end of the file; hash the rest.
	rest, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	c := &canonical{
		Canonical: store.Canonical{Bytes: cr.n + rest, SHA256: hex.EncodeToString(h.Sum(nil)), Samples: a.Samples},
		Index:     a.Index,
	}
	for _, r := range a.Splits {
		c.Ranges = append(c.Ranges, store.SampleRange{Start: r.Start, End: r.End})
	}
	return c, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// mediaVerdict classifies a validation error: definitive rejections (too
// long, too short) versus input a prepare task might still convert.
func mediaVerdict(err error) (convertible bool, rejection *problem) {
	switch {
	case errors.Is(err, media.ErrNotCanonical), errors.Is(err, media.ErrCorrupt):
		return true, nil
	case errors.Is(err, media.ErrTooLong), errors.Is(err, media.ErrTooShort):
		return false, newProblem(http.StatusUnprocessableEntity, api.ProblemMediaInvalid, err.Error())
	}
	return false, nil
}

// canonicalizeUpload tries the fast path for a stored original (DESIGN.md
// §6.1): a canonical WAV is encoded to FLAC, a canonical FLAC is used as
// is. On success canonical.flac and its index are in place and the result
// is returned; (nil, nil) means the upload needs a prepare task. Media that
// no conversion can fix (too long or short) is a 422 problem.
//
// The work is CPU bound (a full decode, plus an encode for WAV), so it holds
// an analysis slot of principal (the uploader), and it stops when ctx ends.
func (s *Server) canonicalizeUpload(ctx context.Context, principal, jobID, sniffed string) (*canonical, error) {
	var c *canonical
	var err error
	switch sniffed {
	case media.TypeWAV, media.TypeFLAC:
	default:
		return nil, nil
	}
	release, err := s.analysisSlot(ctx, principal)
	if err != nil {
		return nil, err
	}
	defer release()
	if sniffed == media.TypeWAV {
		c, err = s.canonicalizeWAV(ctx, jobID)
	} else {
		c, err = s.canonicalizeFLAC(ctx, jobID)
	}
	if ctx.Err() != nil && err != nil {
		return nil, errClientGone // a cut-off decode says nothing about the media
	}
	if errors.Is(err, errNeedsPrepare) {
		_ = s.blobs.Remove(jobID, blobs.Canonical, blobs.CanonicalIndex)
		return nil, nil
	}
	return c, err
}

func (s *Server) canonicalizeWAV(ctx context.Context, jobID string) (*canonical, error) {
	src, err := s.blobs.Open(jobID, blobs.Original)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	wav, err := media.OpenWAV(src, s.maxSamples())
	if err != nil {
		return nil, classify(err)
	}
	out, err := s.blobs.Create(jobID, blobs.Canonical)
	if err != nil {
		return nil, err
	}
	defer out.Abort()
	if err := media.EncodeFLACContext(ctx, out, wav.PCM, wav.Samples); err != nil {
		return nil, classify(err)
	}
	// Server-encoded: the ratio check would only misjudge digital silence.
	c, err := s.analyze(ctx, out.File, 0)
	if err != nil {
		return nil, fmt.Errorf("re-reading encoded FLAC: %w", err)
	}
	if err := s.blobs.WriteFile(jobID, blobs.CanonicalIndex, blobs.EncodeIndex(c.Index)); err != nil {
		return nil, err
	}
	return c, out.Commit()
}

func (s *Server) canonicalizeFLAC(ctx context.Context, jobID string) (*canonical, error) {
	src, err := s.blobs.Open(jobID, blobs.Original)
	if err != nil {
		return nil, err
	}
	c, err := s.analyze(ctx, src, fastPathSamplesPerByte)
	_ = src.Close()
	if err != nil {
		return nil, classify(err)
	}
	if err := s.blobs.WriteFile(jobID, blobs.CanonicalIndex, blobs.EncodeIndex(c.Index)); err != nil {
		return nil, err
	}
	// The upload is the canonical audio: move it rather than copy it.
	return c, s.blobs.Rename(jobID, blobs.Original, blobs.Canonical)
}

// classify maps a fast-path validation error: convertible input becomes
// errNeedsPrepare, rejections become problems, anything else
// (I/O) passes through.
func classify(err error) error {
	convertible, rej := mediaVerdict(err)
	switch {
	case convertible:
		return errNeedsPrepare
	case rej != nil:
		return rej
	}
	return err
}

// errNeedsPrepare signals that the fast path does not apply.
var errNeedsPrepare = errors.New("upload needs a prepare task")
