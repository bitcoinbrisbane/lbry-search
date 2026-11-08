package chain

import "errors"

const (
	opClaimName   = 0xb5
	opSupport     = 0xb6
	opUpdateClaim = 0xb7
	op2Drop       = 0x6d
	opDrop        = 0x75
	opPushData1   = 0x4c
	opPushData2   = 0x4d
	opPushData4   = 0x4e
)

type ScriptKind int

const (
	NotClaim ScriptKind = iota
	ClaimName
	UpdateClaim
	SupportClaim
)

// ClaimScript is the claim part at the front of an output script.
type ClaimScript struct {
	Kind    ScriptKind
	Name    []byte
	ClaimID string // display hex; for updates and supports
	Value   []byte // claim value (new/update) or support data
}

var errNotPush = errors.New("expected a push")

func readPush(script []byte, off int) ([]byte, int, error) {
	if off >= len(script) {
		return nil, off, errNotPush
	}
	op := int(script[off])
	off++
	var n int
	switch {
	case op == 0:
		return []byte{}, off, nil
	case op < opPushData1:
		n = op
	case op == opPushData1:
		if off+1 > len(script) {
			return nil, off, errNotPush
		}
		n = int(script[off])
		off++
	case op == opPushData2:
		if off+2 > len(script) {
			return nil, off, errNotPush
		}
		n = int(script[off]) | int(script[off+1])<<8
		off += 2
	case op == opPushData4:
		if off+4 > len(script) {
			return nil, off, errNotPush
		}
		n = int(script[off]) | int(script[off+1])<<8 | int(script[off+2])<<16 | int(script[off+3])<<24
		off += 4
	default:
		return nil, off, errNotPush
	}
	if n < 0 || off+n > len(script) {
		return nil, off, errNotPush
	}
	return script[off : off+n], off + n, nil
}

// ParseClaimScript recognises OP_CLAIM_NAME, OP_UPDATE_CLAIM and OP_SUPPORT_CLAIM
// (with or without support data). Anything else is NotClaim.
func ParseClaimScript(script []byte) ClaimScript {
	if len(script) == 0 {
		return ClaimScript{}
	}
	switch script[0] {
	case opClaimName:
		name, off, err := readPush(script, 1)
		if err != nil {
			return ClaimScript{}
		}
		value, off, err := readPush(script, off)
		if err != nil || !hasOps(script, off, op2Drop, opDrop) {
			return ClaimScript{}
		}
		return ClaimScript{Kind: ClaimName, Name: name, Value: value}
	case opUpdateClaim:
		name, off, err := readPush(script, 1)
		if err != nil {
			return ClaimScript{}
		}
		id, off, err := readPush(script, off)
		if err != nil || len(id) != 20 {
			return ClaimScript{}
		}
		value, off, err := readPush(script, off)
		if err != nil || !hasOps(script, off, op2Drop, op2Drop) {
			return ClaimScript{}
		}
		return ClaimScript{Kind: UpdateClaim, Name: name, ClaimID: DisplayHex(id), Value: value}
	case opSupport:
		name, off, err := readPush(script, 1)
		if err != nil {
			return ClaimScript{}
		}
		id, off, err := readPush(script, off)
		if err != nil || len(id) != 20 {
			return ClaimScript{}
		}
		if hasOps(script, off, op2Drop, opDrop) {
			return ClaimScript{Kind: SupportClaim, Name: name, ClaimID: DisplayHex(id)}
		}
		data, off, err := readPush(script, off)
		if err != nil || !hasOps(script, off, op2Drop, op2Drop) {
			return ClaimScript{}
		}
		return ClaimScript{Kind: SupportClaim, Name: name, ClaimID: DisplayHex(id), Value: data}
	}
	return ClaimScript{}
}

func hasOps(script []byte, off int, ops ...byte) bool {
	if off+len(ops) > len(script) {
		return false
	}
	for i, op := range ops {
		if script[off+i] != op {
			return false
		}
	}
	return true
}
