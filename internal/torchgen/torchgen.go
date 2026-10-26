// Package torchgen writes synthetic traces of PyTorch's profiler: the
// Chrome-trace JSON Kineto writes (torch.profiler with record_shapes and
// with_stack, export_chrome_trace or tensorboard_trace_handler), for the
// ranks of a data-parallel training job. They are made to look like real
// ones in structure: ProfilerStep#N spans, Python stacks from the script
// through nn.Module down to the layers, aten:: operators that launch CUDA
// kernels (by correlation id) running asynchronously on a stream, the
// backward pass on autograd's own thread (no Python stack there), DDP's
// NCCL all-reduce buckets on another stream, overlapping it and ending at
// the same moment on every rank, an .item() that waits for the GPU, and
// each rank's wall clock off by a millisecond or so. Deterministic.
package torchgen

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// Options say what to write.
type Options struct {
	Ranks  int // ranks of the job (1 if 0)
	Steps  int // profiled training steps (8 if 0)
	Layers int // transformer blocks (6 if 0)
	// NoStack leaves out the Python calls, as without with_stack.
	NoStack bool
	// Relative writes times from baseTimeNanoseconds, as some Kineto
	// versions do, instead of Unix epoch microseconds.
	Relative bool
}

// epoch is when the synthetic job ran: 2026-10-20 08:00 UTC, in ns.
const epoch = 1_792_483_200_000_000_000

// WriteDir writes rank<N>.pt.trace.json (gzipped as .json.gz with gz) for
// each rank into dir, and returns their paths.
func WriteDir(dir string, o Options, gz bool) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	o = o.defaults()
	ranks := newJob(o)
	var paths []string
	for _, r := range ranks {
		name := fmt.Sprintf("rank%d.pt.trace.json", r.rank)
		if gz {
			name += ".gz"
		}
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			return nil, err
		}
		var w io.Writer = f
		var zw *gzip.Writer
		if gz {
			zw = gzip.NewWriter(f)
			w = zw
		}
		err = r.write(w, o)
		if zw != nil {
			zw.Close()
		}
		f.Close()
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// WriteMerged writes the ranks into one file, as tools that merge the
// ranks' traces do: each rank's events after the others' (the file's first
// event is not its earliest), their processes named "rank<N>:<pid>", no
// distributedInfo or deviceProperties; gzipped with gz.
func WriteMerged(path string, o Options, gz bool) error {
	o = o.defaults()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var w io.Writer = f
	if gz {
		zw := gzip.NewWriter(f)
		defer zw.Close()
		w = zw
	}
	bw := bufio.NewWriterSize(w, 1<<20)
	bw.WriteString("{\n  \"traceEvents\": [\n")
	ranks := newJob(o)
	// The last rank first: its first event is later than the others'.
	for i := len(ranks) - 1; i >= 0; i-- {
		ranks[i].writeEvents(bw, o, fmt.Sprintf("rank%d:", ranks[i].rank), 0)
	}
	bw.WriteString(`  {"name": "Record Window End", "ph": "i", "s": "g", "pid": "", "tid": "", "ts": 0.0}` + "\n  ],\n  \"displayTimeUnit\": \"ms\"\n}\n")
	return bw.Flush()
}

func (o Options) defaults() Options {
	if o.Ranks <= 0 {
		o.Ranks = 1
	}
	if o.Steps <= 0 {
		o.Steps = 8
	}
	if o.Layers <= 0 {
		o.Layers = 6
	}
	return o
}

// event is a span of a trace, times in ns of the job's true clock.
type event struct {
	cat, name   string
	pid, tid    int64
	start, end  int64
	corr        int64
	stream      int64
	grid, block int
	bytes       int64
	pyID, pyPar int64
	phase       byte // 'X', or 's'/'f' for flows
}

