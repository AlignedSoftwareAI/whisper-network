package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/blobs"
	"github.com/deweysasser/whisper-network/internal/media"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
)

const secretPrompt = "SECRET-PROMPT-councilors-smith-and-jones"

// TestFullFlowCanonicalWAV drives a job end to end through the HTTP API:
// register → approve → fast-path WAV upload split into several tasks →
// claim → input → heartbeat → results → assembled transcript → downloads →
// delete. It then checks the logs and metrics the run produced.
func TestFullFlowCanonicalWAV(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{Hostname: "nuc-01"}, "public")

	audio := speech(10, media.SampleRate)
	job := h.submit(client, "public", api.JobParams{Model: "small.en", Prompt: secretPrompt, Label: "council",
		Source: "https://example.org/v?id=1;x", Metadata: json.RawMessage(`{"requester":"SECRET-REQUESTER"}`)},
		canonicalWAV(audio))
	assert.Equal(t, api.JobQueued, job.State)
	assert.True(t, job.Media.OriginalPurged, "the WAV original is purged once encoded")
	assert.InDelta(t, 10.0, job.Media.DurationS, 0.001)
	require.GreaterOrEqual(t, job.Tasks.Total, 3, "10 s at a 3 s target splits")
	assert.Equal(t, job.Tasks.Total, job.Tasks.Pending)
	assert.Equal(t, media.TypeWAV, job.Media.OriginalType)
	_, err := os.Stat(blobPath(t, h, job.ID, blobs.Original))
	assert.ErrorIs(t, err, os.ErrNotExist)

	var offset int64
	starts := map[int]int64{}
	for i := 0; i < job.Tasks.Total; i++ {
		l := h.claim(wid, wtok, "", nil)
		require.NotNil(t, l)
		assert.Equal(t, api.KindTranscribe, l.Kind)
		assert.Equal(t, i+1, l.Seq, "tasks of one job are leased in seq order")
		assert.Equal(t, "small.en", l.Model)
		assert.Equal(t, secretPrompt, l.Prompt, "the prompt goes to the worker")
		assert.NotEmpty(t, l.ModelSHA256)
		assert.Equal(t, "silero-v5.1.2", l.VADModel)
		assert.Equal(t, 30, l.HeartbeatIntervalS)
		starts[l.Seq] = offset

		resp := h.do(req{method: "GET", path: l.InputURL, token: wtok})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "audio/wav", resp.Header.Get("Content-Type"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		got := wavSamples(t, body)
		require.Len(t, got, int(l.AudioSamples))
		assert.Equal(t, audio[offset:offset+l.AudioSamples], got, "input is exactly the task's sample range")
		offset += l.AudioSamples

		var hb api.HeartbeatResponse
		h.expect("POST", "/lease/"+l.ID+"/heartbeat", wtok, api.HeartbeatRequest{Gauges: &api.Gauges{SlotsBusy: 1}},
			http.StatusOK, &hb)
		assert.True(t, hb.ExpiresAt.After(h.clk.Now()))

		resp2, b := h.putResult(l, wtok, result(l, fmt.Sprintf("task %d says hello", l.Seq)))
		require.Equal(t, http.StatusOK, resp2.StatusCode, string(b))
		// A retried PUT of the same result is idempotent.
		resp3, b := h.putResult(l, wtok, result(l, fmt.Sprintf("task %d says hello", l.Seq)))
		require.Equal(t, http.StatusOK, resp3.StatusCode, string(b))
	}
	assert.Equal(t, int64(len(audio)), offset, "tasks cover the audio exactly")
	assert.Nil(t, h.claim(wid, wtok, "", nil), "nothing left")

	done := h.job(job.ID)
	require.Equal(t, api.JobDone, done.State)
	assert.InDelta(t, 1.5*float64(job.Tasks.Total), done.ComputeS, 0.001)

	resp, tb := h.call("GET", "/job/"+job.ID+"/transcript", client, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
	lines := strings.Split(strings.TrimSuffix(string(tb), "\n"), "\n")
	require.Len(t, lines, 1+job.Tasks.Total)
	assert.Regexp(t, regexp.MustCompile(`^# transcript-source: whisper; whisper\.cpp=1\.9\.5; model=small\.en; `+
		`vad=silero-v5\.1\.2; language=en; transcribed=2026-11-15T12:00:00Z; job=`+job.ID+
		`; source=https://example\.org/v\?id=1%3Bx; tasks=\d+; workers=1$`), lines[0])
	for seq := 1; seq <= job.Tasks.Total; seq++ {
		abs := starts[seq]*1000/media.SampleRate + 500
		assert.Equal(t, fmt.Sprintf("[%s] task %d says hello", formatTimestamp(abs), seq), lines[seq])
	}

	var jr api.JobResult
	h.expect("GET", "/job/"+job.ID+"/result", client, nil, http.StatusOK, &jr)
	assert.Equal(t, job.ID, jr.JobID)
	require.Len(t, jr.Segments, job.Tasks.Total)
	assert.Equal(t, starts[2]*1000/media.SampleRate+500, jr.Segments[1].StartMS, "absolute times")
	assert.Equal(t, strings.TrimPrefix(lines[0], "# "), jr.Provenance)

	dl := h.job(job.ID).Downloads
	assert.Equal(t, 2, dl.Count)
	assert.NotNil(t, dl.First)

	h.expect("DELETE", "/job/"+job.ID, client, nil, http.StatusNoContent, nil)
	resp, b := h.call("GET", "/job/"+job.ID, client, nil)
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)
	_, err = os.Stat(blobPath(t, h, job.ID, ""))
	assert.ErrorIs(t, err, os.ErrNotExist, "blobs removed with the job")

	// Log hygiene: no secrets, and the catalog events are present.
	logs := h.logs.Bytes()
	obstest.AssertClean(t, logs)
	assert.NotContains(t, string(logs), secretPrompt)
	assert.NotContains(t, string(logs), "SECRET-REQUESTER")
	assert.NotContains(t, string(logs), "says hello", "transcript text is never logged")
	for _, ev := range []obs.Event{obs.EventWorkerRegistered, obs.EventWorkerApproved, obs.EventJobCreated,
		obs.EventJobPrepared, obs.EventTaskLeased, obs.EventTaskDone, obs.EventJobDone, obs.EventJobDeleted,
		obs.EventHTTPRequest, obs.EventAdminChange, obs.EventJobPurged} {
		assert.NotEmpty(t, h.logs.events(t, ev), "missing event %s", ev)
	}
	created := h.logs.events(t, obs.EventJobCreated)[0]
	assert.Equal(t, job.ID, created[obs.FieldJobID])
	assert.Equal(t, "public", created[obs.FieldQueue])
	assert.Equal(t, true, created[obs.FieldFastPath])
	assert.Equal(t, "server", created[obs.FieldMode])
	leased := h.logs.events(t, obs.EventTaskLeased)[0]
	for _, f := range []string{obs.FieldJobID, obs.FieldTaskSeq, obs.FieldTaskKind, obs.FieldLeaseID,
		obs.FieldWorkerID, obs.FieldQueue, obs.FieldCost, obs.FieldVTime} {
		assert.Contains(t, leased, f)
	}
	for _, r := range h.logs.events(t, obs.EventHTTPRequest) {
		assert.NotContains(t, r[obs.FieldRoute], job.ID, "routes are patterns, never raw paths")
	}
	doneEv := h.logs.events(t, obs.EventJobDone)[0]
	assert.Contains(t, doneEv, obs.FieldComputeSeconds)

	// Metrics: event counters plus the refreshed snapshot.
	h.srv.refreshStats(h.ctx)
	var mb bytes.Buffer
	h.metrics.WritePrometheus(&mb)
	m := mb.String()
	for _, want := range []string{
		`whisper_http_requests_total{route="/queue/{q}",method="POST",code="201"} 1`,
		`whisper_jobs_created_total{queue="public",weight="medium",fast_path="true"} 1`,
		`whisper_jobs_finished_total{queue="public",outcome="done"} 1`,
		fmt.Sprintf(`whisper_leases_ended_total{outcome="done",kind="transcribe"} %d`, job.Tasks.Total),
		`whisper_task_compute_seconds_count{kind="transcribe",model="small.en"}`,
		`whisper_task_realtime_factor_count{model="small.en",worker="` + wid + `"}`,
		`whisper_worker_audio_seconds_total{worker="` + wid + `",model="small.en"} 10`,
		`whisper_sched_served_cost_total{queue="public",class="medium"}`,
		`whisper_upload_bytes_total{queue="public"}`,
		`whisper_input_served_bytes_total{kind="transcribe"}`,
		`whisper_claim_wait_seconds_count`,
		`whisper_workers{state="active"} 1`,
		`whisper_worker_info{worker="` + wid + `",hostname="nuc-01"`,
		`whisper_worker_slots_busy{worker="` + wid + `"} 1`,
		`whisper_queue_weight{queue="public"} 100`,
		`whisper_data_free_bytes 1.073741824e+11`,
		`whisper_storage_bytes{kind="transcript"} 0`,
	} {
		assert.Contains(t, m, want)
	}
}

