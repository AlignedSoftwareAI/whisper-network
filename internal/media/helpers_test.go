package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// memWS is an in-memory io.WriteSeeker.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	if end := m.pos + int64(len(p)); end > int64(len(m.buf)) {
		m.buf = append(m.buf, make([]byte, end-int64(len(m.buf)))...)
	}
	copy(m.buf[m.pos:], p)
	m.pos += int64(len(p))
	return len(p), nil
}

func (m *memWS) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.buf)) + off
	}
	return m.pos, nil
}

// speechLike returns n samples of a noisy tone with a wandering envelope,
// which compresses roughly like speech under fixed predictors.
func speechLike(n int, seed uint64) []int16 {
	rng := rand.New(rand.NewPCG(seed, 1))
	out := make([]int16, n)
	for i := range out {
		env := 0.5 + 0.5*math.Sin(float64(i)/9000)
		tone := math.Sin(float64(i)*0.07)*6000 + math.Sin(float64(i)*0.19)*2500
		out[i] = int16(env*tone + rng.NormFloat64()*150)
	}
	return out
}

func noise(n int, seed uint64) []int16 {
	rng := rand.New(rand.NewPCG(seed, 2))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(rng.Uint32())
	}
	return out
}

func pcmBytes(s []int16) []byte {
	b := make([]byte, 0, len(s)*2)
	for _, v := range s {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return b
}

func toWAV(s []int16) []byte {
	h := WAVHeader(int64(len(s)))
	return append(h[:], pcmBytes(s)...)
}

func encodeFLAC(t testing.TB, s []int16) []byte {
	t.Helper()
	var m memWS
	require.NoError(t, EncodeFLAC(&m, bytes.NewReader(pcmBytes(s)), int64(len(s))))
	return m.buf
}

// decodeWAV returns the samples of a WAV produced by ServeRange, checking
// that its header is exactly the canonical one for its length.
func decodeWAV(t testing.TB, b []byte) []int16 {
	t.Helper()
	require.GreaterOrEqual(t, len(b), wavHeaderSize)
	raw := b[wavHeaderSize:]
	want := WAVHeader(int64(len(raw) / 2))
	require.Equal(t, want[:], b[:wavHeaderSize])
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out
}