// rank is the state of one rank as the job runs.
type rank struct {
	rank     int
	rng      *rand.Rand
	pid      int64
	main, bw int64 // the main thread and autograd's
	skew     int64 // its wall clock minus the true one
	cpu      int64 // where the main thread is
	bwCPU    int64 // where autograd's thread is
	free     map[int64]int64
	corr     int64
	pyID     int64
	stack    []int64 // open Python calls: indexes in events
	events   []event
}

const (
	computeStream = 7
	commStream    = 22
)

func newJob(o Options) []*rank {
	var ranks []*rank
	for r := range o.Ranks {
		pid := int64(41200 + 17*r)
		rk := &rank{rank: r, rng: rand.New(rand.NewPCG(uint64(r)+1, 99)), pid: pid, main: pid, bw: pid + 9,
			skew: int64((r*7919)%3000-1500) * 1000, free: map[int64]int64{}, corr: int64(1 << 20 * (r + 1)), pyID: 1}
		rk.cpu = epoch + int64(r)*137_000 // the ranks reach the profiler a little apart
		ranks = append(ranks, rk)
	}
	// The script's outer frames, open over every step.
	for _, rk := range ranks {
		if !o.NoStack {
			for _, f := range []string{"runpy.py(198): _run_module_as_main", "runpy.py(88): _run_code", "train.py(171): <module>", "train.py(152): main", "train.py(109): train"} {
				rk.push(f)
				rk.cpu += 2_000
			}
		}
	}
	for step := range o.Steps {
		for _, rk := range ranks {
			rk.stepStart(step, o)
		}
		for _, rk := range ranks {
			rk.forward(o)
		}
		// Backward on autograd's thread, the gradients' all-reduce in
		// buckets as the layers finish (DDP), every rank in lockstep at
		// each bucket.
		buckets := 3
		per := (o.Layers + buckets - 1) / buckets
		for _, rk := range ranks {
			rk.backwardStart(o)
		}
		for b := range buckets {
			for _, rk := range ranks {
				for l := o.Layers - 1 - b*per; l >= max(0, o.Layers-(b+1)*per); l-- {
					rk.backwardLayer(l)
				}
			}
			allReduce(ranks, b)
		}
		for _, rk := range ranks {
			rk.backwardEnd(o)
			rk.optimizer(o)
			rk.stepEnd(o)
		}
	}
	for _, rk := range ranks {
		for len(rk.stack) > 0 {
			rk.cpu += 3_000
			rk.pop()
		}
		end := rk.cpu + 50_000
		rk.events = append(rk.events, event{cat: "Trace", name: "PyTorch Profiler (0)", pid: -1, tid: -1, start: epoch - 10_000, end: end, phase: 'X'})
	}
	return ranks
}

func (r *rank) jitter(ns int64) int64 { return ns + int64(r.rng.NormFloat64()*float64(ns)*0.08) }

// push opens a Python call on the main thread.
func (r *rank) push(name string) {
	ev := event{cat: "python_function", name: name, pid: r.pid, tid: r.main, start: r.cpu, pyID: r.pyID, phase: 'X'}
	if len(r.stack) > 0 {
		ev.pyPar = r.events[r.stack[len(r.stack)-1]].pyID
	}
	r.pyID++
	r.events = append(r.events, ev)
	r.stack = append(r.stack, int64(len(r.events)-1))
	r.cpu += 600
}

func (r *rank) pop() {
	i := r.stack[len(r.stack)-1]
	r.stack = r.stack[:len(r.stack)-1]
	r.cpu += 400
	r.events[i].end = r.cpu
}

// py runs body inside Python calls (none without stacks).
func (r *rank) py(o Options, frames []string, body func()) {
	if o.NoStack {
		body()
		return
	}
	for _, f := range frames {
		r.push(f)
	}
	body()
	for range frames {
		r.pop()
	}
}

