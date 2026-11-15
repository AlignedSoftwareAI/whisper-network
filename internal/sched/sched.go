// Package sched implements the two-level weighted fair queueing used to pick
// which task a worker receives (DESIGN.md §8.2).
//
// It is start-time fair queueing over virtual time. Every queue q has a virtual
// time V_q and every (queue, class) pair has V_qc. A claim picks the candidate
// queue with the smallest V_q, then the candidate class within it with the
// smallest V_qc, and charges the task's cost: V_q += cost/W_q and
// V_qc += cost/w_c.
//
// The package is pure: no I/O and no clock. The store loads a [State] inside
// its claim transaction, calls [State.Pick] and [State.Charge] (or
// [State.Reactivate] when work appears), and persists the result.
package sched

import (
	"fmt"
	"math"
	"slices"

	"github.com/deweysasser/whisper-network/internal/api"
)

// Classes lists the job weight classes in tie-break order: when two classes
// of one queue have equal virtual time, the earlier one in this list wins.
var Classes = [...]api.Weight{api.WeightHigh, api.WeightMedium, api.WeightLow}

// ClassWeights are the relative shares w_c of the job weight classes within a
// queue.
type ClassWeights struct {
	Low, Medium, High float64
}

// DefaultClassWeights returns the default 1/2/4 class weights.
func DefaultClassWeights() ClassWeights {
	return ClassWeights{Low: 1, Medium: 2, High: 4}
}

// Of returns the weight of class c. Unknown classes get the medium weight.
func (w ClassWeights) Of(c api.Weight) float64 {
	switch c {
	case api.WeightLow:
		return w.Low
	case api.WeightHigh:
		return w.High
	default:
		return w.Medium
	}
}

// Validate reports an error unless every class weight is positive and finite.
func (w ClassWeights) Validate() error {
	for _, c := range Classes {
		if v := w.Of(c); !(v > 0) || math.IsInf(v, 0) {
			return fmt.Errorf("class weight %s must be positive, got %v", c, v)
		}
	}
	return nil
}

// Key identifies a virtual clock. Class "" is the queue-level clock V_q;
// a non-empty Class is the class clock V_qc.
type Key struct {
	Queue int64
	Class api.Weight
}

// QueueKey returns the key of queue q's queue-level clock.
func QueueKey(q int64) Key { return Key{Queue: q} }

// queueLevel is the Last key that holds the last charged queue-level V.
// Queue ids are positive, so 0 never collides with a real queue.
const queueLevel int64 = 0

// State is a snapshot of the scheduler's virtual times. The zero value is
// ready to use; missing clocks read as 0.
type State struct {
	// V holds V_q (Key.Class == "") and V_qc (Key.Class != "").
	V map[Key]float64
	// Last holds the most recently charged virtual time, used by reactivation
	// when no peer is backlogged. Last[0] is the queue level (across queues);
	// Last[q] is the class level within queue q.
	Last map[int64]float64
}

// Get returns the virtual time of clock k (0 if unset).
func (s *State) Get(k Key) float64 { return s.V[k] }

func (s *State) set(k Key, v float64) {
	if s.V == nil {
		s.V = make(map[Key]float64)
	}
	s.V[k] = v
}

func (s *State) setLast(scope int64, v float64) {
	if s.Last == nil {
		s.Last = make(map[int64]float64)
	}
	s.Last[scope] = v
}

// Pick chooses the (queue, class) pair to serve next from candidates, each of
// which names a pair with a pending task the worker may take.
//
// The queue with the smallest V_q wins; equal V_q is broken by the smaller
// queue id. Within that queue the class with the smallest V_qc wins; equal
// V_qc is broken by class order high > medium > low. It reports false if
// candidates is empty. Pick does not modify s.
func (s *State) Pick(candidates []Key) (Key, bool) {
	if len(candidates) == 0 {
		return Key{}, false
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if s.less(c, best) {
			best = c
		}
	}
	return best, true
}

// less orders candidates by (V_q, queue id, V_qc, class rank).
func (s *State) less(a, b Key) bool {
	if va, vb := s.Get(QueueKey(a.Queue)), s.Get(QueueKey(b.Queue)); va != vb {
		return va < vb
	}
	if a.Queue != b.Queue {
		return a.Queue < b.Queue
	}
	if va, vb := s.Get(a), s.Get(b); va != vb {
		return va < vb
	}
	return classRank(a.Class) < classRank(b.Class)
}

func classRank(c api.Weight) int {
	if i := slices.Index(Classes[:], c); i >= 0 {
		return i
	}
	return len(Classes)
}

// Charge records that a task of the given cost was served from k:
// V_q += cost/queueWeight and V_qc += cost/w_c. It also records the charged
// values for later reactivations. Non-positive weights are treated as 1 so a
// misconfiguration cannot produce infinite virtual times.
func (s *State) Charge(k Key, cost, queueWeight float64, cw ClassWeights) {
	if queueWeight <= 0 {
		queueWeight = 1
	}
	wc := cw.Of(k.Class)
	if wc <= 0 {
		wc = 1
	}
	vq := s.Get(QueueKey(k.Queue)) + cost/queueWeight
	vc := s.Get(k) + cost/wc
	s.set(QueueKey(k.Queue), vq)
	s.set(k, vc)
	s.setLast(queueLevel, vq)
	s.setLast(k.Queue, vc)
}

// Reactivate applies the reactivation rule when pending work is about to be
// added to k. backlogged lists the (queue, class) pairs that have pending work
// before the addition.
//
// If queue k.Queue has no entry in backlogged it is reactivating, and
// V_q = max(V_q, V_min), where V_min is the smallest V_q among backlogged
// queues, or the last charged queue-level V if there are none. The same rule
// applies to class k.Class among the backlogged classes of k.Queue. This keeps
// an idle queue (or class) from banking credit while it had nothing to run.
func (s *State) Reactivate(k Key, backlogged []Key) {
	queueBusy, classBusy := false, false
	minQ, minC := math.Inf(1), math.Inf(1)
	for _, b := range backlogged {
		if b.Queue == k.Queue {
			queueBusy = true
			if b.Class == k.Class {
				classBusy = true
			} else {
				minC = min(minC, s.Get(b))
			}
			continue
		}
		minQ = min(minQ, s.Get(QueueKey(b.Queue)))
	}
	if !queueBusy {
		if math.IsInf(minQ, 1) {
			minQ = s.Last[queueLevel]
		}
		qk := QueueKey(k.Queue)
		s.set(qk, max(s.Get(qk), minQ))
	}
	if !classBusy {
		if math.IsInf(minC, 1) {
			minC = s.Last[k.Queue]
		}
		s.set(k, max(s.Get(k), minC))
	}
}
