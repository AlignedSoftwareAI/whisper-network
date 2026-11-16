package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/store"
)

// enginePrefix is stripped from reported engine versions in the header.
const enginePrefix = "whisper.cpp "

// assemble writes transcript.txt and result.json for a job whose transcribe
// tasks are all done (DESIGN.md §8.4): segments in seq order with absolute
// times, under a provenance header. Both files are written atomically, so a
// crash leaves either the old state or the new file, and assembly can simply
// be repeated.
func (s *Server) assemble(ctx context.Context, job *store.Job) error {
	outs, err := s.st.JobOutputs(ctx, job.ID)
	if err != nil {
		return err
	}
	segs, prov, err := buildTranscript(job, outs, s.now())
	if err != nil {
		return err
	}
	txt := transcriptText(prov, segs, nil)
	res, err := json.Marshal(api.JobResult{JobID: job.ID, Provenance: prov, Segments: segs})
	if err != nil {
		return err
	}
	if err := s.blobs.WriteFile(job.ID, blobs.Result, append(res, '\n')); err != nil {
		return err
	}
	return s.blobs.WriteFile(job.ID, blobs.Transcript, txt)
}

// transcriptText renders transcript.txt: the provenance header, then one
// "[mm:ss.ss] text" line per segment, with each gap's marker line (partial
// transcripts only) before the first segment at or after its start.
func transcriptText(prov string, segs []api.Segment, gaps []api.Gap) []byte {
	var txt strings.Builder
	txt.WriteString("# " + prov + "\n")
	g := 0
	for _, sg := range segs {
		for ; g < len(gaps) && gaps[g].StartMS <= sg.StartMS; g++ {
			txt.WriteString(gapMarker(gaps[g]) + "\n")
		}
		fmt.Fprintf(&txt, "[%s] %s\n", formatTimestamp(sg.StartMS), sg.Text)
	}
	for ; g < len(gaps); g++ {
		txt.WriteString(gapMarker(gaps[g]) + "\n")
	}
	return []byte(txt.String())
}

// gapMarker is a partial transcript's inline mark for missing audio:
// "[... missing 1:03:12-1:58:40 (task 6) ...]".
func gapMarker(g api.Gap) string {
	return fmt.Sprintf("[... missing %s-%s (task %d) ...]", hms(g.StartMS), hms(g.EndMS), g.Task)
}

// hms renders milliseconds as H:MM:SS, truncating to the second.
func hms(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// partialOutputs builds the transcript text and result JSON of a job that
// is not done (DESIGN.md §8.4): the done tasks' segments, a gap for every
// transcribe task that is not done, and "; partial=true; missing=K/N" at
// the end of the provenance header. Nothing is stored: the output changes
// as tasks finish, and a retried job may still complete.
func partialOutputs(job *store.Job, outs *store.JobOutputs, now time.Time) (txt, res []byte, err error) {
	segs, prov, err := buildTranscript(job, outs, now)
	if err != nil {
		return nil, nil, err
	}
	total := len(outs.Transcribe) + len(outs.Missing)
	prov += fmt.Sprintf("; partial=true; missing=%d/%d", len(outs.Missing), total)
	gaps := make([]api.Gap, len(outs.Missing))
	for i, t := range outs.Missing {
		gaps[i] = api.Gap{Task: t.Seq, StartMS: t.StartSample * 1000 / media.SampleRate,
			EndMS: t.EndSample * 1000 / media.SampleRate}
	}
	res, err = json.Marshal(api.JobResult{JobID: job.ID, Provenance: prov, Segments: segs,
		Partial: true, Missing: gaps})
	if err != nil {
		return nil, nil, err
	}
	return transcriptText(prov, segs, gaps), append(res, '\n'), nil
}

// buildTranscript converts task outputs to absolute segments and the
// provenance header line (without its leading "# "):
//
//	transcript-source: whisper; whisper.cpp=1.9.5; model=small.en; vad=silero-v5.1.2; language=en;
//	transcribed=2026-11-15T14:03:11Z; job=…; source=https://…; ffmpeg=7.1.1; tasks=4; workers=2
//
// Fields keep that order. Values that differ between tasks are listed
// comma-separated and sorted (e.g. whisper.cpp=1.9.5,1.9.6). vad is "none"
// when no task used VAD; source and ffmpeg are omitted when the job has no
// source or took the fast path. Values are sanitized so they cannot break
// the "; " / "=" syntax or the single line.
func buildTranscript(job *store.Job, outs *store.JobOutputs, now time.Time) ([]api.Segment, string, error) {
	var segs []api.Segment
	var engines, models, vads, langs, workers []string
	for _, t := range outs.Transcribe {
		var r storedResult
		if err := json.Unmarshal(t.Result, &r); err != nil {
			return nil, "", fmt.Errorf("task %d result: %w", t.Seq, err)
		}
		offset := t.StartSample * 1000 / media.SampleRate
		for _, sg := range r.Segments {
			text := strings.Join(strings.Fields(sg.Text), " ")
			if text == "" {
				continue
			}
			segs = append(segs, api.Segment{StartMS: offset + sg.StartMS, EndMS: offset + sg.EndMS, Text: text})
		}
		engines = addUnique(engines, strings.TrimPrefix(t.EngineVersion, enginePrefix))
		models = addUnique(models, r.Model)
		vads = addUnique(vads, r.VAD)
		langs = addUnique(langs, r.Language)
		workers = addUnique(workers, t.WorkerID)
	}
	if segs == nil {
		segs = []api.Segment{}
	}
	if len(models) == 0 {
		models = []string{job.Model}
	}
	language := job.Language
	if language == "auto" && len(langs) > 0 {
		language = strings.Join(langs, ",")
	}
	vad := strings.Join(vads, ",")
	if vad == "" {
		vad = "none"
	}

	fields := []string{
		"transcript-source: whisper",
		"whisper.cpp=" + headerValue(strings.Join(engines, ",")),
		"model=" + headerValue(strings.Join(models, ",")),
		"vad=" + headerValue(vad),
		"language=" + headerValue(language),
		"transcribed=" + now.UTC().Format(time.RFC3339),
		"job=" + job.ID,
	}
	if job.Source != "" {
		fields = append(fields, "source="+headerValue(job.Source))
	}
	if outs.Prepare != nil && outs.Prepare.EngineVersion != "" {
		fields = append(fields, "ffmpeg="+headerValue(outs.Prepare.EngineVersion))
	}
	fields = append(fields, fmt.Sprintf("tasks=%d", len(outs.Transcribe)), fmt.Sprintf("workers=%d", len(workers)))
	return segs, strings.Join(fields, "; "), nil
}

func addUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	i, found := slices.BinarySearch(list, v)
	if found {
		return list
	}
	return slices.Insert(list, i, v)
}

// headerValue escapes the characters that would break the header syntax:
// ";" separates fields and newlines end the header.
func headerValue(v string) string {
	return strings.NewReplacer(";", "%3B", "\n", " ", "\r", " ").Replace(v)
}

// formatTimestamp renders milliseconds as mm:ss.ss, the shape of YouTube
// caption transcripts. Minutes are not wrapped into hours, so they exceed
// 59 for long audio (e.g. 61:12.40). Hundredths are truncated.
func formatTimestamp(ms int64) string {
	cs := ms / 10
	return fmt.Sprintf("%02d:%02d.%02d", cs/6000, cs/100%60, cs%100)
}
