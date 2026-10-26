package nsysrep

import (
	"encoding/binary"
	"math"
	"slices"
)

// A protobuf writer for synthetic reports.
type pb []byte

func (p pb) tag(num, typ int) pb { return binary.AppendUvarint(p, uint64(num<<3|typ)) }
func (p pb) u(num int, v uint64) pb {
	return binary.AppendUvarint(p.tag(num, wireVarint), v)
}
func (p pb) i(num int, v int64) pb { return p.u(num, uint64(v)) }
func (p pb) b(num int, b []byte) pb {
	p = binary.AppendUvarint(p.tag(num, wireBytes), uint64(len(b)))
	return append(p, b...)
}
func (p pb) s(num int, s string) pb { return p.b(num, []byte(s)) }
func (p pb) f64(num int, v float64) pb {
	return binary.LittleEndian.AppendUint64(p.tag(num, wireFixed64), math.Float64bits(v))
}

// lz4Literal encodes src as an LZ4 block of literals only, which is valid
// LZ4 that any decoder reads.
func lz4Literal(src []byte) []byte {
	n := len(src)
	var out []byte
	if n < 15 {
		out = append(out, byte(n<<4))
	} else {
		out = append(out, 0xf0)
		for n -= 15; n >= 255; n -= 255 {
			out = append(out, 255)
		}
		out = append(out, byte(n))
	}
	return append(out, src...)
}

// compressedFrame frames msg as the report stores most frames.
func compressedFrame(msg []byte) []byte {
	raw := binary.AppendUvarint(nil, uint64(len(msg)))
	raw = append(raw, msg...)
	block := lz4Literal(raw)
	out := binary.AppendUvarint(nil, uint64(len(block)))
	out = binary.AppendUvarint(out, uint64(len(raw)))
	return append(out, block...)
}

func plainFrame(msg []byte) []byte {
	return append(binary.AppendUvarint(nil, uint64(len(msg))), msg...)
}

func event(typ int, payload int, body pb) pb { return pb(nil).u(1, uint64(typ)).b(payload, body) }

// Synthetic builds a small report: the given header version, a few events
// of each kind, and the session's start and host if given. Tests make
// reports with it (no report written by Nsight Systems is in the
// repository).
func Synthetic(version string, sess Session) []byte {
	gpu := func(start, end, corr int64, kind int, sub pb) pb {
		return pb(nil).i(1, start).i(2, end).i(3, corr).u(4, 1).u(5, 1).u(6, 7).u(7, 3).i(8, 0x10<<24).u(9, 0).b(kind, sub)
	}
	batch := pb(nil).
		b(1, event(79, payloadGPU, gpu(100, 200, 5, gpuKernel, pb(nil).u(1, 2).u(2, 1).u(3, 3).u(10, 64).u(11, 1).u(12, 1).u(13, 128).u(14, 1).u(15, 1).u(16, 0).u(17, 48).u(21, 32)))).
		b(1, event(142, payloadGPU, gpu(210, 220, 6, gpuMemcpy, pb(nil).u(1, 4096).u(2, 1).u(4, 1).u(5, 2)))).
		b(1, event(80, payloadGPU, gpu(230, 240, 7, gpuMemset, pb(nil).u(1, 512).u(2, 9).u(4, 2)))).
		b(1, event(106, payloadGPU, gpu(250, 260, 8, gpuSync, pb(nil).u(1, 11).u(4, 3)))).
		b(1, event(48, payloadAPI, pb(nil).i(1, 90).i(2, 95).i(3, 5).u(4, 1).u(5, 1).u(6, 0).u(7, 4).u(8, 0).i(13, 0x10<<24|0x11))).
		b(1, event(48, payloadAPI, pb(nil).i(1, 96).i(2, 97).i(3, 0).u(6, 103).u(7, 4).i(13, 0x10<<24|0x11))). // overhead: skipped
		b(1, event(59, payloadNVTX, pb(nil).u(1, 59).i(2, -50).i(10, 300).s(7, "range").i(9, 0x10<<24|0x11).u(13, 0))).
		b(1, event(34, payloadNVTX, pb(nil).u(1, 34).i(2, 120).u(12, 5).i(9, 0x10<<24|0x11))).
		b(1, event(27, payloadProcess, pb(nil).u(1, 1).i(2, 0x10<<24).s(5, "short").s(6, "proc")))
	strs := pb(nil)
	for _, s := range []string{"", "kern", "kern(int)", "_Z4kerni", "cudaLaunchKernel", "mark"} {
		strs = strs.s(1, s)
	}
	strs = strs.b(2, pb(nil).u(1, 1).u(2, 2).u(3, 3))
	threads := pb(nil).b(1, pb(nil).u(2, 4).u(3, 0).i(4, 0x10<<24|0x11))
	target := pb(nil).b(1, pb(nil).u(1, 1).b(2, pb(nil).b(7, pb(nil).u(1, 1).s(2, "Some GPU").u(9, 1)))).b(15, pb(nil).u(1, 7))

	// Frames: a plain metadata one, the batch, the strings, the threads,
	// the target info.
	var raw []byte
	raw = append(raw, rawMagic...)
	meta := pb(nil).s(1, "meta").u(15, 1)
	if sess.Host != "" {
		meta = meta.b(2, pb(nil).s(9, sess.Host))
	}
	if sess.MonoStart != 0 {
		meta = meta.i(17, sess.MonoStart).i(19, sess.MonoStart)
	}
	raw = append(raw, plainFrame(meta)...)
	raw = append(raw, compressedFrame(batch)...)
	strOff := len(raw) - len(rawMagic)
	raw = append(raw, compressedFrame(strs)...)
	raw = append(raw, plainFrame(threads)...)
	raw = append(raw, plainFrame(target)...)

	head := []byte(headerPrefix + version + "v0.")
	file := slices.Clone(head)
	type sec struct {
		name string
		data []byte
	}
	var toc pb
	// The time conversion data: wall-clock samples against a counter, and,
	// for a session with a wall-clock start, the reading of the wall clock
	// and the monotonic clock taken together a little before it.
	sample := func(a, b int64, rate float64) pb {
		return pb(nil).b(3, pb(nil).b(1, pb(nil).i(1, a).i(2, b)).f64(2, rate))
	}
	tc := pb(nil).b(2, pb(nil).b(1, sample(1_792_411_100_000_000_000, 3_000_000_000_000_000, 2.1)))
	if sess.WallStart != 0 && sess.MonoStart != 0 {
		const before = 129_123_456
		tc = tc.b(2, pb(nil).b(1, sample(sess.WallStart-before, sess.MonoStart-before, 1)))
	}
	for _, s := range []sec{
		{"Analysis Options Data", pb(nil).s(1, "x")},
		{sectionTime, tc},
		{sectionRaw, raw},
		{sectionOffsets, pb(nil).s(1, "lz4").u(2, uint64(strOff)).u(3, 0)},
	} {
		toc = toc.b(1, pb(nil).s(1, s.name).u(2, uint64(len(file))).u(3, uint64(len(s.data))))
		file = append(file, s.data...)
	}
	tocAt := len(file)
	file = append(file, toc...)
	file = binary.LittleEndian.AppendUint64(file, uint64(tocAt))
	return append(file, trailer...)
}
