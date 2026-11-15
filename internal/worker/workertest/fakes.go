package workertest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/media"
)

// FakeWhisperVersion is what the fake whisper-cli reports.
const FakeWhisperVersion = "1.9.5"

// FakeFFmpegVersion is what the fake ffmpeg reports.
const FakeFFmpegVersion = "7.1.1"

// applyMode handles the modes shared by both fakes. It returns false when the
// fake should exit with code.
func applyMode(tool, mode string) (code int, proceed bool) {
	switch {
	case mode == ModeCrash:
		fmt.Fprintf(os.Stderr, "%s: fatal: simulated crash\n", tool)
		return 3, false
	case mode == ModeHang:
		time.Sleep(time.Hour)
		return 1, false
	case strings.HasPrefix(mode, "sleep:"):
		d, err := time.ParseDuration(strings.TrimPrefix(mode, "sleep:"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2, false
		}
		time.Sleep(d)
	}
	return 0, true
}

// fakeWhisper imitates whisper-cli v1.9.5: --version, @response files, and
// -oj output in output_json's exact layout (tabs, "offsets" in ms).
func fakeWhisper(args []string) int {
	mode := os.Getenv(envWhisper)
	if len(args) == 1 && args[0] == "--version" {
		if mode == ModeNoVersion {
			fmt.Fprintln(os.Stderr, "error: unknown argument: --version")
			return 1
		}
		fmt.Printf("whisper.cpp version: %s\n", FakeWhisperVersion)
		return 0
	}
	if len(args) == 1 && strings.HasPrefix(args[0], "@") {
		b, err := os.ReadFile(args[0][1:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: response file '%s' not found\n", args[0][1:])
			return 1
		}
		args = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	opts := map[string]string{}
	flags := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-m", "-f", "-l", "-t", "-of", "--prompt", "-vm":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "error: argument %s requires value\n", a)
				return 1
			}
			opts[a] = args[i+1]
			i++
		case "-oj", "-np", "--vad":
			flags[a] = true
		default:
			fmt.Fprintf(os.Stderr, "error: unknown argument: %s\n", a)
			return 1
		}
	}
	if mode == ModeForkTree {
		return forkTree("whisper-cli")
	}
	if code, ok := applyMode("whisper-cli", mode); !ok {
		return code
	}
	if _, err := os.ReadFile(opts["-m"]); err != nil {
		fmt.Fprintf(os.Stderr, "error: model file not found '%s'\n", opts["-m"])
		return 3
	}
	if flags["--vad"] {
		if _, err := os.ReadFile(opts["-vm"]); err != nil {
			fmt.Fprintf(os.Stderr, "error: vad model not readable '%s'\n", opts["-vm"])
			return 3
		}
	}
	in, err := os.Open(opts["-f"])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: input file not found '%s'\n", opts["-f"])
		return 11
	}
	defer in.Close()
	wav, err := media.OpenWAV(in, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to read audio file '%s'\n", opts["-f"])
		return 11
	}
	if !flags["-oj"] || opts["-of"] == "" {
		return 0
	}
	out := opts["-of"] + ".json"
	if target, ok := strings.CutPrefix(mode, "symlink:"); ok {
		return plantSymlink(target, out)
	}
	switch mode {
	case ModeNoJSON:
		return 0
	case ModeBadJSON:
		_ = os.WriteFile(out, []byte("{\n\t\"transcription\": [\n"), 0o600)
		return 0
	}
	segMS, _ := strconv.Atoi(os.Getenv(envSegment))
	if segMS <= 0 {
		segMS = 5000
	}
	lang := opts["-l"]
	if lang == "auto" {
		lang = "en"
	}
	durMS := wav.Samples * 1000 / media.SampleRate
	if err := os.WriteFile(out, WhisperJSON(opts["-m"], lang, durMS, int64(segMS)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "output_json: failed to open '%s' for writing\n", out)
		return 11
	}
	// whisper-cli prints segments on stdout as it goes.
	fmt.Println("[00:00:00.000 --> 00:00:05.000]   segment 0")
	return 0
}

