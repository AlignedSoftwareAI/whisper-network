// Package models is the built-in table of known whisper.cpp model files,
// pinned by sha256. Hashes and sizes are the Git LFS object ids published by
// Hugging Face (verified 2026-11-15).
package models

import "fmt"

// Kind distinguishes transcription models from voice-activity-detection models.
type Kind string

const (
	KindWhisper Kind = "whisper"
	KindVAD     Kind = "vad"
)

// Model is one pinned model file.
type Model struct {
	Name   string // name used in the API, e.g. "small.en"
	Kind   Kind
	File   string // upstream filename
	URL    string // upstream download URL
	SHA256 string
	Size   int64
	// PeakRSS is a conservative estimate of whisper-cli's peak resident memory
	// for a 1-hour chunk with this model, used for slot sizing (DESIGN.md §7.3).
	// Reasoned, not measured; refined by `whisper-network bench`.
	PeakRSS int64
	// CostFactor is the default scheduling cost multiplier (DESIGN.md §8.2).
	CostFactor float64
}

const (
	whisperBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	vadBase     = "https://huggingface.co/ggml-org/whisper-vad/resolve/main/"
	mib         = int64(1) << 20
)

var table = []Model{
	{"tiny.en", KindWhisper, "ggml-tiny.en.bin", whisperBase + "ggml-tiny.en.bin", "921e4cf8686fdd993dcd081a5da5b6c365bfde1162e72b08d75ac75289920b1f", 77704715, 400 * mib, 0.3},
	{"tiny", KindWhisper, "ggml-tiny.bin", whisperBase + "ggml-tiny.bin", "be07e048e1e599ad46341c8d2a135645097a538221678b7acdd1b1919c6e1b21", 77691713, 400 * mib, 0.3},
	{"base.en", KindWhisper, "ggml-base.en.bin", whisperBase + "ggml-base.en.bin", "a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002", 147964211, 600 * mib, 0.5},
	{"base", KindWhisper, "ggml-base.bin", whisperBase + "ggml-base.bin", "60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe", 147951465, 600 * mib, 0.5},
	{"small.en", KindWhisper, "ggml-small.en.bin", whisperBase + "ggml-small.en.bin", "c6138d6d58ecc8322097e0f987c32f1be8bb0a18532a3f88f734d1bbf9c41e5d", 487614201, 1200 * mib, 1},
	{"small", KindWhisper, "ggml-small.bin", whisperBase + "ggml-small.bin", "1be3a9b2063867b937e64e2ec7483364a79917e157fa98c5d94b5c1fffea987b", 487601967, 1200 * mib, 1},
	{"medium.en", KindWhisper, "ggml-medium.en.bin", whisperBase + "ggml-medium.en.bin", "cc37e93478338ec7700281a7ac30a10128929eb8f427dda2e865faa8f6da4356", 1533774781, 2600 * mib, 3},
	{"medium", KindWhisper, "ggml-medium.bin", whisperBase + "ggml-medium.bin", "6c14d5adee5f86394037b4e4e8b59f1673b6cee10e3cf0b11bbdbee79c156208", 1533763059, 2600 * mib, 3},
	{"large-v3-turbo", KindWhisper, "ggml-large-v3-turbo.bin", whisperBase + "ggml-large-v3-turbo.bin", "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69", 1624555275, 2800 * mib, 3},
	{"large-v3", KindWhisper, "ggml-large-v3.bin", whisperBase + "ggml-large-v3.bin", "64d182b440b98d5203c4f9bd541544d84c605196c4f7b845dfa11fb23594d1e2", 3095033483, 4800 * mib, 8},
	{"silero-v5.1.2", KindVAD, "ggml-silero-v5.1.2.bin", vadBase + "ggml-silero-v5.1.2.bin", "29940d98d42b91fbd05ce489f3ecf7c72f0a42f027e4875919a28fb4c04ea2cf", 885098, 0, 0},
	{"silero-v6.2.0", KindVAD, "ggml-silero-v6.2.0.bin", vadBase + "ggml-silero-v6.2.0.bin", "2aa269b785eeb53a82983a20501ddf7c1d9c48e33ab63a41391ac6c9f7fb6987", 885098, 0, 0},
}

// Lookup returns the pinned model with the given API name.
func Lookup(name string) (Model, error) {
	for _, m := range table {
		if m.Name == name {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("unknown model %q", name)
}

// All returns a copy of the model table.
func All() []Model {
	out := make([]Model, len(table))
	copy(out, table)
	return out
}
