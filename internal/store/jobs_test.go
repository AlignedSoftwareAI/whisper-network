package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

func TestCreateJobIdempotencyKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	nj := NewJob{QueueID: f.pubID, ClientID: "c1", IdempotencyKey: "k",
		Params:   api.JobParams{Model: "small.en", Metadata: []byte(`{"a":1}`)},
		Original: Media{Bytes: 5, SHA256: "x", Type: "video/mp4"}}
	j1, created, err := f.s.CreateJob(f.ctx, nj)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, api.JobPreparing, j1.State)
	assert.Equal(t, "en", j1.Language)
	assert.Equal(t, api.WeightMedium, j1.Weight)
	assert.Equal(t, api.TaskCounts{Total: 1, Pending: 1}, j1.Tasks)
	assert.JSONEq(t, `{"a":1}`, string(j1.Metadata))

	j2, created, err := f.s.CreateJob(f.ctx, nj)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, j1.ID, j2.ID)

	got, err := f.s.JobByIdempotencyKey(f.ctx, "c1", "k")
	require.NoError(t, err)
	assert.Equal(t, j1.ID, got.ID)

	nj.ClientID = "c2" // keys are per client
	j3, created, err := f.s.CreateJob(f.ctx, nj)
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, j1.ID, j3.ID)
}

func TestCreateJobValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	base := NewJob{QueueID: f.pubID, ClientID: "c", Params: api.JobParams{Model: "small.en"}}
	cases := map[string]func(*NewJob){
		"bad weight":      func(j *NewJob) { j.Params.Weight = "urgent" },
		"no model":        func(j *NewJob) { j.Params.Model = "" },
		"no client":       func(j *NewJob) { j.ClientID = "" },
		"ranges no audio": func(j *NewJob) { j.Ranges = []SampleRange{{0, 1}} },
		"no ranges":       func(j *NewJob) { j.Canonical = &Canonical{Samples: 10} },
		"range overflow": func(j *NewJob) {
			j.Canonical = &Canonical{Samples: 10}
			j.Ranges = []SampleRange{{0, 11}}
		},
		"overlap": func(j *NewJob) {
			j.Canonical = &Canonical{Samples: 10}
			j.Ranges = []SampleRange{{0, 6}, {5, 10}}
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			nj := base
			mod(&nj)
			_, _, err := f.s.CreateJob(f.ctx, nj)
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
	nj := base
	nj.QueueID = 999
	_, _, err := f.s.CreateJob(f.ctx, nj)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestTaskCostFromModelFactor(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(c *Config) { c.ModelFactors = map[string]float64{"small.en": 2} })
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 100)
	var cost float64
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT cost FROM tasks WHERE job_id = ?`, j.ID).Scan(&cost))
	assert.Equal(t, 200.0, cost)
	assert.Equal(t, 300.0, f.s.TranscribeCost("large-v3-turbo", 100*canonicalSampleRate), "built-in factor")
	assert.Equal(t, 100.0, f.s.TranscribeCost("unknown", 100*canonicalSampleRate))

	p := f.prepJob(f.pubID, "c")
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT cost FROM tasks WHERE job_id = ?`, p.ID).Scan(&cost))
	assert.Equal(t, DefaultPrepareCost, cost)
}