// op runs an aten operator on thread tid at *at, inside which body runs.
func (r *rank) op(tid int64, at *int64, name string, body func()) {
	i := len(r.events)
	r.events = append(r.events, event{cat: "cpu_op", name: name, pid: r.pid, tid: tid, start: *at, phase: 'X'})
	*at += r.jitter(3_000)
	if body != nil {
		body()
	}
	*at += r.jitter(2_000)
	r.events[i].end = *at
}

// launch launches a kernel of dur ns on stream from thread tid at *at.
func (r *rank) launch(tid int64, at *int64, stream int64, kernel string, dur int64, grid, block int) (start, end int64) {
	r.corr++
	c := r.corr
	call := event{cat: "cuda_runtime", name: "cudaLaunchKernel", pid: r.pid, tid: tid, start: *at, corr: c, phase: 'X'}
	*at += r.jitter(5_500)
	call.end = *at
	start = max(call.end+r.jitter(4_000), r.free[stream])
	end = start + r.jitter(dur)
	r.free[stream] = end
	r.events = append(r.events, call,
		event{cat: "kernel", name: kernel, pid: 0, tid: stream, start: start, end: end, corr: c, stream: stream, grid: grid, block: block, phase: 'X'},
		event{cat: "ac2g", name: "ac2g", pid: r.pid, tid: tid, start: call.start, corr: c, phase: 's'},
		event{cat: "ac2g", name: "ac2g", pid: 0, tid: stream, start: start, corr: c, phase: 'f'})
	return
}

// memcpy copies bytes on the compute stream, launched from the main thread.
func (r *rank) memcpy(name string, bytes int64) {
	r.corr++
	c := r.corr
	call := event{cat: "cuda_runtime", name: "cudaMemcpyAsync", pid: r.pid, tid: r.main, start: r.cpu, corr: c, phase: 'X'}
	r.cpu += r.jitter(7_000)
	call.end = r.cpu
	start := max(call.end+3_000, r.free[computeStream])
	end := start + bytes/24 // 24 GB/s
	r.free[computeStream] = end
	r.events = append(r.events, call, event{cat: "gpu_memcpy", name: name, pid: 0, tid: computeStream, start: start, end: end, corr: c, stream: computeStream, bytes: bytes, phase: 'X'})
}

func (r *rank) stepStart(step int, o Options) {
	r.cpu += r.jitter(40_000)
	r.events = append(r.events, event{cat: "user_annotation", name: "ProfilerStep#" + strconv.Itoa(step+2), pid: r.pid, tid: r.main, start: r.cpu, phase: 'X'})
	r.push2(o, "train.py(64): train_step")
	// The batch: the loader, then to the GPU.
	r.py(o, []string{"torch/utils/data/dataloader.py(708): __next__", "torch/utils/data/dataloader.py(1458): _next_data"}, func() {
		r.cpu += r.jitter(180_000)
	})
	r.py(o, []string{"train.py(71): <listcomp>"}, func() {
		for _, sz := range []int64{12 << 20, 64 << 10} {
			r.op(r.main, &r.cpu, "aten::to", func() {
				r.op(r.main, &r.cpu, "aten::_to_copy", func() {
					r.op(r.main, &r.cpu, "aten::copy_", func() { r.memcpy("Memcpy HtoD (Pinned -> Device)", sz) })
				})
			})
		}
	})
}

// push2 opens a frame that stays open until stepEnd (the step's own).
func (r *rank) push2(o Options, name string) {
	if !o.NoStack {
		r.push(name)
	}
}

var module = []string{"torch/nn/modules/module.py(1739): _wrapped_call_impl", "torch/nn/modules/module.py(1750): _call_impl"}

func (r *rank) linear(o Options, at *int64, k string, dur int64) {
	r.py(o, append(module, "torch/nn/modules/linear.py(125): forward"), func() {
		r.op(r.main, at, "aten::linear", func() {
			r.op(r.main, at, "aten::t", nil)
			r.op(r.main, at, "aten::addmm", func() { r.launch(r.main, at, computeStream, k, dur, 264, 384) })
		})
	})
}

