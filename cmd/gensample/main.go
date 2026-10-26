// Command gensample writes a synthetic trace in the schema of
// `nsys export --type sqlite`, for trying the viewer without a GPU.
//
//	go run ./cmd/gensample                       # samples/sample.sqlite, ~200k events
//	go run ./cmd/gensample -steps 2000 -o big.sqlite
//	go run ./cmd/gensample -ranks 8 -o samples/ranks   # rank0.sqlite … rank7.sqlite
//
// With -ranks, it simulates a job of that many ranks, one GPU each, -per-node
// to a host, in lockstep (the all-reduce at the end of each step syncs
// them), and writes each rank's process and GPU to a file of its own. Each
// rank's profiler starts capturing at a different moment (up to -skew
// later), so each file's times are relative to its own session start
// (TARGET_INFO_SESSION_START_TIME): the files line up on absolute time, and
// not on their first events. -starts gives the delays (0,120ms,40ms,...).
// Each node's monotonic clock (systemClockNs) started at another moment.
//
// It simulates a data-parallel training job: one process with a thread per
// GPU launching the kernels of a transformer's forward and backward passes,
// an NCCL all-reduce on a stream of its own, and a data-loader thread
// copying batches to the GPUs. CPU threads run ahead of the GPUs, as CUDA's
// launch queue lets them, and wait for them at the end of every step.
// NVTX ranges nest as step > forward/backward > layer > attention/mlp.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE StringIds (id INTEGER NOT NULL PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE ThreadNames (nameId INTEGER NOT NULL, priority INTEGER, globalTid INTEGER);
CREATE TABLE TARGET_INFO_GPU (vmId INTEGER, id INTEGER NOT NULL, name TEXT, busLocation TEXT,
  totalMemory INTEGER, smCount INTEGER, computeMajor INTEGER, computeMinor INTEGER, uuid TEXT);
CREATE TABLE PROCESSES (globalPid INTEGER, pid INTEGER, name TEXT);
CREATE TABLE TARGET_INFO_SESSION_START_TIME (utcEpochNs INTEGER, utcTime TEXT, localTime TEXT, systemClockNs INTEGER, sessionStartCntVctNs INTEGER);
CREATE TABLE TARGET_INFO_SYSTEM_ENV (globalVid INTEGER, devStateName TEXT, name TEXT, nameEnum INTEGER, value TEXT);
CREATE TABLE ENUM_CUDA_MEMCPY_OPER (id INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, label TEXT);
CREATE TABLE ENUM_NSYS_EVENT_TYPE (id INTEGER NOT NULL PRIMARY KEY, name TEXT NOT NULL, label TEXT);
CREATE TABLE CUPTI_ACTIVITY_KIND_KERNEL (
  start INTEGER NOT NULL, end INTEGER NOT NULL, deviceId INTEGER NOT NULL, contextId INTEGER NOT NULL,
  greenContextId INTEGER, streamId INTEGER NOT NULL, correlationId INTEGER, globalPid INTEGER,
  demangledName INTEGER NOT NULL, shortName INTEGER NOT NULL, mangledName INTEGER,
  launchType INTEGER, cacheConfig INTEGER, registersPerThread INTEGER NOT NULL,
  gridX INTEGER NOT NULL, gridY INTEGER NOT NULL, gridZ INTEGER NOT NULL,
  blockX INTEGER NOT NULL, blockY INTEGER NOT NULL, blockZ INTEGER NOT NULL,
  staticSharedMemory INTEGER NOT NULL, dynamicSharedMemory INTEGER NOT NULL,
  localMemoryPerThread INTEGER NOT NULL, localMemoryTotal INTEGER NOT NULL,
  gridId INTEGER NOT NULL, sharedMemoryExecuted INTEGER, graphNodeId INTEGER, sharedMemoryLimitConfig INTEGER);
CREATE TABLE CUPTI_ACTIVITY_KIND_MEMCPY (
  start INTEGER NOT NULL, end INTEGER NOT NULL, deviceId INTEGER NOT NULL, contextId INTEGER NOT NULL,
  greenContextId INTEGER, streamId INTEGER NOT NULL, correlationId INTEGER, globalPid INTEGER,
  bytes INTEGER NOT NULL, copyKind INTEGER NOT NULL, deprecatedSrcId INTEGER, srcKind INTEGER, dstKind INTEGER,
  srcDeviceId INTEGER, srcContextId INTEGER, dstDeviceId INTEGER, dstContextId INTEGER,
  migrationCause INTEGER, graphNodeId INTEGER, virtualAddress INTEGER);
