package trace

// sortOrder returns the order of events by start, and longer first among
// events that start together, keeping the input order of full ties: the
// index of the first event, then the second, and so on.
//
// It is a stable LSD radix sort of an index array, by end (descending) and
// then by start: a few linear passes, several times faster than a
// comparison sort of the 32-byte events for the hundreds of thousands of
// NVTX ranges that exports list in the order they ended.
func sortOrder(events []Event) []int32 {
	n := len(events)
	if n == 0 {
		return nil
	}
	minStart, maxEnd := events[0].Start, events[0].End
	maxStart, minEnd := minStart, maxEnd
	for i := range events {
		e := &events[i]
		minStart, maxStart = min(minStart, e.Start), max(maxStart, e.Start)
		minEnd, maxEnd = min(minEnd, e.End), max(maxEnd, e.End)
	}
	idx := make([]int32, n)
	tmp := make([]int32, n)
	for i := range idx {
		idx[i] = int32(i)
	}
	// Least significant key first: end, descending.
	idx, tmp = radixPasses(idx, tmp, uint64(maxEnd-minEnd), func(i int32) uint64 { return uint64(maxEnd - events[i].End) })
	idx, _ = radixPasses(idx, tmp, uint64(maxStart-minStart), func(i int32) uint64 { return uint64(events[i].Start - minStart) })
	return idx
}

// sortEvents returns events sorted as sortOrder orders them, in a new slice.
func sortEvents(events []Event) []Event {
	out := make([]Event, len(events))
	for i, j := range sortOrder(events) {
		out[i] = events[j]
	}
	return out
}

const radixBits = 11

// radixPasses stably sorts idx by key, whose values are at most maxKey,
// using tmp as scratch; it returns the sorted slice and the other one.
func radixPasses(idx, tmp []int32, maxKey uint64, key func(int32) uint64) ([]int32, []int32) {
	var count [1 << radixBits]int
	for shift := uint(0); shift < 64 && maxKey>>shift != 0; shift += radixBits {
		clear(count[:])
		for _, i := range idx {
			count[(key(i)>>shift)&(1<<radixBits-1)]++
		}
		sum := 0
		for d, c := range count {
			count[d] = sum
			sum += c
		}
		for _, i := range idx {
			d := (key(i) >> shift) & (1<<radixBits - 1)
			tmp[count[d]] = i
			count[d]++
		}
		idx, tmp = tmp, idx
	}
	return idx, tmp
}