func (r *rank) forward(o Options) {
	gemm := "sm90_xmma_gemm_bf16bf16_bf16f32_f32_tn_n_tilesize128x256x64_warpgroupsize2x1x1_execute_segment_k_off_kernel__5x_cublas"
	r.py(o, []string{module[0], module[1], "model.py(96): forward"}, func() {
		r.py(o, append(module, "torch/nn/modules/sparse.py(190): forward", "torch/nn/functional.py(2551): embedding"), func() {
			r.op(r.main, &r.cpu, "aten::embedding", func() {
				r.launch(r.main, &r.cpu, computeStream, "void at::native::(anonymous namespace)::indexSelectLargeIndex<c10::BFloat16, long, unsigned int, 2, 2, -2, true>", 38_000, 4096, 128)
			})
		})
		for l := range o.Layers {
			r.py(o, append(module, fmt.Sprintf("model.py(%d): forward", 61)), func() {
				r.py(o, append(module, "torch/nn/modules/normalization.py(217): forward", "torch/nn/functional.py(2910): layer_norm"), func() {
					r.op(r.main, &r.cpu, "aten::layer_norm", func() {
						r.op(r.main, &r.cpu, "aten::native_layer_norm", func() {
							r.launch(r.main, &r.cpu, computeStream, "void at::native::(anonymous namespace)::vectorized_layer_norm_kernel<c10::BFloat16, float>", 21_000, 8192, 256)
						})
					})
				})
				r.py(o, append(module, "model.py(39): forward"), func() {
					r.linear(o, &r.cpu, gemm, 240_000)
					r.py(o, []string{"torch/nn/functional.py(5865): scaled_dot_product_attention"}, func() {
						r.op(r.main, &r.cpu, "aten::scaled_dot_product_attention", func() {
							r.op(r.main, &r.cpu, "aten::_scaled_dot_product_flash_attention", func() {
								r.op(r.main, &r.cpu, "aten::_flash_attention_forward", func() {
									r.launch(r.main, &r.cpu, computeStream, "void pytorch_flash::flash_fwd_kernel<Flash_fwd_kernel_traits<128, 128, 64, 4, false, false, cutlass::bfloat16_t, Flash_kernel_traits<128, 128, 64, 4, cutlass::bfloat16_t> >, false, true, false, false, true, true, false, false>(pytorch_flash::Flash_fwd_params)", 410_000, 512, 128)
								})
							})
						})
					})
					r.linear(o, &r.cpu, gemm, 90_000)
				})
				r.py(o, append(module, "model.py(52): forward"), func() {
					r.linear(o, &r.cpu, gemm, 320_000)
					r.py(o, []string{"torch/nn/functional.py(1695): gelu"}, func() {
						r.op(r.main, &r.cpu, "aten::gelu", func() {
							r.launch(r.main, &r.cpu, computeStream, "void at::native::vectorized_elementwise_kernel<4, at::native::GeluCUDAKernelImpl(at::TensorIteratorBase&, at::native::GeluType)::{lambda()#2}::operator()() const::{lambda()#2}::operator()() const::{lambda(c10::BFloat16)#1}, at::detail::Array<char*, 2> >(int, at::native::GeluCUDAKernelImpl(at::TensorIteratorBase&, at::native::GeluType)::{lambda()#2}::operator()() const::{lambda()#2}::operator()() const::{lambda(c10::BFloat16)#1}, at::detail::Array<char*, 2>)", 46_000, 16384, 128)
						})
					})
					r.linear(o, &r.cpu, gemm, 320_000)
				})
				r.op(r.main, &r.cpu, "aten::add", func() {
					r.launch(r.main, &r.cpu, computeStream, "void at::native::vectorized_elementwise_kernel<4, at::native::CUDAFunctor_add<c10::BFloat16>, at::detail::Array<char*, 3> >(int, at::native::CUDAFunctor_add<c10::BFloat16>, at::detail::Array<char*, 3>)", 18_000, 8192, 128)
				})
				_ = l
			})
		}
	})
	r.py(o, []string{"torch/nn/functional.py(3494): cross_entropy"}, func() {
		r.op(r.main, &r.cpu, "aten::cross_entropy_loss", func() {
			r.op(r.main, &r.cpu, "aten::log_softmax", func() {
				r.launch(r.main, &r.cpu, computeStream, "void at::native::(anonymous namespace)::cunn_SoftMaxForward<4, c10::BFloat16, float, float, at::native::(anonymous namespace)::LogSoftMaxForwardEpilogue>(float*, c10::BFloat16 const*, int)", 95_000, 8192, 1024)
			})
			r.op(r.main, &r.cpu, "aten::nll_loss_nd", func() {
				r.launch(r.main, &r.cpu, computeStream, "void at::native::(anonymous namespace)::nll_loss_forward_reduce_cuda_kernel_2d<float, float, long>(float*, float*, float const*, long const*, float const*, bool, long, long, long, long)", 9_000, 1, 1024)
			})
		})
	})
}

