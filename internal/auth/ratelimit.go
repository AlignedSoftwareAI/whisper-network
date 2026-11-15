package auth

import (
	"net/netip"
	"sync"
	"time"
)

// LimiterConfig configures a RateLimiter.
type LimiterConfig struct {
	// Interval is the time to earn one token back.
	Interval time.Duration
	// Burst is the bucket capacity (and the initial allowance per key).
	Burst int
	// MaxKeys bounds memory: the most distinct keys tracked at once.
	MaxKeys int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a per-key token bucket (key = client IP) that is safe for
// concurrent use and bounded in memory. Keys that parse as IP addresses are
// bucketed by IPKey, so one IPv6 host cannot multiply its allowance by
// rotating through the addresses of its /64.
type RateLimiter struct {
	cfg LimiterConfig

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// NewRateLimiter returns a limiter. Non-positive Interval, Burst or MaxKeys
// are replaced by 1 minute, 1 and 10000 respectively.
func NewRateLimiter(cfg LimiterConfig) *RateLimiter {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = 10000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RateLimiter{cfg: cfg, buckets: make(map[string]*bucket)}
}

// Allow consumes one token for key and reports whether one was available.
func (l *RateLimiter) Allow(key string) bool {
	key = IPKey(key)
	now := l.cfg.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		l.makeRoom(now)
		b = &bucket{tokens: float64(l.cfg.Burst), last: now}
		l.buckets[key] = b
	} else if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(float64(l.cfg.Burst), b.tokens+float64(elapsed)/float64(l.cfg.Interval))
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Len returns the number of keys currently tracked.
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// makeRoom keeps len(buckets) < MaxKeys before an insert. A bucket idle for
// a full refill period is indistinguishable from an absent one, so those are
// dropped first (swept at most once per refill period to keep Allow cheap);
// if the table is still full an arbitrary key is evicted.
func (l *RateLimiter) makeRoom(now time.Time) {
	if len(l.buckets) < l.cfg.MaxKeys {
		return
	}
	refill := l.cfg.Interval * time.Duration(l.cfg.Burst)
	if now.Sub(l.lastSweep) >= refill {
		l.lastSweep = now
		for k, b := range l.buckets {
			if now.Sub(b.last) >= refill {
				delete(l.buckets, k)
			}
		}
	}
	for k := range l.buckets {
		if len(l.buckets) < l.cfg.MaxKeys {
			return
		}
		delete(l.buckets, k)
	}
}

// IPKey returns the rate-limit bucket of an address: an IPv4 address (or an
// IPv4-mapped IPv6 one) as itself, an IPv6 address as its /64 prefix, which
// is the smallest block a single subscriber is normally assigned. Anything
// that is not an IP address is returned unchanged.
func IPKey(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return addr
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip, 64).Masked().String()
}
