package claim

import "errors"

// A minimal protobuf reader: enough to pull the fields Carrel indexes out of LBRY's
// claim messages without generated code.

var errProto = errors.New("malformed protobuf")

type field struct {
	num   int
	wire  int
	value uint64 // varint / fixed
	bytes []byte // length-delimited
}

func readVarint(b []byte, off int) (uint64, int, error) {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if off >= len(b) {
			return 0, off, errProto
		}
		c := b[off]
		off++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, off, nil
		}
	}
	return 0, off, errProto
}

// fields parses one message level into its fields, in order.
func fields(b []byte) ([]field, error) {
	var out []field
	off := 0
	for off < len(b) {
		key, n, err := readVarint(b, off)
		if err != nil {
			return nil, err
		}
		off = n
		f := field{num: int(key >> 3), wire: int(key & 7)}
		if f.num == 0 {
			return nil, errProto
		}
		switch f.wire {
		case 0:
			f.value, off, err = readVarint(b, off)
			if err != nil {
				return nil, err
			}
		case 1:
			if off+8 > len(b) {
				return nil, errProto
			}
			off += 8
		case 2:
			l, n, err := readVarint(b, off)
			if err != nil || n+int(l) > len(b) || int(l) < 0 {
				return nil, errProto
			}
			f.bytes = b[n : n+int(l)]
			off = n + int(l)
		case 5:
			if off+4 > len(b) {
				return nil, errProto
			}
			off += 4
		default:
			return nil, errProto
		}
		out = append(out, f)
	}
	return out, nil
}

type msg []field

func parse(b []byte) msg {
	f, err := fields(b)
	if err != nil {
		return nil
	}
	return f
}

func (m msg) bytes(num int) []byte {
	var last []byte
	for _, f := range m {
		if f.num == num && f.wire == 2 {
			last = f.bytes
		}
	}
	return last
}

func (m msg) has(num int) bool {
	for _, f := range m {
		if f.num == num {
			return true
		}
	}
	return false
}

func (m msg) str(num int) string { return string(m.bytes(num)) }

func (m msg) sub(num int) msg {
	if b := m.bytes(num); b != nil {
		return parse(b)
	}
	return nil
}

func (m msg) varint(num int) uint64 {
	var last uint64
	for _, f := range m {
		if f.num == num && f.wire == 0 {
			last = f.value
		}
	}
	return last
}

func (m msg) repeatedBytes(num int) [][]byte {
	var out [][]byte
	for _, f := range m {
		if f.num == num && f.wire == 2 {
			out = append(out, f.bytes)
		}
	}
	return out
}
