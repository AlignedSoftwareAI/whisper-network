package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
)

// UnknownEngine is the engine version reported when whisper-cli does not say.
const UnknownEngine = "whisper.cpp unknown"

// probe measures the machine and the tools and fills w.facts (DESIGN.md
// §7.1 step 2).
func (w *Worker) probe(ctx context.Context) error {
	w.tools = w.probeTools(ctx)
	if err := w.sandboxPaths(); err != nil {
		return err
	}

	level := api.SandboxNone
	if res, err := w.cfg.ProbeSandbox(ctx); err != nil {
		obs.Warn(&w.log, obs.EventWorkerSandbox).Err(err).Str(obs.FieldSandboxLevel, string(level)).
			Msg("sandbox self-test failed; reporting no sandbox")
	} else {
		level = res.Level
		ev := obs.Info(&w.log, obs.EventWorkerSandbox)
		if level != api.SandboxFull {
			ev = obs.Warn(&w.log, obs.EventWorkerSandbox)
		}
		ev.Str(obs.FieldSandboxLevel, string(level)).Int(obs.FieldLandlockABI, res.LandlockABI).
			Strs("sandbox_blocked", res.Blocked()).Strs("sandbox_open", res.Open()).Msg("sandbox probed")
	}

	mem := w.memoryLimit()
	if mem <= 0 {
		return errors.New("cannot determine this machine's memory; set --memory")
	}
	budget := mem - w.cfg.MemoryReserve
	var largest int64
	var names []string
	if w.tools.whisper != "" && slices.Contains(w.cfg.Kinds, api.KindTranscribe) {
		names, largest = modelsThatFit(w.cfg.Catalog, budget)
		for _, n := range names {
			m, _ := w.cfg.Catalog.Lookup(n)
			w.offered = append(w.offered, m)
		}
	}
	var kinds []api.TaskKind
	if w.tools.ffmpeg != "" && slices.Contains(w.cfg.Kinds, api.KindPrepare) && w.cfg.Limits.PrepareMemory <= budget {
		kinds = append(kinds, api.KindPrepare)
	}
	if len(names) > 0 {
		kinds = append(kinds, api.KindTranscribe)
	}
	if len(kinds) == 0 {
		return fmt.Errorf("nothing to offer: whisper-cli %s, ffmpeg %s, %d MiB usable memory, kinds %v",
			found(w.tools.whisper), found(w.tools.ffmpeg), max(budget, 0)/mib, w.cfg.Kinds)
	}

	cores := w.sys.UsableCores()
	slots := w.cfg.Slots
	if slots <= 0 {
		slots = SlotCount(SlotInput{
			UsableCores: cores, ThreadsPerSlot: w.cfg.ThreadsPerSlot,
			Budget: budget, LargestPeak: largest, PrepareMemory: w.cfg.Limits.PrepareMemory,
		})
	}
	if slots <= 0 {
		return fmt.Errorf("no slot fits in %d MiB usable memory", max(budget, 0)/mib)
	}

	hostname, _ := os.Hostname()
	desc := w.cfg.Description
	if desc == "" {
		desc = hostname
	}
	w.facts = api.WorkerFacts{
		Hostname: hostname, Description: desc,
		CPUModel: w.sys.CPUModel(), Cores: cores, MemBytes: mem,
		OS: runtime.GOOS, Arch: runtime.GOARCH,
		EngineVersion: w.tools.engineVersion, FFmpegVersion: w.tools.ffmpegVersion,
		SandboxLevel: level, Kinds: kinds, Slots: slots, Models: names,
		Version: w.cfg.Version,
	}
	if w.tools.whisper == "" {
		w.facts.EngineVersion = ""
	}
	w.log.Info().Str(obs.FieldHostname, hostname).Str("cpu_model", w.facts.CPUModel).Int("cores", cores).
		Int64("mem_bytes", mem).Int("slots", slots).Str("engine_version", w.facts.EngineVersion).
		Str("ffmpeg_version", w.facts.FFmpegVersion).Msg("probed machine")
	return nil
}

// sandboxPaths works out what each child may read (DESIGN.md §7.4) and
// refuses to start when that would expose the worker's state.
func (w *Worker) sandboxPaths() error {
	if err := checkStateOutside(w.cfg.StateDir, append(systemReadOnly, childSystemFiles...)); err != nil {
		return err
	}
	home := homeDir()
	var err error
	if w.tools.whisper != "" {
		if w.tools.whisperRO, err = toolReadOnly(w.tools.whisper, w.cfg.StateDir, home); err != nil {
			return err
		}
	}
	if w.tools.ffmpeg != "" {
		if w.tools.ffmpegRO, err = toolReadOnly(w.tools.ffmpeg, w.cfg.StateDir, home); err != nil {
			return err
		}
	}
	return nil
}

func found(path string) string {
	if path == "" {
		return "not found"
	}
	return "found"
}

func (w *Worker) probeTools(ctx context.Context) tools {
	var t tools
	if p, err := lookTool(w.cfg.WhisperCLI); err == nil {
		t.whisper = p
		t.engineVersion = parseWhisperVersion(w.toolOutput(ctx, p, "--version"))
	} else {
		w.log.Warn().Err(err).Msg("whisper-cli not found; transcribe tasks disabled")
	}
	if w.cfg.EngineVersion != "" {
		t.engineVersion = w.cfg.EngineVersion
	}
	if p, err := lookTool(w.cfg.FFmpeg); err == nil {
		t.ffmpeg = p
		t.ffmpegVersion = parseFFmpegVersion(w.toolOutput(ctx, p, "-version"))
	} else {
		w.log.Warn().Err(err).Msg("ffmpeg not found; prepare tasks disabled")
	}
	return t
}

// lookTool resolves a tool name or path to an absolute path.
func lookTool(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// versionTimeout bounds a --version probe.
const versionTimeout = 15 * time.Second

// toolOutput runs a tool's version query (unsandboxed, but with a child's
// environment) and returns its combined output.
func (w *Worker) toolOutput(ctx context.Context, path string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(childEnv(os.Environ(), os.TempDir()), w.cfg.ChildEnv...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// whisperVersionRE matches "whisper.cpp version: 1.9.5", printed by
// whisper-cli --version (examples/cli/cli.cpp:154-156 in v1.9.5).
var whisperVersionRE = regexp.MustCompile(`whisper\.cpp version:\s*(\S+)`)

func parseWhisperVersion(out string) string {
	if m := whisperVersionRE.FindStringSubmatch(out); m != nil {
		return "whisper.cpp " + m[1]
	}
	return UnknownEngine
}

// parseFFmpegVersion extracts "7.1.1" from "ffmpeg version 7.1.1 Copyright…"
// (or "n7.1.1", a git tag); "unknown" if absent.
func parseFFmpegVersion(out string) string {
	first, _, _ := strings.Cut(out, "\n")
	f := strings.Fields(first)
	if len(f) >= 3 && f[0] == "ffmpeg" && f[1] == "version" {
		v := f[2]
		if len(v) > 1 && v[0] == 'n' && v[1] >= '0' && v[1] <= '9' {
			v = v[1:]
		}
		return v
	}
	return "unknown"
}
