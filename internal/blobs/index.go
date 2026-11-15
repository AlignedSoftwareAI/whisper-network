package blobs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/deweysasser/whisper-network/internal/media"
)

// indexMagic starts every encoded index; the trailing digit is the version.
const indexMagic = "WNI1"

// maxIndexPoints bounds decoding: one point per second of 12 h audio is
// 43 200, so this leaves ample room while refusing absurd allocations.
const maxIndexPoints = 1 << 22

// ErrBadIndex reports a seek index that is truncated, corrupt or inconsistent.
var ErrBadIndex = errors.New("blobs: invalid seek index")

// EncodeIndex serializes a seek index compactly: the magic, the sample total
// and point count, each point as varint deltas from the previous one, then a
// CRC-32 of everything before it. A 12-hour index is a few hundred KB at most.
func EncodeIndex(x *media.Index) []byte {
	b := make([]byte, 0, len(indexMagic)+16+len(x.Points)*5+4)
	b = append(b, indexMagic...)
	b = binary.AppendUvarint(b, uint64(x.Samples))
	b = binary.AppendUvarint(b, uint64(len(x.Points)))
	var prev media.SeekPoint
	for _, p := range x.Points {
		b = binary.AppendUvarint(b, uint64(p.Sample-prev.Sample))
		b = binary.AppendUvarint(b, uint64(p.Offset-prev.Offset))
		prev = p
	}
	return binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

// DecodeIndex parses EncodeIndex output, verifying its checksum and that the
// points are strictly increasing and within the sample total.
func DecodeIndex(b []byte) (*media.Index, error) {
	if len(b) < len(indexMagic)+4 || string(b[:len(indexMagic)]) != indexMagic {
		return nil, fmt.Errorf("%w: bad header", ErrBadIndex)
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrBadIndex)
	}
	r := body[len(indexMagic):]
	next := func() (int64, error) {
		v, n := binary.Uvarint(r)
		if n <= 0 || v > 1<<62 {
			return 0, fmt.Errorf("%w: truncated", ErrBadIndex)
		}
		r = r[n:]
		return int64(v), nil
	}
	samples, err := next()
	if err != nil {
		return nil, err
	}
	count, err := next()
	if err != nil {
		return nil, err
	}
	if count > maxIndexPoints {
		return nil, fmt.Errorf("%w: %d points", ErrBadIndex, count)
	}
	x := &media.Index{Samples: samples, Points: make([]media.SeekPoint, 0, count)}
	var prev media.SeekPoint
	for i := int64(0); i < count; i++ {
		ds, err := next()
		if err != nil {
			return nil, err
		}
		do, err := next()
		if err != nil {
			return nil, err
		}
		p := media.SeekPoint{Sample: prev.Sample + ds, Offset: prev.Offset + do}
		if i > 0 && (ds == 0 || do == 0) || p.Sample >= samples || p.Sample < 0 || p.Offset < 0 {
			return nil, fmt.Errorf("%w: point %d out of order or range", ErrBadIndex, i)
		}
		x.Points = append(x.Points, p)
		prev = p
	}
	if len(r) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrBadIndex, len(r))
	}
	return x, nil
}
