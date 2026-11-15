package worker

import (
	"sort"

	"github.com/deweysasser/whisper-network/internal/models"
)

// SlotInput is what the slot formula of DESIGN.md §7.3 needs.
type SlotInput struct {
	UsableCores    int
	ThreadsPerSlot int
	// Budget is the memory available to children: limit − reserve.
	Budget int64
	// LargestPeak is the peak RSS of the largest model offered; 0 if no
	// model is offered (prepare-only worker).
	LargestPeak int64
	// PrepareMemory is a prepare task's reservation, used when no model is
	// offered.
	PrepareMemory int64
}

// SlotCount applies DESIGN.md §7.3:
//
//	slots_by_cpu = max(1, floor(usable_cores / threads_per_slot))
//	slots_by_mem = floor(budget / peak_rss(largest model offered))
//	slots        = min(slots_by_cpu, slots_by_mem)
//
// A prepare-only worker sizes memory by the prepare reservation. The result
// is 0 when not even one task fits in memory.
func SlotCount(in SlotInput) int {
	byCPU := 1
	if in.ThreadsPerSlot > 0 {
		byCPU = max(1, in.UsableCores/in.ThreadsPerSlot)
	}
	per := in.LargestPeak
	if per <= 0 {
		per = in.PrepareMemory
	}
	if per <= 0 || in.Budget <= 0 {
		return 0
	}
	byMem := int(in.Budget / per)
	return min(byCPU, byMem)
}

// modelsThatFit returns the names of the whisper models whose peak RSS fits
// in budget, smallest first, and the largest such peak.
func modelsThatFit(cat Catalog, budget int64) (names []string, largest int64) {
	ms := cat.Whisper()
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].PeakRSS < ms[j].PeakRSS })
	for _, m := range ms {
		if fits(m, budget) {
			names = append(names, m.Name)
			largest = max(largest, m.PeakRSS)
		}
	}
	return names, largest
}

func fits(m models.Model, free int64) bool { return m.PeakRSS > 0 && m.PeakRSS <= free }