func (r *rank) backwardStart(o Options) {
	r.py(o, []string{"torch/_tensor.py(626): backward", "torch/autograd/__init__.py(347): backward"}, func() {
		r.push2(o, "torch/autograd/graph.py(823): _engine_run_backward")
		r.i(o)
	})
	r.bwCPU = max(r.bwCPU, r.cpu+20_000)
}

// i: the backward call opens; the main thread waits in it until backwardEnd.
func (r *rank) i(o Options) {
	r.op(r.main, &r.cpu, "aten::ones_like", nil)
}

func (r *rank) backwardLayer(l int) {
	gemm := "sm90_xmma_gemm_bf16bf16_bf16f32_f32_nt_n_tilesize128x256x64_warpgroupsize2x1x1_execute_segment_k_off_kernel__5x_cublas"
	for _, fn := range []string{"AddmmBackward0", "GeluBackward0", "AddmmBackward0", "AddmmBackward0", "ScaledDotProductFlashAttentionBackward0", "AddmmBackward0", "NativeLayerNormBackward0"} {
		r.op(r.bw, &r.bwCPU, "autograd::engine::evaluate_function: "+fn, func() {
			r.op(r.bw, &r.bwCPU, fn, func() {
				switch fn {
				case "AddmmBackward0":
					r.op(r.bw, &r.bwCPU, "aten::mm", func() { r.launch(r.bw, &r.bwCPU, computeStream, gemm, 300_000, 264, 384) })
					r.op(r.bw, &r.bwCPU, "aten::mm", func() { r.launch(r.bw, &r.bwCPU, computeStream, gemm, 280_000, 264, 384) })
				case "ScaledDotProductFlashAttentionBackward0":
					r.op(r.bw, &r.bwCPU, "aten::_flash_attention_backward", func() {
						r.launch(r.bw, &r.bwCPU, computeStream, "void pytorch_flash::flash_bwd_dq_dk_dv_loop_seqk_parallel_kernel<Flash_bwd_kernel_traits<128, 64, 128, 8, 2, 4, 2, false, false, cutlass::bfloat16_t, Flash_kernel_traits<128, 64, 128, 8, cutlass::bfloat16_t> >, true, false, false, false, false, true, false>(pytorch_flash::Flash_bwd_params)", 980_000, 1024, 256)
					})
				default:
					r.launch(r.bw, &r.bwCPU, computeStream, "void at::native::vectorized_elementwise_kernel<4, at::native::"+fn+"Functor<c10::BFloat16>, at::detail::Array<char*, 3> >(int, at::native::"+fn+"Functor<c10::BFloat16>, at::detail::Array<char*, 3>)", 52_000, 8192, 128)
				}
			})
		})
	}
	_ = l
}

