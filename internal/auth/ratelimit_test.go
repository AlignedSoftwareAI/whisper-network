package auth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestRateLimiterBurstAndRefill(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(LimiterConfig{Interval: time.Minute, Burst: 3, MaxKeys: 10, Now: clk.Now})

	for range 3 {
		assert.True(t, l.Allow("1.1.1.1"))
	}
	assert.False(t, l.Allow("1.1.1.1"))
	assert.True(t, l.Allow("2.2.2.2"), "keys are independent")

	clk.Advance(30 * time.Second)
	assert.False(t, l.Allow("1.1.1.1"), "half a token is not enough")
	clk.Advance(30 * time.Second)
	assert.True(t, l.Allow("1.1.1.1"))
	assert.False(t, l.Allow("1.1.1.1"))

	clk.Advance(24 * time.Hour)
	for range 3 {
		assert.True(t, l.Allow("1.1.1.1"))
	}
	assert.False(t, l.Allow("1.1.1.1"), "refill is capped at burst")
}

func TestRateLimiterDefaults(t *testing.T) {
	l := NewRateLimiter(LimiterConfig{})
	assert.True(t, l.Allow("k"))
	assert.False(t, l.Allow("k"))
}

func TestRateLimiterClockGoingBackwards(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(LimiterConfig{Interval: time.Minute, Burst: 1, Now: clk.Now})
	assert.True(t, l.Allow("k"))
	clk.Advance(-time.Hour)
	assert.False(t, l.Allow("k"))
}

func TestRateLimiterEvictsIdleKeys(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(LimiterConfig{Interval: time.Minute, Burst: 2, MaxKeys: 5, Now: clk.Now})
	for i := range 5 {
		l.Allow(fmt.Sprint("k", i))
	}
	assert.Equal(t, 5, l.Len())
	clk.Advance(3 * time.Minute) // all idle for more than a refill period
	l.Allow("new")
	assert.Equal(t, 1, l.Len())
}

func TestRateLimiterBoundedWhenAllBusy(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(LimiterConfig{Interval: time.Hour, Burst: 1, MaxKeys: 5, Now: clk.Now})
	for i := range 1000 {
		assert.True(t, l.Allow(fmt.Sprint("k", i)))
		assert.LessOrEqual(t, l.Len(), 5)
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	clk := newClock()
	const burst = 50
	l := NewRateLimiter(LimiterConfig{Interval: time.Hour, Burst: burst, MaxKeys: 100, Now: clk.Now})

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if l.Allow("shared") {
					allowed.Add(1)
				}
				l.Allow("other")
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, burst, allowed.Load(), "exactly burst tokens handed out under contention")
}

func TestIPKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":                "203.0.113.7",
		"::ffff:203.0.113.7":         "203.0.113.7",
		"2001:db8:1:2:aaaa::1":       "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:0:9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":            "2001:db8:1:3::/64",
		"fe80::1%eth0":               "fe80::/64",
		"not-an-ip":                  "not-an-ip",
		"":                           "",
	} {
		assert.Equal(t, want, IPKey(in), in)
	}
}

// TestRateLimiterBucketsIPv6By64: rotating through the addresses of one /64
// does not earn extra tokens; a different /64 (or IPv4 address) does.
func TestRateLimiterBucketsIPv6By64(t *testing.T) {
	clk := newClock()
	l := NewRateLimiter(LimiterConfig{Interval: time.Hour, Burst: 2, Now: clk.Now})
	assert.True(t, l.Allow("2001:db8:0:1::1"))
	assert.True(t, l.Allow("2001:db8:0:1::2"))
	assert.False(t, l.Allow("2001:db8:0:1:dead:beef:0:3"), "same /64")
	assert.True(t, l.Allow("2001:db8:0:2::1"), "another /64")
	assert.True(t, l.Allow("192.0.2.1"))
	assert.True(t, l.Allow("192.0.2.2"), "IPv4 is keyed per address")
	assert.Equal(t, 4, l.Len())
}
