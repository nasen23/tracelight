package trace

import (
	"slices"
	"strings"
)

// Collective kernels: the NCCL kernels of collective operations, which
// every rank of a group runs and which end together on all of them (each
// waits for the others' data). Point-to-point kernels (send/receive) are
// left out: they pair two ranks, not the group. Their ends line the ranks
// of a job up to microseconds, where the hosts' wall clocks agree to
// milliseconds at best.

// collectiveOps are the operations, in the order names are tested: "reduce"
// last, as "allreduce" and "reducescatter" contain it.
var collectiveOps = []string{"allreduce", "allgather", "reducescatter", "broadcast", "alltoall", "reduce"}

// CollectiveOp is the collective operation of a kernel's name ("allreduce",
// "allgather", ...), or "" if it is not a collective kernel.
func CollectiveOp(name string) string {
	l := strings.ToLower(name)
	if !strings.Contains(l, "nccl") || strings.Contains(l, "sendrecv") {
		return ""
	}
	for _, op := range collectiveOps {
		if strings.Contains(l, op) {
			return op
		}
	}
	return ""
}

// Collectives returns the ends of the trace's collective kernels by
// operation, in nanoseconds from its first event (Start), sorted.
func (t *Trace) Collectives() map[string][]int64 {
	ops := map[int32]string{}
	out := map[string][]int64{}
	for _, tk := range t.Tracks {
		if tk.Kind != TrackStream {
			continue
		}
		for _, l := range tk.Lanes {
			for i := range l.Events {
				e := &l.Events[i]
				if e.Kind != KindKernel {
					continue
				}
				op, ok := ops[e.Name]
				if !ok {
					op = CollectiveOp(t.Name(e))
					ops[e.Name] = op
				}
				if op != "" {
					out[op] = append(out[op], e.End-t.Start)
				}
			}
		}
	}
	for _, v := range out {
		slices.Sort(v)
	}
	return out
}