CREATE TABLE CUPTI_ACTIVITY_KIND_MEMSET (
  start INTEGER NOT NULL, end INTEGER NOT NULL, deviceId INTEGER NOT NULL, contextId INTEGER NOT NULL,
  greenContextId INTEGER, streamId INTEGER NOT NULL, correlationId INTEGER, globalPid INTEGER,
  value INTEGER NOT NULL, bytes INTEGER NOT NULL, graphNodeId INTEGER, memKind INTEGER);
CREATE TABLE CUPTI_ACTIVITY_KIND_RUNTIME (
  start INTEGER NOT NULL, end INTEGER NOT NULL, eventClass INTEGER NOT NULL, globalTid INTEGER,
  correlationId INTEGER, nameId INTEGER NOT NULL, returnValue INTEGER NOT NULL, callchainId INTEGER);
CREATE TABLE NVTX_EVENTS (
  start INTEGER NOT NULL, end INTEGER, eventType INTEGER NOT NULL, rangeId INTEGER, category INTEGER,
  color INTEGER, text TEXT, globalTid INTEGER, endGlobalTid INTEGER, textId INTEGER, domainId INTEGER,
  uint64Value INTEGER, int64Value INTEGER, doubleValue REAL, uint32Value INTEGER, int32Value INTEGER,
  floatValue REAL, jsonTextId INTEGER, jsonText TEXT, binaryData TEXT);
