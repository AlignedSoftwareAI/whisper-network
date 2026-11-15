package server

import (
	"context"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/obs"
)

// eligibleWindow is how recently a worker must have been seen to count in
// whisper_queue_eligible_workers (the "no eligible worker" alert, §10.2).
const eligibleWindow = 10 * time.Minute

// refreshStats rebuilds the database-derived metric snapshot.
func (s *Server) refreshStats(ctx context.Context) {
	st, err := s.st.Stats(ctx, eligibleWindow)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn().Err(err).Msg("stats refresh failed")
		}
		return
	}
	ws, err := s.st.ListWorkers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn().Err(err).Msg("stats refresh: listing workers failed")
		}
		return
	}
	var snap obs.Snapshot
	for _, j := range st.Jobs {
		snap.Jobs = append(snap.Jobs, obs.JobCount{Queue: j.Queue, State: string(j.State), N: j.Count})
	}
	for _, t := range st.Tasks {
		snap.Tasks = append(snap.Tasks, obs.TaskCount{Queue: t.Queue, Kind: string(t.Kind), State: string(t.State), N: t.Count})
	}
	for _, q := range st.Queues {
		snap.Queues = append(snap.Queues, obs.QueueStat{Queue: q.Name, BacklogCost: q.BacklogCost,
			OldestPending: q.OldestPending, Weight: float64(q.Weight), VTime: q.VTime, EligibleWorkers: q.EligibleWorkers})
	}
	for state, n := range st.Workers {
		snap.Workers = append(snap.Workers, obs.WorkerCount{State: string(state), N: n})
	}
	for i := range ws {
		wk := &ws[i]
		if wk.State == api.WorkerRejected || wk.State == api.WorkerPending {
			// Rejected workers are kept for the audit trail and pending
			// ones are unvetted self-reports (anyone can register): neither
			// belongs in per-worker series.
			continue
		}
		ws := obs.WorkerStat{ID: wk.ID, Hostname: wk.Facts.Hostname, Description: wk.Facts.Description,
			EngineVersion: wk.Facts.EngineVersion, FFmpegVersion: wk.Facts.FFmpegVersion,
			SandboxLevel: string(wk.Facts.SandboxLevel), Arch: wk.Facts.Arch, Slots: wk.Facts.Slots}
		if wk.LastSeenAt != nil {
			ws.LastSeen = *wk.LastSeenAt
		}
		if g := gaugesOf(wk.Gauges); g != nil {
			ws.SlotsBusy, ws.ChildRSSBytes, ws.Load1 = g.SlotsBusy, g.ChildRSSBytes, g.Load1
			ws.CPUThrottled = secs(g.CPUThrottledSecs)
			ws.ModelsBytes, ws.WorkBytes, ws.StateFreeBytes = g.ModelsBytes, g.WorkBytes, g.StateFreeBytes
		}
		snap.WorkerInfo = append(snap.WorkerInfo, ws)
	}
	if u, err := s.blobs.Usage(); err == nil {
		snap.Storage = obs.StorageBytes{Original: u.Original, Canonical: u.Canonical, Transcript: u.Transcript}
	} else {
		s.log.Warn().Err(err).Msg("stats refresh: blob usage failed")
	}
	if free, err := s.freeBytes(s.cfg.DataDir); err == nil {
		snap.DataFreeBytes = free
	}
	s.metrics.SetSnapshot(snap)
}