// TestPrepareFlow covers a non-canonical upload: a prepare task runs, its
// input is the original, its FLAC result creates the transcribe tasks.
func TestPrepareFlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")

	orig := stereoWAV(1)
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, orig)
	assert.Equal(t, api.JobPreparing, job.State)
	assert.Equal(t, 1, job.Tasks.Total)
	assert.False(t, job.Media.OriginalPurged)

	l := h.claim(wid, wtok, "", &api.ClaimRequest{Kinds: []api.TaskKind{api.KindPrepare, api.KindTranscribe},
		Models: []string{"small.en"}})
	require.NotNil(t, l)
	assert.Equal(t, api.KindPrepare, l.Kind)
	assert.Equal(t, 0, l.Seq)
	assert.Equal(t, int64(len(orig)), l.InputBytes)
	assert.Equal(t, sha256hex(orig), l.InputSHA256)

	resp, in := h.call("GET", l.InputURL, wtok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, orig, in, "prepare input is the original upload")
	resp = h.do(req{method: "GET", path: l.InputURL, token: wtok, header: map[string]string{"Range": "bytes=100-"}})
	require.Equal(t, http.StatusPartialContent, resp.StatusCode, "resumable download")
	part, _ := io.ReadAll(resp.Body)
	assert.Equal(t, orig[100:], part)

	audio := speech(7, media.SampleRate)
	flac := canonicalFLAC(t, audio)
	resp, b := h.putFLAC(l, wtok, flac)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))

	j := h.job(job.ID)
	assert.Equal(t, api.JobQueued, j.State)
	assert.True(t, j.Media.OriginalPurged, "original purged once canonical accepted")
	assert.InDelta(t, 7.0, j.Media.DurationS, 0.001)
	assert.Greater(t, j.Tasks.Total, 2)
	assert.Equal(t, 1, j.Tasks.Done)
	_, err := os.Stat(blobPath(t, h, job.ID, blobs.Original))
	assert.ErrorIs(t, err, os.ErrNotExist)
	stored, err := os.ReadFile(blobPath(t, h, job.ID, blobs.Canonical))
	require.NoError(t, err)
	assert.Equal(t, flac, stored)

	// Idempotent repeat; a different body for the same lease conflicts.
	resp, b = h.putFLAC(l, wtok, flac)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	resp, b = h.putFLAC(l, wtok, canonicalFLAC(t, speech(6, media.SampleRate)))
	h.expectProblem(resp, b, http.StatusConflict, api.ProblemConflict)

	// The input of a finished lease is gone.
	resp, b = h.call("GET", l.InputURL, wtok, nil)
	h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)

	// Transcribe through to the end; the header carries the ffmpeg version.
	for {
		tl := h.claim(wid, wtok, "", nil)
		if tl == nil {
			break
		}
		resp, b := h.putResult(tl, wtok, result(tl, "words"))
		require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	}
	resp, tb := h.call("GET", "/job/"+job.ID+"/transcript", client, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(tb), "; ffmpeg=7.1.1; tasks=")
	assert.NotEmpty(t, h.logs.events(t, obs.EventJobPrepared))
}

