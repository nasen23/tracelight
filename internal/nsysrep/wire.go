package nsysrep

import (
	"errors"
	"fmt"
)

var errCorrupt = errors.New("nsysrep: corrupt data")

// uvarint decodes a protobuf varint at b[i:], returning the value and the
// offset after it, or n < 0 if b ends first or the varint is too long.
func uvarint(b []byte, i int) (v uint64, n int) {
	for s := uint(0); s < 64; s += 7 {
		if i >= len(b) {
			return 0, -1
		}
		c := b[i]
		i++
		v |= uint64(c&0x7f) << s
		if c < 0x80 {
			return v, i
		}
	}
	return 0, -1
}

// Wire types.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// field is one protobuf field: its number, wire type, and value (for varints
// and fixed values) or bytes.
type field struct {
	num   int
	typ   int
	v     uint64
	bytes []byte
}

// reader walks the fields of a protobuf message.
type reader struct {
	b   []byte
	i   int
	err error
}

func newReader(b []byte) reader { return reader{b: b} }

// next reads the next field into f, and returns false at the end of the
// message or on an error (r.err).
func (r *reader) next(f *field) bool {
	if r.i >= len(r.b) || r.err != nil {
		return false
	}
	k, i := uvarint(r.b, r.i)
	if i < 0 || k>>3 == 0 || k>>3 > 1<<29 {
		r.err = fmt.Errorf("%w: bad tag at %d", errCorrupt, r.i)
		return false
	}
	f.num, f.typ, f.bytes = int(k>>3), int(k&7), nil
	switch f.typ {
	case wireVarint:
		f.v, i = uvarint(r.b, i)
	case wireFixed64:
		if i+8 > len(r.b) {
			i = -1
			break
		}
		f.v = le64(r.b[i:])
		i += 8
	case wireFixed32:
		if i+4 > len(r.b) {
			i = -1
			break
		}
		f.v = uint64(le32(r.b[i:]))
		i += 4
	case wireBytes:
		var n uint64
		n, i = uvarint(r.b, i)
		if i < 0 || n > uint64(len(r.b)-i) {
			i = -1
			break
		}
		f.bytes = r.b[i : i+int(n)]
		f.v = n
		i += int(n)
	default:
		i = -1
	}
	if i < 0 {
		r.err = fmt.Errorf("%w: bad field %d at %d", errCorrupt, f.num, r.i)
		return false
	}
	r.i = i
	return true
}

func le64(b []byte) uint64 {
	_ = b[7]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func le32(b []byte) uint32 {
	_ = b[3]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// lz4Block decodes an LZ4 block (the raw block format: no frame header or
// checksum) into dst[:0], which must have room for the rawLen bytes it
// decodes to.
func lz4Block(dst, src []byte, rawLen int) ([]byte, error) {
	if cap(dst) < rawLen {
		dst = make([]byte, 0, rawLen)
	}
	dst = dst[:rawLen]
	d, s := 0, 0
	for s < len(src) {
		tok := src[s]
		s++
		// Literals.
		n := int(tok >> 4)
		if n == 15 {
			for {
				if s >= len(src) {
					return nil, errCorrupt
				}
				c := src[s]
				s++
				n += int(c)
				if c != 255 {
					break
				}
			}
		}
		if n > len(src)-s || n > rawLen-d {
			return nil, errCorrupt
		}
		copy(dst[d:], src[s:s+n])
		d += n
		s += n
		if s == len(src) {
			break // the last sequence has literals only
		}
		// Match.
		if s+2 > len(src) {
			return nil, errCorrupt
		}
		off := int(src[s]) | int(src[s+1])<<8
		s += 2
		m := int(tok & 15)
		if m == 15 {
			for {
				if s >= len(src) {
					return nil, errCorrupt
				}
				c := src[s]
				s++
				m += int(c)
				if c != 255 {
					break
				}
			}
		}
		m += 4
		if off == 0 || off > d || m > rawLen-d {
			return nil, errCorrupt
		}
		from := d - off
		if off >= m {
			copy(dst[d:d+m], dst[from:from+m])
		} else {
			// Overlapping: the match repeats the last off bytes.
			for k := 0; k < m; k++ {
				dst[d+k] = dst[from+k]
			}
		}
		d += m
	}
	if d != rawLen {
		return nil, fmt.Errorf("%w: LZ4 block gave %d bytes, want %d", errCorrupt, d, rawLen)
	}
	return dst, nil
}
