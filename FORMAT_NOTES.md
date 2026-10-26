# The .nsys-rep container: format notes

Clean-room notes on the Nsight Systems report format, from analysis of one
report file (written by Nsight Systems 2026.3.1) compared against an
`nsys export --type sqlite` of the same report. No NVIDIA code or binaries
were used or inspected. Field numbers below are protobuf field numbers;
offsets and counts come from that sample. No content of the sample (names,
paths, command lines) appears here.

There is no public documentation of the format and no open-source reader
for it that we could find: the tools that consume reports (e.g. nsys2prv)
convert them with `nsys export` first.

## Verdict: feasible, and implemented (`internal/nsysrep`)

Everything the viewer reads decodes natively, with exact agreement against
the SQLite export: every kernel, memcpy, memset, CUDA API call, CUDA
synchronization and NVTX event (counts, start/end, correlation ids, device,
stream, thread ids, string ids), the whole string table, GPU names, thread
names and process names. Times are the export's nanoseconds as stored (no
clock conversion), and string ids are the export's `StringIds` ids. The
container is simple (a table of contents, varint framing, LZ4 blocks,
protobuf records); the work is in mapping field numbers, which the export
makes mechanical: mapping took a few hours and the Go decoder about as
long again. The risk is drift: field numbers and framing are unversioned in
the file, so the loader only trusts the versions it was checked against
(see Versions below).

On the sample the native load is about 1.5x faster than loading the SQLite
export (about 285 ms against 425 ms in `tracelight bench`), and needs no
export step at all; its peak heap is higher (about 275 MB against 195 MB),
as the report file and string table are held while events decode.

## Layout

```
"NVIDIA Tegra Profiler Report <version>v0."     text, no terminating newline
sections ...                                    at offsets given by the TOC
TOC                                             protobuf, see below
u64 LE  offset of the TOC
"U3w[END]"                                      8-byte trailer
```

`<version>` is `@`-separated, as `2026@3@1@<build>`. The byte after `v0.`
is 0x0a, which looks like a newline but is the first protobuf tag of the
first section.

### Table of contents

A sequence of `f1` (length-delimited) entries, one per section:
`{f1: name (string), f2: offset (varint, absolute), f3: size (varint)}`.
The sample has six sections:

| section               | offset   | size     | content                          |
|-----------------------|----------|----------|----------------------------------|
| Analysis Options Data | 57       | 15 258   | protobuf: capture settings/host  |
| Raw Sampling Data     | 15 315   | 28.5 MB  | the events, see below            |
| Sampling Data Offsets | end-~45K | 15       | `{f1:"lz4", f2, f3}`             |
| Agent activity list   |          | 35 116   | not decoded (not needed)         |
| Time Conversion Data  |          | 5 123    | wall-clock samples against a CPU counter, see below |
| UI Data               |          | 4 443    | not decoded                      |

### Raw Sampling Data

Starts with the 8-byte magic `QDTEXT00`, followed by frames up to the end of
the section. A frame is either

- compressed: `varint compressedLen, varint rawLen, LZ4 block` (raw LZ4
  block format: no frame header, no checksum), or
- plain: `varint len, protobuf message`.

Nothing marks which; a frame is compressed when `compressedLen` is within
LZ4's worst case for `rawLen` (`rawLen + rawLen/255 + 16`),
the block decodes to exactly `rawLen` bytes, and the decoded bytes start
with a varint equal to `rawLen` minus that varint's own length (every
decoded block holds one length-prefixed message). In the sample the two
first and the last few frames are plain, the rest compressed.

"Sampling Data Offsets" `f1` names the compression (`lz4`); `f2` and `f3`
are offsets relative to the end of the `QDTEXT00` magic: `f2` is the frame
holding the string table, `f3` the last frame (an index of record layouts,
not needed to decode).

Frame order in the sample:

| frames  | kind  | content                                              |
|---------|-------|------------------------------------------------------|
| 0, 1    | plain | session/target metadata                              |
| 2..178  | lz4   | event batches, 7000 events each (last one partial)   |
| 179     | lz4   | string table (at `f2`)                               |
| 180     | plain | thread names                                         |
| 181     | plain | target info: GPUs, CUDA streams, contexts            |
| 182     | lz4   | record layout index (at `f3`)                        |

### Event batches

A batch message is `repeated f1 Event`. An `Event` is
`{f1: event type (ENUM_NSYS_EVENT_TYPE id), f<N>: payload message}`, where
the payload field number tells the payload's shape:

| payload | event type                     | SQLite table                        |
|---------|--------------------------------|-------------------------------------|
| f107    | 79, 142, 80, 106, 127          | CUPTI activities (GPU side), below  |
| f108    | 48 TraceFunctions              | CUDA API (eventClass 0, 1); also overhead (3, 103), others |
| f102    | 59, 60, 34, 70, 71, 39, 75     | NVTX_EVENTS                         |
| f113    | 27 Command                     | PROCESSES                           |
| f110    | 49 Diagnostic                  | DIAGNOSTIC_EVENT                    |

**f107, GPU activity.** Common fields: `1 start, 2 end, 3 correlationId,
4 deviceId, 5 contextId, 6 streamId, 7 activity kind, 8 globalPid,
9 (always 0; greenContextId?)`. The kind's own fields are in one sub-message:

