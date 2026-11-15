package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ErrRange reports a requested sample range outside the audio.
var ErrRange = errors.New("media: sample range out of bounds")

// SeekPoint locates a FLAC frame: the sample number of its first sample and
// its byte offset from the start of the file.
type SeekPoint struct {
	Sample int64
	Offset int64
}

// Index is a seek index for one FLAC file, produced by AnalyzeFLAC. Persist
// it with the file; ServeRange without one must decode from the start.
type Index struct {
	Samples int64 // total samples in the file
	Points  []SeekPoint
}

// find returns the last point at or before sample, or ok=false if none.
func (x *Index) find(sample int64) (SeekPoint, bool) {
	i := sort.Search(len(x.Points), func(i int) bool { return x.Points[i].Sample > sample })
	if i == 0 {
		return SeekPoint{}, false
	}
	return x.Points[i-1], true
}

// wavHeaderSize is the length of the canonical 44-byte WAV header.
const wavHeaderSize = 44

// WAVHeader returns the 44-byte header of a canonical WAV with n samples.
func WAVHeader(n int64) [wavHeaderSize]byte {
	var h [wavHeaderSize]byte
	le := binary.LittleEndian
	dataLen := uint32(n * BytesPerSample)
	copy(h[0:], "RIFF")
	le.PutUint32(h[4:], 36+dataLen)
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], waveFormatPCM)
	le.PutUint16(h[22:], Channels)
	le.PutUint32(h[24:], SampleRate)
	le.PutUint32(h[28:], SampleRate*Channels*BytesPerSample)
	le.PutUint16(h[32:], Channels*BytesPerSample)
	le.PutUint16(h[34:], BitsPerSample)
	copy(h[36:], "data")
	le.PutUint32(h[40:], dataLen)
	return h
}

// ServeRange writes the samples [rng.Start, rng.End) of the canonical FLAC in
// rs to w as a complete WAV file, streaming a frame at a time. Output samples
// are exact: the partial frames at both ends of the range are trimmed.
//
// With an Index from AnalyzeFLAC it seeks directly to the nearest indexed
// frame; with idx nil it decodes from the start of the file. The FLAC is
// assumed to have been validated; frames that nonetheless fail to decode
// produce an error after the WAV header has been written, so callers must
// abort the response. A range outside the audio fails up front with ErrRange.
func ServeRange(w io.Writer, rs io.ReadSeeker, idx *Index, rng Range) (err error) {
	defer recoverPanic(&err)
	if rng.Start < 0 || rng.End <= rng.Start {
		return fmt.Errorf("%w: [%d, %d)", ErrRange, rng.Start, rng.End)
	}

	var pt SeekPoint
	if idx != nil {
		pt, _ = idx.find(rng.Start)
	}
	if _, err := rs.Seek(pt.Offset, io.SeekStart); err != nil {
		return fmt.Errorf("media: seek: %w", err)
	}
	d, err := newDecoder(rs, pt.Offset, 1<<62)
	if err != nil {
		return err
	}
	d.pos = pt.Sample

	total := int64(0)
	if idx != nil {
		total = idx.Samples
	} else {
		total = int64(d.info.NSamples)
		if total == 0 {
			return corruptf("FLAC does not declare its length")
		}
	}
	if rng.End > total {
		return fmt.Errorf("%w: [%d, %d) of %d samples", ErrRange, rng.Start, rng.End, total)
	}

	hdr := WAVHeader(rng.Len())
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var pcm []byte
	for d.pos < rng.End {
		fr, err := d.next()
		if err == io.EOF {
			return corruptf("FLAC ended at sample %d, wanted %d", d.pos, rng.End)
		}
		if err != nil {
			return err
		}
		end := fr.start + int64(len(fr.samples))
		if end <= rng.Start {
			continue
		}
		s := fr.samples[max(rng.Start-fr.start, 0):min(rng.End-fr.start, int64(len(fr.samples)))]
		pcm = appendS16LE(pcm[:0], s)
		if _, err := w.Write(pcm); err != nil {
			return err
		}
	}
	return nil
}
