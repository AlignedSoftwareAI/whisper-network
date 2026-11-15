package workertest

import (
	"encoding/binary"
	"math"

	"github.com/deweysasser/whisper-network/internal/media"
)

// WAV returns a canonical 16 kHz mono s16 WAV of the given length holding a
// 440 Hz tone.
func WAV(seconds float64) []byte {
	n := int64(seconds * media.SampleRate)
	h := media.WAVHeader(n)
	out := make([]byte, 0, len(h)+int(n)*media.BytesPerSample)
	out = append(out, h[:]...)
	for i := range n {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/media.SampleRate))
		out = binary.LittleEndian.AppendUint16(out, uint16(v))
	}
	return out
}