// allReduce runs bucket b's all-reduce on every rank: launched when its
// gradients are ready, it ends on all of them together.
func allReduce(ranks []*rank, b int) {
	var ready int64
	for _, rk := range ranks {
		ready = max(ready, max(rk.free[computeStream], rk.free[commStream])+30_000)
	}
	dur := int64(900_000 + 250_000*b)
	if len(ranks) == 1 {
		dur = 60_000 // nothing to wait for
	}
	end := ready + dur
	for _, rk := range ranks {
		rk.op(rk.bw, &rk.bwCPU, "nccl:all_reduce", func() {
			rk.op(rk.bw, &rk.bwCPU, "record_param_comms", func() {
				start, _ := rk.launch(rk.bw, &rk.bwCPU, commStream, "ncclDevKernel_AllReduce_Sum_bf16_RING_LL(ncclDevComm*, unsigned long, ncclWork*)", 1, 32, 640)
				// The kernel waits for every rank, then reduces: its end is
				// the job's, the same on all of them.
				for i := len(rk.events) - 1; i >= 0; i-- {
					if rk.events[i].cat == "kernel" && rk.events[i].start == start {
						rk.events[i].end = end
						break
					}
				}
				rk.free[commStream] = end
			})
		})
	}
}

func (r *rank) backwardEnd(o Options) {
	// The main thread waited in backward() for autograd's thread.
	r.cpu = max(r.cpu, r.bwCPU+15_000)
	if !o.NoStack {
		r.pop() // _engine_run_backward
	}
}

func (r *rank) optimizer(o Options) {
	r.py(o, []string{"torch/optim/optimizer.py(493): wrapper", "torch/optim/adam.py(226): step", "torch/optim/adam.py(788): _multi_tensor_adam"}, func() {
		// The step waits for the gradients' all-reduce.
		r.cpu = max(r.cpu, r.free[commStream]-200_000)
		for _, f := range []string{"aten::_foreach_lerp_", "aten::_foreach_mul_", "aten::_foreach_addcmul_", "aten::_foreach_sqrt", "aten::_foreach_div_", "aten::_foreach_addcdiv_"} {
			r.op(r.main, &r.cpu, f, func() {
				r.launch(r.main, &r.cpu, computeStream, "void at::native::(anonymous namespace)::multi_tensor_apply_kernel<at::native::(anonymous namespace)::TensorListMetadata<2>, at::native::(anonymous namespace)::BinaryOpListAlphaFunctor<c10::BFloat16, 2, 2, 0>, std::plus<float>, float>(at::native::(anonymous namespace)::TensorListMetadata<2>, at::native::(anonymous namespace)::BinaryOpListAlphaFunctor<c10::BFloat16, 2, 2, 0>, std::plus<float>, float)", 120_000, 432, 512)
			})
		}
	})
	r.py(o, []string{"torch/optim/optimizer.py(1012): zero_grad"}, func() {
		r.op(r.main, &r.cpu, "aten::zero_", func() {
			r.launch(r.main, &r.cpu, computeStream, "void at::native::vectorized_elementwise_kernel<4, at::native::FillFunctor<c10::BFloat16>, at::detail::Array<char*, 1> >(int, at::native::FillFunctor<c10::BFloat16>, at::detail::Array<char*, 1>)", 30_000, 4096, 128)
		})
	})
}

func (r *rank) stepEnd(o Options) {
	// loss.item(): the main thread waits for the GPU.
	r.py(o, []string{"torch/_tensor.py(1072): item"}, func() {
		r.op(r.main, &r.cpu, "aten::item", func() {
			r.op(r.main, &r.cpu, "aten::_local_scalar_dense", func() {
				r.memcpy("Memcpy DtoH (Device -> Pinned)", 4)
				r.corr++
				call := event{cat: "cuda_runtime", name: "cudaStreamSynchronize", pid: r.pid, tid: r.main, start: r.cpu, corr: r.corr, phase: 'X'}
				r.cpu = max(r.cpu+3_000, r.free[computeStream]+6_000)
				call.end = r.cpu
				r.events = append(r.events, call)
			})
		})
	})
	if !o.NoStack {
		r.pop() // train_step
	}
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].cat == "user_annotation" && r.events[i].end == 0 {
			r.cpu += 8_000
			r.events[i].end = r.cpu
			break
		}
	}
}

