package worker

import (
	"context"
	"time"

	"github.com/deweysasser/whisper-network/internal/api"
	"github.com/deweysasser/whisper-network/internal/client"
)

// claimSlack is how much longer than the server's long-poll wait the client
// waits for the response.
const claimSlack = 15 * time.Second

// slot is one slot goroutine (DESIGN.md §7.2): reserve memory for what fits,
// claim, run the lease, release the memory, repeat. It returns when runCtx is
// cancelled, after its current lease has been finished or released.
func (w *Worker) slot(runCtx, workCtx context.Context, n int) {
	log := w.log.With().Int("slot", n).Logger()
	failures := 0
	for runCtx.Err() == nil {
		off, res, err := w.mem.reserve(runCtx, w.facts.Kinds, w.offered, w.cfg.Limits.PrepareMemory)
		if err != nil {
			return
		}
		lease, err := w.claim(runCtx, off)
		if err != nil || lease == nil {
			res.release()
			if err == nil {
				failures = 0
				continue
			}
			var delay time.Duration
			switch {
			case runCtx.Err() != nil:
				return
			case isFatal(err):
				w.fail(err)
				return
			case client.IsPaused(err), client.IsPending(err):
				delay = w.pollInterval
				log.Debug().Err(err).Dur("retry_in", delay).Msg("not allowed to claim; polling slowly")
			default:
				delay = w.cfg.RetryBackoff.Delay(failures)
				failures++
				log.Warn().Err(err).Dur("retry_in", delay).Msg("claim failed")
			}
			if w.cfg.Sleep(runCtx, delay) != nil {
				return
			}
			continue
		}
		failures = 0
		res.shrink(w.leaseMemory(lease))
		w.busy.Add(1)
		w.handleLease(runCtx, workCtx, lease)
		w.busy.Add(-1)
		res.release()
	}
}

func (w *Worker) claim(ctx context.Context, off offer) (*api.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.ClaimWait+claimSlack)
	defer cancel()
	return w.api.Claim(ctx, w.creds.ID, api.ClaimRequest{Kinds: off.kinds, Models: off.models}, w.cfg.ClaimWait)
}

// leaseMemory is the memory a lease's work needs.
func (w *Worker) leaseMemory(l *api.Lease) int64 {
	if l.Kind == api.KindPrepare {
		return w.cfg.Limits.PrepareMemory
	}
	if m, ok := w.cfg.Catalog.Lookup(l.Model); ok {
		return m.PeakRSS
	}
	return 0
}
