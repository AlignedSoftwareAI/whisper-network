package e2etest

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/deweysasser/whisper-network/internal/media"
)

// SpeechWAV returns a canonical 16 kHz mono s16 WAV of seconds of a tone
// with a 150 ms near-silent gap every second, so split points have
// somewhere quiet to land.
func SpeechWAV(seconds float64) []byte {
	n := int64(seconds * media.SampleRate)
	h := media.WAVHeader(n)
	out := make([]byte, 0, len(h)+int(n)*media.BytesPerSample)
	out = append(out, h[:]...)
	for i := range n {
		var v int16
		if i%media.SampleRate < media.SampleRate*85/100 {
			v = int16(8000 * math.Sin(2*math.Pi*220*float64(i)/media.SampleRate))
		} else {
			v = int16(i % 3)
		}
		out = binary.LittleEndian.AppendUint16(out, uint16(v))
	}
	return out
}

// StereoWAV returns a well-formed 44.1 kHz stereo 16-bit WAV: valid audio
// that is not canonical, so it takes the prepare path.
func StereoWAV(seconds float64) []byte {
	const rate = 44100
	n := int(seconds * rate)
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	data := 4 * n
	b.WriteString("RIFF")
	le(uint32(36 + data))
	b.WriteString("WAVEfmt ")
	le(uint32(16))
	le(uint16(1))
	le(uint16(2))
	le(uint32(rate))
	le(uint32(rate * 4))
	le(uint16(4))
	le(uint16(16))
	b.WriteString("data")
	le(uint32(data))
	for i := range n {
		v := int16(8000 * math.Sin(2*math.Pi*220*float64(i)/rate))
		le(v)
		le(v)
	}
	return b.Bytes()
}

// MP4Like returns bytes that sniff as an MP4 container. The fake ffmpeg
// treats any input as raw PCM, so it converts.
func MP4Like(size int) []byte {
	b := make([]byte, size)
	copy(b, []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41"))
	for i := 32; i < size; i++ {
		b[i] = byte(i * 7)
	}
	return b
}
