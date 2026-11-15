package sched

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deweysasser/whisper-network/internal/api"
)

// sim is a tiny fleet simulation: every queue/class in backlog has an
// unbounded supply of tasks whose cost comes from costFn.
type sim struct {
	st      State
	weights map[int64]float64
	cw      ClassWeights
	served  map[Key]float64
	costFn  func() float64
	picks   []Key
}

func newSim(weights map[int64]float64, costFn func() float64) *sim {
	return &sim{
		weights: weights,
		cw:      DefaultClassWeights(),
		served:  map[Key]float64{},
		costFn:  costFn,
	}
}

// step serves one task from the given candidates and returns what it picked.
func (s *sim) step(t *testing.T, candidates []Key) Key {
	t.Helper()
	k, ok := s.st.Pick(candidates)
	require.True(t, ok)
	require.Contains(t, candidates, k, "Pick must return one of the candidates")
	cost := s.costFn()
	s.st.Charge(k, cost, s.weights[k.Queue], s.cw)
	s.served[k] += cost
	s.picks = append(s.picks, k)
	return k
}

func (s *sim) queueServed(q int64) float64 {
	var sum float64
	for k, v := range s.served {
		if k.Queue == q {
			sum += v
		}
	}
	return sum
}

func constCost(c float64) func() float64 { return func() float64 { return c } }

// randCost returns a deterministic cost source in [lo, hi].
func randCost(seed uint64, lo, hi float64) func() float64 {
	r := rand.New(rand.NewPCG(seed, seed))
	return func() float64 { return lo + r.Float64()*(hi-lo) }
}

func keys(qs ...int64) []Key {
	out := make([]Key, 0, len(qs))
	for _, q := range qs {
		out = append(out, Key{Queue: q, Class: api.WeightMedium})
	}
	return out
}

// assertFair checks the SFQ bound between two continuously backlogged queues:
// |S_a/W_a - S_b/W_b| <= cmax/W_a + cmax/W_b, i.e. within one task cost.
func assertFair(t *testing.T, sa, wa, sb, wb, cmax float64) {
	t.Helper()
	diff := math.Abs(sa/wa - sb/wb)
	assert.LessOrEqual(t, diff, cmax/wa+cmax/wb+1e-9,
		"normalized service diverged: %v/%v vs %v/%v", sa, wa, sb, wb)
}

func TestTwoQueuesConvergeToWeights(t *testing.T) {
	for name, cost := range map[string]func() float64{
		"constant": constCost(3600),
		"random":   randCost(1, 60, 3*3600),
	} {
		t.Run(name, func(t *testing.T) {
			s := newSim(map[int64]float64{1: 100, 2: 300}, cost)
			for range 2000 {
				s.step(t, keys(1, 2))
				assertFair(t, s.queueServed(1), 100, s.queueServed(2), 300, 3*3600)
			}
			share := s.queueServed(2) / (s.queueServed(1) + s.queueServed(2))
			assert.InDelta(t, 0.75, share, 0.01)
		})
	}
}

func TestThreeQueuesMixedClasses(t *testing.T) {
	weights := map[int64]float64{1: 100, 2: 200, 3: 700}
	s := newSim(weights, randCost(7, 60, 3600))
	var cands []Key
	for q := range int64(3) {
		for _, c := range Classes {
			cands = append(cands, Key{Queue: q + 1, Class: c})
		}
	}
	for range 5000 {
		s.step(t, cands)
		for a := int64(1); a <= 3; a++ {
			for b := a + 1; b <= 3; b++ {
				assertFair(t, s.queueServed(a), weights[a], s.queueServed(b), weights[b], 3600)
			}
		}
	}
	// Within each queue the classes still share 4:2:1.
	for q := int64(1); q <= 3; q++ {
		hi := s.served[Key{q, api.WeightHigh}]
		med := s.served[Key{q, api.WeightMedium}]
		lo := s.served[Key{q, api.WeightLow}]
		assert.LessOrEqual(t, math.Abs(hi/4-med/2), 3600.0/4+3600.0/2)
		assert.LessOrEqual(t, math.Abs(med/2-lo/1), 3600.0/2+3600.0/1)
	}
}

func TestClassSharesWithinQueue(t *testing.T) {
	s := newSim(map[int64]float64{1: 100}, constCost(100))
	cands := []Key{{1, api.WeightLow}, {1, api.WeightMedium}, {1, api.WeightHigh}}
	for range 700 {
		s.step(t, cands)
	}
	hi := s.served[Key{1, api.WeightHigh}]
	med := s.served[Key{1, api.WeightMedium}]
	lo := s.served[Key{1, api.WeightLow}]
	assert.InDelta(t, 40000, hi, 100)
	assert.InDelta(t, 20000, med, 100)
	assert.InDelta(t, 10000, lo, 100)
	assert.Positive(t, lo, "low is slowed, never starved")
}

func TestIdleQueueDoesNotBankCredit(t *testing.T) {
	s := newSim(map[int64]float64{1: 100, 2: 100}, constCost(100))
	// Queue 1 runs alone for a long time; queue 2 is idle with V=0.
	for range 500 {
		s.step(t, keys(1))
	}
	// Queue 2 gets work. Queue 1 is still backlogged.
	s.st.Reactivate(keys(2)[0], keys(1))
	assert.Equal(t, s.st.Get(QueueKey(1)), s.st.Get(QueueKey(2)),
		"reactivated queue starts at the backlogged minimum")

	before1 := s.queueServed(1)
	for range 100 {
		s.step(t, keys(1, 2))
	}
	got1 := s.queueServed(1) - before1
	got2 := s.queueServed(2)
	assert.InDelta(t, got1, got2, 100, "after waking, queue 2 shares equally instead of monopolizing")
}