func TestPrepareInvalidResultCountsAttempts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{}, "public")
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, []byte("definitely not audio, just bytes"))
	assert.Equal(t, api.JobPreparing, job.State)

	for attempt := 1; attempt <= 2; attempt++ {
		l := h.claim(wid, wtok, "", nil)
		require.NotNil(t, l, "attempt %d", attempt)
		assert.Equal(t, api.KindPrepare, l.Kind)
		resp, b := h.putFLAC(l, wtok, []byte("fLaC but not really"))
		p := h.expectProblem(resp, b, http.StatusUnprocessableEntity, api.ProblemResultInvalid)
		assert.NotEmpty(t, p.Detail)
		// The lease is over: a retry of the PUT is fenced.
		resp, b = h.putFLAC(l, wtok, []byte("x"))
		h.expectProblem(resp, b, http.StatusGone, api.ProblemLeaseGone)
	}
	j := h.job(job.ID)
	assert.Equal(t, api.JobFailed, j.State, "max attempts (2) exhausted")
	require.NotNil(t, j.Error)
	assert.Contains(t, *j.Error, "invalid result")
	assert.Nil(t, h.claim(wid, wtok, "", nil))
	assert.Len(t, h.logs.events(t, obs.EventTaskFailed), 2)
	assert.Len(t, h.logs.events(t, obs.EventJobFailed), 1)

	resp, b := h.call("GET", "/job/"+job.ID+"/transcript", client, nil)
	p := h.expectProblem(resp, b, http.StatusConflict, api.ProblemNotDone)
	assert.Equal(t, "failed", p.State)
}

