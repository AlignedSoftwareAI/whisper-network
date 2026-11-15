package backoff

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPolicyDelay(t *testing.T) {
	p := Policy{Initial: 30 * time.Second, Max: 5 * time.Minute}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		assert.Equal(t, w, p.Delay(i), "attempt %d", i)
	}
	assert.Equal(t, 5*time.Minute, p.Delay(1000), "large attempts stay capped without overflow")
}

func TestPolicyJitterStaysInBounds(t *testing.T) {
	p := Policy{Initial: time.Second, Max: time.Second, Jitter: 0.25}
	for range 200 {
		d := p.Delay(3)
		assert.GreaterOrEqual(t, d, 750*time.Millisecond)
		assert.LessOrEqual(t, d, 1250*time.Millisecond)
	}
}

func TestSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, Sleep(ctx, time.Hour), context.Canceled)
	assert.NoError(t, Sleep(context.Background(), time.Millisecond))
}

func TestScaledRecords(t *testing.T) {
	var got []time.Duration
	s := Scaled(1e6, func(d time.Duration) { got = append(got, d) })
	start := time.Now()
	assert.NoError(t, s(context.Background(), time.Minute))
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, []time.Duration{time.Minute}, got)
}