// us writes ns as microseconds with three decimals, as Kineto does.
func us(b []byte, ns int64) []byte {
	if ns < 0 {
		b = append(b, '-')
		ns = -ns
	}
	b = strconv.AppendInt(b, ns/1000, 10)
	b = append(b, '.')
	f := ns % 1000
	b = append(b, byte('0'+f/100), byte('0'+f/10%10), byte('0'+f%10))
	return b
}

func (r *rank) write(w io.Writer, o Options) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	fmt.Fprintf(bw, `{
  "schemaVersion": 1,
  "deviceProperties": [
    {"id": 0, "name": "NVIDIA H100 80GB HBM3", "totalGlobalMem": 85045379072, "computeMajor": 9, "computeMinor": 0, "maxThreadsPerBlock": 1024, "maxThreadsPerMultiprocessor": 2048, "regsPerBlock": 65536, "warpSize": 32, "sharedMemPerBlock": 49152, "numSms": 132}
  ],
  "distributedInfo": {"backend": "nccl", "rank": %d, "world_size": %d, "pg_count": 1, "pg_config": [], "nccl_version": "2.21.5"},
  "record_shapes": 1,
  "with_stack": %d,
  "cupti_version": 24,
  "cuda_runtime_version": 12040,
  "cuda_driver_version": 12040,
  "trace_id": "SYNTHETIC%04d",
  "displayTimeUnit": "ms",
  "traceEvents": [
`, r.rank, o.Ranks, map[bool]int{false: 1, true: 0}[o.NoStack], r.rank)
	var base int64
	if o.Relative {
		base = epoch + r.skew
	}
	r.writeEvents(bw, o, "", base)
	end := r.cpu + r.skew - base
	buf := append([]byte(nil), `  {"name": "Record Window End", "ph": "i", "s": "g", "pid": "", "tid": "", "ts": `...)
	buf = us(buf, end)
	buf = append(buf, "}\n  ],\n"...)
	bw.Write(buf)
	if o.Relative {
		fmt.Fprintf(bw, "  \"baseTimeNanoseconds\": %d,\n", base)
	}
	fmt.Fprintf(bw, "  \"traceName\": \"rank%d.pt.trace.json\"\n}\n", r.rank)
	return bw.Flush()
}

