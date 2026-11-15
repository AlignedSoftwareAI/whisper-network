package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/media"
)

// TestDeleteWaitsForJobLock: a delete holds the job lock, so it cannot
// interleave with prepare acceptance or transcript assembly; and assembly
// of a job deleted meanwhile is a quiet no-op that writes nothing.
func TestDeleteWaitsForJobLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	client := h.client(submitOn("public"))
	job := h.submit(client, "public", api.JobParams{Model: "small.en"}, canonicalWAV(speech(1, media.SampleRate)))
	require.True(t, h.hasJobDir(job.ID))

	unlock := h.srv.locks.Lock(job.ID) // assembly or prepare acceptance in progress
	deleted := make(chan struct{})
	go func() {
		h.expect("DELETE", "/job/"+job.ID, client, nil, http.StatusNoContent, nil)
		close(deleted)
	}()
	select {
	case <-deleted:
		t.Fatal("delete did not wait for the job lock")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case <-deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not proceed after the lock was released")
	}
	assert.False(t, h.hasJobDir(job.ID), "blobs removed")

	require.NoError(t, h.srv.finishJob(h.ctx, job.ID), "assembling a deleted job is a no-op")
	assert.False(t, h.hasJobDir(job.ID), "assembly of a deleted job writes nothing")
}

func (h *harness) hasJobDir(jobID string) bool {
	h.t.Helper()
	dirs, err := h.blobs.Jobs()
	require.NoError(h.t, err)
	for _, d := range dirs {
		if d.ID == jobID {
			return true
		}
	}
	return false
}
