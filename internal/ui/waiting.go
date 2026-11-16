package ui

import (
	"context"
	"fmt"
	"slices"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/store"
)

// Why a pending task is waiting (#54, DESIGN.md §6.3). Best effort and
// cheap: the queue's eligible workers are read once per page, and each
// task is checked against them in memory the way Claim would offer it. The
// reasons never name a worker, so they need no visibility check.

// fleetView is what the waiting reasons are judged against: the active
// workers seen recently that hold process on one queue.
type fleetView struct {
	queue      string
	workers    []store.Worker
	gauges     []*api.Gauges // workers[i]'s last heartbeat gauges, or nil
	prepareMin api.SandboxLevel
}

// queueFleet reads the eligible workers of queue (id queueID).
func (u *UI) queueFleet(ctx context.Context, queue string, queueID int64) (*fleetView, error) {
	st := u.srv.Store()
	ws, err := st.ProcessWorkers(ctx, queueID, onlineWindow)
	if err != nil {
		return nil, err
	}
	f := &fleetView{queue: queue, workers: ws, gauges: make([]*api.Gauges, len(ws)),
		prepareMin: st.PrepareMinSandbox()}
	for i := range ws {
		f.gauges[i] = workerGauges(&ws[i])
	}
	return f, nil
}

// waitingReasons lists why no eligible worker can take a pending task of
// kind for a job using model right now; failedOn are the workers the
// retry-elsewhere rule keeps it from. None means the task is waiting its
// turn (fair share, or the workers simply have not polled yet).
func (f *fleetView) waitingReasons(kind api.TaskKind, model string, failedOn []string) []string {
	if len(f.workers) == 0 {
		return []string{fmt.Sprintf("No active worker may process queue %s (none is approved for it, or none was seen in the last %s).",
			f.queue, formatDuration(onlineWindow))}
	}
	var able []int
	for i := range f.workers {
		facts := &f.workers[i].Facts
		switch kind {
		case api.KindPrepare:
			if store.CanPrepare(*facts, f.prepareMin) {
				able = append(able, i)
			}
		default:
			if slices.Contains(facts.Kinds, api.KindTranscribe) && slices.Contains(facts.Models, model) {
				able = append(able, i)
			}
		}
	}
	if len(able) == 0 {
		if kind == api.KindPrepare {
			return []string{fmt.Sprintf("Converting the upload needs a worker offering prepare tasks at sandbox level %s or stronger; no eligible worker does.",
				f.prepareMin)}
		}
		return []string{fmt.Sprintf("No eligible worker offers model %s.", model)}
	}
	var free []int
	for _, i := range able {
		if !slices.Contains(failedOn, f.workers[i].ID) {
			free = append(free, i)
		}
	}
	if len(free) == 0 {
		if len(able) == 1 {
			return []string{"The only eligible worker able to run it already failed it; it waits for another worker (retry elsewhere)."}
		}
		return []string{fmt.Sprintf("All %d eligible workers able to run it already failed it; it waits for another worker (retry elsewhere).", len(able))}
	}
	busy, deferred := 0, 0
	for _, i := range free {
		g := f.gauges[i]
		switch {
		case g == nil:
		case g.DeferredForLoad:
			deferred++
		case g.SlotsBusy >= f.workers[i].Facts.Slots:
			busy++
		}
	}
	var out []string
	if busy > 0 && busy+deferred == len(free) {
		out = append(out, fmt.Sprintf("Workers are busy: every slot on the %d eligible worker(s) able to run it is in use.", busy))
	}
	if deferred > 0 && busy+deferred == len(free) {
		out = append(out, fmt.Sprintf("%d eligible worker(s) are deferring work because their host is busy with other work.", deferred))
	}
	return out
}