// writeEvents writes the rank's events (each followed by a comma) into
// traceEvents, its times from base; a merged file's name its processes
// prefix and their own pid ("rank8:41200"), its prefix "rank8:".
func (r *rank) writeEvents(bw *bufio.Writer, o Options, prefix string, base int64) {
	pid := func(p int64) string {
		if prefix == "" {
			return strconv.FormatInt(p, 10)
		}
		return strconv.Quote(prefix + strconv.FormatInt(p, 10))
	}
	meta := []string{
		fmt.Sprintf(`{"name": "process_name", "ph": "M", "ts": 0.000, "pid": %s, "tid": 0, "args": {"name": "python3"}}`, pid(r.pid)),
		fmt.Sprintf(`{"name": "process_labels", "ph": "M", "ts": 0.000, "pid": %s, "tid": 0, "args": {"labels": "CPU"}}`, pid(r.pid)),
		fmt.Sprintf(`{"name": "thread_name", "ph": "M", "ts": 0.000, "pid": %s, "tid": %d, "args": {"name": "python3"}}`, pid(r.pid), r.main),
		fmt.Sprintf(`{"name": "thread_name", "ph": "M", "ts": 0.000, "pid": %s, "tid": %d, "args": {"name": "pt_autograd_0"}}`, pid(r.pid), r.bw),
		fmt.Sprintf(`{"name": "process_name", "ph": "M", "ts": 0.000, "pid": %s, "tid": 0, "args": {"name": "python3"}}`, pid(0)),
		fmt.Sprintf(`{"name": "process_labels", "ph": "M", "ts": 0.000, "pid": %s, "tid": 0, "args": {"labels": "GPU 0"}}`, pid(0)),
		fmt.Sprintf(`{"name": "thread_name", "ph": "M", "ts": 0.000, "pid": %s, "tid": %d, "args": {"name": "stream %d "}}`, pid(0), computeStream, computeStream),
		fmt.Sprintf(`{"name": "thread_name", "ph": "M", "ts": 0.000, "pid": %s, "tid": %d, "args": {"name": "stream %d "}}`, pid(0), commStream, commStream),
	}
	for _, m := range meta {
		bw.WriteString("  " + m + ",\n")
	}
	var buf []byte
	for i, e := range r.events {
		if e.end < e.start {
			e.end = e.start
		}
		// The rank's wall clock: the true one, off by its skew.
		start := e.start + r.skew - base
		buf = buf[:0]
		buf = append(buf, "  {"...)
		switch e.phase {
		case 's', 'f':
			buf = fmt.Appendf(buf, `"ph": "%c", "id": %d, "pid": %s, "tid": %d, "ts": `, e.phase, e.corr, pid(e.pid), e.tid)
			buf = us(buf, start)
			buf = append(buf, `, "cat": "ac2g", "name": "ac2g"`...)
			if e.phase == 'f' {
				buf = append(buf, `, "bp": "e"`...)
			}
		default:
			p, tid := pid(e.pid), strconv.FormatInt(e.tid, 10)
			if e.cat == "Trace" {
				p, tid = `"Spans"`, `"PyTorch Profiler"`
				if prefix != "" {
					p = strconv.Quote(prefix + "Spans")
				}
			}
			buf = fmt.Appendf(buf, `"ph": "X", "cat": %q, "name": %q, "pid": %s, "tid": %s, "ts": `, e.cat, e.name, p, tid)
			buf = us(buf, start)
			buf = append(buf, `, "dur": `...)
			buf = us(buf, e.end-e.start)
			switch e.cat {
			case "kernel":
				buf = fmt.Appendf(buf, `, "args": {"queued": 0, "device": 0, "context": 1, "stream": %d, "correlation": %d, "registers per thread": 168, "shared memory": 213248, "blocks per SM": %.6f, "warps per SM": 8.0, "grid": [%d, 1, 1], "block": [%d, 1, 1], "est. achieved occupancy %%": 13}`,
					e.stream, e.corr, float64(e.grid)/132, e.grid, e.block)
			case "gpu_memcpy":
				buf = fmt.Appendf(buf, `, "args": {"device": 0, "context": 1, "stream": %d, "correlation": %d, "bytes": %d, "memory bandwidth (GB/s)": 24.0}`, e.stream, e.corr, e.bytes)
			case "cuda_runtime":
				buf = fmt.Appendf(buf, `, "args": {"cbid": 211, "correlation": %d}`, e.corr)
			case "python_function":
				buf = fmt.Appendf(buf, `, "args": {"Python parent id": %d, "Python id": %d, "Python thread": 0}`, e.pyPar, e.pyID)
			case "cpu_op":
				buf = fmt.Appendf(buf, `, "args": {"External id": %d, "Record function id": 0, "Ev Idx": %d}`, i+1, i)
			case "user_annotation":
				buf = fmt.Appendf(buf, `, "args": {"External id": %d, "Record function id": 0, "Ev Idx": %d}`, i+1, i)
			}
		}
		buf = append(buf, "},\n"...)
		bw.Write(buf)
	}
}
