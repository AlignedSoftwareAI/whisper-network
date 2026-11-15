package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chunk struct {
	id   string
	size uint32 // declared size
	body []byte
}

func riff(chunks ...chunk) []byte {
	b := []byte("RIFF\x00\x00\x00\x00WAVE")
	for _, c := range chunks {
		b = append(b, c.id...)
		b = binary.LittleEndian.AppendUint32(b, c.size)
		b = append(b, c.body...)
	}
	return b
}

// ck builds a chunk, appending the RIFF pad byte after an odd-sized body.
func ck(id string, body []byte) chunk {
	size := uint32(len(body))
	if len(body)%2 == 1 {
		body = append(body[:len(body):len(body)], 0)
	}
	return chunk{id, size, body}
}

func fmtBody(tag, ch uint16, rate uint32, bits uint16) []byte {
	align := ch * bits / 8
	b := binary.LittleEndian.AppendUint16(nil, tag)
	b = binary.LittleEndian.AppendUint16(b, ch)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*uint32(align))
	b = binary.LittleEndian.AppendUint16(b, align)
	return binary.LittleEndian.AppendUint16(b, bits)
}

func extBody(sub uint16, validBits uint16) []byte {
	b := fmtBody(waveFormatExtensible, 1, 16000, 16)
	b = binary.LittleEndian.AppendUint16(b, 22)
	b = binary.LittleEndian.AppendUint16(b, validBits)
	b = binary.LittleEndian.AppendUint32(b, 4)
	b = binary.LittleEndian.AppendUint16(b, sub)
	return append(b, pcmSubformatTail...)
}

var (
	goodFmt  = ck("fmt ", fmtBody(1, 1, 16000, 16))
	pcm100   = ck("data", make([]byte, 200))
	oddChunk = ck("junk", []byte{1, 2, 3})
)

