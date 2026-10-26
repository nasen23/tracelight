// Package nsysrep reads Nsight Systems reports (.nsys-rep) natively,
// without nsys. FORMAT_NOTES.md at the repository root describes the
// format, as found by comparing reports with their SQLite exports.
//
// It decodes what a timeline needs: GPU kernels, memcpy, memset and
// synchronization, CUDA API calls, NVTX events, the string table, and the
// names of GPUs, processes and threads. Times and string ids are the ones
// `nsys export --type sqlite` writes.
package nsysrep

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Verified are the Nsight Systems versions (year.release) whose reports
// were checked against their SQLite exports. Open refuses others with
// ErrUnknownVersion, unless Options.AnyVersion.
var Verified = []string{"2026.3"}

var (
	// ErrNotReport is returned for files that are not Nsight Systems
	// reports.
	ErrNotReport = errors.New("nsysrep: not an Nsight Systems report")
	// ErrUnknownVersion is returned for reports of a version not in
	// Verified.
	ErrUnknownVersion = errors.New("nsysrep: report version not verified")
)

const (
	headerPrefix = "NVIDIA Tegra Profiler Report "
	trailer      = "U3w[END]"
	rawMagic     = "QDTEXT00"

	sectionRaw     = "Raw Sampling Data"
	sectionOffsets = "Sampling Data Offsets"
	sectionTime    = "Time Conversion Data"
)

// Options change how Open reads a report.
type Options struct {
	// AnyVersion reads reports of versions not in Verified.
	AnyVersion bool
}

// File is an opened report. Its frames are decoded on demand, so that the
// caller can decode them in parallel.
type File struct {
	// Version is the Nsight Systems version that wrote the report, as
	// "2026.3.1".
	Version string

	data     []byte
	sections map[string][2]int // name: offset, size
	frames   []frame
	// strFrame is the index in frames of the string table, -1 if none.
	strFrame int
}

// frame is one frame of the raw sampling data: a protobuf message, stored
// as is or as an LZ4 block.
type frame struct {
	off     int // of the frame's first byte, in data
	payload []byte
	rawLen  int // decoded length, if compressed
	lz4     bool
}

// Open reads and indexes the report at path.
func Open(path string, o Options) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, o)
}

// Parse indexes a report held in memory, which the File then refers to.
func Parse(data []byte, o Options) (*File, error) {
	f := &File{data: data, sections: map[string][2]int{}, strFrame: -1}
	ver, err := ParseVersion(data)
	if err != nil {
		return nil, err
	}
	f.Version = ver
	if !o.AnyVersion && !IsVerified(ver) {
		return nil, fmt.Errorf("%w: %s (verified: %s)", ErrUnknownVersion, ver, strings.Join(Verified, ", "))
	}
	if err := f.readTOC(); err != nil {
		return nil, err
	}
	if err := f.readFrames(); err != nil {
		return nil, err
	}
	return f, nil
}

// ParseVersion returns the version in a report's header, as "2026.3.1",
// given at least the file's first bytes.
func ParseVersion(head []byte) (string, error) {
	if !bytes.HasPrefix(head, []byte(headerPrefix)) {
		return "", ErrNotReport
	}
	rest := head[len(headerPrefix):]
	end := bytes.Index(rest[:min(len(rest), 128)], []byte("v0."))
	if end < 0 {
		return "", fmt.Errorf("%w: no version in the header", ErrNotReport)
	}
	parts := strings.Split(string(rest[:end]), "@")
	if len(parts) < 3 {
		return "", fmt.Errorf("%w: unexpected version format", ErrNotReport)
	}
	for _, p := range parts[:3] {
		if _, err := strconv.Atoi(p); err != nil {
			return "", fmt.Errorf("%w: unexpected version format", ErrNotReport)
		}
	}
	return strings.Join(parts[:3], "."), nil
}

// IsVerified reports whether reports of version (as ParseVersion returns
// it) were checked against their exports.
func IsVerified(version string) bool {
	for _, v := range Verified {
		if version == v || strings.HasPrefix(version, v+".") {
			return true
		}
	}
	return false
}

// Check reads only the header of the report at path, and says whether
// Open would accept its version.
func Check(path string, o Options) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	head := make([]byte, 256)
	n, _ := fh.Read(head)
	ver, err := ParseVersion(head[:n])
	if err != nil {
		return err
	}
	if !o.AnyVersion && !IsVerified(ver) {
		return fmt.Errorf("%w: %s", ErrUnknownVersion, ver)
	}
	return nil
}

// readTOC reads the table of contents: a protobuf message at the offset the
// last 16 bytes give.
func (f *File) readTOC() error {
	d := f.data
	if len(d) < 16 || string(d[len(d)-8:]) != trailer {
		return fmt.Errorf("%w: no table of contents", ErrNotReport)
	}
	end := len(d) - 16
	off := le64(d[end:])
	if off >= uint64(end) {
		return fmt.Errorf("%w: table of contents out of the file", errCorrupt)
	}
	secs, err := parseTOC(d[off:end], end)
	if err != nil {
		return err
	}
	f.sections = secs
	return nil
}