func TestPrepareSandboxPolicy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wid, wtok := h.activeWorker(api.WorkerFacts{SandboxLevel: api.SandboxFSOnly}, "public")
	h.submit(client, "public", api.JobParams{Model: "small.en"}, []byte("some video bytes"))
	assert.Nil(t, h.claim(wid, wtok, "", nil), "fs-only workers get no prepare tasks under the default policy")

	fid, ftok := h.activeWorker(api.WorkerFacts{SandboxLevel: api.SandboxFull}, "public")
	assert.Nil(t, h.claim(fid, ftok, "", &api.ClaimRequest{Kinds: []api.TaskKind{api.KindTranscribe}, Models: []string{"small.en"}}),
		"a slot that does not offer prepare gets none")
	l := h.claim(fid, ftok, "", nil)
	require.NotNil(t, l)
	assert.Equal(t, api.KindPrepare, l.Kind)
}

func TestUploadValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	wav := canonicalWAV(speech(1, media.SampleRate))
	cases := []struct {
		name   string
		params api.JobParams
	}{
		{"model not allowlisted", api.JobParams{Model: "large-v3"}},
		{"bad weight", api.JobParams{Model: "small.en", Weight: "urgent"}},
		{"bad language", api.JobParams{Model: "small.en", Language: "english!"}},
		{"long prompt", api.JobParams{Model: "small.en", Prompt: strings.Repeat("x", api.MaxPromptLen+1)}},
		{"multiline label", api.JobParams{Model: "small.en", Label: "a\nb"}},
		{"huge metadata", api.JobParams{Model: "small.en", Metadata: json.RawMessage(`"` + strings.Repeat("m", api.MaxMetadataBytes) + `"`)}},
	}
	for _, c := range cases {
		resp, b := h.upload(client, "public", c.params, wav, "")
		h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	}
	// Too-short canonical audio cannot be fixed by a prepare task.
	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(0.0005, media.SampleRate)), "")
	h.expectProblem(resp, b, http.StatusUnprocessableEntity, api.ProblemMediaInvalid)

	// Not multipart at all.
	resp, b = h.call("POST", "/queue/public", client, map[string]string{"model": "small.en"})
	h.expectProblem(resp, b, http.StatusBadRequest, api.ProblemBadRequest)
	// Unknown queue.
	resp, b = h.upload(h.admin, "nope", api.JobParams{Model: "small.en"}, wav, "")
	h.expectProblem(resp, b, http.StatusNotFound, api.ProblemNotFound)

	var list api.JobList
	h.expect("GET", "/queue/public", h.admin, nil, http.StatusOK, &list)
	assert.Empty(t, list.Jobs, "no job was created by any rejected upload")
	dirs, err := h.blobs.Jobs()
	require.NoError(t, err)
	assert.Empty(t, dirs, "rejected uploads leave no blobs")
}