func TestListJobsVisibilityFiltersAndPaging(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	other := f.queue("other", 0)
	var mine []string
	for i := range 5 {
		mine = append(mine, f.job(f.pubID, "me", api.WeightMedium, 1, 10).ID)
		f.job(f.pubID, "you", api.WeightMedium, 1, 10)
		if i == 2 {
			f.clk.Advance(time.Minute)
		}
	}
	f.job(other, "me", api.WeightMedium, 1, 10)

	all, next, err := f.s.ListJobs(f.ctx, JobFilter{QueueID: f.pubID})
	require.NoError(t, err)
	assert.Len(t, all, 10)
	assert.Empty(t, next)

	// Visibility: only my jobs, paged two at a time.
	var got []string
	cursor := ""
	for {
		page, next, err := f.s.ListJobs(f.ctx, JobFilter{QueueID: f.pubID, ClientID: "me", Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		for _, j := range page {
			assert.Equal(t, "me", j.ClientID)
			got = append(got, j.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	assert.Equal(t, mine, got)

	// Since: only jobs updated after the clock jump.
	since, _, err := f.s.ListJobs(f.ctx, JobFilter{QueueID: f.pubID, Since: f.clk.Now().Add(-time.Second)})
	require.NoError(t, err)
	assert.Len(t, since, 4)

	// State filter.
	w := f.worker(api.SandboxFull, f.pubID)
	f.claim(w)
	running, _, err := f.s.ListJobs(f.ctx, JobFilter{QueueID: f.pubID, States: []api.JobState{api.JobRunning}})
	require.NoError(t, err)
	require.Len(t, running, 1)
	assert.Equal(t, mine[0], running[0].ID, "oldest job is served first")

	_, _, err = f.s.ListJobs(f.ctx, JobFilter{QueueID: f.pubID, Cursor: "!!"})
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestDeleteJobCascadesAndCancelsLeases(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 3, 60)
	l1, l2 := f.claim(w), f.claim(w)

	cancelled, err := f.s.DeleteJob(f.ctx, j.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{l1.ID, l2.ID}, cancelled)

	_, err = f.s.Job(f.ctx, j.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	var tasks int
	require.NoError(t, f.s.r.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM tasks WHERE job_id = ?`, j.ID).Scan(&tasks))
	assert.Zero(t, tasks)

	_, err = f.s.Heartbeat(f.ctx, l1.ID, w, ttl, LeaseAgeLimit{})
	assert.ErrorIs(t, err, ErrLeaseGone, "holder learns the job is gone")
	_, err = f.s.Lease(f.ctx, l1.ID, w)
	assert.ErrorIs(t, err, ErrLeaseGone)
	_, err = f.s.CompleteTranscribe(f.ctx, l2.ID, w, TranscribeCompletion{Result: []byte(`{}`)})
	assert.ErrorIs(t, err, ErrLeaseGone)

	_, err = f.s.DeleteJob(f.ctx, j.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Nil(t, f.claim(w), "nothing left to claim")
}

func TestJobAPIAndDownloads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightHigh, 2, 30)
	a := j.API(f.clk.Now())
	require.NotNil(t, a.WaitS)
	assert.Zero(t, *a.WaitS)
	assert.Nil(t, a.ElapsedS)
	assert.Equal(t, 60.0, a.Media.DurationS)

	f.clk.Advance(10 * time.Second)
	l1 := f.claim(w)
	f.clk.Advance(20 * time.Second)
	l2 := f.claim(w)
	assert.False(t, f.complete(l1))
	f.clk.Advance(5 * time.Second)
	assert.True(t, f.complete(l2))

	out, err := f.s.JobOutputs(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Nil(t, out.Prepare)
	require.Len(t, out.Transcribe, 2)
	assert.Equal(t, 1, out.Transcribe[0].Seq)
	assert.Equal(t, int64(30*canonicalSampleRate), out.Transcribe[1].StartSample)
	assert.Equal(t, "whisper.cpp 1.9.5", out.Transcribe[0].EngineVersion)
	assert.Equal(t, w, out.Transcribe[0].WorkerID)

	done, err := f.s.MarkJobDone(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobDone, done.State)
	a = done.API(f.clk.Now().Add(time.Hour))
	assert.Equal(t, 10.0, *a.WaitS)
	assert.Equal(t, 25.0, *a.ElapsedS, "completed jobs ignore now")
	assert.Equal(t, 2.0, a.ComputeS)
	assert.Equal(t, api.TaskCounts{Total: 2, Done: 2}, a.Tasks)
	_, err = f.s.MarkJobDone(f.ctx, j.ID)
	require.NoError(t, err, "idempotent")

	require.NoError(t, f.s.RecordDownload(f.ctx, j.ID))
	f.clk.Advance(time.Second)
	require.NoError(t, f.s.RecordDownload(f.ctx, j.ID))
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, got.DownloadCount)
	assert.True(t, got.LastDownloadedAt.After(*got.FirstDownloadedAt))
	assert.Equal(t, done.UpdatedAt, got.UpdatedAt, "downloads do not bump updated_at")
}

func TestMarkJobDoneRequiresAllTasks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	j := f.job(f.pubID, "c", api.WeightMedium, 1, 10)
	_, err := f.s.MarkJobDone(f.ctx, j.ID)
	assert.ErrorIs(t, err, ErrConflict)
	_, err = f.s.MarkJobDone(f.ctx, "missing")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFailJob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := f.worker(api.SandboxFull, f.pubID)
	j := f.job(f.pubID, "c", api.WeightMedium, 2, 10)
	l := f.claim(w)
	cancelled, err := f.s.FailJob(f.ctx, j.ID, "assembly failed")
	require.NoError(t, err)
	assert.Equal(t, []string{l.ID}, cancelled)
	got, err := f.s.Job(f.ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, api.JobFailed, got.State)
	assert.Equal(t, "assembly failed", got.Error)
	assert.NotNil(t, got.CompletedAt)
	assert.Equal(t, 2, got.Tasks.Total)
	assert.Zero(t, got.Tasks.Pending+got.Tasks.Leased)
	_, err = f.s.FailJob(f.ctx, j.ID, "again")
	require.NoError(t, err)
}
