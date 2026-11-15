package e2e

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/e2e/e2etest"
	"github.com/deweysasser/whisper-network/internal/obs"
	"github.com/deweysasser/whisper-network/internal/obs/obstest"
	"github.com/deweysasser/whisper-network/internal/server"
	"github.com/deweysasser/whisper-network/internal/store"
	"github.com/deweysasser/whisper-network/internal/worker/workertest"
)

// secretPrompt names meeting participants; it must never reach a log.
const secretPrompt = "Councilors Zebediah Quixote and Philomena Snodgrass discuss the budget"

// TestCanonicalSplitAcrossTwoWorkers (scenario 1, plus the log and metrics
// checks of scenario 10): a canonical WAV takes the fast path, is split into
// several tasks, two workers transcribe them in parallel, and the assembled
// transcript has continuous absolute times and a full provenance header.
func TestCanonicalSplitAcrossTwoWorkers(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t)
	slow := e2etest.ChildEnv(workertest.WhisperMode(workertest.SleepMode("400ms")), workertest.SegmentEvery(1000))
	a := e.StartWorker("worker-a", slow)
	b := e.StartWorker("worker-b", slow)
	e.Approve(a.ID(), "public")
	e.Approve(b.ID(), "public")

	sub := e.Submitter("public")
	const seconds = 17.0
	job := e.Submit(sub, "public", api.JobParams{Prompt: secretPrompt, Label: "council-2026-11-03",
		Source: "https://example.org/watch?v=e2e", Metadata: []byte(`{"requester":"Agatha Pennywhistle"}`)},
		e2etest.SpeechWAV(seconds))
	assert.Equal(t, api.JobQueued, job.State, "a canonical upload skips prepare")
	require.GreaterOrEqual(t, job.Tasks.Total, 3, "17 s at a 4 s target splits into several tasks")

	done := e.WaitJob(job.ID, api.JobDone)
	// job.done (and its metrics) are recorded just after the state change.
	e.WaitEvent(obs.EventJobDone, func(m map[string]any) bool { return m[obs.FieldJobID] == job.ID })
	assert.Equal(t, job.Tasks.Total, done.Tasks.Done)
	assert.InDelta(t, seconds, done.Media.DurationS, 0.01)
	assert.NotNil(t, done.StartedAt)
	assert.NotNil(t, done.CompletedAt)
	assert.Positive(t, done.ComputeS)

	res, err := sub.Result(context.Background(), job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, res.Segments)
	assert.Equal(t, int64(0), res.Segments[0].StartMS)
	for i := 1; i < len(res.Segments); i++ {
		assert.Equal(t, res.Segments[i-1].EndMS, res.Segments[i].StartMS, "segment %d is not continuous", i)
	}
	assert.Equal(t, int64(seconds*1000), res.Segments[len(res.Segments)-1].EndMS)

	txt := e.Transcript(sub, job.ID)
	header, body, _ := strings.Cut(txt, "\n")
	assert.Regexp(t, `^# transcript-source: whisper; whisper\.cpp=`+regexp.QuoteMeta(workertest.FakeWhisperVersion)+
		`; model=e2e\.en; vad=none; language=en; transcribed=\S+; job=`+job.ID+
		`; source=https://example\.org/watch\?v=e2e; tasks=\d+; workers=2$`, header)
	assert.Contains(t, header, "tasks="+itoa(job.Tasks.Total))
	assert.Regexp(t, `^\[00:00\.00\] segment 0\n`, body)
	assert.Equal(t, len(res.Segments), strings.Count(body, "\n"))

	// Both workers did work.
	leased := map[any]int{}
	for _, ev := range e.Logs.Events(t, obs.EventTaskLeased) {
		leased[ev[obs.FieldWorkerID]]++
	}
	assert.Len(t, leased, 2, "tasks ran on both workers: %v", leased)

	// Scenario 10: log hygiene across server and both workers.
	require.NoError(t, a.Stop())
	require.NoError(t, b.Stop())
	for name, logs := range map[string][]byte{"server": e.Logs.Bytes(), "a": a.Logs.Bytes(), "b": b.Logs.Bytes()} {
		obstest.AssertClean(t, logs)
		s := string(logs)
		for _, secret := range []string{"Zebediah", "Philomena", "Agatha", "segment 1", e.AdminToken} {
			assert.NotContains(t, s, secret, "%s log leaks %q", name, secret)
		}
	}
	for _, ev := range []obs.Event{obs.EventJobCreated, obs.EventTaskLeased, obs.EventTaskDone, obs.EventJobDone,
		obs.EventWorkerRegistered, obs.EventWorkerApproved} {
		assert.NotEmpty(t, e.Logs.Events(t, ev), "server never logged %s", ev)
	}
	for _, w := range []*e2etest.Worker{a, b} {
		assert.NotEmpty(t, w.Logs.Events(t, obs.EventWorkerSandbox), "worker never logged its sandbox level")
		assert.NotEmpty(t, w.Logs.Events(t, obs.EventTaskDone))
	}

	// Metrics: the catalog's series, with the expected labels.
	e.Eventually(func() bool { return strings.Contains(e.Scrape(), `whisper_jobs{queue="public",state="done"} 1`) },
		"whisper_jobs gauge never showed the done job")
	m := e.Scrape()
	for _, series := range []string{
		`whisper_jobs_created_total{queue="public",weight="medium",fast_path="true"} 1`,
		`whisper_jobs_finished_total{queue="public",outcome="done"} 1`,
		`whisper_leases_ended_total{outcome="done",kind="transcribe"} ` + itoa(job.Tasks.Total),
		`whisper_sched_served_cost_total{queue="public",class="medium"}`,
		`whisper_task_compute_seconds_bucket{kind="transcribe",model="e2e.en"`,
		`whisper_task_realtime_factor_bucket{model="e2e.en",worker="` + a.ID() + `"`,
		`whisper_worker_audio_seconds_total{worker="` + b.ID() + `",model="e2e.en"}`,
		`whisper_http_requests_total{route="/worker/{id}/leases",method="POST",code="201"}`,
		`whisper_upload_bytes_total{queue="public"}`,
		`whisper_input_served_bytes_total{kind="transcribe"}`,
		`whisper_claim_wait_seconds_bucket`,
		`whisper_workers{state="active"} 2`,
		`whisper_worker_info{worker="` + a.ID() + `"`,
		`whisper_worker_last_seen_timestamp_seconds{worker="` + a.ID() + `"}`,
		`whisper_queue_weight{queue="public"} 100`,
		`whisper_data_free_bytes`,
	} {
		assert.Contains(t, m, series)
	}
	assert.NotContains(t, m, job.ID, "metrics are never labelled by job")
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestWorkerOnAnotherAddressThanPublicURL: with --public-url set to the
// address clients use (here an unreachable proxy name), a worker that
// reaches the server directly still downloads its input and finishes the
// job, because lease input URLs stay relative to the worker's own --server
// (issue #29).
func TestWorkerOnAnotherAddressThanPublicURL(t *testing.T) {
	t.Parallel()
	e := e2etest.New(t, func(c *server.Config, _ *store.Config) {
		c.PublicURL = "https://wn-proxy.invalid"
	})
	w := e.StartWorker("direct")
	e.Approve(w.ID(), "public")
	sub := e.Submitter("public")
	job := e.Submit(sub, "public", api.JobParams{}, e2etest.SpeechWAV(2))
	done := e.WaitJob(job.ID, api.JobDone)
	assert.Equal(t, 0, done.Tasks.Failed, "no attempt failed on the input URL")
}
