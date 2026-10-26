package nsysrep

import "fmt"

// GPU is what GPU activities share. Field numbers are those of the
// activity message (payload 107 of an event).
type GPU struct {
	Start, End  int64 // 1, 2
	Correlation int64 // 3
	Device      int32 // 4
	Context     int32 // 5
	Stream      int32 // 6
	GlobalPid   int64 // 8
}

// Kernel is a CUDA kernel (CUPTI_ACTIVITY_KIND_KERNEL).
type Kernel struct {
	GPU
	// String ids of the names, -1 if absent.
	DemangledName, ShortName, MangledName int64
	GridX, GridY, GridZ                   int32
	BlockX, BlockY, BlockZ                int32
	Registers                             int32
	StaticShared, DynamicShared           int32
}

// Memcpy is a memory copy (CUPTI_ACTIVITY_KIND_MEMCPY).
type Memcpy struct {
	GPU
	Bytes                      int64
	CopyKind, SrcKind, DstKind int32
}

// Memset is a memory set (CUPTI_ACTIVITY_KIND_MEMSET).
type Memset struct {
	GPU
	Bytes          int64
	Value, MemKind int32
}

// Sync is a CUDA synchronization (CUPTI_ACTIVITY_KIND_SYNCHRONIZATION).
type Sync struct {
	GPU
	SyncType int32
	EventID  int64
}

// API is a CUDA runtime or driver API call (CUPTI_ACTIVITY_KIND_RUNTIME).
type API struct {
	Start, End  int64
	Correlation int64
	GlobalTid   int64
	// EventClass is 0 for the runtime API, 1 for the driver API.
	EventClass  int32
	NameID      int64
	ReturnValue int64
}

// NVTX event types (ENUM_NSYS_EVENT_TYPE), as NVTX.EventType.
const (
	NVTXMark            = 34
	NVTXPushPopRange    = 59
	NVTXStartEndRange   = 60
	NVTXPushPopPayload  = 70
	NVTXStartEndPayload = 71
)

// NVTX is an NVTX event (NVTX_EVENTS).
type NVTX struct {
	EventType  int32
	Start, End int64
	// HasEnd is false for marks, ranges never closed, and other types.
	HasEnd bool
	// Text is the inline text, valid only during the Visitor call; else
	// TextID is the string id of the text, or -1.
	Text      []byte
	TextID    int64
	GlobalTid int64
	Domain    int64
}

// Process names a process (PROCESSES).
type Process struct {
	GlobalPid int64
	// Name is valid only during the Visitor call.
	Name []byte
}

// Visitor receives the events of a frame. Nil functions skip their events.
// The pointers are only valid during the call.
type Visitor struct {
	Kernel  func(*Kernel)
	Memcpy  func(*Memcpy)
	Memset  func(*Memset)
	Sync    func(*Sync)
	API     func(*API)
	NVTX    func(*NVTX)
	Process func(*Process)
}

// Payload field numbers of an event.
const (
	payloadNVTX     = 102
	payloadGPU      = 107
	payloadAPI      = 108
	payloadProcess  = 113
	gpuKernel       = 101
	gpuMemcpy       = 102
	gpuMemset       = 103
	gpuSync         = 105
	apiClassRuntime = 0
	apiClassDriver  = 1
)

// EventFrames returns the frames that may hold events, for DecodeEvents:
// the frames before the string table.
func (f *File) EventFrames() []int {
	var out []int
	for i := 0; i < f.strFrame; i++ {
		out = append(out, i)
	}
	return out
}

// DecodeEvents decodes frame i and calls v for its events. buf is a scratch
// buffer for decompressing, returned for reuse. Frames that are not event
// batches give no events.
func (f *File) DecodeEvents(i int, buf []byte, v *Visitor) ([]byte, error) {
	fr := &f.frames[i]
	msg, buf, err := fr.decode(buf)
	if err != nil {
		return buf, fmt.Errorf("frame %d: %w", i, err)
	}
	if !isBatch(msg) {
		return buf, nil
	}
	if err := decodeBatch(msg, v); err != nil {
		return buf, fmt.Errorf("frame %d: %w", i, err)
	}
	return buf, nil
}

// isBatch reports whether msg is a batch of events: only field 1 messages,
// the first of which starts with its event type.
func isBatch(msg []byte) bool {
	r := newReader(msg)
	var e field
	first := true
	for r.next(&e) {
		if e.num != 1 || e.typ != wireBytes {
			return false
		}
		if first {
			first = false
			er := newReader(e.bytes)
			var g field
			if !er.next(&g) || g.num != 1 || g.typ != wireVarint {
				return false
			}
		}
	}
	return r.err == nil && !first
}

func decodeBatch(msg []byte, v *Visitor) error {
	r := newReader(msg)
	var e, g field
	var (
		k  Kernel
		mc Memcpy
		ms Memset
		sy Sync
		ap API
		nv NVTX
		pr Process
	)
	for r.next(&e) {
		er := newReader(e.bytes)
		evType := int32(-1)
		for er.next(&g) {
			switch {
			case g.num == 1 && g.typ == wireVarint:
				evType = int32(g.v)
			case g.typ != wireBytes:
			case g.num == payloadGPU:
				if err := decodeGPU(g.bytes, v, &k, &mc, &ms, &sy); err != nil {
					return err
				}
			case g.num == payloadAPI && v.API != nil:
				if err := decodeAPI(g.bytes, &ap); err != nil {
					return err
				}
				if ap.EventClass == apiClassRuntime || ap.EventClass == apiClassDriver {
					v.API(&ap)
				}
			case g.num == payloadNVTX && v.NVTX != nil:
				if err := decodeNVTX(g.bytes, &nv); err != nil {
					return err
				}
				if nv.EventType < 0 {
					nv.EventType = evType
				}
				v.NVTX(&nv)
			case g.num == payloadProcess && v.Process != nil:
				if err := decodeProcess(g.bytes, &pr); err != nil {
					return err
				}
				if len(pr.Name) > 0 {
					v.Process(&pr)
				}
			}
		}
		if er.err != nil {
			return er.err
		}
	}
	return r.err
}

