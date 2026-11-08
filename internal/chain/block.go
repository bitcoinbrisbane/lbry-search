package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// LBRY block header: version(4) prev(32) merkle(32) claimtrie(32) time(4) bits(4) nonce(4).
const HeaderSize = 112

type OutPoint struct {
	TxHash []byte // internal byte order
	Index  uint32
}

// Key is "txid:nout" with the display txid.
func (o OutPoint) Key() string { return fmt.Sprintf("%s:%d", DisplayHex(o.TxHash), o.Index) }

type TxIn struct {
	Prev OutPoint
}

type TxOut struct {
	Value  int64 // dewies
	Script []byte
}

type Tx struct {
	Hash    []byte // internal byte order; DisplayHex(Hash) is the txid
	Inputs  []TxIn
	Outputs []TxOut
}

func (t *Tx) ID() string { return DisplayHex(t.Hash) }

// IsCoinbase: a single input spending the null outpoint.
func (t *Tx) IsCoinbase() bool {
	if len(t.Inputs) != 1 || t.Inputs[0].Prev.Index != 0xffffffff {
		return false
	}
	for _, b := range t.Inputs[0].Prev.TxHash {
		if b != 0 {
			return false
		}
	}
	return true
}

type Block struct {
	Hash     []byte // internal byte order
	PrevHash []byte
	Time     uint32
	Txs      []*Tx
}

var errShort = errors.New("unexpected end of data")

type reader struct {
	b   []byte
	off int
}

func (r *reader) bytes(n int) ([]byte, error) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, errShort
	}
	out := r.b[r.off : r.off+n]
	r.off += n
	return out, nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *reader) u64() (uint64, error) {
	b, err := r.bytes(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (r *reader) varint() (uint64, error) {
	b, err := r.bytes(1)
	if err != nil {
		return 0, err
	}
	switch b[0] {
	case 0xfd:
		v, err := r.bytes(2)
		if err != nil {
			return 0, err
		}
		return uint64(binary.LittleEndian.Uint16(v)), nil
	case 0xfe:
		v, err := r.u32()
		return uint64(v), err
	case 0xff:
		return r.u64()
	default:
		return uint64(b[0]), nil
	}
}

// ParseBlock parses a raw LBRY block (getblock <hash> 0).
func ParseBlock(raw []byte) (*Block, error) {
	if len(raw) < HeaderSize {
		return nil, errShort
	}
	hdr := raw[:HeaderSize]
	blk := &Block{
		Hash:     blockHash(hdr),
		PrevHash: append([]byte{}, hdr[4:36]...),
		Time:     binary.LittleEndian.Uint32(hdr[100:104]),
	}
	r := &reader{b: raw, off: HeaderSize}
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < n; i++ {
		tx, err := parseTx(r)
		if err != nil {
			return nil, fmt.Errorf("tx %d: %w", i, err)
		}
		blk.Txs = append(blk.Txs, tx)
	}
	return blk, nil
}

// ParseTx parses one raw transaction.
func ParseTx(raw []byte) (*Tx, error) {
	return parseTx(&reader{b: raw})
}

func parseTx(r *reader) (*Tx, error) {
	start := r.off
	version, err := r.bytes(4)
	if err != nil {
		return nil, err
	}
	segwit := false
	if r.off+2 <= len(r.b) && r.b[r.off] == 0 && r.b[r.off+1] == 1 {
		segwit = true
		r.off += 2
	}
	bodyStart := r.off
	tx := &Tx{}
	nin, err := r.varint()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < nin; i++ {
		hash, err := r.bytes(32)
		if err != nil {
			return nil, err
		}
		idx, err := r.u32()
		if err != nil {
			return nil, err
		}
		slen, err := r.varint()
		if err != nil {
			return nil, err
		}
		if _, err := r.bytes(int(slen)); err != nil {
			return nil, err
		}
		if _, err := r.u32(); err != nil { // sequence
			return nil, err
		}
		tx.Inputs = append(tx.Inputs, TxIn{Prev: OutPoint{TxHash: append([]byte{}, hash...), Index: idx}})
	}
	nout, err := r.varint()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < nout; i++ {
		v, err := r.u64()
		if err != nil {
			return nil, err
		}
		slen, err := r.varint()
		if err != nil {
			return nil, err
		}
		script, err := r.bytes(int(slen))
		if err != nil {
			return nil, err
		}
		tx.Outputs = append(tx.Outputs, TxOut{Value: int64(v), Script: append([]byte{}, script...)})
	}
	bodyEnd := r.off
	if segwit {
		for i := uint64(0); i < nin; i++ {
			items, err := r.varint()
			if err != nil {
				return nil, err
			}
			for j := uint64(0); j < items; j++ {
				l, err := r.varint()
				if err != nil {
					return nil, err
				}
				if _, err := r.bytes(int(l)); err != nil {
					return nil, err
				}
			}
		}
	}
	lock, err := r.bytes(4)
	if err != nil {
		return nil, err
	}
	// txid: hash of the serialization without the segwit marker and witnesses.
	var stripped []byte
	if segwit {
		stripped = append(append(append([]byte{}, version...), r.b[bodyStart:bodyEnd]...), lock...)
	} else {
		stripped = r.b[start:r.off]
	}
	tx.Hash = DoubleSHA256(stripped)
	return tx, nil
}

// blockHash: LBRY's proof-of-work hash isn't the block ID; the ID is sha256d(header).
func blockHash(header []byte) []byte { return DoubleSHA256(header) }