// parseTOC reads the sections a table of contents lists, in a file whose
// table of contents ends at end.
func parseTOC(toc []byte, end int) (map[string][2]int, error) {
	secs := map[string][2]int{}
	r := newReader(toc)
	var e field
	for r.next(&e) {
		if e.num != 1 || e.typ != wireBytes {
			continue
		}
		var name string
		var at, size uint64
		er := newReader(e.bytes)
		var g field
		for er.next(&g) {
			switch {
			case g.num == 1 && g.typ == wireBytes:
				name = string(g.bytes)
			case g.num == 2 && g.typ == wireVarint:
				at = g.v
			case g.num == 3 && g.typ == wireVarint:
				size = g.v
			}
		}
		if er.err != nil {
			return nil, er.err
		}
		if at > uint64(end) || size > uint64(end)-at {
			return nil, fmt.Errorf("%w: section out of the file", errCorrupt)
		}
		secs[name] = [2]int{int(at), int(size)}
	}
	return secs, r.err
}

func (f *File) section(name string) ([]byte, error) {
	s, ok := f.sections[name]
	if !ok {
		return nil, fmt.Errorf("nsysrep: no %q section", name)
	}
	return f.data[s[0] : s[0]+s[1]], nil
}

// readFrames splits the raw sampling data into frames, and finds the string
// table's by the offset the offsets section gives.
func (f *File) readFrames() error {
	raw, err := f.section(sectionRaw)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(raw, []byte(rawMagic)) {
		return fmt.Errorf("%w: raw data does not start with %s", errCorrupt, rawMagic)
	}
	base := f.sections[sectionRaw][0] + len(rawMagic)

	strOff := -1
	if offs, err := f.section(sectionOffsets); err == nil {
		r := newReader(offs)
		var e field
		for r.next(&e) {
			switch {
			case e.num == 1 && e.typ == wireBytes && string(e.bytes) != "lz4":
				return fmt.Errorf("nsysrep: unsupported compression %q", e.bytes)
			case e.num == 2 && e.typ == wireVarint:
				strOff = base + int(e.v)
			}
		}
		if r.err != nil {
			return r.err
		}
	}

	end := f.sections[sectionRaw][0] + len(raw)
	d := f.data[:end]
	for i := base; i < end; {
		fr, next, err := nextFrame(d, i)
		if err != nil {
			return fmt.Errorf("frame at %d: %w", i-base, err)
		}
		if i == strOff {
			f.strFrame = len(f.frames)
		}
		f.frames = append(f.frames, fr)
		i = next
	}
	if f.strFrame < 0 {
		return fmt.Errorf("%w: no string table", errCorrupt)
	}
	return nil
}

// nextFrame reads the frame at d[i:]. Nothing says whether a frame is
// compressed: it is when the first varint (the compressed length) is within
// LZ4's worst case for the second (the decoded length), and the block's
// first literals are the varint length of the one message it decodes to.
func nextFrame(d []byte, i int) (frame, int, error) {
	n, j := uvarint(d, i)
	if j < 0 || n > uint64(len(d)-j) {
		return frame{}, 0, errCorrupt
	}
	plain := frame{off: i, payload: d[j : j+int(n)]}
	next := j + int(n)
	rawLen, k := uvarint(d, j)
	if k < 0 || rawLen > 1<<31 || n > rawLen+rawLen/255+16 || n > uint64(len(d)-k) {
		return plain, next, nil
	}
	block := d[k : k+int(n)]
	if !lz4Starts(block, rawLen) {
		return plain, next, nil
	}
	return frame{off: i, payload: block, rawLen: int(rawLen), lz4: true}, k + int(n), nil
}

// lz4Starts reports whether an LZ4 block of decoded length rawLen starts
// with literals that are the varint rawLen minus the varint's own length.
func lz4Starts(block []byte, rawLen uint64) bool {
	if len(block) < 2 {
		return false
	}
	lit := int(block[0] >> 4)
	s := 1
	if lit == 15 {
		for s < len(block) {
			c := block[s]
			s++
			lit += int(c)
			if c != 255 {
				break
			}
		}
	}
	if lit < 1 || lit > len(block)-s {
		return false
	}
	lits := block[s : s+lit]
	v, n := uvarint(lits, 0)
	return n > 0 && v+uint64(n) == rawLen
}

// decode returns the frame's message, decoding it into buf if compressed,
// and buf (grown if needed) for reuse.
func (fr *frame) decode(buf []byte) (msg, scratch []byte, err error) {
	if !fr.lz4 {
		return fr.payload, buf, nil
	}
	out, err := lz4Block(buf, fr.payload, fr.rawLen)
	if err != nil {
		return nil, buf, err
	}
	// One message, prefixed by its length.
	n, i := uvarint(out, 0)
	if i < 0 || n != uint64(len(out)-i) {
		return nil, out[:0], fmt.Errorf("%w: compressed frame length", errCorrupt)
	}
	return out[i:], out[:0], nil
}