- `f101` kernel: `1 demangledName id, 2 shortName id, 3 mangledName id,
  10/11/12 grid x/y/z, 13/14/15 block x/y/z, 16 staticSharedMemory,
  17 dynamicSharedMemory, 18 localMemoryPerThread, 19 localMemoryTotal,
  20 gridId, 21 registersPerThread, 22 sharedMemoryExecuted, 23 cacheConfig,
  25 sharedMemoryRequestedPercentage, 28 sharedMemoryLimitConfig,
  31/32/33 cluster x/y/z, 35 maxPotentialClusterSize, 36 maxActiveClusters`
- `f102` memcpy: `1 bytes, 2 copyKind, 4 srcKind, 5 dstKind`
- `f103` memset: `1 bytes, 2 value, 4 memKind`
- `f105` synchronization: `1 eventId, 3 eventSyncId, 4 syncType`
- `f106` CUDA event record (CUPTI_ACTIVITY_KIND_CUDA_EVENT)

Device id: fields 4 and 9 are both 0 throughout the sample (one GPU used),
so which one is `deviceId` is inferred from field order (4 comes before
context and stream, as in CUPTI's records), not proven.

**f108, CUDA API.** `1 start, 2 end, 3 correlationId, 4/5 thread ids,
6 eventClass, 7 nameId, 8 returnValue, 13 globalTid`. Calls are eventClass
0 (runtime) and 1 (driver); 3 and 103 are profiler/CUPTI overhead.

**f102, NVTX.** `1 eventType, 2 start, 10 end (absent for unclosed ranges and
non-range types), 7 text (inline string), 12 textId (string id),
9 globalTid, 13 domainId`. Times are uint64 two's complement: NVTX events
from before the session start are negative.

**f113, process.** `1 time, 2 globalPid, 5 and 6 names (the export's name is
6, or 5 where 6 is empty), 7/8/9 more strings (paths, arguments)`. One record
per process; a few records with only `1, 2, 12` are not in PROCESSES.

### String table (frame at `f2`)

`repeated f1 bytes`: the strings in id order, so an id is the index
(379 125 strings, all equal to `StringIds`). Two trailing `f2` messages with
three varints each are not needed.

### Thread names (plain frame after the strings)

`repeated f1 {2 nameId, 3 priority, 4 globalTid}`, as ThreadNames.

### Target info (plain frame)

- `f1.f2.f7` repeated, one per GPU: `1 id, 2 name, 9 = 15 = 19 = id, ...`
  (TARGET_INFO_GPU; 8/8 names equal).
- `f15` repeated, one per CUDA stream: `1 streamId, 2 hwId, 3 vmId,
  4 processId, 5 contextId, 6 priority, 7 flag` (TARGET_INFO_CUDA_STREAM).
- `f14` CUDA context info; `f24` repeated per thread (four varints).

## Validation (sample)

| table                               | rows      | native = export |
|-------------------------------------|-----------|-----------------|
| CUPTI_ACTIVITY_KIND_KERNEL          | 96 178    | all fields      |
| CUPTI_ACTIVITY_KIND_MEMCPY          | 1 349     | all fields      |
| CUPTI_ACTIVITY_KIND_MEMSET          | 14 525    | all fields      |
| CUPTI_ACTIVITY_KIND_RUNTIME         | 536 259   | all fields      |
| CUPTI_ACTIVITY_KIND_SYNCHRONIZATION | 95 320    | all fields read |
| NVTX_EVENTS                         | 455 030   | fields read     |
| StringIds                           | 379 125   | all             |
| ThreadNames                         | 8 676     | all             |
| PROCESSES (names)                   | 306       | all             |
| TARGET_INFO_GPU (names)             | 8         | all             |

Fields were mapped with throwaway analysis scripts (equality counts per
field, joined by correlation id, or by start and globalTid for NVTX). The
tests check the result end to end when a real report and its export are
given (TRACELIGHT_REPORT, TRACELIGHT_TRACE): `TestNativeMatchesExport` loads both into the viewer's model
and compares every event of every track (1 198 638 events on 36 tracks:
times, names, correlation ids, kernel and memory details, device and
stream), the event counts per kind, and the device, thread and process
names; `TestNativeStringIds` compares the string table with `StringIds` id
by id. Synthetic reports test the decoder without the sample.

## Versions

Field numbers and framing carry no version of their own, so the loader
only decodes reports whose header version is in `nsysrep.Verified`
(2026.3.x); others, and any report it fails to decode, go through
`nsys export` as before, with a warning saying why. `TRACELIGHT_NATIVE=force`
tries the native decoder on any version, `=off` never uses it.

## Not decoded

The analysis options, agent activity list, time conversion data and UI
data sections; the record layout index; OS runtime, sampling, and the other
event kinds the viewer does not show.

## The session's clock

Event times count from the session's start. The first frame of the raw
sampling data gives that start on the capturing host's monotonic clock in
field 17 (and 19; field 18 is the end), equal to the export's
`TARGET_INFO_SESSION_START_TIME.systemClockNs`, and the host's name in
field 2.9 (also in the "Analysis Options Data" field 9), the export's
`TARGET_INFO_SYSTEM_ENV` Hostname.

The wall-clock start (the export's `utcEpochNs`) follows from "Time
Conversion Data": groups `tc.2.1`, each a list of samples `3: {1: {1: a,
2: b}, 2: rate (float64)}` relating two clocks. Most groups are samples of
the wall clock against a CPU counter (rate about the counter's ticks per
ns); one group of a single sample at rate 1 holds a reading of the wall
clock (UTC, ns since 1970) in `a` and of the monotonic clock in `b`, taken
together a little before the session starts. The session's wall-clock
start is `a + (monotonic start - b)`, which equals the export's
`utcEpochNs` to the nanosecond. (Another single-sample group pairs local
time with UTC.) `nsysrep.Session` reads both starts and the host.