func decodeGPU(b []byte, v *Visitor, k *Kernel, mc *Memcpy, ms *Memset, sy *Sync) error {
	var c GPU
	var sub []byte
	kind := 0
	r := newReader(b)
	var f field
	for r.next(&f) {
		if f.typ == wireBytes {
			if f.num > 100 {
				kind, sub = f.num, f.bytes
			}
			continue
		}
		switch f.num {
		case 1:
			c.Start = int64(f.v)
		case 2:
			c.End = int64(f.v)
		case 3:
			c.Correlation = int64(f.v)
		case 4:
			c.Device = int32(f.v)
		case 5:
			c.Context = int32(f.v)
		case 6:
			c.Stream = int32(f.v)
		case 8:
			c.GlobalPid = int64(f.v)
		}
	}
	if r.err != nil {
		return r.err
	}
	r = newReader(sub)
	switch {
	case kind == gpuKernel && v.Kernel != nil:
		*k = Kernel{GPU: c, DemangledName: -1, ShortName: -1, MangledName: -1}
		for r.next(&f) {
			if f.typ != wireVarint {
				continue
			}
			x := int32(f.v)
			switch f.num {
			case 1:
				k.DemangledName = int64(f.v)
			case 2:
				k.ShortName = int64(f.v)
			case 3:
				k.MangledName = int64(f.v)
			case 10:
				k.GridX = x
			case 11:
				k.GridY = x
			case 12:
				k.GridZ = x
			case 13:
				k.BlockX = x
			case 14:
				k.BlockY = x
			case 15:
				k.BlockZ = x
			case 16:
				k.StaticShared = x
			case 17:
				k.DynamicShared = x
			case 21:
				k.Registers = x
			}
		}
		if r.err == nil {
			v.Kernel(k)
		}
	case kind == gpuMemcpy && v.Memcpy != nil:
		*mc = Memcpy{GPU: c}
		for r.next(&f) {
			switch f.num {
			case 1:
				mc.Bytes = int64(f.v)
			case 2:
				mc.CopyKind = int32(f.v)
			case 4:
				mc.SrcKind = int32(f.v)
			case 5:
				mc.DstKind = int32(f.v)
			}
		}
		if r.err == nil {
			v.Memcpy(mc)
		}
	case kind == gpuMemset && v.Memset != nil:
		*ms = Memset{GPU: c}
		for r.next(&f) {
			switch f.num {
			case 1:
				ms.Bytes = int64(f.v)
			case 2:
				ms.Value = int32(f.v)
			case 4:
				ms.MemKind = int32(f.v)
			}
		}
		if r.err == nil {
			v.Memset(ms)
		}
	case kind == gpuSync && v.Sync != nil:
		*sy = Sync{GPU: c}
		for r.next(&f) {
			switch f.num {
			case 1:
				sy.EventID = int64(f.v)
			case 4:
				sy.SyncType = int32(f.v)
			}
		}
		if r.err == nil {
			v.Sync(sy)
		}
	}
	return r.err
}

func decodeAPI(b []byte, a *API) error {
	*a = API{EventClass: -1}
	r := newReader(b)
	var f field
	for r.next(&f) {
		if f.typ != wireVarint {
			continue
		}
		switch f.num {
		case 1:
			a.Start = int64(f.v)
		case 2:
			a.End = int64(f.v)
		case 3:
			a.Correlation = int64(f.v)
		case 6:
			a.EventClass = int32(f.v)
		case 7:
			a.NameID = int64(f.v)
		case 8:
			a.ReturnValue = int64(f.v)
		case 13:
			a.GlobalTid = int64(f.v)
		}
	}
	return r.err
}

func decodeNVTX(b []byte, n *NVTX) error {
	*n = NVTX{EventType: -1, TextID: -1}
	r := newReader(b)
	var f field
	for r.next(&f) {
		switch {
		case f.num == 7 && f.typ == wireBytes:
			n.Text = f.bytes
		case f.typ != wireVarint:
		case f.num == 1:
			n.EventType = int32(f.v)
		case f.num == 2:
			n.Start = int64(f.v) // two's complement: before the session is negative
		case f.num == 10:
			n.End, n.HasEnd = int64(f.v), true
		case f.num == 9:
			n.GlobalTid = int64(f.v)
		case f.num == 12:
			n.TextID = int64(f.v)
		case f.num == 13:
			n.Domain = int64(f.v)
		}
	}
	return r.err
}

func decodeProcess(b []byte, p *Process) error {
	*p = Process{}
	var short []byte
	r := newReader(b)
	var f field
	for r.next(&f) {
		switch {
		case f.num == 2 && f.typ == wireVarint:
			p.GlobalPid = int64(f.v)
		case f.num == 5 && f.typ == wireBytes:
			short = f.bytes
		case f.num == 6 && f.typ == wireBytes:
			p.Name = f.bytes
		}
	}
	if len(p.Name) == 0 {
		p.Name = short
	}
	return r.err
}
