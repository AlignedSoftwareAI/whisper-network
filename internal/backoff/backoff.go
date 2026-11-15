// Package backoff computes retry delays and sleeps for them. It holds no
// state: a Policy is a value, and the caller keeps the attempt count.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Policy describes exponential backoff: the n-th delay (from 0) is
// Initial × Multiplier^n, capped at Max, with up to ±Jitter of it randomized.
type Policy struct {
	Initial    time.Duration
	Max        time.Duration
	Multiplier float64 // values <= 1 mean 2
	Jitter     float64 // fraction in [0, 1); 0 disables jitter
}

// Delay returns the wait before retry number attempt (0-based).
func (p Policy) Delay(attempt int) time.Duration {
	mult := p.Multiplier
	if mult <= 1 {
		mult = 2
	}
	d := float64(p.Initial)
	for i := 0; i < attempt && (p.Max <= 0 || d < float64(p.Max)); i++ {
		d *= mult
	}
	if p.Max > 0 && d > float64(p.Max) {
		d = float64(p.Max)
	}
	if p.Jitter > 0 {
		d += d * p.Jitter * (2*rand.Float64() - 1)
	}
	if d < 0 {
		return 0
	}
	return time.Duration(d)
}

// Sleeper waits for d or until ctx is done, returning ctx's error in the
// latter case. It is the injection point for tests that must not wait in real
// time.
type Sleeper func(ctx context.Context, d time.Duration) error

// Sleep is the real-time Sleeper.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Scaled returns a Sleeper that sleeps d/factor in real time. Tests use it to
// run production delays (30 s heartbeats, 5 min backoff) in milliseconds while
// keeping their proportions. If record is non-nil it receives every requested
// (unscaled) duration.
func Scaled(factor float64, record func(time.Duration)) Sleeper {
	return func(ctx context.Context, d time.Duration) error {
		if record != nil {
			record(d)
		}
		return Sleep(ctx, time.Duration(float64(d)/factor))
	}
}