`

const (
	pid       = 4242
	gpus      = 2
	layers    = 24
	t0        = 1_523_000_000 // nsys timestamps do not start at zero
	streamCmp = 7             // compute stream of each GPU
	streamCpy = 14            // copy stream
	streamCom = 22            // NCCL stream
)

type kernelSpec struct {
	short, full string
	base        float64 // mean duration, µs
	grid, block [3]int
	regs        int
	smem        int
}

var kernels = map[string]kernelSpec{
	"qkv":     {"ampere_sgemm_128x64_tn", "ampere_sgemm_128x64_tn", 38, [3]int{96, 8, 1}, [3]int{128, 1, 1}, 122, 49152},
	"flash":   {"flash_fwd_kernel", "void flash::flash_fwd_kernel<Flash_fwd_kernel_traits<128, 128, 64, 4, false, false, cutlass::half_t>, false, false, false, false, true, false, false>(Flash_fwd_params)", 61, [3]int{32, 16, 8}, [3]int{128, 1, 1}, 255, 98304},
	"proj":    {"cutlass_80_tensorop_s1688gemm_256x128_32x3_nn_align4", "cutlass_80_tensorop_s1688gemm_256x128_32x3_nn_align4", 22, [3]int{16, 8, 1}, [3]int{256, 1, 1}, 230, 73728},
	"ln":      {"layer_norm_kernel", "void at::native::(anonymous namespace)::vectorized_layer_norm_kernel<float, float>(int, float, float const*, float const*, float const*, float*, float*, float*)", 6, [3]int{4096, 1, 1}, [3]int{256, 1, 1}, 40, 0},
	"fc1":     {"ampere_sgemm_128x128_nn", "ampere_sgemm_128x128_nn", 52, [3]int{128, 4, 1}, [3]int{256, 1, 1}, 128, 65536},
	"gelu":    {"vectorized_elementwise_kernel", "void at::native::vectorized_elementwise_kernel<4, at::native::GeluCUDAKernelImpl(at::TensorIteratorBase&, at::native::GeluType)::{lambda()#2}::operator()() const::{lambda()#2}::operator()() const::{lambda(float)#1}, at::detail::Array<char*, 2> >(int, at::native::GeluCUDAKernelImpl(at::TensorIteratorBase&, at::native::GeluType)::{lambda()#2}::operator()() const::{lambda()#2}::operator()() const::{lambda(float)#1}, at::detail::Array<char*, 2>)", 9, [3]int{8192, 1, 1}, [3]int{128, 1, 1}, 28, 0},
	"fc2":     {"ampere_sgemm_128x64_nn", "ampere_sgemm_128x64_nn", 47, [3]int{128, 8, 1}, [3]int{128, 1, 1}, 122, 49152},
	"flashb":  {"flash_bwd_dq_dk_dv_loop_kernel", "void flash::flash_bwd_dq_dk_dv_loop_kernel<Flash_bwd_kernel_traits<128, 64, 128, 8, 2, 4, 2, false, false, cutlass::half_t>, false, false, false, false, false>(Flash_bwd_params)", 140, [3]int{16, 16, 8}, [3]int{256, 1, 1}, 255, 114688},
	"dgrad":   {"ampere_sgemm_128x128_tn", "ampere_sgemm_128x128_tn", 64, [3]int{128, 4, 1}, [3]int{256, 1, 1}, 128, 65536},
	"wgrad":   {"ampere_sgemm_64x64_nt", "ampere_sgemm_64x64_nt", 58, [3]int{256, 16, 1}, [3]int{64, 1, 1}, 90, 32768},
	"gelub":   {"vectorized_elementwise_kernel", "void at::native::vectorized_elementwise_kernel<4, at::native::GeluBackwardCUDAKernelImpl(at::TensorIteratorBase&, at::native::GeluType)::{lambda()#2}::operator()() const::{lambda()#2}::operator()() const::{lambda(float, float)#1}, at::detail::Array<char*, 3> >(int, at::native::GeluBackwardCUDAKernelImpl(at::TensorIteratorBase&, at::native::GeluType)::{lambda()#2}::operator()() const::{lambda()#2}::operator()() const::{lambda(float, float)#1}, at::detail::Array<char*, 3>)", 12, [3]int{8192, 1, 1}, [3]int{128, 1, 1}, 30, 0},
	"lnb":     {"layer_norm_grad_input_kernel", "void at::native::(anonymous namespace)::layer_norm_grad_input_kernel<float, float>(float const*, float const*, float const*, float const*, float const*, float*, int)", 9, [3]int{4096, 1, 1}, [3]int{256, 1, 1}, 48, 2048},
	"softmax": {"cunn_SoftMaxForward", "void (anonymous namespace)::cunn_SoftMaxForward<4, float, float, float, (anonymous namespace)::SoftMaxForwardEpilogue>(float*, float const*, int)", 31, [3]int{4096, 1, 1}, [3]int{1024, 1, 1}, 38, 0},
	"xent":    {"nll_loss_forward_reduce_cuda_kernel_2d", "void at::native::(anonymous namespace)::nll_loss_forward_reduce_cuda_kernel_2d<float, float, long>(float*, float*, float const*, long const*, float const*, bool, long, long, long, long)", 14, [3]int{1, 1, 1}, [3]int{1024, 1, 1}, 32, 0},
	"adam":    {"multi_tensor_apply_kernel", "void at::native::(anonymous namespace)::multi_tensor_apply_kernel<at::native::(anonymous namespace)::TensorListMetadata<4>, at::native::(anonymous namespace)::FusedAdamMathFunctor<float, 4, (at::native::ADAM_MODE)0, false>, double*, double, double, double, double, double, bool, float*, float*>(...)", 85, [3]int{320, 1, 1}, [3]int{512, 1, 1}, 64, 0},
	"nccl":    {"ncclDevKernel_AllReduce_Sum_f32_RING_LL", "ncclDevKernel_AllReduce_Sum_f32_RING_LL(ncclDevComm*, unsigned long, ncclWork*)", 900, [3]int{2, 1, 1}, [3]int{640, 1, 1}, 96, 0},
}

type gen struct {
	rng    *rand.Rand
	corr   int64
	gridID int64
	events int
	sinks  []*sink
	// syncColl holds collectives back to end them together (-ranks): an
	// NCCL collective ends on every rank when the last rank's data is in.
	syncColl bool
}

// pendingColl is a collective kernel launched, to write once every rank's
// is known.
type pendingColl struct {
	k          kernelSpec
	start, dur int64
	corr       int64
	stream     int
}

// sink is one output file: a process of the job, on a host, captured from
// a moment on.
type sink struct {
	path    string
	db      *sql.DB
	tx      *sql.Tx
	strs    map[string]int64
	pid     int64
	host    string
	node    int
	gpus    int
	kernel  *sql.Stmt
	memcpy  *sql.Stmt
	memset  *sql.Stmt
	runtime *sql.Stmt
	nvtx    *sql.Stmt
	str     *sql.Stmt
	// capture is when, in the simulation's time, the file's profiler starts:
	// events before are left out, and the file's times count from it.
	capture int64
	events  int
}

// epoch is the wall-clock time, in nanoseconds since 1970, of the
// simulation's time 0 (2026-10-19 12:00:00 UTC).
const epoch = 1792411200 * int64(1e9)

// bootTime is when, in the simulation's time, a node's monotonic clock was
// 0 (it booted): each node's clock starts at another moment, as on real
// machines, so only files of the same node share it.
func bootTime(node int) int64 { return -(1_200_000 + int64(node)*86_417) * int64(time.Second) / 1000 }

// keep reports whether an event starting at t was captured.
func (s *sink) keep(t int64) bool { return t >= t0+s.capture }

// t is the file's time of the simulation's time v: relative to its session
// start, as nsys writes them.
func (s *sink) t(v int64) int64 { return v - s.capture }

// thread is a CPU thread driving one GPU.
type thread struct {
	tid    int64
	gtid   int64
	now    int64 // the thread's clock
	dev    int   // the GPU's index in the simulation
	devOut int   // the GPU's device id in its file
	out    *sink
	stack  []int64 // starts of open NVTX ranges
	names  []string
	// free is when each of the GPU's streams finishes its queued work.
	free map[int]int64
	// coll are the step's collective kernels not written yet (gen.syncColl).
	coll []pendingColl
}

func main() {
	out := flag.String("o", "", "output file (default samples/sample.sqlite), or directory with -ranks (default samples/ranks)")
	steps := flag.Int("steps", 0, "training steps (default 100, which makes about 200k events; 30 with -ranks)")
	seed := flag.Uint64("seed", 1, "random seed")
	ranks := flag.Int("ranks", 0, "write a job of this many ranks, a file each")
	perNode := flag.Int("per-node", 4, "with -ranks, ranks to a host")
	skew := flag.Duration("skew", 300*time.Millisecond, "with -ranks, how much later than the first a rank's capture may start")
	startsFlag := flag.String("starts", "", "with -ranks, how much later than the job's start each rank's capture starts, as durations separated by commas (0,120ms,40ms,...); ranks past the list get -skew")
	flag.Parse()
	if *ranks > 0 {
		if *out == "" {
			*out = filepath.Join("samples", "ranks")
		}
		if *steps == 0 {
			*steps = 30
		}
	} else {
		if *out == "" {
			*out = filepath.Join("samples", "sample.sqlite")
		}
		if *steps == 0 {
			*steps = 100
		}
	}
	var starts []int64
	for _, v := range strings.Split(*startsFlag, ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			log.Fatalf("bad -starts %q", *startsFlag)
		}
		starts = append(starts, d.Nanoseconds())
	}
	if err := run(*out, *steps, *seed, *ranks, max(*perNode, 1), skew.Nanoseconds(), starts); err != nil {
		log.Fatal(err)
	}
}

// openSink creates the database of a sink.
func openSink(s *sink) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	db, err := sql.Open("sqlite", s.path)
	if err != nil {
		return err
	}
	s.db = db
	if _, err := db.Exec(`PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;` + schema); err != nil {
		return err
	}
	if s.tx, err = db.Begin(); err != nil {
		return err
	}
	s.strs = map[string]int64{}
	prep := func(q string) *sql.Stmt {
		st, err2 := s.tx.Prepare(q)
		if err2 != nil && err == nil {
			err = err2
		}
		return st
	}
	s.str = prep(`INSERT INTO StringIds (id, value) VALUES (?, ?)`)
	s.kernel = prep(`INSERT INTO CUPTI_ACTIVITY_KIND_KERNEL VALUES (?,?,?,1,NULL,?,?,?,?,?,?,1,0,?,?,?,?,?,?,?,?,?,0,0,?,?,NULL,1)`)
	s.memcpy = prep(`INSERT INTO CUPTI_ACTIVITY_KIND_MEMCPY VALUES (?,?,?,1,NULL,?,?,?,?,?,NULL,?,?,?,1,?,1,0,NULL,NULL)`)
	s.memset = prep(`INSERT INTO CUPTI_ACTIVITY_KIND_MEMSET VALUES (?,?,?,1,NULL,?,?,?,0,?,NULL,1)`)
	s.runtime = prep(`INSERT INTO CUPTI_ACTIVITY_KIND_RUNTIME VALUES (?,?,1,?,?,?,0,NULL)`)
	s.nvtx = prep(`INSERT INTO NVTX_EVENTS (start, end, eventType, rangeId, category, color, text, globalTid, endGlobalTid, textId, domainId)
		VALUES (?,?,?,NULL,0,?,?,?,?,?,0)`)
	if err != nil {
		return err
	}
	return s.metadata()
}

func run(out string, steps int, seed uint64, ranks, perNode int, skew int64, starts []int64) error {
	g := &gen{rng: rand.New(rand.NewPCG(seed, 0x6e73)), syncColl: ranks > 0}
	var threads, loaders []*thread
	if ranks == 0 {
		// One process driving two GPUs, and a data-loader thread.
		s := &sink{path: out, pid: pid, host: "trainer-0", gpus: gpus}
		g.sinks = []*sink{s}
		if err := openSink(s); err != nil {
			return err
		}
		for d := range gpus {
			tid := int64(pid + d*8)
			threads = append(threads, &thread{tid: tid, gtid: gtidOf(pid, tid), now: t0 + int64(d)*37_000, dev: d, devOut: d, out: s, free: map[int]int64{}})
		}
		loaders = []*thread{{tid: pid + 30, gtid: gtidOf(pid, pid+30), now: t0, out: s, free: map[int]int64{}}}
		for i, th := range append([]*thread{loaders[0]}, threads...) {
			name := []string{"pt_data_worker", "python3", "pt_autograd_0"}[i]
			if _, err := s.tx.Exec(`INSERT INTO ThreadNames VALUES (?, 0, ?)`, s.id(name), th.gtid); err != nil {
				return err
			}
		}
	} else {
		// A process and a GPU per rank, with its own data loader; each
		// rank's capture starts a little after the first's.
		skews := rand.New(rand.NewPCG(seed, 0x736b6577))
		for r := range ranks {
			p := int64(pid + r*17)
			s := &sink{path: filepath.Join(out, fmt.Sprintf("rank%d.sqlite", r)), pid: p,
				host: fmt.Sprintf("node%d", r/perNode), node: r / perNode, gpus: 1}
			switch {
			case r < len(starts):
				s.capture = starts[r]
			case r > 0 && skew > 0:
				s.capture = skews.Int64N(skew)
			}
			g.sinks = append(g.sinks, s)
			if err := openSink(s); err != nil {
				return err
			}
			th := &thread{tid: p, gtid: gtidOf(p, p), now: t0 + int64(r)*37_000, dev: r, devOut: r % perNode, out: s, free: map[int]int64{}}
			ld := &thread{tid: p + 30, gtid: gtidOf(p, p+30), now: t0, dev: r, devOut: r % perNode, out: s, free: th.free}
			threads, loaders = append(threads, th), append(loaders, ld)
			for _, x := range []struct {
				name string
				t    *thread
			}{{"python3", th}, {"pt_data_worker", ld}} {
				if _, err := s.tx.Exec(`INSERT INTO ThreadNames VALUES (?, 0, ?)`, s.id(x.name), x.t.gtid); err != nil {
					return err
				}
			}
		}
	}

	for step := range steps {
		if ranks == 0 {
			// The data loader prepares the next batch for every GPU.
			loader := loaders[0]
			loader.push(g, fmt.Sprintf("load batch %d", step%8), false)
			loader.now += g.us(900, 0.3)
			for _, th := range threads {
				// Copies go through the loader's runtime calls, on each GPU's copy stream.
				loader.dev, loader.devOut = th.dev, th.devOut
				loader.free = th.free
				loader.memcpy(g, 1, 64<<20, streamCpy)
			}
			loader.pop(g)
			loader.now += g.us(200, 0.5)
		} else {
			for _, ld := range loaders {
				ld.push(g, fmt.Sprintf("load batch %d", step%8), false)
				ld.now += g.us(900, 0.3)
				ld.memcpy(g, 1, 64<<20, streamCpy)
				ld.pop(g)
				ld.now += g.us(200, 0.5)
			}
		}
		latest := int64(0)
		for _, ld := range loaders {
			latest = max(latest, ld.now)
		}
		for _, th := range threads {
			th.now = max(th.now, latest-g.us(3000, 0.2))
			g.step(th, step)
		}
		if g.syncColl {
			g.syncCollectives(threads)
		}
		// Steps take as long as the slowest GPU: the all-reduce syncs them.
		end := int64(0)
		for _, th := range threads {
			end = max(end, th.now)
		}
		for _, th := range threads {
			th.now = end + g.us(20, 0.5)
		}
		for _, ld := range loaders {
			ld.now = max(ld.now, end-g.us(6000, 0.2))
		}
	}
	for _, s := range g.sinks {
		if err := s.tx.Commit(); err != nil {
			return err
		}
		s.db.Close()
		fi, _ := os.Stat(s.path)
		fmt.Printf("wrote %s: %d events, %.1f MB\n", s.path, s.events, float64(fi.Size())/1e6)
	}
	return nil
}

func gtidOf(pid, tid int64) int64 { return 1<<56 | pid<<24 | tid }

func (s *sink) metadata() error {
	for i := range s.gpus {
		if _, err := s.tx.Exec(`INSERT INTO TARGET_INFO_GPU VALUES (0, ?, ?, ?, 85520809984, 132, 9, 0, ?)`,
			i, "NVIDIA H100 80GB HBM3", fmt.Sprintf("0000:%02x:00.0", 0x18+i*0x20), fmt.Sprintf("GPU-%08x", 0x5eed0000+i)); err != nil {
			return err
		}
	}
	for id, n := range map[int]string{0: "UNKNOWN", 1: "HTOD", 2: "DTOH", 8: "DTOD", 10: "PTOP"} {
		if _, err := s.tx.Exec(`INSERT INTO ENUM_CUDA_MEMCPY_OPER VALUES (?, ?, ?)`, id, "CUDA_MEMCPY_KIND_"+n, n); err != nil {
			return err
		}
	}
	for id, n := range map[int]string{33: "NvtxCategory", 34: "NvtxMark", 59: "NvtxPushPopRange", 60: "NvtxStartEndRange", 75: "NvtxDomainCreate"} {
		if _, err := s.tx.Exec(`INSERT INTO ENUM_NSYS_EVENT_TYPE VALUES (?, ?, ?)`, id, n, n); err != nil {
			return err
		}
	}
	// When the capture started, on the wall clock and on the node's
	// monotonic clock (systemClockNs): the files' times count from it.
	start := epoch + s.capture
	mono := s.capture - bootTime(s.node)
	utc := time.Unix(0, start).UTC().Format("2006-01-02T15:04:05")
	if _, err := s.tx.Exec(`INSERT INTO TARGET_INFO_SESSION_START_TIME VALUES (?, ?, ?, ?, NULL)`, start, utc, utc, mono); err != nil {
		return err
	}
	if _, err := s.tx.Exec(`INSERT INTO TARGET_INFO_SYSTEM_ENV VALUES (0, 'Local (CLI)', 'Hostname', 0, ?)`, s.host); err != nil {
		return err
	}
	if _, err := s.tx.Exec(`INSERT INTO PROCESSES VALUES (?, ?, 'python3')`, int64(1<<56|s.pid<<24), s.pid); err != nil {
		return err
	}
	return nil
}

// id interns a string into the file's StringIds.
func (s *sink) id(v string) int64 {
	if id, ok := s.strs[v]; ok {
		return id
	}
	id := int64(len(s.strs) + 1)
	s.strs[v] = id
	if _, err := s.str.Exec(id, v); err != nil {
		log.Fatal(err)
	}
	return id
}

// us returns a duration around mean microseconds, log-normally spread.
func (g *gen) us(mean, spread float64) int64 {
	v := mean * math.Exp(g.rng.NormFloat64()*spread-spread*spread/2)
	return int64(v * 1000)
}

// count counts an event written to th's file.
func (g *gen) count(th *thread) {
	g.events++
	th.out.events++
}

func must(_ sql.Result, err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// push opens an NVTX range on the thread; registered strings go to
// textId, others to text, as nsys stores them.
func (th *thread) push(g *gen, name string, registered bool) {
	th.now += g.us(0.4, 0.2)
	th.stack = append(th.stack, th.now)
	if registered {
		th.names = append(th.names, "\x00"+name)
	} else {
		th.names = append(th.names, name)
	}
}

func (th *thread) pop(g *gen) {
	th.now += g.us(0.3, 0.2)
	n := len(th.stack) - 1
	start, name := th.stack[n], th.names[n]
	th.stack, th.names = th.stack[:n], th.names[:n]
	if !th.out.keep(start) {
		return
	}
	var text, textID any
	if name != "" && name[0] == 0 {
		textID = th.out.id(name[1:])
	} else {
		text = name
	}
	must(th.out.nvtx.Exec(th.out.t(start), th.out.t(th.now), 59, nil, text, th.gtid, th.gtid, textID))
	g.count(th)
}

func (th *thread) mark(g *gen, name string) {
	if !th.out.keep(th.now) {
		return
	}
	must(th.out.nvtx.Exec(th.out.t(th.now), nil, 34, nil, name, th.gtid, nil, nil))
	g.count(th)
}

// api records a CUDA runtime call on the thread, lasting d.
func (th *thread) api(g *gen, name string, d int64) (start, end, corr int64) {
	g.corr++
	start = th.now + g.us(0.8, 0.5)
	end = start + d
	if th.out.keep(start) {
		must(th.out.runtime.Exec(th.out.t(start), th.out.t(end), th.gtid, g.corr, th.out.id(name)))
		g.count(th)
	}
	th.now = end
	return start, end, g.corr
}

// launch launches a kernel on stream.
func (th *thread) launch(g *gen, key string, stream int) {
	k := kernels[key]
	_, end, corr := th.api(g, "cudaLaunchKernel_v7000", g.us(4.5, 0.35))
	start := max(end+g.us(3, 0.3), th.free[stream])
	dur := g.us(k.base, 0.18)
	if g.rng.IntN(400) == 0 {
		dur *= 6 // the odd slow kernel, as a real trace has
	}
	th.free[stream] = start + dur
	if g.syncColl && key == "nccl" {
		th.coll = append(th.coll, pendingColl{k, start, dur, corr, stream})
		return
	}
	th.kernel(g, k, start, start+dur, corr, stream)
}

func (th *thread) kernel(g *gen, k kernelSpec, start, end, corr int64, stream int) {
	g.gridID++
	if o := th.out; o.keep(start) {
		must(o.kernel.Exec(o.t(start), o.t(end), th.devOut, stream, corr, o.pid<<24, o.id(k.full), o.id(k.short), o.id(k.short), k.regs,
			k.grid[0], k.grid[1], k.grid[2], k.block[0], k.block[1], k.block[2], k.smem, 0, g.gridID, k.smem))
		g.count(th)
	}
}

// syncCollectives writes the step's collective kernels, the k-th of every
// rank ending together: when the last of them would have, each starting
// no earlier than its rank's previous one ended.
func (g *gen) syncCollectives(threads []*thread) {
	prev := make([]int64, len(threads))
	for k := 0; ; k++ {
		end, any := int64(0), false
		for i, th := range threads {
			if k < len(th.coll) {
				c := th.coll[k]
				end = max(end, max(c.start, prev[i])+c.dur)
				any = true
			}
		}
		if !any {
			break
		}
		for i, th := range threads {
			if k < len(th.coll) {
				c := th.coll[k]
				start := max(c.start, prev[i])
				th.kernel(g, c.k, start, end, c.corr, c.stream)
				prev[i] = end
				th.free[c.stream] = max(th.free[c.stream], end)
			}
		}
	}
	for _, th := range threads {
		th.coll = th.coll[:0]
	}
}

func (th *thread) memcpy(g *gen, kind int, bytes int64, stream int) {
	_, end, corr := th.api(g, "cudaMemcpyAsync_v3020", g.us(6, 0.3))
	start := max(end+g.us(4, 0.3), th.free[stream])
	// ~22 GB/s over PCIe for host copies, far more on the device.
	bw := 22.0
	if kind == 8 {
		bw = 1500
	}
	dur := int64(float64(bytes)/bw) + g.us(2, 0.2)
	src, dst := 0, 0
	switch kind {
	case 1:
		src, dst = 1, 2
	case 2:
		src, dst = 2, 1
	}
	if o := th.out; o.keep(start) {
		must(o.memcpy.Exec(o.t(start), o.t(start+dur), th.devOut, stream, corr, o.pid<<24, bytes, kind, src, dst, th.devOut, th.devOut))
		g.count(th)
	}
	th.free[stream] = start + dur
}

func (th *thread) memset(g *gen, bytes int64, stream int) {
	_, end, corr := th.api(g, "cudaMemsetAsync_v3020", g.us(3.5, 0.3))
	start := max(end+g.us(3, 0.3), th.free[stream])
	dur := int64(float64(bytes)/2500) + g.us(1.5, 0.2)
	if o := th.out; o.keep(start) {
		must(o.memset.Exec(o.t(start), o.t(start+dur), th.devOut, stream, corr, o.pid<<24, bytes))
		g.count(th)
	}
	th.free[stream] = start + dur
}

// wait makes the thread wait in cudaStreamSynchronize for stream.
func (th *thread) wait(g *gen, stream int) {
	start := th.now + g.us(1, 0.3)
	done := max(th.free[stream], th.free[streamCpy]) + g.us(4, 0.3)
	th.now = start
	th.api(g, "cudaStreamSynchronize_v3020", max(done-start, g.us(2, 0.3)))
}

func (g *gen) step(th *thread, step int) {
	th.push(g, fmt.Sprintf("step %d", step), false)
	// The batch must be on the GPU before the forward pass starts.
	th.api(g, "cudaStreamWaitEvent_v3020", g.us(1.5, 0.3))
	th.free[streamCmp] = max(th.free[streamCmp], th.free[streamCpy])

	th.push(g, "forward", true)
	for l := range layers {
		th.push(g, fmt.Sprintf("layer.%02d", l), true)
		th.push(g, "attention", true)
		for _, k := range []string{"ln", "qkv", "flash", "proj"} {
			th.launch(g, k, streamCmp)
		}
		th.pop(g)
		th.push(g, "mlp", true)
		for _, k := range []string{"ln", "fc1", "gelu", "fc2"} {
			th.launch(g, k, streamCmp)
		}
		th.pop(g)
		th.pop(g)
	}
	th.push(g, "loss", true)
	th.launch(g, "softmax", streamCmp)
	th.launch(g, "xent", streamCmp)
	th.pop(g)
	th.pop(g)

	th.push(g, "backward", true)
	th.memset(g, 512<<20, streamCmp)
	for l := layers - 1; l >= 0; l-- {
		th.push(g, fmt.Sprintf("layer.%02d.grad", l), true)
		for _, k := range []string{"dgrad", "wgrad", "gelub", "dgrad", "wgrad", "lnb", "flashb", "dgrad", "wgrad", "lnb"} {
			th.launch(g, k, streamCmp)
		}
		th.pop(g)
		if l%8 == 0 {
			// Gradient buckets all-reduce on the NCCL stream, overlapping the
			// rest of the backward pass.
			th.push(g, "allreduce bucket", true)
			th.free[streamCom] = max(th.free[streamCom], th.free[streamCmp])
			th.launch(g, "nccl", streamCom)
			th.pop(g)
		}
	}
	th.pop(g)

	th.push(g, "optimizer.step", true)
	th.free[streamCmp] = max(th.free[streamCmp], th.free[streamCom])
	for range 4 {
		th.launch(g, "adam", streamCmp)
	}
	th.pop(g)
	th.memcpy(g, 2, 4, streamCmp) // the loss, back to the host
	if step%10 == 0 {
		th.mark(g, fmt.Sprintf("checkpoint %d", step))
	}
	th.wait(g, streamCmp)
	th.pop(g)
}
