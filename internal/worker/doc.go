// Package worker is the transcription worker of DESIGN.md §7: it registers
// with the server, waits for approval, and then runs one goroutine per slot
// that claims a task, downloads its input, runs ffmpeg or whisper-cli under
// sandbox-exec, and uploads the result, heartbeating the lease all the while.
//
// Everything that touches the outside world is injected through Config: the
// HTTP client, the sleeper used for every delay, the filesystem root that
// /proc and /sys are read from, the model catalog and the sandbox prober.
// The package keeps no global state.
//
// # Failure classification
//
// A failed task is reported with retryable=true unless the failure is
// clearly a property of the input, so that every worker would fail the same
// way. Only one case qualifies: ffmpeg reporting undecodable media ("Invalid
// data found when processing input", "does not contain any stream", "matches
// no streams", "could not find codec parameters", "no such file" is excluded
// because it implicates this worker).
//
// Everything else is retryable: crashes, signals including the OOM killer,
// timeouts, unparsable whisper output, whisper-cli failing to read the
// server's own WAV, an input hash mismatch, and model problems. Another
// worker, or this one with more free memory, may succeed, and the server's
// max_attempts bounds the cost of being wrong.
package worker