// WhisperJSON renders whisper-cli v1.9.5 -oj output for audio of durMS with
// one segment every segMS, texted " segment N".
func WhisperJSON(model, lang string, durMS, segMS int64) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{\n\t\"systeminfo\": \"AVX = 1 | AVX2 = 1\",\n\t\"model\": {\n\t\t\"type\": \"small\",\n"+
		"\t\t\"multilingual\": false,\n\t\t\"vocab\": 51864,\n\t\t\"mels\": 80,\n\t\t\"ftype\": 1\n\t},\n"+
		"\t\"params\": {\n\t\t\"model\": %q,\n\t\t\"language\": %q,\n\t\t\"translate\": false\n\t},\n"+
		"\t\"result\": {\n\t\t\"language\": %q\n\t},\n\t\"transcription\": [\n", model, lang, lang)
	n := 0
	for start := int64(0); start < durMS; start += segMS {
		end := min(start+segMS, durMS)
		if n > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "\t\t{\n\t\t\t\"timestamps\": {\n\t\t\t\t\"from\": %q,\n\t\t\t\t\"to\": %q\n\t\t\t},\n"+
			"\t\t\t\"offsets\": {\n\t\t\t\t\"from\": %d,\n\t\t\t\t\"to\": %d\n\t\t\t},\n\t\t\t\"text\": \" segment %d\"\n\t\t}",
			stamp(start), stamp(end), start, end, n)
		n++
	}
	b.WriteString("\n\t]\n}\n")
	return b.Bytes()
}

func stamp(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// expectedFFmpegArgs is DESIGN.md §7.2's prepare command, with IN and OUT
// placeholders. The fake refuses anything else, pinning the command.
var expectedFFmpegArgs = []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", "IN", "-map", "0:a:0", "-vn", "-sn", "-dn",
	"-ac", "1", "-ar", "16000", "-sample_fmt", "s16", "-c:a", "flac", "-compression_level", "5", "OUT"}

// fakeFFmpeg imitates ffmpeg: -version, and the prepare command, treating the
// input bytes as s16le PCM and writing them as canonical FLAC.
func fakeFFmpeg(args []string) int {
	if len(args) == 1 && args[0] == "-version" {
		fmt.Printf("ffmpeg version %s Copyright (c) 2000-2025 the FFmpeg developers\nbuilt with gcc 14\n", FakeFFmpegVersion)
		return 0
	}
	if len(args) != len(expectedFFmpegArgs) {
		fmt.Fprintf(os.Stderr, "fake ffmpeg: unexpected arguments %q\n", args)
		return 8
	}
	for i, want := range expectedFFmpegArgs {
		if want != "IN" && want != "OUT" && args[i] != want {
			fmt.Fprintf(os.Stderr, "fake ffmpeg: argument %d is %q, want %q\n", i, args[i], want)
			return 8
		}
	}
	in, out := args[slices.Index(expectedFFmpegArgs, "IN")], args[len(args)-1]
	mode := os.Getenv(envFFmpeg)
	if mode == ModeInvalid {
		fmt.Fprintf(os.Stderr, "[in#0 @ 0x55] Error opening input: Invalid data found when processing input\n"+
			"Error opening input file %s.\n", in)
		return 1
	}
	if code, ok := applyMode("ffmpeg", mode); !ok {
		return code
	}
	if target, ok := strings.CutPrefix(mode, "symlink:"); ok {
		return plantSymlink(target, out)
	}
	data, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", in, err)
		return 1
	}
	samples := max(int64(len(data)/media.BytesPerSample), media.MinSamples)
	pcm := make([]byte, samples*media.BytesPerSample)
	copy(pcm, data)
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", out, err)
		return 1
	}
	if err := media.EncodeFLAC(f, bytes.NewReader(pcm), samples); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "size=%dKiB time=00:00:01.00 bitrate=256.0kbits/s speed=100x\n", samples*2/1024)
	return 0
}

// plantSymlink replaces the output with a symlink to target and exits 0, as a
// compromised tool might to make the worker read or upload another file.
func plantSymlink(target, out string) int {
	_ = os.Remove(out)
	if err := os.Symlink(target, out); err != nil {
		fmt.Fprintf(os.Stderr, "symlink: %v\n", err)
		return 1
	}
	return 0
}

// forkTree starts a grandchild that hangs, prints its pid on stderr, and
// hangs itself: killing this process alone would leave the grandchild
// running.
func forkTree(tool string) int {
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), envWhisper+"="+ModeHang, envFFmpeg+"="+ModeHang)
	// Share stderr: the sandbox does not let the child open /dev/null for
	// writing.
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: fork: %v\n", tool, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s: grandchild pid %d\n", tool, c.Process.Pid)
	time.Sleep(time.Hour)
	return 1
}