func TestFLACFastPathAndIdempotency(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	flac := canonicalFLAC(t, speech(4, media.SampleRate))
	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, flac, "key-1")
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))
	var first api.Job
	require.NoError(t, json.Unmarshal(b, &first))
	assert.Equal(t, api.JobQueued, first.State)
	assert.Equal(t, media.TypeFLAC, first.Media.OriginalType)
	stored, err := os.ReadFile(blobPath(t, h, first.ID, blobs.Canonical))
	require.NoError(t, err)
	assert.Equal(t, flac, stored, "a canonical FLAC upload is stored as is")

	resp, b = h.upload(client, "public", api.JobParams{Model: "small.en"}, flac, "key-1")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
	var again api.Job
	require.NoError(t, json.Unmarshal(b, &again))
	assert.Equal(t, first.ID, again.ID)

	// The key is per client: another client's same key is a new job.
	other := h.client(submitOn("public"))
	resp, b = h.upload(other, "public", api.JobParams{Model: "small.en"}, flac, "key-1")
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))

	var list api.JobList
	h.expect("GET", "/queue/public", client, nil, http.StatusOK, &list)
	assert.Len(t, list.Jobs, 1, "a submitter sees only its own jobs")
	h.expect("GET", "/queue/public", h.admin, nil, http.StatusOK, &list)
	assert.Len(t, list.Jobs, 2)
}

func TestUploadSizeCapAndDiskGuard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	limit := int64(4000)
	h.expect("PATCH", "/queue/public", h.admin, api.QueuePatch{MaxUploadBytes: &limit}, http.StatusOK, nil)

	resp, b := h.upload(client, "public", api.JobParams{Model: "small.en"}, bytes.Repeat([]byte("v"), 5000), "")
	h.expectProblem(resp, b, http.StatusRequestEntityTooLarge, api.ProblemTooLarge)
	resp, b = h.upload(client, "public", api.JobParams{Model: "small.en"}, bytes.Repeat([]byte("v"), 3000), "")
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(b))

	h.free.Store(10 << 20) // below the 1 GiB default threshold
	resp, b = h.upload(client, "public", api.JobParams{Model: "small.en"}, []byte("small"), "")
	h.expectProblem(resp, b, http.StatusInsufficientStorage, api.ProblemStorageFull)
	resp, _ = h.call("GET", "/readyz", "", nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	h.free.Store(100 << 30)
	resp, _ = h.call("GET", "/readyz", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = h.call("GET", "/healthz", "", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func blobPath(t *testing.T, h *harness, jobID, name string) string {
	t.Helper()
	if name == "" {
		return h.blobs.Root() + "/" + jobID
	}
	p, err := h.blobs.Path(jobID, name)
	require.NoError(t, err)
	return p
}