func TestOpenWAV(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		samples int64
		err     error
	}{
		{"canonical", riff(goodFmt, pcm100), 100, nil},
		{"unknown chunk before fmt", riff(ck("LIST", []byte("abcd")), goodFmt, pcm100), 100, nil},
		{"odd chunk padded", riff(ck("junk", []byte{1, 2, 3}), ck("pad ", []byte{0}), goodFmt, pcm100), 100, nil},
		{"odd chunk without pad", riff(chunk{"junk", 3, []byte{1, 2, 3}}, goodFmt, pcm100), 0, ErrCorrupt},
		{"trailing chunk ignored", riff(goodFmt, pcm100, ck("LIST", []byte("xx"))), 100, nil},
		{"extensible pcm", riff(ck("fmt ", extBody(1, 16)), pcm100), 100, nil},
		{"extensible valid bits zero", riff(ck("fmt ", extBody(1, 0)), pcm100), 100, nil},
		{"extensible float", riff(ck("fmt ", extBody(3, 16)), pcm100), 0, ErrNotCanonical},
		{"extensible 12 valid bits", riff(ck("fmt ", extBody(1, 12)), pcm100), 0, ErrNotCanonical},
		{"extensible short", riff(ck("fmt ", fmtBody(waveFormatExtensible, 1, 16000, 16)), pcm100), 0, ErrCorrupt},
		{"44.1k", riff(ck("fmt ", fmtBody(1, 1, 44100, 16)), pcm100), 0, ErrNotCanonical},
		{"stereo", riff(ck("fmt ", fmtBody(1, 2, 16000, 16)), pcm100), 0, ErrNotCanonical},
		{"8-bit", riff(ck("fmt ", fmtBody(1, 1, 16000, 8)), pcm100), 0, ErrNotCanonical},
		{"float", riff(ck("fmt ", fmtBody(3, 1, 16000, 32)), pcm100), 0, ErrNotCanonical},
		{"adpcm", riff(ck("fmt ", fmtBody(2, 1, 16000, 4)), pcm100), 0, ErrNotCanonical},
		{"rf64", append([]byte("RF64\x00\x00\x00\x00WAVE"), 0), 0, ErrNotCanonical},
		{"zero channels", riff(ck("fmt ", fmtBody(1, 0, 16000, 16)), pcm100), 0, ErrCorrupt},
		{"bad block align", riff(ck("fmt ", func() []byte { b := fmtBody(1, 1, 16000, 16); b[12] = 4; return b }()), pcm100), 0, ErrCorrupt},
		{"fmt too small", riff(ck("fmt ", make([]byte, 8)), pcm100), 0, ErrCorrupt},
		{"fmt huge", riff(chunk{"fmt ", 0xFFFFFFF0, nil}, pcm100), 0, ErrCorrupt},
		{"duplicate fmt", riff(goodFmt, goodFmt, pcm100), 0, ErrCorrupt},
		{"data before fmt", riff(pcm100, goodFmt), 0, ErrCorrupt},
		{"missing data", riff(goodFmt), 0, ErrCorrupt},
		{"missing fmt", riff(ck("junk", nil)), 0, ErrCorrupt},
		{"truncated data", riff(goodFmt, chunk{"data", 400, make([]byte, 100)}), 0, ErrCorrupt},
		{"truncated mid sample", riff(goodFmt, chunk{"data", 200, make([]byte, 199)}), 0, ErrCorrupt},
		{"odd data size", riff(goodFmt, chunk{"data", 201, make([]byte, 201)}), 0, ErrCorrupt},
		{"huge data size", riff(goodFmt, chunk{"data", 0xFFFFFFF0, make([]byte, 64)}), 0, ErrTooLong},
		{"empty data", riff(goodFmt, ck("data", nil)), 0, ErrTooShort},
		{"huge unknown chunk", riff(chunk{"junk", 0xFFFFFFF0, nil}, goodFmt, pcm100), 0, ErrCorrupt},
		{"unknown chunk truncated", riff(chunk{"junk", 1000, make([]byte, 10)}), 0, ErrCorrupt},
		{"many medium chunks", func() []byte {
			var cs []chunk
			for range 40 {
				cs = append(cs, chunk{"junk", 1 << 20, nil})
			}
			return riff(cs...)
		}(), 0, ErrCorrupt},
		{"not riff", []byte("RIFX\x00\x00\x00\x00WAVEfmt "), 0, ErrCorrupt},
		{"not wave", []byte("RIFF\x00\x00\x00\x00AVI LIST"), 0, ErrCorrupt},
		{"short header", []byte("RIFF\x00"), 0, ErrCorrupt},
		{"empty", nil, 0, ErrCorrupt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, err := ValidateWAV(bytes.NewReader(tc.in), 0)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.samples, info.Samples)
		})
	}
}

func TestOpenWAVMaxSamples(t *testing.T) {
	_, err := ValidateWAV(bytes.NewReader(riff(goodFmt, pcm100)), 99)
	require.ErrorIs(t, err, ErrTooLong)
	_, err = ValidateWAV(bytes.NewReader(riff(goodFmt, pcm100)), 100)
	require.NoError(t, err)
}

func TestOpenWAVStreaming(t *testing.T) {
	// One byte per read exercises every boundary in the header parser.
	in := riff(ck("LIST", []byte("abc")), goodFmt, pcm100)
	info, err := ValidateWAV(iotest.OneByteReader(bytes.NewReader(in)), 0)
	require.NoError(t, err)
	assert.Equal(t, int64(100), info.Samples)
}

func TestOpenWAVDoesNotReadPastData(t *testing.T) {
	w, err := OpenWAV(bytes.NewReader(riff(goodFmt, pcm100, ck("LIST", []byte("zz")))), 0)
	require.NoError(t, err)
	n, err := io.Copy(io.Discard, w.PCM)
	require.NoError(t, err)
	assert.Equal(t, int64(200), n)
}

func TestOpenWAVReaderError(t *testing.T) {
	boom := io.ErrClosedPipe
	in := io.MultiReader(bytes.NewReader(riff(goodFmt, pcm100)[:60]), iotest.ErrReader(boom))
	_, err := ValidateWAV(in, 0)
	require.Error(t, err)
}

func TestWAVHeaderRoundTrip(t *testing.T) {
	info, err := ValidateWAV(bytes.NewReader(toWAV(make([]int16, 1234))), 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1234), info.Samples)
	assert.Equal(t, "77ms", info.Duration().Round(1e6).String())
}