func TestWithoutReactivationIdleQueueWouldMonopolize(t *testing.T) {
	// Guards the test above: it would fail if Reactivate were a no-op.
	s := newSim(map[int64]float64{1: 100, 2: 100}, constCost(100))
	for range 500 {
		s.step(t, keys(1))
	}
	for range 100 {
		s.step(t, keys(1, 2))
	}
	assert.Equal(t, float64(100*100), s.queueServed(2), "queue 2 took every claim")
}

func TestReactivationWhenNothingBacklogged(t *testing.T) {
	s := newSim(map[int64]float64{1: 100, 2: 100}, constCost(100))
	for range 10 {
		s.step(t, keys(1))
	}
	last := s.st.Get(QueueKey(1))
	// Everything drained; queue 2 wakes with no peers: it takes the last
	// charged V, not its stale 0.
	s.st.Reactivate(keys(2)[0], nil)
	assert.Equal(t, last, s.st.Get(QueueKey(2)))

	// A queue whose own V is already ahead keeps it (max, never backwards).
	s.st.Reactivate(keys(1)[0], keys(2))
	assert.Equal(t, last, s.st.Get(QueueKey(1)))
}

func TestClassReactivation(t *testing.T) {
	s := newSim(map[int64]float64{1: 100}, constCost(100))
	med := Key{1, api.WeightMedium}
	low := Key{1, api.WeightLow}
	for range 100 {
		s.step(t, []Key{med})
	}
	// A low job arrives while medium is backlogged: the queue is busy (no
	// queue-level change), the low class starts at medium's V.
	vq := s.st.Get(QueueKey(1))
	s.st.Reactivate(low, []Key{med})
	assert.Equal(t, vq, s.st.Get(QueueKey(1)))
	assert.Equal(t, s.st.Get(med), s.st.Get(low))

	// Re-adding work to an already backlogged class changes nothing.
	before := s.st.Get(low)
	s.st.Reactivate(low, []Key{med, low})
	assert.Equal(t, before, s.st.Get(low))
}

func TestEligibilitySubsets(t *testing.T) {
	// Worker X may take queues 1 and 2; worker Y only queue 2. They alternate.
	s := newSim(map[int64]float64{1: 100, 2: 100}, constCost(100))
	for i := range 1000 {
		if i%2 == 0 {
			s.step(t, keys(1, 2))
		} else {
			k := s.step(t, keys(2))
			require.Equal(t, int64(2), k.Queue)
		}
	}
	// X compensates for Y: the fleet-wide split still matches the weights.
	assert.InDelta(t, s.queueServed(1), s.queueServed(2), 200)
}

func TestWeightChangeMidRun(t *testing.T) {
	weights := map[int64]float64{1: 100, 2: 100}
	s := newSim(weights, constCost(100))
	for range 400 {
		s.step(t, keys(1, 2))
	}
	assert.InDelta(t, s.queueServed(1), s.queueServed(2), 100)

	weights[1] = 300 // applies from the next charge
	b1, b2 := s.queueServed(1), s.queueServed(2)
	for range 4000 {
		s.step(t, keys(1, 2))
	}
	d1, d2 := s.queueServed(1)-b1, s.queueServed(2)-b2
	assert.InDelta(t, 0.75, d1/(d1+d2), 0.01)
}

func TestTieBreaks(t *testing.T) {
	var st State
	k, ok := st.Pick([]Key{{3, api.WeightLow}, {2, api.WeightLow}, {2, api.WeightMedium}, {2, api.WeightHigh}})
	require.True(t, ok)
	assert.Equal(t, Key{2, api.WeightHigh}, k, "lowest queue id, then high > medium > low")

	_, ok = st.Pick(nil)
	assert.False(t, ok)
	assert.Empty(t, st.V, "Pick does not modify state")
}

func TestDeterminism(t *testing.T) {
	run := func() []Key {
		s := newSim(map[int64]float64{1: 50, 2: 100, 3: 150}, randCost(42, 10, 1000))
		var cands []Key
		for q := int64(1); q <= 3; q++ {
			for _, c := range Classes {
				cands = append(cands, Key{q, c})
			}
		}
		for range 1000 {
			s.step(t, cands)
		}
		return s.picks
	}
	assert.Equal(t, run(), run())
}

func TestChargeGuardsBadWeights(t *testing.T) {
	var st State
	st.Charge(Key{1, api.WeightMedium}, 10, 0, ClassWeights{})
	assert.Equal(t, 10.0, st.Get(QueueKey(1)))
	assert.Equal(t, 10.0, st.Get(Key{1, api.WeightMedium}))
}

func TestClassWeightsValidate(t *testing.T) {
	require.NoError(t, DefaultClassWeights().Validate())
	assert.Error(t, ClassWeights{Low: 0, Medium: 2, High: 4}.Validate())
	assert.Error(t, ClassWeights{Low: 1, Medium: math.NaN(), High: 4}.Validate())
	assert.Equal(t, 2.0, DefaultClassWeights().Of("bogus"))
}
